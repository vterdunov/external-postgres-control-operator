//go:build envtest

package envtest_test

import (
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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

type watchedRoleFactory struct {
	mu         sync.Mutex
	applyHosts []string
	drops      int
}

const (
	roleTestAccessLabel   = "epco-test-access"
	roleTestGeneratedName = "generated"
	roleTestAdmin         = "admin"
	roleTestTLSMode       = "disable"
	roleTestApplication   = "application"
	roleTestAllowed       = "allowed"
)

func (f *watchedRoleFactory) Open(_ context.Context, config *postgres.Config) (postgres.RoleSession, error) {
	return &watchedRoleSession{factory: f, host: config.Connection.Host}, nil
}
func (f *watchedRoleFactory) appliedHost() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.applyHosts) == 0 {
		return ""
	}
	return f.applyHosts[len(f.applyHosts)-1]
}
func (f *watchedRoleFactory) applyCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.applyHosts)
}
func (f *watchedRoleFactory) dropCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.drops }

type watchedRoleSession struct {
	factory *watchedRoleFactory
	host    string
}

func (*watchedRoleSession) Inspect(_ context.Context, name string) (*postgres.RoleState, error) {
	return &postgres.RoleState{
		Exists:              true,
		Name:                name,
		AdminUser:           roleTestAdmin,
		ServerVersion:       180000,
		MaxIdentifierLength: 63,
	}, nil
}
func (s *watchedRoleSession) Apply(context.Context, postgres.RoleDesired) (*postgres.RoleApplyResult, error) {
	s.factory.mu.Lock()
	s.factory.applyHosts = append(s.factory.applyHosts, s.host)
	s.factory.mu.Unlock()
	return &postgres.RoleApplyResult{}, nil
}
func (s *watchedRoleSession) Drop(context.Context, string) error {
	s.factory.mu.Lock()
	s.factory.drops++
	s.factory.mu.Unlock()
	return nil
}
func (*watchedRoleSession) Close(context.Context) error { return nil }

func startRoleManager(ctx context.Context, factory *watchedRoleFactory) client.Reader {
	builder, err := postgres.NewConfigBuilder()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	instance, err := environment.StartManager(ctx, harness.ManagerOptions{
		CachedObjects: []client.Object{&epco.DatabaseRole{}, &epco.ExternalPostgres{}, &corev1.Secret{}, &corev1.Namespace{}},
		Register: func(mgr manager.Manager) error {
			return (&controller.DatabaseRoleReconciler{
				Client:    mgr.GetClient(),
				APIReader: mgr.GetAPIReader(),
				Builder:   builder,
				Roles:     factory,
				Locks:     &controller.ManagementLocks{},
			}).SetupWithManager(mgr)
		},
	})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		Expect(instance.Stop(stopCtx)).To(Succeed())
	})
	return instance.Manager.GetCache()
}
func createRoleDescriptor(ctx context.Context, access *epco.ClusterAccess) *epco.ExternalPostgres {
	admin := &corev1.Secret{
		Namespace: namespace.Name,
		Name:      "role-admin",
		Type:      corev1.SecretTypeBasicAuth,
		Data: map[string][]byte{
			corev1.BasicAuthUsernameKey: []byte(roleTestAdmin),
			passwordKey:                 []byte("manager-test-input"),
		},
	}
	ExpectWithOffset(1, scope.Create(ctx, admin)).To(Succeed())
	cluster := &epco.ExternalPostgres{
		Namespace: namespace.Name,
		Name:      "role-connection",
		Spec: epco.ExternalPostgresSpec{
			Connection:        epco.PostgresConnection{Host: "first.example"},
			CredentialsSecret: epco.LocalReference{Name: admin.Name},
			TLS:               &epco.PostgresTLS{Mode: roleTestTLSMode},
			Access:            access,
			ReconcileInterval: "1h",
		},
	}
	ExpectWithOffset(1, scope.Create(ctx, cluster)).To(Succeed())
	return cluster
}
func expectRoleStatus(ctx context.Context, role *epco.DatabaseRole,
	status metav1.ConditionStatus, reason string) *epco.DatabaseRole {
	var latest *epco.DatabaseRole
	Eventually(func(g Gomega) {
		latest = &epco.DatabaseRole{}
		g.Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(role), latest)).To(Succeed())
		condition := meta.FindStatusCondition(latest.Status.Conditions, "Ready")
		g.Expect(condition).NotTo(BeNil())
		g.Expect(condition.Status).To(Equal(status))
		g.Expect(condition.Reason).To(Equal(reason))
		g.Expect(condition.ObservedGeneration).To(Equal(latest.Generation))
	}).WithContext(ctx).WithTimeout(10 * time.Second).Should(Succeed())
	return latest
}

