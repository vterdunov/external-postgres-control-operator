//go:build integration

package integration

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/controller"
	pg "github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	kubeenv "github.com/vterdunov/external-postgres-control-operator/test/envtest"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type permissionRecoveryClient struct {
	client.Client
	reject func(context.Context, *epco.Database) bool
	failed atomic.Bool
}

type permissionRecoveryWriter struct {
	client.SubResourceWriter
	owner *permissionRecoveryClient
}

func (c *permissionRecoveryClient) Status() client.SubResourceWriter {
	return &permissionRecoveryWriter{SubResourceWriter: c.Client.Status(), owner: c}
}
func (w *permissionRecoveryWriter) failure(ctx context.Context, obj client.Object) error {
	db, ok := obj.(*epco.Database)
	if ok && w.owner.reject(ctx, db) {
		w.owner.failed.Store(true)
		return errors.New("injected ACL status persistence failure")
	}
	return nil
}
func (w *permissionRecoveryWriter) Update(ctx context.Context, obj client.Object,
	opts ...client.SubResourceUpdateOption) error {
	if err := w.failure(ctx, obj); err != nil {
		return err
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}
func (w *permissionRecoveryWriter) Patch(ctx context.Context, obj client.Object,
	patch client.Patch, opts ...client.SubResourcePatchOption) error {
	if err := w.failure(ctx, obj); err != nil {
		return err
	}
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

// Read the real serialized status without a compilation dependency on new fields.
func permissionRecoveryHistory(database *epco.Database) []any {
	raw, err := kruntime.DefaultUnstructuredConverter.ToUnstructured(database)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	history, _, err := unstructured.NestedSlice(raw, "status", "managedACLTargets")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	var bindings []any
	for _, target := range history {
		object, ok := target.(map[string]any)
		ExpectWithOffset(1, ok).To(BeTrue())
		rows, _, nestedErr := unstructured.NestedSlice(object, "bindings")
		ExpectWithOffset(1, nestedErr).NotTo(HaveOccurred())
		bindings = append(bindings, rows...)
	}
	return bindings
}

func permissionDatabaseGrant(ctx context.Context, f *Fixture, name, grantee, privilege string) bool {
	var found bool
	err := f.Server.Admin.QueryRowContext(ctx, "SELECT EXISTS ("+
		"SELECT 1 FROM pg_database d, LATERAL aclexplode(COALESCE(d.datacl,acldefault('d',d.datdba))) a "+
		"JOIN pg_roles r ON r.oid=a.grantee WHERE d.datname=$1 AND r.rolname=$2 AND a.privilege_type=$3)",
		name, grantee, privilege).Scan(&found)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return found
}

func permissionRecoveryReconcile(ctx context.Context, r *controller.DatabaseReconciler, database *epco.Database) {
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(database)})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
}

