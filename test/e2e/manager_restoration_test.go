//go:build e2e

package e2e

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const installedManagerLeaseName = "ccd893cc.epco.io"

func installedManagerRestored(ctx context.Context, replicas int32, restoredAt time.Time) {
	Eventually(func(g Gomega) {
		pods, err := kubeClient.CoreV1().Pods(operatorNamespace).List(ctx,
			metav1.ListOptions{LabelSelector: installedManagerSelector})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(pods.Items).To(HaveLen(int(replicas)))
		lease, err := kubeClient.CoordinationV1().Leases(operatorNamespace).Get(
			ctx, installedManagerLeaseName, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(lease.Spec.HolderIdentity).NotTo(BeNil())
		g.Expect(lease.Spec.RenewTime).NotTo(BeNil())
		g.Expect(lease.Spec.RenewTime.Time).To(BeTemporally(">=", restoredAt))
		holderPresent := false
		for _, pod := range pods.Items {
			g.Expect(pod.DeletionTimestamp).To(BeNil())
			g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning))
			g.Expect(pod.Status.ContainerStatuses).NotTo(BeEmpty())
			for _, status := range pod.Status.ContainerStatuses {
				g.Expect(status.Ready).To(BeTrue())
			}
			holderPresent = holderPresent || strings.HasPrefix(*lease.Spec.HolderIdentity, pod.Name+"_")
		}
		g.Expect(holderPresent).To(BeTrue(), "The restored manager must hold the renewed Lease")
	}).WithContext(ctx).WithTimeout(50 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
}
