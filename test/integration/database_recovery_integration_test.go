//go:build integration

package integration

import (
	"context"
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
	pgdatabases "github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	kubeenv "github.com/vterdunov/external-postgres-control-operator/test/envtest"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const databaseRecoveryApplicationName = "app"

type databaseRecoveryClient struct {
	client.Client
	reject atomic.Bool
	failed atomic.Int32
}

type databaseRecoveryWriter struct {
	client.SubResourceWriter
	owner *databaseRecoveryClient
}

func (c *databaseRecoveryClient) Status() client.SubResourceWriter {
	return &databaseRecoveryWriter{SubResourceWriter: c.Client.Status(), owner: c}
}

func (w *databaseRecoveryWriter) fail(object client.Object) error {
	if _, ok := object.(*epco.Database); ok && w.owner.reject.Load() {
		w.owner.failed.Add(1)
		return errors.New("injected Database status persistence failure")
	}
	return nil
}

func (w *databaseRecoveryWriter) Update(ctx context.Context, object client.Object,
	options ...client.SubResourceUpdateOption) error {
	if err := w.fail(object); err != nil {
		return err
	}
	return w.SubResourceWriter.Update(ctx, object, options...)
}

func (w *databaseRecoveryWriter) Patch(ctx context.Context, object client.Object,
	patch client.Patch, options ...client.SubResourcePatchOption) error {
	if err := w.fail(object); err != nil {
		return err
	}
	return w.SubResourceWriter.Patch(ctx, object, patch, options...)
}

type databaseRecoveryFactory struct {
	delegate pgdatabases.DatabaseFactory
	after    func()
	unknown  bool
}

type databaseRecoverySession struct {
	pgdatabases.DatabaseSession
	owner *databaseRecoveryFactory
}

func (f *databaseRecoveryFactory) Open(ctx context.Context,
	config *pgdatabases.Config, options pgdatabases.DatabaseSessionOptions) (pgdatabases.DatabaseSession, error) {
	session, err := f.delegate.Open(ctx, config, options)
	if err != nil {
		return nil, err
	}
	return &databaseRecoverySession{DatabaseSession: session, owner: f}, nil
}

func (s *databaseRecoverySession) Apply(ctx context.Context,
	desired pgdatabases.DatabaseDesired) (*pgdatabases.DatabaseApplyResult, error) {
	result, err := s.DatabaseSession.Apply(ctx, desired)
	if err == nil {
		if s.owner.after != nil {
			s.owner.after()
		}
		if s.owner.unknown {
			return nil, &pgdatabases.Failure{Reason: pgdatabases.ConnectionUnavailable,
				Message: "injected lost SQL response after committed database creation"}
		}
	}
	return result, err
}

func databaseRecoveryReconciler(api client.Client, factory pgdatabases.DatabaseFactory) *controller.DatabaseReconciler {
	builder, err := pgdatabases.NewConfigBuilder()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return &controller.DatabaseReconciler{Client: api, APIReader: api, Builder: builder,
		Databases: factory, Locks: &controller.ManagementLocks{}}
}

func recoveredDatabase(ctx context.Context, api client.Client, database *epco.Database) *epco.Database {
	current := &epco.Database{}
	ExpectWithOffset(1, api.Get(ctx, client.ObjectKeyFromObject(database), current)).To(Succeed())
	return current
}

func recoverDatabaseReady(ctx context.Context, api client.Client,
	reconciler *controller.DatabaseReconciler, database *epco.Database) *epco.Database {
	Eventually(func(g Gomega) {
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(database)})
		g.Expect(err).NotTo(HaveOccurred())
		current := &epco.Database{}
		g.Expect(api.Get(ctx, client.ObjectKeyFromObject(database), current)).To(Succeed())
		condition := meta.FindStatusCondition(current.Status.Conditions, "Ready")
		g.Expect(condition).NotTo(BeNil())
		g.Expect(condition.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(condition.ObservedGeneration).To(Equal(current.Generation))
		g.Expect(current.Status.HasManagedSQL).To(BeTrue())
	}).WithContext(ctx).WithTimeout(10 * time.Second).Should(Succeed())
	return recoveredDatabase(ctx, api, database)
}

