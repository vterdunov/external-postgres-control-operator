package controller_test

import (
	"context"
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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	dbTestOwnerName            = "owner"
	dbTestTLSDisable           = "disable"
	dbTestSuccessCase          = "success"
	dbTestUnexpectedSchemaCase = "absent schema exists"
	dbTestApplicationName      = "app"
	dbTestAbsentCase           = "absent"
	dbTestFinalizerCase        = "finalizer"
	dbTestCatalogSchema        = "pg_catalog"
	dbTestOtherDescriptorCase  = "different descriptor"
	dbTestPreviousOwner        = "previous"
	dbTestOpenStage            = "open"
	dbTestSystemObserveCase    = "system observe"
	dbTestAbsentCatalogCase    = "actual missing"
	dbTestMissingOwner         = "missing"
	dbTestUnrelatedSecretKey   = "unrelated"
	dbTestSecondEndpoint       = "second.example"
	dbTestAdminDatabase        = "maintenance"
	dbTestUnmanagedCase        = "never managed"
	dbTestRetainCase           = "retain"
	dbTestBlockedDropCase      = "blocked drop"
	dbTestRotatedCredential    = "rotated-input"
	dbTestMissingDatabaseCase  = "database missing"
	dbTestMissingSchemaCase    = "schema missing"
	dbTestFormerManagerCase    = "former manager"
	dbTestObserveCase          = "observe"
	dbTestPresentCase          = "present"
	dbTestAbsentSchema         = "gone"
	dbTestResourceUID          = "uid"
	dbTestObservedReason       = "Observed"
)

type databaseSessionStub struct {
	state                  *postgres.DatabaseState
	schemas                []postgres.SchemaState
	inspect                func(context.Context, string) (*postgres.DatabaseState, error)
	inspectSchemas         func(context.Context, string, []epco.SchemaSpec) ([]postgres.SchemaState, error)
	apply                  func(context.Context, postgres.DatabaseDesired) (*postgres.DatabaseApplyResult, error)
	drop                   func(context.Context, string) error
	applies, drops, closed int
}

func (s *databaseSessionStub) Inspect(ctx context.Context, name string) (*postgres.DatabaseState, error) {
	if s.inspect != nil {
		return s.inspect(ctx, name)
	}
	return s.state, nil
}
func (s *databaseSessionStub) InspectSchemas(ctx context.Context, name string, spec []epco.SchemaSpec) ([]postgres.SchemaState, error) {
	if s.inspectSchemas != nil {
		return s.inspectSchemas(ctx, name, spec)
	}
	return s.schemas, nil
}
func (s *databaseSessionStub) Apply(ctx context.Context, d postgres.DatabaseDesired) (*postgres.DatabaseApplyResult, error) {
	s.applies++
	if s.apply != nil {
		return s.apply(ctx, d)
	}
	return &postgres.DatabaseApplyResult{}, nil
}
func (s *databaseSessionStub) Drop(ctx context.Context, name string) error {
	s.drops++
	if s.drop != nil {
		return s.drop(ctx, name)
	}
	return nil
}
func (s *databaseSessionStub) Close(context.Context) error { s.closed++; return nil }

type databaseFactoryFunc func(context.Context, *postgres.Config) (postgres.DatabaseSession, error)

