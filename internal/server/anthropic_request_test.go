package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAnthropicImagesRejectUnsupportedRoles(t *testing.T) {
	for _, role := range []string{"assistant", "tool", "system"} {
		t.Run(role, func(t *testing.T) {
			s := anthropicGatewayTest(t, func(http.ResponseWriter, *http.Request) { t.Error("unsupported image role reached upstream") })
			body := `{"model":"nv/test","max_tokens":10,"messages":[{"role":"` + role + `","content":[{"type":"image","source":{"type":"url","url":"https://images.invalid/a.png"}}]}]}`
			assertAnthropicRequestError(t, anthropicCall(s, body, false), 400, "invalid_request_error")
		})
	}
}

func TestAnthropicImageSources(t *testing.T) {
	body := []byte(`{"model":"test","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"before"},{"type":"image","source":{"type":"url","url":"https://images.invalid/a.png?x=1"}},{"type":"text","text":"between"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1n"}},{"type":"text","text":"after"}]}]}`)
	chat, _, err := anthropicToOpenAI(body)
	if err != nil {
		t.Fatal(err)
	}
	want := []any{
		map[string]any{"type": "text", "text": "before"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://images.invalid/a.png?x=1"}},
		map[string]any{"type": "text", "text": "between"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,aW1n"}},
		map[string]any{"type": "text", "text": "after"},
	}
	messages := chat["messages"].([]any)
	if len(messages) != 1 || !reflect.DeepEqual(messages[0].(map[string]any)["content"], want) {
		t.Fatalf("ordered image content lost: %#v", messages)
	}
}

func TestAnthropicToolResultImagesPreserveAssociationAndOrder(t *testing.T) {
	body := []byte(`{"model":"test","max_tokens":10,"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"call_a","name":"first","input":{}},{"type":"tool_use","id":"call_b","name":"second","input":{}}]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"call_a","content":[{"type":"text","text":"a-before"},{"type":"image","source":{"type":"url","url":"https://images.invalid/a.png"}},{"type":"text","text":"a-between"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YQ=="}},{"type":"text","text":"a-after"}]},
			{"type":"tool_result","tool_use_id":"call_b","content":[{"type":"image","source":{"type":"url","url":"https://images.invalid/b.png"}},{"type":"text","text":"b-after"}]},
			{"type":"text","text":"user-after"}
		]}
	]}`)
	chat, _, err := anthropicToOpenAI(body)
	if err != nil {
		t.Fatal(err)
	}
	messages := chat["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("expected assistant, two tools, then user: %#v", messages)
	}
	calls := messages[0].(map[string]any)["tool_calls"].([]any)
	for i, id := range []string{"call_a", "call_b"} {
		tool := messages[i+1].(map[string]any)
		if calls[i].(map[string]any)["id"] != id || tool["role"] != "tool" || tool["tool_call_id"] != id {
			t.Fatalf("tool association lost: %#v", messages)
		}
		if _, ok := tool["content"].(string); !ok {
			t.Fatalf("unsupported multimodal tool content: %#v", tool)
		}
	}
	if messages[1].(map[string]any)["content"] != "a-beforea-betweena-after" || messages[2].(map[string]any)["content"] != "b-after" {
		t.Fatalf("tool text lost or mixed: %#v", messages)
	}
	user := messages[3].(map[string]any)
	if user["role"] != "user" {
		t.Fatalf("images must have user role: %#v", user)
	}
	var sequence []string
	for _, item := range user["content"].([]any) {
		part := item.(map[string]any)
		if part["type"] == "text" {
			sequence = append(sequence, part["text"].(string))
		} else {
			sequence = append(sequence, part["image_url"].(map[string]any)["url"].(string))
		}
	}
	want := []string{"Tool result for tool_call_id call_a:", "a-before", "https://images.invalid/a.png", "a-between", "data:image/png;base64,YQ==", "a-after", "End tool result for tool_call_id call_a.", "Tool result for tool_call_id call_b:", "https://images.invalid/b.png", "b-after", "End tool result for tool_call_id call_b.", "user-after"}
	if !reflect.DeepEqual(sequence, want) {
		t.Fatalf("image/text order or call boundaries lost:\n got %#v\nwant %#v", sequence, want)
	}
}

func TestAnthropicInvalidImagesReturnClientError(t *testing.T) {
	for _, image := range []string{
		`{"type":"image"}`,
		`{"type":"image","source":{"type":"url"}}`,
		`{"type":"image","source":{"type":"url","url":"file:///tmp/image"}}`,
		`{"type":"image","source":{"type":"url","url":"/relative.png"}}`,
		`{"type":"image","source":{"type":"url","url":"https://:80/image.png"}}`,
		`{"type":"image","source":{"type":"base64","media_type":"image/png"}}`,
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"not base64!"}}`,
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"\n"}}`,
		`{"type":"image","source":{"type":"base64","media_type":"application/pdf","data":"YQ=="}}`,
		`{"type":"image","source":{"type":"unknown","data":"YQ=="}}`,
	} {
		for _, nested := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/nested=%t", image, nested), func(t *testing.T) {
				content := image
				if nested {
					content = `{"type":"tool_result","tool_use_id":"call_a","content":[` + image + `]}`
				}
				s := anthropicGatewayTest(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid image reached upstream") })
				w := anthropicCall(s, `{"model":"nv/test","max_tokens":10,"messages":[{"role":"user","content":[`+content+`]}]}`, false)
				assertAnthropicRequestError(t, w, http.StatusBadRequest, "invalid_request_error")
			})
		}
	}
}

