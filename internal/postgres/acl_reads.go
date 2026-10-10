package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

// aclReadQuerier bounds each read through consumption, rather than applying one
// deadline to discovery and mutation of every object in a broad scope.
type aclReadQuerier struct {
	roleQuerier
	timeout time.Duration
}

func (s *databaseSession) aclQueries(q roleQuerier) roleQuerier {
	if q == nil {
		return nil
	}
	return &aclReadQuerier{roleQuerier: q, timeout: s.config.StatementTimeout}
}

func (q *aclReadQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	bounded, cancel := context.WithTimeout(ctx, q.timeout)
	return &aclReadRow{Row: q.roleQuerier.QueryRow(bounded, sql, args...), cancel: cancel}
}

func (q *aclReadQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	bounded, cancel := context.WithTimeout(ctx, q.timeout)
	rows, err := q.roleQuerier.Query(bounded, sql, args...)
	if err != nil {
		if rows != nil {
			rows.Close()
		}
		cancel()
		return nil, err
	}
	return &aclReadRows{Rows: rows, cancel: cancel}, nil
}

type aclReadRow struct {
	pgx.Row
	cancel context.CancelFunc
}

func (r *aclReadRow) Scan(dest ...any) error {
	defer r.cancel()
	return r.Row.Scan(dest...)
}

type aclReadRows struct {
	pgx.Rows
	cancel context.CancelFunc
}

func (r *aclReadRows) Close() {
	r.Rows.Close()
	r.cancel()
}

func (r *aclReadRows) Next() bool {
	if r.Rows.Next() {
		return true
	}
	r.Close()
	return false
}

func (e *aclEngine) queries(b epco.ACLBinding) roleQuerier {
	connection := e.connection(b)
	if connection == nil {
		return nil
	}
	return e.session.aclQueries(connection)
}