func recoveryDatabaseGone(ctx context.Context, api client.Client,
	reconciler *controller.DatabaseReconciler, database *epco.Database) {
	ExpectWithOffset(1, api.Delete(ctx, database)).To(Succeed())
	Eventually(func(g Gomega) {
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(database)})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(apierrors.IsNotFound(api.Get(ctx, client.ObjectKeyFromObject(database), &epco.Database{}))).To(BeTrue())
	}).WithContext(ctx).WithTimeout(10 * time.Second).Should(Succeed())
}

func recoveryDatabaseExists(ctx context.Context, server *Server, name string) bool {
	exists, err := server.DatabaseExists(ctx, name)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return exists
}

// Privileged cleanup targets only the database allocated by this spec. Its FORCE
// is fixture teardown; blocked production DROP is asserted separately below.
func recoveryDatabaseCleanup(server *Server, name string) {
	DeferCleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		Expect(server.cleanupOwnedSQL(ctx, []string{name}, nil)).To(Succeed())
	})
}

var _ = Describe("Database combined API and PostgreSQL recovery", Ordered, func() {
	var environment *kubeenv.Environment
	var scope *kubeenv.Scope
	var fixture *Fixture
	var cluster *epco.ExternalPostgres
	var database *epco.Database

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
		scope, _, err = environment.NewScope(ctx, "r2-recovery-")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			Expect(scope.Cleanup(cleanup)).To(Succeed())
		})
		cluster = recoveryCluster(ctx, scope, fixture)
		owner := roleName(fixture)
		roleSQL(ctx, fixture, "CREATE ROLE "+quoteIdentifier(owner)+" NOLOGIN")
		name, err := randomName("recovery_db_")
		Expect(err).NotTo(HaveOccurred())
		recoveryDatabaseCleanup(fixture.Server, name)
		database = &epco.Database{Name: databaseRecoveryApplicationName, Namespace: scope.Namespace.Name,
			Spec: epco.DatabaseSpec{Name: epco.SQLName(name), Owner: epco.SQLName(owner),
				Cluster: epco.ClusterReference{Name: cluster.Name}, ReclaimPolicy: epco.ReclaimDelete,
				Schemas: []epco.SchemaSpec{{Name: databaseRecoveryApplicationName}}}}
	}, NodeTimeout(30*time.Second))

	DescribeTable("recovers a committed CREATE without a successful first status record after restart",
		func(ctx SpecContext, unknown bool) {
			Expect(scope.Create(ctx, database)).To(Succeed())
			fault := &databaseRecoveryClient{Client: environment.Client}
			factory := &databaseRecoveryFactory{delegate: pgdatabases.NewDatabaseFactory(), unknown: unknown,
				after: func() { fault.reject.Store(true) }}
			reconciler := databaseRecoveryReconciler(fault, factory)
			Eventually(func() bool {
				_, _ = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(database)})
				return fault.failed.Load() > 0
			}).WithContext(ctx).WithTimeout(10 * time.Second).Should(BeTrue())
			current := recoveredDatabase(ctx, environment.Client, database)
			Expect(current.Status.HasManagedSQL).To(BeFalse())
			Expect(recoveryDatabaseExists(ctx, fixture.Server, string(database.Spec.Name))).To(BeTrue())
			target, err := fixture.Server.Open(string(database.Spec.Name), roleSQLAdmin, fixture.Server.password)
			Expect(err).NotTo(HaveOccurred())
			_, err = target.ExecContext(ctx, "CREATE TABLE app.sentinel(value int); INSERT INTO app.sentinel VALUES(42)")
			Expect(err).NotTo(HaveOccurred())
			Expect(target.Close()).To(Succeed())
			restarted := databaseRecoveryReconciler(environment.Client, pgdatabases.NewDatabaseFactory())
			current = recoverDatabaseReady(ctx, environment.Client, restarted, database)
			Expect(current.Status.EffectiveName).To(Equal(string(database.Spec.Name)))
			target, err = fixture.Server.Open(string(database.Spec.Name), roleSQLAdmin, fixture.Server.password)
			Expect(err).NotTo(HaveOccurred())
			defer func() { Expect(target.Close()).To(Succeed()) }()
			var value int
			Expect(target.QueryRowContext(ctx, "SELECT value FROM app.sentinel").Scan(&value)).To(Succeed())
			Expect(value).To(Equal(42), "ordinary retry must preserve the created database and its data")
		},
		Entry("successful SQL and failed status write", false, NodeTimeout(35*time.Second)),
		Entry("unknown SQL outcome and failed status write", true, NodeTimeout(35*time.Second)),
	)

	It("retains a SQL orphan when deletion precedes the first successful status publication", func(ctx SpecContext) {
		Expect(scope.Create(ctx, database)).To(Succeed())
		fault := &databaseRecoveryClient{Client: environment.Client}
		factory := &databaseRecoveryFactory{delegate: pgdatabases.NewDatabaseFactory(),
			after: func() { fault.reject.Store(true) }}
		reconciler := databaseRecoveryReconciler(fault, factory)
		Eventually(func() bool {
			_, _ = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(database)})
			return fault.failed.Load() > 0
		}).WithContext(ctx).WithTimeout(10 * time.Second).Should(BeTrue())
		current := recoveredDatabase(ctx, environment.Client, database)
		Expect(current.Status.HasManagedSQL).To(BeFalse())
		Expect(recoveryDatabaseExists(ctx, fixture.Server, string(database.Spec.Name))).To(BeTrue())
		recoveryDatabaseGone(ctx, environment.Client,
			databaseRecoveryReconciler(environment.Client, pgdatabases.NewDatabaseFactory()), current)
		Expect(recoveryDatabaseExists(ctx, fixture.Server, string(database.Spec.Name))).To(BeTrue(),
			"reclaim delete has no authority without recorded successful management")
	}, NodeTimeout(30*time.Second))

	It("reserves an absent name and prevents losing contenders from dropping external SQL", func(ctx SpecContext) {
		database.Spec.Ensure = epco.EnsureAbsent
		database.Spec.ReclaimPolicy = epco.ReclaimRetain
		Expect(scope.Create(ctx, database)).To(Succeed())
		reconciler := databaseRecoveryReconciler(environment.Client, pgdatabases.NewDatabaseFactory())
		current := recoverDatabaseReady(ctx, environment.Client, reconciler, database)
		Expect(recoveryDatabaseExists(ctx, fixture.Server, string(database.Spec.Name))).To(BeFalse())
		_, err := fixture.Server.Admin.ExecContext(ctx, "CREATE DATABASE "+quoteIdentifier(string(database.Spec.Name))+
			" OWNER "+quoteIdentifier(string(database.Spec.Owner)))
		Expect(err).NotTo(HaveOccurred())
		loser := &epco.Database{Name: "loser", Namespace: scope.Namespace.Name, Spec: current.Spec}
		loser.Spec.Ensure = epco.EnsurePresent
		loser.Spec.ReclaimPolicy = epco.ReclaimDelete
		Expect(scope.Create(ctx, loser)).To(Succeed())
		Eventually(func(g Gomega) {
			_, reconcileErr := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(loser)})
			g.Expect(reconcileErr).NotTo(HaveOccurred())
			latest := recoveredDatabase(ctx, environment.Client, loser)
			condition := meta.FindStatusCondition(latest.Status.Conditions, "Ready")
			g.Expect(condition).NotTo(BeNil())
			g.Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(condition.Reason).To(Equal("OwnershipConflict"))
			g.Expect(latest.Status.HasManagedSQL).To(BeFalse())
		}).WithContext(ctx).WithTimeout(10 * time.Second).Should(Succeed())
		recoveryDatabaseGone(ctx, environment.Client, reconciler, loser)
		Expect(recoveryDatabaseExists(ctx, fixture.Server, string(database.Spec.Name))).To(BeTrue())
		recoveryDatabaseGone(ctx, environment.Client, reconciler, current)
		Expect(recoveryDatabaseExists(ctx, fixture.Server, string(database.Spec.Name))).To(BeTrue())
	}, NodeTimeout(30*time.Second))

	It("rechecks A to B to A and a recreated descriptor, preserving former servers", func(ctx SpecContext) {
		Expect(scope.Create(ctx, database)).To(Succeed())
		reconciler := databaseRecoveryReconciler(environment.Client, pgdatabases.NewDatabaseFactory())
		recoverDatabaseReady(ctx, environment.Client, reconciler, database)
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
		roleSQL(ctx, otherFixture, "CREATE ROLE "+quoteIdentifier(string(database.Spec.Owner))+" NOLOGIN")
		recoveryDatabaseCleanup(other, string(database.Spec.Name))
		setEndpoint := func(target *Fixture) {
			latest := &epco.ExternalPostgres{}
			Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(cluster), latest)).To(Succeed())
			latest.Spec.Connection.Port = int32(target.Server.port)
			latest.Spec.Connection.Database = epco.SQLName(target.Database)
			Expect(environment.Client.Update(ctx, latest)).To(Succeed())
			for secretName, key := range map[string]string{recoveryAdminSecret: corev1.BasicAuthPasswordKey, "ca": roleCAKey} {
				secret := &corev1.Secret{}
				secretKey := client.ObjectKey{Name: secretName, Namespace: scope.Namespace.Name}
				Expect(environment.Client.Get(ctx, secretKey, secret)).To(Succeed())
				value := []byte(target.Server.password)
				if secretName == "ca" {
					value, err = os.ReadFile(target.Server.TLS.CA)
					Expect(err).NotTo(HaveOccurred())
				}
				secret.Data[key] = value
				Expect(environment.Client.Update(ctx, secret)).To(Succeed())
			}
		}
		setEndpoint(otherFixture)
		recoverDatabaseReady(ctx, environment.Client, reconciler, database)
		Expect(recoveryDatabaseExists(ctx, other, string(database.Spec.Name))).To(BeTrue())
		Expect(recoveryDatabaseExists(ctx, fixture.Server, string(database.Spec.Name))).To(BeTrue())
		current := recoveredDatabase(ctx, environment.Client, database)
		current.Spec.ConnectionLimit = new(int64(7))
		Expect(environment.Client.Update(ctx, current)).To(Succeed())
		recoverDatabaseReady(ctx, environment.Client, reconciler, database)
		setEndpoint(fixture)
		latest := &epco.ExternalPostgres{}
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(cluster), latest)).To(Succeed())
		oldUID := latest.UID
		Expect(environment.Client.Delete(ctx, latest)).To(Succeed())
		recreated := &epco.ExternalPostgres{Name: latest.Name, Namespace: latest.Namespace, Spec: latest.Spec}
		Expect(scope.Create(ctx, recreated)).To(Succeed())
		Expect(recreated.UID).NotTo(Equal(oldUID))
		restarted := databaseRecoveryReconciler(environment.Client, pgdatabases.NewDatabaseFactory())
		recoverDatabaseReady(ctx, environment.Client, restarted, database)
		for _, server := range []*Server{fixture.Server, other} {
			var limit int
			Expect(server.Admin.QueryRowContext(ctx, "SELECT datconnlimit FROM pg_database WHERE datname=$1",
				string(database.Spec.Name)).Scan(&limit)).To(Succeed())
			Expect(limit).To(Equal(7))
		}
		current = recoveredDatabase(ctx, environment.Client, database)
		recoveryDatabaseGone(ctx, environment.Client, restarted, current)
		Expect(recoveryDatabaseExists(ctx, fixture.Server, string(database.Spec.Name))).To(BeFalse())
		Expect(recoveryDatabaseExists(ctx, other, string(database.Spec.Name))).To(BeTrue(),
			"finalizer uses only the current target")
	}, NodeTimeout(120*time.Second))
})
