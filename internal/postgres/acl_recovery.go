package postgres

import (
	"context"
	"errors"
	"reflect"
	"slices"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

func (e *aclEngine) preserveConnect(ctx context.Context, removing epco.ACLBinding) error {
	rows, err := readACLRows(ctx, e.queries(removing), removing)
	if err != nil {
		return databaseFailure(err)
	}
	// Only a genuinely necessary pin becomes operator-owned history.
	if e.connectWithout(rows, removing) {
		return nil
	}
	if removing.GranteeOID == e.catalog.admin.OID {
		return &Failure{InsufficientPrivileges, "Requested administrator CONNECT removal conflicts with required target access"}
	}
	pin := epco.ACLBinding{Declaration: "administrative/connect/" + e.catalog.admin.Name, Scope: aclClassDatabase, Class: aclClassDatabase, Privilege: aclConnect, Grantee: e.catalog.admin.Name, Administrative: true}
	pin, exists, err := e.catalog.bind(pin, false)
	if err != nil {
		return err
	}
	if !exists {
		return &Failure{DependenciesNotReady, "The administrator CONNECT binding could not be resolved"}
	}
	if err = e.reconcileBinding(ctx, pin, false); err != nil {
		return err
	}
	return e.verifyConnect(ctx)
}
func (e *aclEngine) connectWithout(rows []aclRow, removing epco.ACLBinding) bool {
	if e.catalog.admin.super {
		return true
	}
	for _, row := range rows {
		if row.privilege != aclConnect {
			continue
		}
		if row.grantee == removing.GranteeOID && row.grantor == removing.GrantorOID {
			continue
		}
		if row.grantee == 0 || row.grantee == e.catalog.admin.OID || e.catalog.rolesByOID[row.grantee].inherited {
			return true
		}
	}
	return false
}
func (e *aclEngine) verifyConnect(ctx context.Context) error {
	var effective, admission bool
	if err := e.session.aclQueries(e.session.connection).QueryRow(ctx, `SELECT has_database_privilege(current_user,oid,'CONNECT'),datallowconn FROM pg_catalog.pg_database WHERE datname=$1`, e.desired.Name).Scan(&effective, &admission); err != nil {
		return databaseFailure(err)
	}
	if !effective {
		return &Failure{InsufficientPrivileges, "The administrator lacks effective CONNECT on the target database"}
	}
	if !admission {
		return nil
	}
	_, disposal, err := e.session.targetConnection(ctx, e.desired.Name)
	if err != nil {
		return databaseFailure(err)
	}
	closeDatabaseConnection(disposal)
	return nil
}

func (e *aclEngine) cleanupHistory(ctx context.Context, declarations []aclDeclaration) error {
	history := slices.Clone(e.targets[e.index].Bindings)
	var first error
	for _, old := range history {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := e.cleanupBinding(ctx, old, declarations)
		if err != nil {
			if _, ok := errors.AsType[*aclCheckpointFailure](err); ok {
				return err
			}
			if first == nil {
				first = err
			}
			e.outcome(ctx, old, false, err.Error())
		}
	}
	return first
}
func (e *aclEngine) cleanupBinding(ctx context.Context, old epco.ACLBinding, declarations []aclDeclaration) error {
	if old.Class != aclClassDatabase && e.targetFailure != nil {
		return e.targetFailure
	}
	trusted := e.trusted && old.BindingRevision == e.desired.BindingRevision
	b, exists, err := e.catalog.bind(old, trusted)
	if err != nil {
		return err
	}
	if !exists {
		e.remove(old)
		return e.checkpoint(ctx)
	}
	b.BindingRevision = e.desired.BindingRevision
	if b.Schema != "" && IsProtectedSchema(b.Schema) {
		return &Failure{ProtectedSchema, "A historical ACL object moved into a protected schema"}
	}
	rows, err := readACLRows(ctx, e.queries(b), b)
	if err != nil {
		return databaseFailure(err)
	}
	b, err = e.resolveHistoricalOwner(old, b, trusted)
	if err != nil {
		potential := potentialOwnerBinding(old, b)
		if e.explicitAbsence(potential, declarations) {
			return nil
		}
		if e.blocked == nil {
			e.blocked = make(map[string]bool)
		}
		e.blocked[concreteACLKey(potential)] = true
		old.Pending = "owner-unresolved"
		e.put(old)
		return err
	}
	if b.Pending == aclPendingGrant && b.GrantorOID == 0 {
		matching := matchingACLRows(rows, b)
		matching = slices.DeleteFunc(matching, func(row aclRow) bool {
			return slices.ContainsFunc(b.Before, func(before epco.ACLGrantIdentity) bool { return before.Grantor.OID == row.grantor }) ||
				!slices.ContainsFunc(b.GrantorCandidates, func(candidate epco.ACLRoleIdentity) bool { return candidate.OID == row.grantor })
		})
		if len(matching) == 0 {
			if slices.ContainsFunc(declarations, func(d aclDeclaration) bool {
				return d.binding.Declaration == b.Declaration && !d.absent && bindingStillDesired(b, d.binding)
			}) {
				return nil
			}
			e.remove(old)
			return e.checkpoint(ctx)
		}
		if len(matching) > 1 {
			return &Failure{ACLBindingUnresolved, "A possibly committed grant has ambiguous grantor provenance"}
		}
		b.GrantorOID = matching[0].grantor
		b.Grantor = e.catalog.rolesByOID[b.GrantorOID].Name
	}
	e.remove(old)
	e.put(b)
	desired := slices.IndexFunc(declarations, func(d aclDeclaration) bool { return d.binding.Declaration == b.Declaration })
	keep := false
	if b.Administrative {
		keep = b.GranteeOID == e.catalog.admin.OID
	} else if desired >= 0 && !declarations[desired].absent {
		keep = bindingStillDesired(b, declarations[desired].binding)
	}
	if keep {
		if b.Pending != aclPendingGrant {
			b.Pending = ""
		}
		b.PreviousOwner = nil
		b.DesiredOwner = nil
		if reflect.DeepEqual(old, b) {
			return nil
		}
		e.remove(b)
		e.put(b)
		return e.checkpoint(ctx)
	}
	if !aclRowPresent(rows, b) {
		e.remove(b)
		return e.checkpoint(ctx)
	}
	return e.revoke(ctx, b)
}
func bindingStillDesired(b, desired epco.ACLBinding) bool {
	if b.Grantee != desired.Grantee || b.Creator != desired.Creator {
		return false
	}
	switch b.Scope {
	case aclClassTable:
		return b.Schema == desired.Schema && b.Table == desired.Table
	case aclScopeTables, aclScopeSequences, aclClassSchema, aclScopeDefault:
		return b.Schema == desired.Schema
	}
	return true
}
func (e *aclEngine) resolveHistoricalOwner(old, b epco.ACLBinding, trusted bool) (epco.ACLBinding, error) {
	var err error
	ownerEvidence := old
	if !trusted {
		if old.Owner != b.Owner && (old.Grantor == old.Owner || old.Grantee == old.Owner || old.GrantorOID == 0 && slices.ContainsFunc(old.GrantorCandidates, func(candidate epco.ACLRoleIdentity) bool { return candidate.Name == old.Owner })) {
			err = &Failure{ACLBindingUnresolved, "Owner rewrite provenance cannot be transferred across catalog epochs"}
		} else {
			ownerEvidence.OwnerOID = b.OwnerOID
		}
	}
	if err != nil {
		return b, err
	}
	return e.resolveOwnerRewrite(ownerEvidence, b)
}

func (e *aclEngine) resolveOwnerRewrite(old, b epco.ACLBinding) (epco.ACLBinding, error) {
	previous := old.OwnerOID
	if old.PreviousOwner != nil {
		previous = old.PreviousOwner.OID
	}
	if previous == 0 || previous == b.OwnerOID || b.Scope == aclScopeDefault {
		return b, nil
	}
	// pg_database_owner remains the real schema ACL owner even when its
	// implicit database-owner authority changes.
	if old.Owner == databaseOwnerRole && b.Owner == databaseOwnerRole {
		return b, nil
	}
	if old.GrantorOID == previous || old.GrantorOID == 0 && slices.ContainsFunc(old.GrantorCandidates, func(candidate epco.ACLRoleIdentity) bool { return candidate.OID == previous }) {
		if old.Pending != aclPendingOwner || old.PreviousOwner == nil || old.DesiredOwner == nil || old.DesiredOwner.OID != b.OwnerOID {
			return b, &Failure{ACLBindingUnresolved, "External owner transfer may have merged an independent grant; declare explicit absence to authorize removal"}
		}
		if slices.ContainsFunc(old.Before, func(row epco.ACLGrantIdentity) bool { return row.Grantor.OID == b.OwnerOID }) {
			return b, &Failure{ACLBindingUnresolved, "Owner transfer merged managed and independent ACL grantor rows"}
		}
		b.GrantorOID = b.OwnerOID
		b.Grantor = b.Owner
	}
	if old.GranteeOID == previous {
		if old.PreviousOwner == nil {
			return b, &Failure{ACLBindingUnresolved, "External owner transfer changed the managed grantee without a provenance checkpoint"}
		}
		if slices.ContainsFunc(old.Before, func(row epco.ACLGrantIdentity) bool {
			return row.Grantee != nil && row.Grantee.OID == b.OwnerOID && (row.Grantor.OID == b.GrantorOID || row.Grantor.OID == previous && b.GrantorOID == b.OwnerOID)
		}) {
			return b, &Failure{ACLBindingUnresolved, "Owner transfer merged managed and independent ACL grantee rows"}
		}
		b.GranteeOID = b.OwnerOID
		b.Grantee = b.Owner
	}
	// An owner transition may have been checkpointed but not issued. Current
	// catalog owner, rather than transition intent, decides the binding.
	b.Pending = ""
	b.PreviousOwner = nil
	b.DesiredOwner = nil
	return b, nil
}

func (s *databaseSession) restoreACLAccess(ctx context.Context, d ACLDesired, state *DatabaseState) ([]epco.ManagedACLTarget, error) {
	var err error
	if len(d.Spec.Schemas) > 0 || aclNeedsTarget(d.Spec, d.Targets, d.Target) {
		var access bool
		if err = s.aclQueries(s.connection).QueryRow(ctx, "SELECT pg_catalog.has_database_privilege(current_user,$1,'CONNECT')", d.Name).Scan(&access); err != nil {
			return d.Targets, databaseFailure(err)
		}
		if !access && state.AllowConnections {
			if slices.ContainsFunc(aclDeclarations(d.Spec), func(declaration aclDeclaration) bool {
				return declaration.absent && declaration.binding.Class == aclClassDatabase && declaration.binding.Privilege == aclConnect && declaration.binding.Grantee == state.AdminUser
			}) {
				return d.Targets, &Failure{InsufficientPrivileges, "Requested administrator CONNECT removal conflicts with required target access"}
			}
			accessEngine, dispose, openErr := s.newACLEngine(ctx, d, false)
			if openErr != nil {
				return d.Targets, openErr
			}
			pin := epco.ACLBinding{Declaration: "administrative/connect/" + accessEngine.catalog.admin.Name, Scope: aclClassDatabase, Class: aclClassDatabase, Privilege: aclConnect, Grantee: accessEngine.catalog.admin.Name, Administrative: true}
			pin, _, err = accessEngine.catalog.bind(pin, false)
			if err == nil {
				err = accessEngine.reconcileBinding(ctx, pin, false)
			}
			if err == nil {
				err = accessEngine.verifyConnect(ctx)
			}
			dispose()
			if err != nil {
				return accessEngine.targets, err
			}
			d.Targets = accessEngine.targets
		}
	}
	return d.Targets, nil
}

func (s *databaseSession) PrepareACL(ctx context.Context, d ACLDesired) (*ACLResult, error) {
	if d.Observe {
		return &ACLResult{Targets: d.Targets}, nil
	}
	state, err := s.Inspect(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	if !state.Exists {
		return &ACLResult{Targets: d.Targets}, nil
	}
	d.Targets, err = s.restoreACLAccess(ctx, d, state)
	if err != nil {
		return &ACLResult{Targets: d.Targets}, err
	}
	e, cleanup, err := s.newACLEngine(ctx, d, aclNeedsTarget(epco.DatabaseSpec{}, d.Targets, d.Target))
	if err != nil {
		return nil, err
	}
	defer cleanup()
	if err = e.resolvePriorOwnerTransitions(ctx); err != nil {
		return e.result(), err
	}
	history := slices.Clone(e.targets[e.index].Bindings)
	changed := false
	for _, old := range history {
		if old.Class != aclClassDatabase && e.targetFailure != nil {
			continue
		}
		b, exists, bindErr := e.catalog.bind(old, e.trusted && old.BindingRevision == e.desired.BindingRevision)
		if bindErr != nil {
			continue
		}
		if !exists {
			continue
		}
		b.BindingRevision = e.desired.BindingRevision
		newOwner := e.requestedOwner(b)
		if newOwner == "" {
			continue
		}
		role, ok := e.catalog.roles[newOwner]
		if !ok {
			return e.result(), &Failure{DependenciesNotReady, "A requested new ACL object owner does not exist"}
		}
		rows, readErr := readACLRows(ctx, e.queries(b), b)
		if readErr != nil {
			return e.result(), databaseFailure(readErr)
		}
		b.Pending = aclPendingOwner
		b.Generation = d.Generation
		b.PreviousOwner = &epco.ACLRoleIdentity{Name: b.Owner, OID: b.OwnerOID}
		b.DesiredOwner = new(role.ACLRoleIdentity)
		b.Before = e.ownerTransitionBefore(rows, b, role.OID)
		e.remove(old)
		e.put(b)
		changed = true
	}
	if changed {
		if err = e.checkpoint(ctx); err != nil {
			return e.result(), err
		}
	}
	return e.result(), nil
}

func (e *aclEngine) requestedOwner(b epco.ACLBinding) string {
	if b.Class == aclClassDatabase && string(e.desired.Spec.Owner) != e.catalog.database.owner {
		return string(e.desired.Spec.Owner)
	}
	if b.Class == aclClassSchema {
		for _, schema := range e.desired.Spec.Schemas {
			if string(schema.Name) == b.Schema && schema.Owner != "" && string(schema.Owner) != b.Owner && schema.Ensure != epco.EnsureAbsent {
				return string(schema.Owner)
			}
		}
	}
	return ""
}

// Preserve the original owner evidence until recovery has durably confirmed its
// current binding. The next transition must not replace an interrupted one.
func (e *aclEngine) resolvePriorOwnerTransitions(ctx context.Context) error {
	changed := false
	for _, old := range slices.Clone(e.targets[e.index].Bindings) {
		if old.Class != aclClassDatabase && e.targetFailure != nil {
			continue
		}
		trusted := e.trusted && old.BindingRevision == e.desired.BindingRevision
		b, exists, err := e.catalog.bind(old, trusted)
		if err != nil || !exists || e.requestedOwner(b) == "" {
			continue
		}
		b, err = e.resolveHistoricalOwner(old, b, trusted)
		if err != nil {
			return err
		}
		b.BindingRevision = e.desired.BindingRevision
		// The old command can also have been checkpointed without being issued.
		// The catalog still confirms the previous owner and its original binding.
		if b.Pending == aclPendingOwner {
			b.Pending = ""
			b.PreviousOwner = nil
			b.DesiredOwner = nil
		}
		if reflect.DeepEqual(old, b) {
			continue
		}
		e.remove(old)
		e.put(b)
		changed = true
	}
	if changed {
		return e.checkpoint(ctx)
	}
	return nil
}

func (e *aclEngine) ownerTransitionBefore(rows []aclRow, b epco.ACLBinding, desiredOwner uint32) []epco.ACLGrantIdentity {
	var before []epco.ACLGrantIdentity
	for _, row := range rows {
		if row.privilege != b.Privilege || (row.grantee != b.GranteeOID && (b.GranteeOID != b.OwnerOID || row.grantee != desiredOwner)) {
			continue
		}
		before = append(before, epco.ACLGrantIdentity{Grantor: e.catalog.rolesByOID[row.grantor].ACLRoleIdentity, Grantee: new(e.catalog.rolesByOID[row.grantee].ACLRoleIdentity), Grantable: row.grantable})
	}
	return before
}
