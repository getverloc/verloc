package server

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

var testReasoningScope = reasoningScope{clientID: "app-a", model: "public-a", provider: "primary", upstream: "up"}

const testReasoningCall = `{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"x","arguments":"{}"}}]}`

func seedReasoning(cache *reasoningCache, scope reasoningScope, history, reasoning string) {
	_, ctx := ensureReasoningReplay([]byte(`{"messages":`+history+`}`), cache, scope)
	response := `{"choices":[{"message":` + strings.TrimSuffix(testReasoningCall, "}") + `,"reasoning_content":` + strconv.Quote(reasoning) + `}}]}`
	rememberFromResponse([]byte(response), cache, ctx)
}

func TestEnsureReasoningReplayCopy(t *testing.T) {
	raw := []byte(`{"messages":[{"role":"assistant","content":"","reasoning":"think","tool_calls":[{"id":"c1","type":"function","function":{"name":"x","arguments":"{}"}}]}]}`)
	out, _ := ensureReasoningReplay(raw, newReasoningCache(8), testReasoningScope)
	var p map[string]any
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	msg := p["messages"].([]any)[0].(map[string]any)
	if msg["reasoning_content"] != "think" {
		t.Fatalf("got %#v", msg["reasoning_content"])
	}
}

func TestEnsureReasoningReplayFromCache(t *testing.T) {
	cache := newReasoningCache(8)
	seedReasoning(cache, testReasoningScope, `[]`, "prior think")
	raw := []byte(`{"messages":[{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"x","arguments":"{}"}}]}]}`)
	out, _ := ensureReasoningReplay(raw, cache, testReasoningScope)
	var p map[string]any
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	msg := p["messages"].([]any)[0].(map[string]any)
	if msg["reasoning_content"] != "prior think" {
		t.Fatalf("got %#v", msg["reasoning_content"])
	}
}

func TestEnsureReasoningReplayEmptyOnToolCalls(t *testing.T) {
	raw := []byte(`{"messages":[{"role":"assistant","content":"hi","tool_calls":[{"id":"c1","type":"function","function":{"name":"x","arguments":"{}"}}]}]}`)
	out, _ := ensureReasoningReplay(raw, newReasoningCache(8), testReasoningScope)
	var p map[string]any
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	msg := p["messages"].([]any)[0].(map[string]any)
	s, ok := msg["reasoning_content"].(string)
	if !ok || s != "" {
		t.Fatalf("expected empty reasoning_content, got %#v", msg["reasoning_content"])
	}
}

func TestEnsureReasoningReplaySkipsPlainAssistant(t *testing.T) {
	raw := []byte(`{"messages":[{"role":"assistant","content":"hello"}]}`)
	out, _ := ensureReasoningReplay(raw, newReasoningCache(8), testReasoningScope)
	if string(out) != string(raw) {
		t.Fatalf("should pass through unchanged: %s", out)
	}
}

func TestSseReasoningAccStore(t *testing.T) {
	cache := newReasoningCache(8)
	_, ctx := ensureReasoningReplay([]byte(`{"messages":[]}`), cache, testReasoningScope)
	acc := &sseReasoningAcc{}
	acc.ingest([]byte(`{"choices":[{"delta":{"reasoning_content":"ab"}}]}`))
	acc.ingest([]byte(`{"choices":[{"delta":{"reasoning":"cd","tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"x","arguments":"{}"}}]}}]}`))
	acc.store(cache, ctx)
	out, _ := ensureReasoningReplay([]byte(`{"messages":[`+testReasoningCall+`]}`), cache, testReasoningScope)
	if !strings.Contains(string(out), `"reasoning_content":"abcd"`) {
		t.Fatalf("got %s", out)
	}
}

func TestAliasReasoningInChunk(t *testing.T) {
	in := []byte(`{"choices":[{"delta":{"reasoning":"think"}}]}`)
	out, ok := aliasReasoningInChunk(in)
	if !ok {
		t.Fatal("expected rewrite")
	}
	if !strings.Contains(string(out), `"reasoning_content":"think"`) {
		t.Fatalf("got %s", out)
	}
}
