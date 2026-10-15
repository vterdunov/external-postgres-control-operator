//go:build envtest

package envtest_test

import (
	"context"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// This adapter exposes catalog activity through the production manager, indexes,
// predicates and queue. The SQL owner deliberately has no spec/history reference.
type capabilityWatchFactory struct {
	databaseWatchFactory
	inspectionsMu sync.Mutex
	inspections   map[string]int
}

type capabilityWatchSession struct {
	postgres.DatabaseSession
	factory *capabilityWatchFactory
}

func (f *capabilityWatchFactory) Open(ctx context.Context,
	config *postgres.Config, options postgres.DatabaseSessionOptions,
) (postgres.DatabaseSession, error) {
	session, err := f.databaseWatchFactory.Open(ctx, config, options)
	if err != nil {
		return nil, err
	}
	return &capabilityWatchSession{DatabaseSession: session, factory: f}, nil
}

func (f *capabilityWatchFactory) count(name string) int {
	f.inspectionsMu.Lock()
	defer f.inspectionsMu.Unlock()
	return f.inspections[name]
}

func (s *capabilityWatchSession) Inspect(ctx context.Context, name string) (*postgres.DatabaseState, error) {
	s.factory.inspectionsMu.Lock()
	if s.factory.inspections == nil {
		s.factory.inspections = make(map[string]int)
	}
	s.factory.inspections[name]++
	s.factory.inspectionsMu.Unlock()
	return s.DatabaseSession.Inspect(ctx, name)
}

var _ = Describe("Managed owner capability namespace watches", func() {
	It("requeues every Database on a role manager's canonical cluster after "+
		"authorization removal and restoration", func(ctx SpecContext) {
		cluster := createRoleDescriptor(ctx, &epco.ClusterAccess{NamespaceSelector: &metav1.LabelSelector{}})
		roleScope, roleNamespace, err := environment.NewScope(ctx, "epco-capability-role-")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx SpecContext) { Expect(roleScope.Cleanup(ctx)).To(Succeed()) })
		roleNamespace.Labels = map[string]string{roleTestAccessLabel: roleTestAllowed}
		Expect(environment.Client.Update(ctx, roleNamespace)).To(Succeed())
		actualOwner := &epco.DatabaseRole{Namespace: roleNamespace.Name, Name: "actual-owner-resource",
			Spec: epco.DatabaseRoleSpec{Cluster: epco.ClusterReference{Namespace: namespace.Name, Name: cluster.Name},
				Name: "actual_catalog_owner", ManagementPolicy: epco.ManagementManage}}
		Expect(roleScope.Create(ctx, actualOwner)).To(Succeed())
		actualOwner.Status.HasManagedSQL = true
		Expect(environment.Client.Status().Update(ctx, actualOwner)).To(Succeed())

		factory := &capabilityWatchFactory{owner: string(actualOwner.Spec.Name)}
		startConnectionAPIManager(ctx, factory)
		// Observe keeps the deliberately different actual owner in the SQL catalog.
		// Namespace fanout must not depend on desired owners or retained ACL bindings.
		databases := []*epco.Database{
			watchedDatabase(cluster, "first-catalog-owner"), watchedDatabase(cluster, "second-catalog-owner"),
		}
		for _, db := range databases {
			db.Spec.Name = epco.SQLName(db.Name)
			db.Spec.ManagementPolicy = epco.ManagementObserve
			db.Spec.Owner = "unrelated_spec_owner"
			Expect(scope.Create(ctx, db)).To(Succeed())
			latest := expectDatabaseStatus(ctx, db, metav1.ConditionTrue, "Observed")
			Expect(latest.Status.ManagedACLTargets).To(BeEmpty())
		}
		// A similarly named descriptor in the role namespace is a different canonical
		// cluster. Its local Database must not hide a missing cross-namespace fanout.
		unrelatedCluster := &epco.PostgresCluster{Namespace: roleNamespace.Name, Name: cluster.Name, Spec: cluster.Spec}
		Expect(roleScope.Create(ctx, unrelatedCluster)).To(Succeed())

		for _, allowed := range []bool{false, true} {
			before := make([]int, len(databases))
			for i, db := range databases {
				before[i] = factory.count(string(db.Spec.Name))
			}
			Consistently(func() []int {
				counts := make([]int, len(databases))
				for i, db := range databases {
					counts[i] = factory.count(string(db.Spec.Name))
				}
				return counts
			}).WithContext(ctx).WithTimeout(300*time.Millisecond).Should(Equal(before),
				"settle startup events before testing the Namespace watch")
			Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(roleNamespace), roleNamespace)).To(Succeed())
			if allowed {
				roleNamespace.Labels[roleTestAccessLabel] = roleTestAllowed
			} else {
				delete(roleNamespace.Labels, roleTestAccessLabel)
			}
			Expect(environment.Client.Update(ctx, roleNamespace)).To(Succeed())
			for i, db := range databases {
				Eventually(func() bool { return factory.count(string(db.Spec.Name)) > before[i] }).
					WithContext(ctx).WithTimeout(4*time.Second).Should(BeTrue(),
					"a role Namespace label change must enqueue Databases on its canonical cluster "+
						"even when the actual owner is absent from Database spec/history")
				latest := &epco.Database{}
				Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(db), latest)).To(Succeed())
				Expect(latest.Generation).To(Equal(db.Generation))
			}
		}
	}, NodeTimeout(35*time.Second))

	It("maps role status, management policy and deletion through ordinary "+
		"same-generation dependency retries", func(ctx SpecContext) {
		cluster := createRoleDescriptor(ctx, &epco.ClusterAccess{NamespaceSelector: &metav1.LabelSelector{}})
		roleScope, roleNamespace, err := environment.NewScope(ctx, "epco-capability-events-")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx SpecContext) { Expect(roleScope.Cleanup(ctx)).To(Succeed()) })
		role := &epco.DatabaseRole{Namespace: roleNamespace.Name, Name: "schema-owner-manager",
			Spec: epco.DatabaseRoleSpec{Cluster: epco.ClusterReference{Namespace: namespace.Name, Name: cluster.Name},
				Name: "schema_owner"}}
		Expect(roleScope.Create(ctx, role)).To(Succeed())
		factory := &capabilityWatchFactory{}
		startConnectionAPIManager(ctx, factory)
		db := watchedDatabase(cluster, "role-capability-events")
		db.Spec.Name = epco.SQLName(db.Name)
		db.Spec.ManagementPolicy = epco.ManagementObserve
		db.Spec.Schemas = []epco.SchemaSpec{{Name: "app", Owner: role.Spec.Name}}
		Expect(scope.Create(ctx, db)).To(Succeed())
		expectDatabaseStatus(ctx, db, metav1.ConditionTrue, "Observed")
		for _, change := range []string{"record management", "observe policy", "delete"} {
			before := factory.count(string(db.Spec.Name))
			Consistently(func() int { return factory.count(string(db.Spec.Name)) }).
				WithContext(ctx).WithTimeout(300 * time.Millisecond).Should(Equal(before))
			Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(role), role)).To(Succeed())
			switch change {
			case "record management":
				role.Status.HasManagedSQL = true
				Expect(environment.Client.Status().Update(ctx, role)).To(Succeed())
			case "observe policy":
				role.Spec.ManagementPolicy = epco.ManagementObserve
				Expect(environment.Client.Update(ctx, role)).To(Succeed())
			case "delete":
				Expect(environment.Client.Delete(ctx, role)).To(Succeed())
			}
			Eventually(func() bool { return factory.count(string(db.Spec.Name)) > before }).
				WithContext(ctx).WithTimeout(4*time.Second).Should(BeTrue(), change)
			latest := &epco.Database{}
			Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(db), latest)).To(Succeed())
			Expect(latest.Generation).To(Equal(db.Generation))
		}
	}, NodeTimeout(35*time.Second))
})
