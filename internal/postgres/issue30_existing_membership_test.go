package postgres

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

const (
	issue30ExistingChild  = "application"
	issue30ExistingAdmin  = "operator_login"
	issue30ExistingParent = "reader_group"
)

// This catalog has an independently granted, already-satisfied membership. The
// supplied connection may inspect it but has no authority to change memberships.
type issue30MembershipCatalog struct {
	mutations int
}

func TestIssue30ExistingIndependentMembershipNeedsNoMutationAuthority(t *testing.T) {
	for _, policy := range []epco.MembershipPolicy{epco.MembershipManaged, epco.MembershipAuthoritative} {
		t.Run(string(policy), func(t *testing.T) {
			catalog := &issue30MembershipCatalog{}
			binding := epco.RoleMembershipBinding{Role: issue30ExistingParent, Member: issue30ExistingChild, Grantor: "independent_dba"}
			state := &RoleState{Exists: true, Name: issue30ExistingChild, AdminUser: issue30ExistingAdmin, ServerVersion: 160015,
				MaxIdentifierLength: 63, Memberships: []epco.RoleMembershipBinding{binding}}
			desired := RoleDesired{Name: issue30ExistingChild, Spec: epco.DatabaseRoleSpec{
				InRoles: []epco.SQLName{issue30ExistingParent}, MembershipPolicy: policy,
			}}

			owned, err := applyMemberships(t.Context(), catalog, state, desired)
			if err != nil {
				t.Fatalf("an existing independent membership must satisfy inRoles without mutation authority: %v", err)
			}
			if catalog.mutations != 0 {
				t.Fatalf("already-satisfied membership caused %d SQL mutations", catalog.mutations)
			}
			if len(owned) != 0 {
				t.Fatalf("independent membership was acquired as cleanup responsibility: %#v", owned)
			}
			if !slices.Equal(state.Memberships, []epco.RoleMembershipBinding{binding}) {
				t.Fatalf("independent membership changed: %#v", state.Memberships)
			}
		})
	}
}

func (*issue30MembershipCatalog) QueryRow(_ context.Context, query string, _ ...any) pgx.Row {
	switch {
	case strings.Contains(query, "m.admin_option"):
		return issue30CatalogRow{values: []any{false}}
	case strings.Contains(query, "SELECT EXISTS"):
		return issue30CatalogRow{values: []any{true, false, false}}
	case strings.Contains(query, "SELECT current_user"):
		return issue30CatalogRow{values: []any{issue30ExistingAdmin, 160015, 63}}
	case strings.Contains(query, "SELECT bootstrap.rolname"):
		return issue30CatalogRow{values: []any{"catalog_bootstrap", issue30ExistingAdmin}}
	case strings.Contains(query, "SELECT rolcanlogin"):
		return issue30CatalogRow{values: []any{true, true, false, false, false, false, false, int64(-1), "", pgtype.Timestamptz{}}}
	default:
		return issue30CatalogRow{err: fmt.Errorf("unsupported catalog query: %s", query)}
	}
}

func (*issue30MembershipCatalog) Query(_ context.Context, query string, _ ...any) (pgx.Rows, error) {
	switch {
	case strings.Contains(query, "pg_auth_members"):
		return &issue30CatalogRows{rows: [][]any{{issue30ExistingParent, issue30ExistingChild, "independent_dba", false}}}, nil
	case strings.Contains(query, "pg_db_role_setting"):
		return &issue30CatalogRows{}, nil
	default:
		return nil, fmt.Errorf("unsupported catalog query: %s", query)
	}
}

func (c *issue30MembershipCatalog) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	c.mutations++
	return pgconn.CommandTag{}, &pgconn.PgError{Code: "42501", Message: "membership mutation denied"}
}

type issue30CatalogRow struct {
	values []any
	err    error
}

func (r issue30CatalogRow) Scan(destinations ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(destinations) != len(r.values) {
		return fmt.Errorf("catalog columns: got %d destinations for %d values", len(destinations), len(r.values))
	}
	for i, destination := range destinations {
		pointer := reflect.ValueOf(destination)
		value := reflect.ValueOf(r.values[i])
		if pointer.Kind() != reflect.Pointer || !value.Type().AssignableTo(pointer.Elem().Type()) {
			return fmt.Errorf("catalog column %d cannot scan %T into %T", i, r.values[i], destination)
		}
		pointer.Elem().Set(value)
	}
	return nil
}

type issue30CatalogRows struct {
	rows    [][]any
	current int
}

func (*issue30CatalogRows) Close()                                       {}
func (*issue30CatalogRows) Err() error                                   { return nil }
func (*issue30CatalogRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (*issue30CatalogRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (*issue30CatalogRows) RawValues() [][]byte                          { return nil }
func (*issue30CatalogRows) Conn() *pgx.Conn                              { return nil }

func (r *issue30CatalogRows) Next() bool {
	if r.current >= len(r.rows) {
		return false
	}
	r.current++
	return true
}

func (r *issue30CatalogRows) Scan(destinations ...any) error {
	return (issue30CatalogRow{values: r.rows[r.current-1]}).Scan(destinations...)
}

func (r *issue30CatalogRows) Values() ([]any, error) {
	return slices.Clone(r.rows[r.current-1]), nil
}
