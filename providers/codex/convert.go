package codex

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/erain/glue/loop"
)

// responsesRequest is the JSON body sent to the Codex Responses
// endpoint. Fields mirror Codex CLI's ResponsesApiRequest in
// codex-rs/core/src/client.rs.
type responsesRequest struct {
	Model             string           `json:"model"`
	Instructions      string           `json:"instructions,omitempty"`
	Input             []responsesInput `json:"input"`
	Tools             []responsesTool  `json:"tools,omitempty"`
	ToolChoice        string           `json:"tool_choice,omitempty"`
	ParallelToolCalls bool             `json:"parallel_tool_calls"`
	Stream            bool             `json:"stream"`
	Store             bool             `json:"store"`
	Include           []string         `json:"include,omitempty"`
}

// responsesInput is one item in the Responses input array. The active
// fields depend on Type:
//
//   - "message": Role + Content
//   - "function_call": CallID + Name + Arguments
//   - "function_call_output": CallID + Output
type responsesInput struct {
	Type      string             `json:"type"`
	Role      string             `json:"role,omitempty"`
	Content   []responsesContent `json:"content,omitempty"`
	CallID    string             `json:"call_id,omitempty"`
	Name      string             `json:"name,omitempty"`
	Arguments string             `json:"arguments,omitempty"`
	Output    string             `json:"output,omitempty"`
}

// responsesContent is one content part inside a Responses message item.
// Type is "input_text" on user messages and "output_text" on assistant
// messages.
type responsesContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// responsesTool is the Responses tools-array shape. Note this is *not*
// the Chat Completions shape ({type:function, function:{...}}).
type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// messagesToInput converts a normalized message transcript into the
// Responses input array. Tool-call messages from the assistant become
// "function_call" items keyed by call_id; tool-role messages (results)
// become "function_call_output" items keyed by the same call_id.
//
// Returns an error if a message contains non-text user/assistant
// content (image / thinking are not supported in v0.1 per ADR-0006
// §3). Thinking content from assistant messages is silently dropped.
func messagesToInput(msgs []loop.Message) ([]responsesInput, error) {
	out := make([]responsesInput, 0, len(msgs)+1)
	for i, m := range msgs {
		switch m.Role {
		case loop.MessageRoleUser:
			parts, err := textPartsOnly(m.Content, "user")
			if err != nil {
				return nil, fmt.Errorf("message %d: %w", i, err)
			}
			if len(parts) == 0 {
				continue
			}
			out = append(out, responsesInput{
				Type:    "message",
				Role:    "user",
				Content: toInputContent("input_text", parts),
			})
		case loop.MessageRoleAssistant:
			text := assistantText(m.Content)
			if text != "" {
				out = append(out, responsesInput{
					Type:    "message",
					Role:    "assistant",
					Content: toInputContent("output_text", []string{text}),
				})
			}
			for _, p := range m.Content {
				if p.Type == loop.ContentTypeToolCall && p.ToolCall != nil {
					out = append(out, responsesInput{
						Type:      "function_call",
						CallID:    p.ToolCall.ID,
						Name:      p.ToolCall.Name,
						Arguments: string(p.ToolCall.Arguments),
					})
				}
			}
		case loop.MessageRoleTool:
			callID := m.ToolCallID
			if callID == "" {
				return nil, fmt.Errorf("message %d: tool result missing tool_call_id", i)
			}
			out = append(out, responsesInput{
				Type:   "function_call_output",
				CallID: callID,
				Output: toolResultText(m.Content),
			})
		}
	}
	return out, nil
}

// textPartsOnly returns the text fields of content, refusing image
// parts (which are out of scope for v0.1). Thinking parts on user
// messages are unusual and treated the same way.
func textPartsOnly(parts []loop.ContentPart, role string) ([]string, error) {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case loop.ContentTypeText:
			if p.Text != "" {
				out = append(out, p.Text)
			}
		case loop.ContentTypeImage:
			return nil, fmt.Errorf("%s: image content not supported by codex provider v0.1", role)
		}
	}
	return out, nil
}

// assistantText concatenates all text-typed content parts from an
// assistant message. Tool calls and thinking are ignored here; tool
// calls are emitted as separate function_call items.
func assistantText(parts []loop.ContentPart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Type == loop.ContentTypeText && p.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// toolResultText flattens a tool result's content parts into a single
// string the Responses API can carry on a function_call_output item.
func toolResultText(parts []loop.ContentPart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Type == loop.ContentTypeText && p.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func toInputContent(contentType string, texts []string) []responsesContent {
	out := make([]responsesContent, 0, len(texts))
	for _, t := range texts {
		out = append(out, responsesContent{Type: contentType, Text: t})
	}
	return out
}

// toolSpecsToTools converts glue tool specs to Responses tools.
// Parameters is forwarded as-is; an empty parameters block becomes
// "{}" so the schema validates.
func toolSpecsToTools(specs []loop.ToolSpec) []responsesTool {
	if len(specs) == 0 {
		return nil
	}
	out := make([]responsesTool, 0, len(specs))
	for _, s := range specs {
		params := s.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{}`)
		}
		out = append(out, responsesTool{
			Type:        "function",
			Name:        s.Name,
			Description: s.Description,
			Parameters:  params,
		})
	}
	return out
}
