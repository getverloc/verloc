package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestKeyPoolProactiveRateLimitSteering(t *testing.T) {
	t.Run("multi_key_steers_away_without_hard_cooldown", func(t *testing.T) {
		pool := NewKeyPool([]KeyEntry{
			{ID: "k1", Key: "secret-1"},
			{ID: "k2", Key: "secret-2"},
		}, 30*time.Second)

		k1 := pool.states[0]
		h := http.Header{}
		h.Set("X-Ratelimit-Remaining-Requests", "0")
		h.Set("X-Ratelimit-Reset-Requests", "15s")
		pool.ObserveHeaders(k1, h)

		if pool.InCooldown() != 0 {
			t.Fatalf("soft steering must not count as hard cooldown: InCooldown=%d", pool.InCooldown())
		}
		if k1.failCount.Load() != 0 {
			t.Fatalf("soft steering must not increment failCount: %d", k1.failCount.Load())
		}

		for range 10 {
			got := pool.Next()
			if got == nil || got.ID != "k2" {
				t.Fatalf("expected healthy k2 while k1 is soft-cooled, got %+v", got)
			}
		}

		// Positive remaining header clears soft cooldown immediately.
		hOK := http.Header{}
		hOK.Set("X-Ratelimit-Remaining-Requests", "42")
		pool.ObserveHeaders(k1, hOK)
		seenK1 := false
		for range 4 {
			if got := pool.Next(); got != nil && got.ID == "k1" {
				seenK1 = true
				break
			}
		}
		if !seenK1 {
			t.Fatal("k1 remained soft-cooled after positive remaining header")
		}
	})

	t.Run("single_key_falls_back_cleanly", func(t *testing.T) {
		pool := NewKeyPool([]KeyEntry{{ID: "only", Key: "secret-only"}}, 30*time.Second)
		k := pool.states[0]
		h := http.Header{}
		h.Set("Anthropic-Ratelimit-Tokens-Remaining", "0")
		h.Set("Anthropic-Ratelimit-Tokens-Reset", time.Now().Add(10*time.Second).UTC().Format(time.RFC3339))
		pool.ObserveHeaders(k, h)

		got := pool.Next()
		if got == nil || got.ID != "only" {
			t.Fatalf("single soft-cooled key must fall back instead of returning nil: got %+v", got)
		}
	})

	t.Run("parseRateLimitReset_formats", func(t *testing.T) {
		if d := parseRateLimitReset("2.5"); d < 2400*time.Millisecond || d > 2600*time.Millisecond {
			t.Fatalf("numeric seconds: got %v", d)
		}
		if d := parseRateLimitReset("750ms"); d != 750*time.Millisecond {
			t.Fatalf("go duration: got %v", d)
		}
		future := time.Now().Add(5 * time.Second).UTC().Format(time.RFC3339)
		if d := parseRateLimitReset(future); d < 3*time.Second || d > 6*time.Second {
			t.Fatalf("rfc3339: got %v", d)
		}
		if d := parseRateLimitReset("bogus"); d != 0 {
			t.Fatalf("invalid reset should be 0, got %v", d)
		}
	})
}

func TestKeyPoolErrorClassBackoff(t *testing.T) {
	pool := NewKeyPool([]KeyEntry{
		{ID: "auth-bad", Key: "k-auth"},
		{ID: "transient", Key: "k-503"},
		{ID: "override", Key: "k-429"},
	}, 10*time.Second)

	pool.MarkFailureStatus(pool.states[0], http.StatusUnauthorized, 0)
	authRem := time.Until(time.Unix(0, pool.states[0].cooldownUntil.Load()))
	if authRem < 4*time.Minute+20*time.Second || authRem > 6*time.Minute {
		t.Fatalf("401 quarantine expected ~5m±10%%, got %v", authRem)
	}

	pool.MarkFailureStatus(pool.states[1], http.StatusServiceUnavailable, 0)
	trRem := time.Until(time.Unix(0, pool.states[1].cooldownUntil.Load()))
	if trRem < 8*time.Second || trRem > 12*time.Second {
		t.Fatalf("503 cooldown expected ~10s±10%%, got %v", trRem)
	}

	pool.MarkFailureStatus(pool.states[2], http.StatusTooManyRequests, 42*time.Second)
	ovRem := time.Until(time.Unix(0, pool.states[2].cooldownUntil.Load()))
	if ovRem < 40*time.Second || ovRem > 43*time.Second {
		t.Fatalf("explicit override expected ~42s, got %v", ovRem)
	}
}

