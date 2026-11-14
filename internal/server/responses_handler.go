package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// errMissingModel rejects a request that names no model, before any upstream
// attempt.
var errMissingModel = errors.New("model is required")

// responsesContext carries the parsed inbound Responses request through the
// shared gateway path. It is the Responses counterpart of anthropicContext:
// forward() translates the request once before admission and any upstream
// attempt, then translates the upstream chat answer back on the way out.
//
// The upstream conversation is always chat completions — no backend needs to
// implement /responses — which is what makes every chat-capable model in the
// catalog serve this endpoint.
type responsesContext struct {
	// model is the agent-facing verloc model id, echoed on the response.
	model string
	// chatBody is the translated chat-completions request body.
	chatBody []byte
}

func (r *responsesContext) endpointName() string { return "responses" }

// translateRequest converts the inbound Responses body to a chat-completions
// body. Errors are surfaced as a Responses-shaped 400 by forward().
func (r *responsesContext) translateRequest(raw []byte) error {
	chat, parsed, err := responsesToOpenAI(raw)
	if err != nil {
		return err
	}
	if parsed.Model == "" {
		return errMissingModel
	}
	encoded, err := marshalNoEscape(chat)
	if err != nil {
		return err
	}
	r.chatBody = encoded
	r.model = parsed.Model
	return nil
}

func (r *responsesContext) body() []byte { return r.chatBody }

func (r *responsesContext) errorWriter(w http.ResponseWriter, status int, message string) {
	writeResponsesError(w, status, message)
}

func (r *responsesContext) streamCopier(w http.ResponseWriter, src io.Reader, onUsage func(in, out, cache uint64), onFirst func()) error {
	return streamCopyResponses(w, src, r.model, onUsage, onFirst)
}

// translateResponse converts a complete chat-completions body into a Responses
// object. A translation failure returns nil so forward() can fail the request
// rather than emit a body of the wrong shape.
func (r *responsesContext) translateResponse(body []byte) []byte {
	out, err := openAIToResponses(body, r.model)
	if err != nil {
		return nil
	}
	return out
}

// writeResponsesError emits the Responses error envelope.
func writeResponsesError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(responsesErrorEnvelope(status, message))
}

// marshalNoEscape encodes a translated body without HTML escaping, so model
// names and tool schemas round-trip byte-for-byte.
func marshalNoEscape(body map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return out, nil
}
