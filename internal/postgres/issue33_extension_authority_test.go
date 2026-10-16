package postgres

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

const (
	issue33Admin                  = "extension_administrator"
	issue33Owner                  = "provider_extension_owner"
	issue33Name                   = "hstore"
	issue33Version                = "omega"
	issue33Schema                 = "public"
	issue33Destination            = "destination"
	issue33Drop                   = "drop"
	issue33MoveUpdate             = "move_and_update"
	issue33UpdateMove             = "update_then_move"
	issue33ServerVersionParameter = "server_version"
	issue33Encoding               = "UTF8"
	issue33StandardStrings        = "standard_conforming_strings"
	issue33EncodingParameter      = "client_encoding"
)

// This backend models a provider that authorizes extension SQL independently of
// ordinary role membership. It does not establish any real provider's policy.
// The real pgx client must recover an aborted transaction before retrying SQL.
type issue33ExtensionBackend struct {
	mu           sync.Mutex
	workers      sync.WaitGroup
	sockets      []net.Conn
	state        ExtensionState
	adminFailure string
	ownerAllowed bool
	failOnMove   bool
	fixedVersion bool
	rollbackFail bool
	dependent    string
	attempts     []string
	selections   int
	unexpected   []string
}

func newIssue33Session(t *testing.T, backend *issue33ExtensionBackend) *databaseSession {
	t.Helper()
	backend.state = ExtensionState{Name: issue33Name, Exists: true, Version: "seed", Schema: issue33Schema,
		Owner: issue33Owner, Relocatable: !backend.fixedVersion}
	config, err := pgx.ParseConfig("host=127.0.0.1 user=" + issue33Admin + " dbname=postgres sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	config.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	config.DialFunc = func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		backend.mu.Lock()
		backend.sockets = append(backend.sockets, client, server)
		backend.workers.Go(func() { backend.serve(server) })
		backend.mu.Unlock()
		return client, nil
	}
	t.Cleanup(func() {
		backend.mu.Lock()
		for _, socket := range backend.sockets {
			_ = socket.Close()
		}
		backend.mu.Unlock()
		backend.workers.Wait()
		if len(backend.unexpected) != 0 {
			t.Errorf("unexpected backend SQL: %v", backend.unexpected)
		}
	})
	return &databaseSession{config: Config{Connection: config, ConnectTimeout: time.Second, StatementTimeout: time.Second}}
}

func (b *issue33ExtensionBackend) serve(socket net.Conn) {
	defer func() { _ = socket.Close() }()
	_ = socket.SetDeadline(time.Now().Add(5 * time.Second))
	backend := pgproto3.NewBackend(socket, socket)
	if _, err := backend.ReceiveStartupMessage(); err != nil {
		return
	}
	backend.Send(&pgproto3.AuthenticationOk{})
	backend.Send(&pgproto3.ParameterStatus{Name: issue33ServerVersionParameter, Value: "16.11"})
	backend.Send(&pgproto3.ParameterStatus{Name: issue33EncodingParameter, Value: issue33Encoding})
	backend.Send(&pgproto3.ParameterStatus{Name: issue33StandardStrings, Value: "on"})
	backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if err := backend.Flush(); err != nil {
		return
	}
	status, role := byte('I'), issue33Admin
	var original, checkpoint ExtensionState
	for {
		message, err := backend.Receive()
		if err != nil {
			return
		}
		query, ok := message.(*pgproto3.Query)
		if !ok {
			return
		}
		sql := strings.TrimSpace(query.String)
		upper := strings.ToUpper(sql)
		b.mu.Lock()
		code := ""
		var types []uint32
		var values []string
		switch {
		case upper == "BEGIN":
			status, role, original = 'T', issue33Admin, b.state
		case strings.HasPrefix(upper, "ROLLBACK TO"):
			if b.rollbackFail {
				code = "08006"
			} else {
				status, role, b.state = 'T', issue33Admin, checkpoint
			}
		case upper == "ROLLBACK":
			status, role, b.state = 'I', issue33Admin, original
		case status == 'E':
			code = "25P02"
		case strings.HasPrefix(upper, "SAVEPOINT "):
			checkpoint = b.state
		case strings.HasPrefix(upper, "RELEASE "):
		case upper == "COMMIT":
			status, role = 'I', issue33Admin
		case strings.Contains(sql, "FROM pg_catalog.pg_extension e"):
			types = []uint32{25, 25, 25, 16}
			if b.state.Exists {
				values = []string{b.state.Version, b.state.Schema, b.state.Owner, fmt.Sprint(b.state.Relocatable)}
			}
		case strings.Contains(sql, "pg_is_in_recovery()"):
			types, values = []uint32{16, 16}, []string{"f", "f"}
		case strings.Contains(sql, "pg_has_role"):
			b.selections++
			types, values = []uint32{16}, []string{"f"} // NOINHERIT, including when SET is authorized.
		case strings.HasPrefix(upper, "SET LOCAL ROLE "):
			b.selections++
			if b.ownerAllowed {
				role = issue33Owner
			} else {
				code = "42501"
			}
		case strings.Contains(sql, "FROM pg_catalog.pg_available_extension_versions"):
			types, values = []uint32{16, 25, 1009}, []string{"t", "", "{}"}
		case strings.Contains(sql, "pg_extension_update_paths"):
			types, values = []uint32{16}, []string{"t"}
		case strings.Contains(sql, "FROM pg_catalog.pg_namespace"):
			types, values = []uint32{25}, []string{issue33Admin}
		case strings.HasPrefix(upper, "DROP EXTENSION "), strings.HasPrefix(upper, "ALTER EXTENSION "):
			code = b.applyExtensionSQL(role, sql, upper)
		default:
			b.unexpected = append(b.unexpected, sql)
			code = "XX000"
		}
		if code != "" && status == 'T' {
			status = 'E'
		}
		b.mu.Unlock()
		if code != "" {
			backend.Send(&pgproto3.ErrorResponse{Severity: "ERROR", Code: code, Message: "backend refused extension operation"})
		} else if types != nil {
			if err = extensionDeadlineRows(backend, types, values, true, status); err != nil {
				return
			}
			continue
		} else {
			backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("OK")})
		}
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: status})
		if err = backend.Flush(); err != nil {
			return
		}
	}
}

