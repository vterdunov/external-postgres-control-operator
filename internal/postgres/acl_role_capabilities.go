package postgres

import (
	"context"
	"errors"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

func aclServiceRoleDemand(d ACLDesired, state *DatabaseState) bool {
	if !databaseMatches(state, d.Spec) || len(d.Spec.Schemas) > 0 || len(aclDeclarations(d.Spec)) > 0 {
		return true
	}
	for _, target := range d.Targets {
		if aclTargetKey(target) != aclTargetKey(d.Target) {
			continue
		}
		for _, binding := range target.Bindings {
			if !binding.Administrative {
				return true
			}
		}
	}
	return false
}

func (e *aclEngine) refreshRoleCapabilities(ctx context.Context) error {
	var target roleQuerier
	if e.target != nil {
		target = e.session.aclQueries(e.target)
	}
	catalog, err := readACLCatalog(ctx, e.session.aclQueries(e.session.connection), target, e.desired.Name, e.catalog.version)
	if err != nil {
		return databaseFailure(err)
	}
	e.catalog = catalog
	return nil
}

// Resolve authority before calculating grantor candidates or persisting ACL
// intent. Ordinary grantees never become service-membership demand.
func (e *aclEngine) prepareRoleCapability(ctx context.Context, b epco.ACLBinding, grant bool, rows []aclRow) error {
	if e.desired.Observe || e.catalog.admin.super {
		return nil
	}
	if b.Administrative && !e.serviceRoleDemand {
		return nil
	}
	purpose := RoleCapabilityProvision
	role := ""
	if !grant {
		purpose = RoleCapabilityCleanup
		if b.Scope != aclScopeDefault {
			if err := e.authorizeExactRevoke(ctx, b); err == nil {
				return nil
			} else if failure, ok := errors.AsType[*Failure](err); !ok || failure.Reason != InsufficientPrivileges {
				return err
			}
			role = b.Grantor
		} else {
			role = b.Creator
		}
	} else if b.Scope == aclScopeDefault {
		if len(matchingACLRows(rows, b)) > 0 {
			return nil
		}
		role = b.Creator
	} else {
		if len(matchingACLRows(rows, b)) > 0 || len(e.candidates(b, rows)) > 0 {
			return nil
		}
		role = b.Owner
	}
	if role == databaseOwnerRole {
		role = e.catalog.database.owner
	}
	authority, ok := e.catalog.roles[role]
	if !ok {
		return &Failure{DependenciesNotReady, "An ACL owner, grantor or creator role no longer exists"}
	}
	if b.Scope == aclScopeDefault && (authority.inherited || authority.set) {
		return nil
	}
	if err := e.session.ensureServiceRole(ctx, role, purpose); err != nil {
		return databaseFailure(err)
	}
	return e.refreshRoleCapabilities(ctx)
}
