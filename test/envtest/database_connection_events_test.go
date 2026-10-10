//go:build envtest

package envtest_test

import (
	"bytes"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/test/integration"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func connectionAPISettled(ctx SpecContext, factory *connectionAPIFactory, db *epco.Database) {
	previous := factory.count(string(db.Spec.Name))
	changed := time.Now()
	Eventually(func() bool {
		current := factory.count(string(db.Spec.Name))
		if current != previous {
			previous, changed = current, time.Now()
		}
		return time.Since(changed) >= 400*time.Millisecond
	}).WithContext(ctx).WithTimeout(5*time.Second).Should(BeTrue(),
		"initial reconciliation must settle before testing an independent dependency event")
}

func connectionAPIPublicCA() []byte {
	fixture, err := integration.NewTLSFixture(GinkgoT().TempDir())
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	public, err := os.ReadFile(fixture.CA)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return public
}

var _ = Describe("R4 independent connection dependency events", func() {
	It("recreates an acknowledged output from its deletion event alone", func(ctx SpecContext) {
		cluster, role, source := connectionAPIFixture(ctx)
		connectionAPIApplyRole(ctx, connectionAPIRoleReconciler(), role)
		factory := &connectionAPIFactory{}
		startConnectionAPIManager(ctx, factory)
		db := connectionAPIDatabase(cluster, "deleted-output")
		Expect(scope.Create(ctx, db)).To(Succeed())
		connectionAPIExpectPassword(ctx, db, source.Data[passwordKey])
		expectDatabaseStatus(ctx, db, metav1.ConditionTrue, "Ready")
		connectionAPISettled(ctx, factory, db)
		original := connectionAPIOutput(ctx, db).DeepCopy()
		Expect(environment.Client.Delete(ctx, original)).To(Succeed())
		connectionAPIExpectPassword(ctx, db, source.Data[passwordKey])
		after := connectionAPIOutput(ctx, db)
		Expect(after.UID).NotTo(Equal(original.UID),
			"output deletion must enqueue current root state without a role/source change")
	}, NodeTimeout(30*time.Second))

	It("refreshes a configured public CA on its own Secret update under TLS disable", func(ctx SpecContext) {
		cluster, role, source := connectionAPIFixture(ctx)
		ca := &corev1.Secret{Namespace: namespace.Name, Name: "disabled-tls-public-ca",
			Data: map[string][]byte{databaseTestCAKey: connectionAPIPublicCA()}}
		Expect(scope.Create(ctx, ca)).To(Succeed())
		cluster.Spec.TLS.CASecret = &epco.SecretKeyReference{Name: ca.Name, Key: databaseTestCAKey}
		Expect(environment.Client.Update(ctx, cluster)).To(Succeed())
		connectionAPIApplyRole(ctx, connectionAPIRoleReconciler(), role)
		factory := &connectionAPIFactory{}
		startConnectionAPIManager(ctx, factory)
		db := connectionAPIDatabase(cluster, "public-ca-update")
		Expect(scope.Create(ctx, db)).To(Succeed())
		connectionAPIExpectPassword(ctx, db, source.Data[passwordKey])
		expectDatabaseStatus(ctx, db, metav1.ConditionTrue, "Ready")
		Expect(bytes.Equal(connectionAPIOutput(ctx, db).Data["ca.crt"], ca.Data[databaseTestCAKey])).To(BeTrue())
		connectionAPISettled(ctx, factory, db)
		beforeUID := connectionAPIOutput(ctx, db).UID
		ca.Data[databaseTestCAKey] = connectionAPIPublicCA()
		Expect(environment.Client.Update(ctx, ca)).To(Succeed())
		Eventually(func() bool {
			output := &corev1.Secret{}
			err := environment.Client.Get(ctx, connectionAPIOutputKey(db), output)
			return err == nil && output.UID == beforeUID && bytes.Equal(output.Data["ca.crt"], ca.Data[databaseTestCAKey])
		}).WithContext(ctx).WithTimeout(10*time.Second).Should(BeTrue(),
			"configured application CA updates must be watched independently of administrative TLS use")
	}, NodeTimeout(30*time.Second))

	It("publishes when an unrelated role releases its password-source reservation", func(ctx SpecContext) {
		cluster, role, source := connectionAPIFixture(ctx)
		connectionAPIApplyRole(ctx, connectionAPIRoleReconciler(), role)
		db := connectionAPIDatabase(cluster, "reservation-release")
		reservation := &epco.DatabaseRole{Namespace: namespace.Name, Name: "unrelated-source-reservation",
			Spec: epco.DatabaseRoleSpec{Cluster: epco.ClusterReference{Name: cluster.Name},
				Name:               "unrelated_reservation_login",
				PasswordGeneration: &epco.PasswordGeneration{SecretName: db.Spec.ConnectionSecrets[0].Name}}}
		Expect(scope.Create(ctx, reservation)).To(Succeed())
		factory := &connectionAPIFactory{}
		startConnectionAPIManager(ctx, factory)
		Expect(scope.Create(ctx, db)).To(Succeed())
		expectDatabaseStatus(ctx, db, metav1.ConditionFalse, "ConnectionSecretConflict")
		Expect(apierrors.IsNotFound(environment.Client.Get(
			ctx, connectionAPIOutputKey(db), &corev1.Secret{}))).To(BeTrue())
		Expect(environment.Client.Delete(ctx, reservation)).To(Succeed())
		connectionAPIExpectPassword(ctx, db, source.Data[passwordKey])
		expectDatabaseStatus(ctx, db, metav1.ConditionTrue, "Ready")
	}, NodeTimeout(30*time.Second))

	It("initializes a generated source when a Database releases its output reservation", func(ctx SpecContext) {
		cluster := createRoleDescriptor(ctx, nil)
		role := &epco.DatabaseRole{Namespace: namespace.Name, Name: "reserved-generated-role",
			Spec: epco.DatabaseRoleSpec{Cluster: epco.ClusterReference{Name: cluster.Name},
				Name: "reserved_generated_login", Login: true,
				PasswordGeneration: &epco.PasswordGeneration{SecretName: "reserved-generated-source"}}}
		reservation := connectionAPIDatabase(cluster, "output-reservation")
		reservation.Spec.ConnectionSecrets[0].Name = role.Spec.PasswordGeneration.SecretName
		Expect(scope.Create(ctx, reservation)).To(Succeed())
		Expect(scope.Create(ctx, role)).To(Succeed())
		startRoleManager(ctx, &watchedRoleFactory{})
		expectRoleStatus(ctx, role, metav1.ConditionFalse, "ConnectionSecretConflict")
		key := client.ObjectKey{Namespace: role.Namespace, Name: role.Spec.PasswordGeneration.SecretName}
		Expect(apierrors.IsNotFound(environment.Client.Get(ctx, key, &corev1.Secret{}))).To(BeTrue())
		Expect(environment.Client.Delete(ctx, reservation)).To(Succeed())
		Eventually(func() bool {
			source := &corev1.Secret{}
			err := environment.Client.Get(ctx, key, source)
			return err == nil && source.Type == corev1.SecretTypeBasicAuth &&
				string(source.Data[connectionAPIUsernameKey]) == string(role.Spec.Name) && len(source.Data[passwordKey]) > 0
		}).WithContext(ctx).WithTimeout(10*time.Second).Should(BeTrue(),
			"Database reservation removal must enqueue the blocked password-source owner")
		expectRoleStatus(ctx, role, metav1.ConditionTrue, "Ready")
	}, NodeTimeout(30*time.Second))
})
