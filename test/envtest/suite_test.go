//go:build envtest

package envtest_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	harness "github.com/vterdunov/external-postgres-control-operator/test/envtest"
)

var environment *harness.Environment
var scope *harness.Scope
var namespace *corev1.Namespace

func TestEnvtest(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "EPCO API and manager suite")
}

var _ = BeforeSuite(func(ctx SpecContext) {
	var err error
	environment, err = harness.Start(ctx, harness.Options{
		CRDDirectory:          filepath.Join("..", "..", "config", "crd", "bases"),
		BinaryAssetsDirectory: os.Getenv("KUBEBUILDER_ASSETS"),
	})
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		Expect(environment.Stop(cleanupCtx)).To(Succeed())
	})
}, NodeTimeout(90*time.Second))

var _ = BeforeEach(func(ctx SpecContext) {
	var err error
	scope, namespace, err = environment.NewScope(ctx, "epco-api-")
	Expect(err).NotTo(HaveOccurred())
}, NodeTimeout(15*time.Second))

var _ = AfterEach(func(ctx SpecContext) {
	var diagnosticErr error
	if CurrentSpecReport().Failed() {
		diagnosticErr = scope.Diagnostics(ctx, GinkgoWriter)
	}
	Expect(errors.Join(diagnosticErr, scope.Cleanup(ctx))).To(Succeed())
}, NodeTimeout(30*time.Second))
