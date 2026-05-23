package glue

import (
	"context"
	"errors"
	"testing"
)

// recordingProvider scripts a sequence of provider responses, one per turn,
// and captures the ProviderRequest seen on each call.
type recordingProvider struct {
	turns    [][]ProviderEvent
	calls    int
	requests []ProviderRequest
}

func (p *recordingProvider) Stream(_ context.Context, req ProviderRequest) (<-chan ProviderEvent, error) {
	if p.calls >= len(p.turns) {
		return nil, errors.New("recordingProvider: unexpected call")
	}
	p.requests = append(p.requests, req)
	events := p.turns[p.calls]
	p.calls++
	ch := make(chan ProviderEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func textTurn(text string) []ProviderEvent {
	return []ProviderEvent{
		{Type: ProviderEventStart},
		{Type: ProviderEventTextDelta, Delta: text},
		{Type: ProviderEventDone},
	}
}

type recordingGluePermission struct {
	requests []PermissionRequest
	decision PermissionDecision
}

func (p *recordingGluePermission) Decide(_ context.Context, req PermissionRequest) (PermissionDecision, error) {
	p.requests = append(p.requests, req)
	return p.decision, nil
}

type recordingGlueHook struct {
	pre  int
	post int
}

func (h *recordingGlueHook) PreTool(context.Context, ToolCall) error {
	h.pre++
	return nil
}

func (h *recordingGlueHook) PostTool(_ context.Context, _ ToolCall, _ *ToolResult) error {
	h.post++
	return nil
}

func TestSessionPromptHappyPath(t *testing.T) {
	t.Parallel()

	provider := &recordingProvider{turns: [][]ProviderEvent{textTurn("hello")}}
	agent := NewAgent(AgentOptions{
		Provider:     provider,
		Model:        "fake-1",
		SystemPrompt: "be terse",
	})
	session, err := agent.Session(context.Background(), "dev")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}

	res, err := session.Prompt(context.Background(), "say hello")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Text != "hello" {
		t.Fatalf("Text = %q, want hello", res.Text)
	}
	if res.Message == nil || res.Message.Role != MessageRoleAssistant {
		t.Fatalf("Message = %#v, want assistant message", res.Message)
	}
	if len(res.NewMessages) != 1 {
		t.Fatalf("NewMessages = %d, want 1", len(res.NewMessages))
	}
	if len(res.Messages) != 2 {
		t.Fatalf("Messages = %d, want 2 (user + assistant)", len(res.Messages))
	}
	if got := provider.requests[0].SystemPrompt; got != "be terse" {
		t.Fatalf("SystemPrompt = %q, want 'be terse'", got)
	}
	if got := provider.requests[0].Model; got != "fake-1" {
		t.Fatalf("Model = %q, want fake-1", got)
	}
}

func TestSessionPromptPassesPermissionHooksAndSessionID(t *testing.T) {
	t.Parallel()

	provider := &recordingProvider{turns: [][]ProviderEvent{
		{
			{Type: ProviderEventStart},
			{Type: ProviderEventToolCall, ToolCall: &ToolCall{ID: "c1", Name: "side", Arguments: []byte(`{"x":1}`)}},
			{Type: ProviderEventDone},
		},
		textTurn("done"),
	}}
	perm := &recordingGluePermission{decision: PermissionDecision{Allow: true}}
	hook := &recordingGlueHook{}
	agent := NewAgent(AgentOptions{
		Provider: provider,
		Tools: []Tool{{
			ToolSpec: ToolSpec{
				Name:               "side",
				RequiresPermission: true,
				PermissionAction:   "touch",
			},
			Execute: func(context.Context, ToolCall) (ToolResult, error) {
				return TextResult("ok"), nil
			},
		}},
		Permission: perm,
		Hooks:      []Hook{hook},
	})
	session, err := agent.Session(context.Background(), "dev-session")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}

	if _, err := session.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(perm.requests) != 1 {
		t.Fatalf("permission requests = %d, want 1", len(perm.requests))
	}
	if got := perm.requests[0].SessionID; got != "dev-session" {
		t.Fatalf("SessionID = %q, want dev-session", got)
	}
	if got := perm.requests[0].Action; got != "touch" {
		t.Fatalf("Action = %q, want touch", got)
	}
	if hook.pre != 1 || hook.post != 1 {
		t.Fatalf("hook calls pre=%d post=%d, want 1/1", hook.pre, hook.post)
	}
}

