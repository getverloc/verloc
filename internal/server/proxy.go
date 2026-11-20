package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var hopByHop = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// maxSSELineBytes bounds a single SSE line. bufio.Scanner returns ErrTooLong
// past the limit, which would abort an otherwise healthy stream, so this is
// generous: providers occasionally coalesce large deltas (base64 images, big
// tool-call arguments) into one event. The buffer grows on demand, so a large
// limit costs nothing for ordinary traffic.
var balanceRe = regexp.MustCompile(`(?i)(insufficient|quota|balance|credit|rate.?limit|try again later|payment)`)

// Matches genuine client-side request errors that will fail identically on
// every key, so we must NOT fail over (doing so exhausts the whole pool).
var nonRetryableRe = regexp.MustCompile(`(?i)(context_length_exceeded|maximum context|reduce the length|invalid_request_error|context length|too long)`)

// runtimeState is swapped atomically on live config apply so in-flight
// requests keep a stable cfg + backend map for the duration of the call.
type runtimeState struct {
	cfg               *Config
	backends          map[string]*backend
	modelPrices       map[string]requestPrice
	targetPrices      map[RouteTarget]requestPrice
	mediaPrices       map[string]*MediaPriceSnapshot
	targetMediaPrices map[RouteTarget]*MediaPriceSnapshot
	cache             *responseCache
}

type requestPrice struct {
	known                bool
	input, output, cache float64
}

func (p requestPrice) cost(in, out, cache uint64) float64 {
	if !p.known {
		return 0
	}
	cache = min(cache, in)
	return (float64(in-cache)*p.input + float64(out)*p.output + float64(cache)*p.cache) / 1e6
}

func newRuntimeState(cfg *Config, backends map[string]*backend) *runtimeState {
	rt := &runtimeState{
		cfg:          cfg,
		backends:     backends,
		modelPrices:  make(map[string]requestPrice, len(cfg.ModelMap)),
		targetPrices: make(map[RouteTarget]requestPrice, len(cfg.ModelMap)),
		cache:        newResponseCache(cfg.Cache),
	}
	rt.mediaPrices = map[string]*MediaPriceSnapshot{}
	rt.targetMediaPrices = map[RouteTarget]*MediaPriceSnapshot{}
	for id, mapped := range cfg.ModelMap {
		var price requestPrice
		var image *float64
		var imagePrices *ImagePricing
		if meta := cfg.ModelMeta[id]; meta != nil {
			price = requestPrice{known: meta.PricingKnown == nil || *meta.PricingKnown, input: meta.PricingInput, output: meta.PricingOutput, cache: meta.PricingCache}
			image, imagePrices = meta.PricingImage, meta.ImagePrices
		} else if model := catalog.Lookup(id); model != nil {
			p := model.PricingPerMillion
			price = requestPrice{known: model.PricingKnown, input: p.Input, output: p.Output, cache: p.CacheRead}
			// The embedded catalog carries OpenRouter-style per-image pricing
			// as a decimal string; it is already USD per image.
			if v, err := strconv.ParseFloat(model.Pricing.Image, 64); err == nil && finitePrice(v) {
				image = &v
			}
		}
		rt.modelPrices[id] = price
		rt.mediaPrices[id] = priceSnapshot(price, image, imagePrices)
		target := primaryTarget(cfg, mapped).key()
		if previous, exists := rt.targetMediaPrices[target]; exists && !reflect.DeepEqual(previous, rt.mediaPrices[id]) {
			rt.targetMediaPrices[target] = &MediaPriceSnapshot{}
		} else if !exists {
			rt.targetMediaPrices[target] = rt.mediaPrices[id]
		}
		if previous, exists := rt.targetPrices[target]; exists && previous != price {
			// An ambiguous alias price cannot establish fallback cost coverage.
			rt.targetPrices[target] = requestPrice{}
		} else {
			rt.targetPrices[target] = price
		}
	}
	return rt
}

type Server struct {
	rt             atomic.Pointer[runtimeState]
	metrics        *Metrics
	recent         *RecentLog
	store          *Store
	hub            *Hub
	alerts         *AlertEngine
	ticker         *Ticker
	hints          *hintResolver
	reasonCache    *reasoningCache
	control        *ControlStore
	security       *Security
	shuttingDown   atomic.Bool
	routeRR        atomic.Uint64
	keepWarmMu     sync.Mutex
	keepWarmCancel context.CancelFunc
	admissionOnce  sync.Once
	admissionMu    sync.Mutex
	admissionWake  chan struct{}
	active         int
	queued         int
}

func (s *Server) runtime() *runtimeState {
	return s.rt.Load()
}

// SetTelemetry wires the analytics subsystems (store, live hub, alerts,
// recent-request log) into the server. Called once at startup after the
// store is opened; all fields are optional.
func (s *Server) SetTelemetry(store *Store, hub *Hub, alerts *AlertEngine, recent *RecentLog) {
	s.store = store
	s.hub = hub
	s.alerts = alerts
	if recent == nil {
		recent = NewRecentLog(10000)
	}
	s.recent = recent
	s.wirePoolHooks(s.runtime().backends)
}

