package controller_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/controller"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const issue30FirstParent = "parent_one"

func issue30Checkpoint(t *testing.T, desired postgres.RoleDesired) func(context.Context, epco.RoleMembershipBinding, bool) error {
	t.Helper()
	if desired.CheckpointMembership == nil {
		t.Fatal("role SQL has no callback for durable exact-grantor intent before mutation")
	}
	return desired.CheckpointMembership
}

func issue30TrackedMemberships(t *testing.T, cl client.Reader, role *epco.DatabaseRole) []epco.RoleMembershipBinding {
	t.Helper()
	var bindings []epco.RoleMembershipBinding
	for _, endpoint := range currentRole(t, cl, role).Status.ManagedEndpoints {
		bindings = append(bindings, endpoint.Memberships...)
	}
	return bindings
}

func TestIssue30ExactMembershipIntentSurvivesFinalStatusFailureAndRotation(t *testing.T) {
	role := ownershipRole(testOrdersNamespace, "issue30", "uid-issue30")
	role.Generation = 1
	role.Spec.MembershipPolicy = epco.MembershipManaged
	role.Spec.InRoles = []epco.SQLName{issue30FirstParent, "parent_two"}
	r, cl, session := roleFixture(t, role)
	owned := []epco.RoleMembershipBinding{
		{Role: issue30FirstParent, Member: testSharedSQLName, Grantor: "grantor_one"},
		{Role: "parent_two", Member: testSharedSQLName, Grantor: "grantor_two"},
	}
	independent := epco.RoleMembershipBinding{Role: issue30FirstParent, Member: testSharedSQLName, Grantor: "independent_dba"}
	statusFailure := apierrors.NewServiceUnavailable("final status unavailable")
	r.Client = interceptor.NewClient(cl, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, base client.Client, subresource string, object client.Object, options ...client.SubResourceUpdateOption) error {
			if value, ok := object.(*epco.DatabaseRole); ok && value.Status.HasManagedSQL {
				return statusFailure
			}
			return base.SubResource(subresource).Update(ctx, object, options...)
		},
		SubResourcePatch: func(ctx context.Context, base client.Client, subresource string, object client.Object, patch client.Patch, options ...client.SubResourcePatchOption) error {
			if value, ok := object.(*epco.DatabaseRole); ok && value.Status.HasManagedSQL {
				return statusFailure
			}
			return base.SubResource(subresource).Patch(ctx, object, patch, options...)
		},
	})
	committed := false
	session.apply = func(ctx context.Context, desired postgres.RoleDesired) (*postgres.RoleApplyResult, error) {
		checkpoint := issue30Checkpoint(t, desired)
		rejected := epco.RoleMembershipBinding{Role: issue30FirstParent, Member: testSharedSQLName, Grantor: "refused_candidate"}
		if err := checkpoint(ctx, rejected, true); err != nil {
			return nil, err
		}
		if !slices.Contains(issue30TrackedMemberships(t, cl, role), rejected) {
			t.Fatal("selected candidate intent was not durable before its attempted SQL")
		}
		// The SQL layer invokes retirement only after rollback and absence proof.
		if err := checkpoint(ctx, rejected, false); err != nil {
			return nil, err
		}
		if slices.Contains(issue30TrackedMemberships(t, cl, role), rejected) {
			t.Fatal("confirmed rejected candidate intent was not retired")
		}
		for _, binding := range owned {
			if err := checkpoint(ctx, binding, true); err != nil {
				return nil, err
			}
			if !slices.Contains(issue30TrackedMemberships(t, cl, role), binding) {
				t.Fatalf("exact grantor not durable before SQL: %#v", binding)
			}
		}
		for _, binding := range issue30TrackedMemberships(t, cl, role) {
			if !binding.Administrative && !slices.Contains(owned, binding) {
				t.Fatalf("cleanup intent guessed an unselected grantor: %#v", binding)
			}
		}
		committed = true
		return &postgres.RoleApplyResult{Memberships: owned}, nil
	}
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(role)})
	if !errors.Is(err, statusFailure) || !committed {
		t.Fatalf("SQL commit followed by final status failure: committed=%v error=%v", committed, err)
	}
	latest := currentRole(t, cl, role)
	if latest.Status.HasManagedSQL || latest.Status.AppliedGeneration != 0 {
		t.Fatal("failed status write published successful management")
	}
	for _, binding := range owned {
		if !slices.Contains(issue30TrackedMemberships(t, cl, role), binding) {
			t.Fatalf("final status failure erased exact intent: %#v", binding)
		}
	}
	latest.Spec.InRoles = nil
	latest.Generation++
	if err = cl.Update(t.Context(), latest); err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{}
	if err = cl.Get(t.Context(), client.ObjectKey{Namespace: testInfraNamespace, Name: testAdminName}, secret); err != nil {
		t.Fatal(err)
	}
	secret.Data[corev1.BasicAuthUsernameKey] = []byte("rotated_operator")
	if err = cl.Update(t.Context(), secret); err != nil {
		t.Fatal(err)
	}
	r.Client, r.Locks = cl, &controller.ManagementLocks{}
	session.state.AdminUser = "rotated_operator"
	session.state.Memberships = append(slices.Clone(owned), independent)
	session.apply = func(_ context.Context, desired postgres.RoleDesired) (*postgres.RoleApplyResult, error) {
		for _, binding := range owned {
			if !slices.Contains(desired.TrackedMemberships, binding) {
				t.Fatalf("restart/rotation lost cleanup provenance: %#v", desired.TrackedMemberships)
			}
		}
		if slices.Contains(desired.TrackedMemberships, independent) {
			t.Fatal("independent row became cleanup responsibility")
		}
		return &postgres.RoleApplyResult{}, nil
	}
	runRole(t, r, role)
	if len(issue30TrackedMemberships(t, cl, role)) != 0 {
		t.Fatal("confirmed omission left obsolete cleanup intent")
	}
}

