package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// OpenAI Responses API request shape (inbound from the client)
//
// The gateway serves /v1/responses by translating to a chat-completions body,
// mirroring how /v1/messages is served for Anthropic. Fields with no chat
// equivalent are either rejected (when honouring them silently would return a
// wrong answer) or accepted and ignored (when they carry no observable
// contract on the chat path). See responsesUnsupported for the reject list.
// ---------------------------------------------------------------------------

type responsesRequest struct {
	Model        string          `json:"model"`
	Input        json.RawMessage `json:"input"` // string or item array
	Instructions json.RawMessage `json:"instructions"`
	Stream       bool            `json:"stream"`
	MaxOutput    *uint64         `json:"max_output_tokens"`
	Temperature  *float64        `json:"temperature"`
	TopP         *float64        `json:"top_p"`
	Tools        []responsesTool `json:"tools"`
	ToolChoice   json.RawMessage `json:"tool_choice"`
	Parallel     *bool           `json:"parallel_tool_calls"`
	Text         *responsesText  `json:"text"`
	Reasoning    *struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
	// N is not part of the Responses API (it returns one generation).
	// Chat accepts it, so a client asking for several candidates would
	// silently receive one; reject instead.
	N *int `json:"n"`
	// Responses-only state fields. Presence is checked, not just value.
	PreviousResponseID json.RawMessage `json:"previous_response_id"`
	Conversation       json.RawMessage `json:"conversation"`
	Store              *bool           `json:"store"`
}

type responsesText struct {
	Format *responsesFormat `json:"format"`
}

type responsesFormat struct {
	Type   string          `json:"type"`
	Name   string          `json:"name"`
	Strict *bool           `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

// responsesTool is internally tagged (type + name at the top level), unlike
// chat's externally tagged {type:"function", function:{name,...}}.
type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict"`
}

// responsesInputItem is one entry of the input array. A message item carries
// role+content; function_call and function_call_output correlate by call_id.
type responsesInputItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	// function_call
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	// function_call_output
	Output json.RawMessage `json:"output"`
}

// hostToolTypes are Responses tool types the gateway cannot emulate over a
// chat backend. Silently dropping them would return a plausible answer with
// no evidence the tool never ran, so they are rejected by name.
var hostToolTypes = map[string]bool{
	"web_search":            true,
	"web_search_preview":    true,
	"file_search":           true,
	"code_interpreter":      true,
	"computer_use":          true,
	"computer_use_preview":  true,
	"mcp":                   true,
	"image_generation":      true,
	"local_shell":           true,
	"code_interpreter_call": true,
}

// responsesUnsupported returns a client-facing error when the request asks for
// something chat completions cannot honour.
//
// The asymmetry is deliberate. `store` defaults to true in the official SDKs,
// so rejecting it would fail every ordinary call — and ignoring it costs
// nothing, because the gateway never stored the response to begin with. But
// previous_response_id and conversation mean the client is *depending* on
// continuation: answering them statelessly returns a confident reply that
// silently lost the transcript. That must be an error naming the field.
func responsesUnsupported(req *responsesRequest) error {
	if raw := req.PreviousResponseID; len(raw) > 0 && string(raw) != "null" {
		return errors.New("previous_response_id is not supported: the gateway keeps no response state; send the full conversation in input")
	}
	if raw := req.Conversation; len(raw) > 0 && string(raw) != "null" {
		return errors.New("conversation is not supported: the gateway keeps no response state; send the full conversation in input")
	}
	if req.N != nil && *req.N > 1 {
		return errors.New("n is not supported: the Responses API returns a single generation; make separate requests for multiple candidates")
	}
	for _, tool := range req.Tools {
		if hostToolTypes[tool.Type] {
			return fmt.Errorf("tool %q requires a backend that serves the Responses API natively; it cannot be emulated over chat completions", tool.Type)
		}
	}
	return nil
}