func (s *Server) wirePoolHooks(backends map[string]*backend) {
	store, hub := s.store, s.hub
	for name, b := range backends {
		name := name
		b.pool.SetEventHook(func(key, event string, status int, msg string) {
			if store != nil {
				// Queued, never blocking: key failures can arrive in storms
				// (e.g. a fresh provider with bad keys) and must not hold
				// up request handling behind SQLite.
				store.QueueKeyEvent(time.Now().UnixMilli(), name, key, event, status, msg)
			}
			if hub.HasSubscribers() {
				hub.Publish(map[string]any{
					"type":  "key_event",
					"event": KeyEvent{TS: time.Now().UnixMilli(), Backend: name, Key: key, Event: event, Status: status, Msg: msg},
				})
			}
		})
	}
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	s.forward(w, r, "/chat/completions", nil)
}

// handleResponses implements POST /v1/responses. The client speaks the OpenAI
// Responses API; forward() translates it to chat completions, so every
// chat-capable model serves the endpoint without its backend implementing
// /responses. This mirrors handleAnthropicMessages.
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	s.forward(w, r, "/chat/completions", &responsesContext{})
}

func (s *Server) handleSystemOne(w http.ResponseWriter, r *http.Request) {
	s.forward(w, r, "/systemone", nil)
}

func isCloudflareClef(baseURL, upstream string) bool {
	if upstream != "@cf/cloudflare/clef" && upstream != "@cf/cloudflare/clef-flash" {
		return false
	}
	base := strings.TrimRight(baseURL, "/")
	return strings.HasSuffix(base, "/ai/v1") && strings.Contains(base, "/accounts/")
}

// inferenceTarget keeps native API routing separate from chat normalization.
// Account placeholders remain intact until the credential is selected.
func inferenceTarget(baseURL, apiFormat, upstream, endpoint string) (url, model string, cloudflareEnvelope bool, err error) {
	base := strings.TrimRight(baseURL, "/")
	clef := isCloudflareClef(base, upstream)
	native := apiFormat == "systemone" || clef
	if (endpoint == "/systemone") != native {
		return "", "", false, errors.New("model does not support this inference endpoint")
	}
	if clef {
		return strings.TrimSuffix(base, "/v1") + "/run/" + upstream, strings.TrimPrefix(upstream, "@cf/cloudflare/"), true, nil
	}
	return base + endpoint, upstream, false, nil
}

// Native model remapping preserves the raw JSON values of every other field,
// including exact numeric state and arbitrary typed question/image extensions.
func remapSystemOneBody(raw []byte, model string) []byte {
	if out, handled := spliceTopLevelModel(raw, model); handled {
		return out
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil || body == nil {
		return raw
	}
	body["model"], _ = json.Marshal(model)
	modified, err := json.Marshal(body)
	if err != nil {
		return raw
	}
	return modified
}

// The Workers AI REST API wraps the native result, unlike TypeSafe. Accept a
// direct native result too, but never report an API error envelope as success.
func systemOneResponse(body []byte, cloudflareEnvelope bool) ([]byte, error) {
	if cloudflareEnvelope {
		var envelope struct {
			Success *bool           `json:"success"`
			Result  json.RawMessage `json:"result"`
		}
		if json.Unmarshal(body, &envelope) != nil {
			return nil, errors.New("invalid native response")
		}
		if envelope.Success != nil || envelope.Result != nil {
			if envelope.Success == nil || !*envelope.Success || len(envelope.Result) == 0 {
				return nil, errors.New("unsuccessful native response")
			}
			body = envelope.Result
		}
	}
	var native struct {
		Model   string          `json:"model"`
		Answers json.RawMessage `json:"answers"`
	}
	if json.Unmarshal(body, &native) != nil || native.Model == "" {
		return nil, errors.New("invalid native response")
	}
	answers := bytes.TrimSpace(native.Answers)
	if len(answers) == 0 || answers[0] != '{' {
		return nil, errors.New("invalid native response")
	}
	return body, nil
}

// initGateway is safe for pure runtime constructors and direct handler tests.
func (s *Server) initGateway() {
	s.admissionOnce.Do(func() { s.admissionWake = make(chan struct{}) })
}

// Live settings and shutdown wake queued requests without replacing the gate.
func (s *Server) gatewayConfigChanged() {
	s.initGateway()
	s.admissionMu.Lock()
	close(s.admissionWake)
	s.admissionWake = make(chan struct{})
	s.admissionMu.Unlock()
}

func (s *Server) admissionLimits(fallback GatewayLimits) GatewayLimits {
	if rt := s.runtime(); rt != nil {
		fallback = rt.cfg.Limits
	}
	fallback.applyDefaults()
	return fallback
}

func (s *Server) releaseGateway() {
	s.admissionMu.Lock()
	s.active--
	if s.queued > 0 {
		close(s.admissionWake)
		s.admissionWake = make(chan struct{})
	}
	s.admissionMu.Unlock()
}

func (s *Server) acquireGateway(ctx context.Context, limits GatewayLimits) error {
	s.initGateway()
	s.admissionMu.Lock()
	limits = s.admissionLimits(limits)
	if ctx.Err() != nil {
		s.admissionMu.Unlock()
		return ctx.Err()
	}
	if s.shuttingDown.Load() {
		s.admissionMu.Unlock()
		return errors.New("gateway shutting down")
	}
	if s.active < limits.MaxInFlight {
		s.active++
		s.admissionMu.Unlock()
		return nil
	}
	if s.queued >= limits.MaxQueued {
		s.admissionMu.Unlock()
		return errors.New("gateway queue full")
	}
	s.queued++
	s.admissionMu.Unlock()
	defer func() { s.admissionMu.Lock(); s.queued--; s.admissionMu.Unlock() }()
	timer := time.NewTimer(time.Duration(limits.QueueTimeoutSeconds) * time.Second)
	defer timer.Stop()
	for {
		s.admissionMu.Lock()
		if s.shuttingDown.Load() {
			s.admissionMu.Unlock()
			return errors.New("gateway shutting down")
		}
		if ctx.Err() != nil {
			s.admissionMu.Unlock()
			return ctx.Err()
		}
		limits = s.admissionLimits(limits)
		if s.active < limits.MaxInFlight {
			s.active++
			s.admissionMu.Unlock()
			return nil
		}
		wake := s.admissionWake
		s.admissionMu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return errors.New("gateway queue timeout")
		case <-wake:
		}
	}
}

