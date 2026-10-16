package postgres

import (
	"context"
	"crypto/tls"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

type roleSession struct {
	connection *pgx.Conn
	disposal   *pgxProbeConnection
	timeout    time.Duration
}

type roleQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func (*roleFactory) Open(ctx context.Context, config *Config) (RoleSession, error) {
	if config == nil || config.Connection == nil || config.ConnectTimeout <= 0 || config.StatementTimeout <= 0 {
		return nil, &Failure{InvalidConfiguration, "Role session configuration is invalid"}
	}
	connectCtx, cancel := context.WithTimeout(ctx, config.ConnectTimeout)
	defer cancel()
	connection, err := pgx.ConnectConfig(connectCtx, config.Connection.Copy())
	if err != nil {
		return nil, roleFailure(classifyConnectError(ctx, err))
	}
	socket := connection.PgConn().Conn()
	if tlsSocket, ok := socket.(*tls.Conn); ok {
		socket = tlsSocket.NetConn()
	}
	return &roleSession{connection: connection, timeout: config.StatementTimeout,
		disposal: &pgxProbeConnection{probeConnection: connection, cleanupDone: connection.PgConn().CleanupDone(), socket: socket}}, nil
}

func (s *roleSession) Close(ctx context.Context) error {
	cleanupCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return s.disposal.Close(cleanupCtx)
}

func (s *roleSession) Inspect(ctx context.Context, name string) (*RoleState, error) {
	bounded, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	state, err := inspectRole(bounded, s.connection, name)
	if err != nil {
		return nil, roleFailure(err)
	}
	return state, nil
}

func inspectRole(ctx context.Context, q roleQuerier, name string) (*RoleState, error) {
	state := &RoleState{Name: name, Parameters: make(map[string]string)}
	if err := q.QueryRow(ctx, "SELECT current_user, pg_catalog.current_setting('server_version_num')::pg_catalog.int4, pg_catalog.current_setting('max_identifier_length')::pg_catalog.int4").
		Scan(&state.AdminUser, &state.ServerVersion, &state.MaxIdentifierLength); err != nil {
		return nil, err
	}
	if err := q.QueryRow(ctx, `SELECT bootstrap.rolname, CASE WHEN admin.rolsuper THEN bootstrap.rolname ELSE admin.rolname END
		FROM pg_catalog.pg_roles admin CROSS JOIN pg_catalog.pg_roles bootstrap
		WHERE admin.rolname OPERATOR(pg_catalog.=) current_user AND bootstrap.oid OPERATOR(pg_catalog.=) 10`).Scan(&state.BootstrapUser, &state.MutationGrantor); err != nil {
		return nil, err
	}
	var expiry pgtype.Timestamptz
	err := q.QueryRow(ctx, `SELECT rolcanlogin,rolinherit,rolsuper,rolcreatedb,rolcreaterole,rolreplication,
		rolbypassrls,rolconnlimit,COALESCE(pg_catalog.shobj_description(oid,'pg_authid'),''),rolvaliduntil
		FROM pg_catalog.pg_roles WHERE rolname OPERATOR(pg_catalog.=) $1`, name).Scan(&state.Login, &state.Inherit, &state.Superuser,
		&state.CreateDB, &state.CreateRole, &state.Replication, &state.BypassRLS, &state.ConnectionLimit, &state.Comment, &expiry)
	if errors.Is(err, pgx.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return nil, err
	}
	state.Exists = true
	state.ValidUntilNegativeInfinity = expiry.Valid && expiry.InfinityModifier == pgtype.NegativeInfinity
	if expiry.Valid && expiry.InfinityModifier == pgtype.Finite {
		state.ValidUntil = new(expiry.Time)
	}
	rows, err := q.Query(ctx, `SELECT parent.rolname,member.rolname,grantor.rolname,b.admin_option
		FROM pg_catalog.pg_auth_members b JOIN pg_catalog.pg_roles parent ON parent.oid OPERATOR(pg_catalog.=) b.roleid
		JOIN pg_catalog.pg_roles member ON member.oid OPERATOR(pg_catalog.=) b.member JOIN pg_catalog.pg_roles grantor ON grantor.oid OPERATOR(pg_catalog.=) b.grantor
		WHERE parent.rolname OPERATOR(pg_catalog.=) $1 OR member.rolname OPERATOR(pg_catalog.=) $1 ORDER BY parent.rolname,member.rolname,grantor.rolname`, name)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var binding epco.RoleMembershipBinding
		if err = rows.Scan(&binding.Role, &binding.Member, &binding.Grantor, &binding.AdminOption); err != nil {
			rows.Close()
			return nil, err
		}
		binding.Administrative = binding.Role == name && binding.Member == state.AdminUser
		state.Memberships = append(state.Memberships, binding)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, `SELECT pg_catalog.unnest(s.setconfig) FROM pg_catalog.pg_db_role_setting s
		JOIN pg_catalog.pg_roles r ON r.oid OPERATOR(pg_catalog.=) s.setrole WHERE r.rolname OPERATOR(pg_catalog.=) $1 AND s.setdatabase OPERATOR(pg_catalog.=) 0`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var entry string
		if err = rows.Scan(&entry); err != nil {
			return nil, err
		}
		if key, value, ok := strings.Cut(entry, "="); ok {
			state.Parameters[key] = value
		}
	}
	return state, rows.Err()
}

// IsProtectedRole protects predefined service roles and the administrative account.
func IsProtectedRole(name, admin string) bool {
	return name == admin || name == defaultAdministrativeDatabase || strings.HasPrefix(name, "pg_") ||
		strings.HasPrefix(name, "rds_") || name == "rdsadmin" || name == "azure_pg_admin" ||
		name == "azure_superuser" || strings.HasPrefix(name, "cloudsql")
}

func validateRoleName(name string, maxLength int) error {
	if name == "" || strings.ContainsRune(name, 0) || len(name) > maxLength {
		return &Failure{InvalidConfiguration, "SQL role name is empty, invalid, or exceeds the server identifier limit"}
	}
	return nil
}

func roleFailure(err error) error {
	if failure, ok := errors.AsType[*Failure](err); ok {
		return failure
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if server, ok := errors.AsType[*pgconn.PgError](err); ok {
		switch server.Code {
		case "42501":
			return &Failure{InsufficientPrivileges, "The administrative connection lacks required SQL privileges"}
		case "42704":
			return &Failure{DependenciesNotReady, "A required SQL role does not exist"}
		case "2BP01":
			return &Failure{DeletionBlocked, "SQL dependencies prevent role or membership deletion"}
		case "25006":
			return &Failure{NotWritable, readOnlyMessage}
		case "22023", "42601", "22007", "22008", "42703":
			return &Failure{InvalidConfiguration, "The requested SQL role configuration is invalid"}
		}
	}
	return &Failure{ConnectionUnavailable, "Unable to complete PostgreSQL role reconciliation"}
}

func (s *roleSession) Drop(ctx context.Context, name string) error {
	bounded, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	state, err := inspectRole(bounded, s.connection, name)
	if err != nil {
		return roleFailure(err)
	}
	if err = validateRoleName(name, state.MaxIdentifierLength); err != nil {
		return err
	}
	if IsProtectedRole(name, state.AdminUser) {
		return &Failure{ProtectedRoleReason, "The SQL role is protected from management"}
	}
	_, err = s.connection.Exec(bounded, "DROP ROLE IF EXISTS "+pgx.Identifier{name}.Sanitize())
	return roleFailureOrNil(err)
}

func roleFailureOrNil(err error) error {
	if err == nil {
		return nil
	}
	return roleFailure(err)
}
