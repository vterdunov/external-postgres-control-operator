package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

const (
	maximumMembershipGrantors = 32
	membershipRollbackTimeout = time.Second
)

func authorizationRefused(err error) bool {
	server, ok := errors.AsType[*pgconn.PgError](err)
	return ok && (server.Code == "42501" || server.Code == "0LP01")
}

func checkpointMembership(ctx context.Context, desired RoleDesired, binding epco.RoleMembershipBinding, pending bool) error {
	if desired.CheckpointMembership == nil {
		return nil
	}
	return desired.CheckpointMembership(ctx, binding, pending)
}

// membershipGrantors does not equate catalog ADMIN rows with all possible
// backend authority. The supplied principal always gets the first bounded try.
func membershipGrantors(ctx context.Context, q roleQuerier, state *RoleState, parent string) ([]serviceGrantor, error) {
	identity, err := readServiceRoleIdentity(ctx, q, parent)
	if err != nil {
		return nil, err
	}
	var super bool
	if err = q.QueryRow(ctx, `SELECT rolsuper FROM pg_catalog.pg_roles WHERE oid OPERATOR(pg_catalog.=) $1`, identity.loginOID).Scan(&super); err != nil {
		return nil, err
	}
	grantor := serviceGrantor{name: identity.login, oid: identity.loginOID}
	if super {
		grantor.name = state.BootstrapUser
		if err = q.QueryRow(ctx, `SELECT oid FROM pg_catalog.pg_roles WHERE rolname OPERATOR(pg_catalog.=) $1`, grantor.name).Scan(&grantor.oid); err != nil {
			return nil, err
		}
	}
	candidates, err := reachableServiceGrantors(ctx, q, identity)
	if err != nil {
		return nil, err
	}
	result := make([]serviceGrantor, 1, 1+len(candidates))
	result[0] = grantor
	return append(result, candidates...), nil
}

func grantRoleMembership(ctx context.Context, q roleQuerier, state *RoleState, desired RoleDesired, parent string) (epco.RoleMembershipBinding, error) {
	grantors, err := membershipGrantors(ctx, q, state, parent)
	if err != nil {
		return epco.RoleMembershipBinding{}, err
	}
	for _, grantor := range grantors {
		binding := epco.RoleMembershipBinding{Role: parent, Member: desired.Name, Grantor: grantor.name}
		if err = checkpointMembership(ctx, desired, binding, true); err != nil {
			return binding, err
		}
		err = executeMembershipCandidate(ctx, q, state, grantor, binding, false)
		if err == nil {
			return binding, nil
		}
		if !authorizationRefused(err) {
			return binding, err
		}
		// executeMembershipCandidate has synchronously rolled back its savepoint.
		// Never retire provenance from a refused or indeterminate transaction.
		present, inspectErr := membershipBindingPresent(ctx, q, binding)
		if inspectErr != nil {
			return binding, inspectErr
		}
		if present {
			return binding, &Failure{DependenciesNotReady, "A refused membership candidate has an unconfirmed SQL binding"}
		}
		if err = checkpointMembership(ctx, desired, binding, false); err != nil {
			return binding, err
		}
	}
	return epco.RoleMembershipBinding{}, &Failure{InsufficientPrivileges, "The administrative connection cannot manage the requested parent membership"}
}

func revokeRoleMembership(ctx context.Context, q roleQuerier, state *RoleState, desired RoleDesired, binding epco.RoleMembershipBinding) error {
	grantors, err := membershipGrantors(ctx, q, state, binding.Role)
	if err != nil {
		return err
	}
	if err = checkpointMembership(ctx, desired, binding, true); err != nil {
		return err
	}
	for _, grantor := range grantors {
		err = executeMembershipCandidate(ctx, q, state, grantor, binding, true)
		if err == nil {
			return nil
		}
		if !authorizationRefused(err) {
			return err
		}
	}
	return &Failure{InsufficientPrivileges, "The administrative connection cannot remove the recorded parent membership"}
}

func membershipBindingPresent(ctx context.Context, q roleQuerier, binding epco.RoleMembershipBinding) (bool, error) {
	var present bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_auth_members m
 JOIN pg_catalog.pg_roles p ON p.oid OPERATOR(pg_catalog.=) m.roleid JOIN pg_catalog.pg_roles c ON c.oid OPERATOR(pg_catalog.=) m.member
 JOIN pg_catalog.pg_roles g ON g.oid OPERATOR(pg_catalog.=) m.grantor WHERE p.rolname OPERATOR(pg_catalog.=) $1 AND c.rolname OPERATOR(pg_catalog.=) $2 AND g.rolname OPERATOR(pg_catalog.=) $3)`,
		binding.Role, binding.Member, binding.Grantor).Scan(&present)
	return present, err
}

func executeMembershipCandidate(ctx context.Context, q roleQuerier, state *RoleState, grantor serviceGrantor, binding epco.RoleMembershipBinding, revoke bool) (err error) {
	parent, ok := q.(pgx.Tx)
	if !ok {
		return &Failure{InvalidConfiguration, "Membership mutations require an isolated SQL transaction"}
	}
	tx, err := parent.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), membershipRollbackTimeout)
			defer cancel()
			if rollbackErr := tx.Rollback(cleanup); rollbackErr != nil {
				err = &Failure{ConnectionUnavailable, "Unable to confirm membership candidate rollback"}
			}
		}
	}()
	var actualName string
	if err = tx.QueryRow(ctx, `SELECT rolname FROM pg_catalog.pg_roles WHERE oid OPERATOR(pg_catalog.=) $1`, grantor.oid).Scan(&actualName); err != nil {
		return err
	}
	if actualName != grantor.name {
		return &Failure{DependenciesNotReady, "SQL grantor identity changed while preparing membership"}
	}
	if grantor.switchRole {
		if _, err = tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{grantor.name}.Sanitize()); err != nil {
			return err
		}
	}
	statement := "GRANT " + pgx.Identifier{binding.Role}.Sanitize() + " TO " + pgx.Identifier{binding.Member}.Sanitize()
	if revoke {
		statement = "REVOKE " + pgx.Identifier{binding.Role}.Sanitize() + " FROM " + pgx.Identifier{binding.Member}.Sanitize()
	}
	if state.ServerVersion >= 160000 {
		statement += " GRANTED BY " + pgx.Identifier{binding.Grantor}.Sanitize()
	}
	if revoke {
		statement += " RESTRICT"
	}
	if _, err = tx.Exec(ctx, statement); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "RESET ROLE"); err != nil {
		return err
	}
	present, err := membershipBindingPresent(ctx, tx, binding)
	if err != nil {
		return err
	}
	if present == revoke {
		return &Failure{InsufficientPrivileges, "The requested exact SQL membership could not be confirmed"}
	}
	return tx.Commit(ctx)
}
