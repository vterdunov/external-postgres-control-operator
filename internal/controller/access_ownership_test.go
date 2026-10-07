package controller_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/controller"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	testTeamLabel           = "team"
	testReadyCondition      = "Ready"
	testAccessLabel         = "database-access"
	testOrdersNamespace     = "orders"
	testBillingNamespace    = "billing"
	testEnvironmentLabel    = "environment"
	testInfraNamespace      = "infra"
	testYesLabel            = "yes"
	testTrueLabel           = "true"
	testDescriptorName      = "main"
	testSelectorAccessLabel = "access"
	testOtherName           = "other"
	testSharedSQLName       = "shared"
	testAnotherName         = "another"
	testAdminName           = "admin"
	testGeneratedName       = "generated"
	testParentName          = "parent"
	testSearchPath          = "search_path"
	testEndpointHost        = "database.example"
	testSafeFailure         = "safe failure"
	testValidCase           = "valid"
	testProvidedName        = "provided"
)

func accessClient(t *testing.T, objects ...client.Object) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := epco.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&epco.DatabaseRole{}).WithObjects(objects...).Build()
}

func TestNamespaceSelectorAuthorization(t *testing.T) {
	selected := &metav1.LabelSelector{MatchLabels: map[string]string{testAccessLabel: "allowed"}}
	expressions := &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
		{Key: testTeamLabel, Operator: metav1.LabelSelectorOpIn, Values: []string{testOrdersNamespace, testBillingNamespace}},
		{Key: "restricted", Operator: metav1.LabelSelectorOpDoesNotExist},
		{Key: testAccessLabel, Operator: metav1.LabelSelectorOpExists},
		{Key: testEnvironmentLabel, Operator: metav1.LabelSelectorOpNotIn, Values: []string{"dev"}},
	}}
	for _, tc := range []struct {
		name      string
		namespace string
		access    *epco.ClusterAccess
		labels    map[string]string
		allowed   bool
	}{
		{name: "omitted access allows local", namespace: testInfraNamespace, allowed: true},
		{name: "omitted access denies remote", namespace: testOrdersNamespace},
		{name: "null selector denies local", namespace: testInfraNamespace, access: &epco.ClusterAccess{}},
		{name: "null selector denies remote", namespace: testOrdersNamespace, access: &epco.ClusterAccess{}},
		{name: "explicit empty selector allows local", namespace: testInfraNamespace, access: &epco.ClusterAccess{NamespaceSelector: &metav1.LabelSelector{}}, allowed: true},
		{name: "explicit empty selector allows remote", namespace: testOrdersNamespace, access: &epco.ClusterAccess{NamespaceSelector: &metav1.LabelSelector{}}, allowed: true},
		{name: "matching namespace labels", namespace: testOrdersNamespace, access: &epco.ClusterAccess{NamespaceSelector: selected}, labels: map[string]string{testAccessLabel: "allowed"}, allowed: true},
		{name: "local namespace has no exemption", namespace: testInfraNamespace, access: &epco.ClusterAccess{NamespaceSelector: selected}},
		{name: "expressions match namespace", namespace: testOrdersNamespace, access: &epco.ClusterAccess{NamespaceSelector: expressions}, labels: map[string]string{testTeamLabel: testOrdersNamespace, testAccessLabel: testYesLabel, testEnvironmentLabel: "production"}, allowed: true},
		{name: "expressions reject forbidden label", namespace: testOrdersNamespace, access: &epco.ClusterAccess{NamespaceSelector: expressions}, labels: map[string]string{testTeamLabel: testOrdersNamespace, testAccessLabel: testYesLabel, "restricted": testTrueLabel}},
		{name: "expressions require all terms", namespace: testOrdersNamespace, access: &epco.ClusterAccess{NamespaceSelector: expressions}, labels: map[string]string{testTeamLabel: testOrdersNamespace, testAccessLabel: testYesLabel, testEnvironmentLabel: "dev"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := &epco.PostgresCluster{Namespace: testInfraNamespace, Name: testDescriptorName, Spec: epco.PostgresClusterSpec{Access: tc.access}}
			cl := accessClient(t, &corev1.Namespace{Name: tc.namespace, Labels: tc.labels})
			err := controller.AuthorizeNamespace(t.Context(), cl, cluster, tc.namespace)
			if tc.allowed {
				if err != nil {
					t.Fatalf("authorized namespace rejected: %v", err)
				}
				return
			}
			failure, ok := errors.AsType[*postgres.Failure](err)
			if !ok || failure.Reason != "AccessDenied" {
				t.Fatalf("authorization error=%v want typed AccessDenied", err)
			}
		})
	}
}

