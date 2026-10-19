package controller_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/controller"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type roleSessionStub struct {
	state  *postgres.RoleState
	apply  func(context.Context, postgres.RoleDesired) (*postgres.RoleApplyResult, error)
	drop   func(context.Context, string) error
	closed int
}

func (s *roleSessionStub) Inspect(context.Context, string) (*postgres.RoleState, error) {
	return s.state, nil
}
func (s *roleSessionStub) Apply(ctx context.Context, d postgres.RoleDesired) (*postgres.RoleApplyResult, error) {
	if s.apply != nil {
		return s.apply(ctx, d)
	}
	return &postgres.RoleApplyResult{}, nil
}
func (s *roleSessionStub) Drop(ctx context.Context, name string) error {
	if s.drop != nil {
		return s.drop(ctx, name)
	}
	return nil
}
func (s *roleSessionStub) Close(context.Context) error { s.closed++; return nil }

type roleFactoryFunc func(context.Context, *postgres.Config) (postgres.RoleSession, error)

func (f roleFactoryFunc) Open(ctx context.Context, c *postgres.Config) (postgres.RoleSession, error) {
	return f(ctx, c)
}

func roleFixture(t *testing.T, role *epco.DatabaseRole, objects ...client.Object) (*controller.DatabaseRoleReconciler, client.WithWatch, *roleSessionStub) {
	t.Helper()
	cluster := &epco.ExternalPostgres{Namespace: testInfraNamespace, Name: testDescriptorName, Spec: epco.ExternalPostgresSpec{
		Connection: epco.PostgresConnection{Host: "DATABASE.EXAMPLE."}, CredentialsSecret: epco.LocalReference{Name: testAdminName}, TLS: &epco.PostgresTLS{Mode: dbTestTLSDisable}, Access: &epco.ClusterAccess{NamespaceSelector: &metav1.LabelSelector{}},
	}}
	admin := &corev1.Secret{Namespace: testInfraNamespace, Name: testAdminName, Type: corev1.SecretTypeBasicAuth, Data: map[string][]byte{corev1.BasicAuthUsernameKey: []byte(testAdminName), corev1.BasicAuthPasswordKey: []byte("administrative-credential")}}
	all := append([]client.Object{role, cluster, admin, &corev1.Namespace{Name: testOrdersNamespace}}, objects...)
	cl := accessClient(t, all...)
	builder, err := postgres.NewConfigBuilder()
	if err != nil {
		t.Fatal(err)
	}
	session := &roleSessionStub{state: &postgres.RoleState{Exists: true, Name: testSharedSQLName, AdminUser: testAdminName, ServerVersion: 180000, MaxIdentifierLength: 63}}
	r := &controller.DatabaseRoleReconciler{Client: cl, APIReader: cl, Builder: builder, Roles: roleFactoryFunc(func(context.Context, *postgres.Config) (postgres.RoleSession, error) { return session, nil }), Locks: &controller.ManagementLocks{}, Now: func() time.Time { return time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC) }}
	return r, cl, session
}
func runRole(t *testing.T, r *controller.DatabaseRoleReconciler, role *epco.DatabaseRole) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(role)})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func currentRole(t *testing.T, cl client.Reader, role *epco.DatabaseRole) *epco.DatabaseRole {
	t.Helper()
	latest := &epco.DatabaseRole{}
	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(role), latest); err != nil {
		t.Fatal(err)
	}
	return latest
}
func requireRoleCondition(t *testing.T, role *epco.DatabaseRole, status metav1.ConditionStatus, reason string) {
	t.Helper()
	condition := meta.FindStatusCondition(role.Status.Conditions, testReadyCondition)
	if condition == nil || condition.Status != status || condition.Reason != reason || condition.ObservedGeneration != role.Generation {
		t.Fatalf("Ready condition=%+v want status=%s reason=%s current generation", condition, status, reason)
	}
}
func passwordSource(role *epco.DatabaseRole, name string) *corev1.Secret {
	return &corev1.Secret{Namespace: role.Namespace, Name: name, Type: corev1.SecretTypeBasicAuth, Data: map[string][]byte{corev1.BasicAuthUsernameKey: []byte(testSharedSQLName), corev1.BasicAuthPasswordKey: []byte("application-credential")}}
}

