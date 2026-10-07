package integration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Fixture owns a unique restricted login and database. Admin connects to that
// database as the suite administrator; Limited connects as the per-spec login.
type Fixture struct {
	Server                   *Server
	Admin, Limited           *sql.DB
	Database, Role, Password string
}

// NewFixture allocates isolated names and removes partial setup on error.
func (s *Server) NewFixture(ctx context.Context) (_ *Fixture, resultErr error) {
	f := &Fixture{Server: s}
	var err error
	f.Database, err = randomName("db_")
	if err != nil {
		return nil, err
	}
	f.Role, err = randomName("role_")
	if err != nil {
		return nil, err
	}
	f.Password, err = randomName("password-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			resultErr = errors.Join(resultErr, f.Close(cleanupCtx))
		}
	}()
	createRole := "CREATE ROLE " + quoteIdentifier(f.Role) +
		" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD " + quoteLiteral(f.Password)
	if _, err = s.Admin.ExecContext(ctx, createRole); err != nil {
		return nil, fmt.Errorf("create fixture role: %w", err)
	}
	// The administrator owns the database: tests can explicitly grant and revoke
	// privileges from a genuinely restricted login.
	if _, err = s.Admin.ExecContext(ctx, "CREATE DATABASE "+quoteIdentifier(f.Database)); err != nil {
		return nil, fmt.Errorf("create fixture database: %w", err)
	}
	revoke := "REVOKE CONNECT ON DATABASE " + quoteIdentifier(f.Database) + " FROM PUBLIC"
	if _, err = s.Admin.ExecContext(ctx, revoke); err != nil {
		return nil, err
	}
	grant := "GRANT CONNECT ON DATABASE " + quoteIdentifier(f.Database) + " TO " + quoteIdentifier(f.Role)
	if _, err = s.Admin.ExecContext(ctx, grant); err != nil {
		return nil, err
	}
	f.Admin, err = s.Open(f.Database, "postgres", s.password)
	if err != nil {
		return nil, err
	}
	f.Limited, err = s.Open(f.Database, f.Role, f.Password)
	if err != nil {
		return nil, err
	}
	if err = f.Admin.PingContext(ctx); err != nil {
		return nil, err
	}
	if err = f.Limited.PingContext(ctx); err != nil {
		return nil, err
	}
	return f, nil
}

// Close terminates sessions only in the owned database, drops that database, then
// removes its login. The suite's admin connection remains usable for assertions.
func (f *Fixture) Close(ctx context.Context) error {
	var errs []error
	if f.Limited != nil {
		errs = append(errs, f.Limited.Close())
		f.Limited = nil
	}
	if f.Admin != nil {
		errs = append(errs, f.Admin.Close())
		f.Admin = nil
	}
	if f.Server == nil || f.Server.Admin == nil {
		return errors.Join(errs...)
	}
	if f.Database != "" {
		_, err := f.Server.Admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+quoteIdentifier(f.Database)+" WITH (FORCE)")
		errs = append(errs, err)
	}
	if f.Role != "" {
		_, err := f.Server.Admin.ExecContext(ctx, "DROP ROLE IF EXISTS "+quoteIdentifier(f.Role))
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// RoleExists and DatabaseExists check PostgreSQL's catalogs after mutations and cleanup.
func (s *Server) RoleExists(ctx context.Context, role string) (bool, error) {
	var exists bool
	err := s.Admin.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname=$1)", role).Scan(&exists)
	return exists, err
}
func (s *Server) DatabaseExists(ctx context.Context, database string) (bool, error) {
	var exists bool
	err := s.Admin.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname=$1)", database).Scan(&exists)
	return exists, err
}

// HasTablePrivilege verifies the effective privilege, including inherited grants.
func (f *Fixture) HasTablePrivilege(ctx context.Context, table, privilege string) (bool, error) {
	var allowed bool
	err := f.Admin.QueryRowContext(ctx, "SELECT has_table_privilege($1,$2,$3)", f.Role, table, privilege).Scan(&allowed)
	return allowed, err
}

// SQLState extracts an authentication/authorization error code without exposing
// connection strings or server error detail in assertions.
func SQLState(err error) string {
	type stateError interface{ SQLState() string }
	var state stateError
	if errors.As(err, &state) {
		return state.SQLState()
	}
	return ""
}
