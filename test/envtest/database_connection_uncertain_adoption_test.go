//go:build envtest

package envtest_test

import (
	"bytes"
	"context"
	"maps"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/vterdunov/external-postgres-control-operator/internal/controller"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

var _ = Describe("R4 uncertain initial connection output adoption", func() {
	It("adopts a valid retained output after cached absence and real AlreadyExists without republishing its payload",
		func(ctx SpecContext) {
			fixture := connectionAPIRetainedOutput(ctx, "uncertain-retained-create")
			db, retained := fixture.database, fixture.output
			fixture.source.Data[passwordKey] = []byte("latest-source-before-uncertain-create")
			Expect(environment.Client.Update(ctx, fixture.source)).To(Succeed())
			outputKey := client.ObjectKeyFromObject(retained)
			alreadyExisted := false
			reconciler := connectionAPIDirectReconciler(interceptor.NewClient(connectionAPIWatchClient(), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
					object client.Object, options ...client.GetOption) error {
					if _, ok := object.(*corev1.Secret); ok && key == outputKey {
						return apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, key.Name)
					}
					return c.Get(ctx, key, object, options...)
				},
				Create: func(ctx context.Context, c client.WithWatch, object client.Object,
					options ...client.CreateOption) error {
					err := c.Create(ctx, object, options...)
					if _, ok := object.(*corev1.Secret); ok && client.ObjectKeyFromObject(object) == outputKey {
						alreadyExisted = apierrors.IsAlreadyExists(err)
					}
					return err
				},
			}))
			_, err := reconciler.Reconcile(ctx, connectionAPIRequestFor(db))
			Expect(err).NotTo(HaveOccurred())
			Expect(alreadyExisted).To(BeTrue(), "the real API must reject CREATE while the retained output already exists")
			intent := connectionAPIHistoryEntry(ctx, db, retained.Name)
			Expect(intent).NotTo(BeNil(), "the uncertain CREATE attempt must retain its durable output intent")
			unknownIncarnation := intent["uid"] == nil || intent["uid"] == ""
			Expect(unknownIncarnation).To(BeTrue())
			after := connectionAPIOutput(ctx, db)
			Expect(after.UID).To(Equal(retained.UID))
			Expect(after.ResourceVersion).To(Equal(retained.ResourceVersion))
			Expect(maps.Equal(after.Annotations, retained.Annotations)).To(BeTrue())
			Expect(maps.EqualFunc(after.Data, retained.Data, bytes.Equal)).To(BeTrue())

			// The informer has caught up. The persisted CREATE attempt gates
			// payload recreation, but does not forbid validated ownership adoption.
			reconciler.Client = environment.Client
			for range 2 {
				_, err = reconciler.Reconcile(ctx, connectionAPIRequestFor(db))
				Expect(err).NotTo(HaveOccurred())
				adopted := connectionAPIOutput(ctx, db)
				Expect(adopted.UID).To(Equal(retained.UID))
				Expect(adopted.Annotations[controller.ConnectionDatabaseUIDAnnotation]).To(Equal(string(db.UID)),
					"normal cache retry must adopt validated retained ownership after an uncertain CREATE")
				Expect(maps.EqualFunc(adopted.Data, retained.Data, bytes.Equal)).To(BeTrue(),
					"ownership recovery must preserve the retained payload while the current source has no SQL receipt")
				Expect(connectionAPIHistoryHasUID(ctx, db, retained.Name, string(retained.UID))).To(BeTrue())
				connectionAPIAdoptionPending(ctx, db)
			}
			Expect(environment.Client.Delete(ctx, db)).To(Succeed())
			_, err = connectionAPIDirectReconciler(environment.Client).Reconcile(ctx, connectionAPIRequestFor(db))
			Expect(err).NotTo(HaveOccurred())
			Expect(scope.WaitGone(ctx, db)).To(Succeed())
			readErr := environment.Client.Get(ctx, outputKey, &corev1.Secret{})
			Expect(apierrors.IsNotFound(readErr)).To(BeTrue(), "pre-SQL reclaim must clean the adopted exact output")
		}, NodeTimeout(30*time.Second))
})