func TestRoleObservationNeverAcquiresManagementOrCredentials(t *testing.T) {
	for _, exists := range []bool{true, false} {
		t.Run(map[bool]string{true: "existing", false: "missing"}[exists], func(t *testing.T) {
			role := ownershipRole(testOrdersNamespace, "app", "uid")
			role.Generation = 2
			role.Spec.ManagementPolicy = epco.ManagementObserve
			role.Spec.PasswordGeneration = &epco.PasswordGeneration{SecretName: testGeneratedName}
			r, cl, session := roleFixture(t, role)
			session.state.Exists = exists
			session.apply = func(context.Context, postgres.RoleDesired) (*postgres.RoleApplyResult, error) {
				t.Error("observer mutated SQL")
				return nil, nil
			}
			runRole(t, r, role)
			latest := currentRole(t, cl, role)
			if latest.Status.HasManagedSQL || latest.Status.Applied != nil || latest.Status.AppliedGeneration != 0 || latest.Status.GeneratedPasswordInitialized {
				t.Fatal("observation acquired management or credentials")
			}
			if exists {
				requireRoleCondition(t, latest, metav1.ConditionTrue, "Observed")
			} else {
				requireRoleCondition(t, latest, metav1.ConditionFalse, "RoleNotFound")
			}
			secret := &corev1.Secret{}
			if err := cl.Get(t.Context(), client.ObjectKey{Namespace: role.Namespace, Name: testGeneratedName}, secret); !apierrors.IsNotFound(err) {
				t.Fatal("observation created a password source")
			}
			if session.closed != 1 {
				t.Fatal("observation left SQL session open")
			}
		})
	}
}

func TestRoleManagementRecordsOnlyPersistedCompleteSuccess(t *testing.T) {
	for _, failStatus := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "status failure"}[failStatus], func(t *testing.T) {
			role := ownershipRole(testOrdersNamespace, "app", "uid")
			role.Generation = 2
			r, cl, session := roleFixture(t, role)
			if failStatus {
				failure := apierrors.NewServiceUnavailable("status persistence failure")
				r.Client = interceptor.NewClient(cl, interceptor.Funcs{
					SubResourcePatch: func(_ context.Context, _ client.Client, _ string, obj client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
						if v, ok := obj.(*epco.DatabaseRole); ok && v.Status.HasManagedSQL {
							return failure
						}
						return nil
					},
					SubResourceUpdate: func(_ context.Context, _ client.Client, _ string, obj client.Object, _ ...client.SubResourceUpdateOption) error {
						if v, ok := obj.(*epco.DatabaseRole); ok && v.Status.HasManagedSQL {
							return failure
						}
						return nil
					},
				})
				_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(role)})
				if !errors.Is(err, failure) {
					t.Fatalf("status error=%v want persistence failure", err)
				}
			} else {
				runRole(t, r, role)
			}
			latest := currentRole(t, cl, role)
			if failStatus {
				if latest.Status.HasManagedSQL || latest.Status.AppliedGeneration != 0 {
					t.Fatal("failed status persistence recorded successful manager")
				}
			} else {
				if !latest.Status.HasManagedSQL || latest.Status.AppliedGeneration != 2 || latest.Status.LastSuccessfulReconcileTime == nil {
					t.Fatal("complete success missing sticky manager/generation/time")
				}
				requireRoleCondition(t, latest, metav1.ConditionTrue, testReadyCondition)
			}
			if session.closed != 1 {
				t.Fatal("SQL session was not closed")
			}
		})
	}
}

