package postgres

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

func (s *databaseSession) ReconcileExtensions(ctx context.Context, name string, extensions []epco.ExtensionSpec) (*ExtensionResult, error) {
	return s.reconcileExtensions(ctx, name, extensions, nil)
}

func (s *databaseSession) reconcileExtensions(ctx context.Context, name string, extensions []epco.ExtensionSpec, schemas []epco.SchemaSpec) (*ExtensionResult, error) {
	database, err := s.Inspect(ctx, name)
	if err != nil {
		return nil, err
	}
	if err = validateExtensions(epco.DatabaseSpec{Extensions: extensions}, database.MaxIdentifierLength); err != nil {
		return nil, err
	}
	if !database.Exists {
		return nil, &Failure{DatabaseNotFound, databaseNotFoundMessage}
	}
	if len(extensions) > 0 && !database.AllowConnections {
		return nil, &Failure{DependenciesNotReady, "Extension reconciliation requires the target database to allow connections"}
	}
	result := &ExtensionResult{States: make([]ExtensionState, 0, len(extensions))}
	ordered := slices.Clone(extensions)
	slices.SortFunc(ordered, func(a, b epco.ExtensionSpec) int { return strings.Compare(string(a.Name), string(b.Name)) })
	if len(ordered) > 0 {
		if err = s.validateExtensionDependencies(ctx, name, ordered); err != nil {
			return result, err
		}
	}
	// A successful operation removes an item from the pending set. At most N
	// passes can make progress; missing prerequisites and cycles stop after a
	// pass with no progress. Each attempt has its own bounded transaction.
	for _, absent := range []bool{true, false} {
		pending := slices.DeleteFunc(slices.Clone(ordered), func(extension epco.ExtensionSpec) bool {
			return (extension.Ensure == epco.EnsureAbsent) != absent
		})
		for len(pending) > 0 {
			next := make([]epco.ExtensionSpec, 0, len(pending))
			var first error
			for _, extension := range pending {
				if err = ctx.Err(); err != nil {
					return result, err
				}
				state, applyErr := s.reconcileExtension(ctx, name, extension, schemas)
				if applyErr == nil {
					result.States = append(result.States, *state)
					continue
				}
				failure, classified := errors.AsType[*Failure](applyErr)
				if !classified || failure.Reason != DependenciesNotReady && (!absent || failure.Reason != DeletionBlocked) {
					return result, applyErr
				}
				if first == nil {
					first = applyErr
				}
				next = append(next, extension)
			}
			if len(next) == len(pending) {
				return result, first
			}
			pending = next
		}
	}
	return result, nil
}

func (s *databaseSession) reconcileExtension(ctx context.Context, database string, desired epco.ExtensionSpec, schemas []epco.SchemaSpec) (out *ExtensionState, operationErr error) {
	bounded, cancel := context.WithTimeout(ctx, s.config.StatementTimeout)
	defer cancel()
	defer func() {
		if operationErr == nil {
			return
		}
		if bounded.Err() != nil {
			operationErr = bounded.Err()
			return
		}
		classified := databaseFailure(operationErr)
		if failure, ok := errors.AsType[*Failure](classified); ok {
			operationErr = &ExtensionFailure{Name: string(desired.Name), Failure: failure}
		} else {
			operationErr = classified
		}
	}()
	connection, disposal, err := s.targetConnection(bounded, database)
	if err != nil {
		return nil, databaseFailure(err)
	}
	defer closeDatabaseConnection(disposal)
	tx, err := connection.Begin(bounded)
	if err != nil {
		return nil, databaseFailure(err)
	}
	defer rollbackDatabaseTransaction(tx)
	state, err := inspectExtension(bounded, tx, string(desired.Name))
	if err != nil {
		return nil, databaseFailure(err)
	}
	if !extensionMatches(state, desired) {
		var recovery, readOnly bool
		if err = tx.QueryRow(bounded, `SELECT pg_catalog.pg_is_in_recovery(),pg_catalog.current_setting('transaction_read_only')::pg_catalog.bool`).Scan(&recovery, &readOnly); err != nil {
			return nil, databaseFailure(err)
		}
		if recovery || readOnly {
			return nil, &Failure{NotWritable, readOnlyMessage}
		}
		if state.Exists {
			if IsProtectedSchema(state.Schema) {
				return nil, &Failure{ProtectedSchema, "The installed extension schema is protected from management"}
			}
			err = reconcileExistingExtension(bounded, tx, state, desired)
		} else {
			err = s.createExtension(bounded, tx, database, desired, schemas)
		}
		if err != nil {
			return nil, databaseFailure(err)
		}
	}
	confirmed, err := inspectExtension(bounded, tx, string(desired.Name))
	if err != nil {
		return nil, databaseFailure(err)
	}
	if !extensionMatches(confirmed, desired) {
		return nil, &Failure{DependenciesNotReady, "The requested extension catalog state could not be confirmed"}
	}
	if err = tx.Commit(bounded); err != nil {
		return nil, databaseFailure(err)
	}
	confirmed, err = inspectExtension(bounded, connection, string(desired.Name))
	if err != nil {
		return nil, databaseFailure(err)
	}
	if !extensionMatches(confirmed, desired) {
		return nil, &Failure{DependenciesNotReady, "The requested extension catalog state changed after commit"}
	}
	return confirmed, nil
}