func TestKeyPoolHalfOpenSingleProbe(t *testing.T) {
	pool := NewKeyPool([]KeyEntry{{ID: "k1", Key: "secret-1"}}, 30*time.Second)
	k := pool.states[0]

	// Put key into failed state with an already-expired cooldown (half-open).
	pool.MarkFailureStatus(k, http.StatusServiceUnavailable, time.Millisecond)
	k.cooldownUntil.Store(time.Now().Add(-time.Millisecond).UnixNano())

	// Concurrent callers racing for the expired key must admit exactly ONE probe.
	const racers = 16
	var admitted atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if got := pool.Next(); got != nil {
				admitted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if admitted.Load() != 1 {
		t.Fatalf("half-open admitted %d concurrent probes, want 1", admitted.Load())
	}

	// Releasing the probe (e.g. client cancel) allows another single probe.
	pool.ReleaseProbe(k)
	if got := pool.Next(); got == nil {
		t.Fatal("expected probe admission after ReleaseProbe")
	}
	if got := pool.Next(); got != nil {
		t.Fatal("second concurrent probe must be blocked while probeInFlight is true")
	}

	// First success clears probeInFlight but keeps failCount until recoverSuccesses (2).
	pool.MarkSuccess(k)
	if k.failCount.Load() == 0 {
		t.Fatal("single success cleared failCount prematurely")
	}
	// Second probe succeeds and fully recovers key.
	if got := pool.Next(); got == nil {
		t.Fatal("expected second half-open probe after first success")
	}
	pool.MarkSuccess(k)
	if k.failCount.Load() != 0 || k.cooldownUntil.Load() != 0 {
		t.Fatalf("key did not recover after 2 successes: failCount=%d cooldown=%d", k.failCount.Load(), k.cooldownUntil.Load())
	}
	// Now healthy: multiple concurrent Next() calls succeed without probe gating.
	if pool.Next() == nil || pool.Next() == nil {
		t.Fatal("recovered healthy key should allow concurrent callers")
	}
}

func TestAdaptiveRoutingPeakEWMAAndWeighted(t *testing.T) {
	t.Run("latency_prefers_fast_healthy_backend", func(t *testing.T) {
		var slowHits, fastHits atomic.Int32
		slowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			slowHits.Add(1)
			time.Sleep(40 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"slow"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		}))
		defer slowSrv.Close()
		fastSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fastHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"fast"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		}))
		defer fastSrv.Close()

		cfg := &Config{
			DefaultProvider: "slow",
			ModelMap:        map[string]string{"m": "slow:up-slow"},
			ModelRoutes:     map[string][]RouteTarget{"m": {{Backend: "fast", Upstream: "up-fast"}}},
			ModelMeta:       map[string]*ModelMeta{"m": {Routing: &RoutePolicy{Strategy: "latency"}}},
			Providers: map[string]*ProviderConfig{
				"slow": {BaseURL: slowSrv.URL, NoAuth: true, AllowPrivateUpstreams: true},
				"fast": {BaseURL: fastSrv.URL, NoAuth: true, AllowPrivateUpstreams: true},
			},
		}
		s := runtimeTestServer(t, cfg)

		// Seed EWMA: slow is 250ms, fast is 5ms.
		rt := s.runtime()
		rt.backends["slow"].route.Observe(250*time.Millisecond, true)
		rt.backends["fast"].route.Observe(5*time.Millisecond, true)

		for range 5 {
			w := runtimeCall(s, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		}
		if fastHits.Load() != 5 || slowHits.Load() != 0 {
			t.Fatalf("latency routing chose fast=%d slow=%d, want fast=5 slow=0", fastHits.Load(), slowHits.Load())
		}
	})

	t.Run("weighted_distribution_and_unhealthy_bypass", func(t *testing.T) {
		var aHits, bHits atomic.Int32
		srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			aHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		}))
		defer srvA.Close()
		srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			bHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		}))
		defer srvB.Close()

		cfg := &Config{
			DefaultProvider: "a",
			ModelMap:        map[string]string{"m": "a:up-a"},
			ModelRoutes:     map[string][]RouteTarget{"m": {{Backend: "b", Upstream: "up-b", Weight: 3}}},
			ModelMeta:       map[string]*ModelMeta{"m": {Routing: &RoutePolicy{Strategy: "weighted"}}},
			Providers: map[string]*ProviderConfig{
				"a": {BaseURL: srvA.URL, AllowPrivateUpstreams: true, Keys: []KeyEntry{{ID: "ka", Key: "sec-a"}}},
				"b": {BaseURL: srvB.URL, AllowPrivateUpstreams: true, Keys: []KeyEntry{{ID: "kb", Key: "sec-b"}}},
			},
		}
		s := runtimeTestServer(t, cfg)

		// Default primary weight = 1, fallback b weight = 3 => 1:3 ratio over 20 requests.
		for range 20 {
			w := runtimeCall(s, `{"model":"m"}`)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d", w.Code)
			}
		}
		if aHits.Load() != 5 || bHits.Load() != 15 {
			t.Fatalf("weighted split got a=%d b=%d, want a=5 b=15", aHits.Load(), bHits.Load())
		}

		// Cool down all keys on b: weighted router must immediately steer 100% to a.
		bPool := s.runtime().backends["b"].pool
		bPool.MarkFailureStatus(bPool.states[0], http.StatusTooManyRequests, time.Minute)
		aBefore, bBefore := aHits.Load(), bHits.Load()
		for range 4 {
			w := runtimeCall(s, `{"model":"m"}`)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d", w.Code)
			}
		}
		if aHits.Load()-aBefore != 4 || bHits.Load()-bBefore != 0 {
			t.Fatalf("cooled backend b still received traffic: da=%d db=%d", aHits.Load()-aBefore, bHits.Load()-bBefore)
		}
	})
}

