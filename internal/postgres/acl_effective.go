package postgres

import (
	"context"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

func (e *aclEngine) effective(ctx context.Context, b epco.ACLBinding) *bool {
	if b.DatabaseOID == 0 || e.connection(b) == nil {
		return nil
	}
	if b.Scope == aclScopeDefault {
		return e.effectiveDefault(ctx, b)
	}
	if b.Grantee == aclPublic {
		rows, err := readACLRows(ctx, e.queries(b), b)
		if err != nil {
			return nil
		}
		return new(len(matchingACLRows(rows, b)) > 0)
	}
	if b.GranteeOID == 0 {
		return nil
	}
	var function string
	switch b.Class {
	case aclClassDatabase:
		function = "has_database_privilege"
	case aclClassSchema:
		function = "has_schema_privilege"
	case aclClassTable:
		function = "has_table_privilege"
	case aclClassSequence:
		function = "has_sequence_privilege"
	default:
		return nil
	}
	var effective bool
	if err := e.queries(b).QueryRow(ctx, "SELECT pg_catalog."+function+"($1::oid,$2::oid,$3::text)", b.GranteeOID, b.ObjectOID, b.Privilege).Scan(&effective); err != nil {
		return nil
	}
	return new(effective)
}
func (e *aclEngine) effectiveDefault(ctx context.Context, b epco.ACLBinding) *bool {
	rows, err := readACLRows(ctx, e.queries(b), b)
	if err != nil {
		return nil
	}
	if b.SchemaOID != 0 {
		global := b
		global.Schema = ""
		global.SchemaOID = 0
		globalRows, readErr := readACLRows(ctx, e.queries(global), global)
		if readErr != nil {
			return nil
		}
		rows = append(rows, globalRows...)
	}
	for _, row := range rows {
		if row.privilege != b.Privilege {
			continue
		}
		if row.grantee == 0 || row.grantee == b.GranteeOID {
			return new(true)
		}
		if b.GranteeOID == 0 {
			continue
		}
		var inherited bool
		if err = e.queries(b).QueryRow(ctx, "SELECT pg_catalog.pg_has_role($1::oid,$2::oid,'USAGE')", b.GranteeOID, row.grantee).Scan(&inherited); err != nil {
			return nil
		}
		if inherited {
			return new(true)
		}
	}
	return new(false)
}
