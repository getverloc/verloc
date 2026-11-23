package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Fix #1: Dial-time SSRF guard on runtime upstream clients
// ---------------------------------------------------------------------------

func TestDialTimeSSRFGuardRuntimeUpstreams(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()

	t.Run("blocked_by_default", func(t *testing.T) {
		hits.Store(0)
		cfg := &Config{
			DefaultProvider: "local",
			ModelMap:        map[string]string{"m": "local:up"},
			Providers: map[string]*ProviderConfig{
				"local": {BaseURL: upstream.URL, NoAuth: true},
			},
		}
		s := runtimeTestServer(t, cfg)
		w := runtimeCall(s, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
		if w.Code == http.StatusOK {
			t.Fatalf("private loopback upstream should be blocked by default, got 200: %s", w.Body.String())
		}
		if hits.Load() != 0 {
			t.Fatalf("blocked private upstream received %d hits, want 0", hits.Load())
		}
	})

	t.Run("allowed_when_opted_in", func(t *testing.T) {
		hits.Store(0)
		cfg := &Config{
			DefaultProvider: "local",
			ModelMap:        map[string]string{"m": "local:up"},
			Providers: map[string]*ProviderConfig{
				"local": {BaseURL: upstream.URL, NoAuth: true, AllowPrivateUpstreams: true},
			},
		}
		s := runtimeTestServer(t, cfg)
		w := runtimeCall(s, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
		if w.Code != http.StatusOK || hits.Load() != 1 {
			t.Fatalf("opted-in private upstream failed: status=%d hits=%d body=%s", w.Code, hits.Load(), w.Body.String())
		}
	})

	t.Run("legacy_upstream_base_respects_flag", func(t *testing.T) {
		hits.Store(0)
		blocked := &Config{
			UpstreamBase: upstream.URL,
			Keys:         []string{"k1"},
			ModelMap:     map[string]string{"m": "up"},
		}
		sBlocked := runtimeTestServer(t, blocked)
		if w := runtimeCall(sBlocked, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`); w.Code == http.StatusOK {
			t.Fatalf("legacy UpstreamBase bypassed SSRF guard: %s", w.Body.String())
		}
		if hits.Load() != 0 {
			t.Fatalf("legacy blocked upstream received %d hits", hits.Load())
		}

		allowed := &Config{
			UpstreamBase:          upstream.URL,
			Keys:                  []string{"k1"},
			AllowPrivateUpstreams: true,
			ModelMap:              map[string]string{"m": "up"},
		}
		sAllowed := runtimeTestServer(t, allowed)
		if w := runtimeCall(sAllowed, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`); w.Code != http.StatusOK || hits.Load() != 1 {
			t.Fatalf("legacy opted-in upstream failed: status=%d hits=%d body=%s", w.Code, hits.Load(), w.Body.String())
		}
	})
}

func TestDialTimeSSRFGuardDNSRebindingAndPrivateRanges(t *testing.T) {
	// Direct classification checks on isPrivateIP.
	for _, raw := range []string{
		"127.0.0.1", "::1", "0.0.0.0", "::",
		"10.0.0.1", "172.16.5.1", "192.168.1.10",
		"169.254.169.254", "fe80::1",
		"100.64.0.1", "100.127.255.254", // RFC 6598 CGNAT
		"::ffff:127.0.0.1", "::ffff:169.254.169.254", "::ffff:100.64.0.1",
		"224.0.0.1", "ff02::1", "fc00::1",
	} {
		addr := netip.MustParseAddr(raw)
		if !isPrivateIP(addr) {
			t.Errorf("isPrivateIP(%s) = false, want true", raw)
		}
	}
	for _, raw := range []string{"93.184.216.34", "1.1.1.1", "8.8.8.8", "2606:2800:220:1:248:1893:25c8:1946"} {
		addr := netip.MustParseAddr(raw)
		if isPrivateIP(addr) {
			t.Errorf("isPrivateIP(%s) = true, want false", raw)
		}
	}

	// Simulate DNS rebinding at dial time: a public-looking hostname resolves
	// to private, metadata, CGNAT, or mixed [public, private] answers.
	cases := []struct {
		name  string
		addrs []string
	}{
		{"loopback_v4", []string{"127.0.0.1"}},
		{"loopback_v6", []string{"::1"}},
		{"metadata_imds", []string{"169.254.169.254"}},
		{"rfc1918", []string{"10.1.2.3"}},
		{"cgnat", []string{"100.64.10.1"}},
		{"v4_mapped_v6", []string{"::ffff:127.0.0.1"}},
		{"dual_record_public_and_private", []string{"93.184.216.34", "127.0.0.1"}},
	}

	prevLookup := lookupNetIP
	t.Cleanup(func() { lookupNetIP = prevLookup })

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parsed := make([]netip.Addr, len(tc.addrs))
			for i, a := range tc.addrs {
				parsed[i] = netip.MustParseAddr(a)
			}
			lookupNetIP = func(ctx context.Context, network, host string) ([]netip.Addr, error) {
				if host == "rebind.example.test" {
					return parsed, nil
				}
				return nil, fmt.Errorf("unexpected host %q", host)
			}
			client := newClient(5, false)
			defer client.CloseIdleConnections()
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://rebind.example.test:8080/v1/chat/completions", nil)
			resp, err := client.Do(req)
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			if err == nil {
				t.Fatalf("expected SSRF block for %v, got nil error", tc.addrs)
			}
			if !strings.Contains(err.Error(), "private or local address") {
				t.Fatalf("unexpected error for %v: %v", tc.addrs, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Fix #2: Speculative hedging max_hedges == 2 + global concurrency cap
// ---------------------------------------------------------------------------

func TestSpeculativeHedgingMaxHedgesTwo(t *testing.T) {
	pCanceled := make(chan struct{}, 1)
	h1Canceled := make(chan struct{}, 1)

	hangServer := func(canceled chan struct{}) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case <-r.Context().Done():
				select {
				case canceled <- struct{}{}:
				default:
				}
			case <-time.After(3 * time.Second):
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"too-slow"}}]}`)
			}
		}))
	}

	pSrv := hangServer(pCanceled)
	defer pSrv.Close()
	h1Srv := hangServer(h1Canceled)
	defer h1Srv.Close()
	h2Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"third-route-winner"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	}))
	defer h2Srv.Close()

	cfg := &Config{
		DefaultProvider: "p",
		ModelMap:        map[string]string{"m": "p:up-p"},
		ModelRoutes: map[string][]RouteTarget{
			"m": {
				{Backend: "h1", Upstream: "up-h1"},
				{Backend: "h2", Upstream: "up-h2"},
			},
		},
		ModelMeta: map[string]*ModelMeta{
			"m": {Routing: &RoutePolicy{HedgeDelayMs: 25, MaxHedges: 2}},
		},
		Providers: map[string]*ProviderConfig{
			"p":  {BaseURL: pSrv.URL, NoAuth: true, AllowPrivateUpstreams: true, MaxInFlight: 2},
			"h1": {BaseURL: h1Srv.URL, NoAuth: true, AllowPrivateUpstreams: true, MaxInFlight: 2},
			"h2": {BaseURL: h2Srv.URL, NoAuth: true, AllowPrivateUpstreams: true, MaxInFlight: 2},
		},
	}
	s := runtimeTestServer(t, cfg)

	start := time.Now()
	w := runtimeCall(s, `{"model":"m"}`)
	elapsed := time.Since(start)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "third-route-winner") {
		t.Fatalf("second hedge did not win: status=%d body=%s", w.Code, w.Body.String())
	}
	if elapsed > time.Second {
		t.Fatalf("max_hedges=2 call took too long: %v", elapsed)
	}
	for name, ch := range map[string]chan struct{}{"p": pCanceled, "h1": h1Canceled} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("losing attempt %s was not canceled", name)
		}
	}

	rt := s.runtime()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if rt.backends["p"].route.inFlight.Load() == 0 &&
			rt.backends["h1"].route.inFlight.Load() == 0 &&
			rt.backends["h2"].route.inFlight.Load() == 0 &&
			s.activeHedges.Load() == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if rt.backends["p"].route.inFlight.Load() != 0 ||
		rt.backends["h1"].route.inFlight.Load() != 0 ||
		rt.backends["h2"].route.inFlight.Load() != 0 ||
		s.activeHedges.Load() != 0 {
		t.Fatalf("leaked slots: p=%d h1=%d h2=%d activeHedges=%d",
			rt.backends["p"].route.inFlight.Load(),
			rt.backends["h1"].route.inFlight.Load(),
			rt.backends["h2"].route.inFlight.Load(),
			s.activeHedges.Load())
	}
}

func TestSpeculativeHedgingFastLaunchesSecondHedgeOnFirstBackupError(t *testing.T) {
	pCanceled := make(chan struct{}, 1)
	pSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
			select {
			case pCanceled <- struct{}{}:
			default:
			}
		case <-time.After(3 * time.Second):
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"slow-primary"}}]}`)
		}
	}))
	defer pSrv.Close()
	h1Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"overloaded"}}`)
	}))
	defer h1Srv.Close()
	h2Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"h2-after-h1-503"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer h2Srv.Close()

	cfg := &Config{
		DefaultProvider: "p",
		ModelMap:        map[string]string{"m": "p:up-p"},
		ModelRoutes: map[string][]RouteTarget{
			"m": {
				{Backend: "h1", Upstream: "up-h1"},
				{Backend: "h2", Upstream: "up-h2"},
			},
		},
		ModelMeta: map[string]*ModelMeta{
			"m": {Routing: &RoutePolicy{HedgeDelayMs: 30, MaxHedges: 2}},
		},
		Providers: map[string]*ProviderConfig{
			"p":  {BaseURL: pSrv.URL, NoAuth: true, AllowPrivateUpstreams: true},
			"h1": {BaseURL: h1Srv.URL, NoAuth: true, AllowPrivateUpstreams: true},
			"h2": {BaseURL: h2Srv.URL, NoAuth: true, AllowPrivateUpstreams: true},
		},
	}
	s := runtimeTestServer(t, cfg)
	w := runtimeCall(s, `{"model":"m"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "h2-after-h1-503") {
		t.Fatalf("expected h2 win after h1 503: status=%d body=%s", w.Code, w.Body.String())
	}
	select {
	case <-pCanceled:
	case <-time.After(time.Second):
		t.Fatal("primary was not canceled after h2 won")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && s.activeHedges.Load() != 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if s.activeHedges.Load() != 0 {
		t.Fatalf("activeHedges leaked: %d", s.activeHedges.Load())
	}
}