const anthropicCountBody = `{"model":"test","messages":[{"role":"user","content":"hello"}]}`

func anthropicCountServer(t *testing.T, policy clientKeyRequest, limits GatewayLimits) (*Server, *securityTestEnv, string, ClientKeyView) {
	t.Helper()
	e := newSecurityTestEnv(t)
	cookie, csrf := e.seedSession(t)
	key, view := createSecurityTestKey(t, e, cookie, csrf, policy)
	cfg := runtimeConfig("http://127.0.0.1:1")
	cfg.Limits = limits
	s := runtimeTestServer(t, cfg)
	s.security = e.security
	return s, e, key, view
}

func anthropicCountRequest(t *testing.T, s *Server, key string, body io.ReadCloser) *http.Request {
	t.Helper()
	r := authenticatedClientRequest(t, s.security, key)
	r.Body = body
	return r
}

func anthropicCountCall(t *testing.T, s *Server, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleAnthropicCountTokens(w, anthropicCountRequest(t, s, key, io.NopCloser(strings.NewReader(body))))
	return w
}

func assertAnthropicRequestError(t *testing.T, w *httptest.ResponseRecorder, status int, kind string) {
	t.Helper()
	var response struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if w.Code != status || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Type != "error" || response.Error.Type != kind {
		t.Fatalf("status=%d want=%d kind=%s body=%s", w.Code, status, kind, w.Body.String())
	}
}

type anthropicSlowBody struct {
	started chan struct{}
	release chan struct{}
	closed  chan struct{}
	read    atomic.Bool
	once    sync.Once
	reader  *strings.Reader
}

func newAnthropicSlowBody(t *testing.T) *anthropicSlowBody {
	t.Helper()
	b := &anthropicSlowBody{started: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{}), reader: strings.NewReader(anthropicCountBody)}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func (b *anthropicSlowBody) Read(p []byte) (int, error) {
	if b.read.CompareAndSwap(false, true) {
		close(b.started)
	}
	select {
	case <-b.release:
		return b.reader.Read(p)
	case <-b.closed:
		return 0, io.ErrClosedPipe
	}
}

func (b *anthropicSlowBody) Close() error { b.once.Do(func() { close(b.closed) }); return nil }

func awaitAnthropicSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not make progress")
	}
}

