package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMediaMultipartValidation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fields   [][2]string
		parts    int
		status   int
		endpoint string
	}{
		{"streaming", [][2]string{{"model", "test"}, {"stream", "true"}}, 0, 400, "/v1/images/edits"},
		{"duplicate stream", [][2]string{{"model", "test"}, {"stream", "false"}, {"stream", "true"}}, 0, 400, "/v1/images/edits"},
		{"duplicate model", [][2]string{{"model", "test"}, {"model", "test"}}, 0, 400, "/v1/images/edits"},
		{"invalid stream", [][2]string{{"model", "test"}, {"stream", "1"}}, 0, 400, "/v1/images/edits"},
		{"metadata limit", [][2]string{{"model", "test"}, {"quality", strings.Repeat("a", 1025)}}, 0, 400, "/v1/images/edits"},
		{"part limit", [][2]string{{"model", "test"}}, 65, 400, "/v1/images/edits"},
		{"embeddings are JSON", [][2]string{{"model", "test"}}, 0, 415, "/v1/embeddings"},
		{"generations are JSON", [][2]string{{"model", "test"}}, 0, 415, "/v1/images/generations"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := mediaGatewayTest(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid request reached upstream") }, nil)
			var b bytes.Buffer
			mw := multipart.NewWriter(&b)
			for _, f := range tc.fields {
				mw.WriteField(f[0], f[1])
			}
			for range tc.parts {
				mw.WriteField("image[]", "data")
			}
			mw.Close()
			w := mediaCall(s, tc.endpoint, b.String(), mw.FormDataContentType())
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	for _, contentType := range []string{"text/plain", "application/json; invalid", "application/octet-stream"} {
		if _, err := parseMediaRequest([]byte(`{"model":"test"}`), contentType); err == nil {
			t.Fatalf("accepted %s", contentType)
		}
	}
}

func TestImageAmbiguousFailuresNeverRetry(t *testing.T) {
	for _, mode := range []string{"network", "truncated", "malformed", "sse", "500", "400"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			s := mediaGatewayTest(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch mode {
				case "network":
					c, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					c.Close()
				case "truncated":
					w.Header().Set("Content-Length", "1000")
					io.WriteString(w, `{"data":[`)
				case "malformed":
					io.WriteString(w, `not-json`)
				case "sse":
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: [DONE]\n\n")
				case "500":
					w.WriteHeader(500)
				case "400":
					w.WriteHeader(400)
					io.WriteString(w, `{"error":"insufficient balance"}`)
				}
			}, func(cfg *Config) { cfg.Providers["primary"].Keys = []KeyEntry{{Key: "first"}, {Key: "second"}} })
			w := mediaCall(s, "/v1/images/generations", `{"model":"test","prompt":"a"}`, "application/json")
			r := runtimeRecord(t, s)
			if w.Code < 400 || r.OK || r.CostKnown || calls.Load() != 1 || len(r.AttemptLog) != 1 {
				t.Fatalf("status=%d calls=%d record=%+v", w.Code, calls.Load(), r)
			}
		})
	}
}

func TestImageExplicitRejectionsRotate(t *testing.T) {
	for _, status := range []int{401, 402, 403, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			s := mediaGatewayTest(t, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(status)
					return
				}
				io.WriteString(w, `{"data":[{"url":"a"}]}`)
			}, func(cfg *Config) { cfg.Providers["primary"].Keys = []KeyEntry{{Key: "first"}, {Key: "second"}} })
			w := mediaCall(s, "/v1/images/generations", `{"model":"test"}`, "application/json")
			r := runtimeRecord(t, s)
			if w.Code != 200 || !r.OK || r.Failover != 1 || calls.Load() != 2 {
				t.Fatalf("status=%d calls=%d record=%+v", w.Code, calls.Load(), r)
			}
		})
	}
}

