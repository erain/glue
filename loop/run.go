package loop

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const defaultMaxTurns = 32

// RunRequest configures one [Run].
//
// Provider is required. MaxTurns defaults to 32 when zero or negative. Emit
// receives a snapshot of every loop event in source order; nil disables event
// delivery. Messages, Tools, and Options are defensively copied so callers may
// reuse the input slices and maps.
type RunRequest struct {
	Provider     Provider
	Model        string
	SystemPrompt string
	Messages     []Message
	Tools        []Tool
	Options      map[string]any
	MaxTurns     int
	Emit         func(Event)
}

// RunResult is returned by [Run]. Messages is the full transcript including
// the input messages and every message produced during this run. NewMessages
// is just the messages produced during this run, in append order.
type RunResult struct {
	Messages    []Message
	NewMessages []Message
}

// Run executes a provider-agnostic agent loop until the assistant stops
// requesting tools, the provider errors, the context is canceled, or
// MaxTurns is reached.
//
// Tool execution in this implementation is intentionally minimal: tools are
// matched by name and called with the raw assistant arguments. Argument
// normalization, error-as-tool-result conversion, and ordering guarantees
// belong to the deterministic sequential execution path landing in a
// follow-up issue.
func Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if req.Provider == nil {
		return RunResult{}, errors.New("loop: provider is required")
	}

	maxTurns := req.MaxTurns
	if maxTurns <= 0 {
		maxTurns = defaultMaxTurns
	}

	messages := cloneMessages(req.Messages)
	newMessages := make([]Message, 0)
	emit := func(e Event) {
		if req.Emit != nil {
			req.Emit(e)
		}
	}
	snapshot := func() RunResult {
		return RunResult{Messages: cloneMessages(messages), NewMessages: cloneMessages(newMessages)}
	}
	fail := func(err error) (RunResult, error) {
		emit(Event{Type: EventError, Error: err.Error()})
		return snapshot(), err
	}

	emit(Event{Type: EventLoopStart})
	defer func() {
		emit(Event{Type: EventLoopEnd, Messages: cloneMessages(newMessages)})
	}()

	for turn := 0; turn < maxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}

		emit(Event{Type: EventTurnStart})
		assistant, err := runAssistantTurn(ctx, req, messages, emit)
		if err != nil {
			return fail(err)
		}

		messages = append(messages, assistant)
		newMessages = append(newMessages, assistant)

		toolCalls := collectToolCalls(assistant)
		if len(toolCalls) == 0 {
			emit(Event{Type: EventTurnEnd, Message: messagePtr(assistant)})
			return snapshot(), nil
		}

		for _, call := range toolCalls {
			if err := ctx.Err(); err != nil {
				return fail(err)
			}

			emit(Event{
				Type:       EventToolStart,
				ToolCall:   toolCallPtr(call),
				ToolCallID: call.ID,
				ToolName:   call.Name,
			})

			toolMessage, err := executeBasicTool(ctx, req.Tools, call)
			if err != nil {
				return fail(err)
			}

			messages = append(messages, toolMessage)
			newMessages = append(newMessages, toolMessage)

			emit(Event{
				Type:       EventToolEnd,
				ToolCall:   toolCallPtr(call),
				ToolCallID: call.ID,
				ToolName:   call.Name,
				Message:    messagePtr(toolMessage),
			})
		}

		emit(Event{Type: EventTurnEnd, Message: messagePtr(assistant)})
	}

	return fail(fmt.Errorf("loop: maximum turns exceeded (%d)", maxTurns))
}

// executeBasicTool resolves a tool by name and invokes its executor with the
// raw assistant arguments. Errors propagate; richer error-as-tool-result
// behavior lands in the deterministic sequential execution issue.
func executeBasicTool(ctx context.Context, tools []Tool, call ToolCall) (Message, error) {
	for _, tool := range tools {
		if tool.Name != call.Name {
			continue
		}
		if tool.Execute == nil {
			return Message{}, fmt.Errorf("loop: tool %q has no executor", call.Name)
		}
		result, err := tool.Execute(ctx, call)
		if err != nil {
			return Message{}, fmt.Errorf("loop: tool %q execute: %w", call.Name, err)
		}
		return Message{
			Role:       MessageRoleTool,
			Content:    cloneContent(result.Content),
			ToolCallID: call.ID,
			ToolName:   call.Name,
			IsError:    result.IsError,
			CreatedAt:  time.Now().UTC(),
			Metadata:   cloneMetadata(result.Metadata),
		}, nil
	}
	return Message{}, fmt.Errorf("loop: unknown tool %q", call.Name)
}

