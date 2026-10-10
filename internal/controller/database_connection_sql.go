package controller

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const databasePublicRole = "PUBLIC"

func (r *DatabaseReconciler) confirmConnectionState(ctx context.Context, database *epco.Database, cluster *epco.PostgresCluster, session postgres.DatabaseSession, state *postgres.DatabaseState, desired *postgres.ACLDesired, inputs []connectionSecretInput) (bool, *postgres.DatabaseState, error) {
	connected, coreErr := r.applyConnectionCore(ctx, database, session, state, desired)
	if connected == nil && coreErr != nil {
		return false, nil, coreErr
	}
	refreshErr := r.refreshConnectionSecrets(ctx, database, cluster, session, inputs, connected)
	if errors.Is(refreshErr, context.Canceled) || errors.Is(refreshErr, context.DeadlineExceeded) {
		return false, nil, refreshErr
	}
	current, err := session.Inspect(ctx, EffectiveDatabaseName(database))
	return refreshErr == nil, current, err
}

// Establish database properties and CONNECT before unrelated child prerequisites.
// The full journal remains intact and owner rewrites are checkpointed first.
func (r *DatabaseReconciler) applyConnectionCore(ctx context.Context, database *epco.Database, session postgres.DatabaseSession, state *postgres.DatabaseState, desired *postgres.ACLDesired) (*postgres.ACLResult, error) {
	if state == nil {
		return nil, &postgres.Failure{Reason: postgres.DependenciesNotReady, Message: "The current database catalog is not confirmed"}
	}
	core := *desired
	core.ConnectionOnly = true
	aclSession, capable := session.(postgres.DatabaseACLSession)
	if capable {
		prepared, err := aclSession.PrepareACL(ctx, core)
		if err != nil {
			return nil, err
		}
		if prepared == nil {
			return nil, &postgres.Failure{Reason: postgres.DependenciesNotReady, Message: "Database owner transition history was not confirmed"}
		}
		core.Targets = prepared.Targets
		desired.Targets = prepared.Targets
	}
	if err := r.databaseOwnerDependencies(ctx, database, state, nil, false); err != nil {
		return nil, err
	}
	base := database.Spec.DeepCopy()
	base.ConnectionSecrets, base.Schemas, base.DefaultPrivileges = nil, nil, nil
	base.Permissions = nil
	if err := r.ensureDatabaseCurrent(ctx, database); err != nil {
		return nil, err
	}
	result, err := session.Apply(ctx, postgres.DatabaseDesired{Name: EffectiveDatabaseName(database), Spec: *base})
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, &postgres.Failure{Reason: postgres.DependenciesNotReady, Message: "The current database properties were not confirmed"}
	}
	if !database.Status.HasManagedSQL {
		database.Status.HasManagedSQL = true
		if err = r.ensureDatabaseCurrent(ctx, database); err != nil {
			return nil, err
		}
		if err = r.Client.Status().Update(ctx, database); err != nil {
			return nil, err
		}
	}
	if !capable {
		return &postgres.ACLResult{}, nil
	}
	connected, err := aclSession.ReconcileACL(ctx, core)
	if connected != nil {
		desired.Targets = connected.Targets
		database.Status.ManagedACLTargets = connected.Targets
	}
	return connected, err
}

func connectionDeclarationConfirmed(database *epco.Database, role string, result *postgres.ACLResult) bool {
	if database.Spec.Permissions == nil {
		return true
	}
	for _, declaration := range database.Spec.Permissions.Connect {
		name := string(declaration.Name)
		if strings.EqualFold(name, "public") {
			name = databasePublicRole
		}
		if name != role && name != databasePublicRole {
			continue
		}
		if result == nil || !slices.ContainsFunc(result.Permissions, func(outcome epco.PermissionStatus) bool {
			return outcome.Scope == "database" && outcome.Grantee == name && outcome.Privilege == "CONNECT" && outcome.Applied
		}) {
			return false
		}
	}
	return true
}

func (r *DatabaseReconciler) refreshConnectionSecrets(ctx context.Context, database *epco.Database, cluster *epco.PostgresCluster, session postgres.DatabaseSession, inputs []connectionSecretInput, connected *postgres.ACLResult) error {
	checker, capable := session.(postgres.DatabaseConnectionSession)
	var first error
	for _, input := range inputs {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := r.refreshConnectionSecret(ctx, database, cluster, checker, capable, input, connected)
		if err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (r *DatabaseReconciler) refreshConnectionSecret(ctx context.Context, database *epco.Database, cluster *epco.PostgresCluster, checker postgres.DatabaseConnectionSession, capable bool, input connectionSecretInput, connected *postgres.ACLResult) error {
	receipt := passwordApplicationReceipt(input.role, cluster, input.source)
	if !equality.Semantic.DeepEqual(receipt, input.role.Status.PasswordApplied) || !connectionDeclarationConfirmed(database, EffectiveRoleName(input.role), connected) {
		return &postgres.Failure{Reason: postgres.DependenciesNotReady, Message: "The current application credential and desired CONNECT are not confirmed"}
	}
	if !capable {
		return &postgres.Failure{Reason: postgres.UnsupportedFeature, Message: "The database adapter cannot confirm application catalog eligibility"}
	}
	if err := checker.CheckConnection(ctx, EffectiveDatabaseName(database), EffectiveRoleName(input.role)); err != nil {
		return err
	}
	output := &corev1.Secret{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: database.Namespace, Name: input.entry.Name}, output)
	if err != nil && !apierrors.IsNotFound(err) {
		return secretPayloadWriteError(err)
	}
	if apierrors.IsNotFound(err) {
		output = &corev1.Secret{Namespace: database.Namespace, Name: input.entry.Name, Type: corev1.SecretTypeBasicAuth,
			Labels: map[string]string{ConnectionSecretLabel: connectionSecretLabelValue}, Annotations: connectionAnnotations(database, input), Data: input.data}
		if err = r.ensureDatabaseCurrent(ctx, database); err != nil {
			return err
		}
		if err = r.Client.Create(ctx, output); err != nil {
			return secretPayloadWriteError(err)
		}
	} else {
		if !connectionProvenance(database, output) || output.Annotations[ConnectionDatabaseUIDAnnotation] != string(database.UID) || len(output.OwnerReferences) != 0 {
			return connectionConflict()
		}
		proposed := output.DeepCopy()
		proposed.Type, proposed.Data = corev1.SecretTypeBasicAuth, input.data
		if proposed.Annotations == nil {
			proposed.Annotations = make(map[string]string)
		}
		maps.Copy(proposed.Annotations, connectionAnnotations(database, input))
		if !equality.Semantic.DeepEqual(output.Data, proposed.Data) || output.Type != proposed.Type || !equality.Semantic.DeepEqual(output.Annotations, proposed.Annotations) {
			if err = r.ensureDatabaseCurrent(ctx, database); err != nil {
				return err
			}
			if err = r.Client.Update(ctx, proposed); err != nil {
				return secretPayloadWriteError(err)
			}
			output = proposed
		}
	}
	index := slices.IndexFunc(database.Status.ConnectionSecretHistory, func(entry epco.ConnectionSecretState) bool { return entry.Name == input.entry.Name })
	if index < 0 {
		return &postgres.Failure{Reason: postgres.DependenciesNotReady, Message: "Application output publication intent is not recorded"}
	}
	if database.Status.ConnectionSecretHistory[index].UID != string(output.UID) {
		history := slices.Clone(database.Status.ConnectionSecretHistory)
		history[index].UID = string(output.UID)
		return r.checkpointConnectionHistory(ctx, database, history)
	}
	return nil
}