func TestSessionPromptContinuation(t *testing.T) {
	t.Parallel()

	provider := &recordingProvider{turns: [][]ProviderEvent{textTurn("hi"), textTurn("again")}}
	agent := NewAgent(AgentOptions{Provider: provider, Model: "m"})
	session, err := agent.Session(context.Background(), "")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if session.ID() != "default" {
		t.Fatalf("ID = %q, want default", session.ID())
	}

	if _, err := session.Prompt(context.Background(), "first"); err != nil {
		t.Fatalf("first Prompt: %v", err)
	}
	if _, err := session.Prompt(context.Background(), "second"); err != nil {
		t.Fatalf("second Prompt: %v", err)
	}

	if provider.calls != 2 {
		t.Fatalf("calls = %d, want 2", provider.calls)
	}
	// The 2nd request should carry: user-1, assistant-1, user-2 (3 messages).
	got := provider.requests[1].Messages
	wantRoles := []MessageRole{MessageRoleUser, MessageRoleAssistant, MessageRoleUser}
	if len(got) != len(wantRoles) {
		t.Fatalf("2nd request len = %d, want %d", len(got), len(wantRoles))
	}
	for i, r := range wantRoles {
		if got[i].Role != r {
			t.Fatalf("2nd request [%d].Role = %q, want %q", i, got[i].Role, r)
		}
	}
	if len(session.Messages()) != 4 {
		t.Fatalf("transcript = %d, want 4 (u/a/u/a)", len(session.Messages()))
	}
}

func TestSessionSubscribeAndUnsubscribe(t *testing.T) {
	t.Parallel()

	provider := &recordingProvider{turns: [][]ProviderEvent{textTurn("a"), textTurn("b")}}
	agent := NewAgent(AgentOptions{Provider: provider})
	session, _ := agent.Session(context.Background(), "x")

	var seen []EventType
	unsubscribe := session.Subscribe(func(e Event) { seen = append(seen, e.Type) })
	if _, err := session.Prompt(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	firstCount := len(seen)
	if firstCount == 0 || !containsEventType(seen, EventLoopStart) || !containsEventType(seen, EventLoopEnd) {
		t.Fatalf("first run events = %v, want some incl. loop_start/loop_end", seen)
	}

	unsubscribe()
	if _, err := session.Prompt(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != firstCount {
		t.Fatalf("after unsubscribe: events grew from %d to %d", firstCount, len(seen))
	}
}

func TestSessionPromptWithEvents(t *testing.T) {
	t.Parallel()

	provider := &recordingProvider{turns: [][]ProviderEvent{textTurn("ok")}}
	agent := NewAgent(AgentOptions{Provider: provider})
	session, _ := agent.Session(context.Background(), "x")

	var perPrompt []EventType
	if _, err := session.Prompt(context.Background(), "go", WithEvents(func(e Event) {
		perPrompt = append(perPrompt, e.Type)
	})); err != nil {
		t.Fatal(err)
	}
	if !containsEventType(perPrompt, EventLoopStart) || !containsEventType(perPrompt, EventLoopEnd) {
		t.Fatalf("per-prompt events = %v, missing loop bookends", perPrompt)
	}
}

func TestSessionSubscribeAndWithEventsBothFire(t *testing.T) {
	t.Parallel()

	provider := &recordingProvider{turns: [][]ProviderEvent{textTurn("ok")}}
	agent := NewAgent(AgentOptions{Provider: provider})
	session, _ := agent.Session(context.Background(), "x")

	var subCount, perCount int
	session.Subscribe(func(Event) { subCount++ })
	if _, err := session.Prompt(context.Background(), "go", WithEvents(func(Event) { perCount++ })); err != nil {
		t.Fatal(err)
	}
	if subCount == 0 || perCount == 0 {
		t.Fatalf("subCount=%d perCount=%d, want both > 0", subCount, perCount)
	}
	if subCount != perCount {
		t.Fatalf("subCount=%d perCount=%d, want equal", subCount, perCount)
	}
}

func TestSessionPromptWithModelOverride(t *testing.T) {
	t.Parallel()

	provider := &recordingProvider{turns: [][]ProviderEvent{textTurn("ok")}}
	agent := NewAgent(AgentOptions{Provider: provider, Model: "default-model"})
	session, _ := agent.Session(context.Background(), "x")

	if _, err := session.Prompt(context.Background(), "go", WithModel("override-model")); err != nil {
		t.Fatal(err)
	}
	if got := provider.requests[0].Model; got != "override-model" {
		t.Fatalf("provider saw Model = %q, want override-model", got)
	}
}

func TestPromptRequiresProvider(t *testing.T) {
	t.Parallel()

	agent := NewAgent(AgentOptions{}) // no provider
	session, err := agent.Session(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	_, err = session.Prompt(context.Background(), "hello")
	if err == nil {
		t.Fatal("err = nil, want provider required")
	}
}

func TestAgentSessionReusesByID(t *testing.T) {
	t.Parallel()

	agent := NewAgent(AgentOptions{Provider: &recordingProvider{}})
	first, _ := agent.Session(context.Background(), "abc")
	second, _ := agent.Session(context.Background(), "abc")
	if first != second {
		t.Fatal("Session(id) returned different instances on second call")
	}
}

func TestPublicTypeAliasesMatchLoop(t *testing.T) {
	t.Parallel()

	// Compile-time check that public re-exports satisfy expected shapes.
	var _ Provider = &recordingProvider{}
	var _ Event
	var _ Message
	var _ Tool
	var _ ProviderEvent
	var _ ToolCall
	var _ ToolResult
}

func containsEventType(events []EventType, want EventType) bool {
	for _, e := range events {
		if e == want {
			return true
		}
	}
	return false
}