func (f databaseFactoryFunc) Open(ctx context.Context, c *postgres.Config) (postgres.DatabaseSession, error) {
	return f(ctx, c)
}
func ownershipDatabase(namespace, name, uid string) *epco.Database {
	return &epco.Database{Namespace: namespace, Name: name, UID: types.UID(uid), Generation: 2, Spec: epco.DatabaseSpec{Cluster: epco.ClusterReference{Namespace: testInfraNamespace, Name: testDescriptorName}, Name: testSharedSQLName, Owner: dbTestOwnerName, ManagementPolicy: epco.ManagementManage, Ensure: epco.EnsurePresent, ReclaimPolicy: epco.ReclaimRetain}}
}
func databaseClient(t *testing.T, objects ...client.Object) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := epco.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&epco.Database{}, &epco.DatabaseRole{}).WithObjects(objects...).Build()
}
func databaseFixture(t *testing.T, db *epco.Database, objects ...client.Object) (*controller.DatabaseReconciler, client.WithWatch, *databaseSessionStub) {
	t.Helper()
	cluster := &epco.PostgresCluster{Namespace: testInfraNamespace, Name: testDescriptorName, Spec: epco.PostgresClusterSpec{Connection: epco.PostgresConnection{Host: testEndpointHost, Database: dbTestAdminDatabase}, CredentialsSecret: epco.LocalReference{Name: testAdminName}, TLS: &epco.PostgresTLS{Mode: dbTestTLSDisable}, Access: &epco.ClusterAccess{NamespaceSelector: &metav1.LabelSelector{}}}}
	admin := &corev1.Secret{Namespace: testInfraNamespace, Name: testAdminName, Type: corev1.SecretTypeBasicAuth, Data: map[string][]byte{corev1.BasicAuthUsernameKey: []byte(testAdminName), corev1.BasicAuthPasswordKey: []byte("administrative-credential")}}
	all := append([]client.Object{db, cluster, admin, &corev1.Namespace{Name: db.Namespace}}, objects...)
	cl := databaseClient(t, all...)
	builder, err := postgres.NewConfigBuilder()
	if err != nil {
		t.Fatal(err)
	}
	s := &databaseSessionStub{state: &postgres.DatabaseState{Exists: true, Name: testSharedSQLName, Owner: dbTestOwnerName, AdminUser: testAdminName, AdminDatabase: dbTestAdminDatabase, ServerVersion: 180000, MaxIdentifierLength: 63, AllowConnections: true, ConnectionLimit: -1}}
	r := &controller.DatabaseReconciler{Client: cl, APIReader: cl, Builder: builder, Databases: databaseFactoryFunc(func(context.Context, *postgres.Config) (postgres.DatabaseSession, error) { return s, nil }), Locks: &controller.ManagementLocks{}, Now: func() time.Time { return time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC) }}
	return r, cl, s
}
func runDatabase(t *testing.T, r *controller.DatabaseReconciler, db *epco.Database) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(db)})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func currentDatabase(t *testing.T, cl client.Reader, db *epco.Database) *epco.Database {
	t.Helper()
	latest := &epco.Database{}
	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(db), latest); err != nil {
		t.Fatal(err)
	}
	return latest
}
func requireDatabaseCondition(t *testing.T, db *epco.Database, status metav1.ConditionStatus, reason string) {
	t.Helper()
	c := meta.FindStatusCondition(db.Status.Conditions, testReadyCondition)
	if c == nil || c.Status != status || c.Reason != reason || c.ObservedGeneration != db.Generation {
		t.Fatalf("Ready=%+v want %s/%s current generation", c, status, reason)
	}
}
func updateDatabase(t *testing.T, cl client.Client, db *epco.Database, change func(*epco.Database)) *epco.Database {
	t.Helper()
	latest := currentDatabase(t, cl, db)
	change(latest)
	latest.Generation++
	if err := cl.Update(t.Context(), latest); err != nil {
		t.Fatal(err)
	}
	return latest
}
func noDatabaseMutation(t *testing.T, s *databaseSessionStub) {
	t.Helper()
	if s.applies != 0 || s.drops != 0 {
		t.Fatalf("unexpected SQL mutation apply=%d drop=%d", s.applies, s.drops)
	}
}

