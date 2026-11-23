package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func replayedReasoning(t *testing.T, raw []byte) string {
	t.Helper()
	parsed, ok := decodeChunkMap(raw)
	if !ok {
		t.Fatalf("invalid request: %s", raw)
	}
	messages, _ := parsed["messages"].([]any)
	for i := len(messages) - 1; i >= 0; i-- {
		if message, ok := messages[i].(map[string]any); ok && stringField(message, "role") == "assistant" {
			return stringField(message, "reasoning_content")
		}
	}
	t.Fatal("assistant message missing")
	return ""
}

func TestReasoningReplayScopesHistoryAndPayload(t *testing.T) {
	cache := newReasoningCache(32)
	history := `[{"role":"user","content":"conversation one","record":9007199254740993}]`
	seedReasoning(cache, testReasoningScope, history, "private thought")
	prefix := strings.TrimSuffix(history, "]")
	request := []byte(`{"messages":` + prefix + `,` + testReasoningCall + `,{"role":"tool","tool_call_id":"c1","content":"done"}]}`)
	for _, tc := range []struct {
		name  string
		scope reasoningScope
		raw   []byte
		want  string
	}{
		{"same history next turn", testReasoningScope, request, "private thought"},
		{"different app", reasoningScope{clientID: "app-b", model: "public-a", provider: "primary", upstream: "up"}, request, ""},
		{"different public model", reasoningScope{clientID: "app-a", model: "public-b", provider: "primary", upstream: "up"}, request, ""},
		{"different provider", reasoningScope{clientID: "app-a", model: "public-a", provider: "fallback", upstream: "up"}, request, ""},
		{"different upstream", reasoningScope{clientID: "app-a", model: "public-a", provider: "primary", upstream: "other"}, request, ""},
		{"different conversation", testReasoningScope, []byte(strings.Replace(string(request), "conversation one", "conversation two", 1)), ""},
		{"different function name", testReasoningScope, []byte(strings.Replace(string(request), `"name":"x"`, `"name":"y"`, 1)), ""},
		{"different arguments", testReasoningScope, []byte(strings.Replace(string(request), `"arguments":"{}"`, `"arguments":"{\"n\":1}"`, 1)), ""},
		{"different assistant content", testReasoningScope, []byte(strings.Replace(string(request), `"content":"","tool_calls"`, `"content":"different","tool_calls"`, 1)), ""},
		{"anonymous", reasoningScope{model: "public-a", provider: "primary", upstream: "up"}, request, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, _ := ensureReasoningReplay(tc.raw, cache, tc.scope)
			if got := replayedReasoning(t, out); got != tc.want {
				t.Fatalf("replayed %q, want %q", got, tc.want)
			}
			if !strings.Contains(string(out), "9007199254740993") {
				t.Fatal("history integer lost precision")
			}
		})
	}
	// Two same-client conversations may reuse exactly the same tool-call ID.
	otherHistory := strings.Replace(history, "conversation one", "conversation two", 1)
	seedReasoning(cache, testReasoningScope, otherHistory, "other thought")
	for _, tc := range []struct {
		raw  []byte
		want string
	}{
		{request, "private thought"},
		{[]byte(strings.Replace(string(request), "conversation one", "conversation two", 1)), "other thought"},
	} {
		out, _ := ensureReasoningReplay(tc.raw, cache, testReasoningScope)
		if got := replayedReasoning(t, out); got != tc.want {
			t.Fatalf("conversation collision: got %q want %q", got, tc.want)
		}
	}
	_, ctx := ensureReasoningReplay([]byte(`{"messages":`+history+`}`), cache, testReasoningScope)
	otherCall := strings.Replace(testReasoningCall, `"arguments":"{}"`, `"arguments":"{\"n\":1}"`, 1)
	rememberFromResponse([]byte(`{"choices":[{"message":`+strings.TrimSuffix(otherCall, "}")+`,"reasoning_content":"different payload thought"}}]}`), cache, ctx)
	for _, tc := range []struct {
		call, want string
	}{
		{testReasoningCall, "private thought"},
		{otherCall, "different payload thought"},
	} {
		raw := []byte(`{"messages":` + prefix + `,` + tc.call + `]}`)
		out, _ := ensureReasoningReplay(raw, cache, testReasoningScope)
		if got := replayedReasoning(t, out); got != tc.want {
			t.Fatalf("tool payload collision: got %q want %q", got, tc.want)
		}
	}
}

func TestReasoningReplayRetainsExplicitFields(t *testing.T) {
	cache := newReasoningCache(8)
	seedReasoning(cache, testReasoningScope, `[]`, "cached")
	for _, tc := range []struct{ field, want string }{
		{`"reasoning_content":"provided"`, "provided"},
		{`"reasoning_content":""`, ""},
		{`"reasoning":"alternate"`, "alternate"},
		{`"reasoning":""`, ""},
	} {
		raw := []byte(`{"messages":[` + strings.TrimSuffix(testReasoningCall, "}") + `,` + tc.field + `}]}`)
		out, _ := ensureReasoningReplay(raw, cache, testReasoningScope)
		if got := replayedReasoning(t, out); got != tc.want {
			t.Fatalf("%s: got %q want %q", tc.field, got, tc.want)
		}
	}
}

