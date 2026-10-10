package controller

import (
	"context"
	"encoding/json/v2"
	"errors"
	"slices"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	connectionSecretLabelValue            = "true"
	ConnectionSecretLabel                 = "postgresql.epco.io/connection-secret"
	ConnectionDatabaseUIDAnnotation       = "postgresql.epco.io/database-uid"
	ConnectionDatabaseNameAnnotation      = "postgresql.epco.io/database-name"
	ConnectionDatabaseResourceAnnotation  = "postgresql.epco.io/database-resource-name"
	ConnectionDatabaseNamespaceAnnotation = "postgresql.epco.io/database-resource-namespace"
	ConnectionRoleResourceAnnotation      = "postgresql.epco.io/connection-role-resource"
	ConnectionSourceAnnotation            = "postgresql.epco.io/connection-source"
)

type connectionSecretInput struct {
	entry  epco.ConnectionSecret
	role   *epco.DatabaseRole
	source *corev1.Secret
	data   map[string][]byte
}

func roleSourceName(role *epco.DatabaseRole) string {
	if role.Spec.PasswordGeneration != nil {
		return role.Spec.PasswordGeneration.SecretName
	}
	if role.Spec.PasswordSecret != nil {
		return role.Spec.PasswordSecret.Name
	}
	return ""
}

func databaseOutputNames(database *epco.Database) []string {
	names := make([]string, 0, len(database.Spec.ConnectionSecrets)+len(database.Status.ConnectionSecretHistory))
	for _, entry := range database.Spec.ConnectionSecrets {
		names = append(names, entry.Name)
	}
	for _, entry := range database.Status.ConnectionSecretHistory {
		if !slices.Contains(names, entry.Name) {
			names = append(names, entry.Name)
		}
	}
	return names
}

func connectionConflict() error {
	return &postgres.Failure{Reason: postgres.ConnectionSecretConflict, Message: "Application output and credential source ownership conflict"}
}

// Source and output reservations cover declarations before either Secret exists.
func checkRoleSourceOutputReservation(ctx context.Context, reader client.Reader, role *epco.DatabaseRole) error {
	name := roleSourceName(role)
	if name == "" {
		return nil
	}
	databases := &epco.DatabaseList{}
	if err := reader.List(ctx, databases, client.InNamespace(role.Namespace)); err != nil {
		return err
	}
	for _, database := range databases.Items {
		if slices.Contains(databaseOutputNames(&database), name) {
			return connectionConflict()
		}
	}
	return nil
}

func connectionProvenance(database *epco.Database, output *corev1.Secret) bool {
	return output.Namespace == database.Namespace && output.Labels[ConnectionSecretLabel] == connectionSecretLabelValue &&
		output.Annotations[ConnectionDatabaseUIDAnnotation] != "" &&
		output.Annotations[ConnectionDatabaseNameAnnotation] == EffectiveDatabaseName(database) &&
		output.Annotations[ConnectionDatabaseResourceAnnotation] == database.Name &&
		output.Annotations[ConnectionDatabaseNamespaceAnnotation] == database.Namespace
}

func connectionAnnotations(database *epco.Database, input connectionSecretInput) map[string]string {
	return map[string]string{ConnectionDatabaseUIDAnnotation: string(database.UID), ConnectionDatabaseNameAnnotation: EffectiveDatabaseName(database),
		ConnectionDatabaseResourceAnnotation: database.Name, ConnectionDatabaseNamespaceAnnotation: database.Namespace,
		ConnectionRoleResourceAnnotation: input.role.Name, ConnectionSourceAnnotation: input.source.Name}
}

func (r *DatabaseReconciler) checkpointConnectionHistory(ctx context.Context, database *epco.Database, history []epco.ConnectionSecretState) error {
	proposed := database.DeepCopy()
	proposed.Status.ConnectionSecretHistory = history
	if err := boundDatabasePermissionReports(proposed); err != nil {
		return err
	}
	serialized, err := json.Marshal(proposed)
	if err != nil {
		return err
	}
	if len(serialized) > DatabaseACLStatusByteLimit-databaseACLStatusHeadroom {
		return &postgres.Failure{Reason: postgres.StatusSizeExceeded, Message: "Output history exceeds the resource status size budget"}
	}
	if err = r.ensureDatabaseCurrent(ctx, database); err != nil {
		return err
	}
	if err = r.Client.Status().Update(ctx, proposed); err != nil {
		return err
	}
	database.Status, database.ResourceVersion = proposed.Status, proposed.ResourceVersion
	return nil
}

