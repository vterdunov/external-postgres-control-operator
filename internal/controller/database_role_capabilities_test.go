package controller_test

import (
	"context"
	"errors"
	"testing"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/controller"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const capabilityAccessLabel = "managed-owner-access"

// Administrative capability eligibility must not expand DatabaseRole Ready into
// a gate on operations that already own their SQL objects.
func TestManagedOwnerCapabilityDoesNotAddReadyGateForUnchangedOwner(t *testing.T) {
	db := ownershipDatabase(testOrdersNamespace, "unchanged-owner", "unchanged-owner-uid")
	role := &epco.DatabaseRole{Namespace: testBillingNamespace, Name: "owner-manager", UID: types.UID("owner-manager-uid"), Generation: 4,
		Spec:   epco.DatabaseRoleSpec{Cluster: db.Spec.Cluster, Name: db.Spec.Owner, ManagementPolicy: epco.ManagementManage},
		Status: epco.DatabaseRoleStatus{HasManagedSQL: true, Conditions: []metav1.Condition{{Type: testReadyCondition, Status: metav1.ConditionFalse, Reason: postgres.CredentialsMissing, Message: "unrelated password failure", ObservedGeneration: 3}}}}
	r, cl, session := databaseFixture(t, db, role, &corev1.Namespace{Name: role.Namespace})
	runDatabase(t, r, db)
	requireDatabaseCondition(t, currentDatabase(t, cl, db), metav1.ConditionTrue, testReadyCondition)
	if session.applies != 1 {
		t.Fatalf("unchanged owned database applies = %d, want 1 despite unrelated role password failure", session.applies)
	}
}

func TestManagedOwnerCapabilityPreservesDesiredOwnerReadyGate(t *testing.T) {
	db := ownershipDatabase(testOrdersNamespace, "new-owner", "new-owner-uid")
	role := &epco.DatabaseRole{Namespace: testBillingNamespace, Name: "owner-manager", Generation: 4,
		Spec:   epco.DatabaseRoleSpec{Cluster: db.Spec.Cluster, Name: db.Spec.Owner, ManagementPolicy: epco.ManagementManage},
		Status: epco.DatabaseRoleStatus{HasManagedSQL: true, Conditions: []metav1.Condition{{Type: testReadyCondition, Status: metav1.ConditionFalse, Reason: postgres.CredentialsMissing, Message: "password pending", ObservedGeneration: 4}}}}
	r, cl, session := databaseFixture(t, db, role, &corev1.Namespace{Name: role.Namespace})
	session.state.Owner = "old_sql_owner"
	runDatabase(t, r, db)
	requireDatabaseCondition(t, currentDatabase(t, cl, db), metav1.ConditionFalse, "DependenciesNotReady")
	if session.applies != 0 {
		t.Fatalf("owner change applied %d times before desired owner Ready", session.applies)
	}
}

func TestManagedOwnerCapabilityDeletionUsesActualOwnerDespiteDeletingRole(t *testing.T) {
	db := ownershipDatabase(testOrdersNamespace, "delete-owned", "delete-owned-uid")
	db.Spec.ReclaimPolicy = epco.ReclaimDelete
	db.Spec.Owner = "unready_desired_owner"
	db.Status.HasManagedSQL = true
	db.Finalizers = []string{controller.DatabaseFinalizer}
	db.DeletionTimestamp = new(metav1.Now())
	role := &epco.DatabaseRole{Namespace: testBillingNamespace, Name: "actual-owner-manager", UID: types.UID("actual-owner-uid"), Generation: 4,
		Finalizers: []string{controller.RoleFinalizer}, DeletionTimestamp: new(metav1.Now()),
		Spec:   epco.DatabaseRoleSpec{Cluster: db.Spec.Cluster, Name: dbTestOwnerName, ManagementPolicy: epco.ManagementManage},
		Status: epco.DatabaseRoleStatus{HasManagedSQL: true}}
	r, cl, session := databaseFixture(t, db, role, &corev1.Namespace{Name: role.Namespace})
	runDatabase(t, r, db)
	if session.drops != 1 {
		t.Fatalf("DROP count = %d, want 1 for actual owner managed by deleting role", session.drops)
	}
	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(db), &epco.Database{}); !apierrors.IsNotFound(err) {
		t.Fatalf("completed reclaim must release Database finalizer, got %v", err)
	}
}

