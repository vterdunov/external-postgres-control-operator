package postgres

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const connectionOpenSecret = "connection-open-secret-sentinel"

type connectionOpenResult struct {
	session any
	err     error
}

func connectionOpenConfig(t *testing.T, timeout time.Duration, started chan<- struct{}) *Config {
	t.Helper()
	connection, err := pgx.ParseConfig("host=127.0.0.1 port=5432 user=epco dbname=postgres password=" +
		connectionOpenSecret + " sslmode=disable connect_timeout=0")
	if err != nil {
		t.Fatal(err)
	}
	connection.Fallbacks = nil
	connection.DialFunc = func(ctx context.Context, _, _ string) (net.Conn, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &Config{Connection: connection, ConnectTimeout: timeout, StatementTimeout: time.Second}
}

func TestConnectionOpenTimeoutClassification(t *testing.T) {
	for _, factory := range []struct {
		name string
		open func(context.Context, *Config) (any, error)
	}{
		{"role", func(ctx context.Context, config *Config) (any, error) {
			return NewRoleFactory().Open(ctx, config)
		}},
		{"database", func(ctx context.Context, config *Config) (any, error) {
			return NewDatabaseFactory().Open(ctx, config)
		}},
	} {
		t.Run(factory.name+"/local connect timeout", func(t *testing.T) {
			started := make(chan struct{}, 1)
			config := connectionOpenConfig(t, 40*time.Millisecond, started)
			session, err := factory.open(t.Context(), config)
			if session != nil {
				t.Fatal("timed-out connect returned a session")
			}
			select {
			case <-started:
			default:
				t.Fatal("test did not reach the PostgreSQL dial boundary")
			}
			if t.Context().Err() != nil {
				t.Fatal("parent context ended before the local connect timeout")
			}
			failure, ok := errors.AsType[*Failure](err)
			if !ok || failure.Reason != ConnectionUnavailable {
				t.Fatalf("local connect timeout must publish ConnectionUnavailable, got %T: %v", err, err)
			}
			if strings.Contains(err.Error(), connectionOpenSecret) || strings.Contains(err.Error(), "127.0.0.1") {
				t.Fatal("connection failure exposed connection coordinates or credentials")
			}
		})

		t.Run(factory.name+"/parent cancellation", func(t *testing.T) {
			started := make(chan struct{}, 1)
			config := connectionOpenConfig(t, time.Second, started)
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan connectionOpenResult, 1)
			go func() {
				session, err := factory.open(parent, config)
				result <- connectionOpenResult{session, err}
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("PostgreSQL dial did not start")
			}
			cancel()
			select {
			case got := <-result:
				if got.session != nil || !errors.Is(got.err, context.Canceled) {
					t.Fatalf("parent cancellation must remain cancellation with no session, got %#v", got)
				}
			case <-time.After(time.Second):
				t.Fatal("PostgreSQL dial ignored parent cancellation")
			}
		})

		t.Run(factory.name+"/parent deadline", func(t *testing.T) {
			started := make(chan struct{}, 1)
			config := connectionOpenConfig(t, time.Second, started)
			parent, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
			defer cancel()
			session, err := factory.open(parent, config)
			if session != nil || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("parent deadline must remain cancellation with no session, got %T: %v", err, err)
			}
			select {
			case <-started:
			default:
				t.Fatal("test did not reach the PostgreSQL dial boundary")
			}
		})
	}
}

func TestConnectionFailureMappingPreserved(t *testing.T) {
	for _, factory := range []struct {
		name     string
		mapError func(error) error
	}{
		{"role", roleFailure},
		{"database", databaseFailure},
	} {
		for _, test := range []struct {
			name, sqlstate, want string
		}{
			{"authentication", "28P01", ConnectionUnavailable},
			{"insufficient privileges", "42501", InsufficientPrivileges},
		} {
			t.Run(factory.name+"/"+test.name, func(t *testing.T) {
				err := factory.mapError(&pgconn.PgError{Code: test.sqlstate, Message: connectionOpenSecret})
				failure, ok := errors.AsType[*Failure](err)
				if !ok || failure.Reason != test.want || strings.Contains(err.Error(), connectionOpenSecret) {
					t.Fatalf("unexpected sanitized SQL error mapping: %T: %v", err, err)
				}
			})
		}
	}
}