func (s *databaseSession) createExtension(ctx context.Context, tx pgx.Tx, database string, desired epco.ExtensionSpec, schemas []epco.SchemaSpec) error {
	metadata, err := availableExtensionVersion(ctx, tx, string(desired.Name), desired.Version)
	if err != nil {
		return err
	}
	if metadata.Schema != "" && IsProtectedSchema(metadata.Schema) {
		return &Failure{ProtectedSchema, "The extension control file requests a protected schema"}
	}
	if slices.ContainsFunc(schemas, func(schema epco.SchemaSpec) bool {
		return schema.Ensure == epco.EnsureAbsent && string(schema.Name) == metadata.Schema
	}) {
		return &Failure{InvalidConfiguration, "The extension control file requires an explicitly absent schema"}
	}
	if metadata.Schema != "" && desired.Schema != "" && metadata.Schema != string(desired.Schema) {
		return &Failure{UnsupportedFeature, "The extension control file requires a different installation schema"}
	}
	if desired.Schema != "" && metadata.Schema == "" {
		schema, inspectErr := inspectSchema(ctx, tx, string(desired.Schema))
		if inspectErr != nil {
			return inspectErr
		}
		if !schema.Exists {
			return &Failure{DependenciesNotReady, "The requested extension installation schema does not exist"}
		}
	}
	for _, required := range metadata.Requires {
		state, inspectErr := inspectExtension(ctx, tx, required)
		if inspectErr != nil {
			return inspectErr
		}
		if !state.Exists {
			return &Failure{DependenciesNotReady, "A prerequisite SQL extension is not installed"}
		}
	}
	installationSchema := string(desired.Schema)
	if installationSchema == "" && metadata.Schema == "" {
		installationSchema, err = s.ordinaryExtensionSchema(ctx, database)
		if err != nil {
			return err
		}
	}
	statement := "CREATE EXTENSION " + pgx.Identifier{string(desired.Name)}.Sanitize()
	if desired.Version != "" {
		statement += " VERSION " + literal(desired.Version)
	}
	if installationSchema != "" {
		statement += " SCHEMA " + pgx.Identifier{installationSchema}.Sanitize()
	}
	_, err = tx.Exec(ctx, statement)
	return err
}

