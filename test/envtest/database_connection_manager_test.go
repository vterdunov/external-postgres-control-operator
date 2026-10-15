//go:build envtest

package envtest_test

import (
	"bytes"
	"context"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/controller"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	harness "github.com/vterdunov/external-postgres-control-operator/test/envtest"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const (
	connectionAPIUsernameKey       = "username"
	connectionAPIStatusSubresource = "status"
	connectionAPIRoleName          = "application-role-resource"
	connectionAPISQLRole           = "application_login"
	connectionAPISource            = "application-source"
)

type connectionAPIOfflineFactory struct{}

func (connectionAPIOfflineFactory) Open(context.Context,
	*postgres.Config, postgres.DatabaseSessionOptions,
) (postgres.DatabaseSession, error) {
	return nil, &postgres.Failure{Reason: postgres.DependenciesNotReady, Message: "isolated API test SQL outage"}
}
func connectionAPIRequestFor(object client.Object) ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKeyFromObject(object)}
}

type connectionAPIFactory struct {
	databaseWatchFactory
	connectionMu sync.Mutex
	inspections  map[string]int
}

type connectionAPISession struct {
	postgres.DatabaseSession
	factory *connectionAPIFactory
}

func (f *connectionAPIFactory) Open(ctx context.Context,
	config *postgres.Config, options postgres.DatabaseSessionOptions,
) (postgres.DatabaseSession, error) {
	session, err := f.databaseWatchFactory.Open(ctx, config, options)
	if err != nil {
		return nil, err
	}
	return &connectionAPISession{DatabaseSession: session, factory: f}, nil
}

func (f *connectionAPIFactory) count(name string) int {
	f.connectionMu.Lock()
	defer f.connectionMu.Unlock()
	return f.inspections[name]
}

func (s *connectionAPISession) Inspect(ctx context.Context, name string) (*postgres.DatabaseState, error) {
	s.factory.connectionMu.Lock()
	if s.factory.inspections == nil {
		s.factory.inspections = make(map[string]int)
	}
	s.factory.inspections[name]++
	s.factory.connectionMu.Unlock()
	return s.DatabaseSession.Inspect(ctx, name)
}
func (*connectionAPISession) CheckConnection(context.Context, string, string) error { return nil }
func (*connectionAPISession) PreflightACL(context.Context, postgres.DatabaseDesired) error {
	return nil
}
func (*connectionAPISession) PrepareACL(_ context.Context, desired postgres.ACLDesired) (*postgres.ACLResult, error) {
	return &postgres.ACLResult{Targets: desired.Targets}, nil
}
func (*connectionAPISession) ReconcileACL(_ context.Context, desired postgres.ACLDesired) (*postgres.ACLResult, error) {
	return &postgres.ACLResult{Targets: desired.Targets}, nil
}

func startConnectionAPIManager(ctx context.Context, factory postgres.DatabaseFactory) {
	builder, err := postgres.NewConfigBuilder()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	instance, err := environment.StartManager(ctx, harness.ManagerOptions{
		CachedObjects: []client.Object{
			&epco.Database{}, &epco.DatabaseRole{}, &epco.PostgresCluster{}, &corev1.Secret{}, &corev1.Namespace{},
		},
		Register: func(mgr manager.Manager) error {
			return (&controller.DatabaseReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(),
				Builder: builder, Databases: factory, Locks: &controller.ManagementLocks{}}).SetupWithManager(mgr)
		},
	})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		Expect(instance.Stop(stopCtx)).To(Succeed())
	})
}

func connectionAPIFixture(ctx context.Context) (*epco.PostgresCluster, *epco.DatabaseRole, *corev1.Secret) {
	cluster := createRoleDescriptor(ctx, nil)
	role := &epco.DatabaseRole{Namespace: namespace.Name, Name: connectionAPIRoleName,
		Spec: epco.DatabaseRoleSpec{Cluster: epco.ClusterReference{Namespace: namespace.Name, Name: cluster.Name},
			Name: connectionAPISQLRole, Login: true, PasswordSecret: &epco.LocalReference{Name: connectionAPISource}}}
	source := &corev1.Secret{Namespace: namespace.Name, Name: connectionAPISource, Type: corev1.SecretTypeBasicAuth,
		Data: map[string][]byte{
			connectionAPIUsernameKey: []byte(connectionAPISQLRole), passwordKey: []byte("initial-api-application-value"),
		}}
	ExpectWithOffset(1, scope.Create(ctx, role)).To(Succeed())
	ExpectWithOffset(1, scope.Create(ctx, source)).To(Succeed())
	return cluster, role, source
}

func connectionAPIRoleReconciler() *controller.DatabaseRoleReconciler {
	builder, err := postgres.NewConfigBuilder()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return &controller.DatabaseRoleReconciler{Client: environment.Client, APIReader: environment.Client,
		Builder: builder, Roles: &watchedRoleFactory{}, Locks: &controller.ManagementLocks{}}
}