func TestNamespaceAuthorizationUsesCurrentNamespaceLabels(t *testing.T) {
	namespace := &corev1.Namespace{Name: testOrdersNamespace, Labels: map[string]string{testSelectorAccessLabel: testYesLabel}}
	cl := accessClient(t, namespace)
	cluster := &epco.PostgresCluster{Namespace: testInfraNamespace, Spec: epco.PostgresClusterSpec{Access: &epco.ClusterAccess{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{testSelectorAccessLabel: testYesLabel}}}}}
	if err := controller.AuthorizeNamespace(t.Context(), cl, cluster, testOrdersNamespace); err != nil {
		t.Fatal(err)
	}
	latest := &corev1.Namespace{}
	if err := cl.Get(t.Context(), client.ObjectKey{Name: testOrdersNamespace}, latest); err != nil {
		t.Fatal(err)
	}
	latest.Labels = nil
	if err := cl.Update(t.Context(), latest); err != nil {
		t.Fatal(err)
	}
	failure, ok := errors.AsType[*postgres.Failure](controller.AuthorizeNamespace(t.Context(), cl, cluster, testOrdersNamespace))
	if !ok || failure.Reason != "AccessDenied" {
		t.Fatal("namespace label removal must revoke access on the next evaluation")
	}
	latest.Labels = map[string]string{testSelectorAccessLabel: testYesLabel}
	if err := cl.Update(t.Context(), latest); err != nil {
		t.Fatal(err)
	}
	if err := controller.AuthorizeNamespace(t.Context(), cl, cluster, testOrdersNamespace); err != nil {
		t.Fatalf("access did not resume: %v", err)
	}
}

func TestAuthorizationReturnsNamespaceReadFailure(t *testing.T) {
	failure := apierrors.NewServiceUnavailable("temporary API failure")
	cl := interceptor.NewClient(accessClient(t), interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return failure
	}})
	cluster := &epco.PostgresCluster{Namespace: testInfraNamespace, Spec: epco.PostgresClusterSpec{Access: &epco.ClusterAccess{NamespaceSelector: &metav1.LabelSelector{}}}}
	if err := controller.AuthorizeNamespace(t.Context(), cl, cluster, testOrdersNamespace); !errors.Is(err, failure) {
		t.Fatalf("API failure=%v want original error for retry", err)
	}
}

func TestCanonicalManagementKey(t *testing.T) {
	local := controller.CanonicalManagementKey(testInfraNamespace, epco.ClusterReference{Name: testDescriptorName}, controller.SQLObjectRole, "AppRole")
	explicit := controller.CanonicalManagementKey(testOrdersNamespace, epco.ClusterReference{Namespace: testInfraNamespace, Name: testDescriptorName}, controller.SQLObjectRole, "AppRole")
	want := controller.ManagementKey{Cluster: types.NamespacedName{Namespace: testInfraNamespace, Name: testDescriptorName}, ObjectType: controller.SQLObjectRole, SQLName: "AppRole"}
	if local != want || explicit != want {
		t.Fatalf("equivalent descriptor references differ: local=%+v explicit=%+v want=%+v", local, explicit, want)
	}
	for _, key := range []controller.ManagementKey{
		controller.CanonicalManagementKey(testInfraNamespace, epco.ClusterReference{Name: testDescriptorName}, controller.SQLObjectDatabase, "AppRole"),
		controller.CanonicalManagementKey(testInfraNamespace, epco.ClusterReference{Name: testDescriptorName}, controller.SQLObjectRole, "approle"),
		controller.CanonicalManagementKey(testOtherName, epco.ClusterReference{Name: testDescriptorName}, controller.SQLObjectRole, "AppRole"),
		controller.CanonicalManagementKey(testInfraNamespace, epco.ClusterReference{Name: testOtherName}, controller.SQLObjectRole, "AppRole"),
	} {
		if key == want {
			t.Fatalf("distinct SQL ownership key collapsed: %+v", key)
		}
	}
	role := &epco.DatabaseRole{Name: "fallback"}
	if got := controller.EffectiveRoleName(role); got != "fallback" {
		t.Fatalf("omitted SQL name=%q want metadata name", got)
	}
	role.Spec.Name = "Mixed Case"
	if got := controller.EffectiveRoleName(role); got != "Mixed Case" {
		t.Fatalf("explicit SQL name altered: %q", got)
	}
}

