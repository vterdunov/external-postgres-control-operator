package controller

import (
	"context"
	"slices"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func (r *DatabaseReconciler) prepareConnectionInputs(ctx context.Context, database *epco.Database, cluster *epco.PostgresCluster, config *postgres.Config, secrets map[string]*corev1.Secret) ([]connectionSecretInput, error) {
	if postgres.IsProtectedDatabase(EffectiveDatabaseName(database), config.Connection.Database) {
		return nil, &postgres.Failure{Reason: postgres.ProtectedDatabase, Message: "The SQL database is protected from management"}
	}
	for _, schema := range database.Spec.Schemas {
		if postgres.IsProtectedSchema(string(schema.Name)) {
			return nil, &postgres.Failure{Reason: postgres.ProtectedSchema, Message: "A SQL schema is protected from management"}
		}
	}
	inputs, err := r.connectionInputs(ctx, database, cluster, config, secrets)
	if err != nil {
		return nil, err
	}
	if controllerutil.AddFinalizer(database, DatabaseFinalizer) {
		if err = r.Client.Update(ctx, database); err != nil {
			return nil, err
		}
	}
	if err = r.publishInitialConnectionSecrets(ctx, database, inputs); err != nil {
		return nil, err
	}
	return inputs, nil
}

func (r *DatabaseReconciler) connectionInputs(ctx context.Context, database *epco.Database, cluster *epco.PostgresCluster, config *postgres.Config, secrets map[string]*corev1.Secret) ([]connectionSecretInput, error) {
	if err := r.preflightConnectionNames(ctx, database); err != nil {
		return nil, err
	}
	inputs := make([]connectionSecretInput, 0, len(database.Spec.ConnectionSecrets))
	for _, entry := range database.Spec.ConnectionSecrets {
		role, err := r.connectionRole(ctx, database, cluster, config, entry)
		if err != nil {
			return nil, err
		}
		source, err := r.connectionSource(ctx, role)
		if err != nil {
			return nil, err
		}
		data, err := r.connectionData(ctx, database, cluster, role, source, entry, secrets)
		if err != nil {
			return nil, err
		}
		if err = r.checkConnectionOutput(ctx, database, entry.Name); err != nil {
			return nil, err
		}
		inputs = append(inputs, connectionSecretInput{entry: entry, role: role, source: source, data: data})
	}
	return inputs, nil
}

func (r *DatabaseReconciler) preflightConnectionNames(ctx context.Context, database *epco.Database) error {
	roles := &epco.DatabaseRoleList{}
	if err := r.Client.List(ctx, roles, client.InNamespace(database.Namespace)); err != nil {
		return err
	}
	databases := &epco.DatabaseList{}
	if err := r.Client.List(ctx, databases, client.InNamespace(database.Namespace)); err != nil {
		return err
	}
	seen := make(map[string]bool)
	for _, entry := range database.Spec.ConnectionSecrets {
		if entry.Name == "" || entry.RoleRef.Name == "" || seen[entry.Name] {
			return &postgres.Failure{Reason: postgres.InvalidConfiguration, Message: "Application outputs require distinct names and local role references"}
		}
		seen[entry.Name] = true
		for _, other := range databases.Items {
			if other.UID != database.UID && slices.Contains(databaseOutputNames(&other), entry.Name) {
				return connectionConflict()
			}
		}
		if slices.ContainsFunc(roles.Items, func(role epco.DatabaseRole) bool { return roleSourceName(&role) == entry.Name }) {
			return connectionConflict()
		}
	}
	return nil
}

func (r *DatabaseReconciler) connectionRole(ctx context.Context, database *epco.Database, cluster *epco.PostgresCluster, config *postgres.Config, entry epco.ConnectionSecret) (*epco.DatabaseRole, error) {
	role := &epco.DatabaseRole{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: database.Namespace, Name: entry.RoleRef.Name}, role); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &postgres.Failure{Reason: postgres.DependenciesNotReady, Message: "The exported DatabaseRole does not exist"}
		}
		return nil, err
	}
	if !role.DeletionTimestamp.IsZero() {
		return nil, &postgres.Failure{Reason: postgres.DependenciesNotReady, Message: "The exported DatabaseRole is being deleted"}
	}
	if !exportableRole(role) || roleClusterKey(role) != databaseClusterKey(database) {
		return nil, &postgres.Failure{Reason: postgres.InvalidConfiguration, Message: "An application output requires a local managed LOGIN role with a password source on the same cluster"}
	}
	if postgres.IsProtectedRole(EffectiveRoleName(role), config.Connection.User) || cluster.Namespace == role.Namespace && cluster.Spec.CredentialsSecret.Name == roleSourceName(role) {
		return nil, &postgres.Failure{Reason: postgres.ProtectedRoleReason, Message: "Administrative credentials cannot be exported"}
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	conflict, err := FindRoleOwnershipConflict(ctx, reader, role)
	if err != nil {
		return nil, err
	}
	if conflict != nil {
		return nil, &postgres.Failure{Reason: postgres.OwnershipConflict, Message: "Another DatabaseRole manages the exported SQL role"}
	}
	if err = checkRoleSourceOutputReservation(ctx, r.Client, role); err != nil {
		return nil, err
	}
	return role, nil
}

