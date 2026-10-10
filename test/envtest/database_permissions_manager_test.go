//go:build envtest

package envtest_test

import (
	"context"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/controller"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	harness "github.com/vterdunov/external-postgres-control-operator/test/envtest"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const (
	permissionDatabaseField     = "database"
	permissionHostField         = "host"
	permissionDependencySQLName = "dependency"
)

type permissionWatchFactory struct {
	databaseWatchFactory
	applies atomic.Int32
}
type permissionWatchSession struct {
	postgres.DatabaseSession
	factory *permissionWatchFactory
}

func (f *permissionWatchFactory) Open(ctx context.Context, config *postgres.Config) (postgres.DatabaseSession, error) {
	session, err := f.databaseWatchFactory.Open(ctx, config)
	if err != nil {
		return nil, err
	}
	return &permissionWatchSession{DatabaseSession: session, factory: f}, nil
}
func (*permissionWatchSession) PreflightACL(context.Context, postgres.DatabaseDesired) error {
	return nil
}
func (*permissionWatchSession) PrepareACL(_ context.Context, desired postgres.ACLDesired) (*postgres.ACLResult, error) {
	return &postgres.ACLResult{Targets: desired.Targets}, nil
}
func (s *permissionWatchSession) ReconcileACL(_ context.Context,
	desired postgres.ACLDesired) (*postgres.ACLResult, error) {
	if !desired.Observe {
		s.factory.applies.Add(1)
	}
	return &postgres.ACLResult{Targets: desired.Targets}, nil
}
func startPermissionManager(ctx context.Context, factory *permissionWatchFactory) {
	builder, err := postgres.NewConfigBuilder()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	instance, err := environment.StartManager(ctx, harness.ManagerOptions{
		CachedObjects: []client.Object{
			&epco.Database{}, &epco.DatabaseRole{}, &epco.PostgresCluster{}, &corev1.Secret{}, &corev1.Namespace{}},
		Register: func(mgr manager.Manager) error {
			return (&controller.DatabaseReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Builder: builder,
				Databases: factory, Locks: &controller.ManagementLocks{}}).SetupWithManager(mgr)
		},
	})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		Expect(instance.Stop(cleanup)).To(Succeed())
	})
}

var _ = Describe("R3 Database ACL API and dependency watches", func() {
	It("persists concrete pending status bindings and rejects stale checkpoints", func(ctx SpecContext) {
		db := resource("Database")
		Expect(scope.Create(ctx, db)).To(Succeed())
		generation := db.GetGeneration()
		binding := map[string]any{"declaration": "database/create/reader",
			"scope": permissionDatabaseField, "class": permissionDatabaseField,
			"privilege": "CREATE", "grantee": "reader",
			"grantor": databaseTestOwnerName, databaseTestOwnerName: databaseTestOwnerName,
			"databaseOID": int64(12345), "granteeOID": int64(23456), "grantorOID": int64(34567),
			"pending": "grant", "generation": generation,
			"grantorCandidates": []any{map[string]any{"name": databaseTestOwnerName, "oid": int64(34567)}},
			"before": []any{map[string]any{
				"grantor": map[string]any{"name": databaseTestOwnerName, "oid": int64(34567)}, "grantable": true}}}
		targets := []any{map[string]any{permissionHostField: "postgres.example.test",
			"port": int64(5432), permissionDatabaseField: "postgres",
			"bindingRevision": "descriptor-revision", "bindings": []any{binding}}}
		setField(db, targets, "status", "managedACLTargets")
		setField(db, "current-target", "status", "activeACLTarget")
		Expect(environment.Client.Status().Update(ctx, db)).To(Succeed())
		stale := db.DeepCopy()
		latest := &unstructured.Unstructured{}
		latest.SetGroupVersionKind(db.GroupVersionKind())
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(db), latest)).To(Succeed())
		Expect(field(latest, "status", "managedACLTargets")).To(Equal(targets))
		Expect(field(latest, "status", "activeACLTarget")).To(Equal("current-target"))
		Expect(latest.GetGeneration()).To(Equal(generation))
		setField(latest, "concurrent status update", "status", "message")
		Expect(environment.Client.Status().Update(ctx, latest)).To(Succeed())
		setField(stale, []any{}, "status", "managedACLTargets")
		Expect(apierrors.IsConflict(environment.Client.Status().Update(ctx, stale))).To(BeTrue())
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(db), latest)).To(Succeed())
		Expect(field(latest, "status", "managedACLTargets")).To(Equal(targets))
	}, NodeTimeout(30*time.Second))

	DescribeTable("requeues the current generation when a managed permission role becomes ready",
		func(ctx SpecContext, creator bool) {
			factory := &permissionWatchFactory{}
			startPermissionManager(ctx, factory)
			cluster := createRoleDescriptor(ctx, nil)
			role := &epco.DatabaseRole{Name: "permission-role", Namespace: namespace.Name,
				Spec: epco.DatabaseRoleSpec{Cluster: epco.ClusterReference{Name: cluster.Name}, Name: permissionDependencySQLName}}
			Expect(scope.Create(ctx, role)).To(Succeed())
			db := watchedDatabase(cluster, "permission-dependency")
			if creator {
				db.Spec.DefaultPrivileges = []epco.DefaultPrivileges{{ForRole: permissionDependencySQLName,
					Tables: &epco.TablePermissions{Select: []epco.Permission{{Name: "external-reader"}}}}}
			} else {
				db.Spec.Permissions = &epco.DatabasePermissions{Create: []epco.Permission{{Name: permissionDependencySQLName}}}
			}
			Expect(scope.Create(ctx, db)).To(Succeed())
			latest := expectDatabaseStatus(ctx, db, metav1.ConditionFalse, "DependenciesNotReady")
			generation := latest.Generation
			Expect(factory.applies.Load()).To(BeZero(), "unready managed grantee or creator cannot mutate ACLs")
			Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(role), role)).To(Succeed())
			role.Status.HasManagedSQL = true
			role.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready",
				ObservedGeneration: role.Generation, LastTransitionTime: metav1.Now()}}
			Expect(environment.Client.Status().Update(ctx, role)).To(Succeed())
			latest = expectDatabaseStatus(ctx, db, metav1.ConditionTrue, "Ready")
			Expect(latest.Generation).To(Equal(generation), "role status watch must requeue without a Database spec edit")
			Expect(factory.applies.Load()).To(BeNumerically(">", 0))
		}, Entry("grantee canonical SQL name", false, NodeTimeout(40*time.Second)),
		Entry("default creator canonical SQL name", true, NodeTimeout(40*time.Second)))
})
