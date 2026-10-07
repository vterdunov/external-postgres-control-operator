//go:build e2e

package e2e

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func installedClusterStatus(ctx context.Context,
	key client.ObjectKey,
	status metav1.ConditionStatus,
	reason string) epco.PostgresClusterStatus {
	var observed epco.PostgresClusterStatus
	Eventually(func(g Gomega) {
		latest := &epco.PostgresCluster{}
		g.Expect(apiClient.Get(ctx, key, latest)).To(Succeed())
		condition := meta.FindStatusCondition(latest.Status.Conditions, "Ready")
		g.Expect(condition).NotTo(BeNil())
		g.Expect(condition.Status).To(Equal(status))
		g.Expect(condition.Reason).To(Equal(reason))
		g.Expect(latest.Status.ObservedGeneration).To(Equal(latest.Generation))
		g.Expect(latest.Status.Applied).NotTo(BeNil())
		g.Expect(*latest.Status.Applied).To(Equal(status == metav1.ConditionTrue))
		g.Expect(latest.Status.EffectiveName).To(BeEmpty())
		observed = latest.Status
	}).WithContext(ctx).WithTimeout(40 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
	return observed
}

var _ = Describe("Installed PostgresCluster controller", func() {
	It("authenticates real fixture credentials and recovers after Secret breakage and replacement", func(ctx SpecContext) {
		source, err := kubeClient.CoreV1().Secrets("epco-postgres").Get(ctx, "postgres", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		password, exists := source.Data[corev1.BasicAuthPasswordKey]
		Expect(exists).To(BeTrue())
		Expect(password).NotTo(BeEmpty())
		secret := &corev1.Secret{Name: "connection-admin",
			Namespace: operatorNamespace,
			Type:      corev1.SecretTypeBasicAuth,
			Data: map[string][]byte{corev1.BasicAuthUsernameKey: []byte("postgres"),
				corev1.BasicAuthPasswordKey: password}}
		Expect(apiClient.Create(ctx, secret)).To(Succeed())
		DeferCleanup(func() { deleteObject(secret) })
		cluster := &epco.PostgresCluster{Name: "connection-cluster",
			Namespace: operatorNamespace,
			Spec: epco.PostgresClusterSpec{Connection: epco.PostgresConnection{Host: "postgres.epco-postgres.svc.cluster.local"},
				CredentialsSecret: epco.LocalReference{Name: secret.Name},
				TLS:               &epco.PostgresTLS{Mode: "disable"},
				ReconcileInterval: "5s",
				ConnectTimeout:    "2s",
				StatementTimeout:  "2s"}}
		Expect(apiClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { deleteObject(cluster) })
		key := client.ObjectKeyFromObject(cluster)
		success := installedClusterStatus(ctx, key, metav1.ConditionTrue, "Ready")
		Expect(success.AppliedGeneration).To(Equal(cluster.Generation))
		Expect(success.LastSuccessfulReconcileTime).NotTo(BeNil())
		secret.Data[corev1.BasicAuthPasswordKey] = []byte("incorrect-fixture-password")
		Expect(apiClient.Update(ctx, secret)).To(Succeed())
		failure := installedClusterStatus(ctx, key, metav1.ConditionFalse, "ConnectionUnavailable")
		Expect(failure.AppliedGeneration).To(Equal(success.AppliedGeneration))
		Expect(failure.LastSuccessfulReconcileTime).NotTo(BeNil())
		secret.Data[corev1.BasicAuthPasswordKey] = password
		Expect(apiClient.Update(ctx, secret)).To(Succeed())
		installedClusterStatus(ctx, key, metav1.ConditionTrue, "Ready")
		Expect(apiClient.Delete(ctx, secret)).To(Succeed())
		installedClusterStatus(ctx, key, metav1.ConditionFalse, "SecretNotFound")
		replacement := &corev1.Secret{Name: secret.Name,
			Namespace: secret.Namespace,
			Type:      corev1.SecretTypeBasicAuth,
			Data: map[string][]byte{corev1.BasicAuthUsernameKey: []byte("postgres"),
				corev1.BasicAuthPasswordKey: password}}
		Expect(apiClient.Create(ctx, replacement)).To(Succeed())
		installedClusterStatus(ctx, key, metav1.ConditionTrue, "Ready")
	}, NodeTimeout(3*time.Minute))
})
