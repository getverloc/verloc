package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func mediaGatewayTest(t *testing.T, handler http.HandlerFunc, configure func(*Config)) *Server {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	cfg := runtimeConfig(upstream.URL)
	if configure != nil {
		configure(cfg)
	}
	return runtimeTestServer(t, cfg)
}

func mediaCall(s *Server, path, body, contentType string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	switch path {
	case "/v1/embeddings":
		s.handleEmbeddings(w, req)
	case "/v1/images/generations":
		s.handleImageGenerations(w, req)
	case "/v1/images/edits":
		s.handleImageEdits(w, req)
	default:
		s.handleChat(w, req)
	}
	return w
}

func knownPricing() *bool {
	known := true
	return &known
}

func mediaRate(v float64) *float64 { return &v }

func TestEmbeddingsEndToEnd(t *testing.T) {
	var gotPath, gotModel string
	s := mediaGatewayTest(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		io.WriteString(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1]}],"model":"upstream-embed","usage":{"prompt_tokens":1000,"total_tokens":1000}}`)
	}, func(cfg *Config) {
		cfg.ModelMap["emb/test"] = "primary:upstream-embed"
		cfg.ModelMeta = map[string]*ModelMeta{"emb/test": {PricingInput: 2, PricingKnown: knownPricing()}}
	})
	w := mediaCall(s, "/v1/embeddings", `{"model":"emb/test","input":"hello"}`, "application/json")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gotPath != "/embeddings" || gotModel != "upstream-embed" {
		t.Fatalf("upstream path=%q model=%q", gotPath, gotModel)
	}
	rec := runtimeRecord(t, s)
	if !rec.OK || rec.Endpoint != "embeddings" || rec.TokIn != 1000 || rec.TokOut != 0 || !rec.UsageKnown || !rec.CostKnown {
		t.Fatalf("record wrong: %+v", rec)
	}
	if got, want := rec.Cost, 1000*2.0/1e6; got != want {
		t.Fatalf("cost=%v want %v", got, want)
	}
	if rec.Images != 0 {
		t.Fatalf("embeddings recorded image count %d", rec.Images)
	}
}

func TestImageGenerationPerImagePricing(t *testing.T) {
	s := mediaGatewayTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/images/generations" {
			t.Errorf("upstream path=%s", r.URL.Path)
		}
		io.WriteString(w, `{"created":1,"data":[{"b64_json":"aa"},{"b64_json":"bb"}],"usage":{"input_tokens":10,"output_tokens":400,"total_tokens":410}}`)
	}, func(cfg *Config) {
		cfg.ModelMap["img/test"] = "primary:upstream-image"
		cfg.ModelMeta = map[string]*ModelMeta{"img/test": {PricingImage: mediaRate(0.04), PricingKnown: knownPricing()}}
	})
	w := mediaCall(s, "/v1/images/generations", `{"model":"img/test","prompt":"a cat"}`, "application/json")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	rec := runtimeRecord(t, s)
	if !rec.OK || rec.Endpoint != "images/generations" || rec.Images != 2 || !rec.UsageKnown || !rec.CostKnown {
		t.Fatalf("record wrong: %+v", rec)
	}
	if rec.Cost != 0.08 {
		t.Fatalf("cost=%v want 0.08", rec.Cost)
	}
	if rec.TokIn != 10 || rec.TokOut != 400 {
		t.Fatalf("token usage not captured: %+v", rec)
	}
}

func TestImageGenerationUnknownPricingStaysUnknown(t *testing.T) {
	s := mediaGatewayTest(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"created":1,"data":[{"b64_json":"aa"}]}`)
	}, func(cfg *Config) {
		cfg.ModelMap["img/test"] = "primary:upstream-image"
	})
	w := mediaCall(s, "/v1/images/generations", `{"model":"img/test","prompt":"a cat"}`, "application/json")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	rec := runtimeRecord(t, s)
	if !rec.OK || rec.Images != 1 || !rec.ImagesKnown || rec.UsageKnown {
		t.Fatalf("image count must be known: %+v", rec)
	}
	if rec.CostKnown || rec.Cost != 0 {
		t.Fatalf("missing pricing must stay unknown, not free: %+v", rec)
	}
}