func TestAnthropicCountPreBodyKeyConcurrencyAndRPM(t *testing.T) {
	s, _, key, _ := anthropicCountServer(t, clientKeyRequest{Name: "limited", MaxConcurrent: 1, RPM: 2, TPM: 1}, GatewayLimits{})
	firstBody := newAnthropicSlowBody(t)
	first := anthropicCountRequest(t, s, key, firstBody)
	firstW := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); s.handleAnthropicCountTokens(firstW, first) }()
	awaitAnthropicSignal(t, firstBody.started)
	secondBody := newAnthropicSlowBody(t)
	secondW := httptest.NewRecorder()
	s.handleAnthropicCountTokens(secondW, anthropicCountRequest(t, s, key, secondBody))
	assertAnthropicRequestError(t, secondW, 429, "rate_limit_error")
	if secondW.Header().Get("Retry-After") == "" || secondBody.read.Load() {
		t.Fatal("concurrent request read body or omitted retry interval")
	}
	close(firstBody.release)
	awaitAnthropicSignal(t, done)
	if firstW.Code != 200 {
		t.Fatalf("first count: %d %s", firstW.Code, firstW.Body.String())
	}
	if w := anthropicCountCall(t, s, key, anthropicCountBody); w.Code != 200 {
		t.Fatalf("released lease or local TPM exemption failed: %d %s", w.Code, w.Body.String())
	}
	w := anthropicCountCall(t, s, key, anthropicCountBody)
	assertAnthropicRequestError(t, w, 429, "rate_limit_error")
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("RPM response omitted Retry-After")
	}
	if records := s.recent.Snapshot(10); len(records) != 0 {
		t.Fatalf("local estimate recorded inference/cost: %#v", records)
	}
}

func TestAnthropicCountDoesNotSpendUpstreamTPM(t *testing.T) {
	s, _, key, _ := anthropicCountServer(t, clientKeyRequest{Name: "tokens", MaxConcurrent: 1, TPM: 1}, GatewayLimits{})
	for range 2 {
		if w := anthropicCountCall(t, s, key, anthropicCountBody); w.Code != 200 {
			t.Fatalf("local count: %d %s", w.Code, w.Body.String())
		}
	}
	lease, policy := s.security.AcquireClient(authenticatedClientRequest(t, s.security, key), "test", 1)
	if policy != nil {
		t.Fatalf("local estimates spent upstream tokens: %v", policy)
	}
	defer lease.Finish(1, true)
}

func TestAnthropicCountGlobalConcurrencyAndQueuedCancellation(t *testing.T) {
	s, _, key, _ := anthropicCountServer(t, clientKeyRequest{Name: "global"}, GatewayLimits{MaxInFlight: 1, MaxQueued: 1})
	firstBody := newAnthropicSlowBody(t)
	firstW := httptest.NewRecorder()
	first := anthropicCountRequest(t, s, key, firstBody)
	firstDone := make(chan struct{})
	go func() { defer close(firstDone); s.handleAnthropicCountTokens(firstW, first) }()
	awaitAnthropicSignal(t, firstBody.started)
	secondBody := newAnthropicSlowBody(t)
	second := anthropicCountRequest(t, s, key, secondBody)
	ctx, cancel := context.WithCancel(second.Context())
	defer cancel()
	second = second.WithContext(ctx)
	secondW := httptest.NewRecorder()
	secondDone := make(chan struct{})
	go func() { defer close(secondDone); s.handleAnthropicCountTokens(secondW, second) }()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		s.admissionMu.Lock()
		queued := s.queued
		s.admissionMu.Unlock()
		if queued == 1 {
			break
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("second request never queued")
		}
	}
	if secondBody.read.Load() {
		t.Fatal("queued count read its body before a global slot")
	}
	thirdBody := newAnthropicSlowBody(t)
	thirdW := httptest.NewRecorder()
	s.handleAnthropicCountTokens(thirdW, anthropicCountRequest(t, s, key, thirdBody))
	assertAnthropicRequestError(t, thirdW, 503, "api_error")
	if thirdW.Header().Get("Retry-After") != "1" || thirdBody.read.Load() {
		t.Fatal("full gateway read body or omitted retry interval")
	}
	cancel()
	awaitAnthropicSignal(t, secondDone)
	if secondBody.read.Load() {
		t.Fatal("canceled queued request read body")
	}
	close(firstBody.release)
	awaitAnthropicSignal(t, firstDone)
	if firstW.Code != 200 {
		t.Fatalf("first count: %s", firstW.Body.String())
	}
	if w := anthropicCountCall(t, s, key, anthropicCountBody); w.Code != 200 {
		t.Fatalf("global slot leaked: %d %s", w.Code, w.Body.String())
	}
}

