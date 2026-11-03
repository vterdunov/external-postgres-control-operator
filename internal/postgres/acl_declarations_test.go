package postgres

import (
	"encoding/json/v2"
	"fmt"
	"reflect"
	"testing"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	roleTableMaintainRules = `[{"role":"reader","privileges":["maintain"]}]`
	roleTableRoleField     = "role"
)

func roleTableSpec(t *testing.T, rules string) epco.DatabaseSpec {
	t.Helper()
	var spec epco.DatabaseSpec
	if err := json.Unmarshal([]byte(`{"schemas":[{"name":"app","tablePermissions":`+rules+`}]}`), &spec); err != nil {
		t.Fatal(err)
	}
	return spec
}

func TestRoleTableDeclarationsExpandSemanticTuples(t *testing.T) {
	spec := roleTableSpec(t, `[
	 {"role":"app_user","tables":["customers","products"],"privileges":["select"]},
	 {"role":"app_user","tables":["orders","order_items"],"privileges":["select","insert"]},
	 {"role":"reader","privileges":["select"]},
	 {"role":"PUBLIC","tables":["*","ALL","ALL_TABLES","app.literal"],"privileges":["update"],"ensure":"absent"}
	]`)
	declarations := aclDeclarations(spec)
	if len(declarations) != 11 {
		t.Fatalf("got %d declarations, want 11: %#v", len(declarations), declarations)
	}
	expected := map[string]bool{}
	add := func(scope, table, privilege, role string, absent bool) {
		expected[fmt.Sprintf("%q/%q/%q/%q/%q/%q/%q", scope, "table", "app", table, "", privilege, role)] = absent
	}
	for _, table := range []string{"customers", "products"} {
		add("table", table, "SELECT", "app_user", false)
	}
	for _, table := range []string{"orders", "order_items"} {
		for _, privilege := range []string{"SELECT", "INSERT"} {
			add("table", table, privilege, "app_user", false)
		}
	}
	add("tables", "", "SELECT", "reader", false)
	for _, table := range []string{"*", "ALL", "ALL_TABLES", "app.literal"} {
		add("table", table, "UPDATE", "PUBLIC", true)
	}
	for _, declaration := range declarations {
		absent, found := expected[declaration.binding.Declaration]
		if !found || absent != declaration.absent {
			t.Errorf("unexpected declaration: %#v", declaration)
		}
		delete(expected, declaration.binding.Declaration)
	}
	if len(expected) != 0 {
		t.Errorf("missing declarations: %#v", expected)
	}
}

func TestRoleTableRegroupingPreservesDeclarationIdentity(t *testing.T) {
	grouped := roleTableSpec(t, `[{"role":"reader","tables":["one","two"],"privileges":["select","insert"]}]`)
	regrouped := roleTableSpec(t, `[
	 {"role":"reader","tables":["two","one"],"privileges":["insert"]},
	 {"role":"reader","tables":["two"],"privileges":["select"]},
	 {"role":"reader","tables":["one"],"privileges":["select"]}]`)
	before, after := aclDeclarations(grouped), aclDeclarations(regrouped)
	if len(before) != 4 || !reflect.DeepEqual(before, after) {
		t.Fatalf("regrouping must retain the four existing semantic history keys: before=%#v after=%#v", before, after)
	}
	// This key is serialized in existing status and must remain usable after the API change.
	const historical = `"table"/"table"/"app"/"one"/""/"SELECT"/"reader"`
	for _, declaration := range after {
		if declaration.binding.Declaration == historical {
			return
		}
	}
	t.Fatal("existing serialized declaration identity was lost")
}

