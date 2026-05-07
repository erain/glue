package loop

import (
	"context"
	"encoding/json"
	"time"
)

// MessageRole identifies which actor produced a transcript message.
type MessageRole string

const (
	MessageRoleUser      MessageRole = "user"
	MessageRoleAssistant MessageRole = "assistant"
	MessageRoleTool      MessageRole = "tool"
)

// ContentType identifies the active payload field in a [ContentPart].
type ContentType string

const (
	ContentTypeText     ContentType = "text"
	ContentTypeThinking ContentType = "thinking"
	ContentTypeImage    ContentType = "image"
	ContentTypeToolCall ContentType = "tool_call"
)

// StopReason explains why a provider finished an assistant turn.
type StopReason string

const (
	StopReasonStop     StopReason = "stop"
	StopReasonLength   StopReason = "length"
	StopReasonToolUse  StopReason = "tool_use"
	StopReasonError    StopReason = "error"
	StopReasonCanceled StopReason = "canceled"
	// StopReasonMaxTurns marks the last assistant message in a run
	// that exited because the loop turn budget (RunRequest.MaxTurns)
	// was exhausted while the assistant still had pending tool calls.
	// Distinguishes "we ran out of budget" from "the model finished"
	// (Stop) and from provider-side truncation (Length), so agents can
	// retry with a higher budget cleanly.
	StopReasonMaxTurns StopReason = "max_turns"
)

// ContentPart is a provider-neutral message content block.
//
// Type selects which payload field is meaningful; other fields stay zero.
// The struct shape is intentionally JSON-friendly so transcripts can be
// persisted by file-backed stores without a separate wire format.
type ContentPart struct {
	Type      ContentType   `json:"type"`
	Text      string        `json:"text,omitempty"`
	Thinking  string        `json:"thinking,omitempty"`
	Image     *ImageContent `json:"image,omitempty"`
	ToolCall  *ToolCall     `json:"tool_call,omitempty"`
	Signature string        `json:"signature,omitempty"`
}

// ImageContent stores an inline base64-encoded image.
type ImageContent struct {
	Data     string `json:"data"`
	MIMEType string `json:"mime_type"`
}

// ToolCall is a model request to invoke a named tool with JSON arguments.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Message is a normalized transcript entry.
type Message struct {
	ID         string         `json:"id,omitempty"`
	Role       MessageRole    `json:"role"`
	Content    []ContentPart  `json:"content,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolName   string         `json:"tool_name,omitempty"`
	IsError    bool           `json:"is_error,omitempty"`
	Provider   string         `json:"provider,omitempty"`
	Model      string         `json:"model,omitempty"`
	StopReason StopReason     `json:"stop_reason,omitempty"`
	Usage      *Usage         `json:"usage,omitempty"`
	CreatedAt  time.Time      `json:"created_at,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

// Usage captures token accounting reported by a provider when available.
type Usage struct {
	InputTokens      int64 `json:"input_tokens,omitempty"`
	OutputTokens     int64 `json:"output_tokens,omitempty"`
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	TotalTokens      int64 `json:"total_tokens,omitempty"`
}

// ToolSpec is the provider-visible description of a tool.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ToolExecutor runs a tool call locally and returns a normalized result.
type ToolExecutor func(ctx context.Context, call ToolCall) (ToolResult, error)

// Tool combines the provider-visible specification with its local executor.
// The Execute field is intentionally tagged json:"-" so transcripts and
// provider payloads exclude it.
type Tool struct {
	ToolSpec
	Execute ToolExecutor `json:"-"`
}

// ToolResult is the local result produced by a [ToolExecutor].
type ToolResult struct {
	Content  []ContentPart  `json:"content,omitempty"`
	IsError  bool           `json:"is_error,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// ProviderRequest is the normalized input sent to a provider for one assistant
// turn.
type ProviderRequest struct {
	Model        string         `json:"model"`
	SystemPrompt string         `json:"system_prompt,omitempty"`
	Messages     []Message      `json:"messages,omitempty"`
	Tools        []ToolSpec     `json:"tools,omitempty"`
	Options      map[string]any `json:"options,omitempty"`
}

// Provider streams assistant events for a single assistant turn. Implementations
// must close the returned channel when the turn ends, including on error.
type Provider interface {
	Stream(ctx context.Context, req ProviderRequest) (<-chan ProviderEvent, error)
}

// ProviderEventType identifies events emitted by a provider stream.
type ProviderEventType string

const (
	ProviderEventStart         ProviderEventType = "start"
	ProviderEventTextDelta     ProviderEventType = "text_delta"
	ProviderEventThinkingDelta ProviderEventType = "thinking_delta"
	ProviderEventToolCall      ProviderEventType = "tool_call"
	ProviderEventDone          ProviderEventType = "done"
	ProviderEventError         ProviderEventType = "error"
)

// ProviderEvent is a provider-neutral streaming event. The active fields
// depend on Type: TextDelta uses Delta, ToolCall uses ToolCall, Done uses
// Message, Error uses Error.
type ProviderEvent struct {
	Type         ProviderEventType `json:"type"`
	Message      *Message          `json:"message,omitempty"`
	Delta        string            `json:"delta,omitempty"`
	ContentIndex int               `json:"content_index,omitempty"`
	ToolCall     *ToolCall         `json:"tool_call,omitempty"`
	Error        string            `json:"error,omitempty"`
}

// EventType identifies events emitted by the agent loop.
type EventType string

const (
	EventLoopStart    EventType = "loop_start"
	EventLoopEnd      EventType = "loop_end"
	EventTurnStart    EventType = "turn_start"
	EventTurnEnd      EventType = "turn_end"
	EventMessageStart EventType = "message_start"
	EventMessageEnd   EventType = "message_end"
	EventTextDelta    EventType = "text_delta"
	EventToolStart    EventType = "tool_start"
	EventToolEnd      EventType = "tool_end"
	EventError        EventType = "error"
)

// Event is emitted by the agent loop for callers such as sessions and CLIs.
// The active fields depend on Type.
type Event struct {
	Type       EventType      `json:"type"`
	Message    *Message       `json:"message,omitempty"`
	Messages   []Message      `json:"messages,omitempty"`
	Delta      string         `json:"delta,omitempty"`
	ToolCall   *ToolCall      `json:"tool_call,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolName   string         `json:"tool_name,omitempty"`
	ToolResult *ToolResult    `json:"tool_result,omitempty"`
	Error      string         `json:"error,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}
