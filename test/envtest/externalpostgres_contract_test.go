//go:build envtest

package envtest_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
)

func externalPostgresResource(kind string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.epco.io/v1alpha1", "kind": kind,
		"metadata": map[string]any{nameField: "descriptor", namespaceField: namespace.Name},
		specField: map[string]any{
			connectionField:     map[string]any{hostField: descriptorTestHost},
			"credentialsSecret": map[string]any{nameField: "credentials"},
		},
	}}
}

var _ = Describe("ExternalPostgres public API contract", func() {
	It("discovers the uncountable resource name and epg shorthand without the removed kind", func() {
		discoveryClient, err := discovery.NewDiscoveryClientForConfig(environment.Config)
		Expect(err).NotTo(HaveOccurred())
		resources, err := discoveryClient.ServerResourcesForGroupVersion("postgresql.epco.io/v1alpha1")
		Expect(err).NotTo(HaveOccurred())
		Expect(resources.APIResources).To(ContainElement(And(
			HaveField("Name", "externalpostgres"), HaveField("SingularName", "externalpostgres"),
			HaveField("Kind", "ExternalPostgres"), HaveField("Namespaced", true),
			HaveField("ShortNames", ConsistOf("epg")),
		)))
		for _, resource := range resources.APIResources {
			Expect(resource.Name).NotTo(BeElementOf("postgresclusters", "postgresclusters/status"))
			Expect(resource.Kind).NotTo(Equal("PostgresCluster"))
		}
	})

	It("admits and defaults the renamed descriptor", func(ctx SpecContext) {
		object := externalPostgresResource("ExternalPostgres")
		Expect(scope.Create(ctx, object)).To(Succeed())
		Expect(object.GetKind()).To(Equal("ExternalPostgres"))
		Expect(field(object, specField, connectionField, "port")).To(Equal(int64(5432)))
		Expect(field(object, specField, connectionField, "database")).To(Equal("postgres"))
		Expect(field(object, specField, "connectTimeout")).To(Equal("10s"))
	})

	It("enforces descriptor admission rules under the renamed kind", func(ctx SpecContext) {
		object := externalPostgresResource("ExternalPostgres")
		setField(object, "0s", specField, "connectTimeout")
		err := scope.Create(ctx, object)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%v", err)
		Expect(err).To(MatchError(ContainSubstring("duration must be positive")))
	})

	It("does not serve the removed descriptor endpoint", func(ctx SpecContext) {
		dynamicClient, err := dynamic.NewForConfig(environment.Config)
		Expect(err).NotTo(HaveOccurred())
		legacy := schema.GroupVersionResource{Group: "postgresql.epco.io", Version: "v1alpha1", Resource: "postgresclusters"}
		_, err = dynamicClient.Resource(legacy).Namespace(namespace.Name).List(ctx, metav1.ListOptions{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "removed API endpoint must return NotFound: %v", err)
	})
})
