package server

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// RoutePolicy configures opt-in adaptive routing and hedging per model.
// Zero value preserves the default ordered failover behavior.
type RoutePolicy struct {
	Strategy      string `json:"strategy,omitempty"`       // "" | "ordered" (default) | "latency" | "weighted"
	PrimaryWeight int    `json:"primary_weight,omitempty"` // weight for primary target when strategy == "weighted" (default 1)
	HedgeDelayMs  int    `json:"hedge_delay_ms,omitempty"` // 0 = disabled; >0 launches a speculative backup after delay
	MaxHedges     int    `json:"max_hedges,omitempty"`     // default 1 when HedgeDelayMs > 0 (capped at 2)
}

// normalizeRoutePolicy validates and canonicalizes a RoutePolicy. Returns nil
// when the policy carries only default zero-values so config stays compact.
func normalizeRoutePolicy(p *RoutePolicy) (*RoutePolicy, error) {
	if p == nil {
		return nil, nil
	}
	strategy := strings.ToLower(strings.TrimSpace(p.Strategy))
	switch strategy {
	case "", "ordered", "latency", "weighted":
	default:
		return nil, fmt.Errorf("routing strategy must be ordered, latency, or weighted")
	}
	if p.PrimaryWeight < 0 || p.PrimaryWeight > 1000 {
		return nil, fmt.Errorf("routing primary_weight must be between 0 and 1000")
	}
	if p.HedgeDelayMs < 0 || p.HedgeDelayMs > 30000 {
		return nil, fmt.Errorf("routing hedge_delay_ms must be between 0 and 30000")
	}
	if p.MaxHedges < 0 || p.MaxHedges > 2 {
		return nil, fmt.Errorf("routing max_hedges must be between 0 and 2")
	}
	if strategy == "ordered" {
		strategy = ""
	}
	out := &RoutePolicy{
		Strategy:      strategy,
		PrimaryWeight: p.PrimaryWeight,
		HedgeDelayMs:  p.HedgeDelayMs,
		MaxHedges:     p.MaxHedges,
	}
	if out.Strategy == "" && out.PrimaryWeight == 0 && out.HedgeDelayMs == 0 && out.MaxHedges == 0 {
		return nil, nil
	}
	return out, nil
}

// routeMetrics tracks Peak EWMA latency/error score and per-provider
// concurrency bulkhead state for a single backend.
type routeMetrics struct {
	inFlight    atomic.Int64
	maxInFlight int64 // 0 = unlimited

	mu          sync.Mutex
	initialized bool
	ewmaLatMs   float64
	ewmaErr     float64
	lastUpdate  time.Time
}

func newRouteMetrics(maxInFlight int) *routeMetrics {
	if maxInFlight < 0 {
		maxInFlight = 0
	}
	return &routeMetrics{maxInFlight: int64(maxInFlight)}
}

