package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type hedgedAttempt struct {
	srv           *Server
	b             *backend
	ks            *KeyState
	target        RouteTarget
	isPrimary     bool
	replay        reasoningContext
	attemptStart  time.Time
	ctx           context.Context
	cancel        context.CancelFunc
	budgetTimer   *time.Timer
	resp          *http.Response
	err           error
	slotHeld      bool
	probeHeld     bool
	hedgeSlotHeld bool
}

func (a *hedgedAttempt) releaseHedgeSlot() {
	if a != nil && a.hedgeSlotHeld {
		a.srv.releaseHedgeSlot()
		a.hedgeSlotHeld = false
	}
}

func (a *hedgedAttempt) cleanupCanceled() {
	if a == nil {
		return
	}
	if a.budgetTimer != nil {
		a.budgetTimer.Stop()
	}
	if a.cancel != nil {
		a.cancel()
	}
	if a.resp != nil && a.resp.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(a.resp.Body, 4<<10))
		_ = a.resp.Body.Close()
	}
	if a.slotHeld {
		a.b.route.releaseSlot()
		a.slotHeld = false
	}
	if a.probeHeld {
		a.b.pool.ReleaseProbe(a.ks)
		a.probeHeld = false
	}
	a.releaseHedgeSlot()
}

type hedgeOutcome struct {
	winner          *hedgedAttempt
	attempts        int
	lastStatus      int
	nonRetryStatus  int
	nonRetryMsg     string
	nonRetry        bool
	clientCanceled  bool
	previousBackend *backend
	previousKey     *KeyState
	failedRoutes    map[int]bool
}

