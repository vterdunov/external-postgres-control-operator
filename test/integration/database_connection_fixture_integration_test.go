//go:build integration

package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	pg "github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	kubeenv "github.com/vterdunov/external-postgres-control-operator/test/envtest"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	r4OutputName   = "application-connection"
	r4DatabaseName = "database"
)

type r4ConnectionFixture struct {
	*Fixture
	api      client.Client
	scope    *kubeenv.Scope
	cluster  *epco.ExternalPostgres
	role     *epco.DatabaseRole
	source   *corev1.Secret
	database *epco.Database
	roles    []*epco.DatabaseRole
	dbs      []*epco.Database
}

func r4StartAPI(ctx context.Context) *kubeenv.Environment {
	_, source, _, ok := runtime.Caller(0)
	ExpectWithOffset(1, ok).To(BeTrue())
	environment, err := kubeenv.Start(ctx, kubeenv.Options{
		CRDDirectory:          filepath.Join(filepath.Dir(source), "..", "..", "config", "crd", "bases"),
		BinaryAssetsDirectory: os.Getenv("KUBEBUILDER_ASSETS"), Timeout: 30 * time.Second})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		Expect(environment.Stop(cleanup)).To(Succeed())
	})
	return environment
}
func r4NewFixture(ctx context.Context, environment *kubeenv.Environment) *r4ConnectionFixture {
	base, err := postgres.NewFixture(ctx)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	scope, _, err := environment.NewScope(ctx, "r4-connection-")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	f := &r4ConnectionFixture{Fixture: base, api: environment.Client, scope: scope}
	DeferCleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		// Every owned cleanup runs even when an earlier step fails.
		Expect(errors.Join(scope.Cleanup(cleanup), f.cleanupSQL(cleanup), base.Close(cleanup))).To(Succeed())
	})
	f.cluster = recoveryCluster(ctx, scope, base)
	f.role, f.source = f.addRole(ctx, "application", base.Role, base.Password)
	f.database = f.addDatabase(ctx, r4DatabaseName, f.role, r4OutputName)
	return f
}

func (f *r4ConnectionFixture) cleanupSQL(ctx context.Context) error {
	databases := make([]string, 0, len(f.dbs))
	roles := make([]string, 0, len(f.roles))
	for _, target := range f.dbs {
		databases = append(databases, string(target.Spec.Name))
	}
	for _, role := range f.roles {
		if string(role.Spec.Name) != f.Role {
			roles = append(roles, string(role.Spec.Name))
		}
	}
	return f.Server.cleanupOwnedSQL(ctx, databases, roles)
}