func TestMediaFallbackOperationCompatibility(t *testing.T) {
	for _, path := range []string{"/v1/embeddings", "/v1/chat/completions"} {
		t.Run(path, func(t *testing.T) {
			var fallbackCalls atomic.Int32
			s := mediaGatewayTest(t, func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				if strings.Contains(string(b), "image-only") {
					fallbackCalls.Add(1)
				}
				w.WriteHeader(429)
			}, func(cfg *Config) {
				p := *cfg.Providers["primary"]
				cfg.Providers["fallback"] = &p
				cfg.ModelMap["image"] = "fallback:image-only"
				cfg.ModelMeta = map[string]*ModelMeta{"image": {Operations: []string{"images"}}}
				cfg.ModelRoutes = map[string][]RouteTarget{"test": {{Backend: "fallback", Upstream: "image-only"}}}
			})
			w := mediaCall(s, path, `{"model":"test","input":"x","messages":[]}`, "application/json")
			if w.Code == 200 || fallbackCalls.Load() != 0 {
				t.Fatalf("incompatible fallback reached: status=%d calls=%d", w.Code, fallbackCalls.Load())
			}
		})
	}
}

func TestMediaUploadLimitAndCancellation(t *testing.T) {
	t.Run("upload cap", func(t *testing.T) {
		s := mediaGatewayTest(t, func(http.ResponseWriter, *http.Request) { t.Error("oversized upload reached upstream") }, func(cfg *Config) { cfg.Limits.MaxBodyBytes = 64 })
		var b bytes.Buffer
		mw := multipart.NewWriter(&b)
		mw.WriteField("model", "test")
		f, _ := mw.CreateFormFile("image", "x.png")
		io.WriteString(f, strings.Repeat("A", 128))
		mw.Close()
		w := mediaCall(s, "/v1/images/edits", b.String(), mw.FormDataContentType())
		if w.Code != 413 {
			t.Fatalf("status=%d", w.Code)
		}
		if s.active != 0 {
			t.Fatal("admission leaked")
		}
	})
	for _, mode := range []string{"canceled", "idle timeout"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			accepted := make(chan struct{})
			s := mediaGatewayTest(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				close(accepted)
				<-r.Context().Done()
			}, func(cfg *Config) {
				cfg.Limits.StreamIdleTimeoutSeconds = 1
				cfg.Providers["primary"].Keys = []KeyEntry{{Key: "first"}, {Key: "second"}}
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(`{"model":"test"}`)).WithContext(ctx)
			if mode == "canceled" {
				go func() { <-accepted; cancel() }()
			}
			w := httptest.NewRecorder()
			s.handleImageGenerations(w, req)
			r := runtimeRecord(t, s)
			if r.OK || calls.Load() != 1 || s.active != 0 || len(r.AttemptLog) != 1 {
				t.Fatalf("status=%d calls=%d record=%+v", w.Code, calls.Load(), r)
			}
			if mode == "canceled" && r.Outcome != "canceled" {
				t.Fatalf("outcome=%s", r.Outcome)
			}
		})
	}
}

type mediaFailWriter struct{ *httptest.ResponseRecorder }

func (w mediaFailWriter) Write([]byte) (int, error) { return 0, errors.New("client closed") }

func TestMediaClientFailureRetainsUpstreamExpense(t *testing.T) {
	s := mediaGatewayTest(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"data":[{"url":"a"}]}`) }, func(cfg *Config) { cfg.ModelMeta = map[string]*ModelMeta{"test": {PricingImage: mediaRate(.1)}} })
	s.metrics = NewMetrics()
	s.handleImageGenerations(mediaFailWriter{httptest.NewRecorder()}, httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(`{"model":"test"}`)))
	r := runtimeRecord(t, s)
	if r.OK || r.Outcome != "canceled" || !r.CostKnown || r.Cost != .1 || r.UsageKnown {
		t.Fatalf("upstream expense lost: %+v", r)
	}
	stats := s.runtime().backends["primary"].pool.Stats()
	if len(stats) != 1 || stats[0].Cost != .1 || stats[0].Requests != 1 || stats[0].OK != 1 || stats[0].Err != 0 || !stats[0].Healthy || r.AttemptLog[0].Status != 200 {
		t.Fatalf("client disconnect misattributed upstream completion: stats=%+v record=%+v", stats, r)
	}
	metrics := s.metrics.Snapshot(s.runtime().backends)
	backends, _ := metrics["per_backend"].(map[string]any)
	primary, _ := backends["primary"].(map[string]any)
	if primary["requests"] != uint64(1) || primary["ok"] != uint64(0) || primary["err"] != uint64(1) || primary["cost"] != .1 {
		t.Fatalf("disconnect metrics should count one failed client request and one upstream expense: %+v", primary)
	}
}

func TestMediaSpillCleanup(t *testing.T) {
	dir := t.TempDir()
	old := spillTempDir
	setSpillTempDir(dir)
	defer setSpillTempDir(old)
	for _, valid := range []bool{true, false} {
		s := mediaGatewayTest(t, func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"data":[{"b64_json":"`+strings.Repeat("A", (8<<20)+1))
			if valid {
				io.WriteString(w, `"}]}`)
			}
		}, nil)
		mediaCall(s, "/v1/images/generations", `{"model":"test"}`, "application/json")
		files, err := filepath.Glob(filepath.Join(dir, "verloc-body-*"))
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 0 {
			t.Fatalf("spill files leaked: %v", files)
		}
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
}