// Capture the options actually delivered through the regular controller factory
// path, including the separate reclaim DROP path.
type capabilityOptionsFactory struct {
	session postgres.DatabaseSession
	options postgres.DatabaseSessionOptions
	opens   int
}

func (f *capabilityOptionsFactory) Open(_ context.Context, _ *postgres.Config, options postgres.DatabaseSessionOptions) (postgres.DatabaseSession, error) {
	f.options = options
	f.opens++
	return f.session, nil
}

type capabilityAuthoritativeReader struct {
	client.Reader
	t *testing.T
}

func (r capabilityAuthoritativeReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	switch object.(type) {
	case *epco.DatabaseRole, *corev1.Namespace:
		r.t.Errorf("eligibility used APIReader.Get for %T instead of the manager cache", object)
		return errors.New("unexpected direct capability read")
	}
	return r.Reader.Get(ctx, key, object, options...)
}

func (r capabilityAuthoritativeReader) List(ctx context.Context, objects client.ObjectList, options ...client.ListOption) error {
	switch objects.(type) {
	case *epco.DatabaseRoleList, *corev1.NamespaceList:
		r.t.Errorf("eligibility used APIReader.List for %T instead of the manager cache", objects)
		return errors.New("unexpected direct capability list")
	}
	return r.Reader.List(ctx, objects, options...)
}

func capabilityManager(db *epco.Database) *epco.DatabaseRole {
	return &epco.DatabaseRole{Namespace: testBillingNamespace, Name: "recorded-owner-manager", UID: types.UID("recorded-owner-manager-uid"), Generation: 7,
		Spec:   epco.DatabaseRoleSpec{Cluster: db.Spec.Cluster, Name: db.Spec.Owner, ManagementPolicy: epco.ManagementManage},
		Status: epco.DatabaseRoleStatus{HasManagedSQL: true, Conditions: []metav1.Condition{{Type: testReadyCondition, Status: metav1.ConditionFalse, Reason: postgres.CredentialsMissing, Message: "unrelated password failure", ObservedGeneration: 6}}}}
}

