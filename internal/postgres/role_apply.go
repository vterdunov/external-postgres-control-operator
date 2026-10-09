package postgres

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

func (s *roleSession) Apply(ctx context.Context, desired RoleDesired) (*RoleApplyResult, error) {
	bounded, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	tx, err := s.connection.Begin(bounded)
	if err != nil {
		return nil, roleFailure(err)
	}
	defer func() {
		cleanup, cleanupCancel := context.WithTimeout(context.Background(), time.Second)
		defer cleanupCancel()
		_ = tx.Rollback(cleanup)
	}()
	state, err := inspectRole(bounded, tx, desired.Name)
	if err != nil {
		return nil, roleFailure(err)
	}
	if err = validateRoleName(desired.Name, state.MaxIdentifierLength); err != nil {
		return nil, err
	}
	if IsProtectedRole(desired.Name, state.AdminUser) {
		return nil, &Failure{ProtectedRoleReason, "The SQL role is protected from management"}
	}
	parameters, err := normalizedRoleParameters(desired.Spec.Parameters, state.ServerVersion)
	if err != nil {
		return nil, err
	}
	desired.Spec.Parameters = parameters
	desired.TrackedParameters = slices.Clone(desired.TrackedParameters)
	for i, name := range desired.TrackedParameters {
		desired.TrackedParameters[i] = CanonicalRoleParameterName(name, state.ServerVersion)
	}
	var recovery, readOnly bool
	if err = tx.QueryRow(bounded, "SELECT pg_is_in_recovery(), current_setting('transaction_read_only')::boolean").Scan(&recovery, &readOnly); err != nil {
		return nil, roleFailure(err)
	}
	if recovery || readOnly {
		return nil, &Failure{NotWritable, readOnlyMessage}
	}
	if err = applyAttributes(bounded, tx, state, desired); err != nil {
		return nil, roleFailure(err)
	}
	// Refresh membership defaults and CREATE ROLE's automatic administrative bindings.
	state, err = inspectRole(bounded, tx, desired.Name)
	if err != nil {
		return nil, roleFailure(err)
	}
	bindings, err := applyMemberships(bounded, tx, state, desired)
	if err != nil {
		return nil, roleFailure(err)
	}
	names, err := applyParameters(bounded, tx, state, desired)
	if err != nil {
		return nil, roleFailure(err)
	}
	if err = tx.Commit(bounded); err != nil {
		return nil, roleFailure(err)
	}
	return &RoleApplyResult{Memberships: bindings, Parameters: names}, nil
}

func literal(value string) string {
	return "E'" + strings.ReplaceAll(strings.ReplaceAll(value, "\\", "\\\\"), "'", "''") + "'"
}

func applyAttributes(ctx context.Context, q roleQuerier, state *RoleState, desired RoleDesired) error {
	spec := desired.Spec
	options := make([]string, 0, 10)
	inherit, limit := true, int64(-1)
	if spec.Inherit != nil {
		inherit = *spec.Inherit
	}
	if spec.ConnectionLimit != nil {
		limit = *spec.ConnectionLimit
	}
	for _, attribute := range []struct {
		name             string
		current, desired bool
		privileged       bool
	}{
		{"LOGIN", state.Login, spec.Login, false}, {"INHERIT", state.Inherit, inherit, false},
		{"CREATEDB", state.CreateDB, spec.CreateDB, false}, {"CREATEROLE", state.CreateRole, spec.CreateRole, false},
		{"SUPERUSER", state.Superuser, spec.Superuser, true}, {"REPLICATION", state.Replication, spec.Replication, true},
		{"BYPASSRLS", state.BypassRLS, spec.BypassRLS, true},
	} {
		if state.Exists && attribute.current == attribute.desired {
			continue
		}
		if !state.Exists && attribute.privileged && !attribute.desired {
			continue
		}
		value := attribute.name
		if !attribute.desired {
			value = "NO" + value
		}
		options = append(options, value)
	}
	if !state.Exists || state.ConnectionLimit != limit {
		options = append(options, "CONNECTION LIMIT "+strconv.FormatInt(limit, 10))
	}
	if spec.ValidUntil != nil {
		if state.ValidUntil == nil || !state.ValidUntil.Equal(spec.ValidUntil.Time) {
			options = append(options, "VALID UNTIL "+literal(spec.ValidUntil.UTC().Format(time.RFC3339Nano)))
		}
	} else if state.ValidUntil != nil || state.ValidUntilNegativeInfinity {
		options = append(options, "VALID UNTIL 'infinity'")
	}
	if desired.Password != nil {
		if strings.ContainsRune(*desired.Password, 0) {
			return &Failure{InvalidSecret, "Role password must not contain a NUL byte"}
		}
		verifier, err := passwordVerifier(*desired.Password)
		if err != nil {
			return err
		}
		options = append(options, "PASSWORD "+literal(verifier))
	} else if spec.DisablePassword {
		options = append(options, "PASSWORD NULL")
	}
	verb := "ALTER ROLE "
	if !state.Exists {
		verb = "CREATE ROLE "
	}
	name := pgx.Identifier{desired.Name}.Sanitize()
	if len(options) != 0 || !state.Exists {
		if _, err := q.Exec(ctx, verb+name+" "+strings.Join(options, " ")); err != nil {
			return err
		}
	}
	comment := ""
	if spec.Comment != nil {
		comment = *spec.Comment
	}
	if state.Comment != comment {
		if strings.ContainsRune(comment, 0) {
			return &Failure{InvalidConfiguration, "Role comment contains an invalid byte"}
		}
		if _, err := q.Exec(ctx, "COMMENT ON ROLE "+name+" IS "+literal(comment)); err != nil {
			return err
		}
	}
	return nil
}

