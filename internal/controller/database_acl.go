package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"time"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DatabaseACLStatusByteLimit reserves substantial API-server request headroom.
// It includes the entire resource, not just the journal or its latest batch.
const DatabaseACLStatusByteLimit = 768 * 1024

// Keep room for readiness, generation and timestamp changes after a SQL batch.
const databaseACLStatusHeadroom = 4 * 1024

func preflightDatabaseACL(ctx context.Context, database *epco.Database, session postgres.DatabaseSession) error {
	aclSession, capable := session.(postgres.DatabaseACLSession)
	if !capable && (databaseACLRequested(database.Spec) || len(database.Status.ManagedACLTargets) > 0) {
		return &postgres.Failure{Reason: postgres.UnsupportedFeature, Message: "The database adapter does not support ACL reconciliation"}
	}
	if capable {
		return aclSession.PreflightACL(ctx, postgres.DatabaseDesired{Name: EffectiveDatabaseName(database), Spec: database.Spec})
	}
	return nil
}

func databaseACLRequested(spec epco.DatabaseSpec) bool {
	if spec.Permissions != nil || len(spec.DefaultPrivileges) > 0 {
		return true
	}
	for _, schema := range spec.Schemas {
		if schema.Permissions != nil || len(schema.DefaultPrivileges) > 0 || len(schema.TablePermissions) > 0 {
			return true
		}
	}
	return false
}
func (r *DatabaseReconciler) databaseACLDesired(database *epco.Database, cluster *epco.PostgresCluster, config *postgres.Config) postgres.ACLDesired {
	target := epco.ManagedACLTarget{Host: strings.TrimSuffix(strings.ToLower(strings.TrimSpace(config.Connection.Host)), "."), Port: int32(config.Connection.Port), Database: EffectiveDatabaseName(database)}
	key := fmt.Sprintf("%q/%d/%q", target.Host, target.Port, target.Database)
	prefix := fmt.Sprintf("%x/", sha256.Sum256(fmt.Appendf(nil, "%s/%s", cluster.UID, key)))
	revision := prefix + database.ResourceVersion
	targets := database.Status.DeepCopy().ManagedACLTargets
	if database.Status.ActiveACLTarget == key {
		for _, previous := range targets {
			if previous.Host == target.Host && previous.Port == target.Port && previous.Database == target.Database && strings.HasPrefix(previous.BindingRevision, prefix) {
				revision = previous.BindingRevision
			}
		}
	}
	return postgres.ACLDesired{Name: target.Database, Spec: database.Spec, Targets: targets, Target: target, BindingRevision: revision, Generation: database.Generation, Observe: database.Spec.ManagementPolicy == epco.ManagementObserve,
		Checkpoint: func(ctx context.Context, targets []epco.ManagedACLTarget) error {
			proposed := database.DeepCopy()
			proposed.Status.ManagedACLTargets = targets
			proposed.Status.ActiveACLTarget = key
			if err := boundDatabasePermissionReports(proposed); err != nil {
				return err
			}
			// A failed checkpoint must leave the last persisted responsibility intact.
			serialized, err := json.Marshal(proposed)
			if err != nil {
				return err
			}
			if len(serialized) > DatabaseACLStatusByteLimit-databaseACLStatusHeadroom {
				return &postgres.Failure{Reason: postgres.StatusSizeExceeded, Message: "Concrete ACL history exceeds the status size budget; reduce managed scope or resolve pending cleanup"}
			}
			if err = r.ensureDatabaseCurrent(ctx, database); err != nil {
				return err
			}
			if err = r.Client.Status().Update(ctx, proposed); err != nil {
				return err
			}
			database.Status = proposed.Status
			database.ResourceVersion = proposed.ResourceVersion
			return r.ensureDatabaseCurrent(ctx, database)
		}}
}

