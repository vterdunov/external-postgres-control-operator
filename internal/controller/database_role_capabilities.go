package controller

import (
	"context"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// databaseSessionOptions observes eligibility through the manager cache. SQL
// authority is checked independently by PostgreSQL before any service grant.
func (r *DatabaseReconciler) databaseSessionOptions(database *epco.Database, cluster *epco.ExternalPostgres) postgres.DatabaseSessionOptions {
	return postgres.DatabaseSessionOptions{ResolveRoleCapability: func(ctx context.Context, name string, purpose postgres.RoleCapabilityPurpose) (bool, error) {
		if database.Spec.ManagementPolicy == epco.ManagementObserve && database.DeletionTimestamp.IsZero() {
			return false, nil
		}
		if err := AuthorizeNamespace(ctx, r.Client, cluster, database.Namespace); err != nil {
			return false, err
		}
		roles := &epco.DatabaseRoleList{}
		var options []client.ListOption
		if r.ownerRoleIndexEnabled {
			options = append(options, client.MatchingFields{databaseRoleOwnerIndex: databaseClusterKey(database) + "/" + name})
		}
		if err := r.Client.List(ctx, roles, options...); err != nil {
			return false, err
		}
		var manager *epco.DatabaseRole
		pending := false
		for i := range roles.Items {
			role := &roles.Items[i]
			if roleClusterKey(role) != databaseClusterKey(database) || EffectiveRoleName(role) != name {
				continue
			}
			if hasManagedRole(role) {
				if manager != nil {
					return false, &postgres.Failure{Reason: postgres.OwnershipConflict, Message: "Multiple DatabaseRoles have recorded management of the required SQL role"}
				}
				manager = role
			} else if role.Spec.ManagementPolicy != epco.ManagementObserve && role.DeletionTimestamp.IsZero() {
				pending = true
			}
		}
		if manager == nil {
			if pending {
				return false, &postgres.Failure{Reason: postgres.DependenciesNotReady, Message: "The required SQL role has not recorded managed reconciliation"}
			}
			return false, nil
		}
		if manager.Spec.ManagementPolicy == epco.ManagementObserve {
			return false, nil
		}
		if !manager.DeletionTimestamp.IsZero() && purpose != postgres.RoleCapabilityCleanup {
			return false, nil
		}
		if err := AuthorizeNamespace(ctx, r.Client, cluster, manager.Namespace); err != nil {
			return false, err
		}
		return true, nil
	}}
}
