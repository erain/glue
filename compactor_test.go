package glue

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
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

// ---- SummarizingCompactor ----

// summarizingProvider is a fake Provider tailored to the
// SummarizingCompactor's call pattern. It records every received
// ProviderRequest and emits a configurable summary text on Done.
type summarizingProvider struct {
	summary  string
	requests []ProviderRequest
	calls    int
	failWith error
	// Block holds the goroutine until released; used to test ctx cancel.
	Block chan struct{}
}

func (p *summarizingProvider) Stream(ctx context.Context, req ProviderRequest) (<-chan ProviderEvent, error) {
	p.requests = append(p.requests, req)
	p.calls++
	if p.failWith != nil {
		return nil, p.failWith
	}
	ch := make(chan ProviderEvent, 4)
	go func() {
		defer close(ch)
		ch <- ProviderEvent{Type: ProviderEventStart}
		if p.Block != nil {
			select {
			case <-ctx.Done():
				return
			case <-p.Block:
			}
		}
		ch <- ProviderEvent{Type: ProviderEventDone, Message: &Message{
			Role:    MessageRoleAssistant,
			Content: []ContentPart{{Type: ContentTypeText, Text: p.summary}},
		}}
	}()
	return ch, nil
}

func TestSummarizingCompactor_BelowBudgetPassThrough(t *testing.T) {
	t.Parallel()
	p := &summarizingProvider{summary: "should not be called"}
	c := &SummarizingCompactor{Provider: p, TargetTokens: 1_000_000, KeepRecent: 2}
	in := []Message{
		textMessage(MessageRoleUser, "u1"),
		textMessage(MessageRoleAssistant, "a1"),
		textMessage(MessageRoleUser, "u2"),
		textMessage(MessageRoleAssistant, "a2"),
		textMessage(MessageRoleUser, "u3"),
	}
	out, err := c.Compact(context.Background(), in)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("len(out) = %d, want %d (unchanged below budget)", len(out), len(in))
	}
	if p.calls != 0 {
		t.Fatalf("provider called %d times; expected 0 when below budget", p.calls)
	}
}

func TestSummarizingCompactor_BelowKeepRecentPassThrough(t *testing.T) {
	t.Parallel()
	p := &summarizingProvider{summary: "x"}
	c := &SummarizingCompactor{Provider: p, TargetTokens: 1, KeepRecent: 3}
	in := []Message{textMessage(MessageRoleUser, "u1"), textMessage(MessageRoleAssistant, "a1")}
	out, err := c.Compact(context.Background(), in)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("len(out) = %d, want %d (input ≤ KeepRecent)", len(out), len(in))
	}
	if p.calls != 0 {
		t.Fatalf("provider called %d times; expected 0 below KeepRecent", p.calls)
	}
}

func TestSummarizingCompactor_BoundaryEqualsKeepRecentPassThrough(t *testing.T) {
	t.Parallel()
	// Exactly KeepRecent messages — should pass through even if over budget.
	p := &summarizingProvider{summary: "x"}
	c := &SummarizingCompactor{Provider: p, TargetTokens: 1, KeepRecent: 3}
	in := []Message{
		textMessage(MessageRoleUser, "u1"),
		textMessage(MessageRoleAssistant, "a1"),
		textMessage(MessageRoleUser, "u2"),
	}
	out, err := c.Compact(context.Background(), in)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("len(out) = %d, want %d (boundary == KeepRecent)", len(out), len(in))
	}
	if p.calls != 0 {
		t.Fatalf("provider called %d times; expected 0 at boundary", p.calls)
	}
}