// ordinaryExtensionSchema reads the target's database and login defaults without
// exposing operational SQL to that path. This disposable connection runs only
// the qualified built-in lookup; CREATE remains on the pinned transaction.
func (s *databaseSession) ordinaryExtensionSchema(ctx context.Context, database string) (string, error) {
	config := s.config
	config.Connection = s.config.Connection.Copy()
	config.Connection.Database = database
	delete(config.Connection.RuntimeParams, "search_path")
	connection, disposal, err := openDatabaseConnection(ctx, &config)
	if err != nil {
		return "", err
	}
	defer closeDatabaseConnection(disposal)
	var schema string
	if err = connection.QueryRow(ctx, `SELECT COALESCE(pg_catalog.current_schema()::pg_catalog.text,'')`).Scan(&schema); err != nil {
		return "", err
	}
	if schema == "" {
		return "", &Failure{DependenciesNotReady, "No ordinary default schema is available for extension installation"}
	}
	if IsProtectedSchema(schema) {
		return "", &Failure{ProtectedSchema, "The ordinary extension installation schema is protected from management"}
	}
	return schema, nil
}

func updateExtension(ctx context.Context, tx pgx.Tx, state *ExtensionState, desired epco.ExtensionSpec) error {
	move := desired.Schema != "" && string(desired.Schema) != state.Schema
	update := desired.Version != "" && desired.Version != state.Version
	var metadata *extensionVersion
	if update {
		var err error
		metadata, err = availableExtensionVersion(ctx, tx, state.Name, desired.Version)
		if err != nil {
			return err
		}
		var path bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_extension_update_paths($1)
			WHERE source OPERATOR(pg_catalog.=) $2 AND target OPERATOR(pg_catalog.=) $3
			AND path IS NOT NULL)`, state.Name, state.Version, desired.Version).Scan(&path); err != nil {
			return err
		}
		if !path {
			return &Failure{UnsupportedFeature, "No PostgreSQL extension update path reaches the requested version"}
		}
		for _, required := range metadata.Requires {
			prerequisite, err := inspectExtension(ctx, tx, required)
			if err != nil {
				return err
			}
			if !prerequisite.Exists {
				return &Failure{DependenciesNotReady, "A prerequisite SQL extension is not installed"}
			}
		}
	}
	if move {
		schema, err := inspectSchema(ctx, tx, string(desired.Schema))
		if err != nil {
			return err
		}
		if !schema.Exists {
			return &Failure{DependenciesNotReady, "The requested extension relocation schema does not exist"}
		}
		if !state.Relocatable && (!update || !metadata.Relocatable) {
			return &Failure{UnsupportedFeature, "Neither the installed nor requested extension version supports relocation"}
		}
	}
	// The current capability decides sequencing. A relocatable version moves
	// before updating; a nonrelocatable version may first update to one that
	// permits relocation. Both steps stay in this transaction.
	moveFirst := move && state.Relocatable
	if moveFirst {
		if err := moveExtension(ctx, tx, state, string(desired.Schema)); err != nil {
			return err
		}
	}
	if update {
		if _, err := tx.Exec(ctx, "ALTER EXTENSION "+pgx.Identifier{state.Name}.Sanitize()+" UPDATE TO "+literal(desired.Version)); err != nil {
			return err
		}
		confirmed, err := inspectExtension(ctx, tx, state.Name)
		if err != nil {
			return err
		}
		if !confirmed.Exists || confirmed.Version != desired.Version || confirmed.Schema != state.Schema {
			return &Failure{DependenciesNotReady, "The requested extension update could not be confirmed"}
		}
		*state = *confirmed
	}
	if move && !moveFirst {
		return moveExtension(ctx, tx, state, string(desired.Schema))
	}
	return nil
}

func moveExtension(ctx context.Context, tx pgx.Tx, state *ExtensionState, schema string) error {
	if !state.Relocatable {
		return &Failure{UnsupportedFeature, "The installed extension does not support relocation"}
	}
	if _, err := tx.Exec(ctx, "ALTER EXTENSION "+pgx.Identifier{state.Name}.Sanitize()+" SET SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		return err
	}
	confirmed, err := inspectExtension(ctx, tx, state.Name)
	if err != nil {
		return err
	}
	if !confirmed.Exists || confirmed.Schema != schema || confirmed.Version != state.Version {
		return &Failure{DependenciesNotReady, "The requested extension relocation could not be confirmed"}
	}
	*state = *confirmed
	return nil
}