func TestManagedOwnerCapabilityEligibilityUsesRecordedCurrentManager(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*epco.Database, *epco.DatabaseRole) []*epco.DatabaseRole
		purpose  postgres.RoleCapabilityPurpose
		eligible bool
		reason   string
	}{
		{name: "recorded manager needs no Ready or current generation gate", eligible: true},
		{name: "effective name defaults to metadata name", eligible: true, mutate: func(db *epco.Database, role *epco.DatabaseRole) []*epco.DatabaseRole {
			role.Name = string(db.Spec.Owner)
			role.Spec.Name = ""
			return nil
		}},
		{name: "implicit local reference has the same canonical key", eligible: true, mutate: func(_ *epco.Database, role *epco.DatabaseRole) []*epco.DatabaseRole {
			role.Namespace = testInfraNamespace
			role.Spec.Cluster.Namespace = ""
			return nil
		}},
		{name: "pure observed role", mutate: func(_ *epco.Database, role *epco.DatabaseRole) []*epco.DatabaseRole {
			role.Spec.ManagementPolicy = epco.ManagementObserve
			role.Status.HasManagedSQL = false
			return nil
		}},
		{name: "former manager is currently observe", mutate: func(_ *epco.Database, role *epco.DatabaseRole) []*epco.DatabaseRole {
			role.Spec.ManagementPolicy = epco.ManagementObserve
			return nil
		}},
		{name: "same name on another canonical descriptor", mutate: func(_ *epco.Database, role *epco.DatabaseRole) []*epco.DatabaseRole {
			role.Spec.Cluster.Name = "other-descriptor"
			return nil
		}},
		{name: "same descriptor name in another namespace", mutate: func(_ *epco.Database, role *epco.DatabaseRole) []*epco.DatabaseRole {
			role.Spec.Cluster.Namespace = "different-infra"
			return nil
		}},
		{name: "different SQL name", mutate: func(_ *epco.Database, role *epco.DatabaseRole) []*epco.DatabaseRole {
			role.Spec.Name = "another_sql_role"
			return nil
		}},
		{name: "SQL existence does not record management", reason: postgres.DependenciesNotReady, mutate: func(_ *epco.Database, role *epco.DatabaseRole) []*epco.DatabaseRole {
			role.Status.HasManagedSQL = false
			return nil
		}},
		{name: "recorded manager wins over unreconciled contender", eligible: true, mutate: func(_ *epco.Database, role *epco.DatabaseRole) []*epco.DatabaseRole {
			contender := role.DeepCopy()
			contender.Name = "losing-manager"
			contender.UID = types.UID("losing-manager-uid")
			contender.Status.HasManagedSQL = false
			return []*epco.DatabaseRole{contender}
		}},
		{name: "two recorded managers cannot authorize acquisition", reason: postgres.OwnershipConflict, mutate: func(_ *epco.Database, role *epco.DatabaseRole) []*epco.DatabaseRole {
			contender := role.DeepCopy()
			contender.Name = "competing-recorded-manager"
			contender.UID = types.UID("competing-recorded-manager-uid")
			return []*epco.DatabaseRole{contender}
		}},
		{name: "deleting manager cannot authorize provisioning", mutate: func(_ *epco.Database, role *epco.DatabaseRole) []*epco.DatabaseRole {
			role.DeletionTimestamp = new(metav1.Now())
			role.Finalizers = []string{controller.RoleFinalizer}
			return nil
		}},
		{name: "deleting recorded manager authorizes historical cleanup", eligible: true, purpose: postgres.RoleCapabilityCleanup, mutate: func(_ *epco.Database, role *epco.DatabaseRole) []*epco.DatabaseRole {
			role.DeletionTimestamp = new(metav1.Now())
			role.Finalizers = []string{controller.RoleFinalizer}
			return nil
		}},
		{name: "ordinary historical cleanup", eligible: true, purpose: postgres.RoleCapabilityCleanup},
		{name: "observe Database cannot acquire capability", mutate: func(db *epco.Database, _ *epco.DatabaseRole) []*epco.DatabaseRole {
			db.Spec.ManagementPolicy = epco.ManagementObserve
			return nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := ownershipDatabase(testOrdersNamespace, "resolver-database", "resolver-database-uid")
			role := capabilityManager(db)
			var extra []*epco.DatabaseRole
			if tc.mutate != nil {
				extra = tc.mutate(db, role)
			}
			objects := []client.Object{role, &corev1.Namespace{Name: role.Namespace}}
			for _, candidate := range extra {
				objects = append(objects, candidate)
			}
			r, cl, session := databaseFixture(t, db, objects...)
			r.APIReader = capabilityAuthoritativeReader{Reader: cl, t: t}
			factory := &capabilityOptionsFactory{session: session}
			r.Databases = factory
			runDatabase(t, r, db)
			if factory.opens != 1 || factory.options.ResolveRoleCapability == nil {
				t.Fatalf("regular Database session did not receive a role capability resolver: opens=%d", factory.opens)
			}
			purpose := tc.purpose
			if purpose == "" {
				purpose = postgres.RoleCapabilityProvision
			}
			eligible, err := factory.options.ResolveRoleCapability(t.Context(), string(db.Spec.Owner), purpose)
			if eligible != tc.eligible {
				t.Fatalf("capability eligibility = %t, want %t (error %v)", eligible, tc.eligible, err)
			}
			if tc.reason == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			failure, ok := errors.AsType[*postgres.Failure](err)
			if !ok || failure.Reason != tc.reason {
				t.Fatalf("eligibility error = %v, want %s", err, tc.reason)
			}
		})
	}
}

