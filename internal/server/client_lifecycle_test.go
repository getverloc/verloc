package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientRevokeFailureRetryIsDurable(t *testing.T) {
	e := newSecurityTestEnv(t)
	cookie, csrf := e.seedSession(t)
	key, view := createSecurityTestKey(t, e, cookie, csrf, clientKeyRequest{Name: "retry revoke"})
	r := authenticatedClientRequest(t, e.security, key)
	if _, err := e.store.db.Exec(`CREATE TRIGGER reject_revoke BEFORE UPDATE OF revoked_at ON client_keys BEGIN SELECT RAISE(ABORT,'forced revocation failure'); END`); err != nil {
		t.Fatal(err)
	}
	path := "/dash/api/admin/client-keys/" + view.ID
	if w := e.request("DELETE", path, nil, cookie, csrf); w.Code != 500 {
		t.Fatalf("failed revoke: %d %s", w.Code, w.Body.String())
	}
	assertClientKeyRejected(t, e.security, key)
	if _, err := e.security.AcquireClientPreBody(r); err == nil || err.Status != 401 {
		t.Fatal("failed durable revocation did not immediately deny admission")
	}
	for _, method := range []string{"PUT", "POST"} {
		mutationPath := path
		var body any = clientKeyRequest{Name: "revive", AllowedModels: []string{"*"}}
		if method == "POST" {
			mutationPath += "/rotate"
			body = nil
		}
		if w := e.request(method, mutationPath, body, cookie, csrf); w.Code != 409 {
			t.Fatalf("pending revoke allowed %s: %d", method, w.Code)
		}
	}
	var revoked int64
	if err := e.store.db.QueryRow("SELECT revoked_at FROM client_keys WHERE id=?", view.ID).Scan(&revoked); err != nil || revoked != 0 {
		t.Fatalf("failed transaction unexpectedly persisted: %d %v", revoked, err)
	}
	if _, err := e.store.db.Exec("DROP TRIGGER reject_revoke"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if w := e.request("DELETE", path, nil, cookie, csrf); w.Code != 200 {
			t.Fatalf("retry/idempotent revoke: %d %s", w.Code, w.Body.String())
		}
	}
	if err := e.store.db.QueryRow("SELECT revoked_at FROM client_keys WHERE id=?", view.ID).Scan(&revoked); err != nil || revoked == 0 {
		t.Fatalf("successful retry was not durable: %d %v", revoked, err)
	}
	var audits int
	if err := e.store.db.QueryRow("SELECT count(*) FROM audit_events WHERE action='client_key.revoke' AND subject=?", view.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("idempotent revocation audit: %d %v", audits, err)
	}
	e.startSecurity(t)
	assertClientKeyRejected(t, e.security, key)
}

// The real SQLite commit completes, but its return to the handler is gated so
// publication ordering and inference responsiveness do not depend on scheduling.
type clientCommitGate struct {
	driver    driver.Driver
	dsn       string
	claimed   atomic.Bool
	committed chan struct{}
	release   chan struct{}
	adminRead chan struct{}
	once      sync.Once
}

func (g *clientCommitGate) Connect(context.Context) (driver.Conn, error) {
	conn, err := g.driver.Open(g.dsn)
	if err != nil {
		return nil, err
	}
	return &clientGateConn{Conn: conn, gate: g}, nil
}

func (g *clientCommitGate) Driver() driver.Driver { return g.driver }

func (g *clientCommitGate) unblock() { g.once.Do(func() { close(g.release) }) }

type clientGateConn struct {
	driver.Conn
	gate *clientCommitGate
}

func (c *clientGateConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &clientGateTx{Tx: tx, gate: c.gate}, nil
}

func (c *clientGateConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c *clientGateConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if c.gate.claimed.Load() && strings.Contains(query, "FROM admin_sessions") {
		select {
		case c.gate.adminRead <- struct{}{}:
		default:
		}
	}
	return rows, err
}

type clientGateTx struct {
	driver.Tx
	gate *clientCommitGate
}

func (tx *clientGateTx) Commit() error {
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	if tx.gate.claimed.CompareAndSwap(false, true) {
		close(tx.gate.committed)
		<-tx.gate.release
	}
	return nil
}

func awaitClientMutation[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for synchronized client mutation")
		var zero T
		return zero
	}
}