// runHedgedChat executes speculative hedging across up to 1 + policy.MaxHedges
// eligible routes (up to 3 total: primary + up to 2 staggered backups) when
// policy.HedgeDelayMs > 0. Each speculative backup is gated by the gateway's
// global active-hedge concurrency cap so latency spikes cannot amplify load
// without bound; whichever attempt returns HTTP 200 first wins and cancels and
// reaps all remaining attempts.
func (s *Server) runHedgedChat(
	r *http.Request,
	rt *runtimeState,
	candidates []candidateRoute,
	endpoint string,
	raw []byte,
	bodyScan topLevelScan,
	id string,
	rec *ReqRecord,
	deadline time.Time,
	policy *RoutePolicy,
) (hedgeOutcome, bool) {
	var out hedgeOutcome
	out.lastStatus = http.StatusServiceUnavailable

	limits := rt.cfg.Limits
	limits.applyDefaults()

	maxHedges := policy.MaxHedges
	if maxHedges <= 0 {
		maxHedges = 1
	}
	if maxHedges > 2 {
		maxHedges = 2
	}
	maxPicks := 1 + maxHedges
	if limits.MaxAttempts > 0 && maxPicks > limits.MaxAttempts {
		maxPicks = limits.MaxAttempts
	}
	if maxPicks < 2 {
		return out, false
	}

	type eligible struct {
		idx         int
		cand        candidateRoute
		b           *backend
		upstreamURL string
		upModel     string
	}
	picks := make([]eligible, 0, maxPicks)
	for i, cand := range candidates {
		if !cand.isPrimary && !targetSupportsOperation(rt.cfg, cand.target, "chat") {
			continue
		}
		b := rt.backends[cand.target.Backend]
		if b == nil || cand.target.Upstream == "" {
			continue
		}
		if p := rt.cfg.Providers[cand.target.Backend]; p != nil && p.Disabled {
			continue
		}
		upURL, upModel, _, routeErr := inferenceTarget(b.baseURL, b.apiFormat, cand.target.Upstream, endpoint)
		if routeErr != nil {
			continue
		}
		picks = append(picks, eligible{idx: i, cand: cand, b: b, upstreamURL: upURL, upModel: upModel})
		if len(picks) == maxPicks {
			break
		}
	}
	if len(picks) < 2 {
		return out, false
	}

	type resultMsg struct {
		slot int
		att  *hedgedAttempt
	}
	results := make(chan resultMsg, len(picks))
	launched := make([]*hedgedAttempt, len(picks))

	launchInto := func(slot int, e eligible) (*hedgedAttempt, bool, bool) {
		isHedge := slot > 0
		if isHedge && !s.tryAcquireHedgeSlot(limits) {
			return nil, false, true
		}
		if !e.b.route.tryAcquireSlot() {
			if isHedge {
				s.releaseHedgeSlot()
			}
			return nil, false, false
		}
		ks := e.b.pool.NextUntried(nil)
		if ks == nil {
			e.b.route.releaseSlot()
			if isHedge {
				s.releaseHedgeSlot()
			}
			return nil, false, false
		}
		modified, replay := prepareChatBody(raw, bodyScan, e.upModel, e.b.name == "mistral", e.b.name, e.b.stripReasoning, e.b.maxTokens, s.reasonCache, reasoningScope{
			clientID: rec.ClientKeyID, model: rec.Model, provider: e.b.name,
			upstream: e.upstreamURL + "\x00" + e.upModel,
		})
		ctx, cancel := context.WithCancel(r.Context())
		budgetTimer := time.AfterFunc(time.Until(deadline), cancel)
		upURL := strings.ReplaceAll(e.upstreamURL, "%ACCOUNT%", ks.AccountID)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, upURL, bytes.NewReader(modified))
		if err != nil {
			budgetTimer.Stop()
			cancel()
			e.b.pool.ReleaseProbe(ks)
			e.b.route.releaseSlot()
			if isHedge {
				s.releaseHedgeSlot()
			}
			return nil, false, false
		}
		copyRequestHeaders(req, r)
		req.Header.Set("X-Request-ID", id)
		req.Header.Set("Content-Type", "application/json")
		if !e.b.noAuth {
			req.Header.Set(e.b.authHeader, fmt.Sprintf(e.b.authFormat, ks.Key))
		}
		if e.b.synthesizeCLI {
			req.Header.Set("User-Agent", "opencode-cli/1.0.0")
			req.Header.Set("x-opencode-client", "cli")
			req.Header.Set("x-opencode-project", "default")
			req.Header.Set("x-opencode-request", randomUUID())
			req.Header.Set("x-opencode-session", randomUUID())
		}
		att := &hedgedAttempt{
			srv:           s,
			b:             e.b,
			ks:            ks,
			target:        e.cand.target,
			isPrimary:     e.cand.isPrimary,
			replay:        replay,
			attemptStart:  time.Now(),
			ctx:           ctx,
			cancel:        cancel,
			budgetTimer:   budgetTimer,
			slotHeld:      true,
			probeHeld:     true,
			hedgeSlotHeld: isHedge,
		}
		launched[slot] = att
		go func(a *hedgedAttempt, rq *http.Request) {
			a.resp, a.err = a.b.client.Do(rq)
			results <- resultMsg{slot: slot, att: a}
		}(att, req)
		return att, true, false
	}

	a0, ok0, _ := launchInto(0, picks[0])
	if !ok0 {
		return out, false
	}
	out.failedRoutes = make(map[int]bool, len(picks))
	out.attempts = 1
	out.previousBackend, out.previousKey = a0.b, a0.ks

	recordFailure := func(slot int, a *hedgedAttempt) (nonRetry bool, status int, clientMsg string) {
		a.budgetTimer.Stop()
		a.cancel()
		a.b.route.releaseSlot()
		a.slotHeld = false
		a.releaseHedgeSlot()
		out.failedRoutes[picks[slot].idx] = true
		lat := time.Since(a.attemptStart)
		if a.err != nil {
			a.b.pool.RecordErr(a.ks)
			if r.Context().Err() != nil {
				a.b.pool.ReleaseProbe(a.ks)
				a.probeHeld = false
				rec.AttemptLog = append(rec.AttemptLog, RequestAttempt{
					Backend: a.b.name, Upstream: a.target.Upstream, Status: 499, Outcome: "canceled",
					LatMs: float64(lat.Nanoseconds()) / 1e6,
				})
				out.clientCanceled = true
				return true, 499, ""
			}
			a.b.route.Observe(lat, false)
			a.b.pool.MarkFailureStatus(a.ks, 0, 0)
			a.probeHeld = false
			rec.AttemptLog = append(rec.AttemptLog, RequestAttempt{
				Backend: a.b.name, Upstream: a.target.Upstream, Status: 502, Outcome: "upstream_error",
				LatMs: float64(lat.Nanoseconds()) / 1e6,
			})
			out.lastStatus = 502
			return false, 502, ""
		}
		out.lastStatus = a.resp.StatusCode
		a.b.pool.ObserveHeaders(a.ks, a.resp.Header)
		body, _ := io.ReadAll(io.LimitReader(a.resp.Body, 64<<10))
		_ = a.resp.Body.Close()
		retryable := isRetryableStatus(a.resp.StatusCode) ||
			(a.resp.StatusCode == 400 && balanceRe.Match(body) && !nonRetryableRe.Match(body))
		a.b.pool.RecordErr(a.ks)
		rec.AttemptLog = append(rec.AttemptLog, RequestAttempt{
			Backend: a.b.name, Upstream: a.target.Upstream, Status: a.resp.StatusCode, Outcome: "upstream_rejected",
			LatMs: float64(lat.Nanoseconds()) / 1e6,
		})
		if !retryable {
			a.b.pool.ReleaseProbe(a.ks)
			a.probeHeld = false
			return true, a.resp.StatusCode, safeUpstreamErrorMessage(a.resp.StatusCode, body, a.ks.Key, a.ks.AccountID)
		}
		a.b.route.Observe(lat, false)
		a.b.pool.MarkFailureStatus(a.ks, a.resp.StatusCode, retryAfterDuration(a.resp))
		a.probeHeld = false
		return false, a.resp.StatusCode, ""
	}

	hedgeDelay := time.Duration(policy.HedgeDelayMs) * time.Millisecond
	var hedgeTimer *time.Timer
	var hedgeCh <-chan time.Time
	armHedgeTimer := func(d time.Duration) {
		if hedgeTimer == nil {
			hedgeTimer = time.NewTimer(d)
		} else {
			if !hedgeTimer.Stop() {
				select {
				case <-hedgeTimer.C:
				default:
				}
			}
			hedgeTimer.Reset(d)
		}
		hedgeCh = hedgeTimer.C
	}
	stopHedgeTimer := func() {
		if hedgeTimer != nil {
			hedgeTimer.Stop()
		}
		hedgeCh = nil
	}
	defer stopHedgeTimer()

	inFlight := 1
	nextSlot := 1
	hedgeCapHit := false

	cancelOthersAndReap := func(winnerSlot int) {
		stopHedgeTimer()
		for i, att := range launched {
			if i != winnerSlot && att != nil {
				att.cancel()
			}
		}
		if inFlight > 0 {
			go func(rem int) {
				for range rem {
					msg := <-results
					msg.att.cleanupCanceled()
				}
			}(inFlight)
		}
	}

	launchNextHedge := func() bool {
		for nextSlot < len(picks) {
			slot := nextSlot
			nextSlot++
			att, ok, capped := launchInto(slot, picks[slot])
			if capped {
				hedgeCapHit = true
				return false
			}
			if ok {
				inFlight++
				out.attempts++
				out.previousBackend, out.previousKey = att.b, att.ks
				return true
			}
		}
		return false
	}

	armHedgeTimer(hedgeDelay)

	for inFlight > 0 {
		select {
		case msg := <-results:
			inFlight--
			// Once an attempt finishes its race it is no longer a speculative
			// duplicate, so release the global hedge slot before delivering.
			msg.att.releaseHedgeSlot()
			if msg.att.err == nil && msg.att.resp.StatusCode == http.StatusOK {
				msg.att.b.pool.ObserveHeaders(msg.att.ks, msg.att.resp.Header)
				cancelOthersAndReap(msg.slot)
				out.winner = msg.att
				return out, true
			}
			nonRetry, st, clientMsg := recordFailure(msg.slot, msg.att)
			if nonRetry {
				cancelOthersAndReap(msg.slot)
				out.nonRetry = true
				out.nonRetryStatus = st
				out.nonRetryMsg = clientMsg
				return out, true
			}
			// If no other hedged attempt is currently in flight, return so
			// forward()'s sequential failover loop immediately tries the next
			// untried route with full multi-key rotation (without waiting for
			// hedgeDelay).
			if inFlight == 0 {
				return out, true
			}
			// Another attempt is still slow and we just lost a speculative
			// contender: if another hedge candidate remains under the cap,
			// launch it immediately rather than waiting another full delay.
			if nextSlot < len(picks) && !hedgeCapHit && time.Now().Before(deadline) && r.Context().Err() == nil {
				if launchNextHedge() && nextSlot < len(picks) && !hedgeCapHit {
					armHedgeTimer(hedgeDelay)
				} else {
					stopHedgeTimer()
				}
			}
		case <-hedgeCh:
			hedgeCh = nil
			if !hedgeCapHit && nextSlot < len(picks) && time.Now().Before(deadline) && r.Context().Err() == nil {
				if launchNextHedge() && nextSlot < len(picks) && !hedgeCapHit {
					armHedgeTimer(hedgeDelay)
				}
			}
		}
	}

	return out, true
}