func ownershipRole(namespace, name, uid string) *epco.DatabaseRole {
	return &epco.DatabaseRole{Namespace: namespace, Name: name, UID: types.UID(uid), Spec: epco.DatabaseRoleSpec{Cluster: epco.ClusterReference{Namespace: testInfraNamespace, Name: testDescriptorName}, Name: testSharedSQLName}}
}

func TestRoleOwnershipConflicts(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutate   func(*epco.DatabaseRole)
		conflict bool
	}{
		{name: "cross namespace equal metadata names", mutate: func(r *epco.DatabaseRole) { r.Status.HasManagedSQL = true }, conflict: true},
		{name: "legacy successful management", mutate: func(r *epco.DatabaseRole) { r.Status.AppliedGeneration = 1 }, conflict: true},
		{name: "former manager now observes", mutate: func(r *epco.DatabaseRole) {
			r.Status.HasManagedSQL = true
			r.Spec.ManagementPolicy = epco.ManagementObserve
		}, conflict: true},
		{name: "temporary failure retains ownership", mutate: func(r *epco.DatabaseRole) { r.Status.HasManagedSQL = true; r.Status.Applied = new(false) }, conflict: true},
		{name: "deleting live manager retains ownership", mutate: func(r *epco.DatabaseRole) {
			r.Status.HasManagedSQL = true
			r.DeletionTimestamp = new(metav1.Now())
			r.Finalizers = []string{"epco.io/hold"}
		}, conflict: true},
		{name: "pure observer is not manager", mutate: func(r *epco.DatabaseRole) {
			r.Spec.ManagementPolicy = epco.ManagementObserve
			r.Status.ObservedGeneration = 10
		}},
		{name: "unrecorded managing contender is not manager", mutate: func(r *epco.DatabaseRole) { r.Status.ObservedGeneration = 10; r.Status.Applied = new(false) }},
		{name: "different descriptor namespace", mutate: func(r *epco.DatabaseRole) { r.Status.HasManagedSQL = true; r.Spec.Cluster.Namespace = testAnotherName }},
		{name: "different descriptor name", mutate: func(r *epco.DatabaseRole) { r.Status.HasManagedSQL = true; r.Spec.Cluster.Name = testAnotherName }},
		{name: "different SQL name", mutate: func(r *epco.DatabaseRole) { r.Status.HasManagedSQL = true; r.Spec.Name = testAnotherName }},
		{name: "self excluded by UID", mutate: func(r *epco.DatabaseRole) { r.Status.HasManagedSQL = true; r.UID = "requester" }},
		{name: "local reference canonicalized", mutate: func(r *epco.DatabaseRole) {
			r.Namespace = testInfraNamespace
			r.Status.HasManagedSQL = true
			r.Spec.Cluster.Namespace = ""
		}, conflict: true},
		{name: "effective name metadata fallback", mutate: func(r *epco.DatabaseRole) {
			r.Name = testSharedSQLName
			r.Spec.Name = ""
			r.Status.HasManagedSQL = true
		}, conflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requester := ownershipRole(testOrdersNamespace, "same", "requester")
			other := ownershipRole(testBillingNamespace, "same", testOtherName)
			tc.mutate(other)
			owner, err := controller.FindRoleOwnershipConflict(t.Context(), accessClient(t, other), requester)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.conflict {
				if owner != nil {
					t.Fatalf("unexpected recorded owner: %s/%s", owner.Namespace, owner.Name)
				}
				return
			}
			if owner == nil || owner.UID != other.UID {
				t.Fatalf("owner=%v want competing UID %q", owner, other.UID)
			}
		})
	}
}

