package glue

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func textMessage(role MessageRole, body string) Message {
	return Message{Role: role, Content: []ContentPart{{Type: ContentTypeText, Text: body}}}
}

func TestKeepRecentMessagesUnchangedBelowThreshold(t *testing.T) {
	t.Parallel()

	c := KeepRecentMessages(4)
	in := []Message{
		textMessage(MessageRoleUser, "u1"),
		textMessage(MessageRoleAssistant, "a1"),
	}
	out, err := c.Compact(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2 (no compaction expected)", len(out))
	}
}

func TestKeepRecentMessagesDropsOlderAndInsertsSummary(t *testing.T) {
	t.Parallel()

	c := KeepRecentMessages(2)
	in := []Message{
		textMessage(MessageRoleUser, "u1"),
		textMessage(MessageRoleAssistant, "a1"),
		textMessage(MessageRoleUser, "u2"),
		textMessage(MessageRoleAssistant, "a2"),
		textMessage(MessageRoleUser, "u3"),
	}
	out, err := c.Compact(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("len(out) = %d, want 3 (summary + last 2)", len(out))
	}
	summary := out[0]
	if summary.Role != MessageRoleAssistant {
		t.Fatalf("summary role = %q, want assistant", summary.Role)
	}
	if !strings.Contains(summary.Content[0].Text, "3 messages") {
		t.Fatalf("summary text = %q, want '3 messages dropped'", summary.Content[0].Text)
	}
	if summary.Metadata["compaction"] != "keep_recent" {
		t.Fatalf("summary metadata = %v, want compaction=keep_recent", summary.Metadata)
	}
	if got := summary.Metadata["dropped"]; got != 3 {
		t.Fatalf("summary dropped = %v (%T), want 3", got, got)
	}
	if out[1].Content[0].Text != "a2" || out[2].Content[0].Text != "u3" {
		t.Fatalf("kept tail = %q/%q, want a2/u3", out[1].Content[0].Text, out[2].Content[0].Text)
	}
}

func TestKeepRecentMessagesPositiveN(t *testing.T) {
	t.Parallel()

	if _, err := KeepRecentMessages(0).Compact(context.Background(), []Message{textMessage(MessageRoleUser, "x")}); err == nil {
		t.Fatal("err = nil, want positive n required")
	}
}

func TestSessionCompactsBeforePromptWhenThresholdExceeded(t *testing.T) {
	t.Parallel()

	provider := &recordingProvider{turns: [][]ProviderEvent{textTurn("ok"), textTurn("ok2")}}
	agent := NewAgent(AgentOptions{
		Provider:            provider,
		Compactor:           KeepRecentMessages(2),
		CompactionThreshold: 4,
	})
	session, _ := agent.Session(context.Background(), "x")

	// First prompt: builds a baseline transcript (1 user + 1 assistant).
	if _, err := session.Prompt(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}

	// Inject older messages directly to push the in-memory transcript
	// past the threshold. (Simulating a long-running session.)
	session.mu.Lock()
	for i := 0; i < 4; i++ {
		session.messages = append(session.messages, textMessage(MessageRoleUser, "older"))
	}
	preCount := len(session.messages)
	session.mu.Unlock()

	if preCount <= 4 {
		t.Fatalf("setup invariant: in-memory transcript = %d, want > threshold 4", preCount)
	}

	// Second prompt should trigger compaction.
	if _, err := session.Prompt(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}

	// Provider's 2nd request should see compacted history (summary +
	// last 2) plus the new user message — exactly 4 messages.
	gotMessages := provider.requests[1].Messages
	if len(gotMessages) != 4 {
		t.Fatalf("2nd request len = %d, want 4 (summary + last 2 + new user)", len(gotMessages))
	}
	if gotMessages[0].Role != MessageRoleAssistant ||
		!strings.Contains(gotMessages[0].Content[0].Text, "compaction") {
		t.Fatalf("2nd request[0] = %#v, want compaction summary", gotMessages[0])
	}
	if gotMessages[len(gotMessages)-1].Role != MessageRoleUser ||
		gotMessages[len(gotMessages)-1].Content[0].Text != "second" {
		t.Fatalf("2nd request tail = %#v, want new user message", gotMessages[len(gotMessages)-1])
	}

	// Session.Messages should reflect compacted form too (summary + last 2 + new user + new assistant).
	final := session.Messages()
	if len(final) != 5 {
		t.Fatalf("session.Messages() len = %d, want 5", len(final))
	}
}

func TestSessionDoesNotCompactWhenDisabled(t *testing.T) {
	t.Parallel()

	provider := &recordingProvider{turns: [][]ProviderEvent{textTurn("ok"), textTurn("ok2")}}
	agent := NewAgent(AgentOptions{Provider: provider}) // no compactor
	session, _ := agent.Session(context.Background(), "x")

	if _, err := session.Prompt(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	for i := 0; i < 5; i++ {
		session.messages = append(session.messages, textMessage(MessageRoleUser, "older"))
	}
	session.mu.Unlock()

	if _, err := session.Prompt(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	got := provider.requests[1].Messages
	// 1 (first user) + 1 (first assistant) + 5 injected + 1 (second user) = 8 messages
	if len(got) != 8 {
		t.Fatalf("2nd request len = %d, want 8 (no compaction)", len(got))
	}
}

type errorCompactor struct{ err error }

func (e errorCompactor) Compact(_ context.Context, _ []Message) ([]Message, error) {
	return nil, e.err
}

func TestSessionPromptSurfacesCompactorError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("compactor down")
	provider := &recordingProvider{turns: [][]ProviderEvent{textTurn("ok")}}
	agent := NewAgent(AgentOptions{
		Provider:            provider,
		Compactor:           errorCompactor{err: wantErr},
		CompactionThreshold: 1,
	})
	session, _ := agent.Session(context.Background(), "x")

	// Inject enough messages to exceed threshold.
	session.mu.Lock()
	session.messages = []Message{textMessage(MessageRoleUser, "a"), textMessage(MessageRoleAssistant, "b")}
	session.mu.Unlock()

	_, err := session.Prompt(context.Background(), "next")
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want compactor down", err)
	}
	if provider.calls != 0 {
		t.Fatalf("provider was called despite compactor error: calls=%d", provider.calls)
	}
}

func TestSessionCompactionPersistsViaStore(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	provider := &recordingProvider{turns: [][]ProviderEvent{textTurn("first"), textTurn("second")}}
	agent := NewAgent(AgentOptions{
		Provider:            provider,
		Store:               store,
		Compactor:           KeepRecentMessages(2),
		CompactionThreshold: 4,
	})
	session, _ := agent.Session(context.Background(), "x")

	if _, err := session.Prompt(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	for i := 0; i < 5; i++ {
		session.messages = append(session.messages, textMessage(MessageRoleUser, "older"))
	}
	session.mu.Unlock()

	if _, err := session.Prompt(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	state := store.states["x"]
	if len(state.Messages) != 5 {
		t.Fatalf("persisted Messages = %d, want 5 (compacted)", len(state.Messages))
	}
	if state.Messages[0].Metadata["compaction"] != "keep_recent" {
		t.Fatalf("persisted summary metadata = %v, want compaction=keep_recent", state.Messages[0].Metadata)
	}
}
