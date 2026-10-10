//go:build envtest

package envtest_test

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/controller"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func extensionAPIReconciler(cl client.Client, factory *extensionAPIFactory) *controller.DatabaseReconciler {
	r := connectionAPIDirectReconciler(cl)
	r.Databases = factory
	return r
}

var _ = Describe("R5 Database extensions durable API recovery", func() {
	It("reconciles current extensions and latest spec after a completed SQL change loses its status write",
		func(ctx SpecContext) {
			factory := &extensionAPIFactory{}
			cluster := createRoleDescriptor(ctx, nil)
			db := extensionAPIDatabase(cluster, "extension-recovery")
			Expect(scope.Create(ctx, db)).To(Succeed())
			failure := apierrors.NewServiceUnavailable("isolated extension status failure")
			decorated := interceptor.NewClient(connectionAPIWatchClient(), interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string,
					object client.Object, options ...client.SubResourceUpdateOption) error {
					if _, ok := object.(*epco.Database); ok && subresource == connectionAPIStatusSubresource &&
						factory.state(cluster.Spec.Connection.Host, db.Name).Exists {
						return failure
					}
					return c.SubResource(subresource).Update(ctx, object, options...)
				},
			})
			r := extensionAPIReconciler(decorated, factory)
			_, err := r.Reconcile(ctx, connectionAPIRequestFor(db))
			Expect(errors.Is(err, failure)).To(BeTrue())
			Expect(factory.state(cluster.Spec.Connection.Host, db.Name).Version).To(Equal("1.8"))
			Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(db), db)).To(Succeed())
			Expect(db.Status.AppliedGeneration).To(BeZero())
			db.Spec.Extensions[0].Version = "latest-version-label"
			Expect(environment.Client.Update(ctx, db)).To(Succeed())
			restarted := extensionAPIReconciler(environment.Client, factory)
			_, err = restarted.Reconcile(ctx, connectionAPIRequestFor(db))
			Expect(err).NotTo(HaveOccurred())
			latest := expectDatabaseStatus(ctx, db, metav1.ConditionTrue, "Ready")
			Expect(latest.Status.HasManagedSQL).To(BeTrue())
			Expect(latest.Status.AppliedGeneration).To(Equal(latest.Generation))
			Expect(factory.state(cluster.Spec.Connection.Host, db.Name).Version).To(Equal("latest-version-label"),
				"restart reconciles the latest desired snapshot instead of replaying old commands")
		}, NodeTimeout(30*time.Second))

	It("observes explicit extensions without outputs or management and preserves former-manager history",
		func(ctx SpecContext) {
			factory := &extensionAPIFactory{}
			cluster := createRoleDescriptor(ctx, nil)
			r := extensionAPIReconciler(environment.Client, factory)
			manager := extensionAPIDatabase(cluster, "extension-observation")
			Expect(scope.Create(ctx, manager)).To(Succeed())
			_, err := r.Reconcile(ctx, connectionAPIRequestFor(manager))
			Expect(err).NotTo(HaveOccurred())
			manager = expectDatabaseStatus(ctx, manager, metav1.ConditionTrue, "Ready")
			appliedGeneration := manager.Status.AppliedGeneration
			manager.Spec.ManagementPolicy = epco.ManagementObserve
			manager.Spec.Extensions[0].Version = "mismatched-version"
			Expect(environment.Client.Update(ctx, manager)).To(Succeed())
			_, err = r.Reconcile(ctx, connectionAPIRequestFor(manager))
			Expect(err).NotTo(HaveOccurred())
			manager = expectDatabaseStatus(ctx, manager, metav1.ConditionFalse, postgres.DependenciesNotReady)
			Expect(manager.Status.Applied).To(BeNil())
			Expect(manager.Status.AppliedGeneration).To(Equal(appliedGeneration))
			Expect(manager.Status.HasManagedSQL).To(BeTrue())
			observer := extensionAPIDatabase(cluster, "pure-extension-observer")
			observer.Spec.Name = epco.SQLName(manager.Name)
			observer.Spec.ManagementPolicy = epco.ManagementObserve
			observer.Spec.Extensions[0].Version = "1.8"
			observer.Spec.ConnectionSecrets = []epco.ConnectionSecret{{Name: "ignored-extension-observer-output",
				RoleRef: epco.LocalReference{Name: "missing-observer-role"}}}
			Expect(scope.Create(ctx, observer)).To(Succeed())
			_, err = r.Reconcile(ctx, connectionAPIRequestFor(observer))
			Expect(err).NotTo(HaveOccurred())
			observer = expectDatabaseStatus(ctx, observer, metav1.ConditionTrue, "Observed")
			Expect(observer.Status.HasManagedSQL).To(BeFalse())
			Expect(observer.Status.AppliedGeneration).To(BeZero())
			Expect(observer.Status.Applied).To(BeNil())
			Expect(observer.Finalizers).To(BeEmpty())
			Expect(apierrors.IsNotFound(environment.Client.Get(ctx, client.ObjectKey{Namespace: observer.Namespace,
				Name: observer.Spec.ConnectionSecrets[0].Name}, &corev1.Secret{}))).To(BeTrue())
			Expect(factory.state(cluster.Spec.Connection.Host, manager.Name).Version).To(Equal("1.8"))
			condition := meta.FindStatusCondition(observer.Status.Conditions, "Ready")
			Expect(condition.ObservedGeneration).To(Equal(observer.Generation))
		}, NodeTimeout(30*time.Second))
})