func TestDatabaseSuccessRecordsOnlyCompleteConfirmedManagement(t *testing.T) {
	for _, mode := range []string{dbTestSuccessCase, "failure", "unconfirmed"} {
		t.Run(mode, func(t *testing.T) {
			db := ownershipDatabase(testOrdersNamespace, dbTestApplicationName, dbTestResourceUID)
			db.Spec.Name = ""
			r, cl, s := databaseFixture(t, db)
			s.apply = func(_ context.Context, d postgres.DatabaseDesired) (*postgres.DatabaseApplyResult, error) {
				if d.Name != db.Name || !reflect.DeepEqual(d.Spec, db.Spec) {
					t.Fatal("effective SQL name or requested spec lost")
				}
				switch mode {
				case "failure":
					return nil, &postgres.Failure{Reason: postgres.InsufficientPrivileges, Message: "safe"}
				case "unconfirmed":
					return nil, nil
				}
				return &postgres.DatabaseApplyResult{}, nil
			}
			runDatabase(t, r, db)
			latest := currentDatabase(t, cl, db)
			if latest.Status.EffectiveName != db.Name || latest.Status.ObservedGeneration != 2 {
				t.Fatal("effective name/generation missing")
			}
			if mode == dbTestSuccessCase {
				requireDatabaseCondition(t, latest, metav1.ConditionTrue, testReadyCondition)
				if !latest.Status.HasManagedSQL || latest.Status.Applied == nil || !*latest.Status.Applied || latest.Status.AppliedGeneration != 2 || latest.Status.LastSuccessfulReconcileTime == nil {
					t.Fatal("complete SQL success not recorded")
				}
			} else {
				if latest.Status.HasManagedSQL || latest.Status.AppliedGeneration != 0 || latest.Status.LastSuccessfulReconcileTime != nil {
					t.Fatal("partial SQL acquired recorded management")
				}
				if latest.Status.Applied == nil || *latest.Status.Applied {
					t.Fatal("failed SQL marked applied")
				}
			}
			if s.closed != 1 {
				t.Fatal("session leaked")
			}
		})
	}
}

func TestDatabaseObservationChecksListedSchemasAndNeverAcquiresManagement(t *testing.T) {
	for _, variation := range []string{dbTestPresentCase, dbTestMissingDatabaseCase, dbTestMissingSchemaCase, dbTestUnexpectedSchemaCase, dbTestFormerManagerCase} {
		t.Run(variation, func(t *testing.T) {
			db := ownershipDatabase(testOrdersNamespace, dbTestApplicationName, dbTestResourceUID)
			db.Spec.ManagementPolicy = epco.ManagementObserve
			db.Spec.Schemas = []epco.SchemaSpec{{Name: dbTestApplicationName}, {Name: dbTestAbsentSchema, Ensure: epco.EnsureAbsent}}
			db.Status.HasManagedSQL = variation == dbTestFormerManagerCase
			r, cl, s := databaseFixture(t, db)
			s.schemas = []postgres.SchemaState{{Name: dbTestApplicationName, Exists: true, Owner: "independent"}, {Name: dbTestAbsentSchema, Exists: false}}
			if variation == dbTestMissingDatabaseCase {
				s.state.Exists = false
			}
			if variation == dbTestMissingSchemaCase {
				s.schemas[0].Exists = false
			}
			if variation == dbTestUnexpectedSchemaCase {
				s.schemas[1].Exists = true
			}
			runDatabase(t, r, db)
			latest := currentDatabase(t, cl, db)
			noDatabaseMutation(t, s)
			if latest.Status.HasManagedSQL != (variation == dbTestFormerManagerCase) || latest.Status.Applied != nil || latest.Status.AppliedGeneration != 0 {
				t.Fatal("observation changed management/applied history")
			}
			c := meta.FindStatusCondition(latest.Status.Conditions, testReadyCondition)
			want := metav1.ConditionTrue
			if variation == dbTestMissingDatabaseCase || variation == dbTestMissingSchemaCase || variation == dbTestUnexpectedSchemaCase {
				want = metav1.ConditionFalse
			}
			if c == nil || c.Status != want {
				t.Fatalf("observed state Ready=%+v want %s", c, want)
			}
			if variation == dbTestPresentCase && len(latest.Finalizers) != 0 {
				t.Fatal("pure observer acquired finalizer")
			}
		})
	}
}

