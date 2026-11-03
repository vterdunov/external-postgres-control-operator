//go:build envtest

package envtest_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/controller"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func nullSelectorDatabase(db *epco.Database) *unstructured.Unstructured {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(db)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	object := &unstructured.Unstructured{Object: raw}
	object.SetGroupVersionKind(epco.GroupVersion.WithKind(roleTableKind))
	rule := map[string]any{roleTableRoleField: roleTableReader,
		roleTableTablesField: nil, roleTablePrivilegesField: []any{roleTableSelect}}
	schema := map[string]any{nameField: capacitySchema, roleTableRulesField: []any{rule}}
	setField(object, []any{schema}, specField, roleTableSchemasField)
	return object
}

var _ = Describe("null table selector finalizer safety", func() {
	It("registers early-output cleanup without rewriting the persisted null selector", func(ctx SpecContext) {
		cluster, _, source := connectionAPIFixture(ctx)
		db := connectionAPIDatabase(cluster, "null-selector-output")
		object := nullSelectorDatabase(db)
		Expect(scope.Create(ctx, object)).To(Succeed())
		r := connectionAPIDirectReconciler(environment.Client)
		_, err := r.Reconcile(ctx, connectionAPIRequestFor(db))
		Expect(err).NotTo(HaveOccurred(),
			"finalizer registration must update metadata without resubmitting the invalid selector")
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(object), object)).To(Succeed())
		Expect(object.GetFinalizers()).To(ContainElement(controller.DatabaseFinalizer))
		schema := field(object, specField, roleTableSchemasField).([]any)[0].(map[string]any)
		rule := schema[roleTableRulesField].([]any)[0].(map[string]any)
		Expect(rule).To(HaveKeyWithValue(roleTableTablesField, BeNil()),
			"metadata ownership must not widen or normalize the stored selector")
		output := connectionAPIOutput(ctx, db)
		Expect(output.Data[passwordKey]).To(Equal(source.Data[passwordKey]),
			"initial output publication must retain cleanup protection")
	}, NodeTimeout(30*time.Second))

	DescribeTable("releases a managed UID with an invalid null child selector on deletion",
		func(ctx SpecContext, policy epco.ReclaimPolicy, alreadyAbsent bool) {
			cluster := createRoleDescriptor(ctx, nil)
			db := watchedDatabase(cluster, "null-selector-deletion")
			db.Spec.ReclaimPolicy = policy
			raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(db)
			Expect(err).NotTo(HaveOccurred())
			object := &unstructured.Unstructured{Object: raw}
			object.SetGroupVersionKind(epco.GroupVersion.WithKind(roleTableKind))
			Expect(scope.Create(ctx, object)).To(Succeed())
			factory := &permissionWatchFactory{}
			r := connectionAPIDirectReconciler(environment.Client)
			r.Databases = factory
			_, err = r.Reconcile(ctx, connectionAPIRequestFor(db))
			Expect(err).NotTo(HaveOccurred())
			managed := expectDatabaseStatus(ctx, db, metav1.ConditionTrue, databaseTestReadyCondition)
			Expect(managed.Status.HasManagedSQL).To(BeTrue())
			Expect(managed.Finalizers).To(ContainElement(controller.DatabaseFinalizer))
			uid := managed.UID
			object = nullSelectorDatabase(managed)
			Expect(environment.Client.Update(ctx, object)).To(Succeed())
			Expect(object.GetUID()).To(Equal(uid), "the invalid child edit must keep the existing management session")
			if alreadyAbsent {
				factory.mu.Lock()
				factory.exists = map[string]bool{db.Name: false}
				factory.mu.Unlock()
			}
			Expect(environment.Client.Delete(ctx, object)).To(Succeed())
			_, err = r.Reconcile(ctx, connectionAPIRequestFor(db))
			Expect(err).NotTo(HaveOccurred(), "irrelevant invalid child declarations cannot block authorized finalizer release")
			Expect(scope.WaitGone(ctx, object)).To(Succeed())
			_, _, _, drops := factory.snapshot()
			expectedDrops := 0
			if policy == epco.ReclaimDelete && !alreadyAbsent {
				expectedDrops = 1
			}
			Expect(drops).To(Equal(expectedDrops), "retain and already-confirmed absence must not issue DROP")
		},
		Entry("retain keeps SQL", epco.ReclaimRetain, false, NodeTimeout(30*time.Second)),
		Entry("delete removes SQL", epco.ReclaimDelete, false, NodeTimeout(30*time.Second)),
		Entry("delete after confirmed absence", epco.ReclaimDelete, true, NodeTimeout(30*time.Second)),
	)
})