// Initial publication is permitted only before a durable CREATE intent exists.
func (r *DatabaseReconciler) publishInitialConnectionSecrets(ctx context.Context, database *epco.Database, inputs []connectionSecretInput) error {
	var first error
	for _, input := range inputs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.publishInitialConnectionSecret(ctx, database, input); err != nil {
			failure, _ := errors.AsType[*postgres.Failure](err)
			if connectionCleanupInterrupted(err) || failure != nil && failure.Reason == postgres.StatusSizeExceeded {
				return err
			}
			if first == nil {
				first = err
			}
		}
	}
	return first
}

func (r *DatabaseReconciler) publishInitialConnectionSecret(ctx context.Context, database *epco.Database, input connectionSecretInput) error {
	index := slices.IndexFunc(database.Status.ConnectionSecretHistory, func(entry epco.ConnectionSecretState) bool { return entry.Name == input.entry.Name })
	output, err := r.initialConnectionOutput(ctx, database, input.entry.Name, index)
	if err != nil {
		return err
	}
	if output != nil {
		return r.adoptConnectionSecret(ctx, database, output, index)
	}
	if index >= 0 {
		return nil
	}
	state := epco.ConnectionSecretState{Name: input.entry.Name}
	if err = r.checkpointConnectionState(ctx, database, state); err != nil {
		return err
	}
	output = &corev1.Secret{Namespace: database.Namespace, Name: input.entry.Name, Type: corev1.SecretTypeBasicAuth,
		Labels: map[string]string{ConnectionSecretLabel: connectionSecretLabelValue}, Annotations: connectionAnnotations(database, input), Data: input.data}
	if err = r.ensureDatabaseCurrent(ctx, database); err != nil {
		return err
	}
	if err = r.Client.Create(ctx, output); err != nil {
		return secretPayloadWriteError(err)
	}
	state.UID = string(output.UID)
	return r.checkpointConnectionState(ctx, database, state)
}

func (r *DatabaseReconciler) initialConnectionOutput(ctx context.Context, database *epco.Database, name string, index int) (*corev1.Secret, error) {
	key := client.ObjectKey{Namespace: database.Namespace, Name: name}
	output := &corev1.Secret{}
	err := r.Client.Get(ctx, key, output)
	if err == nil && index >= 0 {
		recorded := database.Status.ConnectionSecretHistory[index].UID
		if recorded != "" && recorded != string(output.UID) {
			// Confirm an identity transition before replacing durable history;
			// an older informer incarnation must not roll responsibility back.
			reader := r.APIReader
			if reader == nil {
				reader = r.Client
			}
			output = &corev1.Secret{}
			err = reader.Get(ctx, key, output)
		}
	}
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return output, err
}

func (r *DatabaseReconciler) adoptConnectionSecret(ctx context.Context, database *epco.Database, output *corev1.Secret, index int) error {
	if !connectionProvenance(database, output) || len(output.OwnerReferences) != 0 {
		return connectionConflict()
	}
	owner := output.Annotations[ConnectionDatabaseUIDAnnotation]
	state := epco.ConnectionSecretState{Name: output.Name, UID: string(output.UID)}
	if owner != string(database.UID) {
		if index >= 0 {
			previous := database.Status.ConnectionSecretHistory[index]
			if previous.UID != "" && previous.UID == state.UID && previous.AdoptionFromDatabaseUID != owner {
				return connectionConflict()
			}
		}
		state.AdoptionFromDatabaseUID = owner
	}
	// A successful checkpoint claims this validated exact incarnation before
	// ownership mutation. An uncertain UPDATE leaves that claim intact.
	if err := r.checkpointConnectionState(ctx, database, state); err != nil {
		return err
	}
	if state.AdoptionFromDatabaseUID == "" {
		return nil
	}
	if err := r.ensureDatabaseCurrent(ctx, database); err != nil {
		return err
	}
	output.Annotations[ConnectionDatabaseUIDAnnotation] = string(database.UID)
	if err := r.Client.Update(ctx, output); err != nil {
		return secretPayloadWriteError(err)
	}
	state.AdoptionFromDatabaseUID = ""
	return r.checkpointConnectionState(ctx, database, state)
}

