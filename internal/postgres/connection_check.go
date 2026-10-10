package postgres

import "context"

// DatabaseConnectionSession confirms application catalog eligibility without logging in as it.
type DatabaseConnectionSession interface {
	CheckConnection(context.Context, string, string) error
}

func (s *databaseSession) CheckConnection(ctx context.Context, database, role string) error {
	bounded, cancel := context.WithTimeout(ctx, s.config.StatementTimeout)
	defer cancel()
	var eligible bool
	err := s.connection.QueryRow(bounded, `SELECT EXISTS (
		SELECT 1 FROM pg_catalog.pg_database d CROSS JOIN pg_catalog.pg_roles r
		WHERE d.datname=$1 AND r.rolname=$2 AND r.rolcanlogin
		AND pg_catalog.has_database_privilege(r.oid,d.oid,'CONNECT'))`, database, role).Scan(&eligible)
	if err != nil {
		return databaseFailure(err)
	}
	if !eligible {
		return &Failure{DependenciesNotReady, "The application role, database or effective CONNECT is not confirmed"}
	}
	return nil
}