func TestMediaHealthyBodyOutlivesRetryBudget(t *testing.T) {
	s := mediaGatewayTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		timer := time.NewTimer(1100 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			io.WriteString(w, `{"data":[{"url":"a"}]}`)
		case <-r.Context().Done():
		}
	}, func(cfg *Config) { cfg.Limits.RetryBudgetSeconds = 1; cfg.Limits.StreamIdleTimeoutSeconds = 3 })
	w := mediaCall(s, "/v1/images/generations", `{"model":"test"}`, "application/json")
	if w.Code != 200 {
		t.Fatalf("healthy body killed: %d %s", w.Code, w.Body.String())
	}
}

func TestMediaClientPolicies(t *testing.T) {
	e := newSecurityTestEnv(t)
	cookie, csrf := e.seedSession(t)
	s := mediaGatewayTest(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"data":[{"url":"a"}]}`) }, nil)
	s.security = e.security
	key, _ := createSecurityTestKey(t, e, cookie, csrf, clientKeyRequest{Name: "media-rpm", AllowedModels: []string{"test"}, RPM: 1})
	call := func(path, body, key string) int {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		return w.Code
	}
	for _, path := range []string{"/v1/embeddings", "/embeddings", "/v1/images/generations", "/images/generations", "/v1/images/edits", "/images/edits"} {
		if got := call(path, `{"model":"test"}`, ""); got != 401 {
			t.Fatalf("anonymous %s=%d", path, got)
		}
	}
	if got := call("/v1/images/generations", `{"model":"test"}`, key); got != 200 {
		t.Fatalf("allowed=%d", got)
	}
	if got := call("/v1/images/generations", `{"model":"test"}`, key); got != 429 {
		t.Fatalf("RPM=%d", got)
	}
	blocked, _ := createSecurityTestKey(t, e, cookie, csrf, clientKeyRequest{Name: "blocked", AllowedModels: []string{"different"}})
	if got := call("/v1/images/edits", `{"model":"test"}`, blocked); got != 403 {
		t.Fatalf("allowlist=%d", got)
	}
	tpm, _ := createSecurityTestKey(t, e, cookie, csrf, clientKeyRequest{Name: "embedding-tpm", AllowedModels: []string{"test"}, TPM: 1})
	if got := call("/v1/embeddings", `{"model":"test","input":"large"}`, tpm); got != 429 {
		t.Fatalf("TPM=%d", got)
	}
}

func TestMediaClientConcurrencyReleased(t *testing.T) {
	e := newSecurityTestEnv(t)
	cookie, csrf := e.seedSession(t)
	accepted, release := make(chan struct{}), make(chan struct{})
	s := mediaGatewayTest(t, func(w http.ResponseWriter, r *http.Request) {
		close(accepted)
		<-release
		io.WriteString(w, `{"data":[{"url":"a"}]}`)
	}, nil)
	s.security = e.security
	key, _ := createSecurityTestKey(t, e, cookie, csrf, clientKeyRequest{Name: "single-media", AllowedModels: []string{"test"}, MaxConcurrent: 1})
	call := func() int {
		r := httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(`{"model":"test"}`))
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w.Code
	}
	done := make(chan int, 1)
	go func() { done <- call() }()
	<-accepted
	if got := call(); got != 429 {
		close(release)
		<-done
		t.Fatalf("concurrency=%d", got)
	}
	close(release)
	if got := <-done; got != 200 {
		t.Fatalf("first status=%d", got)
	}
	r := authenticatedClientRequest(t, e.security, key)
	lease, policy := e.security.AcquireClient(r, "test", 0)
	if policy != nil {
		t.Fatalf("concurrency leaked: %+v", policy)
	}
	lease.Finish(0, true)
}
