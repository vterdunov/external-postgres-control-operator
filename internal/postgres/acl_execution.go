package postgres

import (
	"context"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

func validACLPrivilege(b epco.ACLBinding) bool {
	var allowed []string
	switch b.Class {
	case aclClassDatabase:
		allowed = []string{aclConnect, aclCreate, "TEMPORARY"}
	case aclClassSchema:
		allowed = []string{aclUsage, aclCreate}
	case aclClassTable:
		allowed = []string{aclSelect, "INSERT", aclUpdate, "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER", aclMaintain}
	case aclClassSequence:
		allowed = []string{aclUsage, aclSelect, aclUpdate}
	case aclClassFunction:
		allowed = []string{"EXECUTE"}
	case aclClassType:
		allowed = []string{aclUsage}
	}
	return slices.Contains(allowed, b.Privilege)
}
func aclStatement(b epco.ACLBinding, grant bool) (string, error) {
	if !validACLPrivilege(b) {
		return "", &Failure{InvalidConfiguration, "Invalid recorded ACL object class or privilege"}
	}
	grantee := pgx.Identifier{b.Grantee}.Sanitize()
	if b.Grantee == aclPublic {
		grantee = aclPublic
	}
	action, direction, end := "REVOKE ", " FROM ", " RESTRICT"
	if grant {
		action, direction, end = "GRANT ", " TO ", ""
	}
	if b.Scope == aclScopeDefault {
		classes := map[string]string{aclClassTable: "TABLES", aclClassSequence: "SEQUENCES", aclClassFunction: "FUNCTIONS", aclClassType: "TYPES"}
		class, ok := classes[b.Class]
		if !ok {
			return "", &Failure{InvalidConfiguration, "Invalid default ACL object class"}
		}
		prefix := "ALTER DEFAULT PRIVILEGES FOR ROLE " + pgx.Identifier{b.Creator}.Sanitize() + " "
		if b.Schema != "" {
			prefix += "IN SCHEMA " + pgx.Identifier{b.Schema}.Sanitize() + " "
		}
		return prefix + action + b.Privilege + " ON " + class + direction + grantee + end, nil
	}
	class, name := "", ""
	switch b.Class {
	case aclClassDatabase:
		class = "DATABASE"
		name = pgx.Identifier{b.Table}.Sanitize()
	case aclClassSchema:
		class = "SCHEMA"
		name = pgx.Identifier{b.Schema}.Sanitize()
	case aclClassTable:
		class = "TABLE"
		name = pgx.Identifier{b.Schema, b.Table}.Sanitize()
	case aclClassSequence:
		class = "SEQUENCE"
		name = pgx.Identifier{b.Schema, b.Table}.Sanitize()
	default:
		return "", &Failure{InvalidConfiguration, "Invalid concrete ACL object class"}
	}
	return action + b.Privilege + " ON " + class + " " + name + direction + grantee + end, nil
}

func (e *aclEngine) mutate(ctx context.Context, b epco.ACLBinding, grant bool) ([]aclRow, error) {
	statementBinding := b
	if b.Class == aclClassDatabase {
		statementBinding.Table = e.desired.Name
	}
	statement, err := aclStatement(statementBinding, grant)
	if err != nil {
		return nil, err
	}
	connection := e.connection(b)
	if !grant && b.Scope != aclScopeDefault {
		if err = e.authorizeExactRevoke(ctx, b); err != nil {
			return nil, err
		}
	}
	bounded, cancel := context.WithTimeout(ctx, e.session.config.StatementTimeout)
	defer cancel()
	tx, err := connection.Begin(bounded)
	if err != nil {
		return nil, databaseFailure(err)
	}
	defer rollbackDatabaseTransaction(tx)
	role := ""
	if b.Scope == aclScopeDefault {
		role = b.Creator
	} else if !grant && b.Grantor != "" {
		role = b.Grantor
	} else {
		owner := e.catalog.rolesByOID[b.OwnerOID]
		if owner.Name == databaseOwnerRole {
			owner = e.catalog.rolesByOID[e.catalog.database.ownerOID]
		}
		if !e.catalog.admin.super && !owner.inherited && owner.set {
			role = owner.Name
		}
	}
	if role == databaseOwnerRole {
		role = e.catalog.database.owner
	}
	if role != "" {
		authority, ok := e.catalog.roles[role]
		if !ok {
			return nil, &Failure{DependenciesNotReady, "An ACL grantor or creator role no longer exists"}
		}
		// Inherited grant-option paths remain valid even with SET FALSE. Exact
		// historical grantors use SET only when PostgreSQL authorizes it.
		if authority.set && (role != e.catalog.admin.Name) && b.Scope != aclScopeDefault {
			_, err = tx.Exec(bounded, "SET LOCAL ROLE "+pgx.Identifier{role}.Sanitize())
		} else if b.Scope == aclScopeDefault {
			err = selectDatabaseRole(bounded, tx, role, true)
		}
		if err != nil {
			return nil, databaseFailure(err)
		}
	}
	before, err := readACLRows(bounded, tx, b)
	if err != nil {
		return nil, databaseFailure(err)
	}
	if _, err = tx.Exec(bounded, statement); err != nil {
		return nil, databaseFailure(err)
	}
	if err = e.confirmMutation(bounded, tx, b, grant, before); err != nil {
		return nil, err
	}
	if err = tx.Commit(bounded); err != nil {
		return nil, databaseFailure(err)
	}
	rows, err := readACLRows(bounded, connection, b)
	return rows, databaseFailure(err)
}

// A warning after an arbitrary REVOKE cannot repair accidental removal from
// another grantor. Prove the exact selection before issuing historical cleanup.
func (e *aclEngine) authorizeExactRevoke(ctx context.Context, b epco.ACLBinding) error {
	role := e.catalog.rolesByOID[b.GrantorOID]
	if role.Name == databaseOwnerRole {
		role = e.catalog.rolesByOID[e.catalog.database.ownerOID]
	}
	if role.set || role.OID == e.catalog.admin.OID {
		return nil
	}
	owner := e.catalog.rolesByOID[b.OwnerOID]
	if owner.Name == databaseOwnerRole {
		owner = e.catalog.rolesByOID[e.catalog.database.ownerOID]
	}
	if owner.inherited {
		if b.GrantorOID == b.OwnerOID {
			return nil
		}
		return &Failure{InsufficientPrivileges, "Exact historical ACL grantor authority is unavailable"}
	}
	rows, err := readACLRows(ctx, e.queries(b), b)
	if err != nil {
		return databaseFailure(err)
	}
	eligible := map[uint32]bool{}
	for _, row := range rows {
		if row.privilege == b.Privilege && row.grantable && e.catalog.rolesByOID[row.grantee].inherited {
			eligible[row.grantee] = true
		}
	}
	if len(eligible) == 1 && eligible[b.GrantorOID] {
		return nil
	}
	return &Failure{InsufficientPrivileges, "Exact historical ACL grantor authority is unavailable or ambiguous"}
}

func (e *aclEngine) candidates(b epco.ACLBinding, rows []aclRow) []epco.ACLRoleIdentity {
	if b.Scope == aclScopeDefault {
		return []epco.ACLRoleIdentity{e.catalog.rolesByOID[b.CreatorOID].ACLRoleIdentity}
	}
	ids := map[uint32]bool{}
	owner := e.catalog.rolesByOID[b.OwnerOID]
	if e.catalog.admin.super || owner.inherited || owner.set || owner.Name == databaseOwnerRole && e.catalog.rolesByOID[e.catalog.database.ownerOID].set {
		ids[owner.OID] = true
	}
	for _, row := range rows {
		role := e.catalog.rolesByOID[row.grantee]
		if row.privilege == b.Privilege && row.grantable && role.inherited {
			ids[role.OID] = true
		}
	}
	result := make([]epco.ACLRoleIdentity, 0, len(ids))
	for oid := range ids {
		result = append(result, e.catalog.rolesByOID[oid].ACLRoleIdentity)
	}
	slices.SortFunc(result, func(a, b epco.ACLRoleIdentity) int { return strings.Compare(a.Name, b.Name) })
	return result
}