func TestDatabaseUnsupportedDeclarationsBlockEveryNormalPath(t *testing.T) {
	cases := map[string]func(*epco.DatabaseSpec){"database ACL": func(s *epco.DatabaseSpec) { s.Permissions = &epco.DatabasePermissions{} }, "global defaults": func(s *epco.DatabaseSpec) { s.DefaultPrivileges = []epco.DefaultPrivileges{{ForRole: dbTestOwnerName}} }, "schema ACL": func(s *epco.DatabaseSpec) {
		s.Schemas = []epco.SchemaSpec{{Name: dbTestApplicationName, Permissions: &epco.SchemaPermissions{}}}
	}, "schema defaults": func(s *epco.DatabaseSpec) {
		s.Schemas = []epco.SchemaSpec{{Name: dbTestApplicationName, DefaultPrivileges: []epco.DefaultPrivileges{{ForRole: dbTestOwnerName}}}}
	}, "named table ACL": func(s *epco.DatabaseSpec) {
		s.Schemas = []epco.SchemaSpec{{Name: dbTestApplicationName, TablePermissions: []epco.NamedTablePermissions{{Name: "table"}}}}
	}, "FDWs": func(s *epco.DatabaseSpec) { s.FDWs = []epco.FDWSpec{{Name: "wrapper"}} }, "servers": func(s *epco.DatabaseSpec) { s.Servers = []epco.ServerSpec{{Name: "foreign", FDW: "wrapper"}} }}
	for name, change := range cases {
		for _, policy := range []string{dbTestPresentCase, dbTestObserveCase, dbTestAbsentCase} {
			t.Run(name+"/"+policy, func(t *testing.T) {
				db := ownershipDatabase(testOrdersNamespace, dbTestApplicationName, dbTestResourceUID)
				db.Status.HasManagedSQL = true
				change(&db.Spec)
				if policy == dbTestObserveCase {
					db.Spec.ManagementPolicy = epco.ManagementObserve
				}
				if policy == dbTestAbsentCase {
					db.Spec.Ensure = epco.EnsureAbsent
				}
				r, cl, s := databaseFixture(t, db)
				runDatabase(t, r, db)
				latest := currentDatabase(t, cl, db)
				requireDatabaseCondition(t, latest, metav1.ConditionFalse, "UnsupportedFeature")
				noDatabaseMutation(t, s)
				if !latest.Status.HasManagedSQL {
					t.Fatal("unsupported request erased sticky marker")
				}
				secrets := &corev1.SecretList{}
				if err := cl.List(t.Context(), secrets, client.InNamespace(db.Namespace)); err != nil {
					t.Fatal(err)
				}
				if len(secrets.Items) != 0 {
					t.Fatal("unsupported export created application Secrets")
				}
			})
		}
	}
}

func TestDatabaseProtectedObjectsPreventPartialMutation(t *testing.T) {
	for _, name := range []string{"postgres", "template0", "template1", dbTestAdminDatabase} {
		for _, policy := range []string{dbTestPresentCase, dbTestAbsentCase, dbTestObserveCase, dbTestFinalizerCase} {
			t.Run(name+"/"+policy, func(t *testing.T) {
				db := ownershipDatabase(testOrdersNamespace, dbTestApplicationName, dbTestResourceUID)
				db.Spec.Name = epco.SQLName(name)
				if policy == dbTestObserveCase {
					db.Spec.ManagementPolicy = epco.ManagementObserve
				}
				if policy == dbTestAbsentCase {
					db.Spec.Ensure = epco.EnsureAbsent
				}
				if policy == dbTestFinalizerCase {
					db.Spec.ReclaimPolicy = epco.ReclaimDelete
					db.Status.HasManagedSQL = true
					db.Finalizers = []string{controller.DatabaseFinalizer}
					db.DeletionTimestamp = new(metav1.Now())
				}
				r, cl, s := databaseFixture(t, db)
				runDatabase(t, r, db)
				noDatabaseMutation(t, s)
				latest := currentDatabase(t, cl, db)
				if policy == dbTestObserveCase {
					requireDatabaseCondition(t, latest, metav1.ConditionTrue, dbTestObservedReason)
				} else {
					requireDatabaseCondition(t, latest, metav1.ConditionFalse, "ProtectedDatabase")
				}
				if policy == dbTestFinalizerCase && !slices.Contains(latest.Finalizers, controller.DatabaseFinalizer) {
					t.Fatal("protected current target released cleanup finalizer")
				}
			})
		}
	}
	for _, name := range []string{dbTestCatalogSchema, "information_schema", "pg_internal"} {
		t.Run("schema/"+name, func(t *testing.T) {
			db := ownershipDatabase(testOrdersNamespace, dbTestApplicationName, dbTestResourceUID)
			db.Spec.Schemas = []epco.SchemaSpec{{Name: "ordinary"}, {Name: epco.SQLName(name)}}
			r, cl, s := databaseFixture(t, db)
			runDatabase(t, r, db)
			noDatabaseMutation(t, s)
			requireDatabaseCondition(t, currentDatabase(t, cl, db), metav1.ConditionFalse, "ProtectedSchema")
		})
	}
}