func TestIssue30FailedMembershipCheckpointPreventsSQL(t *testing.T) {
	role := ownershipRole(testOrdersNamespace, "issue30-checkpoint", "uid-checkpoint")
	role.Spec.InRoles = []epco.SQLName{issue30FirstParent}
	r, cl, session := roleFixture(t, role)
	failure := apierrors.NewServiceUnavailable("membership checkpoint unavailable")
	r.Client = interceptor.NewClient(cl, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, base client.Client, subresource string, object client.Object, options ...client.SubResourceUpdateOption) error {
			if value, ok := object.(*epco.DatabaseRole); ok {
				for _, endpoint := range value.Status.ManagedEndpoints {
					if slices.ContainsFunc(endpoint.Memberships, func(binding epco.RoleMembershipBinding) bool { return binding.Grantor == "reachable_grantor" }) {
						return failure
					}
				}
			}
			return base.SubResource(subresource).Update(ctx, object, options...)
		},
	})
	mutations := 0
	attempted := false
	session.apply = func(ctx context.Context, desired postgres.RoleDesired) (*postgres.RoleApplyResult, error) {
		attempted = true
		checkpoint := issue30Checkpoint(t, desired)
		binding := epco.RoleMembershipBinding{Role: issue30FirstParent, Member: testSharedSQLName, Grantor: "reachable_grantor"}
		if err := checkpoint(ctx, binding, true); err != nil {
			return nil, err
		}
		mutations++
		return &postgres.RoleApplyResult{Memberships: []epco.RoleMembershipBinding{binding}}, nil
	}
	_, _ = r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(role)})
	if !attempted {
		t.Fatal("the selected membership candidate was never prepared")
	}
	if mutations != 0 {
		t.Fatal("SQL mutation proceeded after durable intent checkpoint failed")
	}
	if currentRole(t, cl, role).Status.HasManagedSQL {
		t.Fatal("failed checkpoint published SQL success")
	}
}
