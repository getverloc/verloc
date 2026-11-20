package server

import (
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type KeyState struct {
	ID            string
	Key           string
	AccountID     string       // optional; substituted into base_url (%ACCOUNT%)
	masked        string       // precomputed maskKey(Key); immutable, safe to read concurrently
	cooldownUntil atomic.Int64 // unix nano; 0 = healthy (hard cooldown)
	softCoolUntil atomic.Int64 // unix nano; proactive RL header steering (never alters failCount/InCooldown)
	probeInFlight atomic.Bool  // half-open circuit breaker: at most one probe after cooldown expires
	failCount     atomic.Int32
	successStreak atomic.Int32 // consecutive successes since the last failure
	requests      atomic.Uint64
	ok            atomic.Uint64
	err           atomic.Uint64
	failover      atomic.Uint64
	tokIn         atomic.Uint64
	tokOut        atomic.Uint64
	tokCache      atomic.Uint64
	costBits      atomic.Uint64 // float64 bits
}

func (k *KeyState) maskedKey() string {
	if k.masked != "" {
		return k.masked
	}
	return maskKey(k.Key)
}

func (k *KeyState) addCost(c float64) {
	for {
		old := k.costBits.Load()
		if k.costBits.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+c)) {
			return
		}
	}
}

func (k *KeyState) cost() float64 { return math.Float64frombits(k.costBits.Load()) }

// KeyEventHook is called on key-pool transitions (fail → cooldown, recover).
type KeyEventHook func(key string, event string, status int, msg string)

type KeyPool struct {
	mu       sync.Mutex
	states   []*KeyState
	rr       atomic.Uint64
	cooldown time.Duration
	hookMu   sync.RWMutex
	hook     KeyEventHook
}

func NewKeyPool(keys []KeyEntry, cooldown time.Duration) *KeyPool {
	states := make([]*KeyState, len(keys))
	for i, k := range keys {
		states[i] = &KeyState{ID: k.ID, Key: k.Key, AccountID: k.AccountID, masked: maskKey(k.Key)}
	}
	return &KeyPool{states: states, cooldown: cooldown}
}

func (p *KeyPool) SetEventHook(h KeyEventHook) {
	p.hookMu.Lock()
	p.hook = h
	p.hookMu.Unlock()
}

func (p *KeyPool) emit(key, event string, status int, msg string) {
	p.hookMu.RLock()
	h := p.hook
	p.hookMu.RUnlock()
	if h != nil {
		h(key, event, status, msg)
	}
}

// tryClaimHalfOpen enforces single-probe admission for a key whose hard
// cooldown has expired while it still carries failure history. Healthy keys
// (failCount == 0 or cooldownUntil == 0) pass immediately without touching
// probeInFlight.
func tryClaimHalfOpen(s *KeyState, until int64) bool {
	if until > 0 && s.failCount.Load() > 0 {
		return s.probeInFlight.CompareAndSwap(false, true)
	}
	return true
}

// NextUntried scans the immutable pool without allocating. Duplicate
// credentials are excluded by account + key, not just pool position.
//
// Selection priority:
//  1. Healthy (or half-open probe-eligible) key with no active soft rate-limit hint.
//  2. If every untried non-hard-cooled key is soft-cooled by proactive rate-limit
//     headers, fall back to the one whose soft window resets earliest so single-key
//     or saturated pools never artificially fail with 503.
func (p *KeyPool) NextUntried(tried map[credentialKey]bool) *KeyState {
	n := len(p.states)
	if n == 0 {
		return nil
	}
	start := int((p.rr.Add(1) - 1) % uint64(n))
	now := time.Now().UnixNano()
	var softFallback *KeyState
	var softEarliest int64
	for i := range n {
		s := p.states[(start+i)%n]
		if tried != nil && tried[keyIdentity(s)] {
			continue
		}
		until := s.cooldownUntil.Load()
		if until > now {
			continue
		}
		if soft := s.softCoolUntil.Load(); soft > now {
			if softFallback == nil || soft < softEarliest {
				softFallback = s
				softEarliest = soft
			}
			continue
		}
		if !tryClaimHalfOpen(s, until) {
			continue
		}
		return s
	}
	if softFallback == nil {
		return nil
	}
	if tryClaimHalfOpen(softFallback, softFallback.cooldownUntil.Load()) {
		return softFallback
	}
	// If the earliest soft-cooled key already has a half-open probe in flight,
	// try any remaining soft-cooled untried key.
	for i := range n {
		s := p.states[(start+i)%n]
		if s == softFallback || (tried != nil && tried[keyIdentity(s)]) {
			continue
		}
		until := s.cooldownUntil.Load()
		if until > now {
			continue
		}
		if tryClaimHalfOpen(s, until) {
			return s
		}
	}
	return nil
}

