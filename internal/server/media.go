package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Media endpoints are OpenAI-compatible passthroughs with gateway accounting.
// They deliberately bypass chat normalization: reasoning replay, max-token
// capping and Mistral remaps do not apply to embeddings or images.
func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	s.forwardMedia(w, r, "/embeddings")
}

func (s *Server) handleImageGenerations(w http.ResponseWriter, r *http.Request) {
	s.forwardMedia(w, r, "/images/generations")
}

func (s *Server) handleImageEdits(w http.ResponseWriter, r *http.Request) {
	s.forwardMedia(w, r, "/images/edits")
}

func mediaOperation(endpoint string) string {
	if endpoint == "/embeddings" {
		return "embeddings"
	}
	return "images"
}

// modelSupportsOperation treats an empty operation list as "every standard
// operation" so existing model records keep working unchanged.
func modelSupportsOperation(cfg *Config, model, operation string) bool {
	meta := cfg.ModelMeta[model]
	if meta == nil || len(meta.Operations) == 0 {
		return true
	}
	for _, op := range meta.Operations {
		if op == operation {
			return true
		}
	}
	return false
}

// Every explicit alias of a target must allow the requested operation. An
// incompatible fallback cannot be made eligible by its public caller's flags.
func targetSupportsOperation(cfg *Config, target RouteTarget, operation string) bool {
	for id, mapping := range cfg.ModelMap {
		if primaryTarget(cfg, mapping) == target && !modelSupportsOperation(cfg, id, operation) {
			return false
		}
	}
	return true
}

type mediaRequest struct {
	model         string
	stream        bool
	multipart     bool
	contentType   string
	size, quality string
}