// tryAcquireSlot acquires a concurrency slot for this backend. When
// maxInFlight == 0 (default), it always succeeds while still tracking active
// in-flight concurrency for Peak EWMA routing.
func (r *routeMetrics) tryAcquireSlot() bool {
	if r == nil {
		return true
	}
	if r.maxInFlight <= 0 {
		r.inFlight.Add(1)
		return true
	}
	for {
		cur := r.inFlight.Load()
		if cur >= r.maxInFlight {
			return false
		}
		if r.inFlight.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

func (r *routeMetrics) releaseSlot() {
	if r == nil {
		return
	}
	r.inFlight.Add(-1)
}

func (r *routeMetrics) saturated() bool {
	if r == nil || r.maxInFlight <= 0 {
		return false
	}
	return r.inFlight.Load() >= r.maxInFlight
}

const (
	ewmaDecayTauSec    = 10.0 // latency/error EWMA decay window
	ewmaErrDecayTauSec = 30.0 // error-rate recovery half-life when idle
	ewmaColdLatencyMs  = 50.0 // optimistic default for unmeasured routes
)

// Observe updates the backend's time-decayed latency and error EWMA.
func (r *routeMetrics) Observe(lat time.Duration, ok bool) {
	if r == nil {
		return
	}
	latMs := float64(lat.Nanoseconds()) / 1e6
	if latMs < 1.0 {
		latMs = 1.0
	}
	errVal := 0.0
	if !ok {
		errVal = 1.0
	}
	now := time.Now()
	r.mu.Lock()
	if !r.initialized {
		r.initialized = true
		r.ewmaLatMs = latMs
		r.ewmaErr = errVal
		r.lastUpdate = now
		r.mu.Unlock()
		return
	}
	dt := now.Sub(r.lastUpdate).Seconds()
	if dt < 0 {
		dt = 0
	}
	w := math.Exp(-dt / ewmaDecayTauSec)
	r.ewmaLatMs = w*r.ewmaLatMs + (1-w)*latMs
	r.ewmaErr = w*r.ewmaErr + (1-w)*errVal
	r.lastUpdate = now
	r.mu.Unlock()
}

// score returns the Peak EWMA cost (lower is better):
//
//	ewmaLatMs * (inFlight + 1) * (1 + 4*decayedErrorRate)
//
// Saturated bulkheads or fully cooled key pools receive a large penalty so
// healthy routes always sort ahead of them.
func (r *routeMetrics) score(now time.Time, pool *KeyPool) float64 {
	var latMs, errRate float64
	if r != nil {
		r.mu.Lock()
		if !r.initialized {
			latMs = ewmaColdLatencyMs
		} else {
			dt := now.Sub(r.lastUpdate).Seconds()
			if dt < 0 {
				dt = 0
			}
			latMs = r.ewmaLatMs
			errRate = r.ewmaErr * math.Exp(-dt/ewmaErrDecayTauSec)
		}
		r.mu.Unlock()
	} else {
		latMs = ewmaColdLatencyMs
	}
	inFlight := int64(0)
	if r != nil {
		inFlight = max(int64(0), r.inFlight.Load())
	}
	cost := latMs * float64(inFlight+1) * (1.0 + 4.0*errRate)
	if r != nil && r.saturated() {
		cost += 1e12
	}
	if pool != nil && pool.Count() > 0 && pool.InCooldown() >= pool.Count() {
		cost += 1e12
	}
	return cost
}

type candidateRoute struct {
	target    RouteTarget
	isPrimary bool
}

// buildCandidateRoutes orders [primary, fallbacks...] according to the model's
// RoutePolicy. When policy is nil or "ordered", the slice preserves exact
// primary-then-fallback order with zero extra locking.
func buildCandidateRoutes(primary RouteTarget, fallbacks []RouteTarget, rt *runtimeState, policy *RoutePolicy, rr *atomic.Uint64) []candidateRoute {
	if policy != nil && primary.Weight <= 0 && policy.PrimaryWeight > 0 {
		primary.Weight = policy.PrimaryWeight
	}
	out := make([]candidateRoute, 0, 1+len(fallbacks))
	out = append(out, candidateRoute{target: primary, isPrimary: true})
	for _, f := range fallbacks {
		out = append(out, candidateRoute{target: f, isPrimary: false})
	}
	if len(out) <= 1 || policy == nil {
		return out
	}
	switch strings.ToLower(strings.TrimSpace(policy.Strategy)) {
	case "", "ordered":
		return out
	case "latency":
		now := time.Now()
		type scored struct {
			cand  candidateRoute
			score float64
			idx   int
		}
		items := make([]scored, len(out))
		for i, c := range out {
			b := rt.backends[c.target.Backend]
			s := 1e15
			if b != nil {
				s = b.route.score(now, b.pool)
			}
			items[i] = scored{cand: c, score: s, idx: i}
		}
		sort.SliceStable(items, func(i, j int) bool {
			return items[i].score < items[j].score
		})
		for i, it := range items {
			out[i] = it.cand
		}
		return out
	case "weighted":
		total := uint64(0)
		weights := make([]uint64, len(out))
		for i, c := range out {
			w := c.target.Weight
			if w <= 0 {
				w = 1
			}
			weights[i] = uint64(w)
			total += uint64(w)
		}
		if total == 0 {
			return out
		}
		pick := uint64(0)
		if rr != nil {
			pick = (rr.Add(1) - 1) % total
		}
		chosen := 0
		var acc uint64
		for i, w := range weights {
			acc += w
			if pick < acc {
				chosen = i
				break
			}
		}
		// If the chosen route's backend is saturated or all keys are in hard
		// cooldown, rotate to the first healthy candidate while keeping the
		// remaining routes as failovers.
		if !routeBackendHealthy(rt, out[chosen].target.Backend) {
			for i := 1; i < len(out); i++ {
				alt := (chosen + i) % len(out)
				if routeBackendHealthy(rt, out[alt].target.Backend) {
					chosen = alt
					break
				}
			}
		}
		if chosen == 0 {
			return out
		}
		rotated := make([]candidateRoute, 0, len(out))
		rotated = append(rotated, out[chosen])
		for i, c := range out {
			if i != chosen {
				rotated = append(rotated, c)
			}
		}
		return rotated
	default:
		return out
	}
}

func routeBackendHealthy(rt *runtimeState, name string) bool {
	if rt == nil {
		return false
	}
	if p := rt.cfg.Providers[name]; p != nil && p.Disabled {
		return false
	}
	b := rt.backends[name]
	if b == nil {
		return false
	}
	if b.route != nil && b.route.saturated() {
		return false
	}
	if b.pool != nil && b.pool.Count() > 0 && b.pool.InCooldown() >= b.pool.Count() {
		return false
	}
	return true
}

func modelRoutePolicy(cfg *Config, model string) *RoutePolicy {
	if cfg == nil || cfg.ModelMeta == nil {
		return nil
	}
	meta := cfg.ModelMeta[model]
	if meta == nil {
		return nil
	}
	return meta.Routing
}

// syncKeepWarm starts or replaces lightweight connection keep-warm loops for
// providers that explicitly configure KeepWarmSeconds > 0. Zero-value configs
// spawn no goroutines.
func (s *Server) syncKeepWarm() {
	s.keepWarmMu.Lock()
	if s.keepWarmCancel != nil {
		s.keepWarmCancel()
		s.keepWarmCancel = nil
	}
	rt := s.runtime()
	if rt == nil || rt.cfg == nil || s.shuttingDown.Load() {
		s.keepWarmMu.Unlock()
		return
	}
	type target struct {
		b        *backend
		url      string
		interval time.Duration
	}
	var targets []target
	for name, p := range rt.cfg.Providers {
		if p == nil || p.Disabled || p.KeepWarmSeconds <= 0 {
			continue
		}
		b := rt.backends[name]
		if b == nil || b.baseURL == "" || b.client == nil {
			continue
		}
		u := strings.TrimRight(b.baseURL, "/")
		if strings.Contains(u, "%ACCOUNT%") {
			acct := ""
			if len(p.Keys) > 0 {
				acct = p.Keys[0].AccountID
			}
			if acct == "" {
				continue
			}
			u = strings.ReplaceAll(u, "%ACCOUNT%", acct)
		}
		sec := p.KeepWarmSeconds
		if sec < 5 {
			sec = 5
		}
		if sec > 300 {
			sec = 300
		}
		targets = append(targets, target{b: b, url: u, interval: time.Duration(sec) * time.Second})
	}
	if len(targets) == 0 {
		s.keepWarmMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.keepWarmCancel = cancel
	s.keepWarmMu.Unlock()

	for _, t := range targets {
		t := t
		go func() {
			ticker := time.NewTicker(t.interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					reqCtx, reqCancel := context.WithTimeout(ctx, 5*time.Second)
					req, err := http.NewRequestWithContext(reqCtx, http.MethodHead, t.url, nil)
					if err == nil {
						resp, err := t.b.client.Do(req)
						if err == nil {
							_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
							_ = resp.Body.Close()
						}
					}
					reqCancel()
				}
			}
		}()
	}
}

func (s *Server) stopKeepWarm() {
	s.keepWarmMu.Lock()
	if s.keepWarmCancel != nil {
		s.keepWarmCancel()
		s.keepWarmCancel = nil
	}
	s.keepWarmMu.Unlock()
}
