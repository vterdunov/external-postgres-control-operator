package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

func reconcileExistingExtension(ctx context.Context, tx pgx.Tx, state *ExtensionState, desired epco.ExtensionSpec) error {
	// Providers can authorize native extension DDL without allowing SET ROLE to
	// the catalog owner. Try the supplied principal before selecting that owner.
	native, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	err = applyExistingExtension(ctx, native, state, desired)
	if err == nil {
		return native.Commit(ctx)
	}
	// An update and relocation form one attempt. PostgreSQL and the mutable
	// catalog snapshot must both return to their original state before retry.
	if rollbackErr := native.Rollback(ctx); rollbackErr != nil {
		return rollbackErr
	}
	server, ok := errors.AsType[*pgconn.PgError](err)
	if !ok || server.Code != "42501" {
		return err
	}
	if err = selectDatabaseRole(ctx, tx, state.Owner, true); err != nil {
		return err
	}
	return applyExistingExtension(ctx, tx, state, desired)
}

func applyExistingExtension(ctx context.Context, tx pgx.Tx, state *ExtensionState, desired epco.ExtensionSpec) error {
	if desired.Ensure == epco.EnsureAbsent {
		_, err := tx.Exec(ctx, "DROP EXTENSION "+pgx.Identifier{state.Name}.Sanitize()+" RESTRICT")
		return err
	}
	// updateExtension changes its snapshot after each confirmed DDL step. Each
	// authority attempt needs an independent copy of the original catalog state.
	attempt := *state
	return updateExtension(ctx, tx, &attempt, desired)
}
