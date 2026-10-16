package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

const (
	aclDeadlineTimeout      = 40 * time.Millisecond
	aclDeadlineFallback     = 700 * time.Millisecond
	aclDeadlineMaximum      = 300 * time.Millisecond
	aclDeadlineDatabase     = "deadline_database"
	aclDeadlineAdmin        = "deadline_admin"
	aclDeadlineCatalogQuery = "rolname OPERATOR(pg_catalog.=) current_user"
	aclDeadlineACLQuery     = "aclexplode"
	aclDeadlineConnectQuery = "has_database_privilege"
)

// A protocol peer can stop replying after successful connection and inspection.
// Unlike a PostgreSQL statement timeout, the real pgx client deadline must end
// the resulting network read. No external server or sockets are required.
type aclDeadlinePeer struct {
	stalled    chan struct{}
	done       chan struct{}
	dials      atomic.Int32
	mutations  atomic.Int32
	queryCount atomic.Int32
}

func aclDeadlineSend(backend *pgproto3.Backend, types []uint32, values []string, complete bool) error {
	fields := make([]pgproto3.FieldDescription, len(types))
	for i, oid := range types {
		fields[i] = pgproto3.FieldDescription{Name: []byte(fmt.Sprintf("column_%d", i)), DataTypeOID: oid, DataTypeSize: -1}
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
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	}
	return backend.Flush()
}

func aclDeadlineServe(peer net.Conn, state *aclDeadlinePeer, stall string, delayed bool) {
	defer close(state.done)
	defer func() { _ = peer.Close() }()
	if err := peer.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return
	}
	backend := pgproto3.NewBackend(peer, peer)
	if _, err := backend.ReceiveStartupMessage(); err != nil {
		return
	}
	backend.Send(&pgproto3.AuthenticationOk{})
	backend.Send(&pgproto3.ParameterStatus{Name: "server_version", Value: "18.0"})
	backend.Send(&pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"})
	backend.Send(&pgproto3.ParameterStatus{Name: "standard_conforming_strings", Value: "on"})
	backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if err := backend.Flush(); err != nil {
		return
	}
	for {
		message, err := backend.Receive()
		if err != nil {
			return
		}
		query, ok := message.(*pgproto3.Query)
		if !ok {
			return
		}
		state.queryCount.Add(1)
		if !strings.HasPrefix(strings.TrimSpace(query.String), "SELECT") {
			state.mutations.Add(1)
			return
		}
		if strings.Contains(query.String, stall) {
			aclDeadlineStall(backend, peer, state, stall, delayed)
			return
		}
		if strings.Contains(query.String, "current_setting('server_version_num')") {
			err = aclDeadlineSend(backend, []uint32{25, 25, 23, 23}, []string{aclDeadlineAdmin, "postgres", "180000", "63"}, true)
		} else if strings.Contains(query.String, "d.datistemplate") {
			err = aclDeadlineSend(backend, []uint32{25, 16, 16, 23, 25}, []string{aclDeadlineAdmin, "f", "t", "-1", "pg_default"}, true)
		} else {
			return // Unexpected setup queries must fail the test rather than pass it.
		}
		if err != nil {
			return
		}
	}
}

func aclDeadlineStall(backend *pgproto3.Backend, peer net.Conn, state *aclDeadlinePeer, stall string, delayed bool) {
	types := []uint32{16, 16}
	var first []string
	switch stall {
	case aclDeadlineCatalogQuery:
		types = []uint32{26, 25, 16, 16, 16, 16}
		first = []string{"100", aclDeadlineAdmin, "f", "t", "t", "t"}
	case aclDeadlineACLQuery:
		types = []uint32{26, 26, 25, 16}
		first = []string{"200", "100", "SELECT", "f"}
	case "has_table_privilege":
		types = []uint32{16}
	}
	// Stream cases provide a real first row but no completion. Scan-only cases
	// provide descriptions before stalling, so their Scan must keep the deadline.
	if err := aclDeadlineSend(backend, types, first, false); err != nil {
		return
	}
	close(state.stalled)
	if delayed {
		timer := time.NewTimer(10 * time.Millisecond)
		defer timer.Stop()
		<-timer.C
		backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		if err := backend.Flush(); err != nil {
			return
		}
	}
	// Drain client shutdown messages until its socket closes. Test cleanup closes
	// both ends, including on a failed deadline assertion, and waits for this task.
	_, _ = io.Copy(io.Discard, peer)
}

func aclDeadlineSession(t *testing.T, stall string, delayed bool) (*databaseSession, *aclDeadlinePeer) {
	t.Helper()
	clientSocket, serverSocket := net.Pipe()
	state := &aclDeadlinePeer{stalled: make(chan struct{}), done: make(chan struct{})}
	go aclDeadlineServe(serverSocket, state, stall, delayed)
	config, err := pgx.ParseConfig("host=127.0.0.1 user=deadline_admin dbname=postgres sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	config.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	config.DialFunc = func(context.Context, string, string) (net.Conn, error) {
		if state.dials.Add(1) != 1 {
			return nil, errors.New("unexpected connection after stalled ACL query")
		}
		return clientSocket, nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	connection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		_ = clientSocket.Close()
		_ = serverSocket.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = clientSocket.Close()
		_ = serverSocket.Close()
		cleanup, cleanupCancel := context.WithTimeout(context.Background(), time.Second)
		defer cleanupCancel()
		_ = connection.Close(cleanup)
		select {
		case <-state.done:
		case <-cleanup.Done():
			t.Error("protocol peer did not stop during bounded cleanup")
		}
	})
	return &databaseSession{connection: connection, config: Config{Connection: config, StatementTimeout: aclDeadlineTimeout,
		ConnectTimeout: time.Second}}, state
}

