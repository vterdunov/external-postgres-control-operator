//go:build envtest

package envtest_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/controller"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func connectionAPIWatchClient() client.WithWatch {
	direct, err := client.NewWithWatch(environment.Config, client.Options{Scheme: environment.Scheme})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return direct
}

func connectionAPIDirectReconciler(cl client.Client) *controller.DatabaseReconciler {
	builder, err := postgres.NewConfigBuilder()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return &controller.DatabaseReconciler{Client: cl, APIReader: environment.Client,
		Builder: builder, Databases: connectionAPIOfflineFactory{}, Locks: &controller.ManagementLocks{}}
}

var _ = Describe("R4 Database connection durable API recovery", func() {
	It("preserves precreate history after post-create status failure "+
		"and blocks early recreation after restart", func(ctx SpecContext) {
		cluster, _, source := connectionAPIFixture(ctx)
		db := connectionAPIDatabase(cluster, "uncertain-publication")
		Expect(scope.Create(ctx, db)).To(Succeed())
		failure := apierrors.NewServiceUnavailable("isolated post-create status failure")
		failAfterCreate := func(ctx context.Context) bool {
			return environment.Client.Get(ctx, connectionAPIOutputKey(db), &corev1.Secret{}) == nil
		}
		decorated := interceptor.NewClient(connectionAPIWatchClient(), interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string,
				object client.Object, options ...client.SubResourceUpdateOption) error {
				if _, ok := object.(*epco.Database); ok && subresource == connectionAPIStatusSubresource && failAfterCreate(ctx) {
					return failure
				}
				return c.SubResource(subresource).Update(ctx, object, options...)
			},
			SubResourcePatch: func(ctx context.Context, c client.Client, subresource string,
				object client.Object, patch client.Patch, options ...client.SubResourcePatchOption) error {
				if _, ok := object.(*epco.Database); ok && subresource == connectionAPIStatusSubresource && failAfterCreate(ctx) {
					return failure
				}
				return c.SubResource(subresource).Patch(ctx, object, patch, options...)
			},
		})
		reconciler := connectionAPIDirectReconciler(decorated)
		_, err := reconciler.Reconcile(ctx, connectionAPIRequestFor(db))
		Expect(errors.Is(err, failure)).To(BeTrue(), "created output must survive its later status persistence failure")
		output := connectionAPIOutput(ctx, db)
		latest := &epco.Database{}
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(db), latest)).To(Succeed())
		status, err := json.Marshal(latest.Status)
		Expect(err).NotTo(HaveOccurred())
		Expect(bytes.Contains(status, []byte(output.Name))).To(BeTrue(),
			"output intent must already be durable before CREATE")
		Expect(bytes.Contains(status, source.Data[passwordKey])).To(BeFalse(),
			"durable output history may not contain credentials")
		Expect(environment.Client.Delete(ctx, output)).To(Succeed())
		source.Data[passwordKey] = []byte("unconfirmed-after-uncertain-publication")
		Expect(environment.Client.Update(ctx, source)).To(Succeed())
		restarted := connectionAPIDirectReconciler(environment.Client)
		_, err = restarted.Reconcile(ctx, connectionAPIRequestFor(db))
		Expect(err).NotTo(HaveOccurred())
		Expect(apierrors.IsNotFound(environment.Client.Get(ctx, connectionAPIOutputKey(db), &corev1.Secret{}))).To(BeTrue(),
			"a persisted publication attempt is an existing-output SQL gate after restart")
	}, NodeTimeout(30*time.Second))

	It("uses UID and resourceVersion preconditions when cleaning an omitted output", func(ctx SpecContext) {
		cluster, _, source := connectionAPIFixture(ctx)
		db := connectionAPIDatabase(cluster, "conditional-cleanup")
		Expect(scope.Create(ctx, db)).To(Succeed())
		reconciler := connectionAPIDirectReconciler(environment.Client)
		_, err := reconciler.Reconcile(ctx, connectionAPIRequestFor(db))
		Expect(err).NotTo(HaveOccurred())
		original := connectionAPIOutput(ctx, db).DeepCopy()
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(db), db)).To(Succeed())
		db.Spec.ConnectionSecrets = nil
		Expect(environment.Client.Update(ctx, db)).To(Succeed())
		replacement := &corev1.Secret{Namespace: original.Namespace, Name: original.Name,
			Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"independent": []byte("independent-replacement-value")}}
		interposed := false
		reconciler.Client = interceptor.NewClient(connectionAPIWatchClient(), interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.DeleteOption) error {
				if _, ok := object.(*corev1.Secret); !ok ||
					client.ObjectKeyFromObject(object) != client.ObjectKeyFromObject(original) {
					return c.Delete(ctx, object, options...)
				}
				if !interposed {
					interposed = true
					Expect(c.Delete(ctx, original)).To(Succeed())
					Expect(c.Create(ctx, replacement)).To(Succeed())
				}
				// The real API must reject the old UID/RV after same-name recreation.
				return c.Delete(ctx, object, options...)
			},
		})
		_, err = reconciler.Reconcile(ctx, connectionAPIRequestFor(db))
		Expect(err == nil || apierrors.IsConflict(err)).To(BeTrue(),
			"conditional cleanup must retry safely on a real API conflict")
		Expect(interposed).To(BeTrue(), "omitted output must reach conditional cleanup")
		after := &corev1.Secret{}
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(original), after)).To(Succeed())
		Expect(after.UID).NotTo(Equal(original.UID))
		Expect(after.Type).To(Equal(corev1.SecretTypeOpaque))
		Expect(bytes.Equal(after.Data["independent"], replacement.Data["independent"])).To(BeTrue(),
			"cleanup may never delete an independent same-name replacement")
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(source), source)).To(Succeed())
		Expect(bytes.Equal(source.Data[passwordKey], []byte("initial-api-application-value"))).To(BeTrue())
	}, NodeTimeout(30*time.Second))

	It("preserves a same-UID output when its provenance changes before cleanup reaches the API", func(ctx SpecContext) {
		cluster, _, _ := connectionAPIFixture(ctx)
		db := connectionAPIDatabase(cluster, "concurrent-provenance")
		Expect(scope.Create(ctx, db)).To(Succeed())
		reconciler := connectionAPIDirectReconciler(environment.Client)
		_, err := reconciler.Reconcile(ctx, connectionAPIRequestFor(db))
		Expect(err).NotTo(HaveOccurred())
		original := connectionAPIOutput(ctx, db).DeepCopy()
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(db), db)).To(Succeed())
		db.Spec.ConnectionSecrets = nil
		Expect(environment.Client.Update(ctx, db)).To(Succeed())
		interposed := false
		reconciler.Client = interceptor.NewClient(connectionAPIWatchClient(), interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.DeleteOption) error {
				if _, ok := object.(*corev1.Secret); !ok ||
					client.ObjectKeyFromObject(object) != client.ObjectKeyFromObject(original) {
					return c.Delete(ctx, object, options...)
				}
				if !interposed {
					interposed = true
					concurrent := &corev1.Secret{}
					Expect(c.Get(ctx, client.ObjectKeyFromObject(original), concurrent)).To(Succeed())
					concurrent.Annotations = map[string]string{"unrelated-owner": "concurrent-manager"}
					concurrent.Data = map[string][]byte{
						connectionAPIUsernameKey: bytes.Clone(original.Data[connectionAPIUsernameKey]),
						passwordKey:              bytes.Clone(original.Data[passwordKey]),
						"independent":            []byte("concurrent-independent-value"),
					}
					Expect(c.Update(ctx, concurrent)).To(Succeed())
					Expect(concurrent.UID).To(Equal(original.UID))
					Expect(concurrent.ResourceVersion).NotTo(Equal(original.ResourceVersion))
				}
				return c.Delete(ctx, object, options...)
			},
		})
		_, err = reconciler.Reconcile(ctx, connectionAPIRequestFor(db))
		Expect(err == nil || apierrors.IsConflict(err)).To(BeTrue())
		Expect(interposed).To(BeTrue())
		after := &corev1.Secret{}
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(original), after)).To(Succeed())
		Expect(after.UID).To(Equal(original.UID))
		Expect(after.Annotations["unrelated-owner"]).To(Equal("concurrent-manager"))
		Expect(bytes.Equal(after.Data["independent"], []byte("concurrent-independent-value"))).To(BeTrue(),
			"same UID alone is insufficient; cleanup must retain the RV precondition")
	}, NodeTimeout(30*time.Second))

	It("does not publish before a stale intent status checkpoint is accepted", func(ctx SpecContext) {
		cluster, _, _ := connectionAPIFixture(ctx)
		db := connectionAPIDatabase(cluster, "stale-intent")
		Expect(scope.Create(ctx, db)).To(Succeed())
		conflicted := false
		decorated := interceptor.NewClient(connectionAPIWatchClient(), interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string,
				object client.Object, options ...client.SubResourceUpdateOption) error {
				if _, ok := object.(*epco.Database); ok && subresource == connectionAPIStatusSubresource && !conflicted {
					conflicted = true
					concurrent := &epco.Database{}
					Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(db), concurrent)).To(Succeed())
					concurrent.Status.Message = "concurrent safe status writer"
					Expect(environment.Client.Status().Update(ctx, concurrent)).To(Succeed())
				}
				return c.SubResource(subresource).Update(ctx, object, options...)
			},
		})
		_, err := connectionAPIDirectReconciler(decorated).Reconcile(ctx, connectionAPIRequestFor(db))
		Expect(err == nil || apierrors.IsConflict(err)).To(BeTrue())
		Expect(conflicted).To(BeTrue())
		Expect(apierrors.IsNotFound(environment.Client.Get(ctx, connectionAPIOutputKey(db), &corev1.Secret{}))).To(BeTrue(),
			"real API RV conflict must prevent Secret CREATE before durable output intent")
	}, NodeTimeout(30*time.Second))
})
