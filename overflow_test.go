package glue

import (
	"context"
	"errors"
	"testing"
)

// overflowOnceProvider reports a context-window overflow on exactly
// one call (failAt), succeeding otherwise — the shape of a session
// that outgrew the model between two prompts.
type overflowOnceProvider struct {
	failAt   int
	calls    int
	requests []ProviderRequest
}

func (p *overflowOnceProvider) Stream(_ context.Context, req ProviderRequest) (<-chan ProviderEvent, error) {
	p.calls++
	p.requests = append(p.requests, req)
	if p.calls == p.failAt {
		return nil, errors.New("prompt is too long: input token count exceeds the maximum")
	}
	ch := make(chan ProviderEvent, 2)
	ch <- ProviderEvent{Type: ProviderEventTextDelta, Delta: "ok"}
	ch <- ProviderEvent{Type: ProviderEventDone}
	close(ch)
	return ch, nil
}

func TestSessionOverflowCompactsOnceAndRetries(t *testing.T) {
	provider := &overflowOnceProvider{failAt: 4}
	agent := NewAgent(AgentOptions{
		Provider:  provider,
		Compactor: KeepRecentMessages(1),
		// Threshold high enough that pre-prompt compaction never
		// triggers; only the overflow path may compact.
		CompactionThreshold: 100,
	})
	session, err := agent.Session(context.Background(), "overflow-test")
	if err != nil {
		t.Fatal(err)
	}

	// Grow the transcript: three successful prompts = 6 messages.
	for _, p := range []string{"one", "two", "three"} {
		if _, err := session.Prompt(context.Background(), p); err != nil {
			t.Fatalf("prompt %q: %v", p, err)
		}
	}

	// Fourth prompt: provider overflows once; the session must compact
	// and retry instead of surfacing the error.
	res, err := session.Prompt(context.Background(), "fourth")
	if err != nil {
		t.Fatalf("prompt after overflow: %v", err)
	}
	if res.Text != "ok" {
		t.Fatalf("text = %q", res.Text)
	}
	if provider.calls != 5 {
		t.Fatalf("provider calls = %d, want 5 (3 ok, overflow, retried ok)", provider.calls)
	}
	// The retried request must be smaller than the overflowing one.
	if len(provider.requests[4].Messages) >= len(provider.requests[3].Messages) {
		t.Fatalf("retry not compacted: %d -> %d messages",
			len(provider.requests[3].Messages), len(provider.requests[4].Messages))
	}
}

func TestSessionOverflowWithoutCompactorSurfaces(t *testing.T) {
	provider := &overflowOnceProvider{failAt: 2}
	agent := NewAgent(AgentOptions{Provider: provider})
	session, err := agent.Session(context.Background(), "overflow-no-compactor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Prompt(context.Background(), "first"); err != nil {
		t.Fatalf("first prompt: %v", err)
	}
	if _, err := session.Prompt(context.Background(), "second"); err == nil {
		t.Fatal("want overflow error without a compactor")
	}
	if provider.calls != 2 {
		t.Fatalf("provider calls = %d, want 2 (no retry without compactor)", provider.calls)
	}
}