func TestImageEditMultipartRewritesOnlyModel(t *testing.T) {
	var gotModel string
	var gotFile []byte
	s := mediaGatewayTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/images/edits" {
			t.Errorf("upstream path=%s", r.URL.Path)
		}
		mr, err := r.MultipartReader()
		if err != nil {
			t.Errorf("upstream multipart: %v", err)
			return
		}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("part: %v", err)
				return
			}
			switch part.FormName() {
			case "model":
				v, _ := io.ReadAll(part)
				gotModel = string(v)
			case "image":
				gotFile, _ = io.ReadAll(part)
			}
		}
		io.WriteString(w, `{"created":1,"data":[{"b64_json":"aa"}]}`)
	}, func(cfg *Config) {
		cfg.ModelMap["img/test"] = "primary:upstream-image"
	})
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("model", "img/test")
	_ = mw.WriteField("prompt", "make it blue")
	fw, _ := mw.CreateFormFile("image", "in.png")
	_, _ = fw.Write([]byte("PNGDATA"))
	_ = mw.Close()
	w := mediaCall(s, "/v1/images/edits", body.String(), mw.FormDataContentType())
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gotModel != "upstream-image" {
		t.Fatalf("upstream model=%q want upstream-image", gotModel)
	}
	if string(gotFile) != "PNGDATA" {
		t.Fatalf("file bytes changed: %q", gotFile)
	}
	rec := runtimeRecord(t, s)
	if !rec.OK || rec.Images != 1 {
		t.Fatalf("record wrong: %+v", rec)
	}
}

func TestMediaOperationAllowlist(t *testing.T) {
	var calls atomic.Int32
	s := mediaGatewayTest(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{"data":[]}`)
	}, func(cfg *Config) {
		cfg.ModelMap["emb/test"] = "primary:upstream-embed"
		cfg.ModelMeta = map[string]*ModelMeta{"emb/test": {Operations: []string{"embeddings"}}}
	})
	if w := mediaCall(s, "/v1/images/generations", `{"model":"emb/test","prompt":"x"}`, "application/json"); w.Code != http.StatusNotFound {
		t.Fatalf("images status=%d want 404: %s", w.Code, w.Body.String())
	}
	if w := mediaCall(s, "/v1/chat/completions", `{"model":"emb/test","messages":[]}`, "application/json"); w.Code != http.StatusNotFound {
		t.Fatalf("chat status=%d want 404: %s", w.Code, w.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("disallowed operations reached upstream %d times", calls.Load())
	}
}

func TestImageFailureIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	s := mediaGatewayTest(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":{"message":"boom"}}`)
	}, func(cfg *Config) {
		cfg.ModelMap["img/test"] = "primary:upstream-image"
	})
	w := mediaCall(s, "/v1/images/generations", `{"model":"img/test","prompt":"x"}`, "application/json")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", w.Code)
	}
	if calls.Load() != 1 {
		t.Fatalf("ambiguous image failure retried %d times", calls.Load())
	}
	rec := runtimeRecord(t, s)
	if rec.OK || rec.CostKnown || len(rec.AttemptLog) != 1 {
		t.Fatalf("record wrong: %+v", rec)
	}
}

