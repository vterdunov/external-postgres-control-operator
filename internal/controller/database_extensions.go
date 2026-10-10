package controller

import (
	"context"
	"slices"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
)

func preflightDatabaseExtensions(database *epco.Database, session postgres.DatabaseSession) error {
	if database.Spec.Ensure == epco.EnsureAbsent || len(database.Spec.Extensions) == 0 {
		return nil
	}
	if _, capable := session.(postgres.DatabaseExtensionSession); !capable {
		return &postgres.Failure{Reason: postgres.UnsupportedFeature, Message: "The database adapter does not support extension reconciliation"}
	}
	return nil
}

func observeDatabaseExtensions(ctx context.Context, database *epco.Database, session postgres.DatabaseSession) error {
	if len(database.Spec.Extensions) == 0 {
		return nil
	}
	extensions, capable := session.(postgres.DatabaseExtensionSession)
	if !capable {
		return &postgres.Failure{Reason: postgres.UnsupportedFeature, Message: "The database adapter does not support extension observation"}
	}
	states, err := extensions.InspectExtensions(ctx, EffectiveDatabaseName(database), database.Spec.Extensions)
	if err != nil {
		return err
	}
	for _, desired := range database.Spec.Extensions {
		i := slices.IndexFunc(states, func(state postgres.ExtensionState) bool { return state.Name == string(desired.Name) })
		if i < 0 {
			return &postgres.ExtensionFailure{Name: string(desired.Name), Failure: &postgres.Failure{Reason: postgres.DependenciesNotReady, Message: "The listed SQL extension was not inspected"}}
		}
		state := states[i]
		if desired.Ensure == epco.EnsureAbsent && state.Exists || desired.Ensure != epco.EnsureAbsent &&
			(!state.Exists || desired.Version != "" && desired.Version != state.Version || desired.Schema != "" && string(desired.Schema) != state.Schema) {
			return &postgres.ExtensionFailure{Name: string(desired.Name), Failure: &postgres.Failure{Reason: postgres.DependenciesNotReady, Message: "The listed SQL extension does not match its requested presence, version or schema"}}
		}
	}
	return nil
}

func (r *DatabaseReconciler) recordDatabaseBase(ctx context.Context, database *epco.Database, result *postgres.DatabaseApplyResult) error {
	if result == nil || !result.BaseManaged || database.Status.HasManagedSQL || database.Spec.ManagementPolicy == epco.ManagementObserve {
		return nil
	}
	if err := r.ensureDatabaseCurrent(ctx, database); err != nil {
		return err
	}
	database.Status.HasManagedSQL = true
	return r.Client.Status().Update(ctx, database)
}
