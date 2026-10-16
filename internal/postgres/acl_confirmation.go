package postgres

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

func aclIdentityFailure(err error) error {
	if err == nil || errors.Is(err, pgx.ErrNoRows) {
		return &Failure{ACLBindingUnresolved, "A checkpointed ACL identity changed before SQL confirmation"}
	}
	return databaseFailure(err)
}

// Confirm the name-based statement's actual catalog identities before commit.
// Relation GRANT/REVOKE holds its relation lock through this confirmation; the
// other classes use their own catalog identities, not an assumed relation lock.
func (e *aclEngine) confirmBindingIdentity(ctx context.Context, q roleQuerier, b epco.ACLBinding) error {
	var databaseOID, databaseOwner uint32
	if err := q.QueryRow(ctx, "SELECT oid,datdba FROM pg_catalog.pg_database WHERE datname OPERATOR(pg_catalog.=) $1", e.desired.Name).Scan(&databaseOID, &databaseOwner); err != nil {
		return aclIdentityFailure(err)
	}
	if databaseOID != b.DatabaseOID {
		return aclIdentityFailure(nil)
	}
	if b.Class == aclClassDatabase {
		if b.ObjectOID != databaseOID || b.OwnerOID != databaseOwner {
			return aclIdentityFailure(nil)
		}
	} else if b.Scope == aclScopeDefault || b.Class == aclClassSchema {
		if b.Schema != "" {
			var oid, owner uint32
			if err := q.QueryRow(ctx, "SELECT oid,nspowner FROM pg_catalog.pg_namespace WHERE nspname OPERATOR(pg_catalog.=) $1", b.Schema).Scan(&oid, &owner); err != nil {
				return aclIdentityFailure(err)
			}
			if oid != b.SchemaOID || b.Class == aclClassSchema && (oid != b.ObjectOID || owner != b.OwnerOID) {
				return aclIdentityFailure(nil)
			}
		}
	} else {
		var oid, schema, owner uint32
		var kind string
		if err := q.QueryRow(ctx, `SELECT c.oid,c.relnamespace,c.relowner,c.relkind::pg_catalog.text FROM pg_catalog.pg_class c
			JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2`, b.Schema, b.Table).
			Scan(&oid, &schema, &owner, &kind); err != nil {
			return aclIdentityFailure(err)
		}
		validKind := b.Class == aclClassSequence && kind == "S" ||
			b.Class == aclClassTable && slices.Contains([]string{"r", "p", "f", "v", "m"}, kind)
		if oid != b.ObjectOID || schema != b.SchemaOID || owner != b.OwnerOID || !validKind {
			return aclIdentityFailure(nil)
		}
	}
	for _, role := range []epco.ACLRoleIdentity{
		{Name: b.Grantee, OID: b.GranteeOID}, {Name: b.Creator, OID: b.CreatorOID},
		{Name: b.Grantor, OID: b.GrantorOID}, {Name: b.Owner, OID: b.OwnerOID},
	} {
		if role.Name == "" || role.Name == aclPublic {
			continue
		}
		if err := confirmACLRoleIdentity(ctx, q, role); err != nil {
			return err
		}
	}
	return nil
}

func confirmACLRoleIdentity(ctx context.Context, q roleQuerier, role epco.ACLRoleIdentity) error {
	var oid uint32
	if err := q.QueryRow(ctx, "SELECT oid FROM pg_catalog.pg_roles WHERE rolname OPERATOR(pg_catalog.=) $1", role.Name).Scan(&oid); err != nil {
		return aclIdentityFailure(err)
	}
	if oid != role.OID {
		return aclIdentityFailure(nil)
	}
	return nil
}

func installedACLGrant(rows []aclRow, b epco.ACLBinding) int {
	return slices.IndexFunc(rows, func(row aclRow) bool {
		return row.grantee == b.GranteeOID && row.privilege == b.Privilege &&
			(row.grantor == b.GrantorOID ||
				(slices.ContainsFunc(b.GrantorCandidates, func(candidate epco.ACLRoleIdentity) bool { return candidate.OID == row.grantor }) &&
					!slices.ContainsFunc(b.Before, func(before epco.ACLGrantIdentity) bool { return before.Grantor.OID == row.grantor })))
	})
}

func (e *aclEngine) confirmMutation(ctx context.Context, q roleQuerier, b epco.ACLBinding, grant bool, before []aclRow) error {
	if err := e.confirmBindingIdentity(ctx, q, b); err != nil {
		return err
	}
	after, err := readACLRows(ctx, q, b)
	if err != nil {
		return databaseFailure(err)
	}
	if !grant {
		if aclRowPresent(after, b) {
			return &Failure{InsufficientPrivileges, "PostgreSQL did not remove the recorded direct privilege"}
		}
		for _, row := range matchingACLRows(before, b) {
			if row.grantor != b.GrantorOID && !slices.Contains(after, row) {
				return &Failure{ACLBindingUnresolved, "SQL cleanup changed an independent ACL grantor binding"}
			}
		}
		return nil
	}
	selected := installedACLGrant(after, b)
	if selected < 0 {
		return &Failure{InsufficientPrivileges, "PostgreSQL did not install a privilege with confirmed grantor provenance"}
	}
	grantor := epco.ACLRoleIdentity{Name: b.Grantor, OID: after[selected].grantor}
	if grantor.Name == "" || grantor.OID != b.GrantorOID {
		candidate := slices.IndexFunc(b.GrantorCandidates, func(role epco.ACLRoleIdentity) bool { return role.OID == grantor.OID })
		if candidate < 0 {
			return aclIdentityFailure(nil)
		}
		grantor = b.GrantorCandidates[candidate]
	}
	return confirmACLRoleIdentity(ctx, q, grantor)
}