func TestSpeculativeHedging(t *testing.T) {
	t.Run("fast_primary_avoids_hedge", func(t *testing.T) {
		var pHits, hHits atomic.Int32
		pSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			pHits.Add(1)
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"primary"}}],"usage":{"prompt_tokens":2,"completion_tokens":2}}`)
		}))
		defer pSrv.Close()
		hSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hHits.Add(1)
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hedge"}}],"usage":{"prompt_tokens":2,"completion_tokens":2}}`)
		}))
		defer hSrv.Close()

		cfg := &Config{
			DefaultProvider: "p",
			ModelMap:        map[string]string{"m": "p:up-p"},
			ModelRoutes:     map[string][]RouteTarget{"m": {{Backend: "h", Upstream: "up-h"}}},
			ModelMeta:       map[string]*ModelMeta{"m": {Routing: &RoutePolicy{HedgeDelayMs: 200}}},
			Providers: map[string]*ProviderConfig{
				"p": {BaseURL: pSrv.URL, NoAuth: true, AllowPrivateUpstreams: true},
				"h": {BaseURL: hSrv.URL, NoAuth: true, AllowPrivateUpstreams: true},
			},
		}
		s := runtimeTestServer(t, cfg)
		w := runtimeCall(s, `{"model":"m"}`)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "primary") {
			t.Fatalf("unexpected response: %d %s", w.Code, w.Body.String())
		}
		if pHits.Load() != 1 || hHits.Load() != 0 {
			t.Fatalf("fast primary should not trigger hedge: p=%d h=%d", pHits.Load(), hHits.Load())
		}
	})

	t.Run("slow_primary_hedges_and_cancels_loser_without_leaks", func(t *testing.T) {
		primaryCanceled := make(chan struct{}, 1)
		pSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case <-r.Context().Done():
				select {
				case primaryCanceled <- struct{}{}:
				default:
				}
			case <-time.After(2 * time.Second):
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"slow-primary"}}]}`)
			}
		}))
		defer pSrv.Close()
		hSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"fast-hedge"}}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`)
		}))
		defer hSrv.Close()

		cfg := &Config{
			DefaultProvider: "p",
			ModelMap:        map[string]string{"m": "p:up-p"},
			ModelRoutes:     map[string][]RouteTarget{"m": {{Backend: "h", Upstream: "up-h"}}},
			ModelMeta:       map[string]*ModelMeta{"m": {Routing: &RoutePolicy{HedgeDelayMs: 25}}},
			Providers: map[string]*ProviderConfig{
				"p": {BaseURL: pSrv.URL, NoAuth: true, AllowPrivateUpstreams: true, MaxInFlight: 2},
				"h": {BaseURL: hSrv.URL, NoAuth: true, AllowPrivateUpstreams: true, MaxInFlight: 2},
			},
		}
		s := runtimeTestServer(t, cfg)
		start := time.Now()
		w := runtimeCall(s, `{"model":"m"}`)
		elapsed := time.Since(start)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "fast-hedge") {
			t.Fatalf("hedge did not win: %d %s", w.Code, w.Body.String())
		}
		if elapsed > 500*time.Millisecond {
			t.Fatalf("hedged call took too long: %v", elapsed)
		}
		select {
		case <-primaryCanceled:
		case <-time.After(time.Second):
			t.Fatal("losing slow primary request was not canceled")
		}

		// Wait briefly for loser reaper goroutine and verify zero slot leaks.
		rt := s.runtime()
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if rt.backends["p"].route.inFlight.Load() == 0 && rt.backends["h"].route.inFlight.Load() == 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if rt.backends["p"].route.inFlight.Load() != 0 || rt.backends["h"].route.inFlight.Load() != 0 {
			t.Fatalf("leaked bulkhead slots: p=%d h=%d", rt.backends["p"].route.inFlight.Load(), rt.backends["h"].route.inFlight.Load())
		}
	})
}

