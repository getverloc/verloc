package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// SSE translation: chat.completion.chunk → Responses typed events
//
// Mirrors anthropic_stream.go: one state object drives the event sequence,
// usage and TTFT are reported through the same callbacks, and the error
// contract is identical (nil on success, *streamClientError on client write
// failure, io.ErrUnexpectedEOF on truncation).
//
// Sequence for a text response:
//   response.created → response.in_progress →
//   response.output_item.added → response.content_part.added →
//   response.output_text.delta* → response.output_text.done →
//   response.content_part.done → response.output_item.done →
//   response.completed
// ---------------------------------------------------------------------------

type responsesStreamState struct {
	model    string
	respID   string
	itemID   string
	seq      int
	started  bool
	textOpen bool
	// tool bookkeeping: chat streams tool calls by index
	tools      map[int]*responsesToolState
	toolOrder  []int
	outputs    []any
	text       strings.Builder
	inputTok   uint64
	outputTok  uint64
	cacheTok   uint64
	usageKnown bool
	finished   bool
	created    int64
}

type responsesToolState struct {
	callID string
	name   string
	itemID string
	args   strings.Builder
	added  bool
	done   bool
}

func newResponsesStreamState(model string) *responsesStreamState {
	return &responsesStreamState{
		model:   model,
		respID:  "resp_" + randomUUID(),
		tools:   make(map[int]*responsesToolState),
		created: time.Now().Unix(),
	}
}

// writeResponsesEvent marshals one typed event. Responses SSE carries the type
// inside the payload as well as in the event: line, so clients can branch on
// either. Client write errors are wrapped in *streamClientError so forward()
// classifies client disconnects as 499 canceled rather than 502 interrupted.
func writeResponsesEvent(w http.ResponseWriter, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	kind, _ := payload["type"].(string)
	if _, err := w.Write([]byte("event: " + kind + "\n")); err != nil {
		return &streamClientError{err}
	}
	if _, err := w.Write([]byte("data: " + string(b) + "\n\n")); err != nil {
		return &streamClientError{err}
	}
	return nil
}

// seqNext stamps an event with an increasing sequence_number, which the
// Responses stream contract requires for ordering.
func (r *responsesStreamState) seqNext(payload map[string]any) map[string]any {
	payload["sequence_number"] = r.seq
	r.seq++
	return payload
}

func (r *responsesStreamState) emit(w http.ResponseWriter, payload map[string]any) error {
	return writeResponsesEvent(w, r.seqNext(payload))
}

// base is the response object echoed on created/in_progress/completed. It
// reports status honestly: the gateway keeps no response state.
func (r *responsesStreamState) base(status string, output []any) map[string]any {
	if output == nil {
		output = []any{}
	}
	return map[string]any{
		"id":                   r.respID,
		"object":               "response",
		"created_at":           r.created,
		"status":               status,
		"error":                nil,
		"incomplete_details":   nil,
		"instructions":         nil,
		"max_output_tokens":    nil,
		"model":                r.model,
		"output":               output,
		"parallel_tool_calls":  true,
		"previous_response_id": nil,
		"store":                false,
		"temperature":          1.0,
		"text":                 map[string]any{"format": map[string]any{"type": "text"}},
		"tool_choice":          "auto",
		"tools":                []any{},
		"top_p":                1.0,
		"truncation":           "disabled",
		"metadata":             map[string]any{},
	}
}

// begin emits response.created and response.in_progress exactly once.
func (r *responsesStreamState) begin(w http.ResponseWriter) error {
	if r.started {
		return nil
	}
	r.started = true
	if err := r.emit(w, map[string]any{"type": "response.created", "response": r.base("in_progress", nil)}); err != nil {
		return err
	}
	return r.emit(w, map[string]any{"type": "response.in_progress", "response": r.base("in_progress", nil)})
}