func TestSpeculativeHedgingGlobalConcurrencyCap(t *testing.T) {
	// Validate GatewayLimits bounds for MaxConcurrentHedges.
	for _, bad := range []GatewayLimits{{MaxConcurrentHedges: -1}, {MaxConcurrentHedges: 4097}} {
		if bad.Validate() == nil {
			t.Fatalf("expected validation error for %+v", bad)
		}
	}

	var curHedgeHits, peakHedgeHits atomic.Int32
	pSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
			return
		case <-time.After(200 * time.Millisecond):
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"primary-finish"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		}
	}))
	defer pSrv.Close()

	makeHedgeSrv := func(label string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			n := curHedgeHits.Add(1)
			defer curHedgeHits.Add(-1)
			for {
				prev := peakHedgeHits.Load()
				if n <= prev || peakHedgeHits.CompareAndSwap(prev, n) {
					break
				}
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(80 * time.Millisecond):
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"`+label+`"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			}
		}))
	}
	h1Srv := makeHedgeSrv("h1")
	defer h1Srv.Close()
	h2Srv := makeHedgeSrv("h2")
	defer h2Srv.Close()

	cfg := &Config{
		DefaultProvider: "p",
		ModelMap:        map[string]string{"m": "p:up-p"},
		ModelRoutes: map[string][]RouteTarget{
			"m": {
				{Backend: "h1", Upstream: "up-h1"},
				{Backend: "h2", Upstream: "up-h2"},
			},
		},
		ModelMeta: map[string]*ModelMeta{
			"m": {Routing: &RoutePolicy{HedgeDelayMs: 20, MaxHedges: 2}},
		},
		Limits: GatewayLimits{
			MaxConcurrentHedges: 1,
		},
		Providers: map[string]*ProviderConfig{
			"p":  {BaseURL: pSrv.URL, NoAuth: true, AllowPrivateUpstreams: true},
			"h1": {BaseURL: h1Srv.URL, NoAuth: true, AllowPrivateUpstreams: true},
			"h2": {BaseURL: h2Srv.URL, NoAuth: true, AllowPrivateUpstreams: true},
		},
	}
	s := runtimeTestServer(t, cfg)

	var wg sync.WaitGroup
	results := make([]*httptest.ResponseRecorder, 2)
	for i := range 2 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = runtimeCall(s, `{"model":"m"}`)
		}(i)
	}
	wg.Wait()

	for i, w := range results {
		if w.Code != http.StatusOK {
			t.Fatalf("concurrent request %d failed: status=%d body=%s", i, w.Code, w.Body.String())
		}
	}
	if peak := peakHedgeHits.Load(); peak > 1 {
		t.Fatalf("global hedge concurrency cap violated: peak concurrent hedges=%d, want <= 1", peak)
	}

	rt := s.runtime()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if s.activeHedges.Load() == 0 &&
			rt.backends["p"].route.inFlight.Load() == 0 &&
			rt.backends["h1"].route.inFlight.Load() == 0 &&
			rt.backends["h2"].route.inFlight.Load() == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if s.activeHedges.Load() != 0 {
		t.Fatalf("activeHedges leaked after cap test: %d", s.activeHedges.Load())
	}
}