func TestProviderConcurrencyBulkhead(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var pHits, fHits atomic.Int32
	pSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pHits.Add(1)
		close(entered)
		<-release
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"primary"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer pSrv.Close()
	fSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fHits.Add(1)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"fallback"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer fSrv.Close()

	cfg := &Config{
		DefaultProvider: "p",
		ModelMap:        map[string]string{"m": "p:up-p"},
		ModelRoutes:     map[string][]RouteTarget{"m": {{Backend: "f", Upstream: "up-f"}}},
		Providers: map[string]*ProviderConfig{
			"p": {BaseURL: pSrv.URL, NoAuth: true, AllowPrivateUpstreams: true, MaxInFlight: 1},
			"f": {BaseURL: fSrv.URL, NoAuth: true, AllowPrivateUpstreams: true, MaxInFlight: 4},
		},
	}
	s := runtimeTestServer(t, cfg)

	done1 := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done1 <- runtimeCall(s, `{"model":"m"}`)
	}()
	<-entered

	// While request 1 occupies primary's only bulkhead slot, request 2 must
	// immediately fail over to fallback without blocking.
	start := time.Now()
	w2 := runtimeCall(s, `{"model":"m"}`)
	if w2.Code != http.StatusOK || !strings.Contains(w2.Body.String(), "fallback") {
		t.Fatalf("second request should fail over to fallback: %d %s", w2.Code, w2.Body.String())
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("bulkhead failover blocked unexpectedly: %v", time.Since(start))
	}

	close(release)
	w1 := <-done1
	if w1.Code != http.StatusOK || !strings.Contains(w1.Body.String(), "primary") {
		t.Fatalf("first request failed: %d %s", w1.Code, w1.Body.String())
	}
	if pHits.Load() != 1 || fHits.Load() != 1 {
		t.Fatalf("hits p=%d f=%d, want 1/1", pHits.Load(), fHits.Load())
	}
	rt := s.runtime()
	if rt.backends["p"].route.inFlight.Load() != 0 || rt.backends["f"].route.inFlight.Load() != 0 {
		t.Fatalf("leaked bulkhead slots: p=%d f=%d", rt.backends["p"].route.inFlight.Load(), rt.backends["f"].route.inFlight.Load())
	}
}