func proxyError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": message, "type": "gateway_error"}})
}

// trackingResponseWriter records whether any bytes were committed to the
// client. The panic recovery in forward needs this: before the first write a
// 500 is still deliverable, after it the only correct action is to abort the
// connection (appending JSON to a half-written body would corrupt it).
type trackingResponseWriter struct {
	http.ResponseWriter
	wrote bool
}

// Unwrap lets http.ResponseController reach SetReadDeadline/SetWriteDeadline
// and Flush on the real writer through this wrapper.
func (w *trackingResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *trackingResponseWriter) WriteHeader(status int) {
	w.wrote = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *trackingResponseWriter) Write(p []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(p)
}

// FlushError keeps the streaming path's interface check working through the
// wrapper; without it a wrapped writer silently loses per-chunk flushing.
func (w *trackingResponseWriter) FlushError() error {
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(time.Minute))
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

type requestMetadata struct {
	Model         string `json:"model"`
	Stream        bool   `json:"stream"`
	MaxTokens     uint64 `json:"max_tokens"`
	MaxCompletion uint64 `json:"max_completion_tokens"`
	MaxOutput     uint64 `json:"max_output_tokens"`
}

func tokenReservation(p requestMetadata, bodyBytes int) uint64 {
	output := max(p.MaxTokens, p.MaxCompletion, p.MaxOutput)
	if output == 0 {
		output = 32768
		if m := catalog.Lookup(p.Model); m != nil && m.MaxTokens > 0 {
			output = uint64(m.MaxTokens)
		}
	}
	input := uint64(bodyBytes) // byte count is conservative for text tokenization.
	if output > ^uint64(0)-input {
		return ^uint64(0)
	}
	return input + output
}

// Non-text and referenced inputs cannot be bounded by serialized request bytes.
// Their reservation must use the configured model context upper bound.
func nonTextInput(value any) bool {
	switch value := value.(type) {
	case nil, string:
		return false
	case []any:
		for _, part := range value {
			if nonTextInput(part) {
				return true
			}
		}
		return false
	case map[string]any:
		if content, ok := value["content"]; ok {
			return nonTextInput(content)
		}
		kind, _ := value["type"].(string)
		switch kind {
		case "text", "input_text", "output_text", "refusal", "function_call":
			return false
		case "function_call_output":
			return nonTextInput(value["output"])
		}
		return true
	default:
		return true
	}
}

// Only TPM-limited clients require normalization. Chat output reservations are
// enforced in the forwarded body; native typed APIs reserve their full context.
func prepareTPMRequest(raw []byte, metadata requestMetadata, cfg *Config, endpoint string) ([]byte, uint64, error) {
	maxOutput, contextWindow := 32768, 0
	if model := catalog.Lookup(metadata.Model); model != nil {
		if model.MaxTokens > 0 {
			maxOutput = model.MaxTokens
		}
		contextWindow = model.ContextWindow
	}
	if model := cfg.ModelMeta[metadata.Model]; model != nil {
		if model.MaxTokens > 0 {
			maxOutput = model.MaxTokens
		}
		if model.ContextWindow > 0 {
			contextWindow = model.ContextWindow
		}
	}
	if endpoint == "/systemone" {
		if contextWindow <= 0 {
			return nil, 0, errors.New("TPM-limited System One requests require a configured model context window")
		}
		// Native typed evaluation has no max-token parameter. Reserve the full
		// context plus serialized schema/state rather than injecting chat limits;
		// actual native usage reconciles the lease when the call completes.
		return raw, saturatingTokenAdd(uint64(contextWindow), uint64(len(raw))), nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		return nil, 0, err
	}
	output := uint64(maxOutput)
	if requested := max(metadata.MaxTokens, metadata.MaxCompletion, metadata.MaxOutput); requested > 0 {
		output = min(output, requested)
	}
	for _, field := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if _, present := body[field]; present {
			body[field] = output
		}
	}
	if metadata.MaxTokens == 0 && metadata.MaxCompletion == 0 {
		body["max_tokens"] = output
	}
	multimodal := nonTextInput(body["input"])
	if messages, ok := body["messages"].([]any); ok {
		for _, message := range messages {
			if message, ok := message.(map[string]any); ok && nonTextInput(message["content"]) {
				multimodal = true
			}
		}
	}
	if reference, ok := body["previous_response_id"].(string); ok && reference != "" {
		multimodal = true
	}
	if body["conversation"] != nil {
		multimodal = true
	}
	input := uint64(len(raw))
	if multimodal {
		if contextWindow <= 0 {
			return nil, 0, errors.New("TPM-limited non-text input requires a configured model context window")
		}
		input = max(input, uint64(contextWindow))
	}
	reservation := ^uint64(0)
	if output <= ^uint64(0)-input {
		reservation = input + output
	}
	bounded, err := json.Marshal(body)
	return bounded, reservation, err
}