// ReleaseProbe clears an active half-open probe slot when an attempt exits
// without recording success or failure (e.g. client cancel, non-retryable 4xx,
// or canceled hedge loser).
func (p *KeyPool) ReleaseProbe(s *KeyState) {
	if s != nil {
		s.probeInFlight.Store(false)
	}
}

type credentialKey struct{ account, key string }

func keyIdentity(s *KeyState) credentialKey { return credentialKey{s.AccountID, s.Key} }

func (p *KeyPool) Next() *KeyState { return p.NextUntried(nil) }

// MarkFailure puts the key into cooldown. If override > 0 it is used
// (e.g. parsed from a Retry-After header); otherwise cooldown grows with
// consecutive failures, capped at 5 minutes.
func (p *KeyPool) MarkFailure(s *KeyState, override time.Duration) {
	s.probeInFlight.Store(false)
	s.successStreak.Store(0)
	fc := s.failCount.Add(1)
	var d time.Duration
	if override > 0 {
		d = override
	} else {
		d = p.cooldown * time.Duration(min(fc, 3600))
		if d > 5*time.Minute {
			d = 5 * time.Minute
		}
	}
	if d > time.Hour {
		d = time.Hour
	}
	s.cooldownUntil.Store(time.Now().Add(d).UnixNano())
	p.emit(s.masked, "cooldown", 0, d.String())
}

// MarkFailureStatus applies error-class-aware backoff with ±10% jitter:
//   - 401/402/403 (auth / billing / forbidden): long quarantine (min 5m, up to 1h)
//     so invalid or exhausted keys stop burning retry budget every 30s.
//   - 429 / 5xx / network: explicit Retry-After / reset override when present,
//     otherwise jittered linear backoff capped at 5m.
func (p *KeyPool) MarkFailureStatus(s *KeyState, status int, override time.Duration) {
	s.probeInFlight.Store(false)
	s.successStreak.Store(0)
	fc := s.failCount.Add(1)
	var d time.Duration
	switch {
	case override > 0:
		d = override
	case status == http.StatusUnauthorized || status == http.StatusPaymentRequired || status == http.StatusForbidden:
		base := max(p.cooldown*10, 5*time.Minute)
		d = base * time.Duration(min(fc, 12))
		d = applyJitter(d)
	default:
		d = p.cooldown * time.Duration(min(fc, 3600))
		if d > 5*time.Minute {
			d = 5 * time.Minute
		}
		d = applyJitter(d)
	}
	if d > time.Hour {
		d = time.Hour
	}
	if d < 0 {
		d = 0
	}
	s.cooldownUntil.Store(time.Now().Add(d).UnixNano())
	p.emit(s.masked, "cooldown", status, d.String())
}

// applyJitter perturbs d by ±10% to prevent synchronized retry storms when
// multiple keys fail against the same upstream.
func applyJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	// Factor in [0.9, 1.1).
	factor := 0.9 + rand.Float64()*0.2
	return time.Duration(float64(d) * factor)
}

// ObserveHeaders inspects standard OpenAI / Anthropic / Groq rate-limit
// response headers and softly steers traffic away from a key whose remaining
// quota just hit 0 before the next request suffers a 429.
func (p *KeyPool) ObserveHeaders(s *KeyState, h http.Header) {
	if s == nil || len(h) == 0 {
		return
	}
	var softReset time.Duration
	exhausted := false
	seenAny := false

	checkPair := func(remKey, resetKey string) {
		rem := strings.TrimSpace(h.Get(remKey))
		if rem == "" {
			return
		}
		seenAny = true
		if rem == "0" {
			exhausted = true
			d := parseRateLimitReset(h.Get(resetKey))
			if d <= 0 {
				d = time.Second
			}
			if d > softReset {
				softReset = d
			}
		}
	}

	checkPair("X-Ratelimit-Remaining-Requests", "X-Ratelimit-Reset-Requests")
	checkPair("X-Ratelimit-Remaining-Tokens", "X-Ratelimit-Reset-Tokens")
	checkPair("Anthropic-Ratelimit-Requests-Remaining", "Anthropic-Ratelimit-Requests-Reset")
	checkPair("Anthropic-Ratelimit-Tokens-Remaining", "Anthropic-Ratelimit-Tokens-Reset")

	if exhausted {
		if softReset < 100*time.Millisecond {
			softReset = 100 * time.Millisecond
		}
		if softReset > 5*time.Minute {
			softReset = 5 * time.Minute
		}
		until := time.Now().Add(softReset).UnixNano()
		for {
			old := s.softCoolUntil.Load()
			if old >= until {
				break
			}
			if s.softCoolUntil.CompareAndSwap(old, until) {
				break
			}
		}
		return
	}
	if seenAny && s.softCoolUntil.Load() != 0 {
		s.softCoolUntil.Store(0)
	}
}

