package postgres_test

import (
	"testing"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

const expectedAdministrativeSearchPath = "pg_catalog, pg_temp"

func TestAdministrativeConfigPinsSearchPathAndStatementTimeout(t *testing.T) {
	for _, test := range []struct {
		name    string
		timeout epco.PositiveDuration
		want    string
	}{
		{name: "defaults", want: "30000"},
		{name: "explicit timeout", timeout: "17s", want: "17000"},
		{name: "submillisecond timeout", timeout: "1ns", want: "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			descriptor := cluster()
			descriptor.Spec.StatementTimeout = test.timeout
			config := build(t, descriptor, credentials())
			if got := config.Connection.RuntimeParams["search_path"]; got != expectedAdministrativeSearchPath {
				t.Errorf("administrative startup search_path=%q, want %q", got, expectedAdministrativeSearchPath)
			}
			if got := config.Connection.RuntimeParams["statement_timeout"]; got != test.want {
				t.Errorf("administrative startup statement_timeout=%q, want %q", got, test.want)
			}
		})
	}
}

func TestAdministrativeConfigSearchPathSurvivesTargetCopies(t *testing.T) {
	config := build(t, cluster(), credentials())
	target := config.Connection.Copy()
	target.Database = "application"
	if got := target.RuntimeParams["search_path"]; got != expectedAdministrativeSearchPath {
		t.Errorf("target startup search_path=%q, want %q", got, expectedAdministrativeSearchPath)
	}
	if got := target.RuntimeParams["statement_timeout"]; got != "30000" {
		t.Errorf("target startup statement_timeout=%q, want 30000", got)
	}
	target.RuntimeParams["search_path"] = "public"
	if got := config.Connection.RuntimeParams["search_path"]; got != expectedAdministrativeSearchPath {
		t.Errorf("changing a target copy changed the administrative startup search_path to %q", got)
	}
	if got := build(t, cluster(), credentials()).Connection.RuntimeParams["search_path"]; got != expectedAdministrativeSearchPath {
		t.Errorf("a fresh configuration inherited another connection's search_path %q", got)
	}
}
