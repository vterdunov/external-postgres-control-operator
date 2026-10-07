//go:build e2e

package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	apiClient                   client.Client
	kubeClient                  kubernetes.Interface
	kubeconfigPath, kubeContext string
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Installed EPCO deployment")
}

var _ = BeforeSuite(func() {
	kubeconfigPath = os.Getenv("KUBECONFIG")
	kubeContext = os.Getenv("EPCO_E2E_CONTEXT")
	Expect(kubeconfigPath).NotTo(BeEmpty(), "Use bash test/e2e/run.sh")
	Expect(kubeContext).To(HavePrefix("kind-epco-e2e-"), "Use the run-owned Kind context")
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfigPath},
		&clientcmd.ConfigOverrides{CurrentContext: kubeContext},
	).ClientConfig()
	Expect(err).NotTo(HaveOccurred())
	config.Timeout = 20 * time.Second
	scheme := runtime.NewScheme()
	Expect(corev1.AddToScheme(scheme)).To(Succeed())
	Expect(rbacv1.AddToScheme(scheme)).To(Succeed())
	Expect(epco.AddToScheme(scheme)).To(Succeed())
	apiClient, err = client.New(config, client.Options{Scheme: scheme})
	Expect(err).NotTo(HaveOccurred())
	kubeClient, err = kubernetes.NewForConfig(config)
	Expect(err).NotTo(HaveOccurred())
})

func boundedContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}