// ---------------------------------------------------------------------------
// Fix #3: Streaming client-disconnect classification in Anthropic & Responses
// ---------------------------------------------------------------------------

type errAfterFirstRead struct {
	first []byte
	done  bool
	err   error
}

func (r *errAfterFirstRead) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		n := copy(p, r.first)
		return n, nil
	}
	return 0, r.err
}

func TestStreamingClientDisconnectClassifiedAsCanceledNotBackendFailure(t *testing.T) {
	// Unit-level writer wrapping checks.
	var clientErr *streamClientError
	dw := &disconnectedStreamWriter{headers: make(http.Header)}
	if err := writeAnthropicEvent(dw, "ping", map[string]string{"type": "ping"}); !errors.As(err, &clientErr) {
		t.Fatalf("writeAnthropicEvent did not wrap client write error in *streamClientError: %v", err)
	}
	if err := emitPing(dw); !errors.As(err, &clientErr) {
		t.Fatalf("emitPing did not wrap client write error in *streamClientError: %v", err)
	}
	if err := writeResponsesEvent(dw, map[string]any{"type": "response.created"}); !errors.As(err, &clientErr) {
		t.Fatalf("writeResponsesEvent did not wrap client write error in *streamClientError: %v", err)
	}

	// Upstream read error during Anthropic streaming must NOT be masked by a
	// synthetic message_stop or misclassified as *streamClientError.
	upErr := errors.New("upstream TCP reset")
	recBuf := httptest.NewRecorder()
	err := streamCopyAnthropic(recBuf, &errAfterFirstRead{
		first: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"),
		err:   upErr,
	}, "nv/test", nil, nil)
	if !errors.Is(err, upErr) {
		t.Fatalf("expected upstream read error %v, got %v", upErr, err)
	}
	if errors.As(err, &clientErr) {
		t.Fatalf("upstream read error misclassified as *streamClientError: %v", err)
	}
	if strings.Contains(recBuf.Body.String(), "message_stop") {
		t.Fatalf("errored Anthropic stream emitted message_stop: %s", recBuf.Body.String())
	}

	// End-to-end tests for both Anthropic Messages and OpenAI Responses.
	for _, proto := range []string{"anthropic", "responses"} {
		t.Run(proto, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n")
				_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":6,\"completion_tokens\":2}}\n\n")
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			}))
			defer upstream.Close()

			cfg := runtimeConfig(upstream.URL)
			s := runtimeTestServer(t, cfg)
			w := &disconnectedStreamWriter{headers: make(http.Header)}
			if proto == "anthropic" {
				req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"test","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
				s.handleAnthropicMessages(w, req)
			} else {
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test","input":"hi","stream":true}`))
				s.handleResponses(w, req)
			}

			rec := runtimeRecord(t, s)
			if rec.OK || rec.Status != 499 || rec.Outcome != "canceled" {
				t.Fatalf("%s disconnect accounting wrong: %+v", proto, rec)
			}
			if len(rec.AttemptLog) != 1 || rec.AttemptLog[0].Outcome != "canceled" || rec.AttemptLog[0].Status != 499 {
				t.Fatalf("%s attempt log wrong: %+v", proto, rec.AttemptLog)
			}
			rm := s.runtime().backends["primary"].route
			rm.mu.Lock()
			ewmaErr := rm.ewmaErr
			rm.mu.Unlock()
			if ewmaErr != 0 {
				t.Fatalf("%s client disconnect penalized backend health: ewmaErr=%v", proto, ewmaErr)
			}
			s.admissionMu.Lock()
			active := s.active
			s.admissionMu.Unlock()
			if active != 0 {
				t.Fatalf("%s client disconnect leaked %d admissions", proto, active)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Fix #4: Cancellable rollup backfill tracked & awaited on Store.Close()
// ---------------------------------------------------------------------------

func TestStoreRollupBackfillCancellableAndAwaitedOnClose(t *testing.T) {
	seedStore := func(t *testing.T) *Store {
		t.Helper()
		path := filepath.Join(t.TempDir(), "analytics.db")
		store, err := OpenStore(path)
		if err != nil {
			t.Fatal(err)
		}
		base := time.Now().Add(-3 * time.Hour).Truncate(time.Minute).UnixMilli()
		for i := range 60 {
			d := &Delta{
				TS:       base + int64(i)*30_000,
				Requests: 2,
				TokIn:    100,
				TokOut:   50,
				Cost:     0.001,
				MS:       200,
				Models:   map[string]deltaModel{"m": {Requests: 2, TokIn: 100, TokOut: 50, Cost: 0.001}},
				Backends: map[string]deltaBackend{"b": {Requests: 2, TokIn: 100, TokOut: 50, Cost: 0.001}},
			}
			if err := store.InsertDelta(d); err != nil {
				t.Fatal(err)
			}
		}
		batch := make([]ReqRecord, 0, 50)
		for i := range 50 {
			batch = append(batch, ReqRecord{
				TS: base + int64(i)*10_000, ID: fmt.Sprintf("req-%d", i),
				Model: "m", Backend: "b", OK: true, Status: 200, Outcome: "completed",
				UsageKnown: true, TokIn: 10, TokOut: 5, LatMs: 50,
			})
		}
		if err := store.insertRequests(batch); err != nil {
			t.Fatal(err)
		}
		// Clear derived rollups & totals & request hour cache marker so
		// EnsureRollupsBackfilled launches all background repair stages.
		store.mu.Lock()
		for _, q := range []string{
			`DELETE FROM snapshot_minute`,
			`DELETE FROM snapshot_10m`,
			`DELETE FROM snapshot_hour`,
			`UPDATE analytics_totals SET ticks=0, oldest_ts=0, newest_ts=0 WHERE id=1`,
			`DELETE FROM analytics_meta WHERE name='request_hour_cache_ready'`,
		} {
			if _, err := store.db.Exec(q); err != nil {
				store.mu.Unlock()
				t.Fatalf("reset derived state %q: %v", q, err)
			}
		}
		store.mu.Unlock()
		return store
	}

	t.Run("close_cancels_and_awaits_backfill_without_races", func(t *testing.T) {
		store := seedStore(t)
		store.EnsureRollupsBackfilled(context.Background())
		if err := store.Close(); err != nil {
			t.Fatalf("Store.Close() failed while backfill active: %v", err)
		}
		// Calling EnsureRollupsBackfilled after Close must be a safe no-op.
		store.EnsureRollupsBackfilled(context.Background())
	})

	t.Run("pre_canceled_context_returns_immediately", func(t *testing.T) {
		store := seedStore(t)
		defer func() {
			if err := store.Close(); err != nil {
				t.Fatalf("Store.Close(): %v", err)
			}
		}()
		canceled, cancel := context.WithCancel(context.Background())
		cancel()

		if _, err := store.RebuildRollups(canceled); !errors.Is(err, context.Canceled) {
			t.Fatalf("RebuildRollups(canceled) = %v, want context.Canceled", err)
		}
		if _, _, err := store.RebuildCoarseRollupsFromMinutes(canceled); !errors.Is(err, context.Canceled) {
			t.Fatalf("RebuildCoarseRollupsFromMinutes(canceled) = %v, want context.Canceled", err)
		}
		if err := store.RebuildTotalsContext(canceled); !errors.Is(err, context.Canceled) {
			t.Fatalf("RebuildTotalsContext(canceled) = %v, want context.Canceled", err)
		}
		store.EnsureRollupsBackfilled(canceled)
	})
}

// ---------------------------------------------------------------------------
// Fix #6: Safe upstream 4xx error forwarding (with strict analytics isolation)
// ---------------------------------------------------------------------------

func TestSafeUpstreamErrorMessageSanitization(t *testing.T) {
	secrets := []string{"sk-live-secret-999", "acct-secret-42"}

	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"openai_nested_error", 400, `{"error":{"message":"max_tokens exceeds limit 8192","type":"invalid_request_error"}}`, "max_tokens exceeds limit 8192"},
		{"string_error_field", 422, `{"error":"invalid tool schema"}`, "invalid tool schema"},
		{"top_level_message", 400, `{"message":"bad parameter temperature"}`, "bad parameter temperature"},
		{"fastapi_detail", 422, `{"detail":"field required: messages"}`, "field required: messages"},
		{"cloudflare_errors_array", 400, `{"success":false,"errors":[{"message":"invalid input shape"}]}`, "invalid input shape"},
		{"auth_401_suppressed", 401, `{"error":{"message":"invalid api key sk-foo"}}`, defaultUpstreamRejectedMsg},
		{"billing_402_suppressed", 402, `{"error":{"message":"insufficient credits on org acct"}}`, defaultUpstreamRejectedMsg},
		{"forbidden_403_suppressed", 403, `{"error":{"message":"organization suspended"}}`, defaultUpstreamRejectedMsg},
		{"proxy_auth_407_suppressed", 407, `{"error":{"message":"proxy auth failed"}}`, defaultUpstreamRejectedMsg},
		{"server_500_suppressed", 500, `{"error":{"message":"internal stack trace"}}`, defaultUpstreamRejectedMsg},
		{"raw_html_suppressed", 400, `<html><body>Bad Request</body></html>`, defaultUpstreamRejectedMsg},
		{"echoed_upstream_key_rejected", 400, `{"error":{"message":"rejected request for key sk-live-secret-999"}}`, defaultUpstreamRejectedMsg},
		{"echoed_account_id_rejected", 400, `{"error":{"message":"account acct-secret-42 not allowed"}}`, defaultUpstreamRejectedMsg},
		{"bearer_redacted", 400, `{"error":{"message":"bad header Bearer eyJhbGciOiJIUzI1NiJ9.payload"}}`, "bad header [redacted]"},
		{"key_prefix_redacted", 400, `{"error":{"message":"invalid token sk-ant-1234567890abcdef in payload"}}`, "invalid token [redacted] in payload"},
		{"url_creds_redacted", 400, `{"error":{"message":"failed calling https://admin:supersecret@api.example.com/v1"}}`, "failed calling https://[redacted]@api.example.com/v1"},
		{"control_chars_collapsed", 400, "{\"error\":{\"message\":\"line1\\r\\n\\tline2\\u0007\"}}", "line1 line2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := safeUpstreamErrorMessage(tc.status, []byte(tc.body), secrets...)
			if got != tc.want {
				t.Fatalf("safeUpstreamErrorMessage(%d, %s) = %q, want %q", tc.status, tc.body, got, tc.want)
			}
		})
	}

	// Long UTF-8 message truncated cleanly to <= 300 bytes + "...".
	longMsg := strings.Repeat("é", 250)
	got := safeUpstreamErrorMessage(400, []byte(`{"error":{"message":"`+longMsg+`"}}`))
	if !utf8.ValidString(got) || len(got) > 304 || !strings.HasSuffix(got, "...") {
		t.Fatalf("long UTF-8 truncation invalid: len=%d valid=%v suffix=%q", len(got), utf8.ValidString(got), got[max(0, len(got)-6):])
	}
}

