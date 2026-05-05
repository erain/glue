package nvidia

import (
	"encoding/json"
	"fmt"

	"github.com/erain/glue/loop"
)

// chatMessage is the OpenAI-compatible request message shape.
type chatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	Name       string         `json:"name,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
}

type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function chatToolFunction `json:"function"`
}

type chatToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatFuncSpec `json:"function"`
}

type chatFuncSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// chatRequest is the JSON body sent to /v1/chat/completions.
type chatRequest struct {
	Model          string            `json:"model"`
	Messages       []chatMessage     `json:"messages"`
	Tools          []chatTool        `json:"tools,omitempty"`
	Stream         bool              `json:"stream"`
	StreamOptions  *streamOptions    `json:"stream_options,omitempty"`
	Temperature    *float64          `json:"temperature,omitempty"`
	MaxTokens      *int              `json:"max_tokens,omitempty"`
	TopP           *float64          `json:"top_p,omitempty"`
	ResponseFormat json.RawMessage   `json:"response_format,omitempty"`
	Extra          map[string]any    `json:"-"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// MarshalJSON encodes the request including any keys carried in Extra.
// Extra wins on collision, allowing callers to pass through provider-specific
// options not represented as struct fields.
func (r chatRequest) MarshalJSON() ([]byte, error) {
	type alias chatRequest
	base, err := json.Marshal(alias(r))
	if err != nil {
		return nil, err
	}
	if len(r.Extra) == 0 {
		return base, nil
	}
	merged := map[string]any{}
	if err := json.Unmarshal(base, &merged); err != nil {
		return nil, err
	}
	for k, v := range r.Extra {
		merged[k] = v
	}
	return json.Marshal(merged)
}

// buildChatRequest converts a Glue ProviderRequest into the OpenAI-compatible
// request body. The caller fills in Model and Stream separately.
func buildChatRequest(req loop.ProviderRequest) (chatRequest, error) {
	messages, err := convertMessages(req.SystemPrompt, req.Messages)
	if err != nil {
		return chatRequest{}, err
	}
	tools, err := convertTools(req.Tools)
	if err != nil {
		return chatRequest{}, err
	}
	body := chatRequest{
		Messages: messages,
		Tools:    tools,
	}
	if err := applyOptions(&body, req.Options); err != nil {
		return chatRequest{}, err
	}
	return body, nil
}

func convertMessages(systemPrompt string, messages []loop.Message) ([]chatMessage, error) {
	out := make([]chatMessage, 0, len(messages)+1)
	if systemPrompt != "" {
		out = append(out, chatMessage{Role: "system", Content: systemPrompt})
	}
	for _, message := range messages {
		converted, err := convertMessage(message)
		if err != nil {
			return nil, err
		}
		if converted != nil {
			out = append(out, *converted)
		}
	}
	return out, nil
}

func convertMessage(message loop.Message) (*chatMessage, error) {
	switch message.Role {
	case loop.MessageRoleUser:
		text := joinText(message.Content)
		return &chatMessage{Role: "user", Content: text}, nil
	case loop.MessageRoleAssistant:
		out := chatMessage{Role: "assistant"}
		text := ""
		for _, part := range message.Content {
			switch part.Type {
			case loop.ContentTypeText:
				text += part.Text
			case loop.ContentTypeThinking:
				// reasoning_content does not round-trip in OpenAI request
				// schema; drop on replay rather than fail.
			case loop.ContentTypeToolCall:
				if part.ToolCall == nil {
					return nil, fmt.Errorf("nvidia: tool call content missing payload")
				}
				args := string(part.ToolCall.Arguments)
				if args == "" {
					args = "{}"
				}
				out.ToolCalls = append(out.ToolCalls, chatToolCall{
					ID:   part.ToolCall.ID,
					Type: "function",
					Function: chatToolFunction{
						Name:      part.ToolCall.Name,
						Arguments: args,
					},
				})
			case loop.ContentTypeImage:
				return nil, fmt.Errorf("nvidia: image content not yet supported")
			default:
				return nil, fmt.Errorf("nvidia: unsupported content type %q", part.Type)
			}
		}
		out.Content = text
		if out.Content == "" && len(out.ToolCalls) == 0 {
			return nil, nil
		}
		return &out, nil
	case loop.MessageRoleTool:
		if message.ToolCallID == "" {
			return nil, fmt.Errorf("nvidia: tool message missing tool_call_id")
		}
		return &chatMessage{
			Role:       "tool",
			ToolCallID: message.ToolCallID,
			Content:    joinText(message.Content),
		}, nil
	default:
		return nil, fmt.Errorf("nvidia: unsupported message role %q", message.Role)
	}
}

func joinText(parts []loop.ContentPart) string {
	var text string
	for _, part := range parts {
		if part.Type == loop.ContentTypeText {
			text += part.Text
		}
	}
	return text
}

func convertTools(tools []loop.ToolSpec) ([]chatTool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]chatTool, 0, len(tools))
	for _, tool := range tools {
		spec := chatFuncSpec{
			Name:        tool.Name,
			Description: tool.Description,
		}
		if len(tool.Parameters) > 0 {
			// Validate that parameters is a JSON object (or null) so the
			// upstream API does not reject the entire request.
			var probe any
			if err := json.Unmarshal(tool.Parameters, &probe); err != nil {
				return nil, fmt.Errorf("nvidia: tool %q parameters: %w", tool.Name, err)
			}
			spec.Parameters = append(json.RawMessage(nil), tool.Parameters...)
		}
		out = append(out, chatTool{Type: "function", Function: spec})
	}
	return out, nil
}

func applyOptions(body *chatRequest, options map[string]any) error {
	if len(options) == 0 {
		return nil
	}
	for key, value := range options {
		switch key {
		case "temperature":
			f, ok := numberAsFloat64(value)
			if !ok {
				return fmt.Errorf("nvidia: temperature must be numeric")
			}
			body.Temperature = &f
		case "top_p":
			f, ok := numberAsFloat64(value)
			if !ok {
				return fmt.Errorf("nvidia: top_p must be numeric")
			}
			body.TopP = &f
		case "max_tokens", "max_output_tokens":
			n, ok := numberAsInt(value)
			if !ok {
				return fmt.Errorf("nvidia: %s must be numeric", key)
			}
			body.MaxTokens = &n
		case "response_format":
			raw, err := json.Marshal(value)
			if err != nil {
				return fmt.Errorf("nvidia: response_format: %w", err)
			}
			body.ResponseFormat = raw
		default:
			if body.Extra == nil {
				body.Extra = map[string]any{}
			}
			body.Extra[key] = value
		}
	}
	return nil
}

func numberAsFloat64(value any) (float64, bool) {
	switch v := value.(type) {
	case float32:
		return float64(v), true
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int32:
		return float64(v), true
	case int64:
		return float64(v), true
	default:
		return 0, false
	}
}

func numberAsInt(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int32:
		return int(v), true
	case int64:
		return int(v), true
	case float32:
		return int(v), true
	case float64:
		return int(v), true
	default:
		return 0, false
	}
}

// mapFinishReason maps an OpenAI-shape finish_reason onto a Glue StopReason.
func mapFinishReason(reason string) loop.StopReason {
	switch reason {
	case "", "stop":
		return loop.StopReasonStop
	case "length":
		return loop.StopReasonLength
	case "tool_calls", "function_call":
		return loop.StopReasonToolUse
	case "content_filter":
		return loop.StopReasonError
	default:
		return loop.StopReasonStop
	}
}
