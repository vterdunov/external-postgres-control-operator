//go:build envtest

package envtest_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/controller"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	harness "github.com/vterdunov/external-postgres-control-operator/test/envtest"
	"github.com/vterdunov/external-postgres-control-operator/test/integration"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const (
	databaseTestOwnerName      = "owner"
	databaseTestCAKey          = "root"
	databaseTestReadyCondition = "Ready"
)

type databaseWatchFactory struct {
	mu             sync.Mutex
	host, password string
	applies, drops int
	exists         map[string]bool
	owner          string
}

func (f *databaseWatchFactory) Open(_ context.Context, c *postgres.Config) (postgres.DatabaseSession, error) {
	f.mu.Lock()
	f.host = c.Connection.Host
	f.password = c.Connection.Password
	f.mu.Unlock()
	return &databaseWatchSession{f: f}, nil
}
func (f *databaseWatchFactory) snapshot() (string, string, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.host, f.password, f.applies, f.drops
}

type databaseWatchSession struct{ f *databaseWatchFactory }

func (s *databaseWatchSession) Inspect(_ context.Context, name string) (*postgres.DatabaseState, error) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	exists := true
	if value, ok := s.f.exists[name]; ok {
		exists = value
	}
	owner := s.f.owner
	if owner == "" {
		owner = databaseTestOwnerName
	}
	return &postgres.DatabaseState{
		Exists: exists, Name: name, Owner: owner, AdminUser: roleTestAdmin, AdminDatabase: "postgres",
		MaxIdentifierLength: 63, ServerVersion: 180000, AllowConnections: true, ConnectionLimit: -1,
	}, nil
}
func (*databaseWatchSession) InspectSchemas(_ context.Context, _ string,
	spec []epco.SchemaSpec) ([]postgres.SchemaState, error) {
	states := make([]postgres.SchemaState, 0, len(spec))
	for _, schema := range spec {
		states = append(states, postgres.SchemaState{
			Name: string(schema.Name), Exists: schema.Ensure != epco.EnsureAbsent, Owner: string(schema.Owner),
		})
	}
	return states, nil
}
func (s *databaseWatchSession) Apply(_ context.Context,
	d postgres.DatabaseDesired) (*postgres.DatabaseApplyResult, error) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	s.f.applies++
	if s.f.exists == nil {
		s.f.exists = make(map[string]bool)
	}
	s.f.exists[d.Name] = true
	s.f.owner = string(d.Spec.Owner)
	return &postgres.DatabaseApplyResult{}, nil
}
func (s *databaseWatchSession) Drop(_ context.Context, name string) error {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	s.f.drops++
	if s.f.exists == nil {
		s.f.exists = make(map[string]bool)
	}
	s.f.exists[name] = false
	return nil
}
func (*databaseWatchSession) Close(context.Context) error { return nil }
func startDatabaseManager(ctx context.Context, f *databaseWatchFactory) {
	startDatabaseManagerWithClient(ctx, f, nil)
}
func startDatabaseManagerWithClient(ctx context.Context, f *databaseWatchFactory,
	decorate func(client.Client) client.Client) client.Client {
	builder, err := postgres.NewConfigBuilder()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	var cached client.Client
	instance, err := environment.StartManager(ctx, harness.ManagerOptions{
		CachedObjects: []client.Object{
			&epco.Database{}, &epco.DatabaseRole{}, &epco.PostgresCluster{}, &corev1.Secret{}, &corev1.Namespace{},
		},
		Register: func(mgr manager.Manager) error {
			cached = mgr.GetClient()
			reconcilerClient := cached
			if decorate != nil {
				reconcilerClient = decorate(cached)
			}
			return (&controller.DatabaseReconciler{
				Client: reconcilerClient, APIReader: mgr.GetAPIReader(), Builder: builder,
				Databases: f, Locks: &controller.ManagementLocks{},
			}).SetupWithManager(mgr)
		},
	})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		Expect(instance.Stop(stopCtx)).To(Succeed())
	})
	return cached
}

// Record returned candidates, without depending on selector spelling or call ordering.
type databaseOwnerCandidateClient struct {
	client.Client
	mu             sync.Mutex
	lists, maximum int
	candidates     map[client.ObjectKey]bool
}