func (r *DatabaseReconciler) applyDatabaseACL(ctx context.Context, database *epco.Database, session postgres.DatabaseSession, state *postgres.DatabaseState, interval time.Duration, desired postgres.ACLDesired, connectionsReady bool) (ctrl.Result, error) {
	aclSession, capable := session.(postgres.DatabaseACLSession)
	if !capable {
		return r.applyDatabaseSession(ctx, database, session, state, interval, connectionsReady)
	}
	// An explicit admission repair uses the existing owner only, before target
	// inspection. It cannot smuggle an uncheckpointed owner transition.
	if state.Exists && !state.AllowConnections && database.Spec.AllowConnections != nil && *database.Spec.AllowConnections {
		interim := epco.DatabaseSpec{Owner: epco.SQLName(state.Owner), AllowConnections: new(true)}
		if err := r.ensureDatabaseCurrent(ctx, database); err != nil {
			return r.databaseError(ctx, database, err, interval)
		}
		result, err := session.Apply(ctx, postgres.DatabaseDesired{Name: desired.Name, Spec: interim})
		if err != nil {
			return r.databaseError(ctx, database, err, interval)
		}
		if result == nil {
			return r.databaseStatus(ctx, database, false, postgres.DependenciesNotReady, "Connection admission repair was not confirmed", interval)
		}
		state, err = session.Inspect(ctx, desired.Name)
		if err != nil {
			return r.databaseError(ctx, database, err, interval)
		}
	}
	if err := r.databaseACLDependencies(ctx, database); err != nil {
		return r.databaseError(ctx, database, err, interval)
	}
	prepared, err := aclSession.PrepareACL(ctx, desired)
	if err != nil {
		return r.databaseError(ctx, database, err, interval)
	}
	if prepared == nil {
		return r.databaseStatus(ctx, database, false, postgres.UnsupportedFeature, "ACL preparation did not confirm journal state", interval)
	}
	desired.Targets = prepared.Targets
	base := database.Spec.DeepCopy()
	base.ConnectionSecrets = nil
	base.Permissions = nil
	base.DefaultPrivileges = nil
	for i := range base.Schemas {
		base.Schemas[i].Permissions = nil
		base.Schemas[i].DefaultPrivileges = nil
		base.Schemas[i].TablePermissions = nil
	}
	var schemas []postgres.SchemaState
	if state.Exists && len(base.Schemas) > 0 {
		schemas, err = session.InspectSchemas(ctx, desired.Name, base.Schemas)
		if err != nil {
			return r.databaseError(ctx, database, err, interval)
		}
	}
	if err = r.databaseOwnerDependencies(ctx, database, state, schemas, true); err != nil {
		return r.databaseError(ctx, database, err, interval)
	}
	if err = r.ensureDatabaseCurrent(ctx, database); err != nil {
		return r.databaseError(ctx, database, err, interval)
	}
	result, err := session.Apply(ctx, postgres.DatabaseDesired{Name: desired.Name, Spec: *base})
	if receiptErr := r.recordDatabaseBase(ctx, database, result); receiptErr != nil {
		return r.databaseError(ctx, database, receiptErr, interval)
	}
	if err != nil {
		return r.databaseError(ctx, database, err, interval)
	}
	if result == nil {
		return r.databaseStatus(ctx, database, false, postgres.DependenciesNotReady, "Database lifecycle was not confirmed", interval)
	}
	// The first recorded lifecycle manager owns its name even while migrations
	// have not created a named table. Full ACL readiness is a later checkpoint.
	if !database.Status.HasManagedSQL {
		database.Status.HasManagedSQL = true
		if err = r.ensureDatabaseCurrent(ctx, database); err != nil {
			return r.databaseError(ctx, database, err, interval)
		}
		if err = r.Client.Status().Update(ctx, database); err != nil {
			return roleWriteResult(err)
		}
	}
	aclResult, err := aclSession.ReconcileACL(ctx, desired)
	if aclResult != nil {
		database.Status.Permissions = aclResult.Permissions
		database.Status.PermissionsOmitted = 0
	}
	if err != nil {
		return r.databaseError(ctx, database, err, interval)
	}
	if aclResult == nil {
		return r.databaseStatus(ctx, database, false, postgres.DependenciesNotReady, "ACL reconciliation did not confirm all scopes", interval)
	}
	database.Status.ManagedACLTargets = aclResult.Targets
	return r.databaseSuccess(ctx, database, interval, connectionsReady)
}
func (r *DatabaseReconciler) observeDatabaseACL(ctx context.Context, database *epco.Database, session postgres.DatabaseSession, state *postgres.DatabaseState, interval time.Duration, desired postgres.ACLDesired) (ctrl.Result, error) {
	aclSession, capable := session.(postgres.DatabaseACLSession)
	if capable && state.Exists && databaseACLRequested(database.Spec) {
		desired.Observe = true
		result, err := aclSession.ReconcileACL(ctx, desired)
		if result != nil {
			database.Status.Permissions = result.Permissions
			database.Status.PermissionsOmitted = 0
		}
		if err != nil {
			return r.databaseError(ctx, database, err, interval)
		}
		if result == nil {
			return r.databaseStatus(ctx, database, false, postgres.DependenciesNotReady, "ACL observation did not confirm declared scopes", interval)
		}
	}
	return r.observeDatabase(ctx, database, session, state, interval)
}
func (r *DatabaseReconciler) databaseACLDependencies(ctx context.Context, database *epco.Database) error {
	needed := postgres.ACLRoleDependencies(database.Spec)
	if len(needed) == 0 {
		return nil
	}
	roles := &epco.DatabaseRoleList{}
	if r.ownerRoleIndexEnabled {
		for _, name := range needed {
			candidates := &epco.DatabaseRoleList{}
			if err := r.Client.List(ctx, candidates, client.MatchingFields{databaseRoleOwnerIndex: databaseClusterKey(database) + "/" + name}); err != nil {
				return err
			}
			roles.Items = append(roles.Items, candidates.Items...)
		}
	} else if err := r.Client.List(ctx, roles); err != nil {
		return err
	}
	recorded := map[string]bool{}
	for _, role := range roles.Items {
		if roleClusterKey(&role) == databaseClusterKey(database) && hasManagedRole(&role) {
			recorded[EffectiveRoleName(&role)] = true
		}
	}
	for _, role := range roles.Items {
		name := EffectiveRoleName(&role)
		if roleClusterKey(&role) != databaseClusterKey(database) || !slices.Contains(needed, name) {
			continue
		}
		if !hasManagedRole(&role) && (recorded[name] || role.Spec.ManagementPolicy == epco.ManagementObserve) {
			continue
		}
		condition := meta.FindStatusCondition(role.Status.Conditions, readyCondition)
		if !role.DeletionTimestamp.IsZero() || condition == nil || condition.Status != metav1.ConditionTrue || condition.ObservedGeneration != role.Generation {
			return &postgres.Failure{Reason: postgres.DependenciesNotReady, Message: "A managed SQL grantee or default creator role is not ready"}
		}
	}
	return nil
}
