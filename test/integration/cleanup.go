package integration

import (
	"context"
	"database/sql"
	"errors"
)

// withCleanupConnection isolates the cleanup timeout setting from every normal
// fixture connection. Checkpoint waits use the caller's bounded cleanup context.
func (s *Server) withCleanupConnection(
	ctx context.Context, database string, cleanup func(*sql.Conn) error,
) (resultErr error) {
	pool, err := s.Open(database, "postgres", s.password)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, pool.Close()) }()
	connection, err := pool.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, connection.Close()) }()
	if _, err = connection.ExecContext(ctx, "SET statement_timeout=0"); err != nil {
		return err
	}
	return cleanup(connection)
}

// cleanupOwnedSQL receives only the names allocated by the calling fixture.
// Every drop is attempted and retained in the final error, even after a failure.
func (s *Server) cleanupOwnedSQL(ctx context.Context, databases, roles []string) error {
	return s.withCleanupConnection(ctx, "postgres", func(connection *sql.Conn) error {
		var errs []error
		for _, name := range databases {
			if name != "" {
				_, err := connection.ExecContext(ctx, "DROP DATABASE IF EXISTS "+quoteIdentifier(name)+" WITH (FORCE)")
				errs = append(errs, err)
			}
		}
		for _, name := range roles {
			if name != "" {
				_, err := connection.ExecContext(ctx, "DROP ROLE IF EXISTS "+quoteIdentifier(name))
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	})
}