var _ = Describe("R3 Database ACL durable recovery", Ordered, ContinueOnFailure, func() {
	var environment *kubeenv.Environment
	var scope *kubeenv.Scope
	var fixture *Fixture
	var database *epco.Database
	var grantee string

	BeforeAll(func(ctx SpecContext) {
		_, source, _, ok := runtime.Caller(0)
		Expect(ok).To(BeTrue())
		var err error
		environment, err = kubeenv.Start(ctx, kubeenv.Options{
			CRDDirectory:          filepath.Join(filepath.Dir(source), "..", "..", "config", "crd", "bases"),
			BinaryAssetsDirectory: os.Getenv("KUBEBUILDER_ASSETS"), Timeout: 30 * time.Second})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			Expect(environment.Stop(cleanup)).To(Succeed())
		})
	}, NodeTimeout(60*time.Second))

	BeforeEach(func(ctx SpecContext) {
		var err error
		fixture, err = postgres.NewFixture(ctx)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			Expect(fixture.Close(cleanup)).To(Succeed())
		})
		scope, _, err = environment.NewScope(ctx, "r3-recovery-")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			Expect(scope.Cleanup(cleanup)).To(Succeed())
		})
		cluster := recoveryCluster(ctx, scope, fixture)
		grantee = roleName(fixture)
		roleSQL(ctx, fixture, "CREATE ROLE "+quoteIdentifier(grantee)+" NOLOGIN")
		// Provision the target as the restricted administrator, so superuser authority
		// cannot hide a missing ACL capability in production reconciliation.
		name, err := randomName("r3_managed_")
		Expect(err).NotTo(HaveOccurred())
		recoveryDatabaseCleanup(fixture.Server, name)
		roleSQL(ctx, fixture, "CREATE DATABASE "+quoteIdentifier(name)+" OWNER "+quoteIdentifier(fixture.Role))
		secret := &corev1.Secret{}
		secretKey := client.ObjectKey{Name: recoveryAdminSecret, Namespace: scope.Namespace.Name}
		Expect(environment.Client.Get(ctx, secretKey, secret)).To(Succeed())
		secret.Data[corev1.BasicAuthUsernameKey] = []byte(fixture.Role)
		secret.Data[corev1.BasicAuthPasswordKey] = []byte(fixture.Password)
		Expect(environment.Client.Update(ctx, secret)).To(Succeed())
		database = &epco.Database{Name: databaseRecoveryApplicationName, Namespace: scope.Namespace.Name,
			Spec: epco.DatabaseSpec{Name: epco.SQLName(name), Owner: epco.SQLName(fixture.Role),
				Cluster: epco.ClusterReference{Name: cluster.Name}, ReclaimPolicy: epco.ReclaimRetain,
				Schemas: []epco.SchemaSpec{{Name: databaseRecoveryApplicationName}}}}
		Expect(scope.Create(ctx, database)).To(Succeed())
		database = recoverDatabaseReady(ctx, environment.Client,
			databaseRecoveryReconciler(environment.Client, pg.NewDatabaseFactory()), database)
	}, NodeTimeout(30*time.Second))

	setCreate := func(ctx context.Context) {
		database = recoveredDatabase(ctx, environment.Client, database)
		database.Spec.Permissions = &epco.DatabasePermissions{Create: []epco.Permission{{Name: epco.SQLName(grantee)}}}
		Expect(environment.Client.Update(ctx, database)).To(Succeed())
	}

	It("confirms a direct grant and durable cleanup responsibility before current success", func(ctx SpecContext) {
		setCreate(ctx)
		r := databaseRecoveryReconciler(environment.Client, pg.NewDatabaseFactory())
		current := recoverDatabaseReady(ctx, environment.Client, r, database)
		condition := meta.FindStatusCondition(current.Status.Conditions, "Ready")
		Expect(condition).NotTo(BeNil())
		Expect(condition.Status).To(Equal(metav1.ConditionTrue), "supported R3 database CREATE must converge")
		Expect(condition.ObservedGeneration).To(Equal(current.Generation))
		Expect(current.Status.AppliedGeneration).To(Equal(current.Generation))
		Expect(permissionDatabaseGrant(ctx, fixture, string(database.Spec.Name), grantee, "CREATE")).To(BeTrue())
		Expect(permissionRecoveryHistory(current)).NotTo(BeEmpty(), "direct ACL cleanup responsibility must survive restart")
	}, NodeTimeout(30*time.Second))

	It("allows no grant when the pre-SQL cleanup checkpoint cannot persist", func(ctx SpecContext) {
		setCreate(ctx)
		fault := &permissionRecoveryClient{Client: environment.Client}
		fault.reject = func(writeCtx context.Context, candidate *epco.Database) bool {
			return len(permissionRecoveryHistory(candidate)) > 0 &&
				!permissionDatabaseGrant(writeCtx, fixture, string(database.Spec.Name), grantee, "CREATE")
		}
		r := databaseRecoveryReconciler(fault, pg.NewDatabaseFactory())
		Eventually(func() bool {
			_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(database)})
			return fault.failed.Load()
		}).WithContext(ctx).WithTimeout(5*time.Second).Should(BeTrue(), "cleanup checkpoint must precede the grant")
		Expect(permissionDatabaseGrant(ctx, fixture, string(database.Spec.Name), grantee, "CREATE")).To(BeFalse())
		current := recoveredDatabase(ctx, environment.Client, database)
		Expect(current.Status.AppliedGeneration).NotTo(Equal(current.Generation))
		Expect(permissionRecoveryHistory(current)).To(BeEmpty())
		recoverDatabaseReady(ctx, environment.Client,
			databaseRecoveryReconciler(environment.Client, pg.NewDatabaseFactory()), database)
		Expect(permissionDatabaseGrant(ctx, fixture, string(database.Spec.Name), grantee, "CREATE")).To(BeTrue())
	}, NodeTimeout(30*time.Second))

	It("revokes a committed grant after failed completion status, omission and restart", func(ctx SpecContext) {
		setCreate(ctx)
		fault := &permissionRecoveryClient{Client: environment.Client}
		fault.reject = func(writeCtx context.Context, _ *epco.Database) bool {
			return permissionDatabaseGrant(writeCtx, fixture, string(database.Spec.Name), grantee, "CREATE")
		}
		r := databaseRecoveryReconciler(fault, pg.NewDatabaseFactory())
		Eventually(func() bool {
			_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(database)})
			return fault.failed.Load()
		}).WithContext(ctx).WithTimeout(5 * time.Second).Should(BeTrue())
		Expect(permissionDatabaseGrant(ctx, fixture, string(database.Spec.Name), grantee, "CREATE")).To(BeTrue())
		current := recoveredDatabase(ctx, environment.Client, database)
		Expect(permissionRecoveryHistory(current)).NotTo(BeEmpty(), "intent predates the SQL completion barrier")
		Expect(current.Status.AppliedGeneration).NotTo(Equal(current.Generation))
		current.Spec.Permissions = nil
		Expect(environment.Client.Update(ctx, current)).To(Succeed())
		current = recoverDatabaseReady(ctx, environment.Client,
			databaseRecoveryReconciler(environment.Client, pg.NewDatabaseFactory()), current)
		Expect(permissionDatabaseGrant(ctx, fixture, string(database.Spec.Name), grantee, "CREATE")).To(BeFalse())
		Expect(current.Status.AppliedGeneration).To(Equal(current.Generation))
	}, NodeTimeout(30*time.Second))

	It("removes same-UID grants after block omission while preserving unrelated SQL privileges", func(ctx SpecContext) {
		roleSQL(ctx, fixture, "GRANT TEMPORARY ON DATABASE "+
			quoteIdentifier(string(database.Spec.Name))+" TO "+quoteIdentifier(grantee))
		setCreate(ctx)
		database = recoverDatabaseReady(ctx, environment.Client,
			databaseRecoveryReconciler(environment.Client, pg.NewDatabaseFactory()), database)
		uid := database.UID
		database.Spec.Permissions = nil
		Expect(environment.Client.Update(ctx, database)).To(Succeed())
		database = recoverDatabaseReady(ctx, environment.Client,
			databaseRecoveryReconciler(environment.Client, pg.NewDatabaseFactory()), database)
		Expect(database.UID).To(Equal(uid))
		Expect(permissionDatabaseGrant(ctx, fixture, string(database.Spec.Name), grantee, "CREATE")).To(BeFalse())
		Expect(permissionDatabaseGrant(ctx, fixture, string(database.Spec.Name), grantee, "TEMPORARY")).To(BeTrue())
	}, NodeTimeout(30*time.Second))

	It("retains grants across deletion and new UID until explicitly absent", func(ctx SpecContext) {
		setCreate(ctx)
		r := databaseRecoveryReconciler(environment.Client, pg.NewDatabaseFactory())
		database = recoverDatabaseReady(ctx, environment.Client, r, database)
		oldUID := database.UID
		spec := database.Spec
		recoveryDatabaseGone(ctx, environment.Client, r, database)
		Expect(permissionDatabaseGrant(ctx, fixture, string(spec.Name), grantee, "CREATE")).To(BeTrue())
		spec.Permissions = nil
		replacement := &epco.Database{Name: database.Name, Namespace: database.Namespace, Spec: spec}
		Expect(scope.Create(ctx, replacement)).To(Succeed())
		replacement = recoverDatabaseReady(ctx, environment.Client,
			databaseRecoveryReconciler(environment.Client, pg.NewDatabaseFactory()), replacement)
		Expect(replacement.UID).NotTo(Equal(oldUID))
		Expect(permissionRecoveryHistory(replacement)).To(BeEmpty())
		Expect(permissionDatabaseGrant(ctx, fixture, string(spec.Name), grantee, "CREATE")).To(BeTrue())
		replacement.Spec.Permissions = &epco.DatabasePermissions{Create: []epco.Permission{{
			Name: epco.SQLName(grantee), Ensure: epco.EnsureAbsent}}}
		Expect(environment.Client.Update(ctx, replacement)).To(Succeed())
		recoverDatabaseReady(ctx, environment.Client,
			databaseRecoveryReconciler(environment.Client, pg.NewDatabaseFactory()), replacement)
		Expect(permissionDatabaseGrant(ctx, fixture, string(spec.Name), grantee, "CREATE")).To(BeFalse())
	}, NodeTimeout(40*time.Second))

	It("records confirmed lifecycle ownership while a named table remains pending", func(ctx SpecContext) {
		database = recoveredDatabase(ctx, environment.Client, database)
		r := databaseRecoveryReconciler(environment.Client, pg.NewDatabaseFactory())
		recoveryDatabaseGone(ctx, environment.Client, r, database)
		database = &epco.Database{Name: database.Name, Namespace: database.Namespace, Spec: database.Spec}
		database.Spec.Schemas[0].TablePermissions = []epco.NamedTablePermissions{{Name: "missing",
			Permissions: epco.TablePermissions{Select: []epco.Permission{{Name: epco.SQLName(grantee)}}}}}
		database.Spec.Schemas[0].DefaultPrivileges = []epco.DefaultPrivileges{{ForRole: epco.SQLName(fixture.Role),
			Tables: &epco.TablePermissions{Select: []epco.Permission{{Name: epco.SQLName(grantee)}}}}}
		Expect(scope.Create(ctx, database)).To(Succeed())
		Eventually(func(g Gomega) {
			_, reconcileErr := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(database)})
			g.Expect(reconcileErr).NotTo(HaveOccurred())
			current := &epco.Database{}
			g.Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(database), current)).To(Succeed())
			condition := meta.FindStatusCondition(current.Status.Conditions, "Ready")
			g.Expect(condition).NotTo(BeNil())
			g.Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(condition.Reason).To(Equal("DependenciesNotReady"), current.Status.Message)
			g.Expect(current.Status.HasManagedSQL).To(BeTrue())
		}).WithContext(ctx).WithTimeout(10 * time.Second).Should(Succeed())
		current := recoveredDatabase(ctx, environment.Client, database)
		condition := meta.FindStatusCondition(current.Status.Conditions, "Ready")
		Expect(condition).NotTo(BeNil())
		Expect(condition.Status).To(Equal(metav1.ConditionFalse))
		Expect(condition.Reason).To(Equal("DependenciesNotReady"))
		Expect(current.Status.HasManagedSQL).To(BeTrue())
		Expect(current.Status.AppliedGeneration).NotTo(Equal(current.Generation))
		competitor := &epco.Database{Name: "competitor", Namespace: database.Namespace, Spec: database.Spec}
		competitor.Spec.Schemas = nil
		Expect(scope.Create(ctx, competitor)).To(Succeed())
		permissionRecoveryReconcile(ctx, r, competitor)
		loser := recoveredDatabase(ctx, environment.Client, competitor)
		Expect(meta.FindStatusCondition(loser.Status.Conditions, "Ready").Reason).To(Equal("OwnershipConflict"))
		Expect(loser.Status.HasManagedSQL).To(BeFalse())
		target, err := fixture.Server.Open(string(database.Spec.Name), fixture.Role, fixture.Password)
		Expect(err).NotTo(HaveOccurred())
		defer func() { Expect(target.Close()).To(Succeed()) }()
		_, err = target.ExecContext(ctx, "CREATE TABLE app.future(value int)")
		Expect(err).NotTo(HaveOccurred())
		var direct bool
		Expect(target.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_class c, LATERAL aclexplode(c.relacl) a "+
			"WHERE c.oid='app.future'::regclass AND a.grantee=(SELECT oid FROM pg_roles WHERE rolname=$1) "+
			"AND a.privilege_type='SELECT')", grantee).Scan(&direct)).To(Succeed())
		Expect(direct).To(BeTrue(), "independent defaults converge despite an unresolved named-table dependency")
	}, NodeTimeout(30*time.Second))

	It("cleans omitted schema grants while preserving schema and data", func(ctx SpecContext) {
		database = recoveredDatabase(ctx, environment.Client, database)
		database.Spec.Schemas[0].Permissions = &epco.SchemaPermissions{
			Usage: []epco.Permission{{Name: epco.SQLName(grantee)}}}
		Expect(environment.Client.Update(ctx, database)).To(Succeed())
		database = recoverDatabaseReady(ctx, environment.Client,
			databaseRecoveryReconciler(environment.Client, pg.NewDatabaseFactory()), database)
		target, err := fixture.Server.Open(string(database.Spec.Name), fixture.Role, fixture.Password)
		Expect(err).NotTo(HaveOccurred())
		defer func() { Expect(target.Close()).To(Succeed()) }()
		_, err = target.ExecContext(ctx, "CREATE TABLE app.sentinel(value int); INSERT INTO app.sentinel VALUES(42)")
		Expect(err).NotTo(HaveOccurred())
		database.Spec.Schemas = nil
		Expect(environment.Client.Update(ctx, database)).To(Succeed())
		recoverDatabaseReady(ctx, environment.Client,
			databaseRecoveryReconciler(environment.Client, pg.NewDatabaseFactory()), database)
		var count, value int
		Expect(target.QueryRowContext(ctx, "SELECT count(*) FROM pg_namespace n, LATERAL aclexplode(n.nspacl) a "+
			"WHERE n.nspname='app' AND a.grantee=(SELECT oid FROM pg_roles WHERE rolname=$1) "+
			"AND a.privilege_type='USAGE'", grantee).Scan(&count)).To(Succeed())
		Expect(count).To(BeZero())
		Expect(target.QueryRowContext(ctx, "SELECT value FROM app.sentinel").Scan(&value)).To(Succeed())
		Expect(value).To(Equal(42))
	}, NodeTimeout(30*time.Second))

	It("preserves managed history in observe mode and resumes current omission cleanup", func(ctx SpecContext) {
		setCreate(ctx)
		database = recoverDatabaseReady(ctx, environment.Client,
			databaseRecoveryReconciler(environment.Client, pg.NewDatabaseFactory()), database)
		history := permissionRecoveryHistory(database)
		database.Spec.Permissions = nil
		database.Spec.ManagementPolicy = epco.ManagementObserve
		Expect(environment.Client.Update(ctx, database)).To(Succeed())
		permissionRecoveryReconcile(ctx, databaseRecoveryReconciler(environment.Client, pg.NewDatabaseFactory()), database)
		database = recoveredDatabase(ctx, environment.Client, database)
		Expect(meta.FindStatusCondition(database.Status.Conditions, "Ready").Reason).To(Equal("Observed"))
		Expect(database.Status.HasManagedSQL).To(BeTrue())
		Expect(permissionRecoveryHistory(database)).To(Equal(history))
		Expect(permissionDatabaseGrant(ctx, fixture, string(database.Spec.Name), grantee, "CREATE")).To(BeTrue())
		database.Spec.ManagementPolicy = epco.ManagementManage
		Expect(environment.Client.Update(ctx, database)).To(Succeed())
		recoverDatabaseReady(ctx, environment.Client,
			databaseRecoveryReconciler(environment.Client, pg.NewDatabaseFactory()), database)
		Expect(permissionDatabaseGrant(ctx, fixture, string(database.Spec.Name), grantee, "CREATE")).To(BeFalse())
	}, NodeTimeout(30*time.Second))

	It("pauses omission cleanup and reclaim deletion while Namespace authorization is denied", func(ctx SpecContext) {
		setCreate(ctx)
		r := databaseRecoveryReconciler(environment.Client, pg.NewDatabaseFactory())
		database = recoverDatabaseReady(ctx, environment.Client, r, database)
		cluster := &epco.PostgresCluster{}
		clusterKey := client.ObjectKey{Name: database.Spec.Cluster.Name, Namespace: database.Namespace}
		Expect(environment.Client.Get(ctx, clusterKey, cluster)).To(Succeed())
		cluster.Spec.Access = &epco.ClusterAccess{NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"r3-access": "allowed"}}}
		Expect(environment.Client.Update(ctx, cluster)).To(Succeed())
		database.Spec.Permissions = nil
		database.Spec.ReclaimPolicy = epco.ReclaimDelete
		Expect(environment.Client.Update(ctx, database)).To(Succeed())
		permissionRecoveryReconcile(ctx, r, database)
		database = recoveredDatabase(ctx, environment.Client, database)
		Expect(meta.FindStatusCondition(database.Status.Conditions, "Ready").Reason).To(Equal("AccessDenied"))
		Expect(permissionDatabaseGrant(ctx, fixture, string(database.Spec.Name), grantee, "CREATE")).To(BeTrue())
		Expect(permissionRecoveryHistory(database)).NotTo(BeEmpty())
		Expect(environment.Client.Delete(ctx, database)).To(Succeed())
		permissionRecoveryReconcile(ctx, r, database)
		database = recoveredDatabase(ctx, environment.Client, database)
		Expect(database.Finalizers).To(ContainElement(controller.DatabaseFinalizer))
		Expect(recoveryDatabaseExists(ctx, fixture.Server, string(database.Spec.Name))).To(BeTrue())
		Expect(permissionDatabaseGrant(ctx, fixture, string(database.Spec.Name), grantee, "CREATE")).To(BeTrue())
		cluster.Spec.Access = &epco.ClusterAccess{NamespaceSelector: &metav1.LabelSelector{}}
		Expect(environment.Client.Update(ctx, cluster)).To(Succeed())
		Eventually(func(g Gomega) {
			_, reconcileErr := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(database)})
			g.Expect(reconcileErr).NotTo(HaveOccurred())
			getErr := environment.Client.Get(ctx, client.ObjectKeyFromObject(database), &epco.Database{})
			g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue())
		}).WithContext(ctx).WithTimeout(10 * time.Second).Should(Succeed())
		Expect(recoveryDatabaseExists(ctx, fixture.Server, string(database.Spec.Name))).To(BeFalse())
	}, NodeTimeout(35*time.Second))

	It("retains A history on B and rebinds cleanup on returning to A", func(ctx SpecContext) {
		setCreate(ctx)
		r := databaseRecoveryReconciler(environment.Client, pg.NewDatabaseFactory())
		database = recoverDatabaseReady(ctx, environment.Client, r, database)
		generation := database.Generation
		other, err := Start(ctx, Options{Version: os.Getenv("EPCO_PG_VERSION"), RunID: postgres.RunID,
			ArtifactsDir: os.Getenv("EPCO_ARTIFACTS_DIR")})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			Expect(other.Close(cleanup)).To(Succeed())
		})
		otherFixture, err := other.NewFixture(ctx)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			Expect(otherFixture.Close(cleanup)).To(Succeed())
		})
		name := string(database.Spec.Name)
		roleSQL(ctx, otherFixture, "CREATE ROLE "+quoteIdentifier(fixture.Role)+
			" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD "+quoteLiteral(fixture.Password))
		roleSQL(ctx, otherFixture, "CREATE ROLE "+quoteIdentifier(grantee)+" NOLOGIN")
		roleSQL(ctx, otherFixture, "GRANT CONNECT ON DATABASE "+
			quoteIdentifier(otherFixture.Database)+" TO "+quoteIdentifier(fixture.Role))
		DeferCleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			roleSQL(cleanup, otherFixture, "REVOKE CONNECT ON DATABASE "+
				quoteIdentifier(otherFixture.Database)+" FROM "+quoteIdentifier(fixture.Role))
			roleSQL(cleanup, otherFixture, "DROP ROLE IF EXISTS "+quoteIdentifier(grantee))
			roleSQL(cleanup, otherFixture, "DROP ROLE IF EXISTS "+quoteIdentifier(fixture.Role))
		})
		// Advance B's catalog independently: coincidental A identities must never
		// authorize cleanup of an unrelated role/object on the selected endpoint.
		roleSQL(ctx, otherFixture, "CREATE ROLE r3_identity_padding NOLOGIN")
		roleSQL(ctx, otherFixture, "DROP ROLE r3_identity_padding")
		roleSQL(ctx, otherFixture, "CREATE DATABASE "+quoteIdentifier(name)+" OWNER "+quoteIdentifier(fixture.Role))
		recoveryDatabaseCleanup(other, name)
		cluster := &epco.PostgresCluster{}
		clusterKey := client.ObjectKey{Name: database.Spec.Cluster.Name, Namespace: database.Namespace}
		selectTarget := func(target *Fixture) {
			Expect(environment.Client.Get(ctx, clusterKey, cluster)).To(Succeed())
			cluster.Spec.Connection.Port = int32(target.Server.port)
			cluster.Spec.Connection.Database = epco.SQLName(target.Database)
			Expect(environment.Client.Update(ctx, cluster)).To(Succeed())
			ca, readErr := os.ReadFile(target.Server.TLS.CA)
			Expect(readErr).NotTo(HaveOccurred())
			secret := &corev1.Secret{}
			caKey := client.ObjectKey{Name: "ca", Namespace: database.Namespace}
			Expect(environment.Client.Get(ctx, caKey, secret)).To(Succeed())
			secret.Data[roleCAKey] = ca
			Expect(environment.Client.Update(ctx, secret)).To(Succeed())
		}
		selectTarget(otherFixture)
		database = recoverDatabaseReady(ctx, environment.Client, r, database)
		Expect(database.Generation).To(Equal(generation))
		Expect(permissionDatabaseGrant(ctx, otherFixture, name, grantee, "CREATE")).To(BeTrue())
		Expect(permissionDatabaseGrant(ctx, fixture, name, grantee, "CREATE")).To(BeTrue())
		var aRoleOID, bRoleOID int64
		Expect(fixture.Server.Admin.QueryRowContext(ctx,
			"SELECT oid FROM pg_roles WHERE rolname=$1", grantee).Scan(&aRoleOID)).To(Succeed())
		Expect(other.Admin.QueryRowContext(ctx,
			"SELECT oid FROM pg_roles WHERE rolname=$1", grantee).Scan(&bRoleOID)).To(Succeed())
		Expect(aRoleOID).NotTo(Equal(bRoleOID), "fixture must establish independently allocated identities")
		database.Spec.Permissions = nil
		Expect(environment.Client.Update(ctx, database)).To(Succeed())
		database = recoverDatabaseReady(ctx, environment.Client, r, database)
		Expect(permissionDatabaseGrant(ctx, otherFixture, name, grantee, "CREATE")).To(BeFalse())
		Expect(permissionDatabaseGrant(ctx, fixture, name, grantee, "CREATE")).To(BeTrue(), "inactive A remains untouched")
		Expect(permissionRecoveryHistory(database)).NotTo(BeEmpty(), "A cleanup responsibility survives B convergence")
		selectTarget(fixture)
		Expect(environment.Client.Get(ctx, clusterKey, cluster)).To(Succeed())
		oldUID := cluster.UID
		Expect(environment.Client.Delete(ctx, cluster)).To(Succeed())
		replacement := &epco.PostgresCluster{Name: cluster.Name, Namespace: cluster.Namespace, Spec: cluster.Spec}
		Expect(scope.Create(ctx, replacement)).To(Succeed())
		Expect(replacement.UID).NotTo(Equal(oldUID))
		recoverDatabaseReady(ctx, environment.Client,
			databaseRecoveryReconciler(environment.Client, pg.NewDatabaseFactory()), database)
		Expect(permissionDatabaseGrant(ctx, fixture, name, grantee, "CREATE")).To(BeFalse())
		Expect(permissionDatabaseGrant(ctx, otherFixture, name, grantee, "CREATE")).To(BeFalse())
	}, NodeTimeout(120*time.Second))

	It("stops broad ACL mutations at the checkpoint budget and preserves issued bindings", func(ctx SpecContext) {
		target, err := fixture.Server.Open(string(database.Spec.Name), fixture.Role, fixture.Password)
		Expect(err).NotTo(HaveOccurred())
		defer func() { Expect(target.Close()).To(Succeed()) }()
		_, err = target.ExecContext(ctx, "DO $fixture$ BEGIN FOR i IN 1..3000 LOOP "+
			"EXECUTE format('CREATE TABLE app.%I(value int)', 'relation_' || lpad(i::text, 10, '0') || repeat('x', 40)); "+
			"END LOOP; END $fixture$")
		Expect(err).NotTo(HaveOccurred())
		database = recoveredDatabase(ctx, environment.Client, database)
		database.Spec.Schemas[0].Permissions = &epco.SchemaPermissions{Tables: &epco.TablePermissions{
			Select: []epco.Permission{{Name: epco.SQLName(grantee)}},
			Insert: []epco.Permission{{Name: epco.SQLName(grantee)}}}}
		Expect(environment.Client.Update(ctx, database)).To(Succeed())
		permissionRecoveryReconcile(ctx, databaseRecoveryReconciler(environment.Client, pg.NewDatabaseFactory()), database)
		database = recoveredDatabase(ctx, environment.Client, database)
		condition := meta.FindStatusCondition(database.Status.Conditions, "Ready")
		Expect(condition).NotTo(BeNil())
		Expect(condition.Status).To(Equal(metav1.ConditionFalse))
		Expect(condition.Reason).To(Equal("StatusSizeExceeded"))
		Expect(database.Status.AppliedGeneration).NotTo(Equal(database.Generation))
		serialized, serializeErr := json.Marshal(database)
		Expect(serializeErr).NotTo(HaveOccurred())
		Expect(len(serialized)).To(BeNumerically("<=", 768*1024), "error publication retains the resource size budget")
		// Incremental bounded checkpoints may have committed earlier units. Every
		// issued unit must still have concrete persisted cleanup responsibility.
		bindings := map[int64]map[string]bool{}
		for _, item := range permissionRecoveryHistory(database) {
			binding := item.(map[string]any)
			if binding["grantee"] != grantee {
				continue
			}
			oid, ok := binding["objectOID"].(int64)
			Expect(ok).To(BeTrue())
			if bindings[oid] == nil {
				bindings[oid] = map[string]bool{}
			}
			bindings[oid][binding["privilege"].(string)] = true
		}
		rows, queryErr := target.QueryContext(ctx, "SELECT c.oid::bigint,a.privilege_type FROM pg_class c "+
			"JOIN pg_namespace n ON n.oid=c.relnamespace, LATERAL aclexplode(c.relacl) a "+
			"WHERE n.nspname='app' AND a.grantee=(SELECT oid FROM pg_roles WHERE rolname=$1)", grantee)
		Expect(queryErr).NotTo(HaveOccurred())
		defer func() { Expect(rows.Close()).To(Succeed()) }()
		for rows.Next() {
			var oid int64
			var privilege string
			Expect(rows.Scan(&oid, &privilege)).To(Succeed())
			Expect(bindings[oid][privilege]).To(BeTrue(), "each issued grant has persisted responsibility")
		}
		Expect(rows.Err()).NotTo(HaveOccurred())
	}, NodeTimeout(120*time.Second))

	It("checkpoints tracked ACL owner rewrites before changing the database owner", func(ctx SpecContext) {
		setCreate(ctx)
		database = recoverDatabaseReady(ctx, environment.Client,
			databaseRecoveryReconciler(environment.Client, pg.NewDatabaseFactory()), database)
		newOwner := roleName(fixture)
		roleSQL(ctx, fixture, "CREATE ROLE "+quoteIdentifier(newOwner)+" NOLOGIN CREATEDB")
		roleSQL(ctx, fixture, "ALTER ROLE "+quoteIdentifier(fixture.Role)+" CREATEDB")
		roleSQL(ctx, fixture, "GRANT "+quoteIdentifier(newOwner)+" TO "+quoteIdentifier(fixture.Role))
		// Dispose the database before either owner role, including retained ACLs.
		recoveryDatabaseCleanup(fixture.Server, string(database.Spec.Name))
		database.Spec.Owner = epco.SQLName(newOwner)
		Expect(environment.Client.Update(ctx, database)).To(Succeed())
		fault := &permissionRecoveryClient{Client: environment.Client}
		fault.reject = func(_ context.Context, candidate *epco.Database) bool {
			for _, item := range permissionRecoveryHistory(candidate) {
				binding := item.(map[string]any)
				if binding["pending"] == "owner" {
					return true
				}
				owner, ok := binding["desiredOwner"].(map[string]any)
				if ok && owner["name"] == newOwner {
					return true
				}
			}
			return false
		}
		r := databaseRecoveryReconciler(fault, pg.NewDatabaseFactory())
		Eventually(func() bool {
			_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(database)})
			return fault.failed.Load()
		}).WithContext(ctx).WithTimeout(5*time.Second).Should(BeTrue(), "owner intent predates ALTER DATABASE OWNER")
		var owner string
		Expect(fixture.Server.Admin.QueryRowContext(ctx, "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname=$1",
			string(database.Spec.Name)).Scan(&owner)).To(Succeed())
		Expect(owner).To(Equal(fixture.Role))
		Expect(permissionDatabaseGrant(ctx, fixture, string(database.Spec.Name), grantee, "CREATE")).To(BeTrue())
		database = recoverDatabaseReady(ctx, environment.Client,
			databaseRecoveryReconciler(environment.Client, pg.NewDatabaseFactory()), database)
		Expect(fixture.Server.Admin.QueryRowContext(ctx, "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname=$1",
			string(database.Spec.Name)).Scan(&owner)).To(Succeed())
		Expect(owner).To(Equal(newOwner))
		database.Spec.Permissions = nil
		Expect(environment.Client.Update(ctx, database)).To(Succeed())
		recoverDatabaseReady(ctx, environment.Client,
			databaseRecoveryReconciler(environment.Client, pg.NewDatabaseFactory()), database)
		Expect(permissionDatabaseGrant(ctx, fixture, string(database.Spec.Name), grantee, "CREATE")).To(BeFalse())
	}, NodeTimeout(40*time.Second))
})
