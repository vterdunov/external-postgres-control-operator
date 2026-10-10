package controller_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/controller"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func ownedGeneratedSource(role *epco.DatabaseRole) *corev1.Secret {
	source := passwordSource(role, testGeneratedName)
	source.Labels = map[string]string{controller.GeneratedPasswordLabel: testTrueLabel}
	source.Annotations = map[string]string{
		controller.GeneratedRoleUIDAnnotation:       string(role.UID),
		controller.GeneratedRoleNameAnnotation:      testSharedSQLName,
		controller.GeneratedRoleResourceAnnotation:  role.Name,
		controller.GeneratedRoleNamespaceAnnotation: role.Namespace,
	}
	return source
}

func TestRoleAccessDenialPausesMutationsAndPendingFinalizer(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial", true: "pending deletion"}[deleting], func(t *testing.T) {
			role := ownershipRole(testOrdersNamespace, "app", "uid")
			role.Spec.ReclaimPolicy = epco.ReclaimDelete
			role.Spec.PasswordGeneration = &epco.PasswordGeneration{SecretName: testGeneratedName}
			if deleting {
				role.Status.HasManagedSQL = true
				role.Finalizers = []string{controller.RoleFinalizer}
				role.DeletionTimestamp = new(metav1.Now())
			}
			source := ownedGeneratedSource(role)
			r, cl, session := roleFixture(t, role, source)
			cluster := &epco.PostgresCluster{}
			if err := cl.Get(t.Context(), client.ObjectKey{Namespace: testInfraNamespace, Name: testDescriptorName}, cluster); err != nil {
				t.Fatal(err)
			}
			cluster.Spec.Access = &epco.ClusterAccess{}
			if err := cl.Update(t.Context(), cluster); err != nil {
				t.Fatal(err)
			}
			session.apply = func(context.Context, postgres.RoleDesired) (*postgres.RoleApplyResult, error) {
				t.Fatal("access-denied role mutated SQL")
				return nil, nil
			}
			session.drop = func(context.Context, string) error { t.Fatal("access-denied deleting role dropped SQL"); return nil }
			runRole(t, r, role)
			latest := currentRole(t, cl, role)
			requireRoleCondition(t, latest, metav1.ConditionFalse, testAccessDeniedReason)
			if deleting && !slices.Contains(latest.Finalizers, controller.RoleFinalizer) {
				t.Fatal("access removal released pending finalizer")
			}
			if !deleting && len(latest.Finalizers) != 0 {
				t.Fatal("access-denied initial role acquired cleanup finalizer")
			}
			after := &corev1.Secret{}
			if err := cl.Get(t.Context(), client.ObjectKeyFromObject(source), after); err != nil {
				t.Fatal("access denial changed credentials")
			}
			if after.ResourceVersion != source.ResourceVersion && source.ResourceVersion != "" {
				t.Fatal("access denial updated credentials")
			}
		})
	}
}

func TestRoleDeletionRespectsRecordedManagerAndReclaimPolicy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		managed bool
		policy  epco.ReclaimPolicy
		drop    bool
	}{
		{name: "never successful delete", policy: epco.ReclaimDelete},
		{name: "recorded manager retain", managed: true, policy: epco.ReclaimRetain},
		{name: "recorded manager delete", managed: true, policy: epco.ReclaimDelete, drop: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			role := ownershipRole(testOrdersNamespace, "app", "uid")
			role.Spec.ReclaimPolicy = tc.policy
			role.Status.HasManagedSQL = tc.managed
			role.Finalizers = []string{controller.RoleFinalizer}
			role.DeletionTimestamp = new(metav1.Now())
			role.Spec.PasswordGeneration = &epco.PasswordGeneration{SecretName: testGeneratedName}
			source := ownedGeneratedSource(role)
			r, cl, session := roleFixture(t, role, source)
			drops := 0
			session.drop = func(context.Context, string) error {
				drops++
				if err := cl.Get(t.Context(), client.ObjectKeyFromObject(source), &corev1.Secret{}); err != nil {
					t.Fatal("generated source removed before SQL deletion confirmed")
				}
				return nil
			}
			runRole(t, r, role)
			want := 0
			if tc.drop {
				want = 1
			}
			if drops != want {
				t.Fatalf("SQL drops=%d want=%d", drops, want)
			}
			latest := &epco.DatabaseRole{}
			err := cl.Get(t.Context(), client.ObjectKeyFromObject(role), latest)
			if err != nil && !apierrors.IsNotFound(err) {
				t.Fatal(err)
			}
			if err == nil && slices.Contains(latest.Finalizers, controller.RoleFinalizer) {
				t.Fatal("completed deletion retained finalizer")
			}
			err = cl.Get(t.Context(), client.ObjectKeyFromObject(source), &corev1.Secret{})
			if tc.drop {
				if !apierrors.IsNotFound(err) {
					t.Fatal("confirmed SQL deletion did not remove owned generated source")
				}
			} else if err != nil {
				t.Fatal("retain or never-successful deletion removed credentials")
			}
		})
	}
}