func TestSummarizingCompactor_OverBudgetPartitionAndMarker(t *testing.T) {
	t.Parallel()
	summary := "Earlier the user introduced themselves as Yu and asked about Australian Shepherds. The assistant explained shedding."
	p := &summarizingProvider{summary: summary}
	c := &SummarizingCompactor{
		Provider:     p,
		TargetTokens: 1, // trip over budget on any non-trivial input
		KeepRecent:   2,
	}
	in := []Message{
		textMessage(MessageRoleUser, "hi I am Yu"),
		textMessage(MessageRoleAssistant, "hello Yu"),
		textMessage(MessageRoleUser, "tell me about Aussies"),
		textMessage(MessageRoleAssistant, "they shed a lot"),
		textMessage(MessageRoleUser, "what about exercise"),
		textMessage(MessageRoleAssistant, "lots needed"),
	}
	out, err := c.Compact(context.Background(), in)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("len(out) = %d, want 3 (marker + last 2)", len(out))
	}
	marker := out[0]
	if marker.Role != MessageRoleAssistant {
		t.Fatalf("marker role = %s", marker.Role)
	}
	if len(marker.Content) == 0 || marker.Content[0].Text != summary {
		t.Fatalf("marker text = %q, want summary", marker.Content[0].Text)
	}
	if marker.Metadata["compaction"] != "summarizing" {
		t.Fatalf("marker metadata.compaction = %v", marker.Metadata["compaction"])
	}
	if marker.Metadata["original_message_count"] != 4 {
		t.Fatalf("original_message_count = %v, want 4", marker.Metadata["original_message_count"])
	}
	if _, hasFirst := marker.Metadata["original_first_ts"]; hasFirst {
		t.Errorf("first_ts should be absent when messages have no CreatedAt; got %v", marker.Metadata["original_first_ts"])
	}
	// Kept tail = last 2 originals.
	if out[1].Content[0].Text != "what about exercise" {
		t.Errorf("kept[0] = %q", out[1].Content[0].Text)
	}
	if out[2].Content[0].Text != "lots needed" {
		t.Errorf("kept[1] = %q", out[2].Content[0].Text)
	}
	// Provider was called once with the older 4 messages rendered into
	// the user prompt.
	if p.calls != 1 {
		t.Fatalf("provider calls = %d", p.calls)
	}
	req := p.requests[0]
	if req.SystemPrompt == "" {
		t.Errorf("expected SummarizingCompactor system prompt")
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != MessageRoleUser {
		t.Fatalf("provider request shape: %+v", req.Messages)
	}
	body := req.Messages[0].Content[0].Text
	for _, want := range []string{"USER:", "ASSISTANT:", "hi I am Yu", "they shed"} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered transcript missing %q\n--\n%s", want, body)
		}
	}
}

func TestSummarizingCompactor_TimestampMetadata(t *testing.T) {
	t.Parallel()
	t1 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	in := []Message{
		{Role: MessageRoleUser, Content: []ContentPart{{Type: ContentTypeText, Text: "early"}}, CreatedAt: t2},
		{Role: MessageRoleAssistant, Content: []ContentPart{{Type: ContentTypeText, Text: "mid"}}, CreatedAt: t1},
		{Role: MessageRoleUser, Content: []ContentPart{{Type: ContentTypeText, Text: "u2"}}},
		{Role: MessageRoleAssistant, Content: []ContentPart{{Type: ContentTypeText, Text: "a2"}}},
	}
	p := &summarizingProvider{summary: "s"}
	c := &SummarizingCompactor{Provider: p, TargetTokens: 1, KeepRecent: 2}
	out, err := c.Compact(context.Background(), in)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	first := out[0].Metadata["original_first_ts"]
	last := out[0].Metadata["original_last_ts"]
	if first != t1.Format(time.RFC3339) {
		t.Errorf("first_ts = %v, want %s (earliest)", first, t1.Format(time.RFC3339))
	}
	if last != t2.Format(time.RFC3339) {
		t.Errorf("last_ts = %v, want %s (latest)", last, t2.Format(time.RFC3339))
	}
}

func TestSummarizingCompactor_ProviderStreamErrorPropagates(t *testing.T) {
	t.Parallel()
	p := &summarizingProvider{summary: "x", failWith: errors.New("upstream down")}
	c := &SummarizingCompactor{Provider: p, TargetTokens: 1, KeepRecent: 1}
	_, err := c.Compact(context.Background(), []Message{
		textMessage(MessageRoleUser, "older"),
		textMessage(MessageRoleAssistant, "older2"),
		textMessage(MessageRoleUser, "kept"),
	})
	if err == nil || !strings.Contains(err.Error(), "upstream down") {
		t.Fatalf("err = %v, want upstream down propagated", err)
	}
}

func TestSummarizingCompactor_ProviderEventErrorPropagates(t *testing.T) {
	t.Parallel()
	provider := &erroringProvider{errMsg: "model refused"}
	c := &SummarizingCompactor{Provider: provider, TargetTokens: 1, KeepRecent: 1}
	_, err := c.Compact(context.Background(), []Message{
		textMessage(MessageRoleUser, "older"),
		textMessage(MessageRoleAssistant, "older2"),
		textMessage(MessageRoleUser, "kept"),
	})
	if err == nil || !strings.Contains(err.Error(), "model refused") {
		t.Fatalf("err = %v, want model refused propagated", err)
	}
}

type erroringProvider struct{ errMsg string }

func (p *erroringProvider) Stream(_ context.Context, _ ProviderRequest) (<-chan ProviderEvent, error) {
	ch := make(chan ProviderEvent, 2)
	ch <- ProviderEvent{Type: ProviderEventStart}
	ch <- ProviderEvent{Type: ProviderEventError, Error: p.errMsg}
	close(ch)
	return ch, nil
}

