package controller_test

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/controller"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestDatabaseDeletionUsesRecordedSuccessAndIgnoresIrrelevantDesiredPayload(t *testing.T) {
	for _, mode := range []string{dbTestUnmanagedCase, dbTestRetainCase, "delete", "ignored children", dbTestAbsentCatalogCase, dbTestBlockedDropCase} {
		t.Run(mode, func(t *testing.T) {
			db := ownershipDatabase(testOrdersNamespace, dbTestApplicationName, dbTestResourceUID)
			db.Spec.ReclaimPolicy = epco.ReclaimDelete
			db.Status.HasManagedSQL = mode != dbTestUnmanagedCase
			db.Finalizers = []string{controller.DatabaseFinalizer}
			db.DeletionTimestamp = new(metav1.Now())
			if mode == dbTestRetainCase {
				db.Spec.ReclaimPolicy = epco.ReclaimRetain
			}
			if mode == "ignored children" {
				db.Spec.Schemas = []epco.SchemaSpec{{Name: dbTestCatalogSchema, Owner: dbTestMissingOwner, Permissions: &epco.SchemaPermissions{}}}
				db.Spec.ConnectionSecrets = []epco.ConnectionSecret{{Name: "existing-export", RoleRef: epco.LocalReference{Name: dbTestMissingOwner}}}
				db.Spec.Owner = dbTestMissingOwner
			}
			export := &corev1.Secret{Namespace: db.Namespace, Name: "existing-export", Data: map[string][]byte{dbTestUnrelatedSecretKey: []byte("keep")}}
			r, cl, s := databaseFixture(t, db, export)
			if mode == dbTestAbsentCatalogCase {
				s.state.Exists = false
			}
			if mode == dbTestBlockedDropCase {
				s.drop = func(context.Context, string) error {
					return &postgres.Failure{Reason: "DeletionBlocked", Message: "active sessions"}
				}
			}
			runDatabase(t, r, db)
			if mode == dbTestUnmanagedCase || mode == dbTestRetainCase {
				noDatabaseMutation(t, s)
			} else if mode != dbTestAbsentCatalogCase && s.drops != 1 {
				t.Fatalf("drop count %d want 1", s.drops)
			}
			latest := &epco.Database{}
			err := cl.Get(t.Context(), client.ObjectKeyFromObject(db), latest)
			if mode == dbTestBlockedDropCase {
				if err != nil {
					t.Fatal(err)
				}
				requireDatabaseCondition(t, latest, metav1.ConditionFalse, "DeletionBlocked")
				if !slices.Contains(latest.Finalizers, controller.DatabaseFinalizer) || !latest.Status.HasManagedSQL {
					t.Fatal("blocked DROP lost management/finalizer")
				}
			} else if err == nil && slices.Contains(latest.Finalizers, controller.DatabaseFinalizer) {
				t.Fatal("completed reclaim did not release finalizer")
			} else if err != nil && !apierrors.IsNotFound(err) {
				t.Fatal(err)
			}
			if err := cl.Get(t.Context(), client.ObjectKeyFromObject(export), &corev1.Secret{}); err != nil {
				t.Fatal("reclaim removed Secret named by unsupported export")
			}
		})
	}
}

func TestDatabaseExplicitAbsenceOverridesRetainRecordsReservationAndSkipsOwners(t *testing.T) {
	db := ownershipDatabase(testOrdersNamespace, dbTestApplicationName, dbTestResourceUID)
	db.Spec.Ensure = epco.EnsureAbsent
	db.Spec.Owner = dbTestMissingOwner
	db.Spec.Schemas = []epco.SchemaSpec{{Name: dbTestApplicationName, Owner: dbTestMissingOwner}}
	r, cl, s := databaseFixture(t, db)
	s.drop = func(_ context.Context, name string) error {
		if name != testSharedSQLName {
			t.Fatal("DROP selected wrong SQL database")
		}
		s.state.Exists = false
		return nil
	}
	runDatabase(t, r, db)
	latest := currentDatabase(t, cl, db)
	requireDatabaseCondition(t, latest, metav1.ConditionTrue, testReadyCondition)
	if s.drops != 1 || !latest.Status.HasManagedSQL {
		t.Fatal("explicit absence did not DROP and record reservation")
	}
	competitor := ownershipDatabase(testBillingNamespace, "competing", testOtherName)
	owner, err := controller.FindDatabaseOwnershipConflict(t.Context(), cl, competitor)
	if err != nil || owner == nil || owner.UID != db.UID {
		t.Fatal("live absent manager lost SQL name reservation")
	}
}