func applyParameters(ctx context.Context, q roleQuerier, state *RoleState, desired RoleDesired) ([]string, error) {
	role := pgx.Identifier{desired.Name}.Sanitize()
	for _, name := range desired.TrackedParameters {
		if _, declared := desired.Spec.Parameters[name]; declared {
			continue
		}
		if err := validateParameter(name, ""); err != nil {
			return nil, err
		}
		if _, err := q.Exec(ctx, "ALTER ROLE "+role+" RESET "+pgx.Identifier{name}.Sanitize()); err != nil {
			return nil, err
		}
	}
	names := slices.Sorted(maps.Keys(desired.Spec.Parameters))
	for _, name := range names {
		value := desired.Spec.Parameters[name]
		if err := validateParameter(name, value); err != nil {
			return nil, err
		}
		if current, present := state.Parameters[name]; present && current == value {
			continue
		}
		if _, err := q.Exec(ctx, "ALTER ROLE "+role+" SET "+pgx.Identifier{name}.Sanitize()+" TO "+literal(value)); err != nil {
			return nil, err
		}
	}
	return names, nil
}

func validateParameter(name, value string) error {
	if name == "" || strings.EqualFold(name, "all") || strings.ContainsRune(name, 0) || strings.ContainsRune(value, 0) {
		return &Failure{InvalidConfiguration, "A role parameter name or value is invalid"}
	}
	return nil
}

// CanonicalRoleParameterName resolves PostgreSQL's case-insensitive and legacy setting names.
func CanonicalRoleParameterName(name string, serverVersion int) string {
	name = strings.ToLower(name)
	switch name {
	case "sort_mem":
		return "work_mem"
	case "vacuum_mem":
		return "maintenance_work_mem"
	case "ssl_ecdh_curve":
		if serverVersion >= 180000 {
			return "ssl_groups"
		}
	}
	return name
}

func normalizedRoleParameters(parameters map[string]string, serverVersion int) (map[string]string, error) {
	normalized := make(map[string]string, len(parameters))
	for name, value := range parameters {
		if err := validateParameter(name, value); err != nil {
			return nil, err
		}
		name = CanonicalRoleParameterName(name, serverVersion)
		if previous, exists := normalized[name]; exists && previous != value {
			return nil, &Failure{InvalidConfiguration, "Conflicting declarations refer to the same role parameter"}
		}
		normalized[name] = value
	}
	return normalized, nil
}