func TestDatabaseAccessDenialNeverConnectsAndPreservesPendingFinalizer(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: dbTestFinalizerCase}[deleting], func(t *testing.T) {
			db := ownershipDatabase(testOrdersNamespace, dbTestApplicationName, dbTestResourceUID)
			if deleting {
				db.Spec.ReclaimPolicy = epco.ReclaimDelete
				db.Status.HasManagedSQL = true
				db.Finalizers = []string{controller.DatabaseFinalizer}
				db.DeletionTimestamp = new(metav1.Now())
			}
			r, cl, s := databaseFixture(t, db)
			cluster := &epco.PostgresCluster{}
			if err := cl.Get(t.Context(), client.ObjectKey{Namespace: testInfraNamespace, Name: testDescriptorName}, cluster); err != nil {
				t.Fatal(err)
			}
			cluster.Spec.Access = &epco.ClusterAccess{}
			if err := cl.Update(t.Context(), cluster); err != nil {
				t.Fatal(err)
			}
			r.Databases = databaseFactoryFunc(func(context.Context, *postgres.Config) (postgres.DatabaseSession, error) {
				t.Error("unauthorized Database connected")
				return s, nil
			})
			runDatabase(t, r, db)
			latest := currentDatabase(t, cl, db)
			requireDatabaseCondition(t, latest, metav1.ConditionFalse, testAccessDeniedReason)
			if deleting && !slices.Contains(latest.Finalizers, controller.DatabaseFinalizer) {
				t.Fatal("authorization denial released finalizer")
			}
			if !deleting && len(latest.Finalizers) != 0 {
				t.Fatal("initial denied CR acquired finalizer")
			}
		})
	}
}

func TestDatabaseOwnershipUsesStickyMarkerAuthoritativeScopeAndUID(t *testing.T) {
	for _, variation := range []string{"recorded", "former observer", connectionExplicitAbsentCase, "deleting", "pure observer", "generation only", "self UID", dbTestOtherDescriptorCase, "different SQL"} {
		t.Run(variation, func(t *testing.T) {
			db := ownershipDatabase(testOrdersNamespace, "requester", "requester")
			other := ownershipDatabase(testBillingNamespace, "manager", testOtherName)
			other.Status.HasManagedSQL = true
			conflict := true
			switch variation {
			case "former observer":
				other.Spec.ManagementPolicy = epco.ManagementObserve
			case connectionExplicitAbsentCase:
				other.Spec.Ensure = epco.EnsureAbsent
			case "deleting":
				other.DeletionTimestamp = new(metav1.Now())
				other.Finalizers = []string{controller.DatabaseFinalizer}
			case "pure observer":
				other.Spec.ManagementPolicy = epco.ManagementObserve
				other.Status.HasManagedSQL = false
				conflict = false
			case "generation only":
				other.Status.HasManagedSQL = false
				other.Status.ObservedGeneration = 2
				conflict = false
			case "self UID":
				other.UID = db.UID
				conflict = false
			case dbTestOtherDescriptorCase:
				other.Spec.Cluster.Name = testOtherName
				conflict = false
			case "different SQL":
				other.Spec.Name = "different"
				conflict = false
			}
			owner, err := controller.FindDatabaseOwnershipConflict(t.Context(), databaseClient(t, other), db)
			if err != nil {
				t.Fatal(err)
			}
			if (owner != nil) != conflict {
				t.Fatalf("conflict=%v want=%v", owner, conflict)
			}
			r, cl, s := databaseFixture(t, db)
			r.APIReader = databaseClient(t, db, other)
			runDatabase(t, r, db)
			if conflict {
				requireDatabaseCondition(t, currentDatabase(t, cl, db), metav1.ConditionFalse, connectionOwnershipConflictReason)
				noDatabaseMutation(t, s)
			} else {
				requireDatabaseCondition(t, currentDatabase(t, cl, db), metav1.ConditionTrue, testReadyCondition)
			}
		})
	}
}

