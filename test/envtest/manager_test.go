//go:build envtest

package envtest_test

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	harness "github.com/vterdunov/external-postgres-control-operator/test/envtest"
)

type observation struct {
	key             client.ObjectKey
	resourceVersion string
}

type secretObserver struct {
	client client.Client
	seen   chan observation
}

func (r *secretObserver) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	secret := &corev1.Secret{}
	if err := r.client.Get(ctx, request.NamespacedName, secret); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	select {
	case r.seen <- observation{key: request.NamespacedName, resourceVersion: secret.ResourceVersion}:
	case <-ctx.Done():
		return reconcile.Result{}, ctx.Err()
	}
	return reconcile.Result{}, nil
}

var _ = Describe("explicit manual reconcile", func() {
	It("runs a reconciler directly against the API without a manager or cache", func(ctx SpecContext) {
		secret := &corev1.Secret{Name: "manual", Namespace: namespace.Name}
		Expect(scope.Create(ctx, secret)).To(Succeed())
		reconciler := &secretObserver{client: environment.Client, seen: make(chan observation, 1)}
		request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(secret)}
		result, err := reconciler.Reconcile(ctx, request)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(reconcile.Result{}))
		expected := observation{key: request.NamespacedName, resourceVersion: secret.ResourceVersion}
		Expect(reconciler.seen).To(Receive(Equal(expected)))
	})
})

var _ = Describe("manager, cache, watches and queue", func() {
	It("returns registration errors before starting any manager goroutine", func(ctx SpecContext) {
		registrationErr := errors.New("intentional test registration failure")
		instance, err := environment.StartManager(ctx, harness.ManagerOptions{
			Register: func(manager.Manager) error { return registrationErr },
		})
		Expect(instance).To(BeNil())
		Expect(err).To(MatchError(registrationErr))
		// The API remains usable after the rejected manager startup.
		Expect(scope.Create(ctx, &corev1.Secret{Name: "after-rejection", Namespace: namespace.Name})).To(Succeed())
	})
	It("observes Secret creation and rotation through a real watch, then joins the manager", func(ctx SpecContext) {
		seen := make(chan observation, 16)
		instance, err := environment.StartManager(ctx, harness.ManagerOptions{
			Namespace: namespace.Name, CachedObjects: []client.Object{&corev1.Secret{}},
			Register: func(mgr manager.Manager) error {
				reconciler := &secretObserver{client: mgr.GetClient(), seen: seen}
				return ctrl.NewControllerManagedBy(mgr).Named("test-secret-watch").
					For(&corev1.Secret{}).Complete(reconciler)
			},
		})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			Expect(instance.Stop(stopCtx)).To(Succeed())
		})
		secret := &corev1.Secret{
			Name: "watched", Namespace: namespace.Name, Data: map[string][]byte{passwordKey: []byte("initial")},
		}
		Expect(scope.Create(ctx, secret)).To(Succeed())
		key := client.ObjectKeyFromObject(secret)
		// Observing the create proves the informer and queue are operating before rotation.
		expectedCreate := observation{key: key, resourceVersion: secret.ResourceVersion}
		Eventually(seen).WithContext(ctx).WithTimeout(10 * time.Second).Should(Receive(Equal(expectedCreate)))
		initialVersion := secret.ResourceVersion
		secret.Data[passwordKey] = []byte("rotated")
		Expect(environment.Client.Update(ctx, secret)).To(Succeed())
		Expect(secret.ResourceVersion).NotTo(Equal(initialVersion))
		expectedRotation := observation{key: key, resourceVersion: secret.ResourceVersion}
		Eventually(func() observation {
			select {
			case observed := <-seen:
				return observed
			default:
				return observation{}
			}
		}).WithContext(ctx).WithTimeout(10 * time.Second).Should(Equal(expectedRotation))
		Expect(instance.Stop(ctx)).To(Succeed())
	}, NodeTimeout(45*time.Second))
})