func membershipGrantor(ctx context.Context, q roleQuerier, state *RoleState, parent string) (string, error) {
	var exists, super, createRole bool
	if err := q.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1),rolsuper,rolcreaterole FROM pg_roles WHERE rolname=current_user", parent).Scan(&exists, &super, &createRole); err != nil {
		return "", err
	}
	if !exists {
		return "", &Failure{DependenciesNotReady, "A required parent SQL role does not exist"}
	}
	if super {
		var bootstrap string
		if err := q.QueryRow(ctx, "SELECT rolname FROM pg_roles WHERE oid=10").Scan(&bootstrap); err != nil {
			return "", err
		}
		return bootstrap, nil
	}
	if state.ServerVersion < 160000 && createRole {
		var protected bool
		if err := q.QueryRow(ctx, "SELECT rolsuper FROM pg_roles WHERE rolname=$1", parent).Scan(&protected); err != nil {
			return "", err
		}
		if !protected {
			return state.AdminUser, nil
		}
	}
	var authorized bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_auth_members m
		JOIN pg_roles r ON r.oid=m.roleid JOIN pg_roles u ON u.oid=m.member
		WHERE r.rolname=$1 AND u.rolname=current_user AND m.admin_option)`, parent).Scan(&authorized); err != nil {
		return "", err
	}
	if !authorized {
		return "", &Failure{InsufficientPrivileges, "The administrative connection cannot manage the requested parent membership"}
	}
	return state.AdminUser, nil
}

func bindingMatches(a, b epco.RoleMembershipBinding) bool {
	return a.Role == b.Role && a.Member == b.Member && a.Grantor == b.Grantor
}

func applyMemberships(ctx context.Context, q roleQuerier, state *RoleState, desired RoleDesired) ([]epco.RoleMembershipBinding, error) {
	declared := make(map[string]struct{}, len(desired.Spec.InRoles))
	result := make([]epco.RoleMembershipBinding, 0, len(desired.Spec.InRoles)+len(desired.TrackedMemberships))
	for _, parent := range desired.Spec.InRoles {
		name := string(parent)
		if err := validateRoleName(name, state.MaxIdentifierLength); err != nil {
			return nil, err
		}
		grantor, err := membershipGrantor(ctx, q, state, name)
		if err != nil {
			return nil, err
		}
		binding := epco.RoleMembershipBinding{Role: name, Member: desired.Name, Grantor: grantor}
		declared[name] = struct{}{}
		present := slices.ContainsFunc(state.Memberships, func(current epco.RoleMembershipBinding) bool {
			return current.Role == name && current.Member == desired.Name
		})
		if present {
			// Credential changes do not transfer or erase an existing managed grantor binding.
			result = append(result, trackedParentBindings(state, desired, name)...)
			continue
		}
		statement := "GRANT " + pgx.Identifier{name}.Sanitize() + " TO " + pgx.Identifier{desired.Name}.Sanitize()
		if state.ServerVersion >= 160000 {
			statement += " GRANTED BY " + pgx.Identifier{grantor}.Sanitize()
		}
		if _, err = q.Exec(ctx, statement); err != nil {
			return nil, err
		}
		result = append(result, binding)
	}
	remove := make([]epco.RoleMembershipBinding, 0)
	for _, current := range state.Memberships {
		if current.Member != desired.Name || current.Administrative {
			continue
		}
		if _, keep := declared[current.Role]; keep {
			continue
		}
		tracked := slices.ContainsFunc(desired.TrackedMemberships, func(old epco.RoleMembershipBinding) bool {
			return !old.Administrative && bindingMatches(old, current)
		})
		if desired.Spec.MembershipPolicy == epco.MembershipManaged && !tracked {
			continue
		}
		remove = append(remove, current)
	}
	for _, binding := range remove {
		if _, err := membershipGrantor(ctx, q, state, binding.Role); err != nil {
			return nil, err
		}
		statement := "REVOKE " + pgx.Identifier{binding.Role}.Sanitize() + " FROM " + pgx.Identifier{binding.Member}.Sanitize()
		if state.ServerVersion >= 160000 {
			statement += " GRANTED BY " + pgx.Identifier{binding.Grantor}.Sanitize()
		}
		statement += " RESTRICT"
		if _, err := q.Exec(ctx, statement); err != nil {
			return nil, err
		}
	}
	final, err := inspectRole(ctx, q, desired.Name)
	if err != nil {
		return nil, err
	}
	if err = verifyRoleMemberships(final, desired); err != nil {
		return nil, err
	}
	for _, binding := range result {
		if !slices.ContainsFunc(final.Memberships, func(current epco.RoleMembershipBinding) bool {
			return bindingMatches(current, binding)
		}) {
			return nil, &Failure{InsufficientPrivileges, "A requested SQL membership was not granted"}
		}
	}
	for _, binding := range remove {
		if slices.ContainsFunc(final.Memberships, func(current epco.RoleMembershipBinding) bool {
			return bindingMatches(current, binding)
		}) {
			return nil, &Failure{InsufficientPrivileges, "A requested SQL membership could not be removed"}
		}
	}
	for _, binding := range final.Memberships {
		if binding.Administrative {
			result = append(result, binding)
		}
	}
	return result, nil
}

func trackedParentBindings(state *RoleState, desired RoleDesired, parent string) []epco.RoleMembershipBinding {
	bindings := make([]epco.RoleMembershipBinding, 0)
	for _, current := range state.Memberships {
		if current.Role == parent && current.Member == desired.Name && slices.ContainsFunc(desired.TrackedMemberships, func(old epco.RoleMembershipBinding) bool {
			return !old.Administrative && bindingMatches(old, current)
		}) {
			bindings = append(bindings, current)
		}
	}
	return bindings
}

func verifyRoleMemberships(state *RoleState, desired RoleDesired) error {
	for _, parent := range desired.Spec.InRoles {
		if !slices.ContainsFunc(state.Memberships, func(binding epco.RoleMembershipBinding) bool {
			return binding.Role == string(parent) && binding.Member == desired.Name
		}) {
			return &Failure{InsufficientPrivileges, "A requested SQL membership is absent after reconciliation"}
		}
	}
	if desired.Spec.MembershipPolicy == epco.MembershipManaged {
		return nil
	}
	for _, binding := range state.Memberships {
		if binding.Member == desired.Name && !binding.Administrative && !slices.Contains(desired.Spec.InRoles, epco.SQLName(binding.Role)) {
			return &Failure{InsufficientPrivileges, "An undeclared authoritative SQL membership remains"}
		}
	}
	return nil
}