func TestDatabaseObserveToManageRechecksRecordedOwner(t *testing.T) {
	db := ownershipDatabase(testOrdersNamespace, dbTestApplicationName, dbTestResourceUID)
	db.Spec.ManagementPolicy = epco.ManagementObserve
	other := ownershipDatabase(testBillingNamespace, "manager", testOtherName)
	other.Status.HasManagedSQL = true
	r, cl, s := databaseFixture(t, db, other)
	runDatabase(t, r, db)
	requireDatabaseCondition(t, currentDatabase(t, cl, db), metav1.ConditionTrue, dbTestObservedReason)
	db = updateDatabase(t, cl, db, func(d *epco.Database) { d.Spec.ManagementPolicy = epco.ManagementManage })
	runDatabase(t, r, db)
	requireDatabaseCondition(t, currentDatabase(t, cl, db), metav1.ConditionFalse, connectionOwnershipConflictReason)
	noDatabaseMutation(t, s)
}

func TestDatabaseDesiredOwnerDependenciesOnlyGateOperationsNeedingOwner(t *testing.T) {
	for _, mode := range []string{"create", "owner change", "new schema", "explicit schema owner", "unchanged existing", dbTestObserveCase, dbTestAbsentCase, dbTestFinalizerCase, "schema absence", dbTestOtherDescriptorCase, "ready recorded owner", "pure observer owner"} {
		t.Run(mode, func(t *testing.T) {
			db := ownershipDatabase(testOrdersNamespace, dbTestApplicationName, dbTestResourceUID)
			owner := ownershipRole(testBillingNamespace, "managed-owner", "owner-uid")
			owner.Spec.Name = dbTestOwnerName
			owner.Generation = 3
			_, _, s := databaseFixture(t, db, owner)
			blocked := false
			switch mode {
			case "create":
				s.state.Exists = false
				blocked = true
			case "owner change":
				s.state.Owner = dbTestPreviousOwner
				blocked = true
			case "new schema":
				db.Spec.Schemas = []epco.SchemaSpec{{Name: dbTestApplicationName}}
				blocked = true
			case "explicit schema owner":
				db.Spec.Schemas = []epco.SchemaSpec{{Name: dbTestApplicationName, Owner: dbTestOwnerName}}
				s.schemas = []postgres.SchemaState{{Name: dbTestApplicationName, Exists: true, Owner: dbTestPreviousOwner}}
				blocked = true
			case dbTestObserveCase:
				db.Spec.ManagementPolicy = epco.ManagementObserve
			case dbTestAbsentCase:
				db.Spec.Ensure = epco.EnsureAbsent
			case dbTestFinalizerCase:
				db.Spec.ReclaimPolicy = epco.ReclaimDelete
				db.Status.HasManagedSQL = true
				db.Finalizers = []string{controller.DatabaseFinalizer}
				db.DeletionTimestamp = new(metav1.Now())
			case "schema absence":
				db.Spec.Schemas = []epco.SchemaSpec{{Name: dbTestAbsentSchema, Ensure: epco.EnsureAbsent}}
				s.schemas = []postgres.SchemaState{{Name: dbTestAbsentSchema, Exists: true, Owner: dbTestPreviousOwner}}
			case dbTestOtherDescriptorCase:
				owner.Spec.Cluster.Name = testOtherName
				s.state.Exists = false
			case "ready recorded owner":
				owner.Status.HasManagedSQL = true
				owner.Status.Conditions = []metav1.Condition{{Type: testReadyCondition, Status: metav1.ConditionTrue, Reason: testReadyCondition, ObservedGeneration: 3}}
				s.state.Exists = false
			case "pure observer owner":
				owner.Spec.ManagementPolicy = epco.ManagementObserve
				s.state.Exists = false
			} // Store modified inputs without fabricating a reconcile result.
			r, cl, s2 := databaseFixture(t, db, owner)
			s2.state = s.state
			s2.schemas = s.schemas
			s = s2
			runDatabase(t, r, db)
			if blocked {
				noDatabaseMutation(t, s)
				requireDatabaseCondition(t, currentDatabase(t, cl, db), metav1.ConditionFalse, "DependenciesNotReady")
			} else if mode != dbTestFinalizerCase {
				c := meta.FindStatusCondition(currentDatabase(t, cl, db).Status.Conditions, testReadyCondition)
				if c == nil || c.Status != metav1.ConditionTrue {
					t.Fatalf("irrelevant desired-owner dependency blocked %s: %+v", mode, c)
				}
			}
		})
	}
}