func aclDeadlineEngine(session *databaseSession) *aclEngine {
	return &aclEngine{session: session, target: session.connection, desired: ACLDesired{Name: aclDeadlineDatabase, Observe: true},
		catalog: &aclCatalog{rolesByOID: map[uint32]aclRole{}, roles: map[string]aclRole{}}}
}

func aclDeadlineBinding() epco.ACLBinding {
	return epco.ACLBinding{Scope: aclClassTable, Class: aclClassTable, Privilege: aclSelect,
		DatabaseOID: 10, ObjectOID: 11, GranteeOID: 200, GrantorOID: 100, Grantee: "deadline_reader"}
}

func TestACLReadClientDeadlines(t *testing.T) {
	for _, test := range []struct {
		name         string
		stall        string
		returnsError bool
		run          func(context.Context, *databaseSession) error
	}{
		{"catalog_stream", aclDeadlineCatalogQuery, true, func(ctx context.Context, session *databaseSession) error {
			_, dispose, err := session.newACLEngine(ctx, ACLDesired{Name: aclDeadlineDatabase}, false)
			if dispose != nil {
				dispose()
			}
			return err
		}},
		{"direct_acl_stream", aclDeadlineACLQuery, true, func(ctx context.Context, session *databaseSession) error {
			return aclDeadlineEngine(session).reconcileBinding(ctx, aclDeadlineBinding(), false)
		}},
		{"effective_scan", "has_table_privilege", false, func(ctx context.Context, session *databaseSession) error {
			if result := aclDeadlineEngine(session).effective(ctx, aclDeadlineBinding()); result != nil {
				t.Error("stalled effective access must remain unknown")
			}
			return nil
		}},
		{"default_acl_stream", aclDeadlineACLQuery, false, func(ctx context.Context, session *databaseSession) error {
			binding := aclDeadlineBinding()
			binding.Scope = aclScopeDefault
			binding.CreatorOID = 100
			if result := aclDeadlineEngine(session).effectiveDefault(ctx, binding); result != nil {
				t.Error("incomplete default ACL stream must remain unknown")
			}
			return nil
		}},
		{"connect_scan", aclDeadlineConnectQuery, true, func(ctx context.Context, session *databaseSession) error {
			return aclDeadlineEngine(session).verifyConnect(ctx)
		}},
		{"restore_connect_scan", aclDeadlineConnectQuery, true, func(ctx context.Context, session *databaseSession) error {
			_, err := session.restoreACLAccess(ctx, ACLDesired{Name: aclDeadlineDatabase,
				Spec: epco.DatabaseSpec{Schemas: []epco.SchemaSpec{{Name: "app"}}}}, &DatabaseState{AllowConnections: true})
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, peer := aclDeadlineSession(t, test.stall, false)
			ctx, cancel := context.WithTimeout(t.Context(), aclDeadlineFallback)
			defer cancel()
			began := time.Now()
			err := test.run(ctx, session)
			elapsed := time.Since(began)
			select {
			case <-peer.stalled:
			default:
				t.Fatalf("fixture never reached the intended stalled read: %v", err)
			}
			if elapsed > aclDeadlineMaximum {
				t.Errorf("configured SQL timeout %s did not end stalled ACL read: elapsed=%s, outer fallback=%s",
					aclDeadlineTimeout, elapsed, aclDeadlineFallback)
			}
			if test.returnsError && !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("stalled ACL read must propagate its deadline: %v", err)
			}
			// pgx may dial a cancellation connection while disposing the stalled
			// socket. That is driver cleanup, not subsequent application SQL.
			if peer.mutations.Load() != 0 {
				t.Error("stalled read must not proceed to mutation")
			}
		})
	}
}

func TestACLReadDeadlineLivesThroughStreamCompletion(t *testing.T) {
	session, peer := aclDeadlineSession(t, aclDeadlineACLQuery, true)
	ctx, cancel := context.WithTimeout(t.Context(), aclDeadlineFallback)
	defer cancel()
	if err := aclDeadlineEngine(session).reconcileBinding(ctx, aclDeadlineBinding(), false); err != nil {
		t.Fatalf("valid delayed stream must be consumed through completion within its deadline: %v", err)
	}
	if peer.mutations.Load() != 0 || peer.queryCount.Load() != 1 {
		t.Error("observed complete stream must not mutate or repeat the read")
	}
}

func TestACLReadPreservesParentCancellation(t *testing.T) {
	session, peer := aclDeadlineSession(t, aclDeadlineACLQuery, false)
	session.config.StatementTimeout = time.Second
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- aclDeadlineEngine(session).reconcileBinding(ctx, aclDeadlineBinding(), false) }()
	select {
	case <-peer.stalled:
	case <-time.After(time.Second):
		t.Fatal("fixture never reached its stalled stream")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("parent cancellation was not propagated: %v", err)
		}
	case <-time.After(aclDeadlineMaximum):
		t.Fatal("parent cancellation did not end stalled stream")
	}
}