func (s *Server) forward(w http.ResponseWriter, r *http.Request, endpoint string, adapter inboundAdapter) {
	// Wrap first so the panic recovery below can determine whether a response
	// was already committed to the client.
	tw := &trackingResponseWriter{ResponseWriter: w}
	w = tw
	start := time.Now()
	rt := s.runtime()
	limits := rt.cfg.Limits
	limits.applyDefaults()
	id := randomUUID()
	w.Header().Set("X-Request-ID", id)
	rec := ReqRecord{TS: nowMs(), ID: id, IP: clientIP(r), UA: truncate(r.Header.Get("User-Agent"), 60),
		Status: http.StatusBadRequest, Outcome: "invalid_request", Endpoint: "chat"}
	rec.Endpoint = strings.TrimPrefix(endpoint, "/")
	if adapter != nil {
		// The forwarded endpoint is chat completions; the client-facing
		// protocol is what analytics should record.
		rec.Endpoint = adapter.endpointName()
	}
	if identity := ClientIdentityFromRequest(r); identity != nil {
		rec.ClientKeyID, rec.ClientName = identity.ID, identity.Name
	}
	var lease *ClientLease
	var selectedPrice requestPrice
	var heldSlot *routeMetrics
	var heldProbePool *KeyPool
	var heldProbeKey *KeyState
	releaseHeld := func() {
		if heldSlot != nil {
			heldSlot.releaseSlot()
			heldSlot = nil
		}
		if heldProbePool != nil && heldProbeKey != nil {
			heldProbePool.ReleaseProbe(heldProbeKey)
			heldProbePool, heldProbeKey = nil, nil
		}
	}
	defer func() {
		releaseHeld()
		// A panic anywhere in forward() must not take down the process or
		// lose the request record. net/http only recovers the connection, and
		// we have no other recover on the request path (proxy_test.go covers
		// the base64/regex/decoder paths but nothing will ever be complete).
		// The tracking writer tells us whether a 5xx is still sendable: once
		// bytes are committed mid-stream we can only abort the connection.
		if p := recover(); p != nil {
			rec.OK, rec.Status, rec.Outcome = false, 500, "internal_error"
			rec.ErrMsg = "gateway internal error"
			log.Printf("[panic] request=%s model=%s recovered: %v", rec.ID, rec.Model, p)
			if tw, ok := w.(*trackingResponseWriter); ok && !tw.wrote {
				proxyError(w, 500, "gateway internal error")
			}
		}
		if !rec.OK && r.Context().Err() != nil {
			rec.OK, rec.Status, rec.Outcome, rec.ErrMsg = false, 499, "canceled", "client canceled"
		}
		rec.LatMs = float64(time.Since(start).Nanoseconds()) / 1e6
		rec.Failover = max(0, len(rec.AttemptLog)-1)
		if rec.Outcome != "cache_hit" {
			rec.CostKnown = rec.UsageKnown && selectedPrice.known
			rec.Cost = selectedPrice.cost(rec.TokIn, rec.TokOut, rec.TokCache)
		}
		if lease != nil {
			// No upstream attempt means no tokens could have been consumed.
			lease.Finish(rec.TokIn+rec.TokOut, rec.UsageKnown || len(rec.AttemptLog) == 0)
		}
		if s.metrics != nil {
			metered := rec
			if _, configured := rt.cfg.ModelMap[metered.Model]; !configured {
				metered.Model = "(unknown)"
			}
			s.metrics.RecordRequest(metered)
		}
		if s.store != nil {
			s.store.QueueRequest(rec)
		} else if s.recent != nil {
			// The in-memory ring is only the fallback source for
			// /dash/api/recent; when the analytics store is present it is the
			// authority and the ring would be a third copy of the same record
			// (plus a crypto/rand shortID) on every request.
			s.recent.Add(rec)
		}
		if s.hub.HasSubscribers() {
			s.hub.Publish(map[string]any{"type": "req", "req": rec})
		}
	}()
	fail := func(status int, outcome, message string) {
		rec.Status, rec.Outcome, rec.ErrMsg = status, outcome, message
		if r.Context().Err() == nil {
			if adapter != nil {
				adapter.errorWriter(w, status, message)
			} else {
				proxyError(w, status, message)
			}
		}
	}
	if s.shuttingDown.Load() {
		fail(503, "unavailable", "gateway shutting down")
		return
	}
	if r.Body == nil {
		fail(400, "invalid_request", "request body required")
		return
	}
	if endpoint == "/systemone" {
		if encoding := r.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
			fail(http.StatusUnsupportedMediaType, "invalid_request", "System One requests require uncompressed JSON")
			return
		}
	}
	if err := s.acquireGateway(r.Context(), limits); err != nil {
		w.Header().Set("Retry-After", "1")
		fail(503, "overloaded", "gateway capacity unavailable")
		return
	}
	defer s.releaseGateway()
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(time.Duration(limits.QueueTimeoutSeconds) * time.Second))
	r.Body = http.MaxBytesReader(tw.ResponseWriter, r.Body, limits.MaxBodyBytes)
	raw, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	_ = controller.SetReadDeadline(time.Time{})
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(413, "body_too_large", "request body too large")
		} else {
			fail(400, "invalid_request", "failed to read body")
		}
		return
	}
	if adapter != nil {
		// Inbound protocol adaptation: translate to a chat-completions body
		// once, before metadata parsing, admission accounting and any upstream
		// attempt. Everything downstream sees an ordinary chat request.
		if err := adapter.translateRequest(raw); err != nil {
			fail(400, "invalid_request", err.Error())
			return
		}
		raw = adapter.body()
	}
	metadata, bodyScan, err := parseRequestMetadata(raw)
	if err != nil {
		fail(400, "invalid_request", "invalid request JSON")
		return
	}
	rec.Model, rec.Stream = metadata.Model, metadata.Stream
	mapped, configured := rt.cfg.ModelMap[rec.Model]
	if !configured || rec.Model == "" || rt.cfg.DisabledModels[rec.Model] {
		fail(404, "model_unavailable", "unknown or disabled model")
		return
	}
	if !modelSupportsOperation(rt.cfg, rec.Model, "chat") {
		fail(404, "model_unavailable", "model does not support "+capabilityLabel(adapter))
		return
	}
	primary := primaryTarget(rt.cfg, mapped)
	if endpoint == "/systemone" && metadata.Stream {
		fail(400, "invalid_request", "System One does not support streaming")
		return
	}
	if b := rt.backends[primary.Backend]; b != nil {
		if _, _, _, err := inferenceTarget(b.baseURL, b.apiFormat, primary.Upstream, endpoint); err != nil {
			fail(400, "incompatible_endpoint", err.Error())
			return
		}
	}
	if s.security != nil {
		reservation := tokenReservation(metadata, len(raw))
		if identity := ClientIdentityFromRequest(r); identity != nil && identity.TPM > 0 {
			raw, reservation, err = prepareTPMRequest(raw, metadata, rt.cfg, endpoint)
			if err != nil {
				fail(400, "invalid_request", "TPM-limited input requires known model token bounds")
				return
			}
			bodyScan = topLevelScan{} // raw was rewritten by prepareTPMRequest
		}
		var policy *PolicyError
		lease, policy = s.security.AcquireClient(r, rec.Model, reservation)
		if policy != nil {
			if policy.RetryAfter > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(policy.RetryAfter))
			}
			fail(policy.Status, "policy_rejected", policy.Message)
			return
		}
		if lease.Identity != nil {
			rec.ClientKeyID, rec.ClientName = lease.Identity.ID, lease.Identity.Name
		}
	}

	// Opt-in deterministic response cache (disabled unless Config.Cache.Enabled).
	cacheEligible := rt.cache != nil && rt.cache.isCacheEligible(endpoint, rec.Model, rec.Stream, raw, r.Header)
	var cacheKey [32]byte
	if cacheEligible {
		cacheKey = responseCacheKey(rec.ClientKeyID, rec.Endpoint, rec.Model, primary, raw)
		if hit, ok := rt.cache.Get(cacheKey); ok {
			w.Header().Set("Content-Type", hit.contentType)
			w.Header().Set("X-Request-ID", id)
			w.Header().Set("X-Verloc-Cache", "HIT")
			w.WriteHeader(http.StatusOK)
			_, writeErr := w.Write(hit.body)
			if writeErr != nil {
				rec.Status, rec.Outcome, rec.ErrMsg = 499, "canceled", "client write failed"
				return
			}
			rec.OK, rec.Status, rec.Outcome, rec.ErrMsg = true, 200, "cache_hit", ""
			rec.Backend, rec.Upstream = "cache", primary.Upstream
			rec.Cost, rec.CostKnown = 0, true
			return
		}
	}

	// Budget applies until an attempt is ready to flush, never to the whole stream.
	deadline := time.Now().Add(time.Duration(limits.RetryBudgetSeconds) * time.Second)
	policy := modelRoutePolicy(rt.cfg, rec.Model)
	candidates := buildCandidateRoutes(primary, rt.cfg.ModelRoutes[rec.Model], rt, policy, &s.routeRR)
	rec.AttemptLog = make([]RequestAttempt, 0, limits.MaxAttempts)
	attempts := 0
	var previousBackend *backend
	var previousKey *KeyState
	lastStatus := http.StatusServiceUnavailable

	// deliver200 handles an HTTP 200 upstream response (for both hedged and
	// sequential attempts). Returns done=true when forward() should return,
	// or done=false when the caller may fail over to another attempt.
	deliver200 := func(
		resp *http.Response,
		b *backend,
		ks *KeyState,
		target RouteTarget,
		isPrimary bool,
		cloudflareEnvelope bool,
		replay reasoningContext,
		attemptStart time.Time,
		ctx context.Context,
		cancel context.CancelFunc,
		budgetTimer *time.Timer,
	) (done bool) {
		rec.Backend, rec.Upstream, rec.Key = b.name, target.Upstream, ks.maskedKey()
		selectedPrice = rt.modelPrices[rec.Model]
		if !isPrimary {
			selectedPrice = rt.targetPrices[target.key()]
		}
		attempt := RequestAttempt{Backend: b.name, Upstream: target.Upstream, Outcome: "upstream_error"}
		finishAttempt := func(status int, outcome string) {
			attempt.Status, attempt.Outcome = status, outcome
			attempt.LatMs = float64(time.Since(attemptStart).Nanoseconds()) / 1e6
			rec.AttemptLog = append(rec.AttemptLog, attempt)
		}

		if rec.Stream {
			idleTimeout := time.Duration(limits.StreamIdleTimeoutSeconds) * time.Second
			idle := newIdleReader(resp.Body, idleTimeout, cancel)
			streamWriter := &committedStreamWriter{ResponseWriter: w, response: resp, requestID: id, idle: idleTimeout,
				beforeCommit: func() error {
					if !budgetTimer.Stop() || ctx.Err() != nil {
						return errors.New("retry budget exhausted")
					}
					return nil
				}}
			var acc *sseReasoningAcc
			if !b.stripReasoning {
				acc = &sseReasoningAcc{}
			}
			absTimer := time.AfterFunc(maxPost200Duration, cancel)
			onUsage := func(i, o, c uint64) {
				rec.TokIn, rec.TokOut, rec.TokCache, rec.UsageKnown = i, o, c, true
			}
			onFirst := func() { rec.TTFTMs = float64(time.Since(start).Nanoseconds()) / 1e6 }
			var err error
			if adapter != nil {
				err = adapter.streamCopier(streamWriter, idle, onUsage, onFirst)
			} else {
				err = streamCopyWithUsage(streamWriter, idle, onUsage, onFirst, streamOptions{
					aliasReasoning: !b.stripReasoning,
					acc:            acc,
				})
			}
			budgetTimer.Stop()
			absTimer.Stop()
			idle.Close()
			cancel()
			_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
			if err == nil {
				acc.store(s.reasonCache, replay)
			} else {
				outcome := "interrupted"
				if idle.timedOut.Load() {
					outcome = "idle_timeout"
				}
				var clientErr *streamClientError
				if streamWriter.committed && errors.As(err, &clientErr) {
					outcome = "canceled"
				}
				if r.Context().Err() != nil {
					outcome = "canceled"
				}
				rec.Status, rec.Outcome, rec.ErrMsg = 502, outcome, "upstream stream interrupted"
				if outcome == "canceled" {
					rec.Status = 499
				}
				if outcome == "idle_timeout" {
					rec.Status = 504
				}
				if outcome != "canceled" {
					b.route.Observe(time.Since(attemptStart), false)
				}
				b.pool.RecordErr(ks)
				releaseHeld()
				finishAttempt(rec.Status, outcome)
				if !streamWriter.committed && r.Context().Err() == nil {
					lastStatus = 502
					return false
				}
				return true // The first client write permanently commits this route.
			}
		} else {
			if !budgetTimer.Stop() || ctx.Err() != nil {
				_ = resp.Body.Close()
				cancel()
				b.route.Observe(time.Since(attemptStart), false)
				b.pool.RecordErr(ks)
				releaseHeld()
				finishAttempt(502, "interrupted")
				lastStatus = 502
				return false
			}
			idleTimeout := time.Duration(limits.StreamIdleTimeoutSeconds) * time.Second
			idle := newIdleReader(resp.Body, idleTimeout, cancel)
			absTimer := time.AfterFunc(maxPost200Duration, cancel)
			sb, rerr := readSpillBody(io.LimitReader(idle, maxNonStreamBody+1))
			absTimer.Stop()
			_ = idle.Close()
			err := rerr
			if err == nil && (sb.size > maxNonStreamBody || !sb.validJSON()) {
				err = errors.New("incomplete upstream response")
			}
			cancel()
			var body []byte
			spilled := sb.file != nil && endpoint != "/systemone" && b.stripReasoning
			if err == nil && !spilled {
				body, err = sb.bytes()
			}
			if err == nil && endpoint == "/systemone" {
				body, err = systemOneResponse(body, cloudflareEnvelope)
			}
			if err != nil {
				sb.close()
				b.route.Observe(time.Since(attemptStart), false)
				b.pool.RecordErr(ks)
				releaseHeld()
				finishAttempt(502, "interrupted")
				lastStatus = 502
				return false
			}
			if spilled {
				rec.TokIn, rec.TokOut, rec.TokCache, rec.UsageKnown = usageFromReader(sb.reader())
			} else {
				rec.TokIn, rec.TokOut, rec.TokCache, rec.UsageKnown = usageOf(body)
				if endpoint != "/systemone" && !b.stripReasoning {
					body = rememberFromResponse(body, s.reasonCache, replay)
				}
			}
			copyResponseHeaders(w, resp, false)
			w.Header().Del("Content-Length") // Reasoning aliases can change the body length.
			w.Header().Set("X-Request-ID", id)
			if adapter != nil {
				if spilled {
					if body, err = sb.bytes(); err != nil {
						sb.close()
						b.route.Observe(time.Since(attemptStart), false)
						b.pool.RecordErr(ks)
						releaseHeld()
						finishAttempt(502, "interrupted")
						lastStatus = 502
						return false
					}
				}
				translated := adapter.translateResponse(body)
				if translated == nil {
					sb.close()
					b.route.Observe(time.Since(attemptStart), false)
					b.pool.RecordErr(ks)
					releaseHeld()
					finishAttempt(502, "upstream_error")
					fail(502, "upstream_error", "upstream response could not be translated")
					return true
				}
				body = translated
				spilled = false
				w.Header().Set("Content-Type", "application/json")
				w.Header().Del("Content-Length")
			}
			if cacheEligible && !spilled {
				w.Header().Set("X-Verloc-Cache", "MISS")
				rt.cache.Put(cacheKey, body, w.Header().Get("Content-Type"), rec.TokIn, rec.TokOut, rec.TokCache, rec.UsageKnown)
			}
			w.WriteHeader(http.StatusOK)
			if spilled {
				_, err = w.Write(sb.mem)
				if err == nil {
					_, err = io.Copy(w, io.NewSectionReader(sb.file, 0, sb.fileSize))
				}
			} else {
				_, err = w.Write(body)
			}
			sb.close()
			if err != nil {
				rec.Status, rec.Outcome, rec.ErrMsg = 499, "canceled", "client write failed"
				b.pool.RecordErr(ks)
				releaseHeld()
				finishAttempt(499, "canceled")
				return true
			}
		}

		lat := time.Since(attemptStart)
		b.route.Observe(lat, true)
		rec.OK, rec.Status, rec.Outcome, rec.ErrMsg = true, 200, "completed", ""
		rec.CostKnown = rec.UsageKnown && selectedPrice.known
		rec.Cost = selectedPrice.cost(rec.TokIn, rec.TokOut, rec.TokCache)
		b.pool.RecordSuccess(ks, rec.TokIn, rec.TokOut, rec.TokCache, rec.Cost)
		heldProbePool, heldProbeKey = nil, nil // RecordSuccess cleared probeInFlight
		releaseHeld()
		finishAttempt(200, "completed")
		return true
	}

	// Opt-in speculative hedging for chat/responses/messages when configured.
	var skipRouteIdx map[int]bool
	if endpoint != "/systemone" && policy != nil && policy.HedgeDelayMs > 0 && len(candidates) >= 2 && limits.MaxAttempts >= 2 && time.Until(deadline) > time.Duration(policy.HedgeDelayMs)*time.Millisecond {
		if ho, ran := s.runHedgedChat(r, rt, candidates, endpoint, raw, bodyScan, id, &rec, deadline, policy); ran {
			attempts += ho.attempts
			lastStatus = ho.lastStatus
			previousBackend, previousKey = ho.previousBackend, ho.previousKey
			skipRouteIdx = ho.failedRoutes
			if ho.clientCanceled {
				return
			}
			if ho.nonRetry {
				fail(ho.nonRetryStatus, "upstream_rejected", "upstream rejected request")
				return
			}
			if ho.winner != nil {
				win := ho.winner
				heldSlot = win.b.route
				heldProbePool, heldProbeKey = win.b.pool, win.ks
				if deliver200(win.resp, win.b, win.ks, win.target, win.isPrimary, false, win.replay, win.attemptStart, win.ctx, win.cancel, win.budgetTimer) {
					return
				}
			}
		}
	}

	for routeIndex, cand := range candidates {
		if skipRouteIdx != nil && skipRouteIdx[routeIndex] {
			continue
		}
		target := cand.target
		if !cand.isPrimary && !targetSupportsOperation(rt.cfg, target, "chat") {
			continue
		}
		b := rt.backends[target.Backend]
		if b == nil || target.Upstream == "" {
			continue
		}
		if p := rt.cfg.Providers[target.Backend]; p != nil && p.Disabled {
			continue
		}
		upstreamURL, upstreamModel, cloudflareEnvelope, routeErr := inferenceTarget(b.baseURL, b.apiFormat, target.Upstream, endpoint)
		if routeErr != nil {
			continue // Never convert typed evaluation into chat during failover.
		}
		var modified []byte
		var replay reasoningContext
		if endpoint == "/systemone" {
			if bodyScan.valid {
				modified = spliceFromScan(raw, bodyScan, upstreamModel, 0)
			} else {
				modified = remapSystemOneBody(raw, upstreamModel)
			}
		} else {
			modified, replay = prepareChatBody(raw, bodyScan, upstreamModel, b.name == "mistral", b.name, b.stripReasoning, b.maxTokens, s.reasonCache, reasoningScope{
				clientID: rec.ClientKeyID, model: rec.Model, provider: b.name,
				upstream: upstreamURL + "\x00" + upstreamModel,
			})
		}
		tried := make(map[credentialKey]bool, min(b.pool.Count(), limits.MaxAttempts))
		for attempts < limits.MaxAttempts && time.Now().Before(deadline) && r.Context().Err() == nil {
			// Per-provider concurrency bulkhead: if this backend is saturated,
			// fail over immediately to the next configured route.
			if !b.route.tryAcquireSlot() {
				lastStatus = http.StatusServiceUnavailable
				break
			}
			heldSlot = b.route
			ks := b.pool.NextUntried(tried)
			if ks == nil {
				releaseHeld()
				break
			}
			heldProbePool, heldProbeKey = b.pool, ks
			tried[keyIdentity(ks)] = true
			if previousKey != nil {
				previousBackend.pool.RecordFailover(previousKey)
				if s.metrics != nil {
					s.metrics.RecordFailover(rec.Model, previousBackend.name)
				}
			}
			previousBackend, previousKey = b, ks
			attempts++
			rec.Backend, rec.Upstream, rec.Key = b.name, target.Upstream, ks.maskedKey()
			selectedPrice = rt.modelPrices[rec.Model]
			if !cand.isPrimary {
				selectedPrice = rt.targetPrices[target.key()]
			}
			attemptStart := time.Now()
			ctx, cancel := context.WithCancel(r.Context())
			budgetTimer := time.AfterFunc(time.Until(deadline), cancel)
			upURL := strings.ReplaceAll(upstreamURL, "%ACCOUNT%", ks.AccountID)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, upURL, bytes.NewReader(modified))
			if err != nil {
				budgetTimer.Stop()
				cancel()
				releaseHeld()
				fail(502, "upstream_error", "invalid upstream configuration")
				return
			}
			copyRequestHeaders(req, r)
			req.Header.Set("X-Request-ID", id)
			req.Header.Set("Content-Type", "application/json")
			if !b.noAuth {
				req.Header.Set(b.authHeader, fmt.Sprintf(b.authFormat, ks.Key))
			}
			if b.synthesizeCLI {
				req.Header.Set("User-Agent", "opencode-cli/1.0.0")
				req.Header.Set("x-opencode-client", "cli")
				req.Header.Set("x-opencode-project", "default")
				req.Header.Set("x-opencode-request", randomUUID())
				req.Header.Set("x-opencode-session", randomUUID())
			}
			attempt := RequestAttempt{Backend: b.name, Upstream: target.Upstream, Outcome: "upstream_error"}
			finishAttempt := func(status int, outcome string) {
				attempt.Status, attempt.Outcome = status, outcome
				attempt.LatMs = float64(time.Since(attemptStart).Nanoseconds()) / 1e6
				rec.AttemptLog = append(rec.AttemptLog, attempt)
			}
			resp, err := b.client.Do(req)
			if err != nil {
				budgetTimer.Stop()
				cancel()
				b.pool.RecordErr(ks)
				if r.Context().Err() != nil {
					releaseHeld()
					finishAttempt(499, "canceled")
					return
				}
				b.route.Observe(time.Since(attemptStart), false)
				b.pool.MarkFailureStatus(ks, 0, 0)
				heldProbePool, heldProbeKey = nil, nil
				releaseHeld()
				finishAttempt(502, "upstream_error")
				lastStatus = 502
				continue
			}
			b.pool.ObserveHeaders(ks, resp.Header)
			lastStatus = resp.StatusCode
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
				_ = resp.Body.Close()
				budgetTimer.Stop()
				cancel()
				retryable := isRetryableStatus(resp.StatusCode) ||
					(resp.StatusCode == 400 && balanceRe.Match(body) && !nonRetryableRe.Match(body))
				b.pool.RecordErr(ks)
				finishAttempt(resp.StatusCode, "upstream_rejected")
				if !retryable {
					releaseHeld()
					fail(resp.StatusCode, "upstream_rejected", "upstream rejected request")
					return
				}
				b.route.Observe(time.Since(attemptStart), false)
				b.pool.MarkFailureStatus(ks, resp.StatusCode, retryAfterDuration(resp))
				heldProbePool, heldProbeKey = nil, nil
				releaseHeld()
				continue
			}
			if deliver200(resp, b, ks, target, cand.isPrimary, cloudflareEnvelope, replay, attemptStart, ctx, cancel, budgetTimer) {
				return
			}
		}
	}
	if !time.Now().Before(deadline) {
		fail(504, "retry_budget_exhausted", "upstream retry budget exhausted")
	} else {
		fail(lastStatus, "unavailable", "upstream unavailable")
	}
}