func TestRoleCleanupIntentSurvivesSQLFailureAndRestart(t *testing.T) {
	role := ownershipRole(testOrdersNamespace, "app", "uid")
	role.Generation = 1
	role.Spec.InRoles = []epco.SQLName{testParentName}
	role.Spec.Parameters = map[string]string{testSearchPath: "app"}
	role.Spec.MembershipPolicy = epco.MembershipManaged
	r, cl, session := roleFixture(t, role)
	session.apply = func(ctx context.Context, d postgres.RoleDesired) (*postgres.RoleApplyResult, error) {
		checkpoint := issue30Checkpoint(t, d)
		if err := checkpoint(ctx, epco.RoleMembershipBinding{Role: testParentName, Member: testSharedSQLName, Grantor: testAdminName}, true); err != nil {
			return nil, err
		}
		latest := currentRole(t, cl, role)
		if len(latest.Status.ManagedEndpoints) != 1 || latest.Status.ManagedEndpoints[0].Host != testEndpointHost || latest.Status.ManagedEndpoints[0].Port != 5432 || !slices.Contains(latest.Status.ManagedEndpoints[0].Parameters, testSearchPath) {
			t.Fatal("cleanup intent not durable before SQL")
		}
		found := false
		for _, binding := range latest.Status.ManagedEndpoints[0].Memberships {
			if binding.Role == testParentName && binding.Member == testSharedSQLName && !binding.Administrative {
				found = true
			}
		}
		if !found {
			t.Fatal("user membership cleanup intent not persisted before SQL")
		}
		return nil, &postgres.Failure{Reason: "InsufficientPrivileges", Message: testSafeFailure}
	}
	runRole(t, r, role)
	latest := currentRole(t, cl, role)
	if latest.Status.HasManagedSQL {
		t.Fatal("failed SQL recorded manager")
	}
	latest.Spec.InRoles = nil
	latest.Spec.Parameters = nil
	latest.Generation++
	if err := cl.Update(t.Context(), latest); err != nil {
		t.Fatal(err)
	}
	r.Locks = &controller.ManagementLocks{}
	session.apply = func(_ context.Context, d postgres.RoleDesired) (*postgres.RoleApplyResult, error) {
		if !slices.Contains(d.TrackedParameters, testSearchPath) {
			t.Fatal("restart lost possibly applied parameter")
		}
		found := false
		for _, binding := range d.TrackedMemberships {
			if binding.Role == testParentName && binding.Member == testSharedSQLName {
				found = true
			}
		}
		if !found {
			t.Fatal("restart lost possibly applied membership")
		}
		return &postgres.RoleApplyResult{}, nil
	}
	runRole(t, r, role)
	latest = currentRole(t, cl, role)
	if len(latest.Status.ManagedEndpoints) != 1 || len(latest.Status.ManagedEndpoints[0].Parameters) != 0 || len(latest.Status.ManagedEndpoints[0].Memberships) != 0 {
		t.Fatal("confirmed cleanup did not prune durable intent")
	}
}

