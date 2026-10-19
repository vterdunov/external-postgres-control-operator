//go:build envtest

package envtest_test

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

type clusterProbe struct {
	mu    sync.Mutex
	calls int
	seen  chan string
	block chan struct{}
}

func (p *clusterProbe) Check(ctx context.Context, c *postgres.Config) error {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()
	select {
	case p.seen <- c.Connection.Password:
	case <-ctx.Done():
		return ctx.Err()
	}
	if n == 1 && p.block != nil {
		select {
		case <-p.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if c.Connection.Password == "broken" {
		return &postgres.Failure{Reason: "ConnectionUnavailable", Message: "authentication failed"}
	}
	return nil
}
func (p *clusterProbe) count() int { p.mu.Lock(); defer p.mu.Unlock(); return p.calls }
func startClusterManager(ctx context.Context, p *clusterProbe) {
	startClusterManagerWithClient(ctx, p, nil)
}
func startClusterManagerWithClient(ctx context.Context, p *clusterProbe, decorate func(client.Client) client.Client) {
	b, err := postgres.NewConfigBuilder()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	instance, err := environment.StartManager(ctx, harness.ManagerOptions{Namespace: namespace.Name,
		CachedObjects: []client.Object{&epco.ExternalPostgres{}, &corev1.Secret{}},
		Register: func(mgr manager.Manager) error {
			controllerClient := mgr.GetClient()
			if decorate != nil {
				controllerClient = decorate(controllerClient)
			}
			return (&controller.ExternalPostgresReconciler{Client: controllerClient,
				APIReader: mgr.GetAPIReader(),
				Builder:   b,
				Checker:   p}).SetupWithManager(mgr)
		},
	})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		Expect(instance.Stop(stopCtx)).To(Succeed())
	})
}
func clusterAuth(name, password string) *corev1.Secret {
	return &corev1.Secret{Name: name,
		Namespace: namespace.Name,
		Type:      corev1.SecretTypeBasicAuth,
		Data: map[string][]byte{connectionAPIUsernameKey: []byte("admin"),
			passwordKey: []byte(password)}}
}
func watchedCluster() *epco.ExternalPostgres {
	return &epco.ExternalPostgres{Name: "watched-cluster",
		Namespace: namespace.Name,
		Spec: epco.ExternalPostgresSpec{Connection: epco.PostgresConnection{Host: "database"},
			CredentialsSecret: epco.LocalReference{Name: "auth"},
			TLS:               &epco.PostgresTLS{Mode: "disable"},
			ReconcileInterval: "2m"}}
}
func clusterCondition(ctx context.Context, c *epco.ExternalPostgres) *metav1.Condition {
	latest := &epco.ExternalPostgres{}
	if environment.Client.Get(ctx, client.ObjectKeyFromObject(c), latest) != nil {
		return nil
	}
	return meta.FindStatusCondition(latest.Status.Conditions, "Ready")
}
func expectReady(ctx context.Context, c *epco.ExternalPostgres, status metav1.ConditionStatus, reason string) {
	Eventually(func(g Gomega) {
		condition := clusterCondition(ctx, c)
		g.Expect(condition).NotTo(BeNil())
		g.Expect(condition.Status).To(Equal(status))
		g.Expect(condition.Reason).To(Equal(reason))
	}).WithContext(ctx).WithTimeout(10 * time.Second).Should(Succeed())
}

type conflictStatusClient struct {
	client.Client
	once sync.Once
	seen *atomic.Bool
}

func (c *conflictStatusClient) Status() client.SubResourceWriter {
	return &conflictStatusWriter{SubResourceWriter: c.Client.Status(), parent: c}
}

type conflictStatusWriter struct {
	client.SubResourceWriter
	parent *conflictStatusClient
}

func (w *conflictStatusWriter) Patch(ctx context.Context,
	obj client.Object,
	patch client.Patch,
	opts ...client.SubResourcePatchOption) error {
	var mutationErr error
	w.parent.once.Do(func() {
		latest := &epco.ExternalPostgres{}
		mutationErr = environment.Client.Get(ctx, client.ObjectKeyFromObject(obj), latest)
		if mutationErr == nil {
			latest.Status.Message = "independent concurrent writer"
			mutationErr = environment.Client.Status().Update(ctx, latest)
		}
	})
	if mutationErr != nil {
		return mutationErr
	}
	err := w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
	if apierrors.IsConflict(err) {
		w.parent.seen.Store(true)
	}
	return err
}