func TestReasoningFragmentedToolCallsMatchNonstream(t *testing.T) {
	streamCache, jsonCache := newReasoningCache(8), newReasoningCache(8)
	_, ctx := ensureReasoningReplay([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), streamCache, testReasoningScope)
	acc := &sseReasoningAcc{}
	for _, chunk := range []string{
		`{"choices":[{"index":0,"delta":{"reasoning":"th","content":"look ","tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"lo","arguments":"{\"n\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_content":"ink","content":"up","tool_calls":[{"index":0,"id":"1","function":{"name":"ok","arguments":"9007199254740993}"}},{"index":1,"id":"c2","type":"function","function":{"name":"second","arguments":"{}"}}]}}]}`,
	} {
		acc.ingest([]byte(chunk))
	}
	acc.store(streamCache, ctx)
	message := `{"role":"assistant","content":"look up","tool_calls":[{"id":"c1","type":"function","function":{"name":"look","arguments":"{\"n\":9007199254740993}"}},{"id":"c2","type":"function","function":{"name":"second","arguments":"{}"}}]}`
	response := []byte(`{"choices":[{"index":0,"message":` + strings.TrimSuffix(message, "}") + `,"reasoning":"think"}}]}`)
	aliased := rememberFromResponse(response, jsonCache, ctx)
	if !strings.Contains(string(aliased), `"reasoning_content":"think"`) || !strings.Contains(string(aliased), "9007199254740993") {
		t.Fatalf("nonstream alias or fidelity lost: %s", aliased)
	}
	request := []byte(`{"messages":[{"role":"user","content":"hi"},` + message + `,{"role":"tool","tool_call_id":"c1","content":"done"}]}`)
	for _, cache := range []*reasoningCache{streamCache, jsonCache} {
		out, _ := ensureReasoningReplay(request, cache, testReasoningScope)
		if got := replayedReasoning(t, out); got != "think" {
			t.Fatalf("fragmented/nonstream replay = %q", got)
		}
	}
}

func TestReasoningAccumulatorBoundsAndUTF8(t *testing.T) {
	cache := newReasoningCache(8)
	_, ctx := ensureReasoningReplay([]byte(`{"messages":[]}`), cache, testReasoningScope)
	acc := &sseReasoningAcc{}
	acc.ingest([]byte(`{"choices":[{"delta":{"reasoning":` + strconv.Quote(strings.Repeat("a", maxReasoningBytes-1)) + `}}]}`))
	acc.ingest([]byte(`{"choices":[{"delta":{"reasoning":"€overflow"}}]}`))
	for range 20 {
		acc.ingest([]byte(`{"choices":[{"delta":{"reasoning":"discarded"}}]}`))
		if acc.reasoningBytes > maxReasoningBytes || len(acc.choices[0].buf) > maxReasoningBytes || cap(acc.choices[0].buf) > maxReasoningBytes {
			t.Fatal("reasoning buffer exceeded cap during ingestion")
		}
	}
	// The cap must not prevent later tool metadata from being captured.
	acc.ingest([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"x","arguments":"{}"}}]}}]}`))
	acc.store(cache, ctx)
	out, _ := ensureReasoningReplay([]byte(`{"messages":[`+testReasoningCall+`]}`), cache, testReasoningScope)
	got := replayedReasoning(t, out)
	if !utf8.ValidString(got) || got != strings.Repeat("a", maxReasoningBytes-1) {
		t.Fatalf("bounded replay was not a valid UTF-8 prefix: bytes=%d", len(got))
	}
	// Payload overflow cannot cause a truncated tool turn to collide with one
	// whose original arguments happen to equal the captured prefix.
	acc = &sseReasoningAcc{}
	acc.ingest([]byte(`{"choices":[{"delta":{"reasoning":"secret","tool_calls":[{"id":"c1","function":{"name":"x","arguments":` + strconv.Quote(strings.Repeat("x", maxReasoningBytes+1)) + `}}]}}]}`))
	if acc.payloadBytes > maxReasoningBytes || !acc.choices[0].invalid {
		t.Fatal("oversized tool payload was not bounded and rejected for replay")
	}
	for i := range maxReasoningEntries + 10 {
		acc.ingest([]byte(fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":%d,"id":"id","function":{"name":"x","arguments":"{}"}}]}}]}`, i)))
	}
	if len(acc.choices[0].calls) > maxReasoningEntries {
		t.Fatal("unbounded tool call tracking")
	}
}