// openText starts the message output item and its text content part.
func (r *responsesStreamState) openText(w http.ResponseWriter) error {
	if err := r.begin(w); err != nil {
		return err
	}
	if r.textOpen {
		return nil
	}
	r.textOpen = true
	r.itemID = "msg_" + randomUUID()
	if err := r.emit(w, map[string]any{
		"type":         "response.output_item.added",
		"output_index": 0,
		"item": map[string]any{
			"type": "message", "id": r.itemID, "status": "in_progress",
			"role": "assistant", "content": []any{},
		},
	}); err != nil {
		return err
	}
	return r.emit(w, map[string]any{
		"type":    "response.content_part.added",
		"item_id": r.itemID, "output_index": 0, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
}

// closeText finishes the message item: text.done, content_part.done, item.done.
func (r *responsesStreamState) closeText(w http.ResponseWriter) error {
	if !r.textOpen {
		return nil
	}
	r.textOpen = false
	text := r.text.String()
	if err := r.emit(w, map[string]any{
		"type":    "response.output_text.done",
		"item_id": r.itemID, "output_index": 0, "content_index": 0, "text": text,
	}); err != nil {
		return err
	}
	if err := r.emit(w, map[string]any{
		"type":    "response.content_part.done",
		"item_id": r.itemID, "output_index": 0, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
	}); err != nil {
		return err
	}
	item := map[string]any{
		"type": "message", "id": r.itemID, "status": "completed", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
	}
	r.outputs = append(r.outputs, item)
	return r.emit(w, map[string]any{
		"type": "response.output_item.done", "output_index": len(r.outputs) - 1, "item": item,
	})
}

// openTool starts a function_call item for a chat tool-call index.
func (r *responsesStreamState) openTool(w http.ResponseWriter, index int) (*responsesToolState, error) {
	tool, ok := r.tools[index]
	if !ok {
		tool = &responsesToolState{itemID: "fc_" + randomUUID()}
		r.tools[index] = tool
		r.toolOrder = append(r.toolOrder, index)
	}
	if tool.added {
		return tool, nil
	}
	tool.added = true
	if err := r.emit(w, map[string]any{
		"type":         "response.output_item.added",
		"output_index": len(r.outputs) + len(r.toolOrder) - 1,
		"item": map[string]any{
			"type": "function_call", "id": tool.itemID, "status": "in_progress",
			"call_id": tool.callID, "name": tool.name, "arguments": "",
		},
	}); err != nil {
		return nil, err
	}
	return tool, nil
}

// closeTools finishes every open function_call item.
func (r *responsesStreamState) closeTools(w http.ResponseWriter) error {
	for _, index := range r.toolOrder {
		tool := r.tools[index]
		if tool == nil || !tool.added || tool.done {
			continue
		}
		tool.done = true
		args := tool.args.String()
		if args == "" {
			args = "{}"
		}
		if err := r.emit(w, map[string]any{
			"type":    "response.function_call_arguments.done",
			"item_id": tool.itemID, "output_index": len(r.outputs),
			"arguments": args,
		}); err != nil {
			return err
		}
		item := map[string]any{
			"type": "function_call", "id": tool.itemID, "status": "completed",
			"call_id": tool.callID, "name": tool.name, "arguments": args,
		}
		r.outputs = append(r.outputs, item)
		if err := r.emit(w, map[string]any{
			"type": "response.output_item.done", "output_index": len(r.outputs) - 1, "item": item,
		}); err != nil {
			return err
		}
	}
	return nil
}

// finish closes any open item and emits response.completed with usage.
func (r *responsesStreamState) finish(w http.ResponseWriter, status string) error {
	if err := r.begin(w); err != nil {
		return err
	}
	// A tool-call-only stream still needs its text item closed if one opened.
	if err := r.closeText(w); err != nil {
		return err
	}
	if err := r.closeTools(w); err != nil {
		return err
	}
	base := r.base(status, r.outputs)
	if usage := responsesUsage(r.inputTok, r.outputTok, r.cacheTok, r.usageKnown); usage != nil {
		base["usage"] = usage
	}
	r.finished = true
	return r.emit(w, map[string]any{"type": "response.completed", "response": base})
}

// ingestUsage reads usage from a chat chunk that carries it.
func (r *responsesStreamState) ingestUsage(payload []byte) {
	if !bytes.Contains(payload, []byte(`"usage"`)) {
		return
	}
	if in, out, cache, known := usageOf(payload); known {
		r.inputTok, r.outputTok, r.cacheTok = in, out, cache
		r.usageKnown = true
	}
}

// ingestChunk translates one chat SSE data payload into Responses events.
func (r *responsesStreamState) ingestChunk(w http.ResponseWriter, payload []byte) error {
	var chunk struct {
		Type    string `json:"type"`
		Choices []struct {
			Delta struct {
				Content   string `json:"content"`
				Reason    string `json:"reasoning_content"`
				Reason2   string `json:"reasoning"`
				ToolCalls []struct {
					Index    *int   `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return nil // not a chat chunk we understand
	}
	if chunk.Type != "" {
		// A Responses-shaped event from an upstream that speaks it natively;
		// the passthrough path never reaches here, but be defensive.
		return nil
	}
	if len(chunk.Choices) == 0 {
		r.ingestUsage(payload)
		return nil
	}
	delta := chunk.Choices[0].Delta
	// Reasoning content shares the text item's stream in Responses; emitting
	// it as output text keeps the sequence valid for clients that only read
	// output_text deltas, while remaining visible to those that parse items.
	if delta.Content != "" {
		if err := r.openText(w); err != nil {
			return err
		}
		r.text.WriteString(delta.Content)
		if err := r.emit(w, map[string]any{
			"type":    "response.output_text.delta",
			"item_id": r.itemID, "output_index": 0, "content_index": 0,
			"delta": delta.Content,
		}); err != nil {
			return err
		}
	}
	reasoning := delta.Reason
	if reasoning == "" {
		reasoning = delta.Reason2
	}
	if reasoning != "" {
		if err := r.openText(w); err != nil {
			return err
		}
		r.text.WriteString(reasoning)
		if err := r.emit(w, map[string]any{
			"type":    "response.output_text.delta",
			"item_id": r.itemID, "output_index": 0, "content_index": 0,
			"delta": reasoning,
		}); err != nil {
			return err
		}
	}
	for _, tc := range delta.ToolCalls {
		index := 0
		if tc.Index != nil {
			index = *tc.Index
		}
		tool := r.tools[index]
		if tool == nil {
			tool = &responsesToolState{itemID: "fc_" + randomUUID()}
			r.tools[index] = tool
			r.toolOrder = append(r.toolOrder, index)
		}
		// A first tool delta arrives after any text; close text before the
		// function_call item opens so output order stays append-only.
		if !tool.added && r.textOpen {
			if err := r.closeText(w); err != nil {
				return err
			}
		}
		if tc.ID != "" {
			tool.callID = tc.ID
		}
		if tc.Function.Name != "" {
			tool.name = tc.Function.Name
		}
		if !tool.added {
			if _, err := r.openTool(w, index); err != nil {
				return err
			}
		}
		if tc.Function.Arguments != "" {
			tool.args.WriteString(tc.Function.Arguments)
			if err := r.emit(w, map[string]any{
				"type":    "response.function_call_arguments.delta",
				"item_id": tool.itemID, "output_index": len(r.outputs),
				"delta": tc.Function.Arguments,
			}); err != nil {
				return err
			}
		}
	}
	r.ingestUsage(payload)
	return nil
}

// streamCopyResponses reads an OpenAI chat SSE stream from src and writes the
// Responses event sequence to w.
func streamCopyResponses(w http.ResponseWriter, src io.Reader, model string, onUsage func(in, out, cache uint64), onFirst func()) error {
	state := newResponsesStreamState(model)
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 4096), maxSSELineBytes)
	// terminal: the stream signalled completion ([DONE] or a terminal event).
	// failed: the stream ended in an upstream error.
	// sawFinish: a finish_reason arrived (a success signal, not a failure).
	terminal, failed, sawFinish := false, false, false
	flusher, _ := w.(http.Flusher)
	lastPing := time.Now()
	// Responses clients use inter-event gaps for idle detection; the gateway
	// keeps them bounded the same way the Anthropic translator does.
	maybePing := func() error {
		if time.Since(lastPing) < 15*time.Second {
			return nil
		}
		lastPing = time.Now()
		_, err := w.Write([]byte(": ping\n\n"))
		return err
	}
	flush := func() error {
		if f, ok := w.(interface{ FlushError() error }); ok {
			if err := f.FlushError(); err != nil {
				return &streamClientError{err}
			}
		} else if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			terminal = true
		} else {
			data := []byte(payload)
			var probe struct {
				Type    string          `json:"type"`
				Error   json.RawMessage `json:"error"`
				Choices []struct {
					Finish *string `json:"finish_reason"`
				} `json:"choices"`
			}
			if json.Unmarshal(data, &probe) == nil {
				switch probe.Type {
				case "response.completed":
					terminal = true
				case "response.failed", "response.incomplete", "error":
					// An upstream that already speaks Responses: surface its
					// failure rather than inventing success.
					terminal, failed = true, true
				}
				if len(probe.Error) > 0 && string(probe.Error) != "null" {
					terminal, failed = true, true
				}
				for _, c := range probe.Choices {
					if c.Finish != nil {
						sawFinish = true
					}
				}
			}
			if onUsage != nil && bytes.Contains(data, usageMarker) {
				if in, out, cache, known := usageOf(data); known {
					onUsage(in, out, cache)
				}
			}
			if !terminal {
				if err := state.ingestChunk(w, data); err != nil {
					return err
				}
			}
		}
		if onFirst != nil {
			onFirst()
			onFirst = nil
		}
		if err := flush(); err != nil {
			return err
		}
		if state.started {
			if err := maybePing(); err != nil {
				return &streamClientError{err}
			}
		}
		if terminal {
			if failed {
				if err := writeResponsesFailed(w, state); err != nil {
					return err
				}
				if err := flush(); err != nil {
					return err
				}
				// The client already received response.failed; the non-nil
				// error is how forward() learns not to record success.
				return errUpstreamStreamFailed
			}
			if err := state.finish(w, "completed"); err != nil {
				return err
			}
			return flush()
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	// The upstream closed without [DONE] or a terminal event. A finish_reason
	// alone is not proof the body completed, so never claim success: emit
	// response.failed and report truncation so accounting records interrupted.
	if !terminal && !sawFinish {
		if err := writeResponsesFailed(w, state); err != nil {
			return err
		}
		if err := flush(); err != nil {
			return err
		}
		return io.ErrUnexpectedEOF
	}
	if !state.finished {
		if err := state.finish(w, "completed"); err != nil {
			return err
		}
	}
	return flush()
}

// errUpstreamStreamFailed marks a stream that ended in an upstream error whose
// failure events were already delivered to the client.
var errUpstreamStreamFailed = errors.New("upstream stream failed")

// writeResponsesFailed emits the terminal failure events for a stream that
// ended in an upstream error.
func writeResponsesFailed(w http.ResponseWriter, state *responsesStreamState) error {
	base := state.base("failed", state.outputs)
	base["error"] = map[string]any{"code": "upstream_error", "message": "upstream stream failed"}
	if err := state.emit(w, map[string]any{"type": "error", "code": "upstream_error", "message": "upstream stream failed"}); err != nil {
		return err
	}
	return state.emit(w, map[string]any{"type": "response.failed", "response": base})
}
