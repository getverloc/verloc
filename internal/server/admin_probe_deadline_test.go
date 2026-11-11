package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func blockingProbeDNS(t *testing.T) <-chan struct{} {
	t.Helper()
	started := make(chan struct{}, 1)
	previous := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	t.Cleanup(func() { net.DefaultResolver = previous })
	return started
}

func awaitProbeSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("probe did not reach the expected phase or stop promptly")
	}
}

func TestResolveProbeConnDNSDeadline(t *testing.T) {
	blockingProbeDNS(t)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := (&Server{}).resolveProbeConn(ctx, probeReq{BaseURL: "https://probe-deadline.invalid/v1", NoAuth: true})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("DNS preflight did not propagate deadline: %v", err)
	}
}

func TestAdminProbeCancellationDuringDNS(t *testing.T) {
	for _, operation := range []string{"discover", "test"} {
		t.Run(operation, func(t *testing.T) {
			started := blockingProbeDNS(t)
			srv, _ := testAdminServer(t)
			body := []byte(`{"base_url":"https://probe-cancel.invalid/v1","no_auth":true,"upstream":"widget-v1","operation":"chat"}`)
			req := httptest.NewRequest(http.MethodPost, "/dash/api/admin/"+operation, bytes.NewReader(body))
			authorizeAdminTest(t, srv, req)
			ctx, cancel := context.WithCancel(req.Context())
			defer cancel()
			req = req.WithContext(ctx)
			rec := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				if operation == "discover" {
					srv.handleAdminDiscover(rec, req)
				} else {
					srv.handleAdminTest(rec, req)
				}
			}()
			awaitProbeSignal(t, started)
			cancel()
			awaitProbeSignal(t, done)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "context canceled") {
				t.Fatalf("canceled DNS preflight: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestProbeClientTimeoutIncludesResponseBody(t *testing.T) {
	stopped := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(stopped)
	}))
	defer up.Close()
	client := probeClient(1, true)
	defer client.CloseIdleConnections()
	if client.Timeout != time.Second {
		t.Fatalf("probe client has no overall timeout: %v", client.Timeout)
	}
	client.Timeout = 50 * time.Millisecond
	resp, err := client.Get(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, err = io.ReadAll(resp.Body)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled body did not time out: %v", err)
	}
	awaitProbeSignal(t, stopped)
}

func TestAdminProbeCancellationDuringBody(t *testing.T) {
	for _, operation := range []string{"discover", "test"} {
		t.Run(operation, func(t *testing.T) {
			started, stopped := make(chan struct{}), make(chan struct{})
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Even a complete JSON value is not a successful response until EOF.
				_, _ = io.WriteString(w, `{"data":[],"choices":[{"message":{"content":"PONG"}}]}`)
				w.(http.Flusher).Flush()
				close(started)
				<-r.Context().Done()
				close(stopped)
			}))
			defer up.Close()
			srv, _ := testAdminServer(t)
			body, _ := json.Marshal(map[string]any{
				"base_url": up.URL + "/v1", "no_auth": true,
				"allow_private_upstreams": true, "upstream": "widget-v1",
				"operation": "chat",
			})
			req := httptest.NewRequest(http.MethodPost, "/dash/api/admin/"+operation, bytes.NewReader(body))
			authorizeAdminTest(t, srv, req)
			ctx, cancel := context.WithCancel(req.Context())
			defer cancel()
			req = req.WithContext(ctx)
			rec := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				if operation == "discover" {
					srv.handleAdminDiscover(rec, req)
				} else {
					srv.handleAdminTest(rec, req)
				}
			}()
			awaitProbeSignal(t, started)
			cancel()
			awaitProbeSignal(t, done)
			awaitProbeSignal(t, stopped)
			if operation == "discover" {
				if rec.Code != http.StatusBadGateway {
					t.Fatalf("canceled discovery body: %d %s", rec.Code, rec.Body.String())
				}
			} else {
				var out struct {
					OK     bool   `json:"ok"`
					Status int    `json:"status"`
					Error  string `json:"error"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
					t.Fatal(err)
				}
				if rec.Code != http.StatusOK || out.OK || out.Status != 0 || out.Error == "" {
					t.Fatalf("canceled connectivity body reported success: %d %s", rec.Code, rec.Body.String())
				}
			}
		})
	}
}

func TestAdminDiscoverCancellationStopsPagination(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	var pages atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := pages.Add(1)
		if r.URL.Path != "/accounts/account/ai/models/search" || r.URL.Query().Get("page") != fmt.Sprint(page) {
			t.Errorf("unexpected pagination request: %s", r.URL)
		}
		if page == 1 {
			_, _ = io.WriteString(w, `{"success":true,"result":[{"id":"first"}],"result_info":{"total_pages":3}}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(stopped)
	}))
	defer up.Close()
	srv, _ := testAdminServer(t)
	body, _ := json.Marshal(map[string]any{
		"base_url": up.URL + "/accounts/account/ai/v1", "no_auth": true, "allow_private_upstreams": true,
	})
	req := httptest.NewRequest(http.MethodPost, "/dash/api/admin/discover", bytes.NewReader(body))
	authorizeAdminTest(t, srv, req)
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.handleAdminDiscover(rec, req)
	}()
	awaitProbeSignal(t, started)
	cancel()
	awaitProbeSignal(t, done)
	awaitProbeSignal(t, stopped)
	if rec.Code != http.StatusBadGateway || pages.Load() != 2 {
		t.Fatalf("canceled pagination continued: pages=%d status=%d body=%s", pages.Load(), rec.Code, rec.Body.String())
	}
}

func TestProbeClientRetainsSSRFGuards(t *testing.T) {
	var reached atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/target", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()
	blocked := probeClient(1, false)
	defer blocked.CloseIdleConnections()
	if resp, err := blocked.Get(up.URL); err == nil {
		resp.Body.Close()
		t.Fatal("private probe destination was reached without opt-in")
	}
	if reached.Load() != 0 {
		t.Fatal("private upstream received a blocked probe")
	}
	allowed := probeClient(1, true)
	defer allowed.CloseIdleConnections()
	resp, err := allowed.Get(up.URL + "/redirect")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound || reached.Load() != 1 {
		t.Fatalf("probe followed a redirect: status=%d requests=%d", resp.StatusCode, reached.Load())
	}
}