func connectionAPIApplyRole(ctx context.Context, r *controller.DatabaseRoleReconciler, role *epco.DatabaseRole) {
	_, err := r.Reconcile(ctx, connectionAPIRequestFor(role))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, environment.Client.Get(ctx, client.ObjectKeyFromObject(role), role)).To(Succeed())
	condition := meta.FindStatusCondition(role.Status.Conditions, "Ready")
	ExpectWithOffset(1, condition).NotTo(BeNil())
	ExpectWithOffset(1, condition.Status).To(Equal(metav1.ConditionTrue))
}

func connectionAPIDatabase(cluster *epco.PostgresCluster, name string) *epco.Database {
	db := watchedDatabase(cluster, name)
	db.Spec.Name = epco.SQLName(name + "_sql")
	db.Spec.ConnectionSecrets = []epco.ConnectionSecret{{
		Name: name + "-connection", RoleRef: epco.LocalReference{Name: connectionAPIRoleName},
	}}
	return db
}

func connectionAPIOutputKey(db *epco.Database) client.ObjectKey {
	return client.ObjectKey{Namespace: db.Namespace, Name: db.Spec.ConnectionSecrets[0].Name}
}

func connectionAPIOutput(ctx context.Context, db *epco.Database) *corev1.Secret {
	secret := &corev1.Secret{}
	ExpectWithOffset(1, environment.Client.Get(ctx, connectionAPIOutputKey(db), secret)).To(Succeed())
	return secret
}

func connectionAPIExpectPassword(ctx context.Context, db *epco.Database, password []byte) {
	Eventually(func() bool {
		output := &corev1.Secret{}
		err := environment.Client.Get(ctx, connectionAPIOutputKey(db), output)
		return err == nil && bytes.Equal(output.Data[passwordKey], password)
	}).WithContext(ctx).WithTimeout(10*time.Second).Should(BeTrue(),
		"output must contain the acknowledged application password")
}