func runAssistantTurn(ctx context.Context, req RunRequest, messages []Message, emit func(Event)) (Message, error) {
	stream, err := req.Provider.Stream(ctx, ProviderRequest{
		Model:        req.Model,
		SystemPrompt: req.SystemPrompt,
		Messages:     cloneMessages(messages),
		Tools:        toolSpecs(req.Tools),
		Options:      cloneOptions(req.Options),
	})
	if err != nil {
		return Message{}, fmt.Errorf("loop: provider stream: %w", err)
	}

	var assistant Message
	started := false
	done := false

	ensureStarted := func() {
		if started {
			return
		}
		started = true
		assistant = Message{Role: MessageRoleAssistant, Model: req.Model, CreatedAt: time.Now().UTC()}
		emit(Event{Type: EventMessageStart, Message: messagePtr(assistant)})
	}

	for {
		select {
		case <-ctx.Done():
			return Message{}, ctx.Err()
		case event, ok := <-stream:
			if !ok {
				if !done {
					return Message{}, errors.New("loop: provider stream closed before done event")
				}
				return assistant, nil
			}

			switch event.Type {
			case ProviderEventStart:
				if event.Message != nil {
					assistant = *event.Message
				} else {
					assistant = Message{Role: MessageRoleAssistant, Model: req.Model, CreatedAt: time.Now().UTC()}
				}
				if assistant.Role == "" {
					assistant.Role = MessageRoleAssistant
				}
				if assistant.CreatedAt.IsZero() {
					assistant.CreatedAt = time.Now().UTC()
				}
				started = true
				emit(Event{Type: EventMessageStart, Message: messagePtr(assistant)})

			case ProviderEventTextDelta:
				ensureStarted()
				appendDelta(&assistant, ContentTypeText, event.ContentIndex, event.Delta)
				emit(Event{Type: EventTextDelta, Message: messagePtr(assistant), Delta: event.Delta})

			case ProviderEventThinkingDelta:
				ensureStarted()
				appendDelta(&assistant, ContentTypeThinking, event.ContentIndex, event.Delta)

			case ProviderEventToolCall:
				ensureStarted()
				if event.ToolCall == nil {
					return Message{}, errors.New("loop: provider tool_call event missing tool call")
				}
				call := *event.ToolCall
				assistant.Content = append(assistant.Content, ContentPart{Type: ContentTypeToolCall, ToolCall: &call})

			case ProviderEventDone:
				ensureStarted()
				if event.Message != nil {
					assistant = *event.Message
					if assistant.Role == "" {
						assistant.Role = MessageRoleAssistant
					}
				}
				if assistant.CreatedAt.IsZero() {
					assistant.CreatedAt = time.Now().UTC()
				}
				if assistant.StopReason == "" {
					if len(collectToolCalls(assistant)) > 0 {
						assistant.StopReason = StopReasonToolUse
					} else {
						assistant.StopReason = StopReasonStop
					}
				}
				done = true
				emit(Event{Type: EventMessageEnd, Message: messagePtr(assistant)})

			case ProviderEventError:
				if event.Error == "" {
					return Message{}, errors.New("loop: provider error")
				}
				return Message{}, errors.New(event.Error)

			default:
				return Message{}, fmt.Errorf("loop: unknown provider event type %q", event.Type)
			}
		}
	}
}

func appendDelta(message *Message, kind ContentType, index int, delta string) {
	if index >= 0 && index < len(message.Content) && message.Content[index].Type == kind {
		appendToPart(&message.Content[index], delta)
		return
	}
	last := len(message.Content) - 1
	if last >= 0 && message.Content[last].Type == kind {
		appendToPart(&message.Content[last], delta)
		return
	}
	part := ContentPart{Type: kind}
	appendToPart(&part, delta)
	message.Content = append(message.Content, part)
}

func appendToPart(part *ContentPart, delta string) {
	switch part.Type {
	case ContentTypeText:
		part.Text += delta
	case ContentTypeThinking:
		part.Thinking += delta
	}
}

func collectToolCalls(message Message) []ToolCall {
	calls := make([]ToolCall, 0)
	for _, part := range message.Content {
		if part.Type == ContentTypeToolCall && part.ToolCall != nil {
			calls = append(calls, *part.ToolCall)
		}
	}
	return calls
}

func toolSpecs(tools []Tool) []ToolSpec {
	specs := make([]ToolSpec, 0, len(tools))
	for _, tool := range tools {
		specs = append(specs, tool.ToolSpec)
	}
	return specs
}

func cloneMessages(messages []Message) []Message {
	if len(messages) == 0 {
		return nil
	}
	out := make([]Message, len(messages))
	for i, m := range messages {
		out[i] = cloneMessage(m)
	}
	return out
}

func cloneMessage(message Message) Message {
	message.Content = cloneContent(message.Content)
	message.Metadata = cloneMetadata(message.Metadata)
	if message.Usage != nil {
		usage := *message.Usage
		message.Usage = &usage
	}
	return message
}

func messagePtr(message Message) *Message {
	cloned := cloneMessage(message)
	return &cloned
}

func toolCallPtr(call ToolCall) *ToolCall {
	cloned := call
	if len(cloned.Arguments) > 0 {
		cloned.Arguments = append(cloned.Arguments[:0:0], cloned.Arguments...)
	}
	return &cloned
}

func cloneContent(content []ContentPart) []ContentPart {
	if len(content) == 0 {
		return nil
	}
	out := make([]ContentPart, len(content))
	copy(out, content)
	for i := range out {
		if out[i].Image != nil {
			image := *out[i].Image
			out[i].Image = &image
		}
		if out[i].ToolCall != nil {
			call := *out[i].ToolCall
			if len(call.Arguments) > 0 {
				call.Arguments = append(call.Arguments[:0:0], call.Arguments...)
			}
			out[i].ToolCall = &call
		}
	}
	return out
}

func cloneOptions(options map[string]any) map[string]any { return cloneMetadata(options) }

func cloneMetadata(metadata map[string]any) map[string]any {
	if len(metadata) == 0 {
		return nil
	}
	out := make(map[string]any, len(metadata))
	for k, v := range metadata {
		out[k] = v
	}
	return out
}