func TestSlidingWindowClientRateLimiting(t *testing.T) {
	e := newSecurityTestEnv(t)
	cookie, csrf := e.seedSession(t)
	key, view := createSecurityTestKey(t, e, cookie, csrf, clientKeyRequest{
		Name:          "sliding",
		AllowedModels: []string{"m"},
		RPM:           5,
		TPM:           100,
		MaxConcurrent: 10,
	})
	r := authenticatedClientRequest(t, e.security, key)

	base := time.Now().Truncate(time.Minute)

	// Start window 0 at base with 1 request, then burst 4 more at base+50s (total 5 = RPM).
	var leases []*ClientLease
	l0, perr := e.security.acquireClientAt(r, "m", 10, true, base)
	if perr != nil {
		t.Fatalf("window 0 req 0 rejected: %v", perr)
	}
	leases = append(leases, l0)
	for i := 1; i < 5; i++ {
		lease, perr := e.security.acquireClientAt(r, "m", 10, true, base.Add(50*time.Second))
		if perr != nil {
			t.Fatalf("window 0 req %d rejected: %v", i, perr)
		}
		leases = append(leases, lease)
	}
	// Release the first 4 in window 0; hold lease[4] across the boundary to test late Finish.
	for _, l := range leases[:4] {
		l.Finish(10, true)
	}
	// 6th request in window 0 is rejected.
	if _, perr := e.security.acquireClientAt(r, "m", 10, true, base.Add(55*time.Second)); perr == nil || perr.Status != 429 {
		t.Fatalf("expected 429 in full window 0, got %v", perr)
	}

	// Cross minute boundary to base+65s (inWindow=5s -> prevWeight=55/60=0.9167 -> ceil(5*55/60)=5).
	// A fixed window would allow 5 fresh requests (10 in 15s = 2x burst); sliding window blocks immediately!
	if _, perr := e.security.acquireClientAt(r, "m", 10, true, base.Add(65*time.Second)); perr == nil || perr.Status != 429 {
		t.Fatalf("sliding window allowed 2x boundary burst at +5s: %v", perr)
	}

	// At base+75s (inWindow=15s -> prevWeight=45/60=0.75 -> ceil(5*0.75)=4): admits 1 request, blocks 2nd.
	l6, perr := e.security.acquireClientAt(r, "m", 10, true, base.Add(75*time.Second))
	if perr != nil {
		t.Fatalf("expected 1 slot freed at +15s, got %v", perr)
	}
	l6.Finish(10, true)
	if _, perr := e.security.acquireClientAt(r, "m", 10, true, base.Add(75*time.Second)); perr == nil || perr.Status != 429 {
		t.Fatalf("expected 2nd request at +15s to be rejected, got %v", perr)
	}

	// Late Finish on lease[4] (from window 0) reconciles prevTokens in the just-slid window 1.
	leases[4].Finish(2, true) // reserved 10, actual 2 -> refund 8 from prevTokens (50 -> 42)
	e.security.mu.Lock()
	rec := e.security.clients[view.ID]
	e.security.mu.Unlock()
	rec.usage.mu.Lock()
	prevTok := rec.usage.prevTokens
	active := rec.usage.active
	rec.usage.mu.Unlock()
	if prevTok != 42 || active != 0 {
		t.Fatalf("late cross-boundary Finish: prevTokens=%d active=%d, want 42/0", prevTok, active)
	}
}