func TestIssue33ExtensionUsesNativeAdministrativeAuthority(t *testing.T) {
	for _, operation := range []string{issue33Drop, issue33MoveUpdate, "no_op"} {
		t.Run(operation, func(t *testing.T) {
			backend := &issue33ExtensionBackend{}
			session := newIssue33Session(t, backend)
			desired := epco.ExtensionSpec{Name: issue33Name, Ensure: epco.EnsureAbsent}
			switch operation {
			case issue33MoveUpdate:
				desired = epco.ExtensionSpec{Name: issue33Name, Version: issue33Version, Schema: issue33Destination}
			case "no_op":
				desired = epco.ExtensionSpec{Name: issue33Name}
			}
			state, err := session.reconcileExtension(t.Context(), "application", desired, nil)
			if err != nil {
				t.Fatalf("backend-authorized %s must not require provider owner SET authority: %v", operation, err)
			}
			if !extensionMatches(state, desired) {
				t.Errorf("unconfirmed extension result: %#v", state)
			}
			if backend.selections != 0 {
				t.Errorf("native operation unnecessarily selected the catalog owner %d times", backend.selections)
			}
			if backend.state.Owner != issue33Owner {
				t.Errorf("extension ownership changed: %q", backend.state.Owner)
			}
		})
	}
}

func TestIssue33ExtensionNativeFailureDoesNotEscalate(t *testing.T) {
	for _, tc := range []struct{ name, code, reason string }{
		{"dependent_application_data", "2BP01", DeletionBlocked},
		{"unsupported_operation", "0A000", UnsupportedFeature},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &issue33ExtensionBackend{adminFailure: tc.code, dependent: "application data remains"}
			session := newIssue33Session(t, backend)
			before := backend.state
			_, err := session.reconcileExtension(t.Context(), "application",
				epco.ExtensionSpec{Name: issue33Name, Ensure: epco.EnsureAbsent}, nil)
			failure, ok := errors.AsType[*ExtensionFailure](err)
			if !ok || failure.Failure.Reason != tc.reason || failure.Name != issue33Name {
				t.Errorf("native %s must retain extension failure %s, got %v", tc.code, tc.reason, err)
			}
			if backend.selections != 0 || len(backend.attempts) != 1 {
				t.Errorf("non-privilege failure retried owner authority: selections=%d attempts=%v", backend.selections, backend.attempts)
			}
			if backend.state != before || backend.dependent != "application data remains" {
				t.Errorf("failed RESTRICT operation changed extension or dependent application data")
			}
		})
	}
}