func TestGeneratedPasswordIsPersistedCheckpointedAndReused(t *testing.T) {
	role := ownershipRole(testOrdersNamespace, "app", "uid")
	role.Spec.PasswordGeneration = &epco.PasswordGeneration{SecretName: testGeneratedName}
	r, cl, session := roleFixture(t, role)
	var first []byte
	attempts := 0
	session.apply = func(_ context.Context, d postgres.RoleDesired) (*postgres.RoleApplyResult, error) {
		attempts++
		source := &corev1.Secret{}
		if err := cl.Get(t.Context(), client.ObjectKey{Namespace: testOrdersNamespace, Name: testGeneratedName}, source); err != nil {
			t.Fatal("generated source not persisted before SQL")
		}
		if !currentRole(t, cl, role).Status.GeneratedPasswordInitialized {
			t.Fatal("accepted source checkpoint not persisted before SQL")
		}
		if source.Type != corev1.SecretTypeBasicAuth || string(source.Data[corev1.BasicAuthUsernameKey]) != testSharedSQLName || d.Password == nil || !bytes.Equal(source.Data[corev1.BasicAuthPasswordKey], []byte(*d.Password)) {
			t.Fatal("SQL did not accept persisted generated source")
		}
		decoded, err := base64.RawURLEncoding.DecodeString(string(source.Data[corev1.BasicAuthPasswordKey]))
		if err != nil || len(decoded) != 32 {
			t.Fatal("generated password must encode 32 random bytes as base64url")
		}
		if len(source.OwnerReferences) != 0 {
			t.Fatal("generated source may not be garbage collected with the role")
		}
		if attempts == 1 {
			first = bytes.Clone(source.Data[corev1.BasicAuthPasswordKey])
			return nil, &postgres.Failure{Reason: "ConnectionUnavailable", Message: testSafeFailure}
		}
		if !bytes.Equal(first, source.Data[corev1.BasicAuthPasswordKey]) {
			t.Fatal("retry regenerated persisted source")
		}
		return &postgres.RoleApplyResult{}, nil
	}
	runRole(t, r, role)
	runRole(t, r, role)
	source := &corev1.Secret{}
	if err := cl.Get(t.Context(), client.ObjectKey{Namespace: testOrdersNamespace, Name: testGeneratedName}, source); err != nil {
		t.Fatal(err)
	}
	if err := cl.Delete(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	session.apply = func(context.Context, postgres.RoleDesired) (*postgres.RoleApplyResult, error) {
		t.Fatal("missing initialized source reached SQL")
		return nil, nil
	}
	runRole(t, r, role)
	requireRoleCondition(t, currentRole(t, cl, role), metav1.ConditionFalse, "CredentialsMissing")
	if err := cl.Get(t.Context(), client.ObjectKey{Namespace: testOrdersNamespace, Name: testGeneratedName}, source); !apierrors.IsNotFound(err) {
		t.Fatal("lost initialized source was regenerated")
	}
}

func TestPasswordCheckpointFailureProhibitsSourceAndSQLMutation(t *testing.T) {
	role := ownershipRole(testOrdersNamespace, "app", "uid")
	role.Spec.PasswordGeneration = &epco.PasswordGeneration{SecretName: testGeneratedName}
	r, cl, session := roleFixture(t, role)
	failure := apierrors.NewConflict(schema.GroupResource{Group: epco.SchemeGroupVersion.Group, Resource: "databaseroles"}, role.Name, errors.New("concurrent writer"))
	r.Client = interceptor.NewClient(cl, interceptor.Funcs{SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
		return failure
	}, SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
		return failure
	}})
	session.apply = func(context.Context, postgres.RoleDesired) (*postgres.RoleApplyResult, error) {
		t.Fatal("failed pre-generation checkpoint reached SQL mutation")
		return nil, nil
	}
	result := runRole(t, r, role)
	if result.RequeueAfter <= 0 {
		t.Fatal("checkpoint conflict must retry")
	}
	source := &corev1.Secret{}
	if err := cl.Get(t.Context(), client.ObjectKey{Namespace: testOrdersNamespace, Name: testGeneratedName}, source); !apierrors.IsNotFound(err) {
		t.Fatal("checkpoint conflict created credentials")
	}
}