var _ = Describe("ExternalPostgres manager reconciliation", func() {
	It("recovers on Secret creation, rotation, deletion and recreation and suppresses status-only events",
		func(ctx SpecContext) {
			p := &clusterProbe{seen: make(chan string, 32)}
			startClusterManager(ctx, p)
			c := watchedCluster()
			Expect(scope.Create(ctx, c)).To(Succeed())
			expectReady(ctx, c, metav1.ConditionFalse, "SecretNotFound")
			s := clusterAuth("auth", "valid")
			Expect(scope.Create(ctx, s)).To(Succeed())
			expectReady(ctx, c, metav1.ConditionTrue, "Ready")
			latest := &epco.ExternalPostgres{}
			Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(c), latest)).To(Succeed())
			successful := latest.Status.LastSuccessfulReconcileTime.DeepCopy()
			before := p.count()
			latest.Status.Message = "independent status edit"
			Expect(environment.Client.Status().Update(ctx, latest)).To(Succeed())
			Consistently(p.count).WithContext(ctx).WithTimeout(500 * time.Millisecond).Should(Equal(before))
			s.Data[passwordKey] = []byte("broken")
			Expect(environment.Client.Update(ctx, s)).To(Succeed())
			expectReady(ctx, c, metav1.ConditionFalse, "ConnectionUnavailable")
			Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(c), latest)).To(Succeed())
			Expect(latest.Status.LastSuccessfulReconcileTime).To(Equal(successful))
			Expect(environment.Client.Delete(ctx, s)).To(Succeed())
			expectReady(ctx, c, metav1.ConditionFalse, "SecretNotFound")
			s = clusterAuth("auth", "fixed")
			Expect(scope.Create(ctx, s)).To(Succeed())
			expectReady(ctx, c, metav1.ConditionTrue, "Ready")
		}, NodeTimeout(60*time.Second))

	It("moves the Secret watch when the reference changes", func(ctx SpecContext) {
		p := &clusterProbe{seen: make(chan string, 32)}
		startClusterManager(ctx, p)
		old := clusterAuth("auth", "first")
		other := clusterAuth("other", "second")
		Expect(scope.Create(ctx, old)).To(Succeed())
		Expect(scope.Create(ctx, other)).To(Succeed())
		c := watchedCluster()
		Expect(scope.Create(ctx, c)).To(Succeed())
		Eventually(p.seen).WithContext(ctx).Should(Receive(Equal("first")))
		expectReady(ctx, c, metav1.ConditionTrue, "Ready")
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(c), c)).To(Succeed())
		c.Spec.CredentialsSecret.Name = other.Name
		Expect(environment.Client.Update(ctx, c)).To(Succeed())
		Eventually(p.seen).WithContext(ctx).Should(Receive(Equal("second")))
		expectReady(ctx, c, metav1.ConditionTrue, "Ready")
		before := p.count()
		old.Data[passwordKey] = []byte("unused")
		Expect(environment.Client.Update(ctx, old)).To(Succeed())
		Consistently(p.count).WithContext(ctx).WithTimeout(500 * time.Millisecond).Should(Equal(before))
		other.Data[passwordKey] = []byte("broken")
		Expect(environment.Client.Update(ctx, other)).To(Succeed())
		expectReady(ctx, c, metav1.ConditionFalse, "ConnectionUnavailable")
	}, NodeTimeout(45*time.Second))

	It("discards a successful probe when its Secret changes in flight", func(ctx SpecContext) {
		release := make(chan struct{})
		p := &clusterProbe{seen: make(chan string, 32), block: release}
		startClusterManager(ctx, p)
		s := clusterAuth("auth", "valid")
		Expect(scope.Create(ctx, s)).To(Succeed())
		c := watchedCluster()
		Expect(scope.Create(ctx, c)).To(Succeed())
		Eventually(p.seen).WithContext(ctx).Should(Receive(Equal("valid")))
		s.Data[passwordKey] = []byte("broken")
		Expect(environment.Client.Update(ctx, s)).To(Succeed())
		close(release)
		Eventually(p.seen).WithContext(ctx).Should(Receive(Equal("broken")))
		expectReady(ctx, c, metav1.ConditionFalse, "ConnectionUnavailable")
		latest := &epco.ExternalPostgres{}
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(c), latest)).To(Succeed())
		Expect(latest.Status.AppliedGeneration).To(BeZero())
		Expect(latest.Status.LastSuccessfulReconcileTime).To(BeNil())
	}, NodeTimeout(45*time.Second))

	It("skips publication when the CR is deleted during a check", func(ctx SpecContext) {
		release := make(chan struct{})
		p := &clusterProbe{seen: make(chan string, 32), block: release}
		startClusterManager(ctx, p)
		Expect(scope.Create(ctx, clusterAuth("auth", "valid"))).To(Succeed())
		c := watchedCluster()
		Expect(scope.Create(ctx, c)).To(Succeed())
		Eventually(p.seen).WithContext(ctx).Should(Receive(Equal("valid")))
		Expect(environment.Client.Delete(ctx, c)).To(Succeed())
		close(release)
		Consistently(p.count).WithContext(ctx).WithTimeout(500 * time.Millisecond).Should(Equal(1))
	}, NodeTimeout(45*time.Second))
	It("uses a real API optimistic lock and checks fresh inputs after a status conflict", func(ctx SpecContext) {
		p := &clusterProbe{seen: make(chan string, 32)}
		observed := &atomic.Bool{}
		startClusterManagerWithClient(ctx,
			p,
			func(c client.Client) client.Client {
				return &conflictStatusClient{Client: c,
					seen: observed}
			})
		Expect(scope.Create(ctx, clusterAuth("auth", "valid"))).To(Succeed())
		c := watchedCluster()
		Expect(scope.Create(ctx, c)).To(Succeed())
		expectReady(ctx, c, metav1.ConditionTrue, "Ready")
		Expect(observed.Load()).To(BeTrue(), "status patch must conflict with a real concurrent API writer")
		Expect(p.count()).To(BeNumerically(">=", 2), "conflicting result must be discarded and SQL check rerun")
	}, NodeTimeout(45*time.Second))
	It("discards a successful check when the spec changes in flight", func(ctx SpecContext) {
		release := make(chan struct{})
		p := &clusterProbe{seen: make(chan string, 32), block: release}
		startClusterManager(ctx, p)
		Expect(scope.Create(ctx, clusterAuth("auth", "valid"))).To(Succeed())
		Expect(scope.Create(ctx, clusterAuth("other", "broken"))).To(Succeed())
		c := watchedCluster()
		Expect(scope.Create(ctx, c)).To(Succeed())
		Eventually(p.seen).WithContext(ctx).Should(Receive(Equal("valid")))
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(c), c)).To(Succeed())
		c.Spec.CredentialsSecret.Name = "other"
		Expect(environment.Client.Update(ctx, c)).To(Succeed())
		close(release)
		Eventually(p.seen).WithContext(ctx).Should(Receive(Equal("broken")))
		expectReady(ctx, c, metav1.ConditionFalse, "ConnectionUnavailable")
		latest := &epco.ExternalPostgres{}
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(c), latest)).To(Succeed())
		Expect(latest.Status.ObservedGeneration).To(Equal(c.Generation))
		Expect(latest.Status.AppliedGeneration).To(BeZero())
		Expect(latest.Status.LastSuccessfulReconcileTime).To(BeNil())
	}, NodeTimeout(45*time.Second))
	It("watches CA and client certificate inputs and removes their indexes when TLS is disabled", func(ctx SpecContext) {
		fixture, err := integration.NewTLSFixture(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		ca, err := os.ReadFile(fixture.CA)
		Expect(err).NotTo(HaveOccurred())
		crt, err := os.ReadFile(fixture.ClientCertificate)
		Expect(err).NotTo(HaveOccurred())
		key, err := os.ReadFile(fixture.ClientKey)
		Expect(err).NotTo(HaveOccurred())
		caSecret := &corev1.Secret{Name: "ca", Namespace: namespace.Name, Data: map[string][]byte{databaseTestCAKey: ca}}
		clientSecret := &corev1.Secret{Name: "client",
			Namespace: namespace.Name,
			Type:      corev1.SecretTypeTLS,
			Data: map[string][]byte{corev1.TLSCertKey: crt,
				corev1.TLSPrivateKeyKey: key}}
		p := &clusterProbe{seen: make(chan string, 32)}
		startClusterManager(ctx, p)
		for _, s := range []*corev1.Secret{clusterAuth("auth", "valid"), caSecret, clientSecret} {
			Expect(scope.Create(ctx, s)).To(Succeed())
		}
		c := watchedCluster()
		c.Spec.TLS = &epco.PostgresTLS{Mode: "verify-full",
			CASecret: &epco.SecretKeyReference{Name: caSecret.Name,
				Key: databaseTestCAKey},
			ClientCertificateSecret: &epco.LocalReference{Name: clientSecret.Name}}
		Expect(scope.Create(ctx, c)).To(Succeed())
		expectReady(ctx, c, metav1.ConditionTrue, "Ready")
		caSecret.Data[databaseTestCAKey] = []byte("invalid")
		Expect(environment.Client.Update(ctx, caSecret)).To(Succeed())
		expectReady(ctx, c, metav1.ConditionFalse, "InvalidSecret")
		caSecret.Data[databaseTestCAKey] = ca
		Expect(environment.Client.Update(ctx, caSecret)).To(Succeed())
		expectReady(ctx, c, metav1.ConditionTrue, "Ready")
		clientSecret.Data[corev1.TLSPrivateKeyKey] = []byte("invalid")
		Expect(environment.Client.Update(ctx, clientSecret)).To(Succeed())
		expectReady(ctx, c, metav1.ConditionFalse, "InvalidSecret")
		clientSecret.Data[corev1.TLSPrivateKeyKey] = key
		Expect(environment.Client.Update(ctx, clientSecret)).To(Succeed())
		expectReady(ctx, c, metav1.ConditionTrue, "Ready")
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(c), c)).To(Succeed())
		c.Spec.TLS.Mode = "disable"
		Expect(environment.Client.Update(ctx, c)).To(Succeed())
		Eventually(func() int64 {
			condition := clusterCondition(ctx, c)
			if condition == nil {
				return 0
			}
			return condition.ObservedGeneration
		}).WithContext(ctx).Should(Equal(c.Generation))
		before := p.count()
		Expect(environment.Client.Delete(ctx, caSecret)).To(Succeed())
		Expect(environment.Client.Delete(ctx, clientSecret)).To(Succeed())
		Consistently(p.count).WithContext(ctx).WithTimeout(500 * time.Millisecond).Should(Equal(before))
		expectReady(ctx, c, metav1.ConditionTrue, "Ready")
	}, NodeTimeout(60*time.Second))

})