// parseRateLimitReset parses common upstream reset formats:
// Go duration ("1s", "500ms", "1m30s"), numeric seconds ("12", "1.5"),
// RFC3339 timestamp ("2026-...Z"), or HTTP-date.
func parseRateLimitReset(raw string) time.Duration {
	v := strings.TrimSpace(raw)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 && !math.IsNaN(secs) && !math.IsInf(secs, 0) {
		// Treat large values (> 1e9) as unix epoch seconds.
		if secs > 1e9 {
			d := time.Until(time.Unix(int64(secs), 0))
			if d > 0 {
				return min(d, time.Hour)
			}
			return 0
		}
		d := time.Duration(secs * float64(time.Second))
		return min(d, time.Hour)
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return min(d, time.Hour)
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		if d := time.Until(t); d > 0 {
			return min(d, time.Hour)
		}
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return min(d, time.Hour)
		}
	}
	return 0
}

// recoverSuccesses is how many consecutive successes a previously-failing
// key needs before its failure history is forgiven. One success used to
// zero failCount immediately, so a flapping key oscillated forever and
// never earned a longer cooldown — it kept re-hammering a struggling
// upstream at the minimum backoff.
const recoverSuccesses = 2

func (p *KeyPool) MarkSuccess(s *KeyState) {
	s.probeInFlight.Store(false)
	if s.failCount.Load() > 0 {
		if s.successStreak.Add(1) < recoverSuccesses {
			return // not yet trusted again; keep the failure history
		}
		s.successStreak.Store(0)
		s.failCount.Store(0)
		s.cooldownUntil.Store(0)
		p.emit(s.masked, "recover", 0, "")
	} else {
		if s.successStreak.Load() != 0 {
			s.successStreak.Store(0)
		}
		if s.cooldownUntil.Load() != 0 {
			s.cooldownUntil.Store(0)
		}
	}
}

// RecordSuccess attributes a completed request to the key. Marks the key
// healthy and accumulates usage + cost for the Keys dashboard.
func (p *KeyPool) RecordSuccess(s *KeyState, tin, tout, tcache uint64, cost float64) {
	p.MarkSuccess(s)
	s.requests.Add(1)
	s.ok.Add(1)
	if tin > 0 {
		s.tokIn.Add(tin)
	}
	if tout > 0 {
		s.tokOut.Add(tout)
	}
	if tcache > 0 {
		s.tokCache.Add(tcache)
	}
	if cost > 0 {
		s.addCost(cost)
	}
}

// RecordErr attributes a failed request to the key.
func (p *KeyPool) RecordErr(s *KeyState) {
	s.requests.Add(1)
	s.err.Add(1)
}

// RecordFailover bumps the key's failover counter (retry on another key).
func (p *KeyPool) RecordFailover(s *KeyState) {
	s.failover.Add(1)
}

func (p *KeyPool) Count() int { return len(p.states) }

// Stats returns a snapshot for health/debug endpoints.
//
// The returned slice is owned by the caller: it is retained by Snapshot's
// output map, and concurrent Snapshot calls (two dashboard polls, or a poll
// plus the ticker) would alias a shared buffer. Only the per-entry masked key
// is cached on the state, which removes the repeated maskKey work without
// handing out shared memory.
func (p *KeyPool) Stats() []KeyStat {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now().UnixNano()
	out := make([]KeyStat, 0, len(p.states))
	for _, s := range p.states {
		until := s.cooldownUntil.Load()
		out = append(out, KeyStat{
			Key:         s.masked,
			Healthy:     until <= now,
			FailCount:   s.failCount.Load(),
			CooldownSec: max(0, (until-now)/int64(time.Second)),
			Requests:    s.requests.Load(),
			OK:          s.ok.Load(),
			Err:         s.err.Load(),
			Failover:    s.failover.Load(),
			TokIn:       s.tokIn.Load(),
			TokOut:      s.tokOut.Load(),
			TokCache:    s.tokCache.Load(),
			Cost:        s.cost(),
		})
	}
	return out
}

// InCooldown counts keys currently cooling down (for alert rules).
func (p *KeyPool) InCooldown() int {
	now := time.Now().UnixNano()
	n := 0
	for _, s := range p.states {
		if s.cooldownUntil.Load() > now {
			n++
		}
	}
	return n
}

type KeyStat struct {
	Key         string  `json:"key"`
	Healthy     bool    `json:"healthy"`
	FailCount   int32   `json:"fail_count"`
	CooldownSec int64   `json:"cooldown_sec"`
	Requests    uint64  `json:"requests"`
	OK          uint64  `json:"ok"`
	Err         uint64  `json:"err"`
	Failover    uint64  `json:"failover"`
	TokIn       uint64  `json:"tok_in"`
	TokOut      uint64  `json:"tok_out"`
	TokCache    uint64  `json:"tok_cache"`
	Cost        float64 `json:"cost"`
}