func TestSuppliedPasswordSourceValidationAndImmutability(t *testing.T) {
	for _, variation := range []string{testValidCase, "wrong type", "missing username", connectionMissingPasswordCase, "mismatched username", "remote source"} {
		t.Run(variation, func(t *testing.T) {
			role := ownershipRole(testOrdersNamespace, "app", "uid")
			role.Spec.PasswordSecret = &epco.LocalReference{Name: testProvidedName}
			source := passwordSource(role, testProvidedName)
			switch variation {
			case "wrong type":
				source.Type = corev1.SecretTypeOpaque
			case "missing username":
				delete(source.Data, corev1.BasicAuthUsernameKey)
			case connectionMissingPasswordCase:
				delete(source.Data, corev1.BasicAuthPasswordKey)
			case "mismatched username":
				source.Data[corev1.BasicAuthUsernameKey] = []byte("different")
			case "remote source":
				source.Namespace = testInfraNamespace
			}
			r, cl, session := roleFixture(t, role, source)
			original := &corev1.Secret{}
			if err := cl.Get(t.Context(), client.ObjectKeyFromObject(source), original); err != nil {
				t.Fatal(err)
			}
			session.apply = func(_ context.Context, d postgres.RoleDesired) (*postgres.RoleApplyResult, error) {
				if variation != testValidCase {
					t.Fatal("invalid/locality-violating source reached SQL")
				}
				if d.Password == nil || *d.Password != string(source.Data[corev1.BasicAuthPasswordKey]) {
					t.Fatal("supplied credential not applied")
				}
				return &postgres.RoleApplyResult{}, nil
			}
			runRole(t, r, role)
			after := &corev1.Secret{}
			if err := cl.Get(t.Context(), client.ObjectKeyFromObject(source), after); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(original, after) {
				t.Fatal("supplied source was modified")
			}
			if variation == testValidCase {
				latest := currentRole(t, cl, role)
				if latest.Status.SecretResourceVersion != source.ResourceVersion && latest.Status.SecretResourceVersion != original.ResourceVersion {
					t.Fatal("accepted supplied source resourceVersion was not recorded")
				}
			}
		})
	}
}

func TestAcceptedPasswordVersionRecordsAppliedSnapshotAndConverges(t *testing.T) {
	role := ownershipRole(testOrdersNamespace, "app", "uid")
	role.Spec.PasswordSecret = &epco.LocalReference{Name: testProvidedName}
	source := passwordSource(role, testProvidedName)
	r, cl, session := roleFixture(t, role, source)
	accepted := &corev1.Secret{}
	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(source), accepted); err != nil {
		t.Fatal(err)
	}
	session.apply = func(_ context.Context, d postgres.RoleDesired) (*postgres.RoleApplyResult, error) {
		if d.Password == nil || *d.Password != string(accepted.Data[corev1.BasicAuthPasswordKey]) {
			t.Fatal("initial snapshot was not applied")
		}
		latest := accepted.DeepCopy()
		latest.Data[corev1.BasicAuthPasswordKey] = []byte("explicit-rotation")
		if err := cl.Update(t.Context(), latest); err != nil {
			t.Fatal(err)
		}
		return &postgres.RoleApplyResult{}, nil
	}
	runRole(t, r, role)
	if currentRole(t, cl, role).Status.SecretResourceVersion != accepted.ResourceVersion {
		t.Fatal("status recorded a newer source version than SQL accepted")
	}
	rotated := &corev1.Secret{}
	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(source), rotated); err != nil {
		t.Fatal(err)
	}
	session.apply = func(_ context.Context, d postgres.RoleDesired) (*postgres.RoleApplyResult, error) {
		if d.Password == nil || *d.Password != string(rotated.Data[corev1.BasicAuthPasswordKey]) {
			t.Fatal("next reconciliation did not converge to rotated source")
		}
		return &postgres.RoleApplyResult{}, nil
	}
	runRole(t, r, role)
	if currentRole(t, cl, role).Status.SecretResourceVersion != rotated.ResourceVersion {
		t.Fatal("rotation acceptance version was not recorded")
	}
}

func TestRoleOrdinaryInputsUseCacheAndAuthoritativeOwnershipUsesAPI(t *testing.T) {
	role := ownershipRole(testOrdersNamespace, "app", "uid")
	role.Spec.PasswordSecret = &epco.LocalReference{Name: testProvidedName}
	r, cl, _ := roleFixture(t, role, passwordSource(role, testProvidedName))
	r.APIReader = interceptor.NewClient(cl, interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		t.Error("ordinary role, descriptor or supplied source read bypassed manager cache")
		return apierrors.NewServiceUnavailable("ordinary direct reads prohibited")
	}})
	runRole(t, r, role)
	requireRoleCondition(t, currentRole(t, cl, role), metav1.ConditionTrue, testReadyCondition)
}

