package postgres

import (
	"context"
	"fmt"
	"slices"
	"strings"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

type aclDeclaration struct {
	binding epco.ACLBinding
	absent  bool
}
type privilegeList struct {
	privilege string
	entries   []epco.Permission
}

func tablePrivilegeLists(p *epco.TablePermissions) []privilegeList {
	if p == nil {
		return nil
	}
	return []privilegeList{{aclSelect, p.Select}, {"INSERT", p.Insert}, {aclUpdate, p.Update}, {"DELETE", p.Delete}, {"TRUNCATE", p.Truncate}, {"REFERENCES", p.References}, {"TRIGGER", p.Trigger}, {aclMaintain, p.Maintain}}
}
func sequencePrivilegeLists(p *epco.SequencePermissions) []privilegeList {
	if p == nil {
		return nil
	}
	return []privilegeList{{aclUsage, p.Usage}, {aclSelect, p.Select}, {aclUpdate, p.Update}}
}
func aclDeclarations(spec epco.DatabaseSpec) []aclDeclaration {
	var declarations []aclDeclaration
	add := func(scope, class, schema, table, creator string, lists []privilegeList) {
		for _, list := range lists {
			for _, entry := range list.entries {
				grantee := string(entry.Name)
				if strings.EqualFold(grantee, "public") {
					grantee = aclPublic
				}
				key := fmt.Sprintf("%q/%q/%q/%q/%q/%q/%q", scope, class, schema, table, creator, list.privilege, grantee)
				declarations = append(declarations, aclDeclaration{epco.ACLBinding{Declaration: key, Scope: scope, Class: class, Schema: schema, Table: table, Creator: creator, Privilege: list.privilege, Grantee: grantee}, entry.Ensure == epco.EnsureAbsent})
			}
		}
	}
	defaults := func(schema string, entries []epco.DefaultPrivileges) {
		for _, entry := range entries {
			creator := string(entry.ForRole)
			add(aclScopeDefault, aclClassTable, schema, "", creator, tablePrivilegeLists(entry.Tables))
			add(aclScopeDefault, aclClassSequence, schema, "", creator, sequencePrivilegeLists(entry.Sequences))
			if entry.Functions != nil {
				add(aclScopeDefault, aclClassFunction, schema, "", creator, []privilegeList{{"EXECUTE", entry.Functions.Execute}})
			}
			if entry.Types != nil {
				add(aclScopeDefault, aclClassType, schema, "", creator, []privilegeList{{aclUsage, entry.Types.Usage}})
			}
		}
	}
	if p := spec.Permissions; p != nil {
		add(aclClassDatabase, aclClassDatabase, "", "", "", []privilegeList{{aclConnect, p.Connect}, {aclCreate, p.Create}, {"TEMPORARY", p.Temporary}})
	}
	defaults("", spec.DefaultPrivileges)
	for _, schema := range spec.Schemas {
		if schema.Ensure == epco.EnsureAbsent {
			continue
		}
		name := string(schema.Name)
		if p := schema.Permissions; p != nil {
			add(aclClassSchema, aclClassSchema, name, "", "", []privilegeList{{aclUsage, p.Usage}, {aclCreate, p.Create}})
			add(aclScopeTables, aclClassTable, name, "", "", tablePrivilegeLists(p.Tables))
			add(aclScopeSequences, aclClassSequence, name, "", "", sequencePrivilegeLists(p.Sequences))
		}
		for _, table := range schema.TablePermissions {
			add(aclClassTable, aclClassTable, name, string(table.Name), "", tablePrivilegeLists(&table.Permissions))
		}
		defaults(name, schema.DefaultPrivileges)
	}
	slices.SortFunc(declarations, func(a, b aclDeclaration) int { return strings.Compare(a.binding.Declaration, b.binding.Declaration) })
	return declarations
}

// ACLRoleDependencies returns current SQL names whose managed readiness matters.
// Cleanup does not wait for obsolete role declarations.
func ACLRoleDependencies(spec epco.DatabaseSpec) []string {
	names := map[string]bool{}
	for _, d := range aclDeclarations(spec) {
		if d.absent {
			continue
		}
		if d.binding.Grantee != aclPublic {
			names[d.binding.Grantee] = true
		}
		if d.binding.Creator != "" {
			names[d.binding.Creator] = true
		}
	}
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	slices.Sort(result)
	return result
}

func (s *databaseSession) PreflightACL(ctx context.Context, desired DatabaseDesired) error {
	state, err := s.Inspect(ctx, desired.Name)
	if err != nil {
		return err
	}
	return preflightACL(state, desired.Spec)
}
func preflightACL(state *DatabaseState, spec epco.DatabaseSpec) error {
	declarations := aclDeclarations(spec)
	seen := map[string]bool{}
	for _, d := range declarations {
		b := d.binding
		if seen[b.Declaration] {
			return &Failure{InvalidConfiguration, "Duplicate permission declarations own the same ACL tuple"}
		}
		seen[b.Declaration] = true
		if b.Privilege == aclMaintain && state.ServerVersion < 170000 {
			return &Failure{UnsupportedFeature, "MAINTAIN requires PostgreSQL 17 or later"}
		}
		for _, name := range []string{b.Schema, b.Table, b.Creator, b.Grantee} {
			if name != "" && name != aclPublic {
				if err := validateDatabaseIdentifier(name, state.MaxIdentifierLength); err != nil {
					return err
				}
			}
		}
		if b.Scope != aclClassTable {
			continue
		}
		broad := b
		broad.Scope = aclScopeTables
		broad.Table = ""
		broad.Declaration = fmt.Sprintf("%q/%q/%q/%q/%q/%q/%q", broad.Scope, broad.Class, broad.Schema, broad.Table, broad.Creator, broad.Privilege, broad.Grantee)
		if slices.ContainsFunc(declarations, func(other aclDeclaration) bool { return other.binding.Declaration == broad.Declaration }) {
			return &Failure{InvalidConfiguration, "Broad and named declarations own the same table privilege and grantee"}
		}
	}
	return nil
}