// responsesToOpenAI converts a Responses request body into a chat-completions
// body plus the parsed request (for admission accounting).
func responsesToOpenAI(raw []byte) (map[string]any, *responsesRequest, error) {
	var req responsesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, nil, errors.New("invalid request JSON")
	}
	if err := responsesUnsupported(&req); err != nil {
		return nil, nil, err
	}
	messages, err := responsesMessages(&req)
	if err != nil {
		return nil, nil, err
	}
	// Instructions alone are not a conversation: require at least one
	// non-system turn, or the upstream has nothing to answer.
	hasUser := false
	for _, m := range messages {
		if mm, ok := m.(map[string]any); ok && mm["role"] != "system" {
			hasUser = true
			break
		}
	}
	if !hasUser {
		return nil, nil, errors.New("input is required")
	}
	// The agent-facing id is carried here and rewritten to the upstream
	// id by remapBody once a route is selected.
	body := map[string]any{"model": req.Model, "messages": messages}
	if req.Stream {
		body["stream"] = true
		// Required for the terminal usage chunk that becomes response.completed.
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if req.MaxOutput != nil {
		body["max_tokens"] = *req.MaxOutput
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if req.Parallel != nil {
		body["parallel_tool_calls"] = *req.Parallel
	}
	if tools := responsesTools(&req); len(tools) > 0 {
		body["tools"] = tools
		if choice, ok, err := responsesToolChoice(req.ToolChoice); err != nil {
			return nil, nil, err
		} else if ok {
			body["tool_choice"] = choice
		}
	}
	if err := responsesFormatInto(body, req.Text); err != nil {
		return nil, nil, err
	}
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		// remapBody drops this on backends that reject it.
		body["reasoning_effort"] = req.Reasoning.Effort
	}
	return body, &req, nil
}

// responsesMessages builds the chat message array from instructions plus the
// input, which may be a bare string or an array of typed items.
func responsesMessages(req *responsesRequest) ([]any, error) {
	messages := make([]any, 0, 4)
	if text, ok, err := responsesInstructions(req.Instructions); err != nil {
		return nil, err
	} else if ok {
		messages = append(messages, map[string]any{"role": "system", "content": text})
	}
	raw := strings.TrimSpace(string(req.Input))
	if raw == "" || raw == "null" {
		return messages, nil
	}
	// A bare string is the common shorthand: input: "hello".
	var inputText string
	if json.Unmarshal(req.Input, &inputText) == nil {
		if inputText != "" {
			messages = append(messages, map[string]any{"role": "user", "content": inputText})
		}
		return messages, nil
	}
	var items []responsesInputItem
	if err := json.Unmarshal(req.Input, &items); err != nil {
		return nil, errors.New("input must be a string or an array of items")
	}
	for _, item := range items {
		produced, err := responsesItemToMessages(item)
		if err != nil {
			return nil, err
		}
		messages = append(messages, produced...)
	}
	return messages, nil
}

// responsesItemToMessages converts one input item into zero or more chat
// messages. A single item can yield one message; the slice return keeps the
// call site uniform as item types grow.
func responsesItemToMessages(item responsesInputItem) ([]any, error) {
	switch item.Type {
	case "", "message":
		role := item.Role
		switch role {
		case "":
			role = "user"
		case "developer":
			// OpenAI's newer name for system; backends reject "developer".
			role = "system"
		case "user", "assistant", "system":
		default:
			return nil, fmt.Errorf("unsupported input message role %q", role)
		}
		content, err := responsesContent(item.Content)
		if err != nil {
			return nil, err
		}
		return []any{map[string]any{"role": role, "content": content}}, nil
	case "function_call":
		// An assistant turn that called a tool. arguments may be a JSON
		// string or a raw object; chat wants the string form.
		args := "{}"
		if len(item.Arguments) > 0 && string(item.Arguments) != "null" {
			var s string
			if json.Unmarshal(item.Arguments, &s) == nil {
				args = s
			} else {
				args = string(item.Arguments)
			}
		}
		return []any{map[string]any{
			"role":    "assistant",
			"content": nil,
			"tool_calls": []any{map[string]any{
				"id":       item.CallID,
				"type":     "function",
				"function": map[string]any{"name": item.Name, "arguments": args},
			}},
		}}, nil
	case "function_call_output":
		output := "{}"
		if len(item.Output) > 0 && string(item.Output) != "null" {
			var s string
			if json.Unmarshal(item.Output, &s) == nil {
				output = s
			} else {
				output = string(item.Output)
			}
		}
		return []any{map[string]any{
			"role":         "tool",
			"tool_call_id": item.CallID,
			"content":      output,
		}}, nil
	case "reasoning":
		// Reasoning items are echoed back when a client replays its own
		// transcript. Chat has no equivalent slot; the content is already
		// represented by the surrounding messages.
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported input item type %q", item.Type)
	}
}

