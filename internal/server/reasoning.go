package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"log"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// decodeChunkMap decodes one JSON object with UseNumber so integers above
// 2^53 and high-precision floats survive a decode→marshal round trip
// byte-faithfully (plain Unmarshal into map[string]any silently reformats
// them through float64).
func decodeChunkMap(payload []byte) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var chunk map[string]any
	if err := dec.Decode(&chunk); err != nil {
		return nil, false
	}
	return chunk, true
}

const (
	maxReasoningEntries = 256
	maxReasoningBytes   = 512 * 1024
)

// Replay is private to an authenticated principal, public model, actual route,
// and the complete history ending in an exact assistant tool-call turn.
type reasoningScope struct {
	clientID, model, provider, upstream string
}

type reasoningKey struct {
	scope   reasoningScope
	history [sha256.Size]byte
}

type reasoningContext struct {
	key   reasoningKey
	valid bool
}

type reasoningCache struct {
	mu    sync.Mutex
	m     map[reasoningKey]string
	order []reasoningKey
	max   int
}

func newReasoningCache(max int) *reasoningCache {
	if max <= 0 {
		max = maxReasoningEntries
	}
	return &reasoningCache{m: make(map[reasoningKey]string, max), max: max}
}

func (c *reasoningCache) put(key reasoningKey, text string) {
	if c == nil || key.scope.clientID == "" || text == "" {
		return
	}
	if len(text) > maxReasoningBytes {
		// Do not retain an oversized decoded response's backing string.
		text = strings.Clone(reasoningPrefix(text, maxReasoningBytes))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[key]; !ok {
		if len(c.order) >= c.max {
			old := c.order[0]
			c.order = c.order[1:]
			delete(c.m, old)
		}
		c.order = append(c.order, key)
	}
	c.m[key] = text
}

func (c *reasoningCache) get(key reasoningKey) string {
	if c == nil || key.scope.clientID == "" {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[key]
}

// Nil, omitted and empty assistant content are protocol-equivalent. Function
// names/arguments and every preceding user/tool message remain fingerprinted.
func reasoningHistoryNext(previous [sha256.Size]byte, message map[string]any) [sha256.Size]byte {
	canonical := make(map[string]any, len(message))
	for k, v := range message {
		if k != "reasoning" && k != "reasoning_content" {
			canonical[k] = v
		}
	}
	if stringField(message, "role") == "assistant" {
		for key, value := range canonical {
			if value == nil {
				delete(canonical, key)
			}
		}
		if content := canonical["content"]; content == nil {
			delete(canonical, "content")
		} else if text, ok := content.(string); ok && text == "" {
			delete(canonical, "content")
		}
		if calls, ok := message["tool_calls"].([]any); ok {
			normalized := make([]any, 0, len(calls))
			for _, call := range calls {
				tc, ok := call.(map[string]any)
				if !ok {
					normalized = append(normalized, call)
					continue
				}
				copy := make(map[string]any, len(tc))
				for k, v := range tc {
					if k != "index" {
						copy[k] = v
					}
				}
				if stringField(copy, "type") == "" {
					copy["type"] = "function"
				}
				normalized = append(normalized, copy)
			}
			canonical["tool_calls"] = normalized
		}
	}
	h := sha256.New()
	_, _ = h.Write(previous[:])
	_ = json.NewEncoder(h).Encode(canonical)
	var digest [sha256.Size]byte
	copy(digest[:], h.Sum(nil))
	return digest
}

func reasoningPrefix(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	end := limit
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end]
}

// ensureReasoningReplay also fingerprints the request history for response
// capture. It is only called for backends that preserve reasoning.
func ensureReasoningReplay(raw []byte, cache *reasoningCache, scope reasoningScope) ([]byte, reasoningContext) {
	ctx := reasoningContext{key: reasoningKey{scope: scope}}
	parsed, ok := decodeChunkMap(raw)
	if !ok {
		return raw, ctx
	}
	msgs, ok := parsed["messages"].([]any)
	if !ok {
		return raw, ctx
	}
	ctx.valid = scope.clientID != ""
	var nCopy, nCache, nEmpty int
	changed := false
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			ctx.valid = false
			continue
		}
		ctx.key.history = reasoningHistoryNext(ctx.key.history, mm)
		if stringField(mm, "role") != "assistant" {
			continue
		}
		// An explicitly supplied field, including "", is authoritative.
		if _, supplied := mm["reasoning_content"]; supplied {
			continue
		}
		if alt, supplied := mm["reasoning"].(string); supplied {
			mm["reasoning_content"] = alt
			nCopy++
			changed = true
			continue
		}
		if !hasToolCalls(mm) {
			continue
		}
		filled := ""
		if ctx.valid {
			filled = cache.get(ctx.key)
		}
		if filled != "" {
			mm["reasoning_content"] = filled
			nCache++
		} else {
			mm["reasoning_content"] = ""
			nEmpty++
		}
		changed = true
	}
	if !changed {
		return raw, ctx
	}
	out, err := json.Marshal(parsed)
	if err != nil {
		return raw, ctx
	}
	log.Printf("[reasoning] replay copy=%d cache=%d empty=%d", nCopy, nCache, nEmpty)
	return out, ctx
}