func parseMediaRequest(raw []byte, contentType string) (mediaRequest, error) {
	req := mediaRequest{contentType: contentType}
	if strings.TrimSpace(contentType) == "" {
		contentType = "application/json"
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return req, errors.New("invalid content type")
	}
	if mediaType == "multipart/form-data" {
		return parseMediaMultipart(raw, contentType)
	}
	if mediaType != "application/json" {
		return req, errors.New("expected application/json or multipart/form-data")
	}
	var body struct {
		Model   string `json:"model"`
		Stream  bool   `json:"stream"`
		Size    string `json:"size"`
		Quality string `json:"quality"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return req, errors.New("invalid request JSON")
	}
	req.model, req.stream = body.Model, body.Stream
	req.size, req.quality = body.Size, body.Quality
	return req, nil
}

func parseMediaMultipart(raw []byte, contentType string) (mediaRequest, error) {
	req := mediaRequest{multipart: true, contentType: contentType}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["boundary"] == "" {
		return req, errors.New("invalid multipart content type")
	}
	mr := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
	seen := map[string]bool{}
	parts := 0
	for {
		p, e := mr.NextRawPart()
		if e == io.EOF {
			break
		}
		if e != nil {
			return req, errors.New("malformed multipart form")
		}
		parts++
		if parts > 64 {
			return req, errors.New("too many multipart parts")
		}
		name := p.FormName()
		if name == "" {
			return req, errors.New("multipart field name required")
		}
		switch name {
		case "model", "stream", "size", "quality":
			if seen[name] || p.FileName() != "" {
				return req, errors.New("duplicate or invalid multipart metadata")
			}
			seen[name] = true
			b, e := io.ReadAll(io.LimitReader(p, 1025))
			if e != nil || len(b) > 1024 {
				return req, errors.New("multipart metadata too large")
			}
			v := string(b)
			switch name {
			case "model":
				req.model = v
			case "stream":
				if v != "true" && v != "false" {
					return req, errors.New("invalid stream flag")
				}
				req.stream = v == "true"
			case "size":
				req.size = v
			case "quality":
				req.quality = v
			}
		default:
			if _, e = io.Copy(io.Discard, p); e != nil {
				return req, errors.New("malformed multipart body")
			}
		}
	}
	if req.model == "" {
		return req, errors.New("multipart request is missing model")
	}
	return req, nil
}

// rewriteMultipartModel rebuilds the form with only the model field changed.
// File bytes are copied verbatim; the whole form is bounded by the request
// body cap before this runs.
func rewriteMultipartModel(raw []byte, contentType, model string) ([]byte, string, error) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["boundary"] == "" {
		return nil, "", errors.New("invalid multipart content type")
	}
	mr := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	found := false
	for {
		part, err := mr.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", err
		}
		if part.FormName() == "model" {
			found = true
			field, err := mw.CreateFormField("model")
			if err != nil {
				return nil, "", err
			}
			if _, err := field.Write([]byte(model)); err != nil {
				return nil, "", err
			}
			continue
		}
		dst, err := mw.CreatePart(part.Header)
		if err != nil {
			return nil, "", err
		}
		if _, err := io.Copy(dst, part); err != nil {
			return nil, "", err
		}
	}
	if !found {
		return nil, "", errors.New("multipart request is missing model")
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

// mediaRetryable is narrower for images: a 5xx or timeout may mean the
// provider already accepted (and billed) the generation, so only statuses
// that prove rejection before work started are retried.
func mediaRetryable(status int, operation string) bool {
	if operation == "images" {
		switch status {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusPaymentRequired, http.StatusTooManyRequests:
			return true
		}
		return false
	}
	return isRetryableStatus(status)
}

func (s *Server) forwardMedia(w http.ResponseWriter, r *http.Request, endpoint string) {
	tw := &trackingResponseWriter{ResponseWriter: w}
	w = tw
	start := time.Now()
	rt := s.runtime()
	limits := rt.cfg.Limits
	limits.applyDefaults()
	operation := mediaOperation(endpoint)
	id := randomUUID()
	w.Header().Set("X-Request-ID", id)
	rec := ReqRecord{TS: nowMs(), ID: id, IP: clientIP(r), UA: truncate(r.Header.Get("User-Agent"), 60),
		Status: http.StatusBadRequest, Outcome: "invalid_request", Endpoint: strings.TrimPrefix(endpoint, "/")}
	if identity := ClientIdentityFromRequest(r); identity != nil {
		rec.ClientKeyID, rec.ClientName = identity.ID, identity.Name
	}
	var lease *ClientLease
	var selectedPrice *MediaPriceSnapshot
	defer func() {
		if p := recover(); p != nil {
			rec.OK, rec.Status, rec.Outcome = false, 500, "internal_error"
			rec.ErrMsg = "gateway internal error"
			log.Printf("[panic] request=%s endpoint=%s recovered: %v", rec.ID, endpoint, p)
			if tw, ok := w.(*trackingResponseWriter); ok && !tw.wrote {
				proxyError(w, 500, "gateway internal error")
			}
		}
		if !rec.OK && r.Context().Err() != nil {
			rec.OK, rec.Status, rec.Outcome, rec.ErrMsg = false, 499, "canceled", "client canceled"
		}
		rec.LatMs = float64(time.Since(start).Nanoseconds()) / 1e6
		rec.Failover = max(0, len(rec.AttemptLog)-1)
		if lease != nil {
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
			s.recent.Add(rec)
		}
		if s.hub != nil {
			s.hub.Publish(map[string]any{"type": "req", "req": rec})
		}
	}()
	fail := func(status int, outcome, message string) {
		rec.Status, rec.Outcome, rec.ErrMsg = status, outcome, message
		if r.Context().Err() == nil {
			proxyError(w, status, message)
		}
	}
	if s.shuttingDown.Load() {
		fail(503, "unavailable", "gateway shutting down")
		return
	}
	if encoding := r.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		fail(415, "invalid_request", "media requests require uncompressed bodies")
		return
	}
	if r.Body == nil {
		fail(400, "invalid_request", "request body required")
		return
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
	media, err := parseMediaRequest(raw, r.Header.Get("Content-Type"))
	if err != nil {
		fail(400, "invalid_request", err.Error())
		return
	}
	rec.Model = media.model
	if media.multipart && endpoint != "/images/edits" {
		fail(415, "invalid_request", "multipart is supported only for image edits")
		return
	}
	if media.stream {
		// Streaming image generation is an SSE contract with partial previews;
		// it is not implemented yet and must not be silently downgraded.
		fail(400, "invalid_request", "streaming is not supported on this endpoint")
		return
	}
	mapped, configured := rt.cfg.ModelMap[rec.Model]
	if !configured || rec.Model == "" || rt.cfg.DisabledModels[rec.Model] {
		fail(404, "model_unavailable", "unknown or disabled model")
		return
	}
	if !modelSupportsOperation(rt.cfg, rec.Model, operation) {
		fail(404, "model_unavailable", "model does not support this endpoint")
		return
	}
	primary := primaryTarget(rt.cfg, mapped)
	if b := rt.backends[primary.Backend]; b != nil {
		if _, _, _, err := inferenceTarget(b.baseURL, b.apiFormat, primary.Upstream, endpoint); err != nil {
			fail(400, "incompatible_endpoint", err.Error())
			return
		}
	}
	if s.security != nil {
		// Embeddings consume input tokens only; image generation has no
		// defensible token bound, so it is admitted without a TPM reservation
		// rather than pretending TPM controls media spend.
		var reservation uint64
		if operation == "embeddings" {
			reservation = uint64(len(raw))
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
	deadline := time.Now().Add(time.Duration(limits.RetryBudgetSeconds) * time.Second)
	routes := rt.cfg.ModelRoutes[rec.Model]
	rec.AttemptLog = make([]RequestAttempt, 0, limits.MaxAttempts)
	attempts := 0
	var previousBackend *backend
	var previousKey *KeyState
	lastStatus := http.StatusServiceUnavailable
	for routeIndex := range len(routes) + 1 {
		target := primary
		if routeIndex > 0 {
			target = routes[routeIndex-1]
		}
		b := rt.backends[target.Backend]
		if b == nil || target.Upstream == "" {
			continue
		}
		if p := rt.cfg.Providers[target.Backend]; p != nil && p.Disabled {
			continue
		}
		if routeIndex > 0 && !targetSupportsOperation(rt.cfg, target, operation) {
			continue
		}
		upstreamURL, upstreamModel, _, routeErr := inferenceTarget(b.baseURL, b.apiFormat, target.Upstream, endpoint)
		if routeErr != nil {
			continue
		}
		modified, contentType, err := remapMediaBody(raw, media, upstreamModel)
		if err != nil {
			fail(400, "invalid_request", err.Error())
			return
		}
		tried := make(map[credentialKey]bool, min(b.pool.Count(), limits.MaxAttempts))
		for attempts < limits.MaxAttempts && time.Now().Before(deadline) && r.Context().Err() == nil {
			ks := b.pool.NextUntried(tried)
			if ks == nil {
				break
			}
			tried[keyIdentity(ks)] = true
			if previousKey != nil {
				previousBackend.pool.RecordFailover(previousKey)
				if s.metrics != nil {
					s.metrics.RecordFailover(rec.Model, previousBackend.name)
				}
			}
			previousBackend, previousKey = b, ks
			attempts++
			rec.Backend, rec.Upstream, rec.Key = b.name, target.Upstream, maskKey(ks.Key)
			selectedPrice = rt.mediaPrices[rec.Model]
			if routeIndex > 0 {
				selectedPrice = rt.targetMediaPrices[target]
			}
			if selectedPrice == nil {
				selectedPrice = &MediaPriceSnapshot{}
			}
			copyPrice := *selectedPrice
			copyPrice.Size, copyPrice.Quality = media.size, media.quality
			selectedPrice = &copyPrice
			rec.PriceSnapshot = selectedPrice
			attemptStart := time.Now()
			ctx, cancel := context.WithCancel(r.Context())
			budgetTimer := time.AfterFunc(time.Until(deadline), cancel)
			upURL := strings.ReplaceAll(upstreamURL, "%ACCOUNT%", ks.AccountID)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, upURL, bytes.NewReader(modified))
			if err != nil {
				budgetTimer.Stop()
				cancel()
				fail(502, "upstream_error", "invalid upstream configuration")
				return
			}
			copyRequestHeaders(req, r)
			req.Header.Set("X-Request-ID", id)
			req.Header.Set("Content-Type", contentType)
			if !b.noAuth {
				req.Header.Set(b.authHeader, fmt.Sprintf(b.authFormat, ks.Key))
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
					finishAttempt(499, "canceled")
					return
				}
				b.pool.MarkFailure(ks, 0)
				finishAttempt(502, "upstream_error")
				lastStatus = 502
				if operation == "images" {
					// The submission may have reached the provider and started
					// billing; retrying could pay twice.
					fail(502, "upstream_error", "upstream request failed; generation may have started")
					return
				}
				continue
			}
			lastStatus = resp.StatusCode
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
				_ = resp.Body.Close()
				budgetTimer.Stop()
				cancel()
				retryable := mediaRetryable(resp.StatusCode, operation)
				b.pool.RecordErr(ks)
				finishAttempt(resp.StatusCode, "upstream_rejected")
				if !retryable {
					fail(resp.StatusCode, "upstream_rejected", "upstream rejected request")
					return
				}
				b.pool.MarkFailure(ks, retryAfterDuration(resp))
				continue
			}
			if !budgetTimer.Stop() || ctx.Err() != nil {
				_ = resp.Body.Close()
				cancel()
				b.pool.RecordErr(ks)
				finishAttempt(502, "interrupted")
				lastStatus = 502
				if operation == "images" {
					fail(502, "upstream_error", "upstream accepted the request; generation may have started")
					return
				}
				continue
			}
			idleTimeout := time.Duration(limits.StreamIdleTimeoutSeconds) * time.Second
			idle := newIdleReader(resp.Body, idleTimeout, cancel)
			absTimer := time.AfterFunc(maxPost200Duration, cancel)
			sb, rerr := readSpillBody(io.LimitReader(idle, maxNonStreamBody+1))
			absTimer.Stop()
			_ = idle.Close()
			cancel()
			if rerr == nil && sb.size > maxNonStreamBody {
				rerr = errors.New("upstream response too large")
			}
			var usage mediaResponse
			if rerr == nil {
				usage, rerr = readMediaResponse(sb.reader(), operation)
			}
			if rerr != nil {
				sb.close()
				b.pool.RecordErr(ks)
				finishAttempt(502, "interrupted")
				lastStatus = 502
				if operation == "images" {
					fail(502, "upstream_error", "upstream response interrupted; generation may have completed")
					return
				}
				continue
			}
			rec.TokIn, rec.TokOut, rec.TokCache = usage.Input, usage.Output, usage.Cache
			rec.Images, rec.ImagesKnown, rec.UsageKnown = usage.Images, usage.ImagesKnown, usage.TokensKnown
			if usage.BreakdownKnown {
				rec.MediaTokens = &MediaTokenBreakdown{usage.TextInput, usage.ImageInput, usage.TextOutput, usage.ImageOutput}
			}
			rec.Cost, rec.CostKnown, rec.CostSource = selectedPrice.cost(usage, media, operation)
			// The upstream completed even if the client disconnects during delivery.
			b.pool.RecordSuccess(ks, rec.TokIn, rec.TokOut, rec.TokCache, rec.Cost)
			if upstreamID := resp.Header.Get("X-Request-ID"); len(upstreamID) <= 128 && !strings.ContainsAny(upstreamID, "\r\n") {
				rec.UpstreamRequestID = upstreamID
			}
			copyResponseHeaders(w, resp, false)
			w.Header().Del("Content-Length")
			w.Header().Set("X-Request-ID", id)
			w.WriteHeader(resp.StatusCode)
			_ = controller.SetWriteDeadline(time.Now().Add(time.Duration(limits.StreamIdleTimeoutSeconds) * time.Second))
			_, writeErr := io.Copy(w, sb.reader())
			sb.close()
			_ = controller.SetWriteDeadline(time.Time{})
			if writeErr != nil {
				rec.Status, rec.Outcome, rec.ErrMsg = 499, "canceled", "client write failed"
				finishAttempt(resp.StatusCode, "completed")
				return
			}
			rec.OK, rec.Status, rec.Outcome, rec.ErrMsg = true, resp.StatusCode, "completed", ""
			finishAttempt(resp.StatusCode, "completed")
			return
		}
	}
	if !time.Now().Before(deadline) {
		fail(504, "retry_budget_exhausted", "upstream retry budget exhausted")
	} else {
		fail(lastStatus, "unavailable", "upstream unavailable")
	}
}

// remapMediaBody rewrites only the model field. Chat transforms must never
// touch these payloads.
func remapMediaBody(raw []byte, media mediaRequest, upstreamModel string) ([]byte, string, error) {
	if media.multipart {
		return rewriteMultipartModel(raw, media.contentType, upstreamModel)
	}
	return remapBody(raw, upstreamModel, false, ""), "application/json", nil
}