func exportableRole(role *epco.DatabaseRole) bool {
	oneSource := (role.Spec.PasswordSecret != nil) != (role.Spec.PasswordGeneration != nil)
	return role.Spec.ManagementPolicy != epco.ManagementObserve && role.Spec.Login && !role.Spec.DisablePassword && oneSource && roleSourceName(role) != ""
}

func (r *DatabaseReconciler) connectionSource(ctx context.Context, role *epco.DatabaseRole) (*corev1.Secret, error) {
	source := &corev1.Secret{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: role.Namespace, Name: roleSourceName(role)}, source); err != nil {
		if apierrors.IsNotFound(err) {
			reason := postgres.SecretNotFound
			if role.Spec.PasswordGeneration != nil {
				reason = postgres.DependenciesNotReady
				if role.Status.GeneratedPasswordInitialized {
					reason = postgres.CredentialsMissing
				}
			}
			return nil, &postgres.Failure{Reason: reason, Message: "The application password source is unavailable"}
		}
		return nil, err
	}
	if err := validatePasswordSource(role, source); err != nil {
		return nil, err
	}
	if source.Labels[ConnectionSecretLabel] == connectionSecretLabelValue {
		return nil, connectionConflict()
	}
	if role.Spec.PasswordGeneration == nil {
		return source, nil
	}
	if !generatedProvenance(role, source) || source.Annotations[GeneratedRoleUIDAnnotation] != string(role.UID) || !validGeneratedOwners(role, source) || len(source.OwnerReferences) > 0 {
		return nil, connectionConflict()
	}
	if !role.Status.GeneratedPasswordInitialized {
		return nil, &postgres.Failure{Reason: postgres.DependenciesNotReady, Message: "The generated password source initialization is not confirmed"}
	}
	return source, nil
}

func (r *DatabaseReconciler) connectionData(ctx context.Context, database *epco.Database, cluster *epco.PostgresCluster, role *epco.DatabaseRole, source *corev1.Secret, entry epco.ConnectionSecret, secrets map[string]*corev1.Secret) (map[string][]byte, error) {
	input := postgres.ApplicationConnection{Username: EffectiveRoleName(role), Password: string(source.Data[corev1.BasicAuthPasswordKey]), Host: cluster.Spec.Connection.Host, Port: cluster.Spec.Connection.Port, Database: EffectiveDatabaseName(database), SSLRootCertPath: entry.SSLRootCertPath}
	if cluster.Spec.TLS != nil {
		input.SSLMode = cluster.Spec.TLS.Mode
		var err error
		input.CA, err = r.connectionCA(ctx, cluster, secrets)
		if err != nil {
			return nil, err
		}
	}
	return postgres.BuildConnectionSecretData(input)
}

func (r *DatabaseReconciler) connectionCA(ctx context.Context, cluster *epco.PostgresCluster, secrets map[string]*corev1.Secret) ([]byte, error) {
	reference := cluster.Spec.TLS.CASecret
	if reference == nil {
		return nil, nil
	}
	if _, resolved := secrets[reference.Name]; !resolved {
		// Administrative TLS=disable does not consume a CA. Application
		// exports still resolve their explicitly configured public input.
		ca := &corev1.Secret{}
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: reference.Name}, ca); err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, err
			}
			ca = nil
		}
		secrets[reference.Name] = ca
	}
	ca := secrets[reference.Name]
	if ca == nil {
		return nil, &postgres.Failure{Reason: postgres.SecretNotFound, Message: "The application CA Secret does not exist"}
	}
	data := ca.Data[reference.Key]
	if len(data) == 0 {
		return nil, &postgres.Failure{Reason: postgres.InvalidSecret, Message: "The application CA Secret has no configured certificate data"}
	}
	return data, nil
}

func (r *DatabaseReconciler) checkConnectionOutput(ctx context.Context, database *epco.Database, name string) error {
	output := &corev1.Secret{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: database.Namespace, Name: name}, output); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !connectionProvenance(database, output) || len(output.OwnerReferences) != 0 {
		return connectionConflict()
	}
	return nil
}