func stringField(mm map[string]any, key string) string {
	s, _ := mm[key].(string)
	return s
}

func hasToolCalls(mm map[string]any) bool {
	tcs, ok := mm["tool_calls"].([]any)
	return ok && len(tcs) > 0
}

// Both reasoning and tool payload capture are bounded while ingesting. A turn
// whose payload cannot be captured exactly is not eligible for cache storage.
type sseReasoningAcc struct {
	choices        map[int]*reasoningChoice
	reasoningBytes int
	payloadBytes   int
}

type reasoningChoice struct {
	buf       []byte
	truncated bool
	content   strings.Builder
	calls     map[int]*reasoningToolCall
	invalid   bool
}

type reasoningToolCall struct {
	id, kind, name, arguments strings.Builder
}

func (a *sseReasoningAcc) ingest(payload []byte) {
	if a == nil || len(payload) == 0 {
		return
	}
	chunk, ok := decodeChunkMap(payload)
	if !ok {
		return
	}
	a.ingestMap(chunk)
}

// ingestMap is ingest for an already-decoded chunk, so the streaming path
// can decode once and share the result with the reasoning alias.
func (a *sseReasoningAcc) ingestMap(chunk map[string]any) {
	if a == nil {
		return
	}
	choices, _ := chunk["choices"].([]any)
	for position, c := range choices {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		index := reasoningIndex(cm, position)
		if a.choices == nil {
			a.choices = make(map[int]*reasoningChoice)
		}
		choice := a.choices[index]
		if choice == nil {
			if len(a.choices) >= maxReasoningEntries {
				continue
			}
			choice = &reasoningChoice{}
			a.choices[index] = choice
		}
		a.ingestObj(choice, cm["delta"])
		a.ingestObj(choice, cm["message"])
	}
}

func reasoningIndex(obj map[string]any, fallback int) int {
	if number, ok := obj["index"].(json.Number); ok {
		if value, err := number.Int64(); err == nil && value >= 0 && value < maxReasoningEntries {
			return int(value)
		}
	}
	return fallback
}

func (a *sseReasoningAcc) ingestObj(choice *reasoningChoice, v any) {
	mm, ok := v.(map[string]any)
	if !ok {
		return
	}
	text := stringField(mm, "reasoning_content")
	if text == "" {
		text = stringField(mm, "reasoning")
	}
	if choice.truncated {
		text = ""
	} else if remaining := maxReasoningBytes - a.reasoningBytes; len(text) > remaining {
		text = reasoningPrefix(text, remaining)
		choice.truncated = true
	}
	if len(text) > 0 {
		// Cap allocation growth as well as the visible buffer length.
		if needed := len(choice.buf) + len(text); needed > cap(choice.buf) {
			size := min(maxReasoningBytes, max(needed, 2*cap(choice.buf)))
			buf := make([]byte, len(choice.buf), size)
			copy(buf, choice.buf)
			choice.buf = buf
		}
		choice.buf = append(choice.buf, text...)
		a.reasoningBytes += len(text)
	}
	if content, present := mm["content"]; present && content != nil {
		if text, ok := content.(string); ok {
			a.capturePayload(choice, &choice.content, text)
		} else {
			choice.invalid = true
		}
	}
	calls, _ := mm["tool_calls"].([]any)
	for position, value := range calls {
		tc, ok := value.(map[string]any)
		if !ok {
			choice.invalid = true
			continue
		}
		if choice.calls == nil {
			choice.calls = make(map[int]*reasoningToolCall)
		}
		index := reasoningIndex(tc, position)
		call := choice.calls[index]
		if call == nil {
			if len(choice.calls) >= maxReasoningEntries {
				choice.invalid = true
				continue
			}
			call = &reasoningToolCall{}
			choice.calls[index] = call
		}
		a.capturePayload(choice, &call.id, stringField(tc, "id"))
		a.capturePayload(choice, &call.kind, stringField(tc, "type"))
		if function, ok := tc["function"].(map[string]any); ok {
			a.capturePayload(choice, &call.name, stringField(function, "name"))
			a.capturePayload(choice, &call.arguments, stringField(function, "arguments"))
		}
	}
}

