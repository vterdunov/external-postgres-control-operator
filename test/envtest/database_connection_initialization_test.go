//go:build envtest

package envtest_test

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type connectionAPIUnavailableRoleFactory struct{}

func (connectionAPIUnavailableRoleFactory) Open(context.Context, *postgres.Config) (postgres.RoleSession, error) {
	return nil, &postgres.Failure{Reason: postgres.DependenciesNotReady, Message: "isolated initializer SQL outage"}
}

func startConnectionAPIUnavailableManager(ctx context.Context) {
	startConnectionAPIManager(ctx, connectionAPIOfflineFactory{})
}

var _ = Describe("R4 generated connection source initialization events", func() {
	It("publishes initial output from owner initialization alone while SQL and Ready remain unavailable",
		func(ctx SpecContext) {
			cluster := createRoleDescriptor(ctx, nil)
			role := &epco.DatabaseRole{Namespace: namespace.Name, Name: connectionAPIRoleName,
				Spec: epco.DatabaseRoleSpec{Cluster: epco.ClusterReference{Name: cluster.Name},
					Name: connectionAPISQLRole, Login: true,
					PasswordGeneration: &epco.PasswordGeneration{SecretName: "status-loss-generated-source"}}}
			Expect(scope.Create(ctx, role)).To(Succeed())
			reconciler := connectionAPIRoleReconciler()
			reconciler.Roles = connectionAPIUnavailableRoleFactory{}

			// Public reconciliation records current-generation Ready=False before
			// generation starts, without synthetic status or SQL acknowledgement.
			admin := &corev1.Secret{}
			adminKey := client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Spec.CredentialsSecret.Name}
			Expect(environment.Client.Get(ctx, adminKey, admin)).To(Succeed())
			Expect(environment.Client.Delete(ctx, admin)).To(Succeed())
			_, err := reconciler.Reconcile(ctx, connectionAPIRequestFor(role))
			Expect(err).NotTo(HaveOccurred())
			role = expectRoleStatus(ctx, role, metav1.ConditionFalse, "SecretNotFound")
			Expect(scope.Create(ctx, &corev1.Secret{Namespace: admin.Namespace, Name: admin.Name,
				Type: admin.Type, Data: maps.Clone(admin.Data)})).To(Succeed())

			failure := apierrors.NewServiceUnavailable("isolated generated-source initializer status loss")
			reconciler.Client = interceptor.NewClient(connectionAPIWatchClient(), interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string,
					object client.Object, options ...client.SubResourceUpdateOption) error {
					if current, ok := object.(*epco.DatabaseRole); ok && subresource == connectionAPIStatusSubresource &&
						current.Status.GeneratedPasswordInitialized {
						return failure
					}
					return c.SubResource(subresource).Update(ctx, object, options...)
				},
			})
			_, err = reconciler.Reconcile(ctx, connectionAPIRequestFor(role))
			Expect(errors.Is(err, failure)).To(BeTrue(), "test must reach owner initialization status loss")
			source := &corev1.Secret{}
			sourceKey := client.ObjectKey{Namespace: role.Namespace, Name: role.Spec.PasswordGeneration.SecretName}
			Expect(environment.Client.Get(ctx, sourceKey, source)).To(Succeed())
			Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(role), role)).To(Succeed())
			Expect(role.Status.GeneratedPasswordInitialized).To(BeFalse())
			before := role.DeepCopy()
			beforeReady := meta.FindStatusCondition(before.Status.Conditions, "Ready")
			Expect(beforeReady).NotTo(BeNil())

			startConnectionAPIUnavailableManager(ctx)
			db := connectionAPIDatabase(cluster, "initializer-event-output")
			Expect(scope.Create(ctx, db)).To(Succeed())
			expectDatabaseStatus(ctx, db, metav1.ConditionFalse, "DependenciesNotReady")
			Consistently(func() bool {
				return apierrors.IsNotFound(environment.Client.Get(ctx, connectionAPIOutputKey(db), &corev1.Secret{}))
			}).WithContext(ctx).WithTimeout(time.Second).Should(BeTrue(),
				"generated source publication must wait for its owner initialization checkpoint")

			reconciler.Client = environment.Client
			_, err = reconciler.Reconcile(ctx, connectionAPIRequestFor(role))
			Expect(err).NotTo(HaveOccurred())
			role = expectRoleStatus(ctx, role, metav1.ConditionFalse, "DependenciesNotReady")
			Expect(role.Status.GeneratedPasswordInitialized).To(BeTrue())
			Expect(role.Generation).To(Equal(before.Generation))
			Expect(role.Status.AppliedGeneration).To(Equal(before.Status.AppliedGeneration))
			Expect(role.Status.HasManagedSQL).To(Equal(before.Status.HasManagedSQL))
			Expect(role.Status.SecretResourceVersion).To(Equal(before.Status.SecretResourceVersion))
			afterReady := meta.FindStatusCondition(role.Status.Conditions, "Ready")
			Expect(afterReady.Status).To(Equal(beforeReady.Status))
			Expect(afterReady.ObservedGeneration).To(Equal(beforeReady.ObservedGeneration))
			afterSource := &corev1.Secret{}
			Expect(environment.Client.Get(ctx, sourceKey, afterSource)).To(Succeed())
			Expect(afterSource.ResourceVersion).To(Equal(source.ResourceVersion),
				"owner initialization must not produce a masking source Secret update")
			connectionAPIExpectPassword(ctx, db, source.Data[passwordKey])
			Expect(bytes.Equal(connectionAPIOutput(ctx, db).Data[passwordKey], source.Data[passwordKey])).To(BeTrue())
			expectDatabaseStatus(ctx, db, metav1.ConditionFalse, "DependenciesNotReady")
		}, NodeTimeout(35*time.Second))
})
