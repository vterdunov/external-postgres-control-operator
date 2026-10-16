package postgres

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

func validateExtensions(spec epco.DatabaseSpec, maximum int) error {
	if spec.Ensure == epco.EnsureAbsent {
		return nil
	}
	seen := make(map[string]bool, len(spec.Extensions))
	for _, extension := range spec.Extensions {
		name := string(extension.Name)
		if err := validateDatabaseIdentifier(name, maximum); err != nil {
			return err
		}
		if seen[name] {
			return &Failure{InvalidConfiguration, "Duplicate SQL extension declarations are invalid"}
		}
		seen[name] = true
		if extension.Ensure == epco.EnsureAbsent {
			continue
		}
		if strings.ContainsRune(extension.Version, 0) {
			return &Failure{InvalidConfiguration, "An extension version contains an invalid byte"}
		}
		if extension.Schema == "" {
			continue
		}
		if err := validateDatabaseIdentifier(string(extension.Schema), maximum); err != nil {
			return err
		}
		if spec.ManagementPolicy != epco.ManagementObserve && IsProtectedSchema(string(extension.Schema)) {
			return &Failure{ProtectedSchema, "The extension target schema is protected from management"}
		}
		for _, schema := range spec.Schemas {
			if schema.Name == extension.Schema && schema.Ensure == epco.EnsureAbsent {
				return &Failure{InvalidConfiguration, "A present extension requires an explicitly absent schema"}
			}
		}
	}
	return nil
}

func inspectExtension(ctx context.Context, q roleQuerier, name string) (*ExtensionState, error) {
	state := &ExtensionState{Name: name}
	// Keep comparisons qualified alongside relations and functions so catalog
	// inspection is independent of session search_path changes.
	err := q.QueryRow(ctx, `SELECT e.extversion,n.nspname,
		pg_catalog.pg_get_userbyid(e.extowner),e.extrelocatable
		FROM pg_catalog.pg_extension e
		JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) e.extnamespace
		WHERE e.extname OPERATOR(pg_catalog.=) $1`, name).
		Scan(&state.Version, &state.Schema, &state.Owner, &state.Relocatable)
	if errors.Is(err, pgx.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return nil, err
	}
	state.Exists = true
	return state, nil
}

func extensionMatches(state *ExtensionState, desired epco.ExtensionSpec) bool {
	if desired.Ensure == epco.EnsureAbsent {
		return !state.Exists
	}
	return state.Exists && (desired.Version == "" || desired.Version == state.Version) &&
		(desired.Schema == "" || string(desired.Schema) == state.Schema)
}

func (s *databaseSession) InspectExtensions(ctx context.Context, name string, extensions []epco.ExtensionSpec) ([]ExtensionState, error) {
	bounded, cancel := context.WithTimeout(ctx, s.config.StatementTimeout)
	defer cancel()
	database, err := s.Inspect(bounded, name)
	if err != nil {
		return nil, err
	}
	if err = validateExtensions(epco.DatabaseSpec{Extensions: extensions, ManagementPolicy: epco.ManagementObserve}, database.MaxIdentifierLength); err != nil {
		return nil, err
	}
	if !database.Exists {
		return nil, &Failure{DatabaseNotFound, databaseNotFoundMessage}
	}
	if len(extensions) == 0 {
		return []ExtensionState{}, nil
	}
	if !database.AllowConnections {
		return nil, &Failure{DependenciesNotReady, "The target database does not allow extension inspection"}
	}
	connection, disposal, err := s.targetConnection(bounded, name)
	if err != nil {
		return nil, databaseFailure(err)
	}
	defer closeDatabaseConnection(disposal)
	states := make([]ExtensionState, 0, len(extensions))
	for _, extension := range extensions {
		state, inspectErr := inspectExtension(bounded, connection, string(extension.Name))
		if inspectErr != nil {
			return nil, databaseFailure(inspectErr)
		}
		states = append(states, *state)
	}
	return states, nil
}

type extensionVersion struct {
	Version, Schema string
	Relocatable     bool
	Requires        []string
}

func availableExtensionVersion(ctx context.Context, q roleQuerier, name, version string) (*extensionVersion, error) {
	if version == "" {
		if err := q.QueryRow(ctx, `SELECT COALESCE(default_version,'') FROM pg_catalog.pg_available_extensions
			WHERE name OPERATOR(pg_catalog.=) $1`, name).Scan(&version); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, &Failure{UnsupportedFeature, "The requested extension is not available on the PostgreSQL server"}
			}
			return nil, err
		}
		if version == "" {
			return nil, &Failure{UnsupportedFeature, "The PostgreSQL extension control file does not specify a default installation version"}
		}
	}
	metadata := &extensionVersion{Version: version}
	err := q.QueryRow(ctx, `SELECT relocatable,COALESCE(schema::pg_catalog.text,''),
		COALESCE(requires::pg_catalog.text[],ARRAY[]::pg_catalog.text[])
		FROM pg_catalog.pg_available_extension_versions
		WHERE name OPERATOR(pg_catalog.=) $1 AND version OPERATOR(pg_catalog.=) $2`, name, version).
		Scan(&metadata.Relocatable, &metadata.Schema, &metadata.Requires)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, &Failure{UnsupportedFeature, "The requested extension version is not available on the PostgreSQL server"}
	}
	return metadata, err
}