func TestSafeUpstream4xxErrorForwardingEndToEnd(t *testing.T) {
	type endpointCase struct {
		name string
		call func(s *Server) *httptest.ResponseRecorder
		// extractMessage reads the error message from the protocol-native JSON envelope.
		extractMessage func(t *testing.T, body []byte) string
	}

	extractOpenAI := func(t *testing.T, body []byte) string {
		t.Helper()
		var out struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("invalid error JSON %s: %v", body, err)
		}
		return out.Error.Message
	}

	extractAnthropic := func(t *testing.T, body []byte) string {
		t.Helper()
		var out struct {
			Type  string `json:"type"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &out); err != nil || out.Type != "error" {
			t.Fatalf("invalid Anthropic error JSON %s: %v", body, err)
		}
		return out.Error.Message
	}

	endpoints := []endpointCase{
		{
			name: "openai_chat",
			call: func(s *Server) *httptest.ResponseRecorder {
				return runtimeCall(s, `{"model":"test","messages":[{"role":"user","content":"hi"}]}`)
			},
			extractMessage: extractOpenAI,
		},
		{
			name: "anthropic_messages",
			call: func(s *Server) *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				s.handleAnthropicMessages(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"test","max_tokens":999999,"messages":[{"role":"user","content":"hi"}]}`)))
				return w
			},
			extractMessage: extractAnthropic,
		},
		{
			name: "openai_responses",
			call: func(s *Server) *httptest.ResponseRecorder {
				return responsesCall(s, `{"model":"test","input":"hi"}`)
			},
			extractMessage: extractOpenAI,
		},
		{
			name: "hedged_chat",
			call: func(s *Server) *httptest.ResponseRecorder {
				return runtimeCall(s, `{"model":"hedged","messages":[{"role":"user","content":"hi"}]}`)
			},
			extractMessage: extractOpenAI,
		},
		{
			name: "embeddings_media",
			call: func(s *Server) *httptest.ResponseRecorder {
				return mediaCall(s, "/v1/embeddings", `{"model":"emb","input":"hi"}`, "application/json")
			},
			extractMessage: extractOpenAI,
		},
	}

	scenarios := []struct {
		name           string
		upstreamStatus int
		upstreamBody   string
		wantClientMsg  string
		forbiddenInRec string
	}{
		{
			name:           "safe_400_forwarded_but_never_in_analytics",
			upstreamStatus: http.StatusBadRequest,
			upstreamBody:   `{"error":{"message":"max_tokens exceeds model limit 8192"}}`,
			wantClientMsg:  "max_tokens exceeds model limit 8192",
			forbiddenInRec: "max_tokens",
		},
		{
			name:           "echoed_upstream_secret_suppressed",
			upstreamStatus: http.StatusBadRequest,
			upstreamBody:   `{"error":{"message":"bad request with secret-credential-1"}}`,
			wantClientMsg:  defaultUpstreamRejectedMsg,
			forbiddenInRec: "secret-credential-1",
		},
		{
			name:           "forbidden_403_billing_suppressed",
			upstreamStatus: http.StatusForbidden,
			upstreamBody:   `{"error":{"message":"organization billing account suspended"}}`,
			wantClientMsg:  defaultUpstreamRejectedMsg,
			forbiddenInRec: "billing",
		},
	}

	for _, ep := range endpoints {
		for _, sc := range scenarios {
			t.Run(ep.name+"/"+sc.name, func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(sc.upstreamStatus)
					_, _ = io.WriteString(w, sc.upstreamBody)
				}))
				defer upstream.Close()

				cfg := runtimeConfig(upstream.URL)
				cfg.ModelMap["hedged"] = "primary:up-h"
				cfg.ModelMap["emb"] = "primary:up-emb"
				cfg.Providers["backup"] = &ProviderConfig{
					BaseURL:               upstream.URL,
					AllowPrivateUpstreams: true,
					Keys:                  []KeyEntry{{ID: "credential-2", Key: "secret-credential-2"}},
				}
				cfg.ModelRoutes = map[string][]RouteTarget{
					"hedged": {{Backend: "backup", Upstream: "up-h2"}},
				}
				cfg.ModelMeta = map[string]*ModelMeta{
					"hedged": {Routing: &RoutePolicy{HedgeDelayMs: 100, MaxHedges: 1}},
				}
				// Ensure 403 triggers non-retryable failure rather than rotating to backup in this scenario.
				if sc.upstreamStatus == http.StatusForbidden {
					cfg.Providers["primary"].Keys = []KeyEntry{{ID: "credential-1", Key: "secret-credential-1"}}
					cfg.ModelRoutes = nil
					cfg.ModelMeta = nil
					if ep.name == "hedged_chat" {
						// 403 is retryable (key failure) and fails over to backup which also returns 403.
						cfg.ModelRoutes = map[string][]RouteTarget{
							"hedged": {{Backend: "backup", Upstream: "up-h2"}},
						}
						cfg.ModelMeta = map[string]*ModelMeta{
							"hedged": {Routing: &RoutePolicy{HedgeDelayMs: 100, MaxHedges: 1}},
						}
					}
				}
				s := runtimeTestServer(t, cfg)
				w := ep.call(s)
				if w.Code != sc.upstreamStatus {
					t.Fatalf("status=%d want %d (body=%s)", w.Code, sc.upstreamStatus, w.Body.String())
				}
				gotMsg := ep.extractMessage(t, w.Body.Bytes())
				if gotMsg != sc.wantClientMsg {
					t.Fatalf("client error message = %q, want %q (body=%s)", gotMsg, sc.wantClientMsg, w.Body.String())
				}
				if strings.Contains(w.Body.String(), "secret-credential-1") {
					t.Fatalf("upstream secret leaked to client: %s", w.Body.String())
				}

				rec := runtimeRecord(t, s)
				encoded, _ := json.Marshal(rec)
				if bytes.Contains(encoded, []byte(sc.forbiddenInRec)) {
					t.Fatalf("upstream error text leaked into analytics record: %s", encoded)
				}
				if rec.ErrMsg != "Request did not complete successfully" {
					t.Fatalf("rec.ErrMsg = %q, want sanitized generic message", rec.ErrMsg)
				}
			})
		}
	}
}