// remapBody rewrites the "model" field to the upstream model name. When
// `isMistral` is true it also normalises request shapes that Mistral
// rejects with 422 but that other backends (vercel, anthropic, ollama,
// nvidia, etc.) accept as-is. The full list of Mistral-specific
// normalisations is in the body below.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func copyRequestHeaders(dst *http.Request, src *http.Request) {
	connVals := src.Header.Values("Connection")
	hasConn := len(connVals) > 0
	for k, vals := range src.Header {
		if hopByHop[k] || k == "Authorization" || k == "Content-Length" || k == "Accept-Encoding" ||
			k == "Cookie" || k == "X-Api-Key" || k == "X-Csrf-Token" || (hasConn && connectionHeaderValues(connVals, k)) {
			continue
		}
		for _, v := range vals {
			dst.Header.Add(k, v)
		}
	}
}

func connectionHeaderValues(connVals []string, name string) bool {
	for _, value := range connVals {
		for _, nominated := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(nominated), name) {
				return true
			}
		}
	}
	return false
}

func copyResponseHeaders(w http.ResponseWriter, resp *http.Response, streaming bool) {
	connVals := resp.Header.Values("Connection")
	hasConn := len(connVals) > 0
	for k, vals := range resp.Header {
		if hopByHop[k] || k == "Set-Cookie" || (hasConn && connectionHeaderValues(connVals, k)) {
			continue
		}
		if streaming && k == "Content-Length" {
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	if streaming {
		w.Header().Set("X-Accel-Buffering", "no")
		w.Header().Set("Cache-Control", "no-cache")
	}
}

func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusUnauthorized, http.StatusPaymentRequired,
		http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func retryAfterDuration(resp *http.Response) time.Duration {
	if h := resp.Header.Get("Retry-After"); h != "" {
		if secs, err := strconv.ParseInt(strings.TrimSpace(h), 10, 64); err == nil && secs > 0 {
			return time.Duration(min(secs, 3600)) * time.Second
		}
		if t, err := http.ParseTime(h); err == nil {
			if d := time.Until(t); d > 0 {
				return min(d, time.Hour)
			}
		}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		var best time.Duration
		for _, key := range []string{
			"X-Ratelimit-Reset-Requests",
			"X-Ratelimit-Reset-Tokens",
			"Anthropic-Ratelimit-Requests-Reset",
			"Anthropic-Ratelimit-Tokens-Reset",
		} {
			if d := parseRateLimitReset(resp.Header.Get(key)); d > best {
				best = d
			}
		}
		if best > 0 {
			return min(best, time.Hour)
		}
	}
	return 0
}

func maskKey(k string) string {
	if len(k) <= 4 {
		return "****"
	}
	return "..." + k[len(k)-4:]
}

// randomUUID generates a version-4 UUID using crypto/rand. Used for
// X-Request-ID and x-opencode-request / x-opencode-session headers.
func randomUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return reasoningPrefix(s, n) + "..."
}

// handleMetrics exposes a live, depless JSON snapshot of all proxy activity.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	rt := s.runtime()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.metrics.Snapshot(rt.backends))
}

// usageOf supports Chat Completions, Responses, and native System One usage.
// Explicit zero usage is known; omitted token fields remain unknown.
// usageFromReader is usageOf over a stream, for spilled bodies that must
// not be materialized just to find the usage object.
