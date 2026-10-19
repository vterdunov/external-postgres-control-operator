//go:build envtest

package envtest_test

import (
	"context"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/controller"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const recoveryDescriptorName = "connection"

// This client models a cache snapshot only. Reads of other resources and all
// writes retain real API server semantics, including resourceVersion conflicts.
type staleRoleSnapshotClient struct {
	client.Client
	snapshot      *epco.DatabaseRole
	conflicts     atomic.Int32
	secretCreates atomic.Int32
}

func (c *staleRoleSnapshotClient) Get(ctx context.Context, key client.ObjectKey,
	obj client.Object, opts ...client.GetOption) error {
	if role, ok := obj.(*epco.DatabaseRole); ok && key == client.ObjectKeyFromObject(c.snapshot) {
		*role = *c.snapshot.DeepCopy()
		return nil
	}
	return c.Client.Get(ctx, key, obj, opts...)
}
func (c *staleRoleSnapshotClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*corev1.Secret); ok {
		c.secretCreates.Add(1)
	}
	return c.Client.Create(ctx, obj, opts...)
}
func (c *staleRoleSnapshotClient) Status() client.SubResourceWriter {
	return &staleRoleStatusWriter{SubResourceWriter: c.Client.Status(), parent: c}
}

type staleRoleStatusWriter struct {
	client.SubResourceWriter
	parent *staleRoleSnapshotClient
}

func (w *staleRoleStatusWriter) Update(ctx context.Context, obj client.Object,
	opts ...client.SubResourceUpdateOption) error {
	err := w.SubResourceWriter.Update(ctx, obj, opts...)
	if apierrors.IsConflict(err) {
		w.parent.conflicts.Add(1)
	}
	return err
}
func (w *staleRoleStatusWriter) Patch(ctx context.Context, obj client.Object,
	patch client.Patch, opts ...client.SubResourcePatchOption) error {
	err := w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
	if apierrors.IsConflict(err) {
		w.parent.conflicts.Add(1)
	}
	return err
}

type recoveryRoleFactory struct{ mutations atomic.Int32 }

func (f *recoveryRoleFactory) Open(context.Context, *postgres.Config) (postgres.RoleSession, error) {
	return &recoveryRoleSession{factory: f}, nil
}

type recoveryRoleSession struct{ factory *recoveryRoleFactory }

func (*recoveryRoleSession) Inspect(context.Context, string) (*postgres.RoleState, error) {
	return &postgres.RoleState{
		Exists:              true,
		Name:                roleTestApplication,
		AdminUser:           roleTestAdmin,
		ServerVersion:       180000,
		MaxIdentifierLength: 63,
	}, nil
}
func (s *recoveryRoleSession) Apply(context.Context, postgres.RoleDesired) (*postgres.RoleApplyResult, error) {
	s.factory.mutations.Add(1)
	return &postgres.RoleApplyResult{}, nil
}
func (s *recoveryRoleSession) Drop(context.Context, string) error {
	s.factory.mutations.Add(1)
	return nil
}
func (*recoveryRoleSession) Close(context.Context) error { return nil }

var _ = Describe("DatabaseRole durable recovery", func() {
	It("rejects stale initialized=false before source recreation or SQL mutation", func(ctx SpecContext) {
		cluster := &epco.ExternalPostgres{
			Namespace: namespace.Name,
			Name:      recoveryDescriptorName,
			Spec: epco.ExternalPostgresSpec{
				Connection:        epco.PostgresConnection{Host: "database.example"},
				CredentialsSecret: epco.LocalReference{Name: roleTestAdmin},
				TLS:               &epco.PostgresTLS{Mode: roleTestTLSMode},
			},
		}
		Expect(scope.Create(ctx, cluster)).To(Succeed())
		admin := &corev1.Secret{
			Namespace: namespace.Name,
			Name:      roleTestAdmin,
			Type:      corev1.SecretTypeBasicAuth,
			Data: map[string][]byte{
				corev1.BasicAuthUsernameKey: []byte(roleTestAdmin),
				passwordKey:                 []byte("test-administrative-input"),
			},
		}
		Expect(scope.Create(ctx, admin)).To(Succeed())
		role := &epco.DatabaseRole{
			Namespace:  namespace.Name,
			Name:       roleTestApplication,
			Finalizers: []string{controller.RoleFinalizer},
			Spec: epco.DatabaseRoleSpec{
				Cluster:            epco.ClusterReference{Name: cluster.Name},
				PasswordGeneration: &epco.PasswordGeneration{SecretName: roleTestGeneratedName},
			},
		}
		Expect(scope.Create(ctx, role)).To(Succeed())
		stale := role.DeepCopy()
		Expect(stale.Status.GeneratedPasswordInitialized).To(BeFalse())
		source := &corev1.Secret{
			Namespace: namespace.Name,
			Name:      roleTestGeneratedName,
			Type:      corev1.SecretTypeBasicAuth,
			Data: map[string][]byte{
				corev1.BasicAuthUsernameKey: []byte(roleTestApplication),
				passwordKey:                 []byte("persisted-original-input"),
			},
		}
		Expect(scope.Create(ctx, source)).To(Succeed())
		role.Status.GeneratedPasswordInitialized = true
		Expect(environment.Client.Status().Update(ctx, role)).To(Succeed())
		Expect(role.ResourceVersion).NotTo(Equal(stale.ResourceVersion))
		Expect(environment.Client.Delete(ctx, source)).To(Succeed())
		cached := &staleRoleSnapshotClient{Client: environment.Client, snapshot: stale}
		builder, err := postgres.NewConfigBuilder()
		Expect(err).NotTo(HaveOccurred())
		factory := &recoveryRoleFactory{}
		r := &controller.DatabaseRoleReconciler{
			Client:    cached,
			APIReader: environment.Client,
			Builder:   builder,
			Roles:     factory,
			Locks:     &controller.ManagementLocks{},
		}
		result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(role)})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(cached.conflicts.Load()).To(BeNumerically(">", 0),
			"an empty pre-generation status patch must still carry an RV precondition")
		Expect(cached.secretCreates.Load()).To(BeZero(), "the stale status checkpoint must fail before source CREATE")
		Expect(factory.mutations.Load()).To(BeZero(), "the stale status checkpoint must fail before SQL mutation")
		latest := &epco.DatabaseRole{}
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(role), latest)).To(Succeed())
		Expect(latest.Status.GeneratedPasswordInitialized).To(BeTrue())
		Expect(apierrors.IsNotFound(environment.Client.Get(
			ctx, client.ObjectKeyFromObject(source), &corev1.Secret{}))).To(BeTrue())
	})
})