func TestManagedOwnerCapabilityRequiresBothNamespacesAndResumes(t *testing.T) {
	db := ownershipDatabase(testOrdersNamespace, "authorization-database", "authorization-database-uid")
	role := capabilityManager(db)
	r, cl, session := databaseFixture(t, db, role, &corev1.Namespace{Name: role.Namespace})
	cluster := &epco.PostgresCluster{}
	if err := cl.Get(t.Context(), client.ObjectKey{Namespace: testInfraNamespace, Name: testDescriptorName}, cluster); err != nil {
		t.Fatal(err)
	}
	cluster.Spec.Access.NamespaceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{capabilityAccessLabel: testAllowedLabel}}
	if err := cl.Update(t.Context(), cluster); err != nil {
		t.Fatal(err)
	}
	databaseNamespace := &corev1.Namespace{}
	if err := cl.Get(t.Context(), client.ObjectKey{Name: db.Namespace}, databaseNamespace); err != nil {
		t.Fatal(err)
	}
	databaseNamespace.Labels = map[string]string{capabilityAccessLabel: testAllowedLabel}
	if err := cl.Update(t.Context(), databaseNamespace); err != nil {
		t.Fatal(err)
	}
	r.APIReader = capabilityAuthoritativeReader{Reader: cl, t: t}
	factory := &capabilityOptionsFactory{session: session}
	r.Databases = factory
	runDatabase(t, r, db)
	if factory.options.ResolveRoleCapability == nil {
		t.Fatal("Database session did not receive resolver")
	}
	eligible, err := factory.options.ResolveRoleCapability(t.Context(), string(db.Spec.Owner), postgres.RoleCapabilityProvision)
	failure, ok := errors.AsType[*postgres.Failure](err)
	if eligible || !ok || failure.Reason != "AccessDenied" {
		t.Fatalf("authorized Database bypassed unauthorized role Namespace: eligible=%t err=%v", eligible, err)
	}

	roleNamespace := &corev1.Namespace{}
	if err = cl.Get(t.Context(), client.ObjectKey{Name: role.Namespace}, roleNamespace); err != nil {
		t.Fatal(err)
	}
	roleNamespace.Labels = map[string]string{capabilityAccessLabel: testAllowedLabel}
	if err = cl.Update(t.Context(), roleNamespace); err != nil {
		t.Fatal(err)
	}
	runDatabase(t, r, db)
	eligible, err = factory.options.ResolveRoleCapability(t.Context(), string(db.Spec.Owner), postgres.RoleCapabilityProvision)
	if !eligible || err != nil {
		t.Fatalf("role Namespace restoration did not resume eligibility: eligible=%t err=%v", eligible, err)
	}

	databaseNamespace.Labels = nil
	if err = cl.Update(t.Context(), databaseNamespace); err != nil {
		t.Fatal(err)
	}
	before := factory.opens
	runDatabase(t, r, db)
	if factory.opens != before {
		t.Fatal("unauthorized Database Namespace opened a SQL session")
	}
	requireDatabaseCondition(t, currentDatabase(t, cl, db), metav1.ConditionFalse, "AccessDenied")
}

