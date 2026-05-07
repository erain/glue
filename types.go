package glue

import "github.com/erain/glue/loop"

// Re-export the loop package's normalized types as part of the public API so
// callers only need to import `glue`.
type (
	Message      = loop.Message
	MessageRole  = loop.MessageRole
	ContentPart  = loop.ContentPart
	ContentType  = loop.ContentType
	ImageContent = loop.ImageContent
	ToolCall     = loop.ToolCall
	ToolResult   = loop.ToolResult
	ToolSpec     = loop.ToolSpec
	ToolExecutor = loop.ToolExecutor
	Tool         = loop.Tool
	Usage        = loop.Usage
	StopReason   = loop.StopReason

	ProviderRequest   = loop.ProviderRequest
	Provider          = loop.Provider
	ProviderEvent     = loop.ProviderEvent
	ProviderEventType = loop.ProviderEventType

	Event     = loop.Event
	EventType = loop.EventType
)

// MessageRole constants re-exported from package loop.
const (
	MessageRoleUser      = loop.MessageRoleUser
	MessageRoleAssistant = loop.MessageRoleAssistant
	MessageRoleTool      = loop.MessageRoleTool
)

// ContentType constants re-exported from package loop.
const (
	ContentTypeText     = loop.ContentTypeText
	ContentTypeThinking = loop.ContentTypeThinking
	ContentTypeImage    = loop.ContentTypeImage
	ContentTypeToolCall = loop.ContentTypeToolCall
)

// StopReason constants re-exported from package loop.
const (
	StopReasonStop     = loop.StopReasonStop
	StopReasonLength   = loop.StopReasonLength
	StopReasonToolUse  = loop.StopReasonToolUse
	StopReasonError    = loop.StopReasonError
	StopReasonCanceled = loop.StopReasonCanceled
	StopReasonMaxTurns = loop.StopReasonMaxTurns
)

// EventType constants re-exported from package loop.
const (
	EventLoopStart    = loop.EventLoopStart
	EventLoopEnd      = loop.EventLoopEnd
	EventTurnStart    = loop.EventTurnStart
	EventTurnEnd      = loop.EventTurnEnd
	EventMessageStart = loop.EventMessageStart
	EventMessageEnd   = loop.EventMessageEnd
	EventTextDelta    = loop.EventTextDelta
	EventToolStart    = loop.EventToolStart
	EventToolEnd      = loop.EventToolEnd
	EventError        = loop.EventError
)

// ProviderEventType constants re-exported from package loop.
const (
	ProviderEventStart         = loop.ProviderEventStart
	ProviderEventTextDelta     = loop.ProviderEventTextDelta
	ProviderEventThinkingDelta = loop.ProviderEventThinkingDelta
	ProviderEventToolCall      = loop.ProviderEventToolCall
	ProviderEventDone          = loop.ProviderEventDone
	ProviderEventError         = loop.ProviderEventError
)
