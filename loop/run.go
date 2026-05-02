package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

const defaultMaxTurns = 32

// RunRequest configures one [Run].
//
// Provider is required. MaxTurns defaults to 32 when zero or negative. Emit
// receives a snapshot of every loop event in source order; nil disables event
// delivery. Messages, Tools, and Options are defensively copied so callers may
// reuse the input slices and maps.
//
// Parallel controls within-turn tool execution. When false (the default),
// tool calls in a single assistant message are executed sequentially in
// source order. When true, the loop fans them out concurrently and waits
// for all of them before appending tool-result messages — but the appended
// results, EventToolStart, and EventToolEnd are still emitted in assistant
// source order so the transcript stays deterministic.
type RunRequest struct {
	Provider     Provider
	Model        string
	SystemPrompt string
	Messages     []Message
	Tools        []Tool
	Options      map[string]any
	MaxTurns     int
	Parallel     bool
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
// Tool execution is sequential and deterministic: requested tool calls are
// executed in the order they appear in the assistant message, their result
// messages are appended in that same order, and unknown tools, invalid JSON
// arguments, missing executors, and executor errors all become tool-result
// messages with IsError=true so the model can see and react instead of the
// loop crashing.
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

		if err := ctx.Err(); err != nil {
			return fail(err)
		}

		toolMessages, err := executeToolCalls(ctx, req, toolCalls, emit)
		if err != nil {
			return fail(err)
		}
		messages = append(messages, toolMessages...)
		newMessages = append(newMessages, toolMessages...)

		emit(Event{Type: EventTurnEnd, Message: messagePtr(assistant)})
	}

	return fail(fmt.Errorf("loop: maximum turns exceeded (%d)", maxTurns))
}

// executeToolCalls runs every tool call requested by an assistant turn and
// returns the tool-result messages in assistant source order. The behavior
// is the same in sequential and parallel modes: source-ordered tool_start
// events, source-ordered tool_end events, source-ordered append. Parallel
// mode only changes when the executors run.
func executeToolCalls(ctx context.Context, req RunRequest, calls []ToolCall, emit func(Event)) ([]Message, error) {
	type result struct {
		call    ToolCall
		out     ToolResult
		message Message
	}

	results := make([]result, len(calls))
	if req.Parallel {
		var wg sync.WaitGroup
		for i, call := range calls {
			emit(Event{
				Type:       EventToolStart,
				ToolCall:   toolCallPtr(call),
				ToolCallID: call.ID,
				ToolName:   call.Name,
			})
			wg.Add(1)
			go func(i int, call ToolCall) {
				defer wg.Done()
				normalizedCall, toolResult := executeToolCall(ctx, req.Tools, call)
				results[i] = result{
					call:    normalizedCall,
					out:     toolResult,
					message: toolResultMessage(normalizedCall, toolResult),
				}
			}(i, call)
		}
		wg.Wait()
	} else {
		for i, call := range calls {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			emit(Event{
				Type:       EventToolStart,
				ToolCall:   toolCallPtr(call),
				ToolCallID: call.ID,
				ToolName:   call.Name,
			})
			normalizedCall, toolResult := executeToolCall(ctx, req.Tools, call)
			results[i] = result{
				call:    normalizedCall,
				out:     toolResult,
				message: toolResultMessage(normalizedCall, toolResult),
			}
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	messages := make([]Message, len(results))
	for i, r := range results {
		messages[i] = r.message
		emit(Event{
			Type:       EventToolEnd,
			ToolCall:   toolCallPtr(r.call),
			ToolCallID: r.call.ID,
			ToolName:   r.call.Name,
			ToolResult: toolResultPtr(r.out),
			Message:    messagePtr(r.message),
		})
	}
	return messages, nil
}

// executeToolCall normalizes a single tool call's arguments and invokes its
// executor. Unknown tools, missing executors, invalid JSON arguments, and
// executor errors all return a ToolResult with IsError=true so the loop can
// surface them to the model instead of failing the whole run.
//
// The first return value is the (possibly argument-normalized) tool call
// that should be referenced by the resulting tool message.
func executeToolCall(ctx context.Context, tools []Tool, call ToolCall) (ToolCall, ToolResult) {
	normalizedCall, err := normalizeToolCallArguments(call)
	if err != nil {
		return call, errorToolResult(fmt.Sprintf("invalid arguments for tool %q: %v", call.Name, err))
	}

	for _, tool := range tools {
		if tool.Name != normalizedCall.Name {
			continue
		}
		if tool.Execute == nil {
			return normalizedCall, errorToolResult(fmt.Sprintf("tool %q has no executor", normalizedCall.Name))
		}
		result, err := tool.Execute(ctx, normalizedCall)
		if err != nil {
			return normalizedCall, errorToolResult(err.Error())
		}
		return normalizedCall, result
	}
	return normalizedCall, errorToolResult(fmt.Sprintf("unknown tool %q", normalizedCall.Name))
}

// normalizeToolCallArguments enforces that arguments are a JSON object and
// substitutes "{}" for empty arguments. The returned call always has a
// non-empty Arguments slice.
func normalizeToolCallArguments(call ToolCall) (ToolCall, error) {
	if len(call.Arguments) == 0 {
		call.Arguments = json.RawMessage(`{}`)
		return call, nil
	}

	var args map[string]any
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return call, err
	}
	if args == nil {
		return call, errors.New("arguments must be a JSON object")
	}

	normalized, err := json.Marshal(args)
	if err != nil {
		return call, err
	}
	call.Arguments = normalized
	return call, nil
}

func errorToolResult(text string) ToolResult {
	return ToolResult{
		Content: []ContentPart{{Type: ContentTypeText, Text: text}},
		IsError: true,
	}
}

func toolResultMessage(call ToolCall, result ToolResult) Message {
	return Message{
		Role:       MessageRoleTool,
		Content:    cloneContent(result.Content),
		ToolCallID: call.ID,
		ToolName:   call.Name,
		IsError:    result.IsError,
		CreatedAt:  time.Now().UTC(),
		Metadata:   cloneMetadata(result.Metadata),
	}
}

func toolResultPtr(result ToolResult) *ToolResult {
	cloned := ToolResult{
		Content:  cloneContent(result.Content),
		IsError:  result.IsError,
		Metadata: cloneMetadata(result.Metadata),
	}
	return &cloned
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