func TestRoleEndpointHistoryPreservesDormantCleanup(t *testing.T) {
	role := ownershipRole(testOrdersNamespace, "app", "uid")
	role.Spec.MembershipPolicy = epco.MembershipManaged
	role.Status.HasManagedSQL = true
	role.Status.ManagedEndpoints = []epco.RoleEndpointState{
		{Host: testEndpointHost, Port: 5432, Parameters: []string{testSearchPath}},
		{Host: "second.example", Port: 5432, Parameters: []string{"statement_timeout"}},
	}
	r, cl, session := roleFixture(t, role)
	currentParameter := testSearchPath
	session.apply = func(_ context.Context, desired postgres.RoleDesired) (*postgres.RoleApplyResult, error) {
		if len(desired.TrackedParameters) != 1 || desired.TrackedParameters[0] != currentParameter {
			t.Fatal("endpoint cleanup mixed current and dormant histories")
		}
		return nil, &postgres.Failure{Reason: "ConnectionUnavailable", Message: testSafeFailure}
	}
	runRole(t, r, role)
	cluster := &epco.ExternalPostgres{}
	if err := cl.Get(t.Context(), client.ObjectKey{Namespace: testInfraNamespace, Name: testDescriptorName}, cluster); err != nil {
		t.Fatal(err)
	}
	cluster.Spec.Connection.Host = "second.example"
	if err := cl.Update(t.Context(), cluster); err != nil {
		t.Fatal(err)
	}
	currentParameter = "statement_timeout"
	session.apply = func(_ context.Context, desired postgres.RoleDesired) (*postgres.RoleApplyResult, error) {
		if len(desired.TrackedParameters) != 1 || desired.TrackedParameters[0] != currentParameter {
			t.Fatal("current endpoint lost its own cleanup history")
		}
		return &postgres.RoleApplyResult{}, nil
	}
	runRole(t, r, role)
	latest := currentRole(t, cl, role)
	var dormant *epco.RoleEndpointState
	for i := range latest.Status.ManagedEndpoints {
		if latest.Status.ManagedEndpoints[i].Host == testEndpointHost {
			dormant = &latest.Status.ManagedEndpoints[i]
		}
	}
	if dormant == nil || !slices.Contains(dormant.Parameters, testSearchPath) {
		t.Fatal("successful B reconcile erased failed A cleanup")
	}
	cluster.Spec.Connection.Host = "DATABASE.EXAMPLE."
	if err := cl.Update(t.Context(), cluster); err != nil {
		t.Fatal(err)
	}
	currentParameter = testSearchPath
	runRole(t, r, role)
	latest = currentRole(t, cl, role)
	if len(latest.Status.ManagedEndpoints) != 2 {
		t.Fatal("endpoint A-B-A did not retain separate histories")
	}
	for _, endpoint := range latest.Status.ManagedEndpoints {
		if len(endpoint.Parameters) != 0 {
			t.Fatal("confirmed current cleanup did not prune endpoint intent")
		}
	}
}

