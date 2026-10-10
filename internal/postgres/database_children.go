package postgres

import (
	"context"
	"slices"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

func (s *databaseSession) applyDatabaseChildren(ctx context.Context, state *DatabaseState, desired DatabaseDesired) error {
	var err error
	presentSchemas, absentSchemas := desired.Spec.Schemas, []epco.SchemaSpec(nil)
	if len(desired.Spec.Extensions) > 0 {
		presentSchemas = slices.DeleteFunc(slices.Clone(desired.Spec.Schemas), func(schema epco.SchemaSpec) bool { return schema.Ensure == epco.EnsureAbsent })
		absentSchemas = slices.DeleteFunc(slices.Clone(desired.Spec.Schemas), func(schema epco.SchemaSpec) bool { return schema.Ensure != epco.EnsureAbsent })
	}
	if len(desired.Spec.Schemas) != 0 {
		if !state.AllowConnections {
			return &Failure{DependenciesNotReady, "Schema reconciliation requires the target database to allow connections"}
		}
		if len(presentSchemas) > 0 {
			if err = s.applySchemas(ctx, state, presentSchemas); err != nil {
				return databaseFailure(err)
			}
		}
	}
	if len(desired.Spec.Extensions) != 0 {
		if _, err = s.reconcileExtensions(ctx, desired.Name, desired.Spec.Extensions, desired.Spec.Schemas); err != nil {
			return err
		}
	}
	if len(absentSchemas) > 0 {
		if err = s.applySchemas(ctx, state, absentSchemas); err != nil {
			return databaseFailure(err)
		}
	}
	return nil
}

func (s *databaseSession) observeDatabaseDesired(ctx context.Context, state *DatabaseState, desired DatabaseDesired) (*DatabaseApplyResult, error) {
	if !state.Exists {
		return nil, &Failure{DatabaseNotFound, "The observed SQL database does not exist"}
	}
	if _, err := s.InspectSchemas(ctx, desired.Name, desired.Spec.Schemas); err != nil {
		return nil, err
	}
	if desired.Spec.Ensure != epco.EnsureAbsent && len(desired.Spec.Extensions) > 0 {
		states, err := s.InspectExtensions(ctx, desired.Name, desired.Spec.Extensions)
		if err != nil {
			return nil, err
		}
		for i, extension := range desired.Spec.Extensions {
			if !extensionMatches(&states[i], extension) {
				return nil, &ExtensionFailure{Name: string(extension.Name), Failure: &Failure{DependenciesNotReady, "The observed extension does not match its requested presence, version or schema"}}
			}
		}
	}
	return &DatabaseApplyResult{}, nil
}