func TestAnthropicCountCancellationReleasesLeaseAndBody(t *testing.T) {
	s, _, key, _ := anthropicCountServer(t, clientKeyRequest{Name: "cancel", MaxConcurrent: 1}, GatewayLimits{MaxInFlight: 1})
	body := newAnthropicSlowBody(t)
	r := anthropicCountRequest(t, s, key, body)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	r = r.WithContext(ctx)
	done := make(chan struct{})
	go func() { defer close(done); s.handleAnthropicCountTokens(httptest.NewRecorder(), r) }()
	awaitAnthropicSignal(t, body.started)
	cancel()
	awaitAnthropicSignal(t, done)
	awaitAnthropicSignal(t, body.closed)
	if w := anthropicCountCall(t, s, key, anthropicCountBody); w.Code != 200 {
		t.Fatalf("canceled request leaked admission: %d %s", w.Code, w.Body.String())
	}
}

func TestAnthropicCountValidationAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		kind       string
	}{
		{"json", `{`, 400, "invalid_request_error"},
		{"trailing json", anthropicCountBody + `{}`, 400, "invalid_request_error"},
		{"missing model", `{"messages":[{"role":"user","content":"x"}]}`, 400, "invalid_request_error"},
		{"missing messages", `{"model":"test"}`, 400, "invalid_request_error"},
		{"unknown model", `{"model":"unknown","messages":[{"role":"user","content":"x"}]}`, 404, "not_found_error"},
		{"image", `{"model":"test","messages":[{"role":"user","content":[{"type":"image"}]}]}`, 400, "invalid_request_error"},
		{"oversized", `{"model":"test","messages":[{"role":"user","content":"` + strings.Repeat("x", 1024) + `"}]}`, 413, "request_too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, key, _ := anthropicCountServer(t, clientKeyRequest{Name: "validation", MaxConcurrent: 1}, GatewayLimits{MaxInFlight: 1, MaxBodyBytes: 512})
			assertAnthropicRequestError(t, anthropicCountCall(t, s, key, tc.body), tc.status, tc.kind)
			if w := anthropicCountCall(t, s, key, anthropicCountBody); w.Code != 200 {
				t.Fatalf("error leaked admission: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestAnthropicCountUsesFreshModelScope(t *testing.T) {
	s, e, key, view := anthropicCountServer(t, clientKeyRequest{Name: "scope", MaxConcurrent: 1, AllowedModels: []string{"test"}}, GatewayLimits{MaxInFlight: 1})
	r := anthropicCountRequest(t, s, key, io.NopCloser(strings.NewReader(anthropicCountBody)))
	cookie, csrf := e.seedSession(t)
	policy := clientKeyRequest{Name: "scope", MaxConcurrent: 1, AllowedModels: []string{"other"}}
	if w := e.request("PUT", "/dash/api/admin/client-keys/"+view.ID, policy, cookie, csrf); w.Code != 200 {
		t.Fatalf("edit key: %d %s", w.Code, w.Body.String())
	}
	w := httptest.NewRecorder()
	s.handleAnthropicCountTokens(w, r)
	assertAnthropicRequestError(t, w, 403, "permission_error")
	policy.AllowedModels = []string{"test"}
	if w := e.request("PUT", "/dash/api/admin/client-keys/"+view.ID, policy, cookie, csrf); w.Code != 200 {
		t.Fatalf("restore scope: %d %s", w.Code, w.Body.String())
	}
	if w := anthropicCountCall(t, s, key, anthropicCountBody); w.Code != 200 {
		t.Fatalf("scope rejection leaked lease: %d %s", w.Code, w.Body.String())
	}
}

func TestAnthropicCountDisabledModel(t *testing.T) {
	for _, providerDisabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("provider=%t", providerDisabled), func(t *testing.T) {
			cfg := runtimeConfig("http://127.0.0.1:1")
			if providerDisabled {
				cfg.Providers["primary"].Disabled = true
			} else {
				cfg.DisabledModels = map[string]bool{"test": true}
			}
			s := runtimeTestServer(t, cfg)
			w := httptest.NewRecorder()
			s.handleAnthropicCountTokens(w, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(anthropicCountBody)))
			assertAnthropicRequestError(t, w, 404, "not_found_error")
		})
	}
}

