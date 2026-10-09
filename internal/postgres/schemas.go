package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

func (s *databaseSession) targetConnection(ctx context.Context, name string) (*pgx.Conn, *pgxProbeConnection, error) {
	config := s.config
	config.Connection = s.config.Connection.Copy()
	config.Connection.Database = name
	return openDatabaseConnection(ctx, &config)
}

func (s *databaseSession) InspectSchemas(ctx context.Context, name string, schemas []epco.SchemaSpec) ([]SchemaState, error) {
	bounded, cancel := context.WithTimeout(ctx, s.config.StatementTimeout)
	defer cancel()
	database, err := inspectDatabase(bounded, s.connection, name)
	if err != nil {
		return nil, databaseFailure(err)
	}
	if err = validateDatabaseIdentifier(name, database.MaxIdentifierLength); err != nil {
		return nil, err
	}
	if !database.Exists {
		return nil, &Failure{DatabaseNotFound, "The target SQL database does not exist"}
	}
	for _, schema := range schemas {
		if err = validateDatabaseIdentifier(string(schema.Name), database.MaxIdentifierLength); err != nil {
			return nil, err
		}
	}
	if len(schemas) == 0 {
		return []SchemaState{}, nil
	}
	if !database.AllowConnections {
		return nil, &Failure{DependenciesNotReady, "The target database does not allow connections"}
	}
	connection, disposal, err := s.targetConnection(bounded, name)
	if err != nil {
		return nil, databaseFailure(err)
	}
	defer closeDatabaseConnection(disposal)
	result := make([]SchemaState, 0, len(schemas))
	for _, schema := range schemas {
		state, inspectErr := inspectSchema(bounded, connection, string(schema.Name))
		if inspectErr != nil {
			return nil, databaseFailure(inspectErr)
		}
		result = append(result, *state)
	}
	return result, nil
}

func inspectSchema(ctx context.Context, q roleQuerier, name string) (*SchemaState, error) {
	state := &SchemaState{Name: name}
	err := q.QueryRow(ctx, `SELECT pg_catalog.pg_get_userbyid(nspowner)
		FROM pg_catalog.pg_namespace WHERE nspname=$1`, name).Scan(&state.Owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return nil, err
	}
	state.Exists = true
	return state, nil
}

func (s *databaseSession) applySchemas(ctx context.Context, database *DatabaseState, schemas []epco.SchemaSpec) error {
	connection, disposal, err := s.targetConnection(ctx, database.Name)
	if err != nil {
		return err
	}
	defer closeDatabaseConnection(disposal)
	tx, err := connection.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackDatabaseTransaction(tx)
	for _, schema := range schemas {
		// Each operation starts from the supplied administrator, regardless of
		// the previous schema's authorized local role.
		if _, err = tx.Exec(ctx, "RESET ROLE"); err != nil {
			return err
		}
		state, inspectErr := inspectSchema(ctx, tx, string(schema.Name))
		if inspectErr != nil {
			return inspectErr
		}
		if err = applySchema(ctx, tx, database, state, schema); err != nil {
			return err
		}
		confirmed, confirmErr := inspectSchema(ctx, tx, string(schema.Name))
		if confirmErr != nil {
			return confirmErr
		}
		if schema.Ensure == epco.EnsureAbsent {
			if confirmed.Exists {
				return &Failure{DependenciesNotReady, "Requested schema absence could not be confirmed"}
			}
			continue
		}
		owner := string(schema.Owner)
		if owner == "" {
			owner = database.Owner
			if state.Exists {
				owner = state.Owner
			}
		}
		if !confirmed.Exists || confirmed.Owner != owner {
			return &Failure{DependenciesNotReady, "Requested schema ownership could not be confirmed"}
		}
	}
	return tx.Commit(ctx)
}

func applySchema(ctx context.Context, tx pgx.Tx, database *DatabaseState, state *SchemaState, desired epco.SchemaSpec) error {
	name := pgx.Identifier{string(desired.Name)}.Sanitize()
	if desired.Ensure == epco.EnsureAbsent {
		if !state.Exists {
			return nil
		}
		owner := state.Owner
		if owner == "pg_database_owner" {
			owner = database.Owner
		}
		if err := selectDatabaseRole(ctx, tx, owner, true); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "DROP SCHEMA "+name+" RESTRICT")
		return err
	}
	owner := string(desired.Owner)
	if state.Exists && (owner == "" || owner == state.Owner) {
		return nil
	}
	if owner == "" {
		owner = database.Owner
	}
	var exists bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=$1)", owner).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return &Failure{DependenciesNotReady, "The requested schema owner does not exist"}
	}
	if !state.Exists {
		// CREATE SCHEMA AUTHORIZATION accepts an owner without CREATE when
		// the supplied administrator has CREATE and can SET to that owner.
		var create bool
		if err := tx.QueryRow(ctx, "SELECT pg_catalog.has_database_privilege(current_user,$1,'CREATE')", database.Name).Scan(&create); err != nil {
			return err
		}
		if !create {
			if err := selectDatabaseRole(ctx, tx, owner, true); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, "CREATE SCHEMA "+name+" AUTHORIZATION "+pgx.Identifier{owner}.Sanitize())
		return err
	}
	// Owner transfer requires the administrative principal to combine the
	// existing owner, new-owner SET authority and database CREATE privileges.
	currentOwner := state.Owner
	if currentOwner == "pg_database_owner" {
		// PostgreSQL 15+ assigns public to the database-owner pseudo-role.
		// Resolve its implicit owner to the actual database role.
		currentOwner = database.Owner
	}
	if err := selectDatabaseRole(ctx, tx, currentOwner, true); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "ALTER SCHEMA "+name+" OWNER TO "+pgx.Identifier{owner}.Sanitize())
	return err
}