func (c *databaseOwnerCandidateClient) List(ctx context.Context, objects client.ObjectList,
	options ...client.ListOption) error {
	if err := c.Client.List(ctx, objects, options...); err != nil {
		return err
	}
	if roles, ok := objects.(*epco.DatabaseRoleList); ok {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.lists++
		c.maximum = max(c.maximum, len(roles.Items))
		for _, role := range roles.Items {
			c.candidates[client.ObjectKeyFromObject(&role)] = true
		}
	}
	return nil
}

func (c *databaseOwnerCandidateClient) snapshot() (int, int, []client.ObjectKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := make([]client.ObjectKey, 0, len(c.candidates))
	for key := range c.candidates {
		keys = append(keys, key)
	}
	return c.lists, c.maximum, keys
}
func watchedDatabase(cluster *epco.PostgresCluster, name string) *epco.Database {
	return &epco.Database{Namespace: namespace.Name, Name: name,
		Spec: epco.DatabaseSpec{Cluster: epco.ClusterReference{Name: cluster.Name}, Owner: databaseTestOwnerName}}
}
func expectDatabaseStatus(ctx context.Context, db *epco.Database,
	status metav1.ConditionStatus, reason string) *epco.Database {
	var latest *epco.Database
	Eventually(func(g Gomega) {
		latest = &epco.Database{}
		g.Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(db), latest)).To(Succeed())
		c := meta.FindStatusCondition(latest.Status.Conditions, databaseTestReadyCondition)
		g.Expect(c).NotTo(BeNil())
		g.Expect(c).To(SatisfyAll(
			HaveField("Status", status), HaveField("Reason", reason),
			HaveField("ObservedGeneration", latest.Generation),
		))
	}).WithContext(ctx).WithTimeout(10 * time.Second).Should(Succeed())
	return latest
}
func expectDatabaseGone(ctx context.Context, db *epco.Database) {
	Eventually(func() bool {
		return apierrors.IsNotFound(environment.Client.Get(ctx, client.ObjectKeyFromObject(db), &epco.Database{}))
	}).WithContext(ctx).WithTimeout(10 * time.Second).Should(BeTrue())
}