func TestRoleTablePreflightConflictsAndVersions(t *testing.T) {
	for _, tc := range []struct {
		name, rules, reason string
		version             int
	}{
		{"disjoint tables", `[{"role":"reader","tables":["one"],"privileges":["select"]},{"role":"reader","tables":["two"],"privileges":["select","insert"]}]`, "", 180000},
		{"same table disjoint privileges", `[{"role":"reader","tables":["one"],"privileges":["select"]},{"role":"reader","tables":["one"],"privileges":["insert"]}]`, "", 180000},
		{"duplicate tuple", `[{"role":"reader","tables":["one"],"privileges":["select"]},{"role":"reader","tables":["one"],"privileges":["select"]}]`, InvalidConfiguration, 180000},
		{"opposing tuple", `[{"role":"reader","tables":["one"],"privileges":["select"]},{"role":"reader","tables":["one"],"privileges":["select"],"ensure":"absent"}]`, InvalidConfiguration, 180000},
		{"broad named overlap", `[{"role":"reader","privileges":["select"]},{"role":"reader","tables":["one"],"privileges":["select"]}]`, InvalidConfiguration, 180000},
		{"broad opposing overlap", `[{"role":"reader","privileges":["select"]},{"role":"reader","tables":["one"],"privileges":["select"],"ensure":"absent"}]`, InvalidConfiguration, 180000},
		{"maintain PG15", roleTableMaintainRules, UnsupportedFeature, 150000},
		{"maintain PG16", `[{"role":"reader","tables":["one"],"privileges":["maintain"]}]`, UnsupportedFeature, 160000},
		{"maintain PG17", roleTableMaintainRules, "", 170000},
		{"maintain PG18", roleTableMaintainRules, "", 180000},
		{"missing role", `[{"tables":["one"],"privileges":["select"]}]`, InvalidConfiguration, 180000},
		{"empty role", `[{"role":"","tables":["one"],"privileges":["select"]}]`, InvalidConfiguration, 180000},
		{"missing privileges", `[{"role":"reader","tables":["one"]}]`, InvalidConfiguration, 180000},
		{"empty privileges", `[{"role":"reader","tables":["one"],"privileges":[]}]`, InvalidConfiguration, 180000},
		{"empty tables", `[{"role":"reader","tables":[],"privileges":["select"]}]`, InvalidConfiguration, 180000},
		{"null tables", `[{"role":"reader","tables":null,"privileges":["select"]}]`, InvalidConfiguration, 180000},
		{"unsupported privilege", `[{"role":"reader","tables":["one"],"privileges":["all"]}]`, InvalidConfiguration, 180000},
		{"invalid ensure", `[{"role":"reader","tables":["one"],"privileges":["select"],"ensure":"drop"}]`, InvalidConfiguration, 180000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := preflightACL(&DatabaseState{ServerVersion: tc.version, MaxIdentifierLength: 63}, roleTableSpec(t, tc.rules))
			if tc.reason == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			failure, ok := err.(*Failure)
			if !ok || failure.Reason != tc.reason {
				t.Fatalf("got %v, want reason %s", err, tc.reason)
			}
		})
	}
}

func TestRoleTableDependenciesTrackPresentRecipientsAndCreators(t *testing.T) {
	var spec epco.DatabaseSpec
	err := json.Unmarshal([]byte(`{
	 "permissions":{"connect":[{"role":"database_user"}]},
	 "defaultPrivileges":[{"forRole":"creator","tables":{"select":[{"role":"future_reader"}]}}],
	 "schemas":[{"name":"app","permissions":{"usage":[{"role":"schema_user"}],"sequences":{"usage":[{"role":"sequence_user"}]}},
	 "tablePermissions":[{"role":"reader","privileges":["select"]},{"role":"reader","tables":["one"],"privileges":["insert"]},
	 {"role":"public","privileges":["delete"]},{"role":"withdrawn","privileges":["select"],"ensure":"absent"}]}]}`), &spec)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"creator", "database_user", "future_reader", "reader", "schema_user", "sequence_user"}
	if got := ACLRoleDependencies(spec); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want present recipients and actual creator %q", got, want)
	}
}

func TestRoleTableNullSelectorDoesNotBecomeBroadThroughConversion(t *testing.T) {
	raw := map[string]any{"schemas": []any{map[string]any{"name": "app", "tablePermissions": []any{map[string]any{
		roleTableRoleField: "reader", "tables": nil, "privileges": []any{"select"}}}}}}
	var spec epco.DatabaseSpec
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &spec); err != nil {
		t.Fatal(err)
	}
	err := preflightACL(&DatabaseState{ServerVersion: 180000, MaxIdentifierLength: 63}, spec)
	failure, ok := err.(*Failure)
	if !ok || failure.Reason != InvalidConfiguration {
		t.Fatalf("null selector decoded through Kubernetes converter became valid: %v", err)
	}
	converted, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&spec)
	if err != nil {
		t.Fatal(err)
	}
	rule := converted["schemas"].([]any)[0].(map[string]any)["tablePermissions"].([]any)[0].(map[string]any)
	selector, found := rule["tables"]
	if !found || selector == nil || reflect.ValueOf(selector).Len() != 0 {
		t.Fatalf("invalid null selector must roundtrip as explicit empty selection, got %#v", rule)
	}
}