func TestRoleDependencyBlockedDeletionPreservesSourceAndFinalizer(t *testing.T) {
	role := ownershipRole(testOrdersNamespace, "app", "uid")
	role.Spec.ReclaimPolicy = epco.ReclaimDelete
	role.Status.HasManagedSQL = true
	role.Finalizers = []string{controller.RoleFinalizer}
	role.DeletionTimestamp = new(metav1.Now())
	role.Spec.PasswordGeneration = &epco.PasswordGeneration{SecretName: testGeneratedName}
	source := ownedGeneratedSource(role)
	r, cl, session := roleFixture(t, role, source)
	session.drop = func(context.Context, string) error {
		return &postgres.Failure{Reason: "DeletionBlocked", Message: "safe dependent objects"}
	}
	runRole(t, r, role)
	latest := currentRole(t, cl, role)
	if !slices.Contains(latest.Finalizers, controller.RoleFinalizer) {
		t.Fatal("dependency failure released finalizer")
	}
	requireRoleCondition(t, latest, metav1.ConditionFalse, "DeletionBlocked")
	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(source), &corev1.Secret{}); err != nil {
		t.Fatal("blocked deletion removed credentials")
	}
}

func TestRoleDeletionPreservesSuppliedCredentialsAndReplacementGeneratedSource(t *testing.T) {
	for _, generated := range []bool{false, true} {
		t.Run(map[bool]string{false: "supplied", true: "generated source replaced in flight"}[generated], func(t *testing.T) {
			role := ownershipRole(testOrdersNamespace, "app", "uid")
			role.Spec.ReclaimPolicy = epco.ReclaimDelete
			role.Status.HasManagedSQL = true
			role.Finalizers = []string{controller.RoleFinalizer}
			role.DeletionTimestamp = new(metav1.Now())
			source := passwordSource(role, testProvidedName)
			if generated {
				role.Spec.PasswordGeneration = &epco.PasswordGeneration{SecretName: testGeneratedName}
				source = ownedGeneratedSource(role)
			} else {
				role.Spec.PasswordSecret = &epco.LocalReference{Name: source.Name}
			}
			source.UID = "source-original"
			r, cl, session := roleFixture(t, role, source)
			session.drop = func(context.Context, string) error {
				if generated {
					latest := &corev1.Secret{}
					if err := cl.Get(t.Context(), client.ObjectKeyFromObject(source), latest); err != nil {
						t.Fatal(err)
					}
					if err := cl.Delete(t.Context(), latest); err != nil {
						t.Fatal(err)
					}
					replacement := source.DeepCopy()
					replacement.UID = "source-replacement"
					replacement.ResourceVersion = ""
					replacement.Annotations[controller.GeneratedRoleUIDAnnotation] = "another-owner"
					if err := cl.Create(t.Context(), replacement); err != nil {
						t.Fatal(err)
					}
				}
				return nil
			}
			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(role)})
			if err != nil && (!generated || !apierrors.IsConflict(err)) {
				t.Fatal(err)
			}
			after := &corev1.Secret{}
			if err := cl.Get(t.Context(), client.ObjectKeyFromObject(source), after); err != nil {
				t.Fatal("cleanup deleted supplied credentials or an in-flight replacement")
			}
			if generated && after.UID != "source-replacement" {
				t.Fatal("source replacement fixture lost")
			}
		})
	}
}

func TestRoleDeletionPreservesForeignOwnedGeneratedSourceAfterSQLCleanup(t *testing.T) {
	role := ownershipRole(testOrdersNamespace, "app", "uid")
	role.Spec.ReclaimPolicy = epco.ReclaimDelete
	role.Spec.PasswordGeneration = &epco.PasswordGeneration{SecretName: testGeneratedName}
	role.Status.HasManagedSQL = true
	role.Finalizers = []string{controller.RoleFinalizer}
	role.DeletionTimestamp = new(metav1.Now())
	source := ownedGeneratedSource(role)
	source.UID = "original-generated-source"
	source.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "v1", Kind: testConfigMapKind, Name: "independent-owner", UID: "foreign-owner",
	}}
	r, cl, session := roleFixture(t, role, source)
	before := &corev1.Secret{}
	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(source), before); err != nil {
		t.Fatal(err)
	}
	drops := 0
	session.drop = func(_ context.Context, name string) error {
		if name != testSharedSQLName {
			t.Fatal("cleanup selected an unrelated SQL role")
		}
		drops++
		return nil
	}
	_, reconcileErr := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(role)})
	if drops != 1 {
		t.Fatalf("recorded manager SQL cleanup calls=%d want=1", drops)
	}
	after := &corev1.Secret{}
	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(source), after); err != nil {
		t.Fatal("confirmed SQL cleanup deleted a generated source with foreign ownership")
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("cleanup modified a foreign-owned generated source")
	}
	if reconcileErr != nil {
		failure, ok := errors.AsType[*postgres.Failure](reconcileErr)
		if !ok || failure.Reason != "ConnectionSecretConflict" {
			t.Fatalf("unexpected reconciliation failure type: %T", reconcileErr)
		}
	}
}