func TestEmbeddingsRotateKeyOn429(t *testing.T) {
	var calls atomic.Int32
	s := mediaGatewayTest(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"slow down"}}`)
			return
		}
		io.WriteString(w, `{"data":[{"embedding":[0.1]}],"usage":{"prompt_tokens":5,"total_tokens":5}}`)
	}, func(cfg *Config) {
		cfg.ModelMap["emb/test"] = "primary:upstream-embed"
		cfg.Providers["primary"].Keys = []KeyEntry{{ID: "a", Key: "k1"}, {ID: "b", Key: "k2"}}
	})
	w := mediaCall(s, "/v1/embeddings", `{"model":"emb/test","input":"hello"}`, "application/json")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	rec := runtimeRecord(t, s)
	if !rec.OK || rec.Failover != 1 || len(rec.AttemptLog) != 2 {
		t.Fatalf("expected one silent failover: %+v", rec)
	}
}

func TestMediaRejectsStreamingAndChatTransforms(t *testing.T) {
	var gotBody map[string]any
	s := mediaGatewayTest(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		io.WriteString(w, `{"data":[{"url":"https://example.com/image.png"}]}`)
	}, func(cfg *Config) {
		cfg.ModelMap["img/test"] = "primary:upstream-image"
	})
	if w := mediaCall(s, "/v1/images/generations", `{"model":"img/test","prompt":"x","stream":true}`, "application/json"); w.Code != http.StatusBadRequest {
		t.Fatalf("stream status=%d want 400", w.Code)
	}
	body := `{"model":"img/test","prompt":"x","max_tokens":7,"reasoning_effort":"high","store":true}`
	if w := mediaCall(s, "/v1/images/generations", body, "application/json"); w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gotBody["max_tokens"] != float64(7) || gotBody["reasoning_effort"] != "high" || gotBody["store"] != true {
		t.Fatalf("media body was chat-normalized: %+v", gotBody)
	}
	if gotBody["model"] != "upstream-image" {
		t.Fatalf("model not remapped: %+v", gotBody)
	}
}

func TestMediaContentTypeAndMultipartErrors(t *testing.T) {
	s := mediaGatewayTest(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be reached")
	}, func(cfg *Config) {
		cfg.ModelMap["img/test"] = "primary:upstream-image"
	})
	if w := mediaCall(s, "/v1/images/edits", "not-a-form", "multipart/form-data; boundary=xyz"); w.Code != http.StatusBadRequest {
		t.Fatalf("malformed multipart status=%d want 400", w.Code)
	}
	if w := mediaCall(s, "/v1/embeddings", `{"input":"x"}`, "application/json"); w.Code != http.StatusNotFound {
		t.Fatalf("missing model status=%d want 404", w.Code)
	}
}

func TestNormalizeOperations(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
		want []string
		bad  bool
	}{
		{name: "absent keeps nil", in: nil, want: nil},
		{name: "empty means all", in: []string{}, want: []string{}},
		{name: "dedupes and trims", in: []string{" chat ", "images", "chat"}, want: []string{"chat", "images"}},
		{name: "rejects unknown", in: []string{"audio"}, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeOperations(tc.in)
			if tc.bad {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v want %v", got, tc.want)
				}
			}
		})
	}
}

// The handler-level tests above call forwardMedia directly. This one goes
// through Handler() so the mux route, client-key middleware and alias are
// exercised together.
func TestMediaRouteThroughHandler(t *testing.T) {
	var gotPath, gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		if strings.Contains(r.URL.Path, "/images/") {
			io.WriteString(w, `{"data":[{"url":"a"}]}`)
		} else {
			io.WriteString(w, `{"data":[{"embedding":[0.1]}],"usage":{"prompt_tokens":3,"total_tokens":3}}`)
		}
	}))
	defer upstream.Close()
	e := newSecurityTestEnv(t)
	cfg := &Config{
		DefaultProvider: "mock",
		ModelMap:        map[string]string{"mock/embed": "mock:upstream-embed"},
		Providers:       map[string]*ProviderConfig{"mock": {BaseURL: upstream.URL, AllowPrivateUpstreams: true, Keys: []KeyEntry{{Key: "sk-test-key"}}}},
	}
	cfg.applyDefaults()
	cfg, err := e.store.LoadOrImportConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv.control, srv.security = e.store, e.security
	cookie, csrf := e.seedSession(t)
	key, _ := createSecurityTestKey(t, e, cookie, csrf, clientKeyRequest{Name: "media"})
	for _, path := range []string{"/v1/embeddings", "/embeddings", "/v1/images/generations", "/images/generations", "/v1/images/edits", "/images/edits"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"mock/embed","input":"x"}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
		if gotPath != strings.TrimPrefix(path, "/v1") || gotModel != "upstream-embed" {
			t.Fatalf("upstream path=%q model=%q", gotPath, gotModel)
		}
	}
	// The same route without a key must be rejected by the client middleware.
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"mock/embed","input":"x"}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous media request status=%d want 401", rec.Code)
	}
}

func TestMultipartModelRoundTripKeepsContentType(t *testing.T) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("model", "client-model")
	_ = mw.Close()
	out, contentType, err := rewriteMultipartModel(body.Bytes(), mw.FormDataContentType(), "upstream-model")
	if err != nil {
		t.Fatal(err)
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		t.Fatalf("content type=%q err=%v", contentType, err)
	}
	parsed, err := parseMediaMultipart(out, contentType)
	if err != nil || parsed.model != "upstream-model" {
		t.Fatalf("model=%q err=%v", parsed.model, err)
	}
}
