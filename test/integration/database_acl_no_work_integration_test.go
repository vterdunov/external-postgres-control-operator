//go:build integration

package integration

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	pg "github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	kubeenv "github.com/vterdunov/external-postgres-control-operator/test/envtest"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type tracedDatabaseFactory struct {
	pg.DatabaseFactory
	trace *administrativeQueryTracer
}

func (f tracedDatabaseFactory) Open(ctx context.Context, config *pg.Config,
	options pg.DatabaseSessionOptions) (pg.DatabaseSession, error) {
	traced := *config
	traced.Connection = config.Connection.Copy()
	traced.Connection.Tracer = f.trace
	return f.DatabaseFactory.Open(ctx, &traced, options)
}

var _ = Describe("Database reconciliation without ACL work", Ordered, func() {
	var environment *kubeenv.Environment
	BeforeAll(func(ctx SpecContext) { environment = r4StartAPI(ctx) }, NodeTimeout(60*time.Second))

	DescribeTable("checks export access without scanning the role catalog",
		func(ctx SpecContext, permissions *epco.DatabasePermissions) {
			fixture := r4NewFixture(ctx, environment)
			fixture.editDatabase(ctx, func(spec *epco.DatabaseSpec) { spec.Permissions = permissions })
			output := fixture.bootstrap(ctx)
			reconciler := databaseRecoveryReconciler(fixture.api, pg.NewDatabaseFactory())
			current := recoverDatabaseReady(ctx, fixture.api, reconciler, fixture.database)
			Expect(r4ACLBindings(current)).To(BeEmpty())

			trace := &administrativeQueryTracer{}
			traced := databaseRecoveryReconciler(fixture.api, tracedDatabaseFactory{
				DatabaseFactory: pg.NewDatabaseFactory(), trace: trace,
			})
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(current)}
			_, err := traced.Reconcile(ctx, request)
			Expect(r4ReconcileSucceeded(err)).To(BeTrue(), "steady reconciliation succeeds")
			current = recoveredDatabase(ctx, fixture.api, current)
			Expect(meta.IsStatusConditionTrue(current.Status.Conditions, "Ready")).To(BeTrue())
			Expect(current.Status.AppliedGeneration).To(Equal(current.Generation))
			Expect(fixture.output(ctx, r4OutputName).UID).To(Equal(output.UID))

			name := quoteIdentifier(string(current.Spec.Name))
			roleSQL(ctx, fixture.Fixture, "REVOKE CONNECT ON DATABASE "+name+" FROM PUBLIC")
			_, err = traced.Reconcile(ctx, request)
			Expect(r4ReconcileSucceeded(err)).To(BeTrue(), "reconciliation reports external CONNECT drift in status")
			current = recoveredDatabase(ctx, fixture.api, current)
			condition := meta.FindStatusCondition(current.Status.Conditions, "Ready")
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal(pg.DependenciesNotReady))
			Expect(fixture.output(ctx, r4OutputName).UID).To(Equal(output.UID))

			roleSQL(ctx, fixture.Fixture, "GRANT CONNECT ON DATABASE "+name+" TO PUBLIC")
			_, err = traced.Reconcile(ctx, request)
			Expect(r4ReconcileSucceeded(err)).To(BeTrue(), "reconciliation confirms effective CONNECT once restored")
			current = recoveredDatabase(ctx, fixture.api, current)
			Expect(meta.IsStatusConditionTrue(current.Status.Conditions, "Ready")).To(BeTrue())
			Expect(r4ACLBindings(current)).To(BeEmpty())

			roleScans := 0
			for _, query := range trace.queries {
				sql := strings.ToLower(query.sql)
				if strings.Contains(sql, "from pg_catalog.pg_roles") && !strings.Contains(sql, "where") {
					roleScans++
				}
			}
			Expect(roleScans).To(BeZero(), "no reconciliation has ACL declarations or tracked bindings")
		},
		Entry("omitted permissions", (*epco.DatabasePermissions)(nil), NodeTimeout(45*time.Second)),
		Entry("empty permissions block", &epco.DatabasePermissions{}, NodeTimeout(45*time.Second)))
})
