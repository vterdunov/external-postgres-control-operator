package postgres

import (
	"context"
	"slices"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

// Plan the concrete enumeration before mutations. This both prevents a broad
// SQL statement from capturing an unjournaled relation and makes size overflow
// a single API check rather than thousands of growing status writes.
func (e *aclEngine) prepareIntent(ctx context.Context, declarations []aclDeclaration) error {
	changed := false
	for _, d := range declarations {
		bindings, err := e.expand(ctx, d)
		if err != nil {
			continue
		}
		for _, b := range bindings {
			rows, err := readACLRows(ctx, e.queries(b), b)
			if err != nil {
				return databaseFailure(err)
			}
			matching := matchingACLRows(rows, b)
			if d.absent {
				for _, row := range matching {
					b.GrantorOID = row.grantor
					b.Grantor = e.catalog.rolesByOID[row.grantor].Name
					if err = e.prepareRoleCapability(ctx, b, false, rows); err != nil {
						return err
					}
					b.Pending = aclPendingRevoke
					b.Generation = e.desired.Generation
					b.Before = e.catalog.before(matching)
					if !e.hasBinding(b) {
						e.put(b)
						changed = true
					}
				}
				continue
			}
			if e.hasDeclarationBinding(b) {
				continue
			}
			if err = e.prepareRoleCapability(ctx, b, true, rows); err != nil {
				return err
			}
			b.Pending = aclPendingGrant
			b.Generation = e.desired.Generation
			b.Before = e.catalog.before(matching)
			b.GrantorCandidates = e.candidates(b, rows)
			if len(matching) > 0 {
				b.GrantorOID = matching[0].grantor
				b.Grantor = e.catalog.rolesByOID[b.GrantorOID].Name
			}
			e.put(b)
			changed = true
		}
	}
	if changed {
		return e.checkpoint(ctx)
	}
	return nil
}
func (e *aclEngine) hasBinding(b epco.ACLBinding) bool {
	return slices.ContainsFunc(e.targets[e.index].Bindings, func(old epco.ACLBinding) bool {
		return bindingKey(old) == bindingKey(b) && old.GranteeOID == b.GranteeOID
	})
}
func (e *aclEngine) hasDeclarationBinding(b epco.ACLBinding) bool {
	return slices.ContainsFunc(e.targets[e.index].Bindings, func(old epco.ACLBinding) bool {
		return old.Declaration == b.Declaration && old.ObjectOID == b.ObjectOID && old.SchemaOID == b.SchemaOID && old.CreatorOID == b.CreatorOID && old.GranteeOID == b.GranteeOID
	})
}
