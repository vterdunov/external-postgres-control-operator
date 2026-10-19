package v1alpha1_test

import (
	"reflect"
	"testing"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestExternalPostgresSchemeContract(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := epco.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"ExternalPostgres", "ExternalPostgresList"} {
		t.Run(kind, func(t *testing.T) {
			gvk := epco.SchemeGroupVersion.WithKind(kind)
			object, err := scheme.New(gvk)
			if err != nil {
				t.Fatalf("public API kind %s must be registered: %v", kind, err)
			}
			if got := reflect.TypeOf(object).Elem().Name(); got != kind {
				t.Fatalf("public API kind %s must use the renamed Go type, got %s", kind, got)
			}
			if _, _, err := scheme.ObjectKinds(object.DeepCopyObject()); err != nil {
				t.Fatalf("deep copy of %s must retain scheme registration: %v", kind, err)
			}
		})
	}
	for _, kind := range []string{"PostgresCluster", "PostgresClusterList"} {
		t.Run("removed "+kind, func(t *testing.T) {
			if scheme.Recognizes(epco.SchemeGroupVersion.WithKind(kind)) {
				t.Fatalf("removed public API kind %s must not be registered", kind)
			}
		})
	}
}
