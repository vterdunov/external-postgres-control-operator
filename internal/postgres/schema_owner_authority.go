package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

type schemaAuthorityRequired struct {
	schema, oldOwner, newOwner, databaseOwner string
	refusal                                   error
}

func (e *schemaAuthorityRequired) Error() string { return e.refusal.Error() }
func (e *schemaAuthorityRequired) Unwrap() error { return e.refusal }

func transferSchemaOwner(ctx context.Context, tx pgx.Tx, database *DatabaseState, state *SchemaState, owner string) error {
	statement := "ALTER SCHEMA " + pgx.Identifier{state.Name}.Sanitize() + " OWNER TO " + pgx.Identifier{owner}.Sanitize()
	// Give native service hooks and existing combined authority the first try.
	nativeErr := attemptSchemaOwner(ctx, tx, "", statement)
	if nativeErr == nil || !authorizationRefused(nativeErr) {
		return nativeErr
	}
	oldOwner := state.Owner
	if oldOwner == databaseOwnerRole {
		oldOwner = database.Owner
	}
	// Preserve an already sufficient old-owner SET path, including external owners.
	err := attemptSchemaOwner(ctx, tx, oldOwner, statement)
	if err == nil || !authorizationRefused(err) {
		return err
	}
	if database.ServerVersion < 160000 {
		return nativeErr
	}
	return &schemaAuthorityRequired{schema: state.Name, oldOwner: oldOwner, newOwner: owner,
		databaseOwner: database.Owner, refusal: nativeErr}
}

func attemptSchemaOwner(ctx context.Context, parent pgx.Tx, owner, statement string) (err error) {
	tx, err := parent.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), membershipRollbackTimeout)
			defer cancel()
			if rollbackErr := tx.Rollback(cleanup); rollbackErr != nil {
				err = &Failure{ConnectionUnavailable, "Unable to confirm schema authority attempt rollback"}
			}
		}
	}()
	if owner != "" {
		if err = selectDatabaseRole(ctx, tx, owner, true); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, statement); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "RESET ROLE"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *databaseSession) prepareSchemaAuthority(ctx context.Context, connection *pgx.Conn, databaseName string, needed *schemaAuthorityRequired) error {
	// The refused schema transaction has ended before retained grants are made.
	database, err := inspectDatabase(ctx, connection, databaseName)
	if err != nil {
		return err
	}
	state, err := inspectSchema(ctx, connection, needed.schema)
	if err != nil {
		return err
	}
	oldOwner := state.Owner
	if oldOwner == databaseOwnerRole {
		oldOwner = database.Owner
	}
	if !state.Exists || oldOwner != needed.oldOwner || database.Owner != needed.databaseOwner {
		return &Failure{DependenciesNotReady, "Schema or database ownership changed while preparing administrative authority"}
	}
	if err = s.ensureServiceInheritance(ctx, oldOwner); err != nil {
		return err
	}
	if err = s.ensureServiceRole(ctx, needed.newOwner, RoleCapabilityProvision); err != nil {
		return err
	}
	var create bool
	if err = connection.QueryRow(ctx, `SELECT pg_catalog.has_database_privilege(session_user,$1,'CREATE')`, databaseName).Scan(&create); err != nil {
		return err
	}
	if !create {
		var ownerCreate bool
		if err = connection.QueryRow(ctx, `SELECT pg_catalog.has_database_privilege($1,$2,'CREATE')`, database.Owner, databaseName).Scan(&ownerCreate); err != nil {
			return err
		}
		if !ownerCreate {
			return needed.refusal
		}
		if err = s.ensureServiceInheritance(ctx, database.Owner); err != nil {
			return err
		}
	}
	var inherited, set bool
	if err = connection.QueryRow(ctx, `SELECT pg_catalog.pg_has_role(session_user,$1,'USAGE'),
 pg_catalog.pg_has_role(session_user,$2,'SET'),pg_catalog.has_database_privilege(session_user,$3,'CREATE')`,
		oldOwner, needed.newOwner, databaseName).Scan(&inherited, &set, &create); err != nil {
		return err
	}
	if !inherited || !set || !create {
		return &Failure{InsufficientPrivileges, fmt.Sprintf("The administrative connection cannot combine schema ownership, destination SET and database CREATE authority for schema %q", needed.schema)}
	}
	return nil
}
