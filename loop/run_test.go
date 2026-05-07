package loop

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProvider scripts a sequence of provider responses, one per assistant turn.
type fakeProvider struct {
	mu     sync.Mutex
	turns  [][]ProviderEvent
	calls  int
	openCh chan ProviderEvent // optional: returned from the first call when set
}

func (p *fakeProvider) Stream(ctx context.Context, _ ProviderRequest) (<-chan ProviderEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.openCh != nil {
		ch := p.openCh
		p.openCh = nil
		return ch, nil
	}
	if p.calls >= len(p.turns) {
		ch := make(chan ProviderEvent)
		close(ch)
		return ch, nil
	}
	events := p.turns[p.calls]
	p.calls++
	ch := make(chan ProviderEvent, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

type erroringProvider struct{ err error }

func (p erroringProvider) Stream(context.Context, ProviderRequest) (<-chan ProviderEvent, error) {
	return nil, p.err
}

func TestRunRequiresProvider(t *testing.T) {
	t.Parallel()
	if _, err := Run(context.Background(), RunRequest{}); err == nil {
		t.Fatal("err = nil, want error")
	}
}

func TestRunTextOnlyResponse(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{turns: [][]ProviderEvent{{
		{Type: ProviderEventStart},
		{Type: ProviderEventTextDelta, Delta: "hello"},
		{Type: ProviderEventTextDelta, Delta: " world"},
		{Type: ProviderEventDone},
	}}}

	var seen []EventType
	res, err := Run(context.Background(), RunRequest{
		Provider: provider,
		Model:    "fake-1",
		Messages: []Message{{Role: MessageRoleUser, Content: []ContentPart{{Type: ContentTypeText, Text: "hi"}}}},
		Emit:     func(e Event) { seen = append(seen, e.Type) },
	})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if len(res.NewMessages) != 1 {
		t.Fatalf("new messages = %d, want 1", len(res.NewMessages))
	}
	final := res.NewMessages[0]
	if final.Role != MessageRoleAssistant {
		t.Fatalf("role = %q, want assistant", final.Role)
	}
	if got := assistantText(final); got != "hello world" {
		t.Fatalf("text = %q, want %q", got, "hello world")
	}
	if final.StopReason != StopReasonStop {
		t.Fatalf("stop reason = %q, want stop", final.StopReason)
	}
	if len(res.Messages) != 2 {
		t.Fatalf("transcript = %d, want 2 (1 user + 1 assistant)", len(res.Messages))
	}
	if seen[0] != EventLoopStart || seen[len(seen)-1] != EventLoopEnd {
		t.Fatalf("event bookends = %q...%q, want loop_start...loop_end (full: %v)",
			seen[0], seen[len(seen)-1], seen)
	}
}

func TestRunRepeatedToolTurns(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{turns: [][]ProviderEvent{
		{
			{Type: ProviderEventStart},
			{Type: ProviderEventToolCall, ToolCall: &ToolCall{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{"v":"a"}`)}},
			{Type: ProviderEventDone},
		},
		{
			{Type: ProviderEventStart},
			{Type: ProviderEventToolCall, ToolCall: &ToolCall{ID: "c2", Name: "echo", Arguments: json.RawMessage(`{"v":"b"}`)}},
			{Type: ProviderEventDone},
		},
		{
			{Type: ProviderEventStart},
			{Type: ProviderEventTextDelta, Delta: "done"},
			{Type: ProviderEventDone},
		},
	}}

	echo := Tool{
		ToolSpec: ToolSpec{Name: "echo", Description: "echoes v"},
		Execute: func(_ context.Context, call ToolCall) (ToolResult, error) {
			var args map[string]string
			if err := json.Unmarshal(call.Arguments, &args); err != nil {
				return ToolResult{}, err
			}
			return ToolResult{Content: []ContentPart{{Type: ContentTypeText, Text: args["v"]}}}, nil
		},
	}

	res, err := Run(context.Background(), RunRequest{
		Provider: provider,
		Tools:    []Tool{echo},
		Messages: []Message{{Role: MessageRoleUser, Content: []ContentPart{{Type: ContentTypeText, Text: "go"}}}},
	})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}

	wantRoles := []MessageRole{MessageRoleAssistant, MessageRoleTool, MessageRoleAssistant, MessageRoleTool, MessageRoleAssistant}
	if len(res.NewMessages) != len(wantRoles) {
		t.Fatalf("new messages = %d, want %d", len(res.NewMessages), len(wantRoles))
	}
	for i, role := range wantRoles {
		if res.NewMessages[i].Role != role {
			t.Fatalf("new[%d].Role = %q, want %q", i, res.NewMessages[i].Role, role)
		}
	}
	if got := res.NewMessages[1].Content[0].Text; got != "a" {
		t.Fatalf("tool1 result = %q, want a", got)
	}
	if got := res.NewMessages[3].Content[0].Text; got != "b" {
		t.Fatalf("tool2 result = %q, want b", got)
	}
	if res.NewMessages[1].ToolCallID != "c1" || res.NewMessages[3].ToolCallID != "c2" {
		t.Fatalf("tool call ids = %q,%q, want c1,c2",
			res.NewMessages[1].ToolCallID, res.NewMessages[3].ToolCallID)
	}
	if got := assistantText(res.NewMessages[4]); got != "done" {
		t.Fatalf("final text = %q, want done", got)
	}
	if len(res.Messages) != 1+len(wantRoles) {
		t.Fatalf("transcript = %d, want %d", len(res.Messages), 1+len(wantRoles))
	}
}

func TestRunProviderStreamEventError(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{turns: [][]ProviderEvent{{
		{Type: ProviderEventStart},
		{Type: ProviderEventError, Error: "boom"},
	}}}

	var seenError string
	_, err := Run(context.Background(), RunRequest{
		Provider: provider,
		Emit: func(e Event) {
			if e.Type == EventError {
				seenError = e.Error
			}
		},
	})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want containing 'boom'", err)
	}
	if seenError != err.Error() {
		t.Fatalf("emitted error = %q, want %q", seenError, err.Error())
	}
}

func TestRunProviderStreamReturnError(t *testing.T) {
	t.Parallel()

	want := errors.New("network down")
	_, err := Run(context.Background(), RunRequest{Provider: erroringProvider{err: want}})
	if err == nil || !errors.Is(err, want) {
		t.Fatalf("err = %v, want wrapping %v", err, want)
	}
}

func TestRunContextPreCanceled(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{turns: [][]ProviderEvent{{{Type: ProviderEventStart}, {Type: ProviderEventDone}}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, RunRequest{Provider: provider})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRunContextCancelMidStream(t *testing.T) {
	t.Parallel()

	open := make(chan ProviderEvent)
	provider := &fakeProvider{openCh: open}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := Run(ctx, RunRequest{Provider: provider})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	close(open)
}

func TestRunMaxTurnsExplicit(t *testing.T) {
	t.Parallel()

	turns := make([][]ProviderEvent, 5)
	for i := range turns {
		turns[i] = []ProviderEvent{
			{Type: ProviderEventStart},
			{Type: ProviderEventToolCall, ToolCall: &ToolCall{ID: "x", Name: "noop", Arguments: json.RawMessage(`{}`)}},
			{Type: ProviderEventDone},
		}
	}
	noop := Tool{
		ToolSpec: ToolSpec{Name: "noop"},
		Execute:  func(context.Context, ToolCall) (ToolResult, error) { return ToolResult{}, nil },
	}
	_, err := Run(context.Background(), RunRequest{
		Provider: &fakeProvider{turns: turns},
		Tools:    []Tool{noop},
		MaxTurns: 2,
	})
	if err == nil || !strings.Contains(err.Error(), "maximum turns exceeded (2)") {
		t.Fatalf("err = %v, want max turns 2", err)
	}
}

func TestRunMaxTurnsDefaultIs32(t *testing.T) {
	t.Parallel()

	turns := make([][]ProviderEvent, 50)
	for i := range turns {
		turns[i] = []ProviderEvent{
			{Type: ProviderEventStart},
			{Type: ProviderEventToolCall, ToolCall: &ToolCall{ID: "x", Name: "noop", Arguments: json.RawMessage(`{}`)}},
			{Type: ProviderEventDone},
		}
	}
	noop := Tool{
		ToolSpec: ToolSpec{Name: "noop"},
		Execute:  func(context.Context, ToolCall) (ToolResult, error) { return ToolResult{}, nil },
	}
	_, err := Run(context.Background(), RunRequest{
		Provider: &fakeProvider{turns: turns},
		Tools:    []Tool{noop},
	})
	if err == nil || !strings.Contains(err.Error(), "maximum turns exceeded (32)") {
		t.Fatalf("err = %v, want default max turns 32", err)
	}
}

func TestRunMaxTurnsTagsLastAssistantStopReason(t *testing.T) {
	t.Parallel()

	turns := make([][]ProviderEvent, 5)
	for i := range turns {
		turns[i] = []ProviderEvent{
			{Type: ProviderEventStart},
			{Type: ProviderEventToolCall, ToolCall: &ToolCall{ID: "x", Name: "noop", Arguments: json.RawMessage(`{}`)}},
			{Type: ProviderEventDone},
		}
	}
	noop := Tool{
		ToolSpec: ToolSpec{Name: "noop"},
		Execute:  func(context.Context, ToolCall) (ToolResult, error) { return ToolResult{}, nil },
	}
	result, err := Run(context.Background(), RunRequest{
		Provider: &fakeProvider{turns: turns},
		Tools:    []Tool{noop},
		MaxTurns: 2,
	})
	if err == nil {
		t.Fatal("expected max-turns error")
	}

	// The last assistant message in the partial transcript must carry
	// StopReasonMaxTurns so retry-with-higher-budget logic can detect
	// budget exhaustion specifically.
	var lastAssistant *Message
	for i := len(result.Messages) - 1; i >= 0; i-- {
		if result.Messages[i].Role == MessageRoleAssistant {
			m := result.Messages[i]
			lastAssistant = &m
			break
		}
	}
	if lastAssistant == nil {
		t.Fatal("expected at least one assistant message in the partial transcript")
	}
	if lastAssistant.StopReason != StopReasonMaxTurns {
		t.Fatalf("last assistant stop_reason = %q, want %q", lastAssistant.StopReason, StopReasonMaxTurns)
	}

	// Same check on NewMessages.
	var lastNew *Message
	for i := len(result.NewMessages) - 1; i >= 0; i-- {
		if result.NewMessages[i].Role == MessageRoleAssistant {
			m := result.NewMessages[i]
			lastNew = &m
			break
		}
	}
	if lastNew == nil || lastNew.StopReason != StopReasonMaxTurns {
		t.Fatalf("NewMessages last assistant stop_reason = %v, want %q", lastNew, StopReasonMaxTurns)
	}
}

func TestRunNaturalStopUsesStopReasonStop(t *testing.T) {
	t.Parallel()

	// One turn, no tool calls — this is the natural-finish path; the
	// last assistant message must remain StopReasonStop so callers can
	// distinguish it from budget exhaustion.
	turns := [][]ProviderEvent{{
		{Type: ProviderEventStart},
		{Type: ProviderEventTextDelta, Delta: "hi"},
		{Type: ProviderEventDone},
	}}
	result, err := Run(context.Background(), RunRequest{
		Provider: &fakeProvider{turns: turns},
		MaxTurns: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	last := result.NewMessages[len(result.NewMessages)-1]
	if last.StopReason != StopReasonStop {
		t.Fatalf("natural stop reason = %q, want %q", last.StopReason, StopReasonStop)
	}
}

func TestRunEmitsEventsInOrder(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{turns: [][]ProviderEvent{{
		{Type: ProviderEventStart},
		{Type: ProviderEventTextDelta, Delta: "ok"},
		{Type: ProviderEventDone},
	}}}

	var got []EventType
	_, err := Run(context.Background(), RunRequest{
		Provider: provider,
		Emit:     func(e Event) { got = append(got, e.Type) },
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []EventType{
		EventLoopStart,
		EventTurnStart,
		EventMessageStart,
		EventTextDelta,
		EventMessageEnd,
		EventTurnEnd,
		EventLoopEnd,
	}
	if len(got) != len(want) {
		t.Fatalf("event types = %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("types[%d] = %q, want %q (full: %v)", i, got[i], w, got)
		}
	}
}

func assistantText(m Message) string {
	var b strings.Builder
	for _, p := range m.Content {
		if p.Type == ContentTypeText {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}