var _ = Describe("Database manager watches", func() {
	DescribeTable("bounds cached owner-role candidates by canonical descriptor and effective SQL name",
		func(ctx SpecContext, explicitName, crossNamespace bool) {
			cluster := createRoleDescriptor(ctx, &epco.ClusterAccess{NamespaceSelector: &metav1.LabelSelector{}})
			otherScope, otherNS, err := environment.NewScope(ctx, "epco-db-owner-index-")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func(ctx SpecContext) { Expect(otherScope.Cleanup(ctx)).To(Succeed()) })
			otherCluster := &epco.PostgresCluster{Namespace: otherNS.Name, Name: cluster.Name, Spec: cluster.Spec}
			Expect(otherScope.Create(ctx, otherCluster)).To(Succeed())
			for i := range 8 {
				unrelated := &epco.PostgresCluster{Namespace: namespace.Name,
					Name: fmt.Sprintf("unrelated-descriptor-%d", i), Spec: cluster.Spec}
				Expect(scope.Create(ctx, unrelated)).To(Succeed())
				for j := range 2 {
					role := &epco.DatabaseRole{Namespace: namespace.Name,
						Name: fmt.Sprintf("other-descriptor-%d-%d", i, j),
						Spec: epco.DatabaseRoleSpec{Cluster: epco.ClusterReference{Name: unrelated.Name},
							Name: databaseTestOwnerName}}
					Expect(scope.Create(ctx, role)).To(Succeed())
				}
			}
			for i := range 16 {
				otherNamespace := &epco.DatabaseRole{Namespace: otherNS.Name,
					Name: fmt.Sprintf("other-namespace-%d", i),
					Spec: epco.DatabaseRoleSpec{Cluster: epco.ClusterReference{Name: cluster.Name},
						Name: databaseTestOwnerName}}
				Expect(otherScope.Create(ctx, otherNamespace)).To(Succeed())
				otherName := &epco.DatabaseRole{Namespace: namespace.Name,
					Name: fmt.Sprintf("other-sql-name-%d", i),
					Spec: epco.DatabaseRoleSpec{Cluster: epco.ClusterReference{
						Namespace: namespace.Name, Name: cluster.Name}}}
				Expect(scope.Create(ctx, otherName)).To(Succeed())
			}
			owner := &epco.DatabaseRole{Namespace: namespace.Name, Name: databaseTestOwnerName,
				Spec: epco.DatabaseRoleSpec{Cluster: epco.ClusterReference{Name: cluster.Name}}}
			if explicitName {
				owner.Name = "different-kubernetes-name"
				owner.Spec.Name = databaseTestOwnerName
			}
			ownerScope := scope
			if crossNamespace {
				owner.Namespace = otherNS.Name
				owner.Spec.Cluster.Namespace = namespace.Name
				ownerScope = otherScope
			}
			Expect(ownerScope.Create(ctx, owner)).To(Succeed())
			factory := &databaseWatchFactory{owner: "previous"}
			recorded := &databaseOwnerCandidateClient{candidates: make(map[client.ObjectKey]bool)}
			cached := startDatabaseManagerWithClient(ctx, factory, func(c client.Client) client.Client {
				recorded.Client = c
				return recorded
			})
			// Establish the populated real cache before creating the Database under test.
			roles := &epco.DatabaseRoleList{}
			Expect(cached.List(ctx, roles)).To(Succeed())
			Expect(roles.Items).To(HaveLen(49))
			db := watchedDatabase(cluster, "indexed-owner")
			Expect(scope.Create(ctx, db)).To(Succeed())
			latest := expectDatabaseStatus(ctx, db, metav1.ConditionFalse, "DependenciesNotReady")
			generation := latest.Generation
			_, _, applies, _ := factory.snapshot()
			Expect(applies).To(BeZero())
			Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(owner), owner)).To(Succeed())
			owner.Status.HasManagedSQL = true
			owner.Status.Conditions = []metav1.Condition{{
				Type: databaseTestReadyCondition, Status: metav1.ConditionTrue, Reason: databaseTestReadyCondition,
				ObservedGeneration: owner.Generation, LastTransitionTime: metav1.Now(),
			}}
			Expect(environment.Client.Status().Update(ctx, owner)).To(Succeed())
			latest = expectDatabaseStatus(ctx, db, metav1.ConditionTrue, databaseTestReadyCondition)
			Expect(latest.Generation).To(Equal(generation))
			Expect(latest.Status.HasManagedSQL).To(BeTrue())
			lists, maximum, candidates := recorded.snapshot()
			Expect(lists).To(BeNumerically(">", 0))
			Expect(maximum).To(BeNumerically("<=", 1), "only the relevant SQL owner may reach dependency evaluation")
			Expect(candidates).To(ConsistOf(client.ObjectKeyFromObject(owner)))
		},
		Entry("metadata fallback with a namespace-local descriptor", false, false, NodeTimeout(60*time.Second)),
		Entry("explicit SQL name with a namespace-local descriptor", true, false, NodeTimeout(60*time.Second)),
		Entry("metadata fallback with a cross-namespace descriptor", false, true, NodeTimeout(60*time.Second)),
		Entry("explicit SQL name with a cross-namespace descriptor", true, true, NodeTimeout(60*time.Second)),
	)

	It("requeues admin and CA inputs, endpoints and recreated descriptors without Database generation changes",
		func(ctx SpecContext) {
			factory := &databaseWatchFactory{}
			startDatabaseManager(ctx, factory)
			cluster := createRoleDescriptor(ctx, nil)
			db := watchedDatabase(cluster, "watched-db")
			Expect(scope.Create(ctx, db)).To(Succeed())
			latest := expectDatabaseStatus(ctx, db, metav1.ConditionTrue, databaseTestReadyCondition)
			generation := latest.Generation
			Expect(latest.Status.HasManagedSQL).To(BeTrue())
			admin := &corev1.Secret{}
			adminKey := client.ObjectKey{Namespace: namespace.Name, Name: "role-admin"}
			Expect(environment.Client.Get(ctx, adminKey, admin)).To(Succeed())
			admin.Data[passwordKey] = []byte("admin-rotated")
			Expect(environment.Client.Update(ctx, admin)).To(Succeed())
			Eventually(func() string {
				_, password, _, _ := factory.snapshot()
				return password
			}).WithContext(ctx).WithTimeout(10 * time.Second).Should(Equal("admin-rotated"))
			fixture, err := integration.NewTLSFixture(GinkgoT().TempDir())
			Expect(err).NotTo(HaveOccurred())
			ca, err := os.ReadFile(fixture.CA)
			Expect(err).NotTo(HaveOccurred())
			caSecret := &corev1.Secret{Namespace: namespace.Name, Name: "database-ca",
				Data: map[string][]byte{databaseTestCAKey: ca}}
			Expect(scope.Create(ctx, caSecret)).To(Succeed())
			cluster.Spec.TLS = &epco.PostgresTLS{Mode: "verify-full",
				CASecret: &epco.SecretKeyReference{Name: caSecret.Name, Key: databaseTestCAKey}}
			Expect(environment.Client.Update(ctx, cluster)).To(Succeed())
			expectDatabaseStatus(ctx, db, metav1.ConditionTrue, databaseTestReadyCondition)
			caSecret.Data[databaseTestCAKey] = []byte("invalid")
			Expect(environment.Client.Update(ctx, caSecret)).To(Succeed())
			expectDatabaseStatus(ctx, db, metav1.ConditionFalse, "InvalidSecret")
			caSecret.Data[databaseTestCAKey] = ca
			Expect(environment.Client.Update(ctx, caSecret)).To(Succeed())
			expectDatabaseStatus(ctx, db, metav1.ConditionTrue, databaseTestReadyCondition)
			cluster.Spec.Connection.Host = "second.example"
			Expect(environment.Client.Update(ctx, cluster)).To(Succeed())
			Eventually(func() string {
				host, _, _, _ := factory.snapshot()
				return host
			}).WithContext(ctx).WithTimeout(10 * time.Second).Should(Equal("second.example"))
			originalUID := cluster.UID
			Expect(environment.Client.Delete(ctx, cluster)).To(Succeed())
			Eventually(func() bool {
				err := environment.Client.Get(ctx, client.ObjectKeyFromObject(cluster), &epco.PostgresCluster{})
				return apierrors.IsNotFound(err)
			}).WithContext(ctx).WithTimeout(10 * time.Second).Should(BeTrue())
			replacement := cluster.DeepCopy()
			replacement.ResourceVersion = ""
			replacement.UID = ""
			replacement.Spec.Connection.Host = "replacement.example"
			Expect(scope.Create(ctx, replacement)).To(Succeed())
			Expect(replacement.UID).NotTo(Equal(originalUID))
			Eventually(func() string {
				host, _, _, _ := factory.snapshot()
				return host
			}).WithContext(ctx).WithTimeout(10 * time.Second).Should(Equal("replacement.example"))
			latest = expectDatabaseStatus(ctx, db, metav1.ConditionTrue, databaseTestReadyCondition)
			Expect(latest.Generation).To(Equal(generation))
			Expect(latest.Status.HasManagedSQL).To(BeTrue())
		}, NodeTimeout(90*time.Second))

	It("pauses and resumes reclaim finalizers on Namespace labels and descriptor access changes", func(ctx SpecContext) {
		factory := &databaseWatchFactory{}
		startDatabaseManager(ctx, factory)
		labels := namespace.DeepCopy()
		labels.Labels = map[string]string{roleTestAccessLabel: roleTestAllowed}
		Expect(environment.Client.Update(ctx, labels)).To(Succeed())
		cluster := createRoleDescriptor(ctx, &epco.ClusterAccess{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{roleTestAccessLabel: roleTestAllowed}},
		})
		db := watchedDatabase(cluster, "finalized-db")
		db.Spec.ReclaimPolicy = epco.ReclaimDelete
		Expect(scope.Create(ctx, db)).To(Succeed())
		latest := expectDatabaseStatus(ctx, db, metav1.ConditionTrue, databaseTestReadyCondition)
		generation := latest.Generation
		labels.Labels = nil
		Expect(environment.Client.Update(ctx, labels)).To(Succeed())
		latest = expectDatabaseStatus(ctx, db, metav1.ConditionFalse, "AccessDenied")
		Expect(latest.Generation).To(Equal(generation))
		Expect(latest.Status.HasManagedSQL).To(BeTrue())
		Expect(environment.Client.Delete(ctx, db)).To(Succeed())
		latest = expectDatabaseStatus(ctx, db, metav1.ConditionFalse, "AccessDenied")
		Expect(latest.Finalizers).To(ContainElement(controller.DatabaseFinalizer))
		_, _, _, drops := factory.snapshot()
		Expect(drops).To(BeZero())
		cluster.Spec.Access = &epco.ClusterAccess{NamespaceSelector: &metav1.LabelSelector{}}
		Expect(environment.Client.Update(ctx, cluster)).To(Succeed())
		expectDatabaseGone(ctx, db)
		_, _, _, drops = factory.snapshot()
		Expect(drops).To(BeNumerically(">=", 1))
	}, NodeTimeout(60*time.Second))

	It("retains a former observer's reservation and requeues a cross-namespace contender when the manager retires",
		func(ctx SpecContext) {
			factory := &databaseWatchFactory{}
			startDatabaseManager(ctx, factory)
			cluster := createRoleDescriptor(ctx, &epco.ClusterAccess{NamespaceSelector: &metav1.LabelSelector{}})
			otherScope, otherNS, err := environment.NewScope(ctx, "epco-db-competing-")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func(ctx SpecContext) { Expect(otherScope.Cleanup(ctx)).To(Succeed()) })
			owner := watchedDatabase(cluster, "recorded-owner")
			owner.Spec.Name = "shared-database"
			Expect(scope.Create(ctx, owner)).To(Succeed())
			expectDatabaseStatus(ctx, owner, metav1.ConditionTrue, databaseTestReadyCondition)
			other := &epco.Database{Namespace: otherNS.Name, Name: "contender", Spec: epco.DatabaseSpec{
				Cluster: epco.ClusterReference{Namespace: namespace.Name, Name: cluster.Name},
				Name:    owner.Spec.Name, Owner: databaseTestOwnerName,
			}}
			Expect(otherScope.Create(ctx, other)).To(Succeed())
			latest := expectDatabaseStatus(ctx, other, metav1.ConditionFalse, "OwnershipConflict")
			generation := latest.Generation
			owner = expectDatabaseStatus(ctx, owner, metav1.ConditionTrue, databaseTestReadyCondition)
			owner.Spec.ManagementPolicy = epco.ManagementObserve
			Expect(environment.Client.Update(ctx, owner)).To(Succeed())
			owner = expectDatabaseStatus(ctx, owner, metav1.ConditionTrue, "Observed")
			Expect(owner.Status.HasManagedSQL).To(BeTrue())
			expectDatabaseStatus(ctx, other, metav1.ConditionFalse, "OwnershipConflict")
			Expect(environment.Client.Delete(ctx, owner)).To(Succeed())
			expectDatabaseGone(ctx, owner)
			latest = expectDatabaseStatus(ctx, other, metav1.ConditionTrue, databaseTestReadyCondition)
			Expect(latest.Generation).To(Equal(generation))
		}, NodeTimeout(60*time.Second))

	It("requeues on managed SQL owner readiness matched by SQL name and canonical descriptor", func(ctx SpecContext) {
		factory := &databaseWatchFactory{owner: "previous"}
		startDatabaseManager(ctx, factory)
		cluster := createRoleDescriptor(ctx, nil)
		owner := &epco.DatabaseRole{Namespace: namespace.Name, Name: "different-kubernetes-name",
			Spec: epco.DatabaseRoleSpec{Cluster: epco.ClusterReference{Name: cluster.Name}, Name: databaseTestOwnerName}}
		Expect(scope.Create(ctx, owner)).To(Succeed())
		db := watchedDatabase(cluster, "waiting-owner")
		Expect(scope.Create(ctx, db)).To(Succeed())
		latest := expectDatabaseStatus(ctx, db, metav1.ConditionFalse, "DependenciesNotReady")
		generation := latest.Generation
		_, _, applies, _ := factory.snapshot()
		Expect(applies).To(BeZero())
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(owner), owner)).To(Succeed())
		owner.Status.HasManagedSQL = true
		owner.Status.Conditions = []metav1.Condition{{
			Type: databaseTestReadyCondition, Status: metav1.ConditionTrue, Reason: databaseTestReadyCondition,
			ObservedGeneration: owner.Generation, LastTransitionTime: metav1.Now(),
		}}
		Expect(environment.Client.Status().Update(ctx, owner)).To(Succeed())
		latest = expectDatabaseStatus(ctx, db, metav1.ConditionTrue, databaseTestReadyCondition)
		Expect(latest.Generation).To(Equal(generation))
		Expect(latest.Status.HasManagedSQL).To(BeTrue())
	}, NodeTimeout(60*time.Second))

	It("persists sticky management through observation and explicit absence using the real status schema",
		func(ctx SpecContext) {
			factory := &databaseWatchFactory{}
			startDatabaseManager(ctx, factory)
			cluster := createRoleDescriptor(ctx, nil)
			db := watchedDatabase(cluster, "sticky-state")
			Expect(scope.Create(ctx, db)).To(Succeed())
			db = expectDatabaseStatus(ctx, db, metav1.ConditionTrue, databaseTestReadyCondition)
			Expect(db.Status.HasManagedSQL).To(BeTrue())
			applied := db.Status.AppliedGeneration
			successfulTime := db.Status.LastSuccessfulReconcileTime
			db.Spec.ManagementPolicy = epco.ManagementObserve
			Expect(environment.Client.Update(ctx, db)).To(Succeed())
			db = expectDatabaseStatus(ctx, db, metav1.ConditionTrue, "Observed")
			Expect(db.Status.HasManagedSQL).To(BeTrue())
			Expect(db.Status.Applied).To(BeNil())
			Expect(db.Status.AppliedGeneration).To(Equal(applied))
			Expect(db.Status.LastSuccessfulReconcileTime).To(Equal(successfulTime))
			db.Spec.ManagementPolicy = epco.ManagementManage
			db.Spec.Ensure = epco.EnsureAbsent
			Expect(environment.Client.Update(ctx, db)).To(Succeed())
			db = expectDatabaseStatus(ctx, db, metav1.ConditionTrue, databaseTestReadyCondition)
			Expect(db.Status.HasManagedSQL).To(BeTrue())
			Expect(db.Status.AppliedGeneration).To(Equal(db.Generation))
			_, _, _, drops := factory.snapshot()
			Expect(drops).To(BeNumerically(">=", 1))
		}, NodeTimeout(60*time.Second))
})

