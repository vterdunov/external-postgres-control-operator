package postgres

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type probeRow struct {
	recovery, readonly bool
	err                error
}

func (r probeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*bool) = r.recovery
	*dest[1].(*bool) = r.readonly
	return nil
}

type fakeProbeConnection struct {
	query func(context.Context, string) pgx.Row
	close func(context.Context) error
}

func (c *fakeProbeConnection) QueryRow(ctx context.Context, sql string, _ ...any) pgx.Row {
	return c.query(ctx, sql)
}
func (c *fakeProbeConnection) Close(ctx context.Context) error { return c.close(ctx) }
func checkConfig() *Config {
	parsed, err := pgx.ParseConfig("host=127.0.0.1 port=1 user=unused password=unused dbname=postgres sslmode=disable")
	if err != nil {
		panic(err)
	}
	return &Config{Connection: parsed, ConnectTimeout: 1500 * time.Microsecond, StatementTimeout: 2500 * time.Microsecond}
}
func assertDeadline(t *testing.T, ctx context.Context, began time.Time, duration time.Duration) {
	t.Helper()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("missing context deadline")
	}
	actual := deadline.Sub(began)
	if actual < duration-time.Millisecond || actual > duration+100*time.Millisecond {
		t.Errorf("deadline offset=%s want exact duration=%s", actual, duration)
	}
}
func TestCheckUsesExactDeadlinesAndFreshConnections(t *testing.T) {
	connectCalls, queryCalls, closeCalls := 0, 0, 0
	checker := &connectionChecker{connect: func(ctx context.Context, _ *pgx.ConnConfig) (probeConnection, error) {
		connectCalls++
		assertDeadline(t, ctx, time.Now().Add(-time.Microsecond), 1500*time.Microsecond)
		return &fakeProbeConnection{query: func(ctx context.Context, sql string) pgx.Row {
			queryCalls++
			assertDeadline(t, ctx, time.Now().Add(-time.Microsecond), 2500*time.Microsecond)
			if !strings.HasPrefix(sql, "SELECT ") || !strings.Contains(sql, "pg_is_in_recovery") || !strings.Contains(sql, "transaction_read_only") {
				t.Error("probe must only read writable-primary state")
			}
			return probeRow{}
		}, close: func(ctx context.Context) error {
			closeCalls++
			if ctx.Err() != nil {
				t.Error("cleanup context already cancelled")
			}
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 5*time.Second {
				t.Error("cleanup deadline missing or unbounded")
			}
			return nil
		}}, nil
	}}
	for range 2 {
		if err := checker.Check(context.Background(), checkConfig()); err != nil {
			t.Errorf("check failed: %v", err)
		}
	}
	if connectCalls != 2 || queryCalls != 2 || closeCalls != 2 {
		t.Errorf("connection lifecycle connect=%d query=%d close=%d want=2 each", connectCalls, queryCalls, closeCalls)
	}
}
func TestCheckFailureAndReadonlyAlwaysClose(t *testing.T) {
	for _, tc := range []struct {
		name   string
		row    probeRow
		reason string
	}{{"query failure", probeRow{err: errors.New("raw-password")}, ConnectionUnavailable}, {"recovery", probeRow{recovery: true}, NotWritable}, {"readonly", probeRow{readonly: true}, NotWritable}} {
		t.Run(tc.name, func(t *testing.T) {
			closed := 0
			checker := &connectionChecker{connect: func(context.Context, *pgx.ConnConfig) (probeConnection, error) {
				return &fakeProbeConnection{query: func(context.Context, string) pgx.Row { return tc.row }, close: func(context.Context) error { closed++; return nil }}, nil
			}}
			err := checker.Check(context.Background(), checkConfig())
			var failure *Failure
			if !errors.As(err, &failure) || failure.Reason != tc.reason {
				t.Errorf("failure=%v want=%s", err, tc.reason)
			}
			if strings.Contains(err.Error(), "raw-password") {
				t.Error("driver detail exposed")
			}
			if closed != 1 {
				t.Errorf("close calls=%d want=1", closed)
			}
		})
	}
}
func TestCheckCleanupSurvivesParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	closed := false
	checker := &connectionChecker{connect: func(context.Context, *pgx.ConnConfig) (probeConnection, error) {
		return &fakeProbeConnection{query: func(ctx context.Context, _ string) pgx.Row { cancel(); <-ctx.Done(); return probeRow{err: ctx.Err()} }, close: func(cleanup context.Context) error {
			closed = true
			if cleanup.Err() != nil {
				t.Error("cancelled parent poisoned cleanup")
			}
			if _, ok := cleanup.Deadline(); !ok {
				t.Error("cleanup must have independent bound")
			}
			return nil
		}}, nil
	}}
	err := checker.Check(ctx, checkConfig())
	if err == nil {
		t.Error("cancelled query reported success")
	}
	if !closed {
		t.Error("cancelled query leaked connection")
	}
}
func TestConnectAndStatementContextsExpireExactly(t *testing.T) {
	for _, phase := range []string{"connect", "statement"} {
		t.Run(phase, func(t *testing.T) {
			config := checkConfig()
			config.ConnectTimeout = 15 * time.Millisecond
			config.StatementTimeout = 25 * time.Millisecond
			calls := 0
			checker := &connectionChecker{connect: func(ctx context.Context, _ *pgx.ConnConfig) (probeConnection, error) {
				calls++
				if phase == "connect" {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return &fakeProbeConnection{query: func(ctx context.Context, _ string) pgx.Row { <-ctx.Done(); return probeRow{err: ctx.Err()} }, close: func(context.Context) error { return nil }}, nil
			}}
			began := time.Now()
			err := checker.Check(context.Background(), config)
			elapsed := time.Since(began)
			expected := config.ConnectTimeout
			if phase == "statement" {
				expected = config.StatementTimeout
			}
			if err == nil || calls != 1 {
				t.Error("deadline failure missing or connector not called")
			}
			if elapsed < expected || elapsed > 500*time.Millisecond {
				t.Errorf("elapsed=%s want deadline near=%s", elapsed, expected)
			}
		})
	}
}

func TestPGXCleanupDeadlinePhysicallyDisposesSocket(t *testing.T) {
	connection, peer := net.Pipe()
	defer func() { _ = connection.Close() }()
	defer func() { _ = peer.Close() }()
	wrapped := &pgxProbeConnection{probeConnection: &fakeProbeConnection{close: func(context.Context) error { return nil }}, cleanupDone: make(chan struct{}), socket: connection}
	if err := peer.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	began := time.Now()
	err := wrapped.Close(ctx)
	if err == nil {
		t.Error("unfinished async cleanup must report deadline")
	}
	if elapsed := time.Since(began); elapsed < 10*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Errorf("cleanup did not honor deadline: %s", elapsed)
	}
	buffer := make([]byte, 1)
	_, err = peer.Read(buffer)
	if !errors.Is(err, io.EOF) {
		t.Errorf("physical socket remains open after cleanup deadline: %v", err)
	}
}
func TestPGXCompletedCleanupReturnsWithoutForcingSocket(t *testing.T) {
	connection, peer := net.Pipe()
	defer func() { _ = connection.Close() }()
	defer func() { _ = peer.Close() }()
	done := make(chan struct{})
	close(done)
	wrapped := &pgxProbeConnection{probeConnection: &fakeProbeConnection{close: func(context.Context) error { return nil }}, cleanupDone: done, socket: connection}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	began := time.Now()
	if err := wrapped.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if time.Since(began) > 100*time.Millisecond {
		t.Error("completed cleanup blocked")
	}
	if err := peer.SetReadDeadline(time.Now().Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 1)
	_, err := peer.Read(buffer)
	if errors.Is(err, io.EOF) {
		t.Error("completed pgx cleanup should not force-close socket again")
	}
}