func TestClientMutationPublicationAndInferenceDuringBlockedCommit(t *testing.T) {
	for _, operations := range [][2]string{{"rotate", "edit"}, {"edit", "rotate"}, {"rotate", "rotate"}, {"edit", "edit"}} {
		t.Run(operations[0]+"_then_"+operations[1], func(t *testing.T) {
			e := newSecurityTestEnv(t)
			cookie, csrf := e.seedSession(t)
			key, view := createSecurityTestKey(t, e, cookie, csrf, clientKeyRequest{Name: "original", AllowedModels: []string{"original"}, RPM: 1, TPM: 10, MaxConcurrent: 1})
			lease, policyErr := e.security.AcquireClient(authenticatedClientRequest(t, e.security, key), "original", 10)
			if policyErr != nil {
				t.Fatal(policyErr)
			}
			defer lease.Finish(10, false)
			oldDB := e.store.db
			gate := &clientCommitGate{driver: oldDB.Driver(), dsn: controlDSN(e.dbPath, false), committed: make(chan struct{}), release: make(chan struct{}), adminRead: make(chan struct{}, 1)}
			e.store.db = sql.OpenDB(gate)
			e.store.db.SetMaxOpenConns(4)
			if err := oldDB.Close(); err != nil {
				t.Fatal(err)
			}
			defer gate.unblock()
			path := "/dash/api/admin/client-keys/" + view.ID
			mutate := func(operation, name string) *httptest.ResponseRecorder {
				if operation == "rotate" {
					return e.request("POST", path+"/rotate", nil, cookie, csrf)
				}
				return e.request("PUT", path, clientKeyRequest{Name: name, AllowedModels: []string{name}, RPM: 1, TPM: 10, MaxConcurrent: 1}, cookie, csrf)
			}
			first := make(chan *httptest.ResponseRecorder, 1)
			go func() { first <- mutate(operations[0], "first") }()
			awaitClientMutation(t, gate.committed)
			inference := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				r := httptest.NewRequest("POST", "/inference-test", nil)
				r.Header.Set("Authorization", "Bearer "+key)
				w := httptest.NewRecorder()
				e.mux.ServeHTTP(w, r)
				inference <- w
			}()
			if w := awaitClientMutation(t, inference); w.Code != 204 {
				t.Fatalf("database-blocked writer stalled authentication: %d", w.Code)
			}
			second := make(chan *httptest.ResponseRecorder, 1)
			go func() { second <- mutate(operations[1], "second") }()
			awaitClientMutation(t, gate.adminRead)
			gate.unblock()
			responses := []*httptest.ResponseRecorder{awaitClientMutation(t, first), awaitClientMutation(t, second)}
			keys := []string{key}
			for _, w := range responses {
				if w.Code != 200 {
					t.Fatalf("concurrent mutation: %d %s", w.Code, w.Body.String())
				}
				var response struct {
					Key string `json:"key"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if response.Key != "" {
					keys = append(keys, response.Key)
				}
			}
			finalKey := keys[len(keys)-1]
			for _, oldKey := range keys[:len(keys)-1] {
				assertClientKeyRejected(t, e.security, oldKey)
			}
			request := authenticatedClientRequest(t, e.security, finalKey)
			identity := ClientIdentityFromRequest(request)
			model := "original"
			if operations[0] == "edit" {
				model = "first"
			}
			if operations[1] == "edit" {
				model = "second"
			}
			if identity.Name != model || !identity.AllowsModel(model) || (model != "original" && identity.AllowsModel("original")) {
				t.Fatalf("mutation restored stale/expanded policy: %+v", identity)
			}
			if _, err := e.security.AcquireClient(request, model, 0); err == nil || err.Status != 429 {
				t.Fatal("mutation refilled concurrency quota")
			}
			lease.Finish(10, false)
			if _, err := e.security.AcquireClient(request, model, 0); err == nil || err.Status != 429 {
				t.Fatal("mutation refilled RPM quota")
			}
			reloaded, err := NewSecurity(e.store, SecurityOptions{SetupToken: e.setupToken})
			if err != nil {
				t.Fatal(err)
			}
			e.security.mu.Lock()
			runtime := e.security.clients[view.ID]
			stored := reloaded.clients[view.ID]
			consistent := reflect.DeepEqual(runtime.view, stored.view) && runtime.hash == stored.hash && runtime.usage == lease.usage && runtime.hash == sha256.Sum256([]byte(finalKey)) && len(e.security.keys) == 1
			e.security.mu.Unlock()
			if !consistent {
				t.Fatal("runtime policy/hash differs from committed database or quota identity")
			}
			authenticatedClientRequest(t, reloaded, finalKey)
		})
	}
}

func TestClientPreBodyAdmissionUsesFreshPolicyWithoutTokenSpend(t *testing.T) {
	e := newSecurityTestEnv(t)
	cookie, csrf := e.seedSession(t)
	key, view := createSecurityTestKey(t, e, cookie, csrf, clientKeyRequest{Name: "prebody", AllowedModels: []string{"old"}, TPM: 10, RPM: 3, MaxConcurrent: 1})
	request := authenticatedClientRequest(t, e.security, key)
	spent, err := e.security.AcquireClient(request, "old", 10)
	if err != nil {
		t.Fatal(err)
	}
	spent.Finish(10, false)
	path := "/dash/api/admin/client-keys/" + view.ID
	if w := e.request("PUT", path, clientKeyRequest{Name: "fresh", AllowedModels: []string{"new"}, TPM: 5, RPM: 3, MaxConcurrent: 1}, cookie, csrf); w.Code != 200 {
		t.Fatalf("edit: %d %s", w.Code, w.Body.String())
	}
	lease, err := e.security.AcquireClientPreBody(request)
	if err != nil {
		t.Fatalf("prebody checked stale TPM bounds or exhausted token budget: %v", err)
	}
	if lease.Identity.TPM != 5 || lease.Identity.Name != "fresh" || !lease.Identity.AllowsModel("new") || lease.Identity.AllowsModel("old") {
		t.Fatalf("prebody returned stale policy: %+v", lease.Identity)
	}
	if _, err := e.security.AcquireClientPreBody(request); err == nil || err.Status != 429 || err.Message != "client concurrency limit reached" {
		t.Fatal("prebody failed to reserve concurrency")
	}
	lease.Finish(0, true)
	lease.Finish(0, true)
	last, err := e.security.AcquireClientPreBody(request)
	if err != nil {
		t.Fatalf("prebody charged more than one request or failed to release concurrency: %v", err)
	}
	last.Finish(0, true)
	if _, err := e.security.AcquireClientPreBody(request); err == nil || err.Status != 429 || err.Message != "client request rate limit reached" {
		t.Fatal("prebody did not charge exactly one RPM request")
	}
	e.security.mu.Lock()
	usage := *e.security.clients[view.ID].usage
	e.security.mu.Unlock()
	if usage.requests != 3 || usage.tokens != 10 || usage.active != 0 {
		t.Fatalf("prebody changed token accounting or leaked leases: %+v", usage)
	}
}

func TestClientPreBodyRejectsExpiredRevokedAndRotatedAuthentication(t *testing.T) {
	e := newSecurityTestEnv(t)
	cookie, csrf := e.seedSession(t)
	for _, mutation := range []string{"expire", "revoke", "rotate"} {
		t.Run(mutation, func(t *testing.T) {
			key, view := createSecurityTestKey(t, e, cookie, csrf, clientKeyRequest{Name: mutation})
			request := authenticatedClientRequest(t, e.security, key)
			path := "/dash/api/admin/client-keys/" + view.ID
			switch mutation {
			case "expire":
				e.security.mu.Lock()
				e.security.clients[view.ID].view.ExpiresAt = time.Now().Add(-time.Second).UnixMilli()
				e.security.mu.Unlock()
			case "revoke":
				if w := e.request("DELETE", path, nil, cookie, csrf); w.Code != 200 {
					t.Fatalf("revoke: %d", w.Code)
				}
			case "rotate":
				if w := e.request("POST", path+"/rotate", nil, cookie, csrf); w.Code != 200 {
					t.Fatalf("rotate: %d", w.Code)
				}
			}
			if _, err := e.security.AcquireClientPreBody(request); err == nil || err.Status != 401 {
				t.Fatal("prebody accepted stale authentication")
			}
			assertClientKeyRejected(t, e.security, key)
		})
	}
	if _, err := e.security.AcquireClientPreBody(httptest.NewRequest("POST", "/", nil)); err == nil || err.Status != 401 {
		t.Fatal("prebody accepted unauthenticated request")
	}
}