var _ = Describe("Database real API status recovery", func() {
	It("retries SQL success after an optimistic status conflict without an initialization journal", func(ctx SpecContext) {
		factory := &databaseWatchFactory{exists: map[string]bool{"status-retry": false}}
		cluster := createRoleDescriptor(ctx, nil)
		db := watchedDatabase(cluster, "status-retry")
		Expect(scope.Create(ctx, db)).To(Succeed())
		builder, err := postgres.NewConfigBuilder()
		Expect(err).NotTo(HaveOccurred())
		var conflictOnce sync.Once
		var injected bool
		direct, err := client.NewWithWatch(environment.Config, client.Options{Scheme: environment.Scheme})
		Expect(err).NotTo(HaveOccurred())
		decorated := interceptor.NewClient(direct, interceptor.Funcs{SubResourceUpdate: func(
			writeCtx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			_, _, applies, _ := factory.snapshot()
			if sub == connectionAPIStatusSubresource && applies > 0 {
				conflictOnce.Do(func() {
					latest := &epco.Database{}
					Expect(c.Get(writeCtx, client.ObjectKeyFromObject(db), latest)).To(Succeed())
					latest.Status.Message = "concurrent status writer"
					Expect(c.Status().Update(writeCtx, latest)).To(Succeed())
					injected = true
				})
			}
			return c.SubResource(sub).Update(writeCtx, obj, opts...)
		}})
		r := &controller.DatabaseReconciler{Client: decorated, APIReader: environment.Client, Builder: builder,
			Databases: factory, Locks: &controller.ManagementLocks{}}
		request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(db)}
		result, err := r.Reconcile(ctx, request)
		Expect(err).NotTo(HaveOccurred())
		Expect(injected).To(BeTrue())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		latest := &epco.Database{}
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(db), latest)).To(Succeed())
		Expect(latest.Status.HasManagedSQL).To(BeFalse())
		_, _, applies, _ := factory.snapshot()
		Expect(applies).To(Equal(1))
		_, err = r.Reconcile(ctx, request)
		Expect(err).NotTo(HaveOccurred())
		latest = expectDatabaseStatus(ctx, db, metav1.ConditionTrue, databaseTestReadyCondition)
		Expect(latest.Status.HasManagedSQL).To(BeTrue())
		Expect(latest.Status.AppliedGeneration).To(Equal(latest.Generation))
	}, NodeTimeout(30*time.Second))
})