// responsesContent turns a Responses content value (string or part array) into
// a chat content value: a plain string when text-only, a part array when any
// image or file part is present.
func responsesContent(raw json.RawMessage) (any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL string `json:"image_url"`
		FileID   string `json:"file_id"`
		FileData string `json:"file_data"`
		Filename string `json:"filename"`
		Detail   string `json:"detail"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, errors.New("input message content must be a string or an array of parts")
	}
	chatParts := make([]any, 0, len(parts))
	hasMedia := false
	var joined strings.Builder
	for _, part := range parts {
		switch part.Type {
		case "", "input_text", "text", "output_text":
			joined.WriteString(part.Text)
			chatParts = append(chatParts, map[string]any{"type": "text", "text": part.Text})
		case "refusal":
			joined.WriteString(part.Text)
			chatParts = append(chatParts, map[string]any{"type": "text", "text": part.Text})
		case "input_image":
			hasMedia = true
			image := map[string]any{"url": part.ImageURL}
			if part.Detail != "" {
				image["detail"] = part.Detail
			}
			chatParts = append(chatParts, map[string]any{"type": "image_url", "image_url": image})
		case "input_file":
			hasMedia = true
			url := part.FileID
			if part.FileData != "" {
				// Inline base64 data becomes a data URL the upstream can read
				// without a file-upload round trip it does not support.
				url = "data:application/octet-stream;base64," + part.FileData
			}
			chatParts = append(chatParts, map[string]any{
				"type":    "file",
				"file":    map[string]any{"file_data": strings.TrimPrefix(url, "data:application/octet-stream;base64,"), "filename": part.Filename},
				"file_id": part.FileID,
			})
		default:
			return nil, fmt.Errorf("unsupported content part type %q", part.Type)
		}
	}
	if !hasMedia {
		// Preserve the plain-string form so backends that only accept a
		// string content (most of them) see what they expect.
		return joined.String(), nil
	}
	return chatParts, nil
}

// responsesInstructions accepts the Responses "instructions" field, which is a
// string in practice but can arrive as a content-part array.
func responsesInstructions(raw json.RawMessage) (string, bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if text == "" {
			return "", false, nil
		}
		return text, true, nil
	}
	content, err := responsesContent(raw)
	if err != nil {
		return "", false, err
	}
	if s, ok := content.(string); ok {
		if s == "" {
			return "", false, nil
		}
		return s, true, nil
	}
	return "", false, errors.New("instructions must be a string or text parts")
}

// responsesTools converts internally-tagged Responses function tools to chat's
// externally-tagged form.
func responsesTools(req *responsesRequest) []any {
	out := make([]any, 0, len(req.Tools))
	for _, tool := range req.Tools {
		if tool.Type != "" && tool.Type != "function" {
			// Hosted tools were rejected earlier; anything else unknown
			// would be silently ignored by an upstream, so skip it here.
			continue
		}
		if tool.Name == "" {
			continue
		}
		fn := map[string]any{"name": tool.Name}
		if tool.Description != "" {
			fn["description"] = tool.Description
		}
		if len(tool.Parameters) > 0 && string(tool.Parameters) != "null" {
			fn["parameters"] = tool.Parameters
		} else {
			fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		if tool.Strict != nil {
			fn["strict"] = *tool.Strict
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// responsesToolChoice maps the Responses tool_choice vocabulary. "required"
// and "auto"/"none" are shared; a named function nests under "function".
func responsesToolChoice(raw json.RawMessage) (any, bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false, nil
	}
	var mode string
	if json.Unmarshal(raw, &mode) == nil {
		return mode, true, nil
	}
	var named struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &named); err != nil {
		return nil, false, errors.New("invalid tool_choice")
	}
	if named.Type != "function" || named.Name == "" {
		return nil, false, fmt.Errorf("unsupported tool_choice type %q", named.Type)
	}
	return map[string]any{"type": "function", "function": map[string]any{"name": named.Name}}, true, nil
}

// responsesFormatInto moves Structured Outputs from text.format to
// response_format, flattening the extra json_schema level.
func responsesFormatInto(body map[string]any, text *responsesText) error {
	if text == nil || text.Format == nil {
		return nil
	}
	f := text.Format
	switch f.Type {
	case "", "text":
		return nil // the default; no response_format needed
	case "json_object":
		body["response_format"] = map[string]any{"type": "json_object"}
		return nil
	case "json_schema":
		if len(f.Schema) == 0 || string(f.Schema) == "null" {
			return errors.New("text.format of type json_schema requires a schema")
		}
		name := f.Name
		if name == "" {
			name = "response"
		}
		inner := map[string]any{"name": name, "schema": f.Schema}
		if f.Strict != nil {
			inner["strict"] = *f.Strict
		}
		body["response_format"] = map[string]any{"type": "json_schema", "json_schema": inner}
		return nil
	default:
		return fmt.Errorf("unsupported text.format type %q", f.Type)
	}
}