func (a *sseReasoningAcc) capturePayload(choice *reasoningChoice, dst *strings.Builder, text string) {
	if choice.invalid {
		return
	}
	if len(text) > maxReasoningBytes-a.payloadBytes {
		choice.invalid = true
		return
	}
	dst.WriteString(text)
	a.payloadBytes += len(text)
}

func (a *sseReasoningAcc) store(cache *reasoningCache, ctx reasoningContext) {
	if a == nil || !ctx.valid {
		return
	}
	for _, choice := range a.choices {
		if choice.invalid || len(choice.buf) == 0 {
			continue
		}
		indices := make([]int, 0, len(choice.calls))
		for index := range choice.calls {
			indices = append(indices, index)
		}
		sort.Ints(indices)
		calls := make([]any, 0, len(indices))
		for _, index := range indices {
			call := choice.calls[index]
			if call.id.Len() == 0 || call.name.Len() == 0 {
				choice.invalid = true
				break
			}
			kind := call.kind.String()
			if kind == "" {
				kind = "function"
			}
			calls = append(calls, map[string]any{"id": call.id.String(), "type": kind,
				"function": map[string]any{"name": call.name.String(), "arguments": call.arguments.String()}})
		}
		if choice.invalid || len(calls) == 0 {
			continue
		}
		message := map[string]any{"role": "assistant", "content": choice.content.String(), "tool_calls": calls}
		key := ctx.key
		key.history = reasoningHistoryNext(key.history, message)
		cache.put(key, string(choice.buf))
	}
}

// aliasReasoningInChunk copies delta/message.reasoning onto reasoning_content
// so clients that only look for the DeepSeek field still capture thinking.
func aliasReasoningInChunk(payload []byte) ([]byte, bool) {
	chunk, ok := decodeChunkMap(payload)
	if !ok {
		return payload, false
	}
	if !aliasReasoningMap(chunk) {
		return payload, false
	}
	out, err := json.Marshal(chunk)
	if err != nil {
		return payload, false
	}
	return out, true
}

// aliasReasoningMap applies the reasoning alias to an already-decoded
// chunk in place, reporting whether anything changed.
func aliasReasoningMap(chunk map[string]any) bool {
	choices, ok := chunk["choices"].([]any)
	if !ok {
		return false
	}
	changed := false
	for _, c := range choices {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"delta", "message"} {
			mm, ok := cm[key].(map[string]any)
			if !ok {
				continue
			}
			if stringField(mm, "reasoning_content") == "" {
				if alt := stringField(mm, "reasoning"); alt != "" {
					mm["reasoning_content"] = alt
					changed = true
				}
			}
		}
	}
	return changed
}

func rememberFromResponse(body []byte, cache *reasoningCache, ctx reasoningContext) []byte {
	chunk, ok := decodeChunkMap(body)
	if !ok {
		return body
	}
	if ctx.valid {
		choices, _ := chunk["choices"].([]any)
		for _, value := range choices {
			choice, ok := value.(map[string]any)
			if !ok {
				continue
			}
			message, ok := choice["message"].(map[string]any)
			if !ok || !hasToolCalls(message) {
				continue
			}
			text := stringField(message, "reasoning_content")
			if text == "" {
				text = stringField(message, "reasoning")
			}
			if text == "" {
				continue
			}
			if _, present := message["role"]; !present {
				copy := make(map[string]any, len(message)+1)
				for k, v := range message {
					copy[k] = v
				}
				copy["role"] = "assistant"
				message = copy
			}
			key := ctx.key
			key.history = reasoningHistoryNext(key.history, message)
			cache.put(key, text)
		}
	}
	if aliasReasoningMap(chunk) {
		if rewritten, err := json.Marshal(chunk); err == nil {
			return rewritten
		}
	}
	return body
}