func (f *r4ConnectionFixture) addRole(ctx context.Context, name, sqlName, password string) (
	*epco.DatabaseRole, *corev1.Secret,
) {
	source := &corev1.Secret{Name: name + "-source", Namespace: f.scope.Namespace.Name,
		Type: corev1.SecretTypeBasicAuth, Data: map[string][]byte{
			corev1.BasicAuthUsernameKey: []byte(sqlName), corev1.BasicAuthPasswordKey: []byte(password)}}
	ExpectWithOffset(1, f.scope.Create(ctx, source)).To(Succeed())
	role := &epco.DatabaseRole{Name: name, Namespace: f.scope.Namespace.Name,
		Spec: epco.DatabaseRoleSpec{Name: epco.SQLName(sqlName), Login: true,
			Cluster: epco.ClusterReference{Name: f.cluster.Name}, PasswordSecret: &epco.LocalReference{Name: source.Name}}}
	ExpectWithOffset(1, f.scope.Create(ctx, role)).To(Succeed())
	f.roles = append(f.roles, role)
	return role, source
}
func (f *r4ConnectionFixture) addDatabase(ctx context.Context, name string,
	role *epco.DatabaseRole, output string,
) *epco.Database {
	sqlName, err := randomName("r4_db_")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	database := &epco.Database{Name: name, Namespace: f.scope.Namespace.Name,
		Spec: epco.DatabaseSpec{Name: epco.SQLName(sqlName), Owner: roleSQLAdmin,
			Cluster: epco.ClusterReference{Name: f.cluster.Name}, ReclaimPolicy: epco.ReclaimRetain,
			ConnectionSecrets: []epco.ConnectionSecret{{Name: output, RoleRef: epco.LocalReference{Name: role.Name}}}}}
	ExpectWithOffset(1, f.scope.Create(ctx, database)).To(Succeed())
	f.dbs = append(f.dbs, database)
	return database
}
func (f *r4ConnectionFixture) reconcile(ctx context.Context,
	database *epco.Database, api client.Client,
) *epco.Database {
	reconciler := databaseRecoveryReconciler(api, pg.NewDatabaseFactory())
	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(database)})
	// No raw controller error is printed: a failing secrecy contract must never
	// make the failure artifact itself disclose credentials.
	ExpectWithOffset(1, r4ReconcileSucceeded(err)).To(BeTrue(), "Database reconciliation returned an error")
	return recoveredDatabase(ctx, f.api, database)
}
func (f *r4ConnectionFixture) output(ctx context.Context, name string) *corev1.Secret {
	secret := &corev1.Secret{}
	ExpectWithOffset(1, f.api.Get(ctx,
		client.ObjectKey{Name: name, Namespace: f.scope.Namespace.Name}, secret)).To(Succeed())
	return secret
}
func (f *r4ConnectionFixture) published(ctx context.Context, database *epco.Database, output string) *corev1.Secret {
	var secret *corev1.Secret
	Eventually(func() bool {
		f.reconcile(ctx, database, f.api)
		secret = &corev1.Secret{}
		return f.api.Get(ctx, client.ObjectKey{Name: output, Namespace: database.Namespace}, secret) == nil
	}).WithContext(ctx).WithTimeout(3*time.Second).Should(BeTrue(), "requested application Secret is published")
	return secret
}
func (f *r4ConnectionFixture) bootstrap(ctx context.Context) *corev1.Secret {
	recoverUntilReady(ctx, f.api, recoveryReconciler(f.api, pg.NewRoleFactory()), f.role)
	secret := f.published(ctx, f.database, r4OutputName)
	Eventually(func() bool {
		f.reconcile(ctx, f.database, f.api)
		exists, err := f.Server.DatabaseExists(ctx, string(f.database.Spec.Name))
		return err == nil && exists && f.login(ctx, string(f.database.Spec.Name), f.Role, f.Password)
	}).WithContext(ctx).WithTimeout(5*time.Second).Should(BeTrue(),
		"rotation starts with an existing database and the published password accepted by SQL")
	r4Password(secret, []byte(f.Password))
	return secret
}
func (f *r4ConnectionFixture) refreshed(ctx context.Context, database *epco.Database,
	name string, password []byte, api client.Client,
) {
	Eventually(func() bool {
		f.reconcile(ctx, database, api)
		return bytes.Equal(f.output(ctx, name).Data["password"], password)
	}).WithContext(ctx).WithTimeout(3*time.Second).Should(BeTrue(),
		"the existing output refreshes to the SQL-confirmed current source")
	f.output(ctx, name)
}
func (f *r4ConnectionFixture) rotate(ctx context.Context, source *corev1.Secret, password []byte) {
	current := &corev1.Secret{}
	ExpectWithOffset(1, f.api.Get(ctx, client.ObjectKeyFromObject(source), current)).To(Succeed())
	current.Data[corev1.BasicAuthPasswordKey] = bytes.Clone(password)
	ExpectWithOffset(1, f.api.Update(ctx, current)).To(Succeed())
}
func (f *r4ConnectionFixture) login(ctx context.Context, database, role, password string) bool {
	connection, err := f.Server.Open(database, role, password)
	if err != nil {
		return false
	}
	defer func() { Expect(connection.Close()).To(Succeed()) }()
	var user, name string
	err = connection.QueryRowContext(ctx, "SELECT current_user,current_database()").Scan(&user, &name)
	return err == nil && user == role && name == database
}
func (f *r4ConnectionFixture) target() *sql.DB {
	target, err := f.Server.Open(string(f.database.Spec.Name), roleSQLAdmin, f.Server.password)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	DeferCleanup(func() { Expect(target.Close()).To(Succeed()) })
	return target
}
func (f *r4ConnectionFixture) editDatabase(ctx context.Context, edit func(*epco.DatabaseSpec)) {
	current := recoveredDatabase(ctx, f.api, f.database)
	edit(&current.Spec)
	ExpectWithOffset(1, f.api.Update(ctx, current)).To(Succeed())
}

