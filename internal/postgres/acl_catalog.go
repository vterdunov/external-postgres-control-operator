package postgres

import (
	"context"
	"slices"
	"strings"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

type aclRole struct {
	epco.ACLRoleIdentity
	inherited, set, super bool
}
type aclObject struct {
	class, schema, name, owner string
	oid, schemaOID, ownerOID   uint32
}
type aclRow struct {
	grantee, grantor uint32
	privilege        string
	grantable        bool
}
type aclCatalog struct {
	database     aclObject
	roles        map[string]aclRole
	rolesByOID   map[uint32]aclRole
	schemas      map[string]aclObject
	schemasByOID map[uint32]aclObject
	relations    map[uint32]aclObject
	admin        aclRole
	version      int
}

func readACLCatalog(ctx context.Context, admin roleQuerier, target roleQuerier, name string, version int) (*aclCatalog, error) {
	c := &aclCatalog{roles: map[string]aclRole{}, rolesByOID: map[uint32]aclRole{}, schemas: map[string]aclObject{}, schemasByOID: map[uint32]aclObject{}, relations: map[uint32]aclObject{}, version: version}
	capability := roleCapabilityMember
	if version >= 160000 {
		capability = roleCapabilitySet
	}
	rows, err := admin.Query(ctx, `SELECT oid,rolname,rolsuper,pg_catalog.pg_has_role(current_user,oid,'USAGE'),pg_catalog.pg_has_role(current_user,oid,$1),rolname OPERATOR(pg_catalog.=) current_user FROM pg_catalog.pg_roles`, capability)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var role aclRole
		var current bool
		if err = rows.Scan(&role.OID, &role.Name, &role.super, &role.inherited, &role.set, &current); err != nil {
			rows.Close()
			return nil, err
		}
		c.roles[role.Name] = role
		c.rolesByOID[role.OID] = role
		if current {
			c.admin = role
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	c.database.class = aclClassDatabase
	c.database.name = name
	if err = admin.QueryRow(ctx, `SELECT oid,datdba,pg_catalog.pg_get_userbyid(datdba) FROM pg_catalog.pg_database WHERE datname OPERATOR(pg_catalog.=) $1`, name).Scan(&c.database.oid, &c.database.ownerOID, &c.database.owner); err != nil {
		return nil, err
	}
	if target == nil {
		return c, nil
	}
	rows, err = target.Query(ctx, `SELECT oid,nspname,nspowner,pg_catalog.pg_get_userbyid(nspowner) FROM pg_catalog.pg_namespace`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		o := aclObject{class: aclClassSchema}
		if err = rows.Scan(&o.oid, &o.name, &o.ownerOID, &o.owner); err != nil {
			rows.Close()
			return nil, err
		}
		o.schema = o.name
		o.schemaOID = o.oid
		c.schemas[o.name] = o
		c.schemasByOID[o.oid] = o
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows, err = target.Query(ctx, `SELECT c.oid,n.nspname,c.relname,c.relnamespace,c.relowner,pg_catalog.pg_get_userbyid(c.relowner),CASE WHEN c.relkind OPERATOR(pg_catalog.=) 'S' THEN 'sequence' ELSE 'table' END FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace WHERE c.relkind IN ('r','p','f','v','m','S')`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var o aclObject
		if err = rows.Scan(&o.oid, &o.schema, &o.name, &o.schemaOID, &o.ownerOID, &o.owner, &o.class); err != nil {
			rows.Close()
			return nil, err
		}
		c.relations[o.oid] = o
	}
	rows.Close()
	return c, rows.Err()
}

func (c *aclCatalog) resolveObject(b epco.ACLBinding, trusted bool) (aclObject, bool) {
	var object aclObject
	var exists bool
	switch b.Class {
	case aclClassDatabase:
		object = c.database
		exists = !trusted || b.ObjectOID == 0 || b.ObjectOID == object.oid
	case aclClassSchema:
		if trusted && b.ObjectOID != 0 {
			object, exists = c.schemasByOID[b.ObjectOID]
		} else {
			object, exists = c.schemas[b.Schema]
		}
	default:
		if b.Scope == aclScopeDefault {
			object = c.database
			object.class = b.Class
			exists = true
			if b.Schema != "" {
				if trusted && b.SchemaOID != 0 {
					object, exists = c.schemasByOID[b.SchemaOID]
				} else {
					object, exists = c.schemas[b.Schema]
				}
				object.class = b.Class
			}
		} else if trusted && b.ObjectOID != 0 {
			object, exists = c.relations[b.ObjectOID]
		} else {
			for _, candidate := range c.relations {
				if candidate.schema == b.Schema && candidate.name == b.Table && candidate.class == b.Class {
					object = candidate
					exists = true
					break
				}
			}
		}
	}
	return object, exists
}

func (c *aclCatalog) bind(b epco.ACLBinding, trusted bool) (epco.ACLBinding, bool, error) {
	// Child catalog OIDs and default keys are local to a database incarnation.
	// A routing change still follows logical rebinding, but same-epoch replacement
	// proves that the former database and all of its ACL responsibility are gone.
	if trusted && b.DatabaseOID != 0 && b.DatabaseOID != c.database.oid {
		return b, false, nil
	}
	object, exists := c.resolveObject(b, trusted)
	if !exists {
		if !trusted && b.ObjectOID != 0 {
			return b, false, &Failure{ACLBindingUnresolved, "A historical object cannot be rebound safely on the current target"}
		}
		return b, false, nil
	}
	if b.Scope != aclScopeDefault && object.class != b.Class {
		return b, false, &Failure{ACLBindingUnresolved, "The historical SQL object class changed"}
	}
	b.DatabaseOID = c.database.oid
	b.SchemaOID = object.schemaOID
	b.OwnerOID = object.ownerOID
	b.Owner = object.owner
	if b.Scope == aclScopeDefault {
		b.ObjectOID = 0
		if b.Schema != "" {
			b.Schema = object.schema
		}
	}
	if b.Class == aclClassSchema {
		b.Schema = object.name
		b.ObjectOID = object.oid
	}
	if b.Class == aclClassDatabase {
		b.ObjectOID = object.oid
	}
	if b.Scope != aclScopeDefault && (b.Class == aclClassTable || b.Class == aclClassSequence) {
		b.Table = object.name
		b.Schema = object.schema
		b.ObjectOID = object.oid
	}
	return c.resolveBindingRoles(b, trusted)
}

func (c *aclCatalog) resolveBindingRoles(b epco.ACLBinding, trusted bool) (epco.ACLBinding, bool, error) {
	resolve := func(name string, oid uint32) (aclRole, bool) {
		if trusted && oid != 0 {
			role, ok := c.rolesByOID[oid]
			return role, ok
		}
		role, ok := c.roles[name]
		return role, ok
	}
	if b.Grantee != aclPublic {
		role, ok := resolve(b.Grantee, b.GranteeOID)
		if !ok {
			if !trusted && b.GranteeOID != 0 {
				return b, false, &Failure{ACLBindingUnresolved, "A historical ACL grantee cannot be rebound on the current target"}
			}
			return b, false, nil
		}
		b.Grantee = role.Name
		b.GranteeOID = role.OID
	}
	if b.Scope == aclScopeDefault {
		role, ok := resolve(b.Creator, b.CreatorOID)
		if !ok {
			if !trusted && b.CreatorOID != 0 {
				return b, false, &Failure{ACLBindingUnresolved, "A historical default creator cannot be rebound on the current target"}
			}
			return b, false, nil
		}
		b.Creator = role.Name
		b.CreatorOID = role.OID
		b.Owner = role.Name
		b.OwnerOID = role.OID
	}
	if b.GrantorOID != 0 {
		if role, ok := resolve(b.Grantor, b.GrantorOID); ok {
			b.Grantor = role.Name
			b.GrantorOID = role.OID
		} else if !trusted {
			return b, false, &Failure{ACLBindingUnresolved, "A historical ACL grantor cannot be rebound on the current target"}
		}
	}
	if !trusted {
		b.GrantorCandidates = slices.Clone(b.GrantorCandidates)
		for i := range b.GrantorCandidates {
			if role, ok := c.roles[b.GrantorCandidates[i].Name]; ok {
				b.GrantorCandidates[i] = role.ACLRoleIdentity
			} else {
				b.GrantorCandidates[i].OID = 0
			}
		}
		b.Before = slices.Clone(b.Before)
		for i := range b.Before {
			if role, ok := c.roles[b.Before[i].Grantor.Name]; ok {
				b.Before[i].Grantor = role.ACLRoleIdentity
			} else {
				b.Before[i].Grantor.OID = 0
			}
		}
	}
	return b, true, nil
}

func readACLRows(ctx context.Context, q roleQuerier, b epco.ACLBinding) ([]aclRow, error) {
	var query string
	var args []any
	if b.Scope == aclScopeDefault {
		kind := defaultACLKind(b.Class)
		defaultKind := kind
		if kind == "S" {
			defaultKind = "s"
		}
		query = `SELECT a.grantee,a.grantor,a.privilege_type,a.is_grantable FROM pg_catalog.aclexplode(NULLIF(COALESCE((SELECT defaclacl FROM pg_catalog.pg_default_acl WHERE defaclrole OPERATOR(pg_catalog.=) $1 AND defaclnamespace OPERATOR(pg_catalog.=) $2 AND defaclobjtype OPERATOR(pg_catalog.=) $3::pg_catalog."char"),CASE WHEN $2::pg_catalog.oid OPERATOR(pg_catalog.=) 0 THEN pg_catalog.acldefault($4::pg_catalog."char",$1) ELSE '{}'::pg_catalog.aclitem[] END),'{}'::pg_catalog.aclitem[])) a`
		args = []any{b.CreatorOID, b.SchemaOID, kind, defaultKind}
	} else {
		switch b.Class {
		case aclClassDatabase:
			query = `SELECT a.grantee,a.grantor,a.privilege_type,a.is_grantable FROM pg_catalog.pg_database d CROSS JOIN LATERAL pg_catalog.aclexplode(NULLIF(COALESCE(d.datacl,pg_catalog.acldefault('d',d.datdba)),'{}'::pg_catalog.aclitem[])) a WHERE d.oid OPERATOR(pg_catalog.=) $1`
		case aclClassSchema:
			query = `SELECT a.grantee,a.grantor,a.privilege_type,a.is_grantable FROM pg_catalog.pg_namespace n CROSS JOIN LATERAL pg_catalog.aclexplode(NULLIF(COALESCE(n.nspacl,pg_catalog.acldefault('n',n.nspowner)),'{}'::pg_catalog.aclitem[])) a WHERE n.oid OPERATOR(pg_catalog.=) $1`
		default:
			query = `SELECT a.grantee,a.grantor,a.privilege_type,a.is_grantable FROM pg_catalog.pg_class c CROSS JOIN LATERAL pg_catalog.aclexplode(NULLIF(COALESCE(c.relacl,pg_catalog.acldefault(CASE WHEN c.relkind OPERATOR(pg_catalog.=) 'S' THEN 's'::pg_catalog."char" ELSE 'r'::pg_catalog."char" END,c.relowner)),'{}'::pg_catalog.aclitem[])) a WHERE c.oid OPERATOR(pg_catalog.=) $1`
		}
		args = []any{b.ObjectOID}
	}
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []aclRow
	for rows.Next() {
		var row aclRow
		if err = rows.Scan(&row.grantee, &row.grantor, &row.privilege, &row.grantable); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	slices.SortFunc(result, func(a, b aclRow) int {
		if a.grantor < b.grantor {
			return -1
		}
		if a.grantor > b.grantor {
			return 1
		}
		return strings.Compare(a.privilege, b.privilege)
	})
	return result, rows.Err()
}
func defaultACLKind(class string) string {
	switch class {
	case aclClassTable:
		return "r"
	case aclClassSequence:
		return "S"
	case aclClassFunction:
		return "f"
	case aclClassType:
		return "T"
	}
	return ""
}
func matchingACLRows(rows []aclRow, b epco.ACLBinding) []aclRow {
	return slices.DeleteFunc(slices.Clone(rows), func(row aclRow) bool { return row.grantee != b.GranteeOID || row.privilege != b.Privilege })
}
func aclRowPresent(rows []aclRow, b epco.ACLBinding) bool {
	return slices.ContainsFunc(rows, func(row aclRow) bool {
		return row.grantee == b.GranteeOID && row.grantor == b.GrantorOID && row.privilege == b.Privilege
	})
}
func (c *aclCatalog) before(rows []aclRow) []epco.ACLGrantIdentity {
	result := make([]epco.ACLGrantIdentity, 0, len(rows))
	for _, row := range rows {
		result = append(result, epco.ACLGrantIdentity{Grantor: c.rolesByOID[row.grantor].ACLRoleIdentity, Grantable: row.grantable})
	}
	return result
}
