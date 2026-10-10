//go:build envtest

package envtest_test

import (
	"bytes"
	"context"
	"maps"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

var _ = Describe("R4 connection output history continuity", func() {
	It("keeps the newer committed output UID when an older owned Secret remains in the cache",
		func(ctx SpecContext) {
			cluster, role, _ := connectionAPIFixture(ctx)
			connectionAPIApplyRole(ctx, connectionAPIRoleReconciler(), role)
			db := connectionAPIDatabase(cluster, "stale-present-history")
			Expect(scope.Create(ctx, db)).To(Succeed())
			reconciler := connectionAPIDirectReconciler(environment.Client)
			reconciler.Databases = &connectionAPIFactory{}
			_, err := reconciler.Reconcile(ctx, connectionAPIRequestFor(db))
			Expect(err).NotTo(HaveOccurred())
			expectDatabaseStatus(ctx, db, metav1.ConditionTrue, databaseTestReadyCondition)
			older := connectionAPIOutput(ctx, db).DeepCopy()
			Expect(environment.Client.Delete(ctx, older)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, connectionAPIRequestFor(db))
			Expect(err).NotTo(HaveOccurred())
			current := connectionAPIOutput(ctx, db).DeepCopy()
			Expect(current.UID).NotTo(Equal(older.UID))
			Expect(connectionAPIHistoryHasUID(ctx, db, current.Name, string(current.UID))).To(BeTrue(),
				"public SQL-confirmed recreation must record its new actual API incarnation")
			outputKey := client.ObjectKeyFromObject(current)
			reconciler.Client = interceptor.NewClient(connectionAPIWatchClient(), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
					object client.Object, options ...client.GetOption) error {
					if cached, ok := object.(*corev1.Secret); ok && key == outputKey {
						*cached = *older.DeepCopy()
						return nil
					}
					return c.Get(ctx, key, object, options...)
				},
			})
			reconciler.Databases = connectionAPIOfflineFactory{}
			_, err = reconciler.Reconcile(ctx, connectionAPIRequestFor(db))
			Expect(err).NotTo(HaveOccurred())
			after := &corev1.Secret{}
			Expect(environment.Client.Get(ctx, outputKey, after)).To(Succeed())
			Expect(after.UID).To(Equal(current.UID))
			Expect(maps.EqualFunc(after.Data, current.Data, bytes.Equal)).To(BeTrue())
			Expect(connectionAPIHistoryHasUID(ctx, db, current.Name, string(current.UID))).To(BeTrue(),
				"an older cached incarnation must not replace newer durable cleanup responsibility")

			Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(db), db)).To(Succeed())
			db.Spec.ConnectionSecrets = nil
			Expect(environment.Client.Update(ctx, db)).To(Succeed())
			reconciler.Client = environment.Client
			reconciler.Databases = &connectionAPIFactory{}
			_, err = reconciler.Reconcile(ctx, connectionAPIRequestFor(db))
			Expect(err).NotTo(HaveOccurred())
			readErr := environment.Client.Get(ctx, outputKey, after)
			Expect(apierrors.IsNotFound(readErr)).To(BeTrue(),
				"omission after cache convergence must delete the current own output rather than forget it")
			Expect(connectionAPIHistoryContains(ctx, db, current.Name)).To(BeFalse())
			expectDatabaseStatus(ctx, db, metav1.ConditionTrue, databaseTestReadyCondition)
		}, NodeTimeout(30*time.Second))
})
