package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

// This real pgx transport returns a valid row description, optionally its first
// row, then withholds completion. SQL statement_timeout cannot terminate such
// a lost response, so the client context must remain alive through Scan/drain.
const extensionDeadlineName = "deadline_extension"

type extensionDeadlinePeer struct {
	mu          sync.Mutex
	sockets     []net.Conn
	workers     sync.WaitGroup
	stalled     chan struct{}
	once        sync.Once
	firstRow    bool
	commitStall atomic.Bool
	active      atomic.Int32
}

func extensionDeadlineRows(backend *pgproto3.Backend, types []uint32, values []string, complete bool,
	status byte) error {
	fields := make([]pgproto3.FieldDescription, len(types))
	for i, oid := range types {
		fields[i] = pgproto3.FieldDescription{Name: []byte(fmt.Sprintf("field_%d", i)), DataTypeOID: oid, DataTypeSize: -1}
	}
	backend.Send(&pgproto3.RowDescription{Fields: fields})
	if values != nil {
		row := make([][]byte, len(values))
		for i, value := range values {
			row[i] = []byte(value)
		}
		backend.Send(&pgproto3.DataRow{Values: row})
	}
	if complete {
		backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: status})
	}
	return backend.Flush()
}

func (p *extensionDeadlinePeer) serve(socket net.Conn) {
	p.active.Add(1)
	defer p.active.Add(-1)
	defer func() { _ = socket.Close() }()
	_ = socket.SetDeadline(time.Now().Add(3 * time.Second))
	backend := pgproto3.NewBackend(socket, socket)
	startup, err := backend.ReceiveStartupMessage()
	if err != nil {
		return
	}
	if _, canceled := startup.(*pgproto3.CancelRequest); canceled {
		return
	}
	backend.Send(&pgproto3.AuthenticationOk{})
	backend.Send(&pgproto3.ParameterStatus{Name: "server_version", Value: "18.0"})
	backend.Send(&pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"})
	backend.Send(&pgproto3.ParameterStatus{Name: "standard_conforming_strings", Value: "on"})
	backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if err = backend.Flush(); err != nil {
		return
	}
	status := byte('I')
	for {
		message, err := backend.Receive()
		if err != nil {
			return
		}
		query, ok := message.(*pgproto3.Query)
		if !ok {
			return
		}
		switch {
		case strings.EqualFold(strings.TrimSpace(query.String), "BEGIN"):
			status = 'T'
			backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("BEGIN")})
			backend.Send(&pgproto3.ReadyForQuery{TxStatus: status})
			err = backend.Flush()
		case strings.EqualFold(strings.TrimSpace(query.String), "COMMIT") && p.commitStall.Load():
			p.once.Do(func() { close(p.stalled) })
			_, _ = io.Copy(io.Discard, socket)
			return
		case strings.Contains(query.String, "FROM pg_catalog.pg_extension"):
			var values []string
			if p.firstRow || p.commitStall.Load() {
				values = []string{"omega", "public", aclDeadlineAdmin, "t"}
			}
			if extensionDeadlineRows(backend, []uint32{25, 25, 25, 16}, values, p.commitStall.Load(), status) != nil {
				return
			}
			if p.commitStall.Load() {
				continue
			}
			p.once.Do(func() { close(p.stalled) })
			_, _ = io.Copy(io.Discard, socket)
			return
		case strings.Contains(query.String, "current_setting('server_version_num')"):
			err = extensionDeadlineRows(backend, []uint32{25, 25, 23, 23}, []string{aclDeadlineAdmin, "postgres",
				"180000", "63"}, true, status)
		case strings.Contains(query.String, "d.datistemplate"):
			err = extensionDeadlineRows(backend, []uint32{25, 16, 16, 23, 25}, []string{aclDeadlineAdmin, "f", "t",
				"-1", "pg_default"}, true, status)
		default:
			return
		}
		if err != nil {
			return
		}
	}
}