func TestIssue33ExtensionPrivilegeFailureUsesExistingSetAuthority(t *testing.T) {
	for _, operation := range []string{issue33Drop, issue33MoveUpdate, issue33UpdateMove} {
		t.Run(operation, func(t *testing.T) {
			backend := &issue33ExtensionBackend{adminFailure: "42501", ownerAllowed: true}
			if operation == issue33UpdateMove {
				backend.fixedVersion, backend.failOnMove = true, true
			}
			session := newIssue33Session(t, backend)
			desired := epco.ExtensionSpec{Name: issue33Name, Ensure: epco.EnsureAbsent}
			if operation != issue33Drop {
				desired = epco.ExtensionSpec{Name: issue33Name, Version: issue33Version, Schema: issue33Destination}
			}
			state, err := session.reconcileExtension(t.Context(), "application", desired, nil)
			if err != nil {
				t.Fatalf("existing NOINHERIT SET authority must permit safe retry after native 42501: %v", err)
			}
			if !extensionMatches(state, desired) {
				t.Errorf("owner retry did not confirm desired state: %#v", state)
			}
			if len(backend.attempts) < 2 || !strings.HasPrefix(backend.attempts[0], issue33Admin+": ") ||
				!strings.HasPrefix(backend.attempts[len(backend.attempts)-1], issue33Owner+": ") {
				t.Errorf("owner fallback must follow actual native privilege failure: %v", backend.attempts)
			}
			switch operation {
			case issue33MoveUpdate:
				ownerMove := issue33Owner + ": ALTER EXTENSION \"hstore\" SET SCHEMA \"destination\""
				if !slices.Contains(backend.attempts, ownerMove) {
					t.Errorf("partial native relocation must be rolled back before owner replay: %v", backend.attempts)
				}
			case issue33UpdateMove:
				ownerUpdates := slices.ContainsFunc(backend.attempts, func(attempt string) bool {
					return strings.HasPrefix(attempt, issue33Owner+": ALTER EXTENSION ") && strings.Contains(attempt, " UPDATE TO ")
				})
				if !ownerUpdates {
					t.Errorf("partial native enabling update must be rolled back before owner replay: %v", backend.attempts)
				}
			}
		})
	}
}

func TestIssue33ExtensionDeniedNativeAndOwnerAuthorityRemainDenied(t *testing.T) {
	backend := &issue33ExtensionBackend{adminFailure: "42501"}
	session := newIssue33Session(t, backend)
	before := backend.state
	_, err := session.reconcileExtension(t.Context(), "application",
		epco.ExtensionSpec{Name: issue33Name, Ensure: epco.EnsureAbsent}, nil)
	failure, ok := errors.AsType[*ExtensionFailure](err)
	if !ok || failure.Failure.Reason != InsufficientPrivileges {
		t.Errorf("native and SET denial must remain InsufficientPrivileges: %v", err)
	}
	want := []string{fmt.Sprintf("%s: DROP EXTENSION %q RESTRICT", issue33Admin, issue33Name)}
	if !slices.Equal(backend.attempts, want) {
		t.Errorf("must ask backend before trying existing owner authority: got %v, want %v", backend.attempts, want)
	}
	if backend.state != before {
		t.Errorf("denied operation changed extension: %#v", backend.state)
	}
}

func TestIssue33ExtensionRollbackFailurePreventsOwnerFallback(t *testing.T) {
	backend := &issue33ExtensionBackend{adminFailure: "42501", ownerAllowed: true, rollbackFail: true}
	session := newIssue33Session(t, backend)
	before := backend.state
	_, err := session.reconcileExtension(t.Context(), "application",
		epco.ExtensionSpec{Name: issue33Name, Ensure: epco.EnsureAbsent}, nil)
	if err == nil {
		t.Fatal("failed rollback must not report extension success")
	}
	if backend.selections != 0 || len(backend.attempts) != 1 {
		t.Errorf("failed native rollback must stop before owner fallback: selections=%d attempts=%v", backend.selections, backend.attempts)
	}
	if backend.state != before {
		t.Errorf("failed rollback must not commit extension changes: %#v", backend.state)
	}
}

func (b *issue33ExtensionBackend) applyExtensionSQL(role, sql, upper string) string {
	b.attempts = append(b.attempts, role+": "+sql)
	if strings.HasPrefix(upper, "DROP EXTENSION ") && !strings.HasSuffix(upper, " RESTRICT") {
		b.unexpected = append(b.unexpected, sql)
		return "XX000"
	} else if role == issue33Admin && b.adminFailure != "" &&
		(strings.Contains(upper, " SET SCHEMA ") == b.failOnMove) {
		return b.adminFailure
	} else if strings.HasPrefix(upper, "DROP EXTENSION ") && b.dependent != "" {
		return "2BP01"
	} else if strings.Contains(upper, " SET SCHEMA ") {
		b.state.Schema = issue33Destination
	} else if strings.Contains(upper, " UPDATE TO ") {
		b.state.Version = issue33Version
		b.state.Relocatable = true
	} else {
		b.state.Exists = false
	}
	return ""
}
