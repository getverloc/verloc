package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// responsesCall drives POST /v1/responses through the real handler.
func responsesCall(s *Server, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.handleResponses(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
	return w
}

// responsesUpstream captures the chat-completions request the adapter sent and
// replies with a canned chat body.
func responsesUpstream(t *testing.T, reply string) (*httptest.Server, *string, *atomic.Int32) {
	t.Helper()
	var seen string
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/chat/completions" {
			t.Errorf("upstream path = %q, want /chat/completions — the adapter must never POST /responses", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		seen = string(raw)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen, &calls
}

const chatTextReply = `{"id":"chatcmpl-1","object":"chat.completion","created":1756315657,"model":"upstream-test",
"choices":[{"index":0,"message":{"role":"assistant","content":"hello there"},"finish_reason":"stop"}],
"usage":{"prompt_tokens":11,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":2}}}`

func TestResponsesTranslatesToChatCompletions(t *testing.T) {
	upstream, seen, calls := responsesUpstream(t, chatTextReply)
	s := runtimeTestServer(t, runtimeConfig(upstream.URL))

	w := responsesCall(s, `{"model":"test","input":"hi"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls=%d, want 1", calls.Load())
	}

	var sent map[string]any
	if err := json.Unmarshal([]byte(*seen), &sent); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	msgs, _ := sent["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages=%v, want one user message", sent["messages"])
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "user" || first["content"] != "hi" {
		t.Fatalf("message=%v", first)
	}
	if sent["model"] != "upstream-test" {
		t.Fatalf("model not remapped to upstream id: %v", sent["model"])
	}

	// The client-facing body must be the Responses object.
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("client body not JSON: %v", err)
	}
	if out["object"] != "response" {
		t.Fatalf("object=%v, want response", out["object"])
	}
	if !strings.HasPrefix(out["id"].(string), "resp_") {
		t.Fatalf("id=%v, want resp_ prefix", out["id"])
	}
	if out["output_text"] != "hello there" {
		t.Fatalf("output_text=%v", out["output_text"])
	}
	if out["status"] != "completed" {
		t.Fatalf("status=%v", out["status"])
	}
	output, _ := out["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output=%v, want one item", output)
	}
	item, _ := output[0].(map[string]any)
	if item["type"] != "message" || item["role"] != "assistant" {
		t.Fatalf("item=%v", item)
	}
	content, _ := item["content"].([]any)
	part, _ := content[0].(map[string]any)
	if part["type"] != "output_text" || part["text"] != "hello there" {
		t.Fatalf("part=%v", part)
	}

	// Usage must be renamed to the Responses vocabulary.
	usage, _ := out["usage"].(map[string]any)
	if usage == nil {
		t.Fatal("usage missing")
	}
	if usage["input_tokens"] != float64(11) || usage["output_tokens"] != float64(3) {
		t.Fatalf("usage=%v", usage)
	}
	if usage["total_tokens"] != float64(14) {
		t.Fatalf("total_tokens=%v", usage["total_tokens"])
	}
	details, _ := usage["input_tokens_details"].(map[string]any)
	if details["cached_tokens"] != float64(2) {
		t.Fatalf("cached_tokens=%v", details)
	}
	// The gateway stores no response state and says so.
	if out["store"] != false || out["previous_response_id"] != nil {
		t.Fatalf("store/previous_response_id misreported: %v / %v", out["store"], out["previous_response_id"])
	}
}

func TestResponsesRecordsEndpointAndUsage(t *testing.T) {
	upstream, _, _ := responsesUpstream(t, chatTextReply)
	s := runtimeTestServer(t, runtimeConfig(upstream.URL))
	if w := responsesCall(s, `{"model":"test","input":"hi"}`); w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	rec := runtimeRecord(t, s)
	if rec.Endpoint != "responses" {
		t.Fatalf("endpoint=%q, want responses (the client protocol, not the forwarded chat path)", rec.Endpoint)
	}
	if !rec.UsageKnown || rec.TokIn != 11 || rec.TokOut != 3 || rec.TokCache != 2 {
		t.Fatalf("usage not recorded: %+v", rec)
	}
}

// The asymmetry in stateful-field handling is deliberate and load-bearing:
// store defaults to true in the SDKs so it must be tolerated, while a client
// depending on continuation must be told rather than silently answered
// statelessly.
func TestResponsesStatefulFieldHandling(t *testing.T) {
	upstream, seen, calls := responsesUpstream(t, chatTextReply)
	s := runtimeTestServer(t, runtimeConfig(upstream.URL))

	// store:true is accepted and ignored.
	if w := responsesCall(s, `{"model":"test","input":"hi","store":true}`); w.Code != http.StatusOK {
		t.Fatalf("store:true rejected: %d %s", w.Code, w.Body.String())
	}

	for _, tc := range []struct{ name, body, wantField string }{
		{"previous_response_id", `{"model":"test","input":"hi","previous_response_id":"resp_abc"}`, "previous_response_id"},
		{"conversation", `{"model":"test","input":"hi","conversation":{"id":"c1"}}`, "conversation"},
		{"web_search", `{"model":"test","input":"hi","tools":[{"type":"web_search"}]}`, "web_search"},
		{"file_search", `{"model":"test","input":"hi","tools":[{"type":"file_search"}]}`, "file_search"},
		{"code_interpreter", `{"model":"test","input":"hi","tools":[{"type":"code_interpreter"}]}`, "code_interpreter"},
		{"mcp", `{"model":"test","input":"hi","tools":[{"type":"mcp"}]}`, "mcp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := calls.Load()
			w := responsesCall(s, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400 — honouring %s silently would return a wrong answer", w.Code, tc.name)
			}
			if calls.Load() != before {
				t.Fatalf("upstream was called for a rejected request")
			}
			if !strings.Contains(w.Body.String(), tc.wantField) {
				t.Fatalf("error must name %q to be actionable: %s", tc.wantField, w.Body.String())
			}
			// The error envelope must be Responses-shaped, not the gateway's.
			var out map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatalf("error body not JSON: %v", err)
			}
			errObj, _ := out["error"].(map[string]any)
			if errObj == nil || errObj["type"] != "invalid_request_error" || errObj["message"] == nil {
				t.Fatalf("error envelope=%v", out)
			}
		})
	}
	_ = seen
}

func TestResponsesInstructionsAndDeveloperRole(t *testing.T) {
	upstream, seen, _ := responsesUpstream(t, chatTextReply)
	s := runtimeTestServer(t, runtimeConfig(upstream.URL))

	w := responsesCall(s, `{"model":"test","instructions":"be terse","input":[
		{"type":"message","role":"developer","content":[{"type":"input_text","text":"rule one"}]},
		{"type":"message","role":"user","content":"question"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var sent struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(*seen), &sent); err != nil {
		t.Fatal(err)
	}
	if len(sent.Messages) != 3 {
		t.Fatalf("messages=%d, want instructions + developer + user", len(sent.Messages))
	}
	if sent.Messages[0].Role != "system" || sent.Messages[0].Content != "be terse" {
		t.Fatalf("instructions not mapped to system: %+v", sent.Messages[0])
	}
	// "developer" must never reach an upstream; backends reject it.
	if sent.Messages[1].Role != "system" {
		t.Fatalf("developer role not remapped: %+v", sent.Messages[1])
	}
	if sent.Messages[1].Content != "rule one" {
		t.Fatalf("text-only part array should flatten to a string: %+v", sent.Messages[1].Content)
	}
	if sent.Messages[2].Content != "question" {
		t.Fatalf("user content=%v", sent.Messages[2].Content)
	}
}

func TestResponsesToolTranslation(t *testing.T) {
	reply := `{"id":"c2","created":1,"choices":[{"index":0,"message":{"role":"assistant","content":null,
	"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Delhi\"}"}}]},
	"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":7}}`
	upstream, seen, _ := responsesUpstream(t, reply)
	s := runtimeTestServer(t, runtimeConfig(upstream.URL))

	w := responsesCall(s, `{"model":"test","input":"weather?","tools":[
		{"type":"function","name":"get_weather","description":"d","parameters":{"type":"object","properties":{"city":{"type":"string"}}}},
		{"type":"function","name":"other","strict":false,"parameters":{"type":"object"}}],
		"tool_choice":{"type":"function","name":"get_weather"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	// Internally tagged Responses tools become externally tagged chat tools.
	var sent struct {
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
			Name string `json:"name"`
		} `json:"tools"`
		ToolChoice map[string]any `json:"tool_choice"`
	}
	if err := json.Unmarshal([]byte(*seen), &sent); err != nil {
		t.Fatal(err)
	}
	if len(sent.Tools) != 2 {
		t.Fatalf("tools=%d, want 2", len(sent.Tools))
	}
	if sent.Tools[0].Type != "function" || sent.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("tool not nested under function: %+v", sent.Tools[0])
	}
	if sent.Tools[0].Name != "" {
		t.Fatalf("top-level name must not survive: %+v", sent.Tools[0])
	}
	if !strings.Contains(string(sent.Tools[0].Function.Parameters), "city") {
		t.Fatalf("parameters lost: %s", sent.Tools[0].Function.Parameters)
	}
	fn, _ := sent.ToolChoice["function"].(map[string]any)
	if sent.ToolChoice["type"] != "function" || fn["name"] != "get_weather" {
		t.Fatalf("tool_choice not nested: %v", sent.ToolChoice)
	}

	// Chat tool_calls become function_call output items.
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	output, _ := out["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output=%v, want one function_call item and no empty message", output)
	}
	item, _ := output[0].(map[string]any)
	if item["type"] != "function_call" || item["call_id"] != "call_1" || item["name"] != "get_weather" {
		t.Fatalf("function_call item=%v", item)
	}
	if item["arguments"] != `{"city":"Delhi"}` {
		t.Fatalf("arguments=%v", item["arguments"])
	}
}

func TestResponsesToolCallReplay(t *testing.T) {
	upstream, seen, _ := responsesUpstream(t, chatTextReply)
	s := runtimeTestServer(t, runtimeConfig(upstream.URL))

	// A client replaying its transcript sends function_call and
	// function_call_output items back.
	w := responsesCall(s, `{"model":"test","input":[
		{"type":"message","role":"user","content":"weather?"},
		{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Delhi\"}"},
		{"type":"function_call_output","call_id":"call_1","output":"sunny"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var sent struct {
		Messages []struct {
			Role      string `json:"role"`
			Content   any    `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(*seen), &sent); err != nil {
		t.Fatal(err)
	}
	if len(sent.Messages) != 3 {
		t.Fatalf("messages=%d, want user + assistant(tool_calls) + tool", len(sent.Messages))
	}
	call := sent.Messages[1]
	if call.Role != "assistant" || len(call.ToolCalls) != 1 || call.ToolCalls[0].ID != "call_1" {
		t.Fatalf("function_call replay=%+v", call)
	}
	if call.ToolCalls[0].Function.Arguments != `{"city":"Delhi"}` {
		t.Fatalf("arguments=%v", call.ToolCalls[0].Function.Arguments)
	}
	toolMsg := sent.Messages[2]
	if toolMsg.Role != "tool" || toolMsg.ToolCallID != "call_1" || toolMsg.Content != "sunny" {
		t.Fatalf("function_call_output replay=%+v", toolMsg)
	}
}

func TestResponsesStructuredOutput(t *testing.T) {
	upstream, seen, _ := responsesUpstream(t, chatTextReply)
	s := runtimeTestServer(t, runtimeConfig(upstream.URL))

	w := responsesCall(s, `{"model":"test","input":"jane","text":{"format":{"type":"json_schema",
		"name":"person","strict":true,"schema":{"type":"object","properties":{"name":{"type":"string"}}}}}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var sent struct {
		ResponseFormat map[string]any `json:"response_format"`
	}
	if err := json.Unmarshal([]byte(*seen), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.ResponseFormat["type"] != "json_schema" {
		t.Fatalf("response_format=%v", sent.ResponseFormat)
	}
	inner, _ := sent.ResponseFormat["json_schema"].(map[string]any)
	if inner["name"] != "person" || inner["strict"] != true {
		t.Fatalf("json_schema not flattened correctly: %v", inner)
	}
	if inner["schema"] == nil {
		t.Fatalf("schema lost: %v", inner)
	}
}

func TestResponsesReasoningEffortAndMaxTokens(t *testing.T) {
	upstream, seen, _ := responsesUpstream(t, chatTextReply)
	s := runtimeTestServer(t, runtimeConfig(upstream.URL))

	if w := responsesCall(s, `{"model":"test","input":"hi","max_output_tokens":64,"temperature":0.2,"top_p":0.9,"reasoning":{"effort":"high"}}`); w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(*seen), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["max_tokens"] != float64(64) {
		t.Fatalf("max_output_tokens not mapped to max_tokens: %v", sent["max_tokens"])
	}
	if sent["temperature"] != 0.2 || sent["top_p"] != 0.9 {
		t.Fatalf("sampling params lost: %v / %v", sent["temperature"], sent["top_p"])
	}
	if sent["reasoning_effort"] != "high" {
		t.Fatalf("reasoning.effort not flattened: %v", sent["reasoning_effort"])
	}
}

func TestResponsesUnknownUsageStaysUnknown(t *testing.T) {
	// A backend that reports no usage must not produce a zero-cost record.
	upstream, _, _ := responsesUpstream(t, `{"id":"c3","created":1,"choices":[{"index":0,
		"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}]}`)
	s := runtimeTestServer(t, runtimeConfig(upstream.URL))
	w := responsesCall(s, `{"model":"test","input":"hi"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if _, present := out["usage"]; present {
		t.Fatalf("usage must be absent when unreported, never zero: %v", out["usage"])
	}
	rec := runtimeRecord(t, s)
	if rec.UsageKnown {
		t.Fatalf("record claims known usage: %+v", rec)
	}
	if rec.CostKnown {
		t.Fatalf("record claims known cost from unknown usage: %+v", rec)
	}
}

func TestResponsesIncompleteStatus(t *testing.T) {
	upstream, _, _ := responsesUpstream(t, `{"id":"c4","created":1,"choices":[{"index":0,
		"message":{"role":"assistant","content":"trunc"},"finish_reason":"length"}],
		"usage":{"prompt_tokens":1,"completion_tokens":2}}`)
	s := runtimeTestServer(t, runtimeConfig(upstream.URL))
	w := responsesCall(s, `{"model":"test","input":"hi"}`)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["status"] != "incomplete" {
		t.Fatalf("status=%v, want incomplete for finish_reason=length", out["status"])
	}
	details, _ := out["incomplete_details"].(map[string]any)
	if details["reason"] != "max_output_tokens" {
		t.Fatalf("incomplete_details=%v", details)
	}
}

func TestResponsesCapabilityErrorNamesTheEndpoint(t *testing.T) {
	upstream, _, calls := responsesUpstream(t, chatTextReply)
	cfg := runtimeConfig(upstream.URL)
	cfg.ModelMeta = map[string]*ModelMeta{"test": {Operations: []string{"images"}}}
	s := runtimeTestServer(t, cfg)

	w := responsesCall(s, `{"model":"test","input":"hi"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", w.Code)
	}
	if calls.Load() != 0 {
		t.Fatal("upstream called despite unsupported operation")
	}
	body := w.Body.String()
	if strings.Contains(body, "chat completions") {
		t.Fatalf("a Responses request must not be told it lacks chat-completions support: %s", body)
	}
	if !strings.Contains(body, "responses") {
		t.Fatalf("error must name the requested endpoint: %s", body)
	}
}

func TestResponsesUnknownModelAndMissingInput(t *testing.T) {
	upstream, _, calls := responsesUpstream(t, chatTextReply)
	s := runtimeTestServer(t, runtimeConfig(upstream.URL))

	if w := responsesCall(s, `{"model":"nope","input":"hi"}`); w.Code != http.StatusNotFound {
		t.Fatalf("unknown model status=%d", w.Code)
	}
	if w := responsesCall(s, `{"model":"test","input":""}`); w.Code != http.StatusBadRequest {
		t.Fatalf("empty input status=%d body=%s", w.Code, w.Body.String())
	}
	if w := responsesCall(s, `{"model":"test"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("missing input status=%d", w.Code)
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream called %d times for rejected requests", calls.Load())
	}
}

func TestResponsesUnsupportedItemType(t *testing.T) {
	upstream, _, calls := responsesUpstream(t, chatTextReply)
	s := runtimeTestServer(t, runtimeConfig(upstream.URL))
	w := responsesCall(s, `{"model":"test","input":[{"type":"mystery","role":"user"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatal("upstream called for an unrepresentable item")
	}
}

func TestResponsesRejectsMultipleGenerations(t *testing.T) {
	upstream, _, calls := responsesUpstream(t, chatTextReply)
	s := runtimeTestServer(t, runtimeConfig(upstream.URL))
	w := responsesCall(s, `{"model":"test","input":"hi","n":3}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 — chat would return one of three candidates", w.Code)
	}
	if !strings.Contains(w.Body.String(), "n is not supported") {
		t.Fatalf("error must name the field: %s", w.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatal("upstream called for a rejected request")
	}
	// n:1 is the default and must be tolerated.
	if w := responsesCall(s, `{"model":"test","input":"hi","n":1}`); w.Code != http.StatusOK {
		t.Fatalf("n:1 rejected: %d %s", w.Code, w.Body.String())
	}
}