func TestProxyReasoningIsolationForAppKeysModelsAndHistories(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			seen := make(chan []byte, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				seen <- body
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning\":\"private \",\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"x\",\"arguments\":\"{\"}}]}}]}\n\n")
					io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning\":\"thought\",\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
				} else {
					io.WriteString(w, `{"choices":[{"message":`+strings.TrimSuffix(testReasoningCall, "}")+`,"reasoning":"private thought"},"finish_reason":"tool_calls"}]}`)
				}
			}))
			defer upstream.Close()
			cfg := runtimeConfig(upstream.URL)
			noStrip := false
			cfg.Providers["primary"].StripReasoningContent = &noStrip
			cfg.Providers["fallback"] = &ProviderConfig{BaseURL: upstream.URL, AllowPrivateUpstreams: true, Keys: []KeyEntry{{ID: "fallback-key", Key: "fallback-secret"}}, StripReasoningContent: &noStrip}
			cfg.ModelMap["model-b"] = "primary:upstream-test"
			cfg.ModelMap["model-route"] = "fallback:other-upstream"
			s := runtimeTestServer(t, cfg)
			e := newSecurityTestEnv(t)
			cookie, csrf := e.seedSession(t)
			keyA, _ := createSecurityTestKey(t, e, cookie, csrf, clientKeyRequest{Name: "app-a"})
			keyB, _ := createSecurityTestKey(t, e, cookie, csrf, clientKeyRequest{Name: "app-b"})
			s.security = e.security
			call := func(t *testing.T, key, model, messages string) []byte {
				t.Helper()
				body := fmt.Sprintf(`{"model":%s,"stream":%t,"messages":%s}`, strconv.Quote(model), stream, messages)
				r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer "+key)
				w := httptest.NewRecorder()
				s.security.ClientMiddleware(http.HandlerFunc(s.handleChat)).ServeHTTP(w, r)
				if w.Code != 200 {
					t.Fatalf("proxy status %d: %s", w.Code, w.Body.String())
				}
				return <-seen
			}
			history := `[{"role":"user","content":"conversation one"}]`
			call(t, keyA, "test", history)
			next := strings.TrimSuffix(history, "]") + `,` + testReasoningCall + `,{"role":"tool","tool_call_id":"c1","content":"done"}]`
			for _, tc := range []struct{ name, key, model, messages, want string }{
				{"other key", keyB, "test", next, ""},
				{"other public model same route", keyA, "model-b", next, ""},
				{"other provider upstream route", keyA, "model-route", next, ""},
				{"other conversation", keyA, "test", strings.Replace(next, "conversation one", "conversation two", 1), ""},
				{"other tool payload", keyA, "test", strings.Replace(next, `"arguments":"{}"`, `"arguments":"{\"n\":1}"`, 1), ""},
				{"legitimate followup", keyA, "test", next, "private thought"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if got := replayedReasoning(t, call(t, tc.key, tc.model, tc.messages)); got != tc.want {
						t.Fatalf("upstream received reasoning %q, want %q", got, tc.want)
					}
				})
			}
		})
	}
}

func TestReasoningChoicesAndEarlierReplayRemainIndependent(t *testing.T) {
	cache := newReasoningCache(16)
	history := `[{"role":"user","content":"hi"}]`
	_, ctx := ensureReasoningReplay([]byte(`{"messages":`+history+`}`), cache, testReasoningScope)
	acc := &sseReasoningAcc{}
	acc.ingest([]byte(`{"choices":[{"index":0,"delta":{"reasoning":"first thought","tool_calls":[{"index":0,"id":"c1","function":{"name":"x","arguments":"{}"}}]}},{"index":1,"delta":{"reasoning":"second thought","tool_calls":[{"index":0,"id":"c1","function":{"name":"y","arguments":"{}"}}]}}]}`))
	acc.store(cache, ctx)
	for _, tc := range []struct {
		call, want string
	}{
		{testReasoningCall, "first thought"},
		{strings.Replace(testReasoningCall, `"name":"x"`, `"name":"y"`, 1), "second thought"},
	} {
		next := strings.TrimSuffix(history, "]") + `,` + tc.call + `,{"role":"tool","tool_call_id":"c1","content":"done"}]`
		out, nextCtx := ensureReasoningReplay([]byte(`{"messages":`+next+`}`), cache, testReasoningScope)
		if got := replayedReasoning(t, out); got != tc.want {
			t.Fatalf("choice replay = %q, want %q", got, tc.want)
		}
		// Fingerprint the next turn after the earlier reasoning was injected.
		withReplay, _ := decodeChunkMap(out)
		injected, _ := json.Marshal(withReplay["messages"])
		_, injectedCtx := ensureReasoningReplay([]byte(`{"messages":`+string(injected)+`}`), cache, testReasoningScope)
		if nextCtx.key != injectedCtx.key {
			t.Fatal("injected reasoning changed the conversation fingerprint")
		}
		secondCall := strings.Replace(testReasoningCall, `"id":"c1"`, `"id":"c2"`, 1)
		response := []byte(`{"choices":[{"message":` + strings.TrimSuffix(secondCall, "}") + `,"reasoning_content":"later thought"}}]}`)
		rememberFromResponse(response, cache, injectedCtx)
		later := strings.TrimSuffix(next, "]") + `,` + secondCall + `,{"role":"tool","tool_call_id":"c2","content":"done"}]`
		replayed, _ := ensureReasoningReplay([]byte(`{"messages":`+later+`}`), cache, testReasoningScope)
		if got := replayedReasoning(t, replayed); got != "later thought" {
			t.Fatalf("earlier replay invalidated legitimate later turn: %q", got)
		}
	}
}