func TestSummarizingCompactor_NoSilentFallbackOnEmptySummary(t *testing.T) {
	t.Parallel()
	p := &summarizingProvider{summary: "   "} // whitespace only
	c := &SummarizingCompactor{Provider: p, TargetTokens: 1, KeepRecent: 1}
	_, err := c.Compact(context.Background(), []Message{
		textMessage(MessageRoleUser, "older"),
		textMessage(MessageRoleAssistant, "older2"),
		textMessage(MessageRoleUser, "kept"),
	})
	if err == nil || !strings.Contains(err.Error(), "no text") {
		t.Fatalf("err = %v, want no-text error", err)
	}
}

func TestSummarizingCompactor_NilProviderErrors(t *testing.T) {
	t.Parallel()
	c := &SummarizingCompactor{TargetTokens: 1, KeepRecent: 1}
	_, err := c.Compact(context.Background(), []Message{
		textMessage(MessageRoleUser, "older"),
		textMessage(MessageRoleAssistant, "older2"),
		textMessage(MessageRoleUser, "kept"),
	})
	if err == nil || !strings.Contains(err.Error(), "Provider is required") {
		t.Fatalf("err = %v, want Provider required", err)
	}
}

func TestSummarizingCompactor_DefaultKnobs(t *testing.T) {
	t.Parallel()
	// Zero TargetTokens and KeepRecent fall back to defaults; we exercise
	// that path by passing a tiny transcript that fits below either knob.
	p := &summarizingProvider{summary: "x"}
	c := &SummarizingCompactor{Provider: p}
	in := []Message{textMessage(MessageRoleUser, "hi"), textMessage(MessageRoleAssistant, "hello")}
	out, err := c.Compact(context.Background(), in)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("default knobs should pass through small transcript; got %d", len(out))
	}
	if p.calls != 0 {
		t.Fatalf("provider unexpectedly called: %d", p.calls)
	}
}

func TestSummarizingCompactor_RendersToolCallsInTranscript(t *testing.T) {
	t.Parallel()
	args := []byte(`{"q":"NYC"}`)
	in := []Message{
		textMessage(MessageRoleUser, "weather please"),
		{Role: MessageRoleAssistant, Content: []ContentPart{
			{Type: ContentTypeText, Text: "checking"},
			{Type: ContentTypeToolCall, ToolCall: &ToolCall{ID: "c1", Name: "weather", Arguments: args}},
		}},
		{Role: MessageRoleTool, ToolCallID: "c1", Content: []ContentPart{{Type: ContentTypeText, Text: "sunny"}}},
		textMessage(MessageRoleUser, "thanks"),
		textMessage(MessageRoleAssistant, "you're welcome"),
		textMessage(MessageRoleUser, "newest"),
	}
	p := &summarizingProvider{summary: "summary"}
	c := &SummarizingCompactor{Provider: p, TargetTokens: 1, KeepRecent: 2}
	if _, err := c.Compact(context.Background(), in); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	body := p.requests[0].Messages[0].Content[0].Text
	for _, want := range []string{"[tool_call name=weather", `args={"q":"NYC"}`, "TOOL:", "sunny"} {
		if !strings.Contains(body, want) {
			t.Errorf("transcript missing %q\n--\n%s", want, body)
		}
	}
}

func TestSummarizingCompactor_ContextCancelAbortsStream(t *testing.T) {
	t.Parallel()
	p := &summarizingProvider{summary: "ignored", Block: make(chan struct{})}
	defer close(p.Block) // release the goroutine on test exit
	c := &SummarizingCompactor{Provider: p, TargetTokens: 1, KeepRecent: 1}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Compact(ctx, []Message{
			textMessage(MessageRoleUser, "older"),
			textMessage(MessageRoleAssistant, "older2"),
			textMessage(MessageRoleUser, "kept"),
		})
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Compact did not return after ctx cancel")
	}
}

func TestEstimateTokens_HeuristicScales(t *testing.T) {
	t.Parallel()
	small := estimateTokens([]Message{textMessage(MessageRoleUser, "one two three four")})
	big := estimateTokens([]Message{textMessage(MessageRoleUser, strings.Repeat("word ", 1000))})
	if small <= 0 || big <= small {
		t.Fatalf("expected big > small > 0; got small=%d big=%d", small, big)
	}
	// 4 words ≈ 3 tokens.
	if small < 2 || small > 4 {
		t.Errorf("small estimate = %d, want around 3", small)
	}
}