func TestRoleOwnershipUsesAuthoritativeReader(t *testing.T) {
	requester := ownershipRole(testOrdersNamespace, "same", "requester")
	owner := ownershipRole(testBillingNamespace, "same", "recorded-owner")
	owner.Status.HasManagedSQL = true
	staleCache := accessClient(t)
	authoritative := accessClient(t, owner)
	if stale, err := controller.FindRoleOwnershipConflict(t.Context(), staleCache, requester); err != nil || stale != nil {
		t.Fatalf("fixture cache should be stale: owner=%v err=%v", stale, err)
	}
	got, err := controller.FindRoleOwnershipConflict(t.Context(), authoritative, requester)
	if err != nil || got == nil || got.UID != owner.UID {
		t.Fatalf("authoritative conflict was missed: owner=%v err=%v", got, err)
	}
	failure := apierrors.NewServiceUnavailable("temporary API failure")
	failedReader := interceptor.NewClient(authoritative, interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return failure }})
	if _, err := controller.FindRoleOwnershipConflict(t.Context(), failedReader, requester); !errors.Is(err, failure) {
		t.Fatalf("ownership read failure=%v want original error", err)
	}
}

func TestManagementLockSerializesCanonicalKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var locks controller.ManagementLocks
		key := controller.CanonicalManagementKey(testInfraNamespace, epco.ClusterReference{Name: testDescriptorName}, controller.SQLObjectRole, testSharedSQLName)
		otherNamespaceKey := controller.CanonicalManagementKey(testBillingNamespace, epco.ClusterReference{Namespace: testInfraNamespace, Name: testDescriptorName}, controller.SQLObjectRole, testSharedSQLName)
		release, err := locks.Acquire(t.Context(), key)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { release() }()
		entered := make(chan func(), 1)
		failed := make(chan error, 1)
		go func() {
			nextRelease, acquireErr := locks.Acquire(t.Context(), otherNamespaceKey)
			if acquireErr != nil {
				failed <- acquireErr
				return
			}
			entered <- nextRelease
		}()
		synctest.Wait()
		select {
		case nextRelease := <-entered:
			nextRelease()
			t.Fatal("same SQL key acquired concurrently across application namespaces")
		case err := <-failed:
			t.Fatal(err)
		default:
		}
		release()
		release = func() {}
		synctest.Wait()
		select {
		case nextRelease := <-entered:
			nextRelease()
		case err := <-failed:
			t.Fatal(err)
		default:
			t.Fatal("released management key did not admit waiting reconciliation")
		}
	})
}

func TestManagementLockCancellationAndIndependentKeys(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var locks controller.ManagementLocks
		key := controller.ManagementKey{Cluster: types.NamespacedName{Namespace: testInfraNamespace, Name: testDescriptorName}, ObjectType: controller.SQLObjectRole, SQLName: testSharedSQLName}
		release, err := locks.Acquire(t.Context(), key)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { release() }()
		ctx, cancel := context.WithCancel(t.Context())
		finished := make(chan error, 1)
		go func() {
			nextRelease, acquireErr := locks.Acquire(ctx, key)
			if nextRelease != nil {
				nextRelease()
			}
			finished <- acquireErr
		}()
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case err := <-finished:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled lock waiter error=%v", err)
			}
		default:
			t.Fatal("canceled waiter did not terminate while holder was still active")
		}
		for _, independent := range []controller.ManagementKey{
			{Cluster: key.Cluster, ObjectType: controller.SQLObjectDatabase, SQLName: key.SQLName},
			{Cluster: key.Cluster, ObjectType: key.ObjectType, SQLName: testOtherName},
			{Cluster: types.NamespacedName{Namespace: testOtherName, Name: testDescriptorName}, ObjectType: key.ObjectType, SQLName: key.SQLName},
		} {
			independentRelease, err := locks.Acquire(t.Context(), independent)
			if err != nil {
				t.Fatal(err)
			}
			independentRelease()
		}
		release()
		release = func() {}
		if unexpectedRelease, err := locks.Acquire(ctx, key); !errors.Is(err, context.Canceled) {
			if unexpectedRelease != nil {
				unexpectedRelease()
			}
			t.Fatalf("already canceled context acquired an idle key: %v", err)
		}
		nextRelease, err := locks.Acquire(t.Context(), key)
		if err != nil {
			t.Fatalf("canceled waiter poisoned future acquisition: %v", err)
		}
		nextRelease()
	})
}
