package controller_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/controller"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	controllerTestNamespace = "test"
	testClusterName         = "cluster"
	deletionCase            = "delete"
)

type checkFunc func(context.Context, *postgres.Config) error

func (f checkFunc) Check(ctx context.Context, c *postgres.Config) error { return f(ctx, c) }
func testCluster() *epco.ExternalPostgres {
	return &epco.ExternalPostgres{Name: testClusterName, Namespace: controllerTestNamespace, Generation: 2, UID: types.UID("original"), Spec: epco.ExternalPostgresSpec{
		Connection: epco.PostgresConnection{Host: "database"}, CredentialsSecret: epco.LocalReference{Name: "auth"}, TLS: &epco.PostgresTLS{Mode: dbTestTLSDisable}, ReconcileInterval: "2m"}}
}
func authSecret() *corev1.Secret {
	return &corev1.Secret{Name: "auth", Namespace: controllerTestNamespace, Type: corev1.SecretTypeBasicAuth, Data: map[string][]byte{"username": []byte("admin"), "password": []byte("sensitive-password")}}
}
func reconciler(t *testing.T, c *epco.ExternalPostgres, secrets ...client.Object) (*controller.ExternalPostgresReconciler, client.Client) {
	t.Helper()
	s := runtime.NewScheme()
	if err := epco.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	objects := append([]client.Object{c}, secrets...)
	cl := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&epco.ExternalPostgres{}).WithObjects(objects...).Build()
	b, err := postgres.NewConfigBuilder()
	if err != nil {
		t.Fatal(err)
	}
	return &controller.ExternalPostgresReconciler{Client: cl, APIReader: cl, Builder: b, Checker: checkFunc(func(context.Context, *postgres.Config) error { return nil }), Now: func() time.Time { return time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC) }}, cl
}
func read(t *testing.T, cl client.Client) *epco.ExternalPostgres {
	t.Helper()
	c := &epco.ExternalPostgres{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: controllerTestNamespace, Name: testClusterName}, c); err != nil {
		t.Fatal(err)
	}
	return c
}
func run(t *testing.T, r *controller.ExternalPostgresReconciler) (ctrl.Result, error) {
	t.Helper()
	return r.Reconcile(context.Background(), ctrl.Request{Namespace: controllerTestNamespace, Name: testClusterName})
}
func assertStatus(t *testing.T, c *epco.ExternalPostgres, applied bool, reason string) {
	t.Helper()
	if c.Status.Applied == nil || *c.Status.Applied != applied {
		t.Errorf("applied=%v want=%v", c.Status.Applied, applied)
	}
	ready := meta.FindStatusCondition(c.Status.Conditions, "Ready")
	if ready == nil {
		t.Fatal("missing Ready condition")
	}
	expected := metav1.ConditionFalse
	if applied {
		expected = metav1.ConditionTrue
	}
	if ready.Status != expected || ready.Reason != reason || ready.ObservedGeneration != c.Generation {
		t.Errorf("Ready=%+v want status=%s reason=%s generation=%d", ready, expected, reason, c.Generation)
	}
	if c.Status.ObservedGeneration != c.Generation || c.Status.EffectiveName != "" {
		t.Error("observed generation/effectiveName contract violated")
	}
	if strings.Contains(c.Status.Message, "sensitive-password") || strings.Contains(ready.Message, "sensitive-password") {
		t.Error("status exposed credentials")
	}
}
func TestSuccessfulClusterStatusAndPeriodicRetry(t *testing.T) {
	c := testCluster()
	c.Spec.Access = &epco.ClusterAccess{}
	r, cl := reconciler(t, c, authSecret())
	calls := 0
	r.Checker = checkFunc(func(_ context.Context, config *postgres.Config) error {
		calls++
		if config.Connection.Password != "sensitive-password" {
			t.Error("checker did not receive secret")
		}
		return nil
	})
	result, err := run(t, r)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("checker calls=%d want=1", calls)
	}
	if result.RequeueAfter != 2*time.Minute {
		t.Errorf("retry=%s want=2m", result.RequeueAfter)
	}
	got := read(t, cl)
	assertStatus(t, got, true, "Ready")
	if got.Status.AppliedGeneration != 2 || got.Status.LastSuccessfulReconcileTime == nil || !got.Status.LastSuccessfulReconcileTime.Time.Equal(r.Now()) {
		t.Error("success generation/time missing")
	}
}
func TestExpectedClusterFailuresPreservePreviousSuccess(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		err          error
		missing      bool
	}{
		{"missing secret", "SecretNotFound", nil, true},
		{"connection", postgres.ConnectionUnavailable, &postgres.Failure{Reason: postgres.ConnectionUnavailable, Message: "sensitive-password server detail"}, false},
		{"readonly", "NotWritable", &postgres.Failure{Reason: "NotWritable", Message: "sensitive-password server detail"}, false},
		{"driver error", postgres.ConnectionUnavailable, errors.New("sensitive-password unprocessed driver error"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testCluster()
			last := metav1.NewTime(time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC))
			c.Status.AppliedGeneration = 1
			c.Status.LastSuccessfulReconcileTime = &last
			objects := []client.Object{}
			if !tc.missing {
				objects = append(objects, authSecret())
			}
			r, cl := reconciler(t, c, objects...)
			r.Checker = checkFunc(func(context.Context, *postgres.Config) error { return tc.err })
			result, err := run(t, r)
			if err != nil {
				t.Fatal(err)
			}
			if result.RequeueAfter != 30*time.Second {
				t.Errorf("retry=%s want=30s", result.RequeueAfter)
			}
			got := read(t, cl)
			assertStatus(t, got, false, tc.reason)
			if got.Status.AppliedGeneration != 1 || got.Status.LastSuccessfulReconcileTime == nil || !got.Status.LastSuccessfulReconcileTime.Equal(&last) {
				t.Error("failure erased prior success")
			}
		})
	}
}
func TestExpectedRetryUsesShorterInterval(t *testing.T) {
	c := testCluster()
	c.Spec.ReconcileInterval = "5s"
	r, _ := reconciler(t, c)
	result, err := run(t, r)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Errorf("retry=%s want=5s", result.RequeueAfter)
	}
}
func TestStaleInputsNeverPublishStatus(t *testing.T) {
	for _, change := range []string{"secret", "spec", "uid", deletionCase} {
		t.Run(change, func(t *testing.T) {
			c := testCluster()
			r, cl := reconciler(t, c, authSecret())
			calls := 0
			r.Checker = checkFunc(func(ctx context.Context, _ *postgres.Config) error {
				calls++
				switch change {
				case "secret":
					s := &corev1.Secret{}
					if err := cl.Get(ctx, client.ObjectKeyFromObject(authSecret()), s); err != nil {
						t.Fatal(err)
					}
					s.Data["password"] = []byte("rotated")
					if err := cl.Update(ctx, s); err != nil {
						t.Fatal(err)
					}
				case "spec":
					latest := read(t, cl)
					latest.Spec.Connection.Host = "changed"
					latest.Generation++
					if err := cl.Update(ctx, latest); err != nil {
						t.Fatal(err)
					}
				case "uid":
					latest := read(t, cl)
					latest.UID = types.UID("replacement")
					if err := cl.Update(ctx, latest); err != nil {
						t.Fatal(err)
					}
				case deletionCase:
					if err := cl.Delete(ctx, c); err != nil {
						t.Fatal(err)
					}
				}
				return nil
			})
			result, err := run(t, r)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("checker calls=%d want=1", calls)
			}
			if change == deletionCase {
				return
			}
			got := read(t, cl)
			if got.Status.Applied != nil || len(got.Status.Conditions) != 0 {
				t.Error("published result for stale input")
			}
			if result.RequeueAfter <= 0 {
				t.Error("stale input must schedule fresh check")
			}
		})
	}
}
func TestDeletingClusterSkipsProbe(t *testing.T) {
	c := testCluster()
	now := metav1.Now()
	c.DeletionTimestamp = &now
	c.Finalizers = []string{"test-hold"}
	r, _ := reconciler(t, c, authSecret())
	r.Checker = checkFunc(func(context.Context, *postgres.Config) error { t.Error("deleting cluster probed"); return nil })
	_, err := run(t, r)
	if err != nil {
		t.Fatal(err)
	}
}
func TestKubernetesFailureReturnsErrorForStandardBackoff(t *testing.T) {
	r, cl := reconciler(t, testCluster(), authSecret())
	failure := apierrors.NewServiceUnavailable("API unavailable")
	r.APIReader = interceptor.NewClient(cl.(client.WithWatch), interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return failure
	}})
	_, err := run(t, r)
	if !errors.Is(err, failure) {
		t.Fatalf("API error=%v want original for standard backoff", err)
	}
}
func TestOptimisticStatusConflictDiscardsResult(t *testing.T) {
	r, cl := reconciler(t, testCluster(), authSecret())
	patches := 0
	r.Client = interceptor.NewClient(cl.(client.WithWatch), interceptor.Funcs{SubResourcePatch: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
		patches++
		return apierrors.NewConflict(schema.GroupResource{Group: epco.SchemeGroupVersion.Group, Resource: "externalpostgres"}, testClusterName, errors.New("concurrent writer"))
	}})
	result, err := run(t, r)
	if err != nil {
		t.Fatal(err)
	}
	if patches != 1 {
		t.Errorf("status patches=%d want=1", patches)
	}
	if result.RequeueAfter <= 0 {
		t.Error("conflict must schedule fresh input check")
	}
	if read(t, cl).Status.Applied != nil {
		t.Error("conflicting result must be discarded")
	}
}
