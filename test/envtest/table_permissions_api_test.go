//go:build envtest

package envtest_test

import (
	"context"
	"encoding/json/v2"
	"maps"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	harness "github.com/vterdunov/external-postgres-control-operator/test/envtest"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	roleTableReader           = "reader"
	roleTableOne              = "one"
	roleTableUser             = "app_user"
	roleTableSelect           = "select"
	roleTableInsert           = "insert"
	roleTableEnsureField      = "ensure"
	roleTablePermissionsField = "permissions"
	roleTablePublic           = "public"
	roleTablePrivilegesField  = "privileges"
	roleTableRulesField       = "tablePermissions"
	roleTableRoleField        = "role"
	roleTableTablesField      = "tables"
	roleTableSchemasField     = "schemas"
	roleTableKind             = "Database"
)

func roleTableAPIObject(rules []any) *unstructured.Unstructured {
	object := resource(roleTableKind)
	setField(object, []any{map[string]any{nameField: capacitySchema, roleTableRulesField: rules}}, specField, roleTableSchemasField)
	return object
}

var _ = Describe("role-oriented table permissions API", func() {
	It("watches managed grantee readiness for a table rule without a spec edit", func(ctx SpecContext) {
		factory := &permissionWatchFactory{}
		startPermissionManager(ctx, factory)
		cluster := createRoleDescriptor(ctx, nil)
		role := &epco.DatabaseRole{Name: "table-rule-reader", Namespace: namespace.Name,
			Spec: epco.DatabaseRoleSpec{Cluster: epco.ClusterReference{Name: cluster.Name}, Name: "table_rule_sql_reader"}}
		Expect(scope.Create(ctx, role)).To(Succeed())
		db := watchedDatabase(cluster, "table-rule-dependency")
		raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(db)
		Expect(err).NotTo(HaveOccurred())
		object := &unstructured.Unstructured{Object: raw}
		object.SetGroupVersionKind(epco.GroupVersion.WithKind(roleTableKind))
		setField(object, []any{map[string]any{nameField: capacitySchema, roleTableRulesField: []any{map[string]any{
			roleTableRoleField: "table_rule_sql_reader", roleTableTablesField: []any{roleTableOne}, roleTablePrivilegesField: []any{roleTableSelect}}}}}, specField, roleTableSchemasField)
		Expect(scope.Create(ctx, object)).To(Succeed())
		latest := expectDatabaseStatus(ctx, db, metav1.ConditionFalse, "DependenciesNotReady")
		generation := latest.Generation
		Expect(factory.applies.Load()).To(BeZero())
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(role), role)).To(Succeed())
		role.Status.HasManagedSQL = true
		role.Status.Conditions = []metav1.Condition{{Type: databaseTestReadyCondition, Status: metav1.ConditionTrue, Reason: databaseTestReadyCondition,
			ObservedGeneration: role.Generation, LastTransitionTime: metav1.Now()}}
		Expect(environment.Client.Status().Update(ctx, role)).To(Succeed())
		latest = expectDatabaseStatus(ctx, db, metav1.ConditionTrue, databaseTestReadyCondition)
		Expect(latest.Generation).To(Equal(generation))
		Expect(factory.applies.Load()).To(BeNumerically(">", 0))
	}, NodeTimeout(40*time.Second))

	It("admits the canonical manifest and preserves role leaves and omitted selectors", func(ctx SpecContext) {
		object := roleTableAPIObject([]any{
			map[string]any{roleTableRoleField: roleTableUser, roleTableTablesField: []any{"customers", "products"}, roleTablePrivilegesField: []any{roleTableSelect}},
			map[string]any{roleTableRoleField: roleTableUser, roleTableTablesField: []any{"orders", "order_items"}, roleTablePrivilegesField: []any{roleTableSelect, roleTableInsert}},
			map[string]any{roleTableRoleField: "orders_reader", roleTablePrivilegesField: []any{roleTableSelect}},
		})
		setField(object, map[string]any{"connect": []any{map[string]any{roleTableRoleField: roleTablePublic, roleTableEnsureField: "absent"}, map[string]any{roleTableRoleField: roleTableUser}}}, specField, roleTablePermissionsField)
		schemas := field(object, specField, roleTableSchemasField).([]any)
		schemas[0].(map[string]any)[roleTablePermissionsField] = map[string]any{"usage": []any{map[string]any{roleTableRoleField: roleTableUser}}}
		setField(object, schemas, specField, roleTableSchemasField)
		Expect(scope.Create(ctx, object, client.FieldValidation("Strict"))).To(Succeed())
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(object), object)).To(Succeed())
		schema := field(object, specField, roleTableSchemasField).([]any)[0].(map[string]any)
		rules := schema[roleTableRulesField].([]any)
		Expect(rules).To(HaveLen(3))
		Expect(rules[0].(map[string]any)).To(HaveKeyWithValue(roleTableRoleField, roleTableUser))
		Expect(rules[0].(map[string]any)).To(HaveKeyWithValue(roleTableEnsureField, "present"))
		Expect(rules[2].(map[string]any)).NotTo(HaveKey(roleTableTablesField))
		Expect(schema[roleTablePermissionsField].(map[string]any)["usage"].([]any)[0].(map[string]any)).To(HaveKeyWithValue(roleTableRoleField, roleTableUser))
	})

	DescribeTable("rejects removed fields and unknown selectors under strict validation", func(ctx SpecContext, mutate func(*unstructured.Unstructured), unknown string) {
		object := roleTableAPIObject([]any{map[string]any{roleTableRoleField: roleTableReader, roleTablePrivilegesField: []any{roleTableSelect}}})
		mutate(object)
		err := scope.Create(ctx, object, client.FieldValidation("Strict"))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(unknown))
	},
		Entry("misspelled selector", func(object *unstructured.Unstructured) {
			setField(object, []any{map[string]any{nameField: capacitySchema, roleTableRulesField: []any{map[string]any{
				roleTableRoleField: roleTableReader, "table": []any{roleTableOne}, roleTablePrivilegesField: []any{roleTableSelect}}}}}, specField, roleTableSchemasField)
		}, "table"),
		Entry("old recipient name", func(object *unstructured.Unstructured) {
			setField(object, map[string]any{"connect": []any{map[string]any{nameField: roleTableReader}}}, specField, roleTablePermissionsField)
		}, nameField),
		Entry("removed schema table map", func(object *unstructured.Unstructured) {
			setField(object, []any{map[string]any{nameField: capacitySchema, roleTablePermissionsField: map[string]any{
				roleTableTablesField: map[string]any{roleTableSelect: []any{map[string]any{roleTableRoleField: roleTableReader}}}}}}, specField, roleTableSchemasField)
		}, roleTableTablesField),
		Entry("old named table shape", func(object *unstructured.Unstructured) {
			setField(object, []any{map[string]any{nameField: capacitySchema, roleTableRulesField: []any{map[string]any{
				nameField: roleTableOne, roleTablePermissionsField: map[string]any{roleTableSelect: []any{map[string]any{roleTableRoleField: roleTableReader}}}}}}}, specField, roleTableSchemasField)
		}, roleTablePermissionsField),
	)

	It("makes the table rule list atomic for server-side apply", func(ctx SpecContext) {
		first := map[string]any{roleTableRoleField: roleTableReader, roleTableTablesField: []any{roleTableOne}, roleTablePrivilegesField: []any{roleTableSelect}}
		object := roleTableAPIObject([]any{first})
		Expect(scope.Create(ctx, object, client.FieldOwner("rules-a"))).To(Succeed())
		second := roleTableAPIObject([]any{first, map[string]any{roleTableRoleField: roleTableReader, roleTableTablesField: []any{"two"}, roleTablePrivilegesField: []any{roleTableInsert}}})
		err := environment.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(second), client.FieldOwner("rules-b"))
		Expect(apierrors.IsConflict(err)).To(BeTrue(), "independent field managers cannot own separate entries: %v", err)
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(object), object)).To(Succeed())
		Expect(field(object, specField, roleTableSchemasField).([]any)[0].(map[string]any)[roleTableRulesField]).To(Equal([]any{map[string]any{
			roleTableRoleField: roleTableReader, roleTableTablesField: []any{roleTableOne}, roleTablePrivilegesField: []any{roleTableSelect}, roleTableEnsureField: "present"}}))
	})

	It("admits repeated roles on the same table with disjoint privileges", func(ctx SpecContext) {
		object := roleTableAPIObject([]any{
			map[string]any{roleTableRoleField: roleTableReader, roleTableTablesField: []any{roleTableOne}, roleTablePrivilegesField: []any{roleTableSelect}},
			map[string]any{roleTableRoleField: roleTableReader, roleTableTablesField: []any{roleTableOne}, roleTablePrivilegesField: []any{roleTableInsert}},
		})
		Expect(scope.Create(ctx, object)).To(Succeed())
		Expect(field(object, specField, roleTableSchemasField).([]any)[0].(map[string]any)[roleTableRulesField]).To(HaveLen(2))
	})

	It("preserves literal selector names and explicit absence", func(ctx SpecContext) {
		rule := map[string]any{roleTableRoleField: roleTablePublic, roleTableTablesField: []any{"*", "ALL", "ALL_TABLES", "app.literal"}, roleTablePrivilegesField: []any{roleTableSelect}, roleTableEnsureField: "absent"}
		object := roleTableAPIObject([]any{rule})
		Expect(scope.Create(ctx, object)).To(Succeed())
		Expect(field(object, specField, roleTableSchemasField).([]any)[0].(map[string]any)[roleTableRulesField]).To(Equal([]any{rule}))
	})

	It("preserves an explicit null selector for rejection before SQL rather than omission", func(ctx SpecContext) {
		object := roleTableAPIObject([]any{map[string]any{roleTableRoleField: roleTableReader, roleTableTablesField: nil, roleTablePrivilegesField: []any{roleTableSelect}}})
		Expect(scope.Create(ctx, object)).To(Succeed())
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(object), object)).To(Succeed())
		rule := field(object, specField, roleTableSchemasField).([]any)[0].(map[string]any)[roleTableRulesField].([]any)[0].(map[string]any)
		Expect(rule).To(HaveKeyWithValue(roleTableTablesField, BeNil()), "nullable selector must survive admission without becoming all tables")
		encoded, err := json.Marshal(object.Object)
		Expect(err).NotTo(HaveOccurred())
		var typed epco.Database
		Expect(json.Unmarshal(encoded, &typed)).To(Succeed())
		encoded, err = json.Marshal(typed)
		Expect(err).NotTo(HaveOccurred())
		var roundTrip map[string]any
		Expect(json.Unmarshal(encoded, &roundTrip)).To(Succeed())
		rule = roundTrip["spec"].(map[string]any)[roleTableSchemasField].([]any)[0].(map[string]any)[roleTableRulesField].([]any)[0].(map[string]any)
		Expect(rule).To(HaveKeyWithValue(roleTableTablesField, BeEmpty()), "typed decode/re-encode must retain invalid explicit selection")

		key := client.ObjectKeyFromObject(object)
		Expect(environment.Client.Get(ctx, key, &typed)).To(Succeed())
		Expect(typed.Spec.Schemas).To(HaveLen(1))
		Expect(typed.Spec.Schemas[0].TablePermissions).To(HaveLen(1))
		Expect(typed.Spec.Schemas[0].TablePermissions[0].Tables).NotTo(BeNil(), "typed client GET must retain explicit selector intent")
		Expect(typed.Spec.Schemas[0].TablePermissions[0].Tables).To(BeEmpty())
		expectStoredNull := func() {
			Expect(environment.Client.Get(ctx, key, object)).To(Succeed())
			stored := field(object, specField, roleTableSchemasField).([]any)[0].(map[string]any)[roleTableRulesField].([]any)[0].(map[string]any)
			Expect(stored).To(HaveKeyWithValue(roleTableTablesField, BeNil()), "status writes cannot change the persisted invalid selector")
		}
		typed.Status.Message = "null selector remains invalid"
		Expect(environment.Client.Status().Update(ctx, &typed)).To(Succeed())
		expectStoredNull()
		Expect(environment.Client.Get(ctx, key, &typed)).To(Succeed())
		before := typed.DeepCopy()
		typed.Status.Message = "status-only patch retains spec"
		Expect(environment.Client.Status().Patch(ctx, &typed, client.MergeFrom(before))).To(Succeed())
		expectStoredNull()
		Expect(environment.Client.Get(ctx, key, &typed)).To(Succeed())
		typed.SetLabels(map[string]string{"typed-update": "must-not-widen"})
		Expect(apierrors.IsInvalid(environment.Client.Update(ctx, &typed))).To(BeTrue(),
			"typed full Update must encode the nonnil empty selector and fail admission rather than select all tables")
		expectStoredNull()
		Expect(environment.Client.Get(ctx, key, &typed)).To(Succeed())
		typed.Spec.Schemas[0].TablePermissions[0].Tables = []epco.SQLName{roleTableOne}
		Expect(environment.Client.Update(ctx, &typed)).To(Succeed(), "correcting the invalid selector must allow ordinary typed updates")
		Expect(environment.Client.Get(ctx, key, object)).To(Succeed())
		rule = field(object, specField, roleTableSchemasField).([]any)[0].(map[string]any)[roleTableRulesField].([]any)[0].(map[string]any)
		Expect(rule).To(HaveKeyWithValue(roleTableTablesField, []any{roleTableOne}))
	})

	It("keeps an omitted all-tables selector omitted through typed client updates", func(ctx SpecContext) {
		object := roleTableAPIObject([]any{map[string]any{roleTableRoleField: roleTableReader, roleTablePrivilegesField: []any{roleTableSelect}}})
		Expect(scope.Create(ctx, object)).To(Succeed())
		key := client.ObjectKeyFromObject(object)
		var typed epco.Database
		Expect(environment.Client.Get(ctx, key, &typed)).To(Succeed())
		Expect(typed.Spec.Schemas[0].TablePermissions[0].Tables).To(BeNil())
		typed.SetLabels(map[string]string{"typed-update": "preserves-omission"})
		Expect(environment.Client.Update(ctx, &typed)).To(Succeed())
		Expect(environment.Client.Get(ctx, key, object)).To(Succeed())
		rule := field(object, specField, roleTableSchemasField).([]any)[0].(map[string]any)[roleTableRulesField].([]any)[0].(map[string]any)
		Expect(rule).NotTo(HaveKey(roleTableTablesField), "omission must retain its selected all-tables meaning")
		Expect(environment.Client.Get(ctx, key, &typed)).To(Succeed())
		Expect(typed.Spec.Schemas[0].TablePermissions[0].Tables).To(BeNil())
	})

	It("preserves null and omitted selectors in the real manager cache", func(ctx SpecContext) {
		instance, err := environment.StartManager(ctx, harness.ManagerOptions{
			Namespace: namespace.Name, CachedObjects: []client.Object{&epco.Database{}},
		})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			Expect(instance.Stop(stopCtx)).To(Succeed())
		})
		nullObject := roleTableAPIObject([]any{map[string]any{roleTableRoleField: roleTableReader, roleTableTablesField: nil, roleTablePrivilegesField: []any{roleTableSelect}}})
		Expect(scope.Create(ctx, nullObject)).To(Succeed())
		omittedObject := roleTableAPIObject([]any{map[string]any{roleTableRoleField: roleTableReader, roleTablePrivilegesField: []any{roleTableSelect}}})
		omittedObject.SetName("omitted-selector")
		Expect(scope.Create(ctx, omittedObject)).To(Succeed())
		cached := instance.Manager.GetClient()
		Eventually(func(g Gomega) {
			var nullTyped, omittedTyped epco.Database
			g.Expect(cached.Get(ctx, client.ObjectKeyFromObject(nullObject), &nullTyped)).To(Succeed())
			g.Expect(cached.Get(ctx, client.ObjectKeyFromObject(omittedObject), &omittedTyped)).To(Succeed())
			g.Expect(nullTyped.Spec.Schemas[0].TablePermissions[0].Tables).NotTo(BeNil())
			g.Expect(nullTyped.Spec.Schemas[0].TablePermissions[0].Tables).To(BeEmpty())
			g.Expect(omittedTyped.Spec.Schemas[0].TablePermissions[0].Tables).To(BeNil())
		}).WithContext(ctx).WithTimeout(10 * time.Second).Should(Succeed())
	}, NodeTimeout(40*time.Second))

	DescribeTable("rejects malformed table rules without broadening selectors", func(ctx SpecContext, changes map[string]any, remove string) {
		rule := map[string]any{roleTableRoleField: roleTableReader, roleTableTablesField: []any{roleTableOne}, roleTablePrivilegesField: []any{roleTableSelect}}
		maps.Copy(rule, changes)
		if remove != "" {
			delete(rule, remove)
		}
		err := scope.Create(ctx, roleTableAPIObject([]any{rule}))
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%v", err)
	},
		Entry("missing role", nil, roleTableRoleField),
		Entry("empty role", map[string]any{roleTableRoleField: ""}, ""),
		Entry("missing privileges", nil, roleTablePrivilegesField),
		Entry("empty privileges", map[string]any{roleTablePrivilegesField: []any{}}, ""),
		Entry("uppercase privilege", map[string]any{roleTablePrivilegesField: []any{"SELECT"}}, ""),
		Entry("unsupported privilege", map[string]any{roleTablePrivilegesField: []any{"all"}}, ""),
		Entry("empty selector", map[string]any{roleTableTablesField: []any{}}, ""),
		Entry("object selector", map[string]any{roleTableTablesField: map[string]any{}}, ""),
		Entry("empty literal name", map[string]any{roleTableTablesField: []any{""}}, ""),
		Entry("duplicate literal name", map[string]any{roleTableTablesField: []any{roleTableOne, roleTableOne}}, ""),
		Entry("duplicate privilege", map[string]any{roleTablePrivilegesField: []any{roleTableSelect, roleTableSelect}}, ""),
		Entry("invalid ensure", map[string]any{roleTableEnsureField: "drop"}, ""),
	)
})
