//go:build envtest

package envtest_test

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/controller"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type connectionAPIAdoptionFixture struct {
	database *epco.Database
	output   *corev1.Secret
	role     *epco.DatabaseRole
	source   *corev1.Secret
}

func connectionAPIRetainedOutput(ctx context.Context, name string) connectionAPIAdoptionFixture {
	cluster, role, source := connectionAPIFixture(ctx)
	former := connectionAPIDatabase(cluster, name)
	former.Spec.ReclaimPolicy = epco.ReclaimRetain
	ExpectWithOffset(1, scope.Create(ctx, former)).To(Succeed())
	reconciler := connectionAPIDirectReconciler(environment.Client)
	_, err := reconciler.Reconcile(ctx, connectionAPIRequestFor(former))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	output := connectionAPIOutput(ctx, former).DeepCopy()
	ExpectWithOffset(1, output.Annotations[controller.ConnectionDatabaseUIDAnnotation]).To(Equal(string(former.UID)))
	ExpectWithOffset(1, environment.Client.Delete(ctx, former)).To(Succeed())
	_, err = reconciler.Reconcile(ctx, connectionAPIRequestFor(former))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, scope.WaitGone(ctx, former)).To(Succeed())
	ExpectWithOffset(1, connectionAPIOutput(ctx, former).UID).To(Equal(output.UID))

	db := connectionAPIDatabase(cluster, name)
	db.Spec.ReclaimPolicy = epco.ReclaimDelete
	ExpectWithOffset(1, scope.Create(ctx, db)).To(Succeed())
	ExpectWithOffset(1, db.UID).NotTo(Equal(former.UID))
	ExpectWithOffset(1, connectionAPIHistoryContains(ctx, db, output.Name)).To(BeFalse())
	return connectionAPIAdoptionFixture{database: db, output: output, role: role, source: source}
}

func connectionAPIAdoptionPending(ctx context.Context, db *epco.Database) {
	latest := &epco.Database{}
	ExpectWithOffset(1, environment.Client.Get(ctx, client.ObjectKeyFromObject(db), latest)).To(Succeed())
	condition := meta.FindStatusCondition(latest.Status.Conditions, databaseTestReadyCondition)
	ExpectWithOffset(1, condition).NotTo(BeNil())
	ExpectWithOffset(1, condition.Status).To(Equal(metav1.ConditionFalse))
	ExpectWithOffset(1, condition.ObservedGeneration).To(Equal(latest.Generation))
	ExpectWithOffset(1, latest.Status.HasManagedSQL).To(BeFalse())
}