var _ = Describe("DatabaseRole manager watches", func() {
	It("ignores descriptor status heartbeats and preserves lifecycle and periodic work", func(ctx SpecContext) {
		cluster := createRoleDescriptor(ctx, nil)
		cluster.Finalizers = []string{"postgresql.epco.io/watch-test-hold"}
		Expect(environment.Client.Update(ctx, cluster)).To(Succeed())
		replacementSpec := *cluster.Spec.DeepCopy()
		factory := &watchedRoleFactory{}
		cache := startRoleManager(ctx, factory)
		role := &epco.DatabaseRole{Namespace: namespace.Name, Name: roleTestApplication,
			Finalizers: []string{controller.RoleFinalizer},
			Spec:       epco.DatabaseRoleSpec{Cluster: epco.ClusterReference{Name: cluster.Name}},
		}
		Expect(scope.Create(ctx, role)).To(Succeed())
		expectRoleStatus(ctx, role, metav1.ConditionTrue, "Ready")
		beforeHeartbeat := factory.applyCount()
		generation := cluster.Generation
		cluster.Status.LastSuccessfulReconcileTime = new(metav1.Now())
		Expect(environment.Client.Status().Update(ctx, cluster)).To(Succeed())
		Expect(cluster.Generation).To(Equal(generation))
		Eventually(func(g Gomega) {
			cached := &epco.ExternalPostgres{}
			g.Expect(cache.Get(ctx, client.ObjectKeyFromObject(cluster), cached)).To(Succeed())
			g.Expect(cached.ResourceVersion).To(Equal(cluster.ResourceVersion))
		}).WithContext(ctx).WithTimeout(10*time.Second).Should(Succeed(), "the descriptor watch received the heartbeat")
		Consistently(factory.applyCount).WithContext(ctx).WithTimeout(time.Second).
			Should(Equal(beforeHeartbeat), "a descriptor status heartbeat must not start a SQL role reconciliation")

		Expect(environment.Client.Delete(ctx, cluster)).To(Succeed())
		latestRole := expectRoleStatus(ctx, role, metav1.ConditionFalse, "DependenciesNotReady")
		Expect(latestRole.Status.Message).To(Equal("The ExternalPostgres is being deleted"))
		latestCluster := &epco.ExternalPostgres{}
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(cluster), latestCluster)).To(Succeed())
		latestCluster.Finalizers = nil
		Expect(environment.Client.Update(ctx, latestCluster)).To(Succeed())

		replacement := &epco.ExternalPostgres{Namespace: cluster.Namespace, Name: cluster.Name, Spec: replacementSpec}
		replacement.Spec.Connection.Host = "periodic.example"
		replacement.Spec.ReconcileInterval = "1s"
		Eventually(func() error { return scope.Create(ctx, replacement) }).WithContext(ctx).
			WithTimeout(10 * time.Second).Should(Succeed())
		Eventually(factory.appliedHost).WithContext(ctx).WithTimeout(10 * time.Second).Should(Equal("periodic.example"))
		expectRoleStatus(ctx, role, metav1.ConditionTrue, "Ready")
		afterReplacement := factory.applyCount()
		Eventually(factory.applyCount).WithContext(ctx).WithTimeout(10*time.Second).
			Should(BeNumerically(">", afterReplacement), "the role keeps reconciling on its own timer")
	}, NodeTimeout(60*time.Second))

	It("watches sources and descriptor changes without role-generation changes", func(ctx SpecContext) {
		factory := &watchedRoleFactory{}
		startRoleManager(ctx, factory)
		cluster := createRoleDescriptor(ctx, nil)
		role := &epco.DatabaseRole{
			Namespace: namespace.Name,
			Name:      roleTestApplication,
			Spec: epco.DatabaseRoleSpec{
				Cluster:        epco.ClusterReference{Name: cluster.Name},
				PasswordSecret: &epco.LocalReference{Name: "source"},
			},
		}
		Expect(scope.Create(ctx, role)).To(Succeed())
		expectRoleStatus(ctx, role, metav1.ConditionFalse, "SecretNotFound")
		source := &corev1.Secret{
			Namespace: namespace.Name,
			Name:      "source",
			Type:      corev1.SecretTypeBasicAuth,
			Data: map[string][]byte{
				corev1.BasicAuthUsernameKey: []byte(roleTestApplication),
				passwordKey:                 []byte("source-a"),
			},
		}
		Expect(scope.Create(ctx, source)).To(Succeed())
		latest := expectRoleStatus(ctx, role, metav1.ConditionTrue, "Ready")
		Expect(latest.Status.SecretResourceVersion).To(Equal(source.ResourceVersion))
		source.Data[passwordKey] = []byte("source-b")
		Expect(environment.Client.Update(ctx, source)).To(Succeed())
		Eventually(func(g Gomega) {
			latest = &epco.DatabaseRole{}
			g.Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(role), latest)).To(Succeed())
			g.Expect(latest.Status.SecretResourceVersion).To(Equal(source.ResourceVersion))
		}).WithContext(ctx).WithTimeout(10 * time.Second).Should(Succeed())
		generation := latest.Generation
		acceptedVersion := latest.Status.SecretResourceVersion
		cluster.Spec.Connection.Host = "second.example"
		Expect(environment.Client.Update(ctx, cluster)).To(Succeed())
		Eventually(factory.appliedHost).WithContext(ctx).WithTimeout(10 * time.Second).Should(Equal("second.example"))
		latest = expectRoleStatus(ctx, role, metav1.ConditionTrue, "Ready")
		Expect(latest.Generation).To(Equal(generation))
		Expect(latest.Status.SecretResourceVersion).To(Equal(acceptedVersion))
		Expect(environment.Client.Delete(ctx, cluster)).To(Succeed())
		replacement := cluster.DeepCopy()
		replacement.ResourceVersion = ""
		replacement.UID = ""
		replacement.Spec.Connection.Host = "replacement.example"
		Expect(scope.Create(ctx, replacement)).To(Succeed())
		Eventually(factory.appliedHost).WithContext(ctx).WithTimeout(10 * time.Second).Should(Equal("replacement.example"))
	}, NodeTimeout(60*time.Second))

	It("pauses and resumes finalizers with namespace access", func(ctx SpecContext) {
		factory := &watchedRoleFactory{}
		startRoleManager(ctx, factory)
		labels := namespace.DeepCopy()
		labels.Labels = map[string]string{roleTestAccessLabel: roleTestAllowed}
		Expect(environment.Client.Update(ctx, labels)).To(Succeed())
		cluster := createRoleDescriptor(ctx, &epco.ClusterAccess{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{roleTestAccessLabel: roleTestAllowed}},
		})
		role := &epco.DatabaseRole{
			Namespace: namespace.Name,
			Name:      "pending-delete",
			Spec: epco.DatabaseRoleSpec{
				Cluster:            epco.ClusterReference{Name: cluster.Name},
				PasswordGeneration: &epco.PasswordGeneration{SecretName: "pending-source"},
				ReclaimPolicy:      epco.ReclaimDelete,
			},
		}
		Expect(scope.Create(ctx, role)).To(Succeed())
		expectRoleStatus(ctx, role, metav1.ConditionTrue, "Ready")
		labels.Labels = nil
		Expect(environment.Client.Update(ctx, labels)).To(Succeed())
		expectRoleStatus(ctx, role, metav1.ConditionFalse, "AccessDenied")
		Expect(environment.Client.Delete(ctx, role)).To(Succeed())
		latest := expectRoleStatus(ctx, role, metav1.ConditionFalse, "AccessDenied")
		Expect(latest.DeletionTimestamp).NotTo(BeNil())
		Expect(latest.Finalizers).To(ContainElement(controller.RoleFinalizer))
		Expect(factory.dropCount()).To(BeZero())
		source := &corev1.Secret{}
		Expect(environment.Client.Get(ctx, client.ObjectKey{
			Namespace: namespace.Name,
			Name:      "pending-source",
		}, source)).To(Succeed())
		labels.Labels = map[string]string{roleTestAccessLabel: roleTestAllowed}
		Expect(environment.Client.Update(ctx, labels)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(environment.Client.Get(ctx, client.ObjectKeyFromObject(role), &epco.DatabaseRole{}))
		}).WithContext(ctx).WithTimeout(10 * time.Second).Should(BeTrue())
		Expect(factory.dropCount()).To(BeNumerically(">=", 1))
		Expect(apierrors.IsNotFound(environment.Client.Get(
			ctx, client.ObjectKeyFromObject(source), &corev1.Secret{}))).To(BeTrue())
	}, NodeTimeout(60*time.Second))

	It("unblocks a competing manager after the recorded manager retires", func(ctx SpecContext) {
		factory := &watchedRoleFactory{}
		startRoleManager(ctx, factory)
		cluster := createRoleDescriptor(ctx, &epco.ClusterAccess{NamespaceSelector: &metav1.LabelSelector{}})
		secondScope, secondNamespace, err := environment.NewScope(ctx, "epco-competing-")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx SpecContext) { Expect(secondScope.Cleanup(ctx)).To(Succeed()) })
		owner := &epco.DatabaseRole{
			Namespace: namespace.Name,
			Name:      "same-name",
			Spec:      epco.DatabaseRoleSpec{Cluster: epco.ClusterReference{Name: cluster.Name}, Name: "shared"},
		}
		Expect(scope.Create(ctx, owner)).To(Succeed())
		expectRoleStatus(ctx, owner, metav1.ConditionTrue, "Ready")
		competitor := &epco.DatabaseRole{
			Namespace: secondNamespace.Name,
			Name:      owner.Name,
			Spec: epco.DatabaseRoleSpec{
				Cluster: epco.ClusterReference{Namespace: namespace.Name, Name: cluster.Name},
				Name:    "shared",
			},
		}
		Expect(secondScope.Create(ctx, competitor)).To(Succeed())
		expectRoleStatus(ctx, competitor, metav1.ConditionFalse, "OwnershipConflict")
		owner = expectRoleStatus(ctx, owner, metav1.ConditionTrue, "Ready")
		previous := owner.DeepCopy()
		owner.Spec.ManagementPolicy = epco.ManagementObserve
		Expect(environment.Client.Patch(ctx, owner, client.MergeFrom(previous))).To(Succeed())
		owner = expectRoleStatus(ctx, owner, metav1.ConditionTrue, "Observed")
		Expect(owner.Status.HasManagedSQL).To(BeTrue())
		expectRoleStatus(ctx, competitor, metav1.ConditionFalse, "OwnershipConflict")
		Expect(environment.Client.Delete(ctx, owner)).To(Succeed())
		expectRoleStatus(ctx, competitor, metav1.ConditionTrue, "Ready")
	}, NodeTimeout(60*time.Second))
})