func TestDeterministicResponseCache(t *testing.T) {
	t.Run("chat_and_embeddings_eligibility_and_tenant_isolation", func(t *testing.T) {
		var chatHits, embHits atomic.Int32
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if strings.HasSuffix(r.URL.Path, "/embeddings") {
				n := embHits.Add(1)
				_, _ = io.WriteString(w, `{"object":"list","data":[{"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":4,"total_tokens":4},"hit":`+jsonInt(n)+`}`)
				return
			}
			n := chatHits.Add(1)
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"answer-`+jsonInt(n)+`"}}],"usage":{"prompt_tokens":6,"completion_tokens":4}}`)
		}))
		defer up.Close()

		cfg := &Config{
			DefaultProvider: "p",
			ModelMap:        map[string]string{"m": "p:up-m", "emb": "p:up-emb"},
			Cache:           &ResponseCacheConfig{Enabled: true, MaxEntries: 16, TTLSeconds: 60},
			Providers:       map[string]*ProviderConfig{"p": {BaseURL: up.URL, NoAuth: true, AllowPrivateUpstreams: true}},
		}
		s := runtimeTestServer(t, cfg)

		callChat := func(body, clientID, cacheCtrl string) *httptest.ResponseRecorder {
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			if cacheCtrl != "" {
				r.Header.Set("Cache-Control", cacheCtrl)
			}
			if clientID != "" {
				r = r.WithContext(context.WithValue(r.Context(), clientIdentityContextKey{}, &ClientIdentity{ID: clientID, Name: clientID, AllowedModels: []string{"*"}}))
			}
			w := httptest.NewRecorder()
			s.handleChat(w, r)
			return w
		}

		// Deterministic chat (temperature: 0): MISS then HIT.
		w1 := callChat(`{"model":"m","temperature":0,"messages":[{"role":"user","content":"2+2"}]}`, "tenant-a", "")
		w2 := callChat(`{"model":"m","temperature":0,"messages":[{"role":"user","content":"2+2"}]}`, "tenant-a", "")
		if w1.Code != 200 || w1.Header().Get("X-Verloc-Cache") != "MISS" {
			t.Fatalf("first chat: code=%d hdr=%q", w1.Code, w1.Header().Get("X-Verloc-Cache"))
		}
		if w2.Code != 200 || w2.Header().Get("X-Verloc-Cache") != "HIT" || w2.Body.String() != w1.Body.String() {
			t.Fatalf("second chat expected cache HIT: code=%d hdr=%q body=%s", w2.Code, w2.Header().Get("X-Verloc-Cache"), w2.Body.String())
		}
		if chatHits.Load() != 1 {
			t.Fatalf("upstream chat hits=%d, want 1", chatHits.Load())
		}

		// Tenant isolation: tenant-b must NOT receive tenant-a's cached entry.
		w3 := callChat(`{"model":"m","temperature":0,"messages":[{"role":"user","content":"2+2"}]}`, "tenant-b", "")
		if w3.Header().Get("X-Verloc-Cache") != "MISS" || chatHits.Load() != 2 {
			t.Fatalf("tenant isolation failed: hdr=%q hits=%d", w3.Header().Get("X-Verloc-Cache"), chatHits.Load())
		}

		// Cache-Control: no-store bypasses cache completely.
		w4 := callChat(`{"model":"m","temperature":0,"messages":[{"role":"user","content":"2+2"}]}`, "tenant-a", "no-store")
		if w4.Header().Get("X-Verloc-Cache") != "" || chatHits.Load() != 3 {
			t.Fatalf("no-store did not bypass cache: hdr=%q hits=%d", w4.Header().Get("X-Verloc-Cache"), chatHits.Load())
		}

		// Non-zero temperature is never cached.
		w5 := callChat(`{"model":"m","temperature":0.7,"messages":[{"role":"user","content":"2+2"}]}`, "tenant-a", "")
		if w5.Header().Get("X-Verloc-Cache") != "" || chatHits.Load() != 4 {
			t.Fatalf("temperature=0.7 should bypass cache: hdr=%q hits=%d", w5.Header().Get("X-Verloc-Cache"), chatHits.Load())
		}

		// Embeddings are cached by default when Cache.Enabled is true.
		callEmb := func(body string) *httptest.ResponseRecorder {
			r := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body))
			w := httptest.NewRecorder()
			s.handleEmbeddings(w, r)
			return w
		}
		e1 := callEmb(`{"model":"emb","input":"hello"}`)
		e2 := callEmb(`{"model":"emb","input":"hello"}`)
		if e1.Code != 200 || e1.Header().Get("X-Verloc-Cache") != "MISS" {
			t.Fatalf("first emb: code=%d hdr=%q", e1.Code, e1.Header().Get("X-Verloc-Cache"))
		}
		if e2.Code != 200 || e2.Header().Get("X-Verloc-Cache") != "HIT" || e2.Body.String() != e1.Body.String() {
			t.Fatalf("second emb: code=%d hdr=%q", e2.Code, e2.Header().Get("X-Verloc-Cache"))
		}
		if embHits.Load() != 1 {
			t.Fatalf("upstream emb hits=%d, want 1", embHits.Load())
		}
	})

	t.Run("lru_eviction_and_ttl_expiry", func(t *testing.T) {
		c := newResponseCache(&ResponseCacheConfig{Enabled: true, MaxEntries: 2, TTLSeconds: 1})
		k1 := responseCacheKey("c", "chat", "m", RouteTarget{Backend: "p", Upstream: "u"}, []byte("1"))
		k2 := responseCacheKey("c", "chat", "m", RouteTarget{Backend: "p", Upstream: "u"}, []byte("2"))
		k3 := responseCacheKey("c", "chat", "m", RouteTarget{Backend: "p", Upstream: "u"}, []byte("3"))

		c.Put(k1, []byte(`{"v":1}`), "application/json", 1, 1, 0, true)
		c.Put(k2, []byte(`{"v":2}`), "application/json", 1, 1, 0, true)
		// Touch k1 so k2 becomes LRU.
		if _, ok := c.Get(k1); !ok {
			t.Fatal("k1 missing")
		}
		c.Put(k3, []byte(`{"v":3}`), "application/json", 1, 1, 0, true)
		if _, ok := c.Get(k2); ok {
			t.Fatal("k2 should have been LRU-evicted")
		}
		if _, ok := c.Get(k1); !ok {
			t.Fatal("k1 should survive LRU eviction")
		}

		// Force expiry on k1 and verify Get evicts it.
		c.mu.Lock()
		c.entries[k1].Value.(*cachedResponse).expiresAt = time.Now().Add(-time.Second)
		c.mu.Unlock()
		if _, ok := c.Get(k1); ok {
			t.Fatal("expired entry returned hit")
		}
	})
}

func TestAdminResiliencePersistenceAndSchemaUpgrade(t *testing.T) {
	srv, dbPath := testAdminServer(t)
	defer srv.stopKeepWarm()

	// 1. Provider bulkhead + keep-warm via Admin API.
	pBody := `{
		"name":"fast",
		"base_url":"https://api.fast.test/v1",
		"no_auth":true,
		"max_in_flight":24,
		"keep_warm_seconds":45
	}`
	req := httptest.NewRequest(http.MethodPut, "/dash/api/admin/providers", bytes.NewBufferString(pBody))
	authorizeAdminTest(t, srv, req)
	rec := httptest.NewRecorder()
	srv.handleAdminProviderPut(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("put provider: %d %s", rec.Code, rec.Body.String())
	}
	var pResp struct {
		Provider adminProviderView `json:"provider"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pResp); err != nil {
		t.Fatal(err)
	}
	if pResp.Provider.MaxInFlight != 24 || pResp.Provider.KeepWarmSeconds != 45 {
		t.Fatalf("provider view lost fields: %+v", pResp.Provider)
	}

	// 2. Model adaptive routing + fallback weights via Admin API.
	mBody := `{
		"id":"fast/weighted",
		"backend":"fast",
		"upstream":"up-fast",
		"routes":[{"backend":"mock","upstream":"up-mock","weight":7}],
		"routing":{"strategy":"weighted","primary_weight":3,"hedge_delay_ms":250,"max_hedges":2}
	}`
	req = httptest.NewRequest(http.MethodPut, "/dash/api/admin/models", bytes.NewBufferString(mBody))
	authorizeAdminTest(t, srv, req)
	rec = httptest.NewRecorder()
	srv.handleAdminModelPut(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("put model: %d %s", rec.Code, rec.Body.String())
	}
	var mResp struct {
		Model adminModelView `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &mResp); err != nil {
		t.Fatal(err)
	}
	if mResp.Model.Routing == nil || mResp.Model.Routing.Strategy != "weighted" ||
		mResp.Model.Routing.PrimaryWeight != 3 || mResp.Model.Routing.HedgeDelayMs != 250 ||
		mResp.Model.Routing.MaxHedges != 2 {
		t.Fatalf("model routing view mismatch: %+v", mResp.Model.Routing)
	}
	if len(mResp.Model.Routes) != 1 || mResp.Model.Routes[0].Weight != 7 {
		t.Fatalf("fallback weight mismatch: %+v", mResp.Model.Routes)
	}

	// 3. Gateway response cache via Admin Settings API.
	sBody := `{
		"cache":{"enabled":true,"max_entries":128,"max_bytes":16777216,"ttl_seconds":120,"models":["fast/weighted"]}
	}`
	req = httptest.NewRequest(http.MethodPut, "/dash/api/admin/settings", bytes.NewBufferString(sBody))
	authorizeAdminTest(t, srv, req)
	rec = httptest.NewRecorder()
	srv.handleAdminSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("put settings cache: %d %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/dash/api/admin/settings", nil)
	authorizeAdminTest(t, srv, req)
	rec = httptest.NewRecorder()
	srv.handleAdminSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get settings: %d %s", rec.Code, rec.Body.String())
	}
	var sResp struct {
		Cache      ResponseCacheConfig `json:"cache"`
		CacheStats ResponseCacheStats  `json:"cache_stats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sResp); err != nil {
		t.Fatal(err)
	}
	if !sResp.Cache.Enabled || sResp.Cache.MaxEntries != 128 || sResp.Cache.MaxBytes != 16777216 ||
		sResp.Cache.TTLSeconds != 120 || len(sResp.Cache.Models) != 1 || sResp.Cache.Models[0] != "fast/weighted" {
		t.Fatalf("settings cache mismatch: %+v", sResp.Cache)
	}
	if sResp.CacheStats.Entries != 0 || srv.runtime().cache == nil {
		t.Fatalf("live cache not active: stats=%+v", sResp.CacheStats)
	}

	// 4. Verify persistence across reopening ControlStore.
	masterKey := filepath.Join(filepath.Dir(dbPath), "master.key")
	reopened, err := OpenControlStore(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	disk, err := reopened.LoadOrImportConfig(&Config{})
	if err != nil {
		reopened.Close()
		t.Fatal(err)
	}
	if disk.Providers["fast"].MaxInFlight != 24 || disk.Providers["fast"].KeepWarmSeconds != 45 {
		reopened.Close()
		t.Fatalf("persisted provider fields mismatch: %+v", disk.Providers["fast"])
	}
	if disk.ModelMeta["fast/weighted"].Routing == nil ||
		disk.ModelMeta["fast/weighted"].Routing.Strategy != "weighted" ||
		disk.ModelMeta["fast/weighted"].Routing.PrimaryWeight != 3 ||
		disk.ModelRoutes["fast/weighted"][0].Weight != 7 {
		reopened.Close()
		t.Fatalf("persisted model routing mismatch: meta=%+v routes=%+v", disk.ModelMeta["fast/weighted"], disk.ModelRoutes["fast/weighted"])
	}
	if disk.Cache == nil || !disk.Cache.Enabled || disk.Cache.MaxEntries != 128 || disk.Cache.TTLSeconds != 120 {
		reopened.Close()
		t.Fatalf("persisted cache config mismatch: %+v", disk.Cache)
	}

	// 5. Simulate an existing pre-weight control.db and verify OpenControlStore upgrades idempotently.
	if _, err := reopened.db.Exec(`ALTER TABLE model_routes DROP COLUMN weight`); err != nil {
		reopened.Close()
		t.Fatalf("drop weight column for upgrade test: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := OpenControlStore(dbPath, masterKey)
	if err != nil {
		t.Fatalf("reopen pre-weight control.db failed: %v", err)
	}
	defer upgraded.Close()
	afterUpgrade, err := upgraded.LoadOrImportConfig(&Config{})
	if err != nil {
		t.Fatalf("load after upgrade failed: %v", err)
	}
	if len(afterUpgrade.ModelRoutes["fast/weighted"]) != 1 || afterUpgrade.ModelRoutes["fast/weighted"][0].Weight != 0 {
		t.Fatalf("upgraded routes unexpected: %+v", afterUpgrade.ModelRoutes["fast/weighted"])
	}
}

func jsonInt(n int32) string {
	b, _ := json.Marshal(n)
	return string(b)
}