var _ = Describe("R4 retained connection output adoption", func() {
	It("publishes an independent initial output while another retained adoption keeps failing",
		func(ctx SpecContext) {
			fixture := connectionAPIRetainedOutput(ctx, "independent-adoption")
			db, retained := fixture.database, fixture.output
			fixture.source.Data[passwordKey] = []byte("current-source-for-never-published-output")
			Expect(environment.Client.Update(ctx, fixture.source)).To(Succeed())
			initial := epco.ConnectionSecret{Name: "independent-initial-output",
				RoleRef: epco.LocalReference{Name: fixture.role.Name}}
			db.Spec.ConnectionSecrets = append(db.Spec.ConnectionSecrets, initial)
			Expect(environment.Client.Update(ctx, db)).To(Succeed())
			failure := apierrors.NewServiceUnavailable("isolated retained-output adoption rejection")
			attempted := false
			reconciler := connectionAPIDirectReconciler(interceptor.NewClient(connectionAPIWatchClient(), interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, object client.Object,
					options ...client.UpdateOption) error {
					if _, ok := object.(*corev1.Secret); ok &&
						client.ObjectKeyFromObject(object) == client.ObjectKeyFromObject(retained) {
						attempted = true
						return failure
					}
					return c.Update(ctx, object, options...)
				},
			}))
			initialKey := client.ObjectKey{Namespace: db.Namespace, Name: initial.Name}
			for range 2 {
				_, err := reconciler.Reconcile(ctx, connectionAPIRequestFor(db))
				Expect(err == nil || errors.Is(err, failure)).To(BeTrue())
				Expect(attempted).To(BeTrue(), "valid retained ownership must reach the rejected adoption write")
				after := connectionAPIOutput(ctx, db)
				Expect(after.UID).To(Equal(retained.UID))
				Expect(after.ResourceVersion).To(Equal(retained.ResourceVersion))
				Expect(maps.Equal(after.Annotations, retained.Annotations)).To(BeTrue())
				Expect(maps.EqualFunc(after.Data, retained.Data, bytes.Equal)).To(BeTrue())
				connectionAPIAdoptionPending(ctx, db)
				published := &corev1.Secret{}
				readErr := environment.Client.Get(ctx, initialKey, published)
				publishedSuccessfully := readErr == nil
				Expect(publishedSuccessfully).To(BeTrue(),
					"a retained adoption rejection must not block a valid never-published output during SQL outage")
				Expect(published.Type).To(Equal(corev1.SecretTypeBasicAuth))
				Expect(bytes.Equal(published.Data[passwordKey], fixture.source.Data[passwordKey])).To(BeTrue())
				Expect(published.Annotations[controller.ConnectionDatabaseUIDAnnotation]).To(Equal(string(db.UID)))
				Expect(published.OwnerReferences).To(BeEmpty())
			}

			reconciler.Client = environment.Client
			_, err := reconciler.Reconcile(ctx, connectionAPIRequestFor(db))
			Expect(err).NotTo(HaveOccurred())
			adopted := connectionAPIOutput(ctx, db)
			Expect(adopted.UID).To(Equal(retained.UID))
			Expect(adopted.Annotations[controller.ConnectionDatabaseUIDAnnotation]).To(Equal(string(db.UID)))
			Expect(maps.EqualFunc(adopted.Data, retained.Data, bytes.Equal)).To(BeTrue(),
				"adoption during SQL outage must preserve the retained payload until current-source SQL confirmation")
			connectionAPIAdoptionPending(ctx, db)
		}, NodeTimeout(30*time.Second))

	It("does not leak current-UID ownership after an adoption history checkpoint fails and the new CR is deleted",
		func(ctx SpecContext) {
			fixture := connectionAPIRetainedOutput(ctx, "adoption-checkpoint-loss")
			db, retained := fixture.database, fixture.output
			failure := apierrors.NewServiceUnavailable("isolated adoption history checkpoint rejection")
			rejected := false
			reconciler := connectionAPIDirectReconciler(interceptor.NewClient(connectionAPIWatchClient(), interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string,
					object client.Object, options ...client.SubResourceUpdateOption) error {
					if current, ok := object.(*epco.Database); ok && subresource == connectionAPIStatusSubresource &&
						!rejected && slices.ContainsFunc(current.Status.ConnectionSecretHistory,
						func(entry epco.ConnectionSecretState) bool { return entry.Name == retained.Name }) {
						rejected = true
						return failure
					}
					return c.SubResource(subresource).Update(ctx, object, options...)
				},
			}))
			_, err := reconciler.Reconcile(ctx, connectionAPIRequestFor(db))
			Expect(errors.Is(err, failure)).To(BeTrue(), "test must reach the adoption history persistence failure")
			Expect(rejected).To(BeTrue())
			latest := &epco.Database{}
			Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(db), latest)).To(Succeed())
			Expect(latest.Status.HasManagedSQL).To(BeFalse())
			condition := meta.FindStatusCondition(latest.Status.Conditions, databaseTestReadyCondition)
			unready := condition == nil || condition.Status == metav1.ConditionFalse
			Expect(unready).To(BeTrue(), "an adoption checkpoint rejection cannot establish readiness")
			Expect(latest.Finalizers).To(ContainElement(controller.DatabaseFinalizer))
			Expect(connectionAPIHistoryContains(ctx, db, retained.Name)).To(BeFalse())

			Expect(environment.Client.Delete(ctx, db)).To(Succeed())
			restarted := connectionAPIDirectReconciler(environment.Client)
			_, err = restarted.Reconcile(ctx, connectionAPIRequestFor(db))
			Expect(err).NotTo(HaveOccurred())
			Expect(scope.WaitGone(ctx, db)).To(Succeed())
			after := &corev1.Secret{}
			readErr := environment.Client.Get(ctx, client.ObjectKeyFromObject(retained), after)
			if apierrors.IsNotFound(readErr) {
				return
			}
			Expect(readErr).NotTo(HaveOccurred())
			Expect(after.UID).To(Equal(retained.UID))
			Expect(maps.EqualFunc(after.Data, retained.Data, bytes.Equal)).To(BeTrue())
			formerOwnershipPreserved := after.Annotations[controller.ConnectionDatabaseUIDAnnotation] ==
				retained.Annotations[controller.ConnectionDatabaseUIDAnnotation]
			Expect(formerOwnershipPreserved).To(BeTrue(),
				"failed adoption history must prevent ownership mutation or clean the current-UID output before CR deletion")
		}, NodeTimeout(30*time.Second))

	It("converges rejected adoption cleanup without deleting an unclaimed retained output or leaking current ownership",
		func(ctx SpecContext) {
			fixture := connectionAPIRetainedOutput(ctx, "rejected-adoption-cleanup")
			db, retained := fixture.database, fixture.output
			failure := apierrors.NewServiceUnavailable("isolated adoption write rejection before omission")
			attempted := false
			reconciler := connectionAPIDirectReconciler(interceptor.NewClient(connectionAPIWatchClient(), interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, object client.Object,
					options ...client.UpdateOption) error {
					if _, ok := object.(*corev1.Secret); ok &&
						client.ObjectKeyFromObject(object) == client.ObjectKeyFromObject(retained) {
						attempted = true
						return failure
					}
					return c.Update(ctx, object, options...)
				},
			}))
			_, err := reconciler.Reconcile(ctx, connectionAPIRequestFor(db))
			Expect(err == nil || errors.Is(err, failure)).To(BeTrue())
			Expect(attempted).To(BeTrue())
			after := connectionAPIOutput(ctx, db)
			Expect(after.UID).To(Equal(retained.UID))
			Expect(maps.Equal(after.Annotations, retained.Annotations)).To(BeTrue())
			Expect(maps.EqualFunc(after.Data, retained.Data, bytes.Equal)).To(BeTrue())
			adoptionClaimed := connectionAPIHistoryHasUID(ctx, db, retained.Name, string(retained.UID))

			Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(db), db)).To(Succeed())
			db.Spec.ConnectionSecrets = nil
			Expect(environment.Client.Update(ctx, db)).To(Succeed())
			connectionAPIApplyRole(ctx, connectionAPIRoleReconciler(), fixture.role)
			restarted := connectionAPIDirectReconciler(environment.Client)
			restarted.Databases = &connectionAPIFactory{}
			_, err = restarted.Reconcile(ctx, connectionAPIRequestFor(db))
			Expect(err).NotTo(HaveOccurred())
			expectDatabaseStatus(ctx, db, metav1.ConditionTrue, databaseTestReadyCondition)
			Expect(connectionAPIHistoryContains(ctx, db, retained.Name)).To(BeFalse(),
				"an uncommitted adoption must not leave permanent cleanup responsibility for the former owner's output")
			Expect(environment.Client.Delete(ctx, db)).To(Succeed())
			_, err = restarted.Reconcile(ctx, connectionAPIRequestFor(db))
			Expect(err).NotTo(HaveOccurred())
			Expect(scope.WaitGone(ctx, db)).To(Succeed())
			readErr := environment.Client.Get(ctx, client.ObjectKeyFromObject(retained), after)
			if apierrors.IsNotFound(readErr) {
				Expect(adoptionClaimed).To(BeTrue(),
					"only a committed exact-UID adoption intent can authorize removal of the former retained output")
				return
			}
			Expect(readErr).NotTo(HaveOccurred())
			Expect(after.UID).To(Equal(retained.UID))
			Expect(maps.Equal(after.Annotations, retained.Annotations)).To(BeTrue())
			Expect(maps.EqualFunc(after.Data, retained.Data, bytes.Equal)).To(BeTrue())
		}, NodeTimeout(30*time.Second))
})
