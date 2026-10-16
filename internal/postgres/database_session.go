package postgres

import (
	"context"
	"crypto/tls"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DatabaseOwnerSetRoleRequiredMessage is a controlled diagnostic safe for status.
const DatabaseOwnerSetRoleRequiredMessage = "The administrative connection requires SET ROLE authority for the requested database owner"

type databaseSession struct {
	connection  *pgx.Conn
	disposal    *pgxProbeConnection
	config      Config
	options     DatabaseSessionOptions
	eligibility map[roleCapabilityKey]roleCapabilityEligibility
}

func (*databaseFactory) Open(ctx context.Context, config *Config, options DatabaseSessionOptions) (DatabaseSession, error) {
	if config == nil || config.Connection == nil || config.ConnectTimeout <= 0 || config.StatementTimeout <= 0 {
		return nil, &Failure{InvalidConfiguration, "Database session configuration is invalid"}
	}
	copyConfig := *config
	copyConfig.Connection = config.Connection.Copy()
	if config.StatementTimeout.Milliseconds() > 2147483647 {
		return nil, &Failure{InvalidConfiguration, "Database statement timeout exceeds the server limit"}
	}
	copyConfig.Connection.ConnectTimeout = config.ConnectTimeout
	if copyConfig.Connection.RuntimeParams == nil {
		copyConfig.Connection.RuntimeParams = make(map[string]string)
	}
	copyConfig.Connection.RuntimeParams["statement_timeout"] = strconv.FormatInt(max(1, config.StatementTimeout.Milliseconds()), 10)
	connection, disposal, err := openDatabaseConnection(ctx, &copyConfig)
	if err != nil {
		return nil, databaseFailure(classifyConnectError(ctx, err))
	}
	return &databaseSession{connection: connection, disposal: disposal, config: copyConfig, options: options, eligibility: make(map[roleCapabilityKey]roleCapabilityEligibility)}, nil
}

func openDatabaseConnection(ctx context.Context, config *Config) (*pgx.Conn, *pgxProbeConnection, error) {
	bounded, cancel := context.WithTimeout(ctx, config.ConnectTimeout)
	defer cancel()
	connection, err := pgx.ConnectConfig(bounded, config.Connection.Copy())
	if err != nil {
		return nil, nil, err
	}
	socket := connection.PgConn().Conn()
	if encrypted, ok := socket.(*tls.Conn); ok {
		socket = encrypted.NetConn()
	}
	return connection, &pgxProbeConnection{probeConnection: connection,
		cleanupDone: connection.PgConn().CleanupDone(), socket: socket}, nil
}

func (s *databaseSession) Close(ctx context.Context) error {
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return s.disposal.Close(bounded)
}

func closeDatabaseConnection(disposal *pgxProbeConnection) {
	bounded, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = disposal.Close(bounded)
}

func (s *databaseSession) Inspect(ctx context.Context, name string) (*DatabaseState, error) {
	bounded, cancel := context.WithTimeout(ctx, s.config.StatementTimeout)
	defer cancel()
	state, err := inspectDatabase(bounded, s.connection, name)
	if err != nil {
		return nil, databaseFailure(err)
	}
	return state, nil
}

func inspectDatabase(ctx context.Context, q roleQuerier, name string) (*DatabaseState, error) {
	if name == "" || strings.ContainsRune(name, 0) {
		return nil, &Failure{InvalidConfiguration, "SQL database name is empty or contains an invalid byte"}
	}
	state := &DatabaseState{Name: name}
	if err := q.QueryRow(ctx, `SELECT current_user, pg_catalog.current_database(),
		pg_catalog.current_setting('server_version_num')::pg_catalog.int4,
		pg_catalog.current_setting('max_identifier_length')::pg_catalog.int4`).
		Scan(&state.AdminUser, &state.AdminDatabase, &state.ServerVersion, &state.MaxIdentifierLength); err != nil {
		return nil, err
	}
	if err := validateDatabaseIdentifier(name, state.MaxIdentifierLength); err != nil {
		return nil, err
	}
	err := q.QueryRow(ctx, `SELECT pg_catalog.pg_get_userbyid(d.datdba),d.datistemplate,d.datallowconn,
		d.datconnlimit,t.spcname FROM pg_catalog.pg_database d
		JOIN pg_catalog.pg_tablespace t ON t.oid OPERATOR(pg_catalog.=) d.dattablespace
		WHERE d.datname OPERATOR(pg_catalog.=) $1`, name).
		Scan(&state.Owner, &state.IsTemplate, &state.AllowConnections, &state.ConnectionLimit, &state.Tablespace)
	if errors.Is(err, pgx.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return nil, err
	}
	state.Exists = true
	return state, nil
}

// IsProtectedDatabase protects standard databases and the current administrative database.
func IsProtectedDatabase(name, administrativeDatabase string) bool {
	return name == defaultAdministrativeDatabase || name == "template0" || name == "template1" || name == administrativeDatabase
}

// IsProtectedSchema protects catalog and reserved system schemas from mutation.
func IsProtectedSchema(name string) bool {
	return name == "pg_catalog" || name == "information_schema" || strings.HasPrefix(name, "pg_")
}

func validateDatabaseIdentifier(name string, maximum int) error {
	if name == "" || strings.ContainsRune(name, 0) || len(name) > maximum {
		return &Failure{InvalidConfiguration, "SQL identifier is empty, invalid, or exceeds the server byte limit"}
	}
	return nil
}

func databaseFailure(err error) error {
	if err == nil {
		return nil
	}
	if failure, ok := errors.AsType[*Failure](err); ok {
		return failure
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if server, ok := errors.AsType[*pgconn.PgError](err); ok {
		switch server.Code {
		case "42501":
			return &Failure{InsufficientPrivileges, "The administrative connection lacks required database or schema privileges"}
		case "2BP01", "55006":
			return &Failure{DeletionBlocked, "SQL dependencies or connected sessions block the database or schema operation"}
		case "3D000":
			return &Failure{DatabaseNotFound, databaseNotFoundMessage}
		case "42704":
			return &Failure{DependenciesNotReady, "A required SQL role or tablespace does not exist"}
		case "25006":
			return &Failure{NotWritable, readOnlyMessage}
		case "0A000":
			return &Failure{UnsupportedFeature, "The PostgreSQL server does not support the requested database configuration"}
		case "22023", "42601", "42703", "42602", "42P17", "42809", "22021":
			return &Failure{InvalidConfiguration, "The requested SQL database or schema configuration is invalid"}
		}
	}
	return &Failure{ConnectionUnavailable, "Unable to complete PostgreSQL database or schema reconciliation"}
}

func (s *databaseSession) preflightDatabaseOwner(ctx context.Context, owner string) error {
	return s.ensureServiceRole(ctx, owner, RoleCapabilityProvision)
}

// selectDatabaseRole keeps inherited authority where possible. Otherwise the
// server checks SET authority itself, including PG16+ membership options.
func selectDatabaseRole(ctx context.Context, q roleQuerier, owner string, local bool) error {
	var inherited bool
	if err := q.QueryRow(ctx, "SELECT pg_catalog.pg_has_role(current_user,$1,'USAGE')", owner).Scan(&inherited); err != nil {
		return err
	}
	if inherited {
		return nil
	}
	command := "SET ROLE "
	if local {
		command = "SET LOCAL ROLE "
	}
	_, err := q.Exec(ctx, command+pgx.Identifier{owner}.Sanitize())
	return err
}

func (s *databaseSession) nontransactionalOwnerSQL(ctx context.Context, owner, statement string) error {
	// Disposable administrative connections cannot return an active SET ROLE to
	// another operation; CREATE, DROP and tablespace moves stay outside transactions.
	connection, disposal, err := openDatabaseConnection(ctx, &s.config)
	if err != nil {
		return err
	}
	defer closeDatabaseConnection(disposal)
	if err = selectDatabaseRole(ctx, connection, owner, false); err != nil {
		return err
	}
	_, err = connection.Exec(ctx, statement)
	return err
}

func (s *databaseSession) Drop(ctx context.Context, name string) error {
	bounded, cancel := context.WithTimeout(ctx, s.config.StatementTimeout)
	defer cancel()
	state, err := inspectDatabase(bounded, s.connection, name)
	if err != nil {
		return databaseFailure(err)
	}
	if err = validateDatabaseIdentifier(name, state.MaxIdentifierLength); err != nil {
		return err
	}
	if IsProtectedDatabase(name, state.AdminDatabase) {
		return &Failure{ProtectedDatabase, "The SQL database is protected from management"}
	}
	if !state.Exists {
		return nil
	}
	// PostgreSQL may spend several seconds waiting for other backends before
	// reporting its ordinary DROP refusal. Detect visible active sessions
	// first so a shorter statement deadline still produces a useful blocker.
	var connected bool
	if err = s.connection.QueryRow(bounded, `SELECT EXISTS (
		SELECT 1 FROM pg_catalog.pg_stat_activity WHERE datname OPERATOR(pg_catalog.=) $1)`, name).Scan(&connected); err != nil {
		return databaseFailure(err)
	}
	if connected {
		return &Failure{DeletionBlocked, "Connected sessions prevent SQL database deletion"}
	}
	if err = s.ensureOwnerExecution(bounded, state.Owner, RoleCapabilityCleanup); err != nil {
		return databaseFailure(err)
	}
	if err = s.nontransactionalOwnerSQL(bounded, state.Owner, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		if bounded.Err() != nil {
			return bounded.Err()
		}
		if server, ok := errors.AsType[*pgconn.PgError](err); ok && server.Code == "57014" {
			return &Failure{DeletionBlocked, "Connected sessions or database locks block deletion within the statement limit"}
		}
		return databaseFailure(err)
	}
	state, err = inspectDatabase(bounded, s.connection, name)
	if err != nil {
		return databaseFailure(err)
	}
	if state.Exists {
		return &Failure{DependenciesNotReady, "Database absence could not be confirmed"}
	}
	return nil
}
