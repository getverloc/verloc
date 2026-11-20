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
	b            *backend
	ks           *KeyState
	target       RouteTarget
	isPrimary    bool
	replay       reasoningContext
	attemptStart time.Time
	ctx          context.Context
	cancel       context.CancelFunc
	budgetTimer  *time.Timer
	resp         *http.Response
	err          error
	slotHeld     bool
	probeHeld    bool
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
}

type hedgeOutcome struct {
	winner          *hedgedAttempt
	attempts        int
	lastStatus      int
	nonRetryStatus  int
	nonRetry        bool
	clientCanceled  bool
	previousBackend *backend
	previousKey     *KeyState
	failedRoutes    map[int]bool
}

// runHedgedChat executes speculative hedging across the top two eligible
// routes when policy.HedgeDelayMs > 0. Route 1 is only launched if route 0 has
// not produced response headers within HedgeDelayMs; whichever attempt returns
// HTTP 200 first wins and immediately cancels the loser.
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

	type eligible struct {
		idx         int
		cand        candidateRoute
		b           *backend
		upstreamURL string
		upModel     string
	}
	var picks [2]eligible
	pickCount := 0
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
		picks[pickCount] = eligible{idx: i, cand: cand, b: b, upstreamURL: upURL, upModel: upModel}
		pickCount++
		if pickCount == 2 {
			break
		}
	}
	if pickCount < 2 {
		return out, false
	}

	type resultMsg struct {
		slot int
		att  *hedgedAttempt
	}
	results := make(chan resultMsg, 2)

	launchInto := func(slot int, e eligible) (*hedgedAttempt, bool) {
		if !e.b.route.tryAcquireSlot() {
			return nil, false
		}
		ks := e.b.pool.NextUntried(nil)
		if ks == nil {
			e.b.route.releaseSlot()
			return nil, false
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
			return nil, false
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
			b:            e.b,
			ks:           ks,
			target:       e.cand.target,
			isPrimary:    e.cand.isPrimary,
			replay:       replay,
			attemptStart: time.Now(),
			ctx:          ctx,
			cancel:       cancel,
			budgetTimer:  budgetTimer,
			slotHeld:     true,
			probeHeld:    true,
		}
		go func(a *hedgedAttempt, rq *http.Request) {
			a.resp, a.err = a.b.client.Do(rq)
			results <- resultMsg{slot: slot, att: a}
		}(att, req)
		return att, true
	}

	a0, ok0 := launchInto(0, picks[0])
	if !ok0 {
		return out, false
	}
	out.failedRoutes = make(map[int]bool, 2)
	out.attempts = 1
	out.previousBackend, out.previousKey = a0.b, a0.ks

	recordFailure := func(slot int, a *hedgedAttempt) (nonRetry bool, status int) {
		a.budgetTimer.Stop()
		a.cancel()
		a.b.route.releaseSlot()
		a.slotHeld = false
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
				return true, 499
			}
			a.b.route.Observe(lat, false)
			a.b.pool.MarkFailureStatus(a.ks, 0, 0)
			a.probeHeld = false
			rec.AttemptLog = append(rec.AttemptLog, RequestAttempt{
				Backend: a.b.name, Upstream: a.target.Upstream, Status: 502, Outcome: "upstream_error",
				LatMs: float64(lat.Nanoseconds()) / 1e6,
			})
			out.lastStatus = 502
			return false, 502
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
			return true, a.resp.StatusCode
		}
		a.b.route.Observe(lat, false)
		a.b.pool.MarkFailureStatus(a.ks, a.resp.StatusCode, retryAfterDuration(a.resp))
		a.probeHeld = false
		return false, a.resp.StatusCode
	}

	hedgeDelay := time.Duration(policy.HedgeDelayMs) * time.Millisecond
	hedgeTimer := time.NewTimer(hedgeDelay)
	defer hedgeTimer.Stop()

	var a1 *hedgedAttempt
	inFlight := 1

	select {
	case msg := <-results:
		inFlight--
		if msg.att.err == nil && msg.att.resp.StatusCode == http.StatusOK {
			msg.att.b.pool.ObserveHeaders(msg.att.ks, msg.att.resp.Header)
			out.winner = msg.att
			return out, true
		}
		if nonRetry, st := recordFailure(msg.slot, msg.att); nonRetry {
			out.nonRetry = true
			out.nonRetryStatus = st
		}
		return out, true
	case <-hedgeTimer.C:
		if time.Now().Before(deadline) && r.Context().Err() == nil {
			var ok1 bool
			a1, ok1 = launchInto(1, picks[1])
			if ok1 {
				inFlight++
				out.attempts++
			}
		}
	}

	cancelOtherAndReap := func(winnerSlot int) {
		other := a1
		if winnerSlot == 1 {
			other = a0
		}
		if other != nil {
			other.cancel()
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

	for inFlight > 0 {
		msg := <-results
		inFlight--
		if msg.att.err == nil && msg.att.resp.StatusCode == http.StatusOK {
			msg.att.b.pool.ObserveHeaders(msg.att.ks, msg.att.resp.Header)
			cancelOtherAndReap(msg.slot)
			out.winner = msg.att
			return out, true
		}
		nonRetry, st := recordFailure(msg.slot, msg.att)
		if nonRetry {
			cancelOtherAndReap(msg.slot)
			out.nonRetry = true
			out.nonRetryStatus = st
			return out, true
		}
	}

	return out, true
}
