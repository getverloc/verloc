package server

import (
	"io"
	"net/http"
)

// inboundAdapter translates a client protocol that the gateway does not serve
// natively into the chat-completions request that every backend speaks.
//
// Anthropic Messages (/v1/messages) and OpenAI Responses (/v1/responses) are
// both served this way: the wire protocol facing the client is the adapter's,
// the upstream conversation is always chat completions. forward() calls
// translateRequest once before admission and any upstream attempt, then
// translateResponse or streamCopier on the way back, so admission, key
// rotation, failover, the retry budget, price snapshots and accounting are
// inherited rather than reimplemented.
type inboundAdapter interface {
	// endpointName is the client-facing endpoint recorded as rec.Endpoint.
	endpointName() string
	// translateRequest converts the inbound body to a chat-completions body.
	// The returned error is delivered to the client in the adapter's own
	// error shape with status 400.
	translateRequest(raw []byte) error
	// body returns the translated chat-completions body.
	body() []byte
	// errorWriter emits an error in the adapter's protocol shape.
	errorWriter(w http.ResponseWriter, status int, message string)
	// streamCopier translates an upstream SSE stream into the adapter's
	// event sequence, writing to the already-committed client writer.
	streamCopier(w http.ResponseWriter, src io.Reader, onUsage func(in, out, cache uint64), onFirst func()) error
	// translateResponse converts a complete chat-completions body into the
	// adapter's response shape.
	translateResponse(body []byte) []byte
}

var (
	_ inboundAdapter = (*anthropicContext)(nil)
	_ inboundAdapter = (*responsesContext)(nil)
)

// capabilityLabel names the inbound endpoint in capability-rejection errors,
// so a Responses request is not told it lacks chat-completions support.
func capabilityLabel(a inboundAdapter) string {
	if a == nil {
		return "chat completions"
	}
	return a.endpointName()
}