func TestDatabaseSQLFailurePreservesMarkerAndClosesSession(t *testing.T) {
	for _, stage := range []string{dbTestOpenStage, "inspect", "schemas", "apply"} {
		t.Run(stage, func(t *testing.T) {
			db := ownershipDatabase(testOrdersNamespace, dbTestApplicationName, dbTestResourceUID)
			db.Status.HasManagedSQL = true
			db.Spec.Schemas = []epco.SchemaSpec{{Name: dbTestApplicationName}}
			r, cl, s := databaseFixture(t, db)
			failure := &postgres.Failure{Reason: postgres.InsufficientPrivileges, Message: "credential must not escape"}
			switch stage {
			case dbTestOpenStage:
				r.Databases = databaseFactoryFunc(func(context.Context, *postgres.Config) (postgres.DatabaseSession, error) { return nil, failure })
			case "inspect":
				s.inspect = func(context.Context, string) (*postgres.DatabaseState, error) { return nil, failure }
			case "schemas":
				s.inspectSchemas = func(context.Context, string, []epco.SchemaSpec) ([]postgres.SchemaState, error) { return nil, failure }
			case "apply":
				s.schemas = []postgres.SchemaState{{Name: dbTestApplicationName, Exists: true}}
				s.apply = func(context.Context, postgres.DatabaseDesired) (*postgres.DatabaseApplyResult, error) {
					return nil, failure
				}
			}
			result := runDatabase(t, r, db)
			latest := currentDatabase(t, cl, db)
			requireDatabaseCondition(t, latest, metav1.ConditionFalse, postgres.InsufficientPrivileges)
			if !latest.Status.HasManagedSQL || latest.Status.LastSuccessfulReconcileTime != nil || result.RequeueAfter <= 0 {
				t.Fatal("failure lost sticky management or retry")
			}
			if latest.Status.Message == failure.Message {
				t.Fatal("SQL diagnostic escaped safe status")
			}
			want := 1
			if stage == dbTestOpenStage {
				want = 0
			}
			if s.closed != want {
				t.Fatal("error leaked session")
			}
		})
	}
}