// Assertions receive only success, so a controller error cannot disclose a
// password or password-bearing connection URI in the failure artifact.
func r4ReconcileSucceeded(err error) bool {
	return err == nil
}
func r4Password(secret *corev1.Secret, expected []byte) {
	ExpectWithOffset(1, bytes.Equal(secret.Data[corev1.BasicAuthPasswordKey], expected)).To(BeTrue(),
		"output contains the expected credential bytes")
}
func r4Unchanged(before, after *corev1.Secret) {
	ExpectWithOffset(1, reflect.DeepEqual(before.Data, after.Data)).To(BeTrue(), "previous output payload is preserved")
}
func r4NotReady(database *epco.Database) {
	condition := meta.FindStatusCondition(database.Status.Conditions, "Ready")
	ExpectWithOffset(1, condition).NotTo(BeNil())
	ExpectWithOffset(1, condition.Status).To(Equal(metav1.ConditionFalse))
	ExpectWithOffset(1, condition.ObservedGeneration).To(Equal(database.Generation))
}
func r4SecretFree(object any, password []byte) {
	encoded, err := json.Marshal(object)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, bytes.Contains(encoded, password)).To(BeFalse(), "status and metadata contain no credential bytes")
}

type r4RoleFactory struct {
	pg.RoleFactory
	reject atomic.Bool
	after  func()
}
type r4RoleSession struct {
	pg.RoleSession
	owner *r4RoleFactory
}

func (f *r4RoleFactory) Open(ctx context.Context, config *pg.Config) (pg.RoleSession, error) {
	session, err := f.RoleFactory.Open(ctx, config)
	if err != nil {
		return nil, err
	}
	return &r4RoleSession{RoleSession: session, owner: f}, nil
}
func (s *r4RoleSession) Apply(ctx context.Context, desired pg.RoleDesired) (*pg.RoleApplyResult, error) {
	if s.owner.reject.Load() {
		return nil, &pg.Failure{Reason: pg.InsufficientPrivileges, Message: "injected SQL role update denial"}
	}
	result, err := s.RoleSession.Apply(ctx, desired)
	if err == nil && s.owner.after != nil {
		s.owner.after()
	}
	return result, err
}

type r4OutputClient struct {
	client.Client
	name   string
	reject atomic.Bool
	failed atomic.Int32
}

func (c *r4OutputClient) Update(ctx context.Context, object client.Object, options ...client.UpdateOption) error {
	if secret, ok := object.(*corev1.Secret); ok && secret.Name == c.name && c.reject.Load() {
		c.failed.Add(1)
		return errors.New("injected connection Secret update failure")
	}
	return c.Client.Update(ctx, object, options...)
}
func (c *r4OutputClient) Patch(ctx context.Context, object client.Object,
	patch client.Patch, options ...client.PatchOption,
) error {
	if secret, ok := object.(*corev1.Secret); ok && secret.Name == c.name && c.reject.Load() {
		c.failed.Add(1)
		return errors.New("injected connection Secret patch failure")
	}
	return c.Client.Patch(ctx, object, patch, options...)
}