func TestManagedOwnerCapabilityResolverIsAttachedToActualOwnerDrop(t *testing.T) {
	db := ownershipDatabase(testOrdersNamespace, "drop-resolver", "drop-resolver-uid")
	db.Spec.Owner = "irrelevant_desired_owner"
	db.Spec.ReclaimPolicy = epco.ReclaimDelete
	db.Status.HasManagedSQL = true
	db.Finalizers = []string{controller.DatabaseFinalizer}
	db.DeletionTimestamp = new(metav1.Now())
	role := capabilityManager(db)
	role.Spec.Name = dbTestOwnerName
	role.Finalizers = []string{controller.RoleFinalizer}
	role.DeletionTimestamp = new(metav1.Now())
	r, cl, session := databaseFixture(t, db, role, &corev1.Namespace{Name: role.Namespace})
	r.APIReader = capabilityAuthoritativeReader{Reader: cl, t: t}
	factory := &capabilityOptionsFactory{session: session}
	r.Databases = factory
	session.drop = func(ctx context.Context, _ string) error {
		if factory.options.ResolveRoleCapability == nil {
			t.Fatal("direct Drop session lost capability resolver")
		}
		eligible, err := factory.options.ResolveRoleCapability(ctx, session.state.Owner, postgres.RoleCapabilityCleanup)
		if !eligible || err != nil {
			t.Fatalf("actual owner Drop cannot use deleting recorded manager: eligible=%t err=%v", eligible, err)
		}
		return nil
	}
	runDatabase(t, r, db)
	if session.drops != 1 {
		t.Fatalf("direct DROP count=%d, want 1", session.drops)
	}
}

type capabilityACLSession struct {
	*databaseSessionStub
	factory              *capabilityOptionsFactory
	prepares, reconciles int
	t                    *testing.T
}

func (*capabilityACLSession) PreflightACL(context.Context, postgres.DatabaseDesired) error {
	return nil
}

func (s *capabilityACLSession) PrepareACL(ctx context.Context, desired postgres.ACLDesired) (*postgres.ACLResult, error) {
	s.prepares++
	if s.factory.options.ResolveRoleCapability == nil {
		s.t.Fatal("ACL preparation session lost capability resolver")
	}
	if len(desired.Targets) != 1 || len(desired.Targets[0].Bindings) != 1 {
		s.t.Fatal("historical ACL binding did not reach preparation")
	}
	historical := desired.Targets[0].Bindings[0]
	for _, name := range []string{historical.Owner, historical.Grantor, historical.Creator} {
		eligible, err := s.factory.options.ResolveRoleCapability(ctx, name, postgres.RoleCapabilityCleanup)
		if !eligible || err != nil {
			s.t.Fatalf("historical ACL role %q requires unrelated Ready status: eligible=%t err=%v", name, eligible, err)
		}
	}
	return &postgres.ACLResult{Targets: desired.Targets}, nil
}

func (s *capabilityACLSession) ReconcileACL(_ context.Context, desired postgres.ACLDesired) (*postgres.ACLResult, error) {
	s.reconciles++
	return &postgres.ACLResult{Targets: desired.Targets}, nil
}

func TestManagedOwnerCapabilityResolverReachesHistoricalACLPreparation(t *testing.T) {
	db := ownershipDatabase(testOrdersNamespace, "acl-resolver", "acl-resolver-uid")
	historicalName := "historical_acl_creator"
	db.Status.ManagedACLTargets = []epco.ManagedACLTarget{{Host: testEndpointHost, Port: 5432, Database: testSharedSQLName,
		Bindings: []epco.ACLBinding{{Owner: historicalName, Creator: historicalName, Grantor: historicalName}}}}
	role := capabilityManager(db)
	role.Spec.Name = epco.SQLName(historicalName)
	role.DeletionTimestamp = new(metav1.Now())
	role.Finalizers = []string{controller.RoleFinalizer}
	r, cl, base := databaseFixture(t, db, role, &corev1.Namespace{Name: role.Namespace})
	r.APIReader = capabilityAuthoritativeReader{Reader: cl, t: t}
	factory := &capabilityOptionsFactory{}
	session := &capabilityACLSession{databaseSessionStub: base, factory: factory, t: t}
	factory.session = session
	r.Databases = factory
	runDatabase(t, r, db)
	if session.prepares != 1 || session.reconciles != 1 {
		t.Fatalf("ACL preparation/reconcile = %d/%d, want 1/1", session.prepares, session.reconciles)
	}
	requireDatabaseCondition(t, currentDatabase(t, cl, db), metav1.ConditionTrue, testReadyCondition)
}