func TestAnthropicCountReauthenticatesBeforeReading(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired=%t", expired), func(t *testing.T) {
			s, e, key, view := anthropicCountServer(t, clientKeyRequest{Name: "reauth"}, GatewayLimits{})
			body := newAnthropicSlowBody(t)
			r := anthropicCountRequest(t, s, key, body)
			if expired {
				e.security.mu.Lock()
				e.security.clients[view.ID].view.ExpiresAt = time.Now().Add(-time.Second).UnixMilli()
				e.security.mu.Unlock()
			} else {
				cookie, csrf := e.seedSession(t)
				if w := e.request("DELETE", "/dash/api/admin/client-keys/"+view.ID, nil, cookie, csrf); w.Code != 200 {
					t.Fatalf("revoke key: %d %s", w.Code, w.Body.String())
				}
			}
			w := httptest.NewRecorder()
			s.handleAnthropicCountTokens(w, r)
			assertAnthropicRequestError(t, w, 401, "authentication_error")
			if body.read.Load() {
				t.Fatal("stale credentials read request body")
			}
		})
	}
}

func TestAnthropicCountReadFailureReleasesAdmission(t *testing.T) {
	s, _, key, _ := anthropicCountServer(t, clientKeyRequest{Name: "read-error", MaxConcurrent: 1}, GatewayLimits{MaxInFlight: 1})
	body := newAnthropicSlowBody(t)
	_ = body.Close()
	w := httptest.NewRecorder()
	s.handleAnthropicCountTokens(w, anthropicCountRequest(t, s, key, body))
	assertAnthropicRequestError(t, w, 400, "invalid_request_error")
	if w := anthropicCountCall(t, s, key, anthropicCountBody); w.Code != 200 {
		t.Fatalf("read failure leaked admission: %d %s", w.Code, w.Body.String())
	}
}

func TestAnthropicCountSlowBodyDeadline(t *testing.T) {
	s, _, key, _ := anthropicCountServer(t, clientKeyRequest{Name: "deadline", MaxConcurrent: 1}, GatewayLimits{MaxInFlight: 1, QueueTimeoutSeconds: 1})
	server := httptest.NewServer(s.security.ClientMiddleware(http.HandlerFunc(s.handleAnthropicCountTokens)))
	defer server.Close()
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(4 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(conn, "POST /v1/messages/count_tokens HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n{", key, len(anthropicCountBody))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("slow request was not bounded by read deadline: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 400 || !strings.Contains(string(body), `"invalid_request_error"`) {
		t.Fatalf("deadline error: %d %s", response.StatusCode, body)
	}
	if w := anthropicCountCall(t, s, key, anthropicCountBody); w.Code != 200 {
		t.Fatalf("deadline leaked admission: %d %s", w.Code, w.Body.String())
	}
}
