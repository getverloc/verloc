package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// sseUpstream serves a canned chat-completions SSE stream and counts calls.
func sseUpstream(t *testing.T, events ...string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/chat/completions" {
			t.Errorf("upstream path=%q, want /chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			io.WriteString(w, e)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// responsesEvents parses a Responses SSE body into ordered (event, payload)
// pairs so the test can assert on the sequence itself.
func responsesEvents(t *testing.T, body string) []struct {
	Event string
	Data  map[string]any
} {
	t.Helper()
	var out []struct {
		Event string
		Data  map[string]any
	}
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" || strings.HasPrefix(block, ":") {
			continue
		}
		var event string
		var data string
		for _, line := range strings.Split(block, "\n") {
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				event = v
			} else if v, ok := strings.CutPrefix(line, "data: "); ok {
				data = v
			}
		}
		if data == "" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("event %q payload not JSON: %v", event, err)
		}
		out = append(out, struct {
			Event string
			Data  map[string]any
		}{event, payload})
	}
	return out
}

func eventTypes(events []struct {
	Event string
	Data  map[string]any
}) []string {
	types := make([]string, 0, len(events))
	for _, e := range events {
		typ, _ := e.Data["type"].(string)
		types = append(types, typ)
	}
	return types
}

func responsesStreamCall(s *Server, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.handleResponses(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
	return w
}

func TestResponsesStreamTextSequence(t *testing.T) {
	upstream, _ := sseUpstream(t,
		`data: {"id":"c1","created":1,"choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}`+"\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"lo"}}]}`+"\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n",
		`data: {"id":"c1","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":1}}}`+"\n\n",
		"data: [DONE]\n\n",
	)
	s := runtimeTestServer(t, runtimeConfig(upstream.URL))

	w := responsesStreamCall(s, `{"model":"test","input":"hi","stream":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	events := responsesEvents(t, w.Body.String())
	types := eventTypes(events)

	// The documented sequence must be present and ordered.
	want := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	for _, name := range want {
		if !containsString(types, name) {
			t.Fatalf("missing %q in %v", name, types)
		}
	}
	// Created must precede deltas, completed must be last.
	if types[0] != "response.created" {
		t.Fatalf("first event=%q, want response.created", types[0])
	}
	if types[len(types)-1] != "response.completed" {
		t.Fatalf("last event=%q, want response.completed", types[len(types)-1])
	}
	if idx(types, "response.output_text.delta") > idx(types, "response.output_text.done") {
		t.Fatalf("delta after done: %v", types)
	}

	// Deltas must reassemble the full text.
	var text strings.Builder
	for _, e := range events {
		if e.Data["type"] == "response.output_text.delta" {
			text.WriteString(e.Data["delta"].(string))
		}
	}
	if text.String() != "Hello" {
		t.Fatalf("reassembled text=%q, want Hello", text.String())
	}

	// sequence_number must be strictly increasing.
	last := -1.0
	for _, e := range events {
		n, ok := e.Data["sequence_number"].(float64)
		if !ok {
			t.Fatalf("event %v has no sequence_number", e.Data["type"])
		}
		if n <= last {
			t.Fatalf("sequence_number not increasing: %v then %v", last, n)
		}
		last = n
	}

	// Completed carries usage in Responses vocabulary.
	var completed map[string]any
	for _, e := range events {
		if e.Data["type"] == "response.completed" {
			completed = e.Data
		}
	}
	resp, _ := completed["response"].(map[string]any)
	if resp["status"] != "completed" {
		t.Fatalf("completed status=%v", resp["status"])
	}
	usage, _ := resp["usage"].(map[string]any)
	if usage["input_tokens"] != float64(9) || usage["output_tokens"] != float64(2) {
		t.Fatalf("usage=%v", usage)
	}
	details, _ := usage["input_tokens_details"].(map[string]any)
	if details["cached_tokens"] != float64(1) {
		t.Fatalf("cached_tokens=%v", details)
	}

	// Accounting sees the same usage the client did.
	rec := runtimeRecord(t, s)
	if !rec.UsageKnown || rec.TokIn != 9 || rec.TokOut != 2 || rec.TokCache != 1 {
		t.Fatalf("usage not recorded: %+v", rec)
	}
	if rec.Endpoint != "responses" {
		t.Fatalf("endpoint=%q", rec.Endpoint)
	}
}

func TestResponsesStreamToolCalls(t *testing.T) {
	upstream, _ := sseUpstream(t,
		`data: {"id":"c2","created":1,"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`+"\n\n",
		`data: {"id":"c2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`+"\n\n",
		`data: {"id":"c2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Delhi\"}"}}]}}]}`+"\n\n",
		`data: {"id":"c2","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n",
		"data: [DONE]\n\n",
	)
	s := runtimeTestServer(t, runtimeConfig(upstream.URL))

	w := responsesStreamCall(s, `{"model":"test","input":"weather","stream":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	events := responsesEvents(t, w.Body.String())

	var addedArgsDescriptor bool
	var argDeltas strings.Builder
	for _, e := range events {
		switch e.Data["type"] {
		case "response.output_item.added":
			item, _ := e.Data["item"].(map[string]any)
			if item["type"] == "function_call" {
				addedArgsDescriptor = true
				if item["call_id"] != "call_9" || item["name"] != "get_weather" {
					t.Fatalf("function_call item=%v", item)
				}
			}
		case "response.function_call_arguments.delta":
			argDeltas.WriteString(e.Data["delta"].(string))
		}
	}
	if !addedArgsDescriptor {
		t.Fatal("no function_call output_item.added event")
	}
	if argDeltas.String() != `{"city":"Delhi"}` {
		t.Fatalf("argument deltas=%q", argDeltas.String())
	}

	// The completed response carries the finished function_call item.
	var completed map[string]any
	for _, e := range events {
		if e.Data["type"] == "response.completed" {
			completed = e.Data
		}
	}
	resp, _ := completed["response"].(map[string]any)
	output, _ := resp["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output=%v, want one function_call item", output)
	}
	item, _ := output[0].(map[string]any)
	if item["type"] != "function_call" || item["arguments"] != `{"city":"Delhi"}` {
		t.Fatalf("final item=%v", item)
	}
}

func TestResponsesStreamTruncationIsNotSuccess(t *testing.T) {
	// An upstream that dies mid-stream must not look like a completed
	// response: no response.completed may be emitted.
	upstream, _ := sseUpstream(t,
		`data: {"id":"c3","created":1,"choices":[{"index":0,"delta":{"content":"partial"}}]}`+"\n\n",
	)
	s := runtimeTestServer(t, runtimeConfig(upstream.URL))
	w := responsesStreamCall(s, `{"model":"test","input":"hi","stream":true}`)

	types := eventTypes(responsesEvents(t, w.Body.String()))
	if containsString(types, "response.completed") {
		t.Fatalf("truncated stream emitted response.completed: %v", types)
	}
	rec := runtimeRecord(t, s)
	if rec.OK {
		t.Fatalf("truncated stream recorded as success: %+v", rec)
	}
	if rec.Outcome != "interrupted" {
		t.Fatalf("outcome=%q, want interrupted", rec.Outcome)
	}
}

func TestResponsesStreamUpstreamError(t *testing.T) {
	upstream, _ := sseUpstream(t,
		`data: {"id":"c4","created":1,"choices":[{"index":0,"delta":{"content":"start"}}]}`+"\n\n",
		`data: {"error":{"message":"model overloaded","type":"server_error"}}`+"\n\n",
	)
	s := runtimeTestServer(t, runtimeConfig(upstream.URL))
	w := responsesStreamCall(s, `{"model":"test","input":"hi","stream":true}`)

	types := eventTypes(responsesEvents(t, w.Body.String()))
	if containsString(types, "response.completed") {
		t.Fatalf("failed stream emitted response.completed: %v", types)
	}
	if !containsString(types, "response.failed") {
		t.Fatalf("missing response.failed in %v", types)
	}
	rec := runtimeRecord(t, s)
	if rec.OK {
		t.Fatalf("failed stream recorded as success: %+v", rec)
	}
}

func TestResponsesStreamRequestsUsageChunk(t *testing.T) {
	// Without stream_options.include_usage many backends omit the terminal
	// usage chunk, so usage would be permanently unknown.
	var seen atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		seen.Store(string(raw))
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"id":"c5","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	s := runtimeTestServer(t, runtimeConfig(srv.URL))
	responsesStreamCall(s, `{"model":"test","input":"hi","stream":true}`)

	var sent struct {
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal([]byte(seen.Load().(string)), &sent); err != nil {
		t.Fatal(err)
	}
	if !sent.StreamOptions.IncludeUsage {
		t.Fatalf("stream_options.include_usage not requested: %s", seen.Load())
	}
}

func TestResponsesNonStreamDoesNotStreamEvents(t *testing.T) {
	upstream, _, _ := responsesUpstream(t, chatTextReply)
	s := runtimeTestServer(t, runtimeConfig(upstream.URL))
	w := responsesStreamCall(s, `{"model":"test","input":"hi"}`)
	if strings.Contains(w.Body.String(), "event: ") {
		t.Fatalf("non-streaming request emitted SSE: %s", w.Body.String())
	}
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func idx(list []string, want string) int {
	for i, v := range list {
		if v == want {
			return i
		}
	}
	return -1
}

var _ = fmt.Sprintf

// TestResponsesRoutesThroughHandler exercises the real mux, client-key
// middleware and both aliases, over a live HTTP server so streaming semantics
// (incremental delivery, chunked encoding) are genuine rather than simulated by
// a ResponseRecorder.
func TestResponsesRoutesThroughHandler(t *testing.T) {
	var upstreamPath string
	var upstreamBody atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		upstreamBody.Store(string(raw))
		var sent map[string]any
		_ = json.Unmarshal(raw, &sent)
		if sent["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, `data: {"id":"h1","created":1,"choices":[{"index":0,"delta":{"content":"streamed"}}]}`+"\n\n")
			io.WriteString(w, `data: {"id":"h1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1}}`+"\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"h2","created":1,"choices":[{"index":0,"message":{"role":"assistant","content":"buffered"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
	}))
	defer upstream.Close()

	e := newSecurityTestEnv(t)
	cfg := &Config{
		DefaultProvider: "mock",
		ModelMap:        map[string]string{"mock/text": "mock:upstream-text"},
		Providers:       map[string]*ProviderConfig{"mock": {BaseURL: upstream.URL, Keys: []KeyEntry{{Key: "sk-test-key"}}}},
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
	key, _ := createSecurityTestKey(t, e, cookie, csrf, clientKeyRequest{Name: "responses"})

	gateway := httptest.NewServer(srv.Handler())
	defer gateway.Close()

	post := func(path, body string) (*http.Response, string) {
		req, err := http.NewRequest(http.MethodPost, gateway.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp, string(raw)
	}

	// Both aliases serve the endpoint, non-streaming.
	for _, path := range []string{"/v1/responses", "/responses"} {
		resp, body := post(path, `{"model":"mock/text","input":"hello"}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, resp.StatusCode, body)
		}
		if upstreamPath != "/chat/completions" {
			t.Fatalf("%s forwarded to %q, want /chat/completions", path, upstreamPath)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("%s body not JSON: %v", path, err)
		}
		if out["object"] != "response" || out["output_text"] != "buffered" {
			t.Fatalf("%s response=%s", path, body)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Fatalf("%s content-type=%q", path, ct)
		}
	}

	// The upstream must always be asked for chat completions.
	var sent struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte(upstreamBody.Load().(string)), &sent); err != nil {
		t.Fatal(err)
	}
	if len(sent.Messages) != 1 || sent.Messages[0]["content"] != "hello" {
		t.Fatalf("upstream messages=%v", sent.Messages)
	}

	// Streaming over real HTTP.
	resp, body := post("/v1/responses", `{"model":"mock/text","input":"hello","stream":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status=%d body=%s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("stream content-type=%q", ct)
	}
	types := eventTypes(responsesEvents(t, body))
	for _, want := range []string{"response.created", "response.output_text.delta", "response.completed"} {
		if !containsString(types, want) {
			t.Fatalf("stream missing %q in %v", want, types)
		}
	}

	// Anonymous access is rejected by the client-key middleware.
	req, _ := http.NewRequest(http.MethodPost, gateway.URL+"/v1/responses", strings.NewReader(`{"model":"mock/text","input":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	anonResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer anonResp.Body.Close()
	if anonResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d, want 401", anonResp.StatusCode)
	}
}