func TestRetainedGeneratedSourceIsConditionallyAdoptedWithoutRotation(t *testing.T) {
	for _, variation := range []string{testValidCase, "unrelated", "wrong resource", "wrong namespace", "wrong SQL name", connectionWrongUsernameCase, "live prior UID", "foreign owner reference"} {
		t.Run(variation, func(t *testing.T) {
			role := ownershipRole(testOrdersNamespace, "app", "new-uid")
			role.Spec.PasswordGeneration = &epco.PasswordGeneration{SecretName: testGeneratedName}
			source := passwordSource(role, testGeneratedName)
			source.Labels = map[string]string{controller.GeneratedPasswordLabel: testTrueLabel}
			source.Annotations = map[string]string{
				controller.GeneratedRoleUIDAnnotation:       "previous-uid",
				controller.GeneratedRoleNameAnnotation:      testSharedSQLName,
				controller.GeneratedRoleResourceAnnotation:  role.Name,
				controller.GeneratedRoleNamespaceAnnotation: role.Namespace,
			}
			switch variation {
			case "unrelated":
				source.Labels = nil
			case "wrong resource":
				source.Annotations[controller.GeneratedRoleResourceAnnotation] = testAnotherName
			case "wrong namespace":
				source.Annotations[controller.GeneratedRoleNamespaceAnnotation] = testAnotherName
			case "wrong SQL name":
				source.Annotations[controller.GeneratedRoleNameAnnotation] = testAnotherName
			case connectionWrongUsernameCase:
				source.Data[corev1.BasicAuthUsernameKey] = []byte(testAnotherName)
			case "foreign owner reference":
				source.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: testConfigMapKind, Name: "independent-owner", UID: "foreign-owner"}}
			}
			r, cl, session := roleFixture(t, role, source)
			if variation == "live prior UID" {
				old := role.DeepCopy()
				old.UID = "previous-uid"
				r.APIReader = accessClient(t, old)
			}
			original := &corev1.Secret{}
			if err := cl.Get(t.Context(), client.ObjectKeyFromObject(source), original); err != nil {
				t.Fatal(err)
			}
			session.apply = func(_ context.Context, desired postgres.RoleDesired) (*postgres.RoleApplyResult, error) {
				if variation != testValidCase {
					t.Fatal("source with invalid provenance or active previous owner reached SQL mutation")
				}
				if desired.Password == nil || *desired.Password != string(original.Data[corev1.BasicAuthPasswordKey]) {
					t.Fatal("retained source password was rotated during adoption")
				}
				return &postgres.RoleApplyResult{}, nil
			}
			runRole(t, r, role)
			after := &corev1.Secret{}
			if err := cl.Get(t.Context(), client.ObjectKeyFromObject(source), after); err != nil {
				t.Fatal(err)
			}
			if variation == testValidCase {
				if after.Annotations[controller.GeneratedRoleUIDAnnotation] != string(role.UID) || !bytes.Equal(after.Data[corev1.BasicAuthPasswordKey], original.Data[corev1.BasicAuthPasswordKey]) || len(after.OwnerReferences) != 0 {
					t.Fatal("retained source adoption failed identity/password/GC contract")
				}
			} else if !reflect.DeepEqual(original, after) {
				t.Fatal("source with invalid provenance was modified")
			}
		})
	}
}

func TestRoleDependencyUsesRecordedParentManagerOverLosingContender(t *testing.T) {
	child := ownershipRole(testOrdersNamespace, "child", "child-uid")
	child.Spec.InRoles = []epco.SQLName{testParentName}
	parent := ownershipRole(testOrdersNamespace, testParentName, "parent-manager")
	parent.Spec.Name = testParentName
	parent.Generation = 1
	parent.Status.HasManagedSQL = true
	parent.Status.Conditions = []metav1.Condition{{Type: testReadyCondition, Status: metav1.ConditionTrue, Reason: testReadyCondition, ObservedGeneration: 1}}
	contender := ownershipRole(testBillingNamespace, testParentName, "losing-contender")
	contender.Spec.Name = testParentName
	contender.Generation = 1
	contender.Status.Conditions = []metav1.Condition{{Type: testReadyCondition, Status: metav1.ConditionFalse, Reason: connectionOwnershipConflictReason, ObservedGeneration: 1}}
	r, cl, _ := roleFixture(t, child, parent, contender)
	runRole(t, r, child)
	requireRoleCondition(t, currentRole(t, cl, child), metav1.ConditionTrue, testReadyCondition)
}