func TestDatabaseSQLSuccessStatusConflictRetriesOrdinaryCurrentState(t *testing.T) {
	db := ownershipDatabase(testOrdersNamespace, dbTestApplicationName, dbTestResourceUID)
	r, cl, s := databaseFixture(t, db)
	fail := true
	failure := apierrors.NewConflict(schema.GroupResource{Group: epco.SchemeGroupVersion.Group, Resource: "databases"}, db.Name, errors.New("concurrent status"))
	r.Client = interceptor.NewClient(cl, interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		if sub == testStatusSubresource && s.applies > 0 && fail {
			return failure
		}
		return c.SubResource(sub).Update(ctx, obj, opts...)
	}, SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
		if sub == testStatusSubresource && s.applies > 0 && fail {
			return failure
		}
		return c.SubResource(sub).Patch(ctx, obj, p, opts...)
	}})
	s.state.Exists = false
	s.apply = func(context.Context, postgres.DatabaseDesired) (*postgres.DatabaseApplyResult, error) {
		s.state.Exists = true
		return &postgres.DatabaseApplyResult{}, nil
	}
	result := runDatabase(t, r, db)
	if s.applies != 1 || !s.state.Exists || result.RequeueAfter <= 0 {
		t.Fatal("status conflict fixture failed to exercise SQL success")
	}
	if currentDatabase(t, cl, db).Status.HasManagedSQL {
		t.Fatal("failed status unexpectedly persisted management")
	}
	fail = false
	runDatabase(t, r, db)
	latest := currentDatabase(t, cl, db)
	requireDatabaseCondition(t, latest, metav1.ConditionTrue, testReadyCondition)
	if !latest.Status.HasManagedSQL {
		t.Fatal("ordinary retry did not record success")
	}
}

func TestDatabaseRoleAndDatabaseSQLNamesAreIndependent(t *testing.T) {
	db := ownershipDatabase(testOrdersNamespace, "database", "database-uid")
	role := ownershipRole(testBillingNamespace, "role", "role-uid")
	role.Status.HasManagedSQL = true
	r, cl, _ := databaseFixture(t, db, role)
	runDatabase(t, r, db)
	requireDatabaseCondition(t, currentDatabase(t, cl, db), metav1.ConditionTrue, testReadyCondition)
}

func TestDatabasePublicSchemaManageAndSystemSchemaObserveAreAllowed(t *testing.T) {
	for _, mode := range []string{"public manage", dbTestSystemObserveCase} {
		t.Run(mode, func(t *testing.T) {
			db := ownershipDatabase(testOrdersNamespace, dbTestApplicationName, dbTestResourceUID)
			db.Spec.Schemas = []epco.SchemaSpec{{Name: "public"}}
			if mode == dbTestSystemObserveCase {
				db.Spec.ManagementPolicy = epco.ManagementObserve
				db.Spec.Schemas = []epco.SchemaSpec{{Name: dbTestCatalogSchema}}
			}
			r, cl, s := databaseFixture(t, db)
			s.schemas = []postgres.SchemaState{{Name: string(db.Spec.Schemas[0].Name), Exists: true, Owner: dbTestOwnerName}}
			runDatabase(t, r, db)
			reason := testReadyCondition
			if mode == dbTestSystemObserveCase {
				reason = dbTestObservedReason
				noDatabaseMutation(t, s)
			}
			requireDatabaseCondition(t, currentDatabase(t, cl, db), metav1.ConditionTrue, reason)
		})
	}
}

func TestDatabaseAuthoritativeOwnershipFailureDoesNotConnect(t *testing.T) {
	db := ownershipDatabase(testOrdersNamespace, dbTestApplicationName, dbTestResourceUID)
	r, cl, s := databaseFixture(t, db)
	failure := apierrors.NewServiceUnavailable("authoritative ownership list unavailable")
	r.APIReader = interceptor.NewClient(cl, interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return failure }})
	r.Databases = databaseFactoryFunc(func(context.Context, *postgres.Config) (postgres.DatabaseSession, error) {
		t.Error("uncertain ownership opened SQL")
		return s, nil
	})
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(db)})
	if !errors.Is(err, failure) {
		t.Fatalf("ownership error=%v want original API error", err)
	}
	noDatabaseMutation(t, s)
}