func TestDatabaseFormerManagerStickyMarkerSurvivesObserveAndFailure(t *testing.T) {
	db := ownershipDatabase(testOrdersNamespace, dbTestApplicationName, dbTestResourceUID)
	r, cl, s := databaseFixture(t, db)
	runDatabase(t, r, db)
	original := currentDatabase(t, cl, db)
	db = updateDatabase(t, cl, db, func(d *epco.Database) { d.Spec.ManagementPolicy = epco.ManagementObserve })
	runDatabase(t, r, db)
	observed := currentDatabase(t, cl, db)
	requireDatabaseCondition(t, observed, metav1.ConditionTrue, dbTestObservedReason)
	if !observed.Status.HasManagedSQL || observed.Status.Applied != nil || observed.Status.AppliedGeneration != original.Status.AppliedGeneration || !observed.Status.LastSuccessfulReconcileTime.Equal(original.Status.LastSuccessfulReconcileTime) {
		t.Fatal("observation erased successful management history")
	}
	db = updateDatabase(t, cl, db, func(d *epco.Database) { d.Spec.ManagementPolicy = epco.ManagementManage })
	s.apply = func(context.Context, postgres.DatabaseDesired) (*postgres.DatabaseApplyResult, error) {
		return nil, &postgres.Failure{Reason: postgres.InsufficientPrivileges, Message: "safe"}
	}
	runDatabase(t, r, db)
	failed := currentDatabase(t, cl, db)
	if !failed.Status.HasManagedSQL || failed.Status.AppliedGeneration != original.Status.AppliedGeneration || !failed.Status.LastSuccessfulReconcileTime.Equal(original.Status.LastSuccessfulReconcileTime) {
		t.Fatal("failed follow-up erased recorded manager history")
	}
}

func TestDatabaseRetryUsesCurrentDescriptorAndSecret(t *testing.T) {
	db := ownershipDatabase(testOrdersNamespace, dbTestApplicationName, dbTestResourceUID)
	r, cl, s := databaseFixture(t, db)
	var hosts, passwords []string
	r.Databases = databaseFactoryFunc(func(_ context.Context, c *postgres.Config) (postgres.DatabaseSession, error) {
		hosts = append(hosts, c.Connection.Host)
		passwords = append(passwords, c.Connection.Password)
		return s, nil
	})
	runDatabase(t, r, db)
	cluster := &epco.ExternalPostgres{}
	key := client.ObjectKey{Namespace: testInfraNamespace, Name: testDescriptorName}
	if err := cl.Get(t.Context(), key, cluster); err != nil {
		t.Fatal(err)
	}
	cluster.Spec.Connection.Host = dbTestSecondEndpoint
	if err := cl.Update(t.Context(), cluster); err != nil {
		t.Fatal(err)
	}
	admin := &corev1.Secret{}
	if err := cl.Get(t.Context(), client.ObjectKey{Namespace: testInfraNamespace, Name: testAdminName}, admin); err != nil {
		t.Fatal(err)
	}
	admin.Data[corev1.BasicAuthPasswordKey] = []byte(dbTestRotatedCredential)
	if err := cl.Update(t.Context(), admin); err != nil {
		t.Fatal(err)
	}
	runDatabase(t, r, db)
	cluster.Spec.Connection.Host = testEndpointHost
	if err := cl.Update(t.Context(), cluster); err != nil {
		t.Fatal(err)
	}
	runDatabase(t, r, db)
	if !slices.Equal(hosts, []string{testEndpointHost, dbTestSecondEndpoint, testEndpointHost}) || !slices.Equal(passwords, []string{"administrative-credential", dbTestRotatedCredential, dbTestRotatedCredential}) {
		t.Fatal("retry pinned old endpoint or administrative credentials")
	}
}

func TestDatabaseCancellationClosesSessionAndDoesNotPublishSuccess(t *testing.T) {
	db := ownershipDatabase(testOrdersNamespace, dbTestApplicationName, dbTestResourceUID)
	r, cl, s := databaseFixture(t, db)
	ctx, cancel := context.WithCancel(t.Context())
	s.apply = func(context.Context, postgres.DatabaseDesired) (*postgres.DatabaseApplyResult, error) {
		cancel()
		return nil, context.Canceled
	}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(db)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error=%v", err)
	}
	if s.closed != 1 || currentDatabase(t, cl, db).Status.HasManagedSQL {
		t.Fatal("canceled SQL leaked session or acquired manager")
	}
}