func (r *DatabaseReconciler) checkpointConnectionState(ctx context.Context, database *epco.Database, state epco.ConnectionSecretState) error {
	history := slices.Clone(database.Status.ConnectionSecretHistory)
	index := slices.IndexFunc(history, func(entry epco.ConnectionSecretState) bool { return entry.Name == state.Name })
	if index < 0 {
		history = append(history, state)
	} else {
		if history[index] == state {
			return nil
		}
		history[index] = state
	}
	return r.checkpointConnectionHistory(ctx, database, history)
}

func (r *DatabaseReconciler) cleanupConnectionSecrets(ctx context.Context, database *epco.Database, all bool) error {
	var first error
	for _, entry := range slices.Clone(database.Status.ConnectionSecretHistory) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !all && slices.ContainsFunc(database.Spec.ConnectionSecrets, func(desired epco.ConnectionSecret) bool { return desired.Name == entry.Name }) {
			continue
		}
		err := r.cleanupConnectionSecret(ctx, database, entry)
		if err != nil {
			if connectionCleanupInterrupted(err) {
				return err
			}
			if first == nil {
				first = err
			}
			continue
		}
		history := slices.DeleteFunc(slices.Clone(database.Status.ConnectionSecretHistory), func(old epco.ConnectionSecretState) bool { return old.Name == entry.Name })
		if err = r.checkpointConnectionHistory(ctx, database, history); err != nil {
			return err
		}
	}
	return first
}

func (r *DatabaseReconciler) cleanupConnectionSecret(ctx context.Context, database *epco.Database, entry epco.ConnectionSecretState) error {
	key := client.ObjectKey{Namespace: database.Namespace, Name: entry.Name}
	output := &corev1.Secret{}
	err := r.Client.Get(ctx, key, output)
	if apierrors.IsNotFound(err) || err == nil && entry.UID != "" && entry.UID != string(output.UID) {
		// Cache absence or an older incarnation cannot retire durable cleanup
		// responsibility. Confirm only this retirement decision directly.
		reader := r.APIReader
		if reader == nil {
			reader = r.Client
		}
		output = &corev1.Secret{}
		err = reader.Get(ctx, key, output)
	}
	if err != nil {
		return client.IgnoreNotFound(err)
	}
	if entry.UID != "" && entry.UID != string(output.UID) {
		// A distinct incarnation confirms the recorded one is gone. Do not
		// mutate the replacement, irrespective of its claimed provenance.
		return nil
	}
	owner := output.Annotations[ConnectionDatabaseUIDAnnotation]
	claimed := owner == string(database.UID) || entry.UID != "" && entry.AdoptionFromDatabaseUID != "" && owner == entry.AdoptionFromDatabaseUID
	if !connectionProvenance(database, output) || !claimed || len(output.OwnerReferences) != 0 {
		// Same or unknown incarnation still carries cleanup responsibility.
		return connectionConflict()
	}
	if err := r.ensureDatabaseCurrent(ctx, database); err != nil {
		return err
	}
	return client.IgnoreNotFound(secretPayloadWriteError(r.Client.Delete(ctx, output,
		client.Preconditions{UID: new(output.UID), ResourceVersion: new(output.ResourceVersion)})))
}

func managesConnectionOutputs(database *epco.Database) bool {
	return database.DeletionTimestamp.IsZero() && database.Spec.ManagementPolicy != epco.ManagementObserve && database.Spec.Ensure != epco.EnsureAbsent
}

func connectionCleanupInterrupted(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errDatabaseSnapshotChanged)
}
