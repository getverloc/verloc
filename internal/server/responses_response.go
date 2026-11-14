package server

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Response translation: chat.completion → Responses "response" object
//
// The shape mirrors the documented create-response schema: a typed object with
// a flat output array of Items (reasoning / message / function_call), a
// convenience output_text, and usage under input/output token names.
// ---------------------------------------------------------------------------

// responsesStatus maps a chat finish_reason onto the Responses status field.
func responsesStatus(finish *string) string {
	if finish == nil {
		return "completed"
	}
	switch *finish {
	case "length", "content_filter":
		return "incomplete"
	}
	return "completed"
}

// responsesFinishDetail fills incomplete_details when status is incomplete.
func responsesFinishDetail(finish *string) map[string]any {
	if finish == nil {
		return nil
	}
	switch *finish {
	case "length":
		return map[string]any{"reason": "max_output_tokens"}
	case "content_filter":
		return map[string]any{"reason": "content_filter"}
	}
	return nil
}

// responsesUsage converts chat token counts to the Responses naming. A nil
// result means usage was not reported: unknown must never be rendered as zero.
func responsesUsage(in, out, cache uint64, known bool) map[string]any {
	if !known {
		return nil
	}
	total := in + out
	return map[string]any{
		"input_tokens":         in,
		"input_tokens_details": map[string]any{"cached_tokens": cache},
		"output_tokens":        out,
		"output_tokens_details": map[string]any{
			"reasoning_tokens": 0,
		},
		"total_tokens": total,
	}
}

// openAIToResponses converts a complete chat-completions body into a Responses
// object. model is the agent-facing model id, not the upstream one.
func openAIToResponses(body []byte, model string) ([]byte, error) {
	var resp struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role      string          `json:"role"`
				Content   json.RawMessage `json:"content"`
				Refusal   string          `json:"refusal"`
				Reason    string          `json:"reasoning_content"`
				Reason2   string          `json:"reasoning"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens        uint64 `json:"prompt_tokens"`
			CompletionTokens    uint64 `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens uint64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, errors.New("invalid upstream response")
	}
	if len(resp.Choices) == 0 {
		return nil, errors.New("upstream response has no choices")
	}
	choice := resp.Choices[0]
	msg := choice.Message

	output := make([]any, 0, 3)
	// Reasoning first, matching the documented item order.
	reasoning := msg.Reason
	if reasoning == "" {
		reasoning = msg.Reason2
	}
	if reasoning != "" {
		output = append(output, map[string]any{
			"type":   "reasoning",
			"id":     "rs_" + randomUUID(),
			"status": "completed",
			"summary": []any{map[string]any{
				"type": "summary_text",
				"text": reasoning,
			}},
		})
	}
	text := responsesTextOf(msg.Content, msg.Refusal)
	// A tool-call-only turn has no message item, matching chat.
	if text != "" || len(msg.ToolCalls) == 0 {
		content := []any{map[string]any{
			"type":        "output_text",
			"text":        text,
			"annotations": []any{},
			"logprobs":    []any{},
		}}
		if msg.Refusal != "" && text == msg.Refusal {
			content = []any{map[string]any{"type": "refusal", "refusal": msg.Refusal}}
		}
		output = append(output, map[string]any{
			"type":    "message",
			"id":      "msg_" + randomUUID(),
			"status":  "completed",
			"role":    "assistant",
			"content": content,
		})
	}
	for _, tc := range msg.ToolCalls {
		args := tc.Function.Arguments
		if args == "" {
			args = "{}"
		}
		output = append(output, map[string]any{
			"type":      "function_call",
			"id":        "fc_" + randomUUID(),
			"status":    "completed",
			"call_id":   tc.ID,
			"name":      tc.Function.Name,
			"arguments": args,
		})
	}

	created := resp.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	out := map[string]any{
		"id":                 "resp_" + resp.ID,
		"object":             "response",
		"created_at":         created,
		"status":             responsesStatus(choice.FinishReason),
		"error":              nil,
		"incomplete_details": responsesFinishDetail(choice.FinishReason),
		"instructions":       nil,
		"max_output_tokens":  nil,
		"model":              model,
		"output":             output,
		"output_text":        text,
		// The gateway keeps no server-side response state, so these are
		// reported honestly rather than echoed from the request.
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
	if resp.Usage != nil {
		out["usage"] = responsesUsage(
			resp.Usage.PromptTokens,
			resp.Usage.CompletionTokens,
			resp.Usage.PromptTokensDetails.CachedTokens,
			true,
		)
	}
	return json.Marshal(out)
}

// responsesTextOf flattens a chat message content value (string or part array)
// into plain text, falling back to the refusal field.
func responsesTextOf(content json.RawMessage, refusal string) string {
	var text strings.Builder
	if len(content) > 0 && string(content) != "null" {
		var s string
		if json.Unmarshal(content, &s) == nil {
			text.WriteString(s)
		} else {
			var parts []struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Refusal string `json:"refusal"`
			}
			if json.Unmarshal(content, &parts) == nil {
				for _, p := range parts {
					if p.Text != "" {
						text.WriteString(p.Text)
					} else if p.Refusal != "" {
						text.WriteString(p.Refusal)
					}
				}
			}
		}
	}
	if text.Len() == 0 && refusal != "" {
		return refusal
	}
	return text.String()
}

// responsesErrorEnvelope is the error body Responses clients expect. It keeps
// the gateway's shape (an error object with message/type) but is written by
// the adapter so a Responses caller never receives a stray chat-era envelope.
func responsesErrorEnvelope(status int, message string) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    responsesErrorType(status),
			"param":   nil,
			"code":    nil,
		},
	}
}

func responsesErrorType(status int) string {
	switch {
	case status == 400:
		return "invalid_request_error"
	case status == 401:
		return "authentication_error"
	case status == 403:
		return "permission_error"
	case status == 404:
		return "not_found_error"
	case status == 429:
		return "rate_limit_error"
	case status >= 500:
		return "server_error"
	}
	return "invalid_request_error"
}
