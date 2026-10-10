package postgres

import (
	"context"
	"slices"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

func potentialOwnerBinding(old, b epco.ACLBinding) epco.ACLBinding {
	previous := old.Owner
	if old.PreviousOwner != nil {
		previous = old.PreviousOwner.Name
	}
	if b.Scope == aclScopeDefault || previous == b.Owner {
		return b
	}
	if old.Grantor == previous {
		b.Grantor = b.Owner
		b.GrantorOID = b.OwnerOID
	}
	if old.Grantee == previous {
		b.Grantee = b.Owner
		b.GranteeOID = b.OwnerOID
	}
	return b
}

// Explicit absence authorizes removal of the currently observed direct tuple,
// including an inseparable owner-merged row. Omission never supplies that consent.
func (e *aclEngine) explicitAbsence(b epco.ACLBinding, declarations []aclDeclaration) bool {
	for _, d := range declarations {
		if !d.absent {
			continue
		}
		candidate := d.binding
		if candidate.Scope == aclScopeTables || candidate.Scope == aclScopeSequences {
			candidate.Table = b.Table
		}
		resolved, exists, err := e.catalog.bind(candidate, false)
		if err == nil && exists && concreteACLKey(resolved) == concreteACLKey(b) {
			return true
		}
	}
	return false
}
func (e *aclEngine) confirmAbsence(ctx context.Context, b epco.ACLBinding) error {
	rows, err := readACLRows(ctx, e.queries(b), b)
	if err != nil {
		return databaseFailure(err)
	}
	if len(matchingACLRows(rows, b)) > 0 {
		return &Failure{InsufficientPrivileges, "The explicitly absent direct ACL tuple remains present"}
	}
	e.targets[e.index].Bindings = slices.DeleteFunc(e.targets[e.index].Bindings, func(old epco.ACLBinding) bool {
		if old.Privilege != b.Privilege || old.Class != b.Class {
			return false
		}
		bound, exists, bindErr := e.catalog.bind(old, e.trusted && old.BindingRevision == e.desired.BindingRevision)
		if bindErr != nil || !exists {
			return false
		}
		return concreteACLKey(potentialOwnerBinding(old, bound)) == concreteACLKey(b)
	})
	return e.checkpoint(ctx)
}
