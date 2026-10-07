//go:build envtest

package envtest_test

import (
	"bytes"
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("scope ownership and cleanup", func() {
	It("isolates namespaces and explicitly removes finalized objects and the namespace", func(ctx SpecContext) {
		other, otherNamespace, err := environment.NewScope(ctx, "epco-api-")
		Expect(err).NotTo(HaveOccurred())
		Expect(otherNamespace.Name).NotTo(Equal(namespace.Name))
		DeferCleanup(func(ctx SpecContext) { Expect(other.Cleanup(ctx)).To(Succeed()) })
		secret := &corev1.Secret{Name: "owned", Namespace: otherNamespace.Name, Finalizers: []string{"tests.epco.io/cleanup"}}
		Expect(scope.Create(ctx, secret)).To(MatchError(ContainSubstring("outside scope")))
		Expect(other.Create(ctx, secret)).To(Succeed())
		Expect(other.Cleanup(ctx)).To(Succeed())
		Expect(apierrors.IsNotFound(environment.Client.Get(ctx, client.ObjectKeyFromObject(secret), secret))).To(BeTrue())
		err = environment.Client.Get(ctx, client.ObjectKeyFromObject(otherNamespace), otherNamespace)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(namespace), namespace)).To(Succeed())
	})

	It("preserves a replacement object at a previously owned name", func(ctx SpecContext) {
		original := &corev1.Secret{Name: "recreated", Namespace: namespace.Name}
		Expect(scope.Create(ctx, original)).To(Succeed())
		Expect(environment.Client.Delete(ctx, original)).To(Succeed())
		Expect(scope.WaitGone(ctx, original)).To(Succeed())
		replacement := &corev1.Secret{
			Name: original.Name, Namespace: original.Namespace,
			Finalizers: []string{"tests.epco.io/replacement"},
			Data:       map[string][]byte{passwordKey: []byte("replacement")},
		}
		Expect(environment.Client.Create(ctx, replacement)).To(Succeed())
		cleanupReplacement := func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			current := &corev1.Secret{}
			if err := environment.Client.Get(cleanupCtx, client.ObjectKeyFromObject(replacement), current); err == nil {
				current.Finalizers = nil
				Expect(environment.Client.Update(cleanupCtx, current)).To(Succeed())
				Expect(environment.Client.Delete(cleanupCtx, current)).To(Succeed())
			}
		}
		DeferCleanup(cleanupReplacement)
		Expect(replacement.UID).NotTo(Equal(original.UID))
		Expect(scope.Cleanup(ctx)).To(MatchError(ContainSubstring("ownership changed")))
		current := &corev1.Secret{}
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(replacement), current)).To(Succeed())
		Expect(current.UID).To(Equal(replacement.UID))
		Expect(current.ResourceVersion).To(Equal(replacement.ResourceVersion))
		Expect(current.Finalizers).To(Equal(replacement.Finalizers))
		Expect(current.Data).To(Equal(replacement.Data))
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(namespace), namespace)).To(Succeed())
		cleanupReplacement()
	})

	It("captures useful metadata without Secret data, annotations or arbitrary status text", func(ctx SpecContext) {
		secret := &corev1.Secret{
			Name: "diagnostic", Namespace: namespace.Name,
			Annotations: map[string]string{"sensitive": "Bearer hidden-token"},
			Data:        map[string][]byte{passwordKey: []byte("hidden-password")},
		}
		Expect(scope.Create(ctx, secret)).To(Succeed())
		object := resource("DatabaseRole")
		Expect(scope.Create(ctx, object)).To(Succeed())
		setField(object, "postgres://user:hidden-password@host/db", "status", "message")
		Expect(environment.Client.Status().Update(ctx, object)).To(Succeed())
		var output bytes.Buffer
		Expect(scope.Diagnostics(ctx, &output)).To(Succeed())
		Expect(output.String()).To(ContainSubstring("diagnostic"))
		Expect(output.String()).To(ContainSubstring("resourceVersion="))
		Expect(output.String()).NotTo(ContainSubstring("hidden-password"))
		Expect(output.String()).NotTo(ContainSubstring("hidden-token"))
		Expect(output.String()).NotTo(ContainSubstring("postgres://"))
	})
})