func TestDatabaseLockRechecksStaleSnapshotAndDoesNotTouchDeletedReplacement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := ownershipDatabase(testOrdersNamespace, dbTestApplicationName, "original-uid")
		r, cl, s := databaseFixture(t, db)
		release, err := r.Locks.Acquire(t.Context(), controller.CanonicalManagementKey(db.Namespace, db.Spec.Cluster, controller.SQLObjectDatabase, testSharedSQLName))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { release() }()
		loaded := make(chan struct{}, 1)
		r.Client = interceptor.NewClient(cl, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			err := c.Get(ctx, key, obj, opts...)
			if _, ok := obj.(*epco.Database); ok {
				select {
				case loaded <- struct{}{}:
				default:
				}
			}
			return err
		}})
		var opened atomic.Int64
		r.Databases = databaseFactoryFunc(func(context.Context, *postgres.Config) (postgres.DatabaseSession, error) {
			opened.Add(1)
			return s, nil
		})
		done := make(chan error, 1)
		go func() {
			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(db)})
			done <- err
		}()
		synctest.Wait()
		select {
		case <-loaded:
		default:
			t.Fatal("reconcile did not load pre-lock snapshot")
		}
		if err := cl.Delete(t.Context(), db); err != nil {
			t.Fatal(err)
		}
		replacement := ownershipDatabase(db.Namespace, db.Name, "replacement-uid")
		replacement.Spec.Owner = "replacement-owner"
		if err := cl.Create(t.Context(), replacement); err != nil {
			t.Fatal(err)
		}
		release()
		release = func() {}
		synctest.Wait()
		select {
		case err := <-done:
			if err != nil && !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
				t.Fatal(err)
			}
		default:
			t.Fatal("reconcile remained blocked")
		}
		if opened.Load() != 0 {
			t.Fatal("deleted stale UID reached SQL after lock acquisition")
		}
		if currentDatabase(t, cl, replacement).Status.HasManagedSQL {
			t.Fatal("stale reconcile recorded management on replacement UID")
		}
	})
}

func TestDatabaseContendersSerializeThenAuthoritativelySeeWinner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first := ownershipDatabase(testOrdersNamespace, "first", "first")
		second := ownershipDatabase(testBillingNamespace, "second", "second")
		r, cl, _ := databaseFixture(t, first, second, &corev1.Namespace{Name: testBillingNamespace})
		entered := make(chan struct{}, 1)
		finish := make(chan struct{})
		var opens atomic.Int64
		s1 := &databaseSessionStub{state: &postgres.DatabaseState{Exists: true, Name: testSharedSQLName, Owner: dbTestOwnerName, AdminUser: testAdminName, AdminDatabase: dbTestAdminDatabase, MaxIdentifierLength: 63, ServerVersion: 180000, AllowConnections: true}}
		s1.apply = func(context.Context, postgres.DatabaseDesired) (*postgres.DatabaseApplyResult, error) {
			entered <- struct{}{}
			<-finish
			return &postgres.DatabaseApplyResult{}, nil
		}
		r.Databases = databaseFactoryFunc(func(context.Context, *postgres.Config) (postgres.DatabaseSession, error) {
			opens.Add(1)
			return s1, nil
		})
		firstDone := make(chan error, 1)
		go func() {
			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(first)})
			firstDone <- err
		}()
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("first manager did not enter SQL")
		}
		secondDone := make(chan error, 1)
		go func() {
			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(second)})
			secondDone <- err
		}()
		synctest.Wait()
		if opens.Load() != 1 {
			t.Fatal("same database key entered SQL concurrently across namespaces")
		}
		close(finish)
		synctest.Wait()
		for _, done := range []chan error{firstDone, secondDone} {
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("serialized reconcile did not finish")
			}
		}
		if opens.Load() != 1 {
			t.Fatal("waiting contender did not recheck authoritative recorded manager")
		}
		requireDatabaseCondition(t, currentDatabase(t, cl, second), metav1.ConditionFalse, connectionOwnershipConflictReason)
	})
}