var _ = Describe("R4 Database connection real API watches", func() {
	It("fans out same-generation source and SQL acknowledgement changes "+
		"by local roleRef resource name", func(ctx SpecContext) {
		cluster, role, source := connectionAPIFixture(ctx)
		roleReconciler := connectionAPIRoleReconciler()
		connectionAPIApplyRole(ctx, roleReconciler, role)
		initialRoleGeneration := role.Generation
		initialReady := meta.FindStatusCondition(role.Status.Conditions, "Ready").Status
		factory := &connectionAPIFactory{}
		startConnectionAPIManager(ctx, factory)
		databases := []*epco.Database{
			connectionAPIDatabase(cluster, "first-app"), connectionAPIDatabase(cluster, "second-app"),
		}
		for _, db := range databases {
			Expect(scope.Create(ctx, db)).To(Succeed())
			connectionAPIExpectPassword(ctx, db, source.Data[passwordKey])
			expectDatabaseStatus(ctx, db, metav1.ConditionTrue, "Ready")
		}
		before := []int{factory.count(string(databases[0].Spec.Name)), factory.count(string(databases[1].Spec.Name))}
		source.Data[passwordKey] = []byte("second-api-application-value")
		Expect(environment.Client.Update(ctx, source)).To(Succeed())
		// Wait for each Database's source watch before acknowledging SQL. This
		// prevents the initial source event from masking a missing role-status watch.
		for i, db := range databases {
			Eventually(func() bool { return factory.count(string(db.Spec.Name)) > before[i] }).
				WithContext(ctx).WithTimeout(10*time.Second).Should(BeTrue(),
				"source update must enqueue every referencing Database")
			Expect(bytes.Equal(connectionAPIOutput(ctx, db).Data[passwordKey],
				[]byte("initial-api-application-value"))).To(BeTrue(),
				"unacknowledged source rotation must preserve the output")
		}
		connectionAPIApplyRole(ctx, roleReconciler, role)
		Expect(role.Generation).To(Equal(initialRoleGeneration))
		Expect(meta.FindStatusCondition(role.Status.Conditions, "Ready").Status).To(Equal(initialReady))
		Expect(role.Status.SecretResourceVersion).To(Equal(source.ResourceVersion))
		for _, db := range databases {
			connectionAPIExpectPassword(ctx, db, source.Data[passwordKey])
			latest := expectDatabaseStatus(ctx, db, metav1.ConditionTrue, "Ready")
			Expect(latest.Generation).To(Equal(db.Generation),
				"receipt status watch must reconcile without changing Database generation")
		}
		oldUID := connectionAPIOutput(ctx, databases[0]).UID
		Expect(environment.Client.Delete(ctx, connectionAPIOutput(ctx, databases[0]))).To(Succeed())
		connectionAPIExpectPassword(ctx, databases[0], source.Data[passwordKey])
		Expect(connectionAPIOutput(ctx, databases[0]).UID).NotTo(Equal(oldUID),
			"output deletion watch must recreate a distinct Secret after SQL acknowledgement")

		// Source deletion preserves both outputs and prevents refresh.
		Expect(environment.Client.Delete(ctx, source)).To(Succeed())
		for _, db := range databases {
			Eventually(func() bool {
				latest := &epco.Database{}
				if environment.Client.Get(ctx, client.ObjectKeyFromObject(db), latest) != nil {
					return false
				}
				condition := meta.FindStatusCondition(latest.Status.Conditions, "Ready")
				return condition != nil && condition.Status == metav1.ConditionFalse
			}).WithContext(ctx).WithTimeout(10 * time.Second).Should(BeTrue())
			Expect(bytes.Equal(connectionAPIOutput(ctx, db).Data[passwordKey], source.Data[passwordKey])).To(BeTrue(),
				"source deletion must freeze the existing output")
		}
	}, NodeTimeout(90*time.Second))

	It("watches a replacement password source through the local roleRef", func(ctx SpecContext) {
		cluster, role, source := connectionAPIFixture(ctx)
		roleReconciler := connectionAPIRoleReconciler()
		connectionAPIApplyRole(ctx, roleReconciler, role)
		factory := &connectionAPIFactory{}
		startConnectionAPIManager(ctx, factory)
		db := connectionAPIDatabase(cluster, "changed-source")
		Expect(scope.Create(ctx, db)).To(Succeed())
		connectionAPIExpectPassword(ctx, db, source.Data[passwordKey])

		newSource := &corev1.Secret{Namespace: namespace.Name, Name: "replacement-password-source",
			Type: corev1.SecretTypeBasicAuth,
			Data: map[string][]byte{connectionAPIUsernameKey: []byte(connectionAPISQLRole),
				passwordKey: []byte("replacement-source-value")}}
		Expect(scope.Create(ctx, newSource)).To(Succeed())
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(role), role)).To(Succeed())
		role.Spec.PasswordSecret = &epco.LocalReference{Name: newSource.Name}
		Expect(environment.Client.Update(ctx, role)).To(Succeed())
		connectionAPIApplyRole(ctx, roleReconciler, role)
		connectionAPIExpectPassword(ctx, db, newSource.Data[passwordKey])
		before := factory.count(string(db.Spec.Name))
		newSource.Data[passwordKey] = []byte("replacement-source-rotated-value")
		Expect(environment.Client.Update(ctx, newSource)).To(Succeed())
		Eventually(func() bool { return factory.count(string(db.Spec.Name)) > before }).
			WithContext(ctx).WithTimeout(10*time.Second).Should(BeTrue(), "replacement source must be watched through roleRef")
		connectionAPIApplyRole(ctx, roleReconciler, role)
		connectionAPIExpectPassword(ctx, db, newSource.Data[passwordKey])
	}, NodeTimeout(60*time.Second))

	It("does not adopt or overwrite a retained output while its former Database UID is active", func(ctx SpecContext) {
		cluster, _, _ := connectionAPIFixture(ctx)
		builder, err := postgres.NewConfigBuilder()
		Expect(err).NotTo(HaveOccurred())
		db := connectionAPIDatabase(cluster, "original-manager")
		Expect(scope.Create(ctx, db)).To(Succeed())
		reconciler := &controller.DatabaseReconciler{Client: environment.Client, APIReader: environment.Client,
			Builder: builder, Databases: connectionAPIOfflineFactory{}, Locks: &controller.ManagementLocks{}}
		_, err = reconciler.Reconcile(ctx, connectionAPIRequestFor(db))
		Expect(err).NotTo(HaveOccurred())
		before := connectionAPIOutput(ctx, db).DeepCopy()
		contender := connectionAPIDatabase(cluster, "another-manager")
		contender.Spec.ConnectionSecrets[0].Name = db.Spec.ConnectionSecrets[0].Name
		Expect(scope.Create(ctx, contender)).To(Succeed())
		_, err = reconciler.Reconcile(ctx, connectionAPIRequestFor(contender))
		Expect(err).NotTo(HaveOccurred())
		latest := &epco.Database{}
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(contender), latest)).To(Succeed())
		condition := meta.FindStatusCondition(latest.Status.Conditions, "Ready")
		Expect(condition).NotTo(BeNil())
		Expect(condition.Reason).To(Equal("ConnectionSecretConflict"))
		after := connectionAPIOutput(ctx, db)
		Expect(after.UID).To(Equal(before.UID))
		Expect(bytes.Equal(after.Data[passwordKey], before.Data[passwordKey])).To(BeTrue())
		Expect(environment.Client.Delete(ctx, contender)).To(Succeed())
		_, err = reconciler.Reconcile(ctx, connectionAPIRequestFor(contender))
		Expect(err).NotTo(HaveOccurred())
		Expect(apierrors.IsNotFound(environment.Client.Get(
			ctx, client.ObjectKeyFromObject(contender), &epco.Database{}))).To(BeTrue())
		Expect(connectionAPIOutput(ctx, db).UID).To(Equal(before.UID),
			"losing contender deletion must preserve winner's output")
	}, NodeTimeout(30*time.Second))
})