func extensionDeadlineSession(t *testing.T, firstRow bool, timeout time.Duration) (DatabaseExtensionSession,
	*extensionDeadlinePeer) {
	t.Helper()
	p := &extensionDeadlinePeer{stalled: make(chan struct{}), firstRow: firstRow}
	config, err := pgx.ParseConfig("host=127.0.0.1 user=deadline_admin dbname=postgres sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	config.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	config.DialFunc = func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		p.mu.Lock()
		p.sockets = append(p.sockets, client, server)
		p.workers.Go(func() { p.serve(server) })
		p.mu.Unlock()
		return client, nil
	}
	t.Cleanup(func() {
		p.mu.Lock()
		for _, socket := range p.sockets {
			_ = socket.Close()
		}
		p.mu.Unlock()
		done := make(chan struct{})
		go func() { p.workers.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("extension protocol peers did not stop")
		}
	})
	session, err := NewDatabaseFactory().Open(t.Context(), &Config{Connection: config,
		ConnectTimeout: time.Second, StatementTimeout: timeout}, DatabaseSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = session.Close(ctx)
	})
	return session.(DatabaseExtensionSession), p
}

func TestExtensionCatalogDeadlineCoversScanAndCompletion(t *testing.T) {
	for _, firstRow := range []bool{false, true} {
		name := "scan_wait"
		if firstRow {
			name = "completion_wait"
		}
		t.Run(name, func(t *testing.T) {
			session, peer := extensionDeadlineSession(t, firstRow, 40*time.Millisecond)
			ctx, cancel := context.WithTimeout(t.Context(), 700*time.Millisecond)
			defer cancel()
			started := time.Now()
			states, err := session.InspectExtensions(ctx, aclDeadlineDatabase,
				[]epco.ExtensionSpec{{Name: extensionDeadlineName}})
			select {
			case <-peer.stalled:
			default:
				t.Fatalf("fixture did not reach catalog stall: %v", err)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("stalled catalog must preserve deadline: %v", err)
			}
			if states != nil {
				t.Errorf("incomplete catalog must not confirm states: %v", states)
			}
			if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
				t.Errorf("configured timeout ended too late: %s", elapsed)
			}
		})
	}
}

func TestExtensionCatalogPreservesInFlightCancellation(t *testing.T) {
	session, peer := extensionDeadlineSession(t, true, time.Second)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := session.InspectExtensions(ctx, aclDeadlineDatabase, []epco.ExtensionSpec{{Name: extensionDeadlineName}})
		finished <- err
	}()
	select {
	case <-peer.stalled:
	case <-time.After(time.Second):
		t.Fatal("fixture did not reach catalog stall")
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("catalog must preserve parent cancellation: %v", err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("cancellation did not stop catalog consumption")
	}
}

func TestExtensionCommitResponseDeadlineAndCleanup(t *testing.T) {
	session, peer := extensionDeadlineSession(t, true, 40*time.Millisecond)
	peer.commitStall.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), 700*time.Millisecond)
	defer cancel()
	started := time.Now()
	result, err := session.ReconcileExtensions(ctx, aclDeadlineDatabase,
		[]epco.ExtensionSpec{{Name: extensionDeadlineName, Version: "omega", Schema: "public"}})
	select {
	case <-peer.stalled:
	default:
		t.Fatalf("fixture never reached stalled commit response: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("unknown commit result must preserve deadline: %v", err)
	}
	if result != nil && len(result.States) > 0 {
		t.Errorf("unknown commit must not report confirmed outcomes: %v", result.States)
	}
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Errorf("commit deadline/cleanup exceeded bound: %s", elapsed)
	}
	until := time.NewTimer(300 * time.Millisecond)
	defer until.Stop()
	for peer.active.Load() != 1 {
		select {
		case <-until.C:
			t.Fatalf("target socket remained open after commit timeout: %d peers", peer.active.Load())
		case <-time.After(time.Millisecond):
		}
	}
}
