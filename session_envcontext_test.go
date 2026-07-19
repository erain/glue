package glue

import (
	"context"
	"strings"
	"testing"
)

// envBlockOf extracts the env-context messages from a provider request.
func envBlocksIn(messages []Message) []string {
	var out []string
	for _, m := range messages {
		if IsEnvContextMessage(m) {
			out = append(out, lastEnvContextText([]Message{m}))
		}
	}
	return out
}

func TestPromptInsertsEnvContextOncePerChange(t *testing.T) {
	provider := &recordingProvider{turns: [][]ProviderEvent{
		textTurn("one"), textTurn("two"), textTurn("three"),
	}}
	env := "<environment_context>\ncwd: /repo\n</environment_context>"
	agent := NewAgent(AgentOptions{
		Provider:   provider,
		EnvContext: func() string { return env },
	})
	session, err := agent.Session(context.Background(), "env")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}

	if _, err := session.Prompt(context.Background(), "first"); err != nil {
		t.Fatalf("Prompt 1: %v", err)
	}
	if got := envBlocksIn(provider.requests[0].Messages); len(got) != 1 || got[0] != env {
		t.Fatalf("first request env blocks = %#v, want exactly the block", got)
	}
	// The block must precede the user prompt.
	msgs := provider.requests[0].Messages
	if !IsEnvContextMessage(msgs[len(msgs)-2]) {
		t.Fatalf("env block is not immediately before the user prompt: %#v", msgs)
	}

	// Unchanged environment: no second copy.
	if _, err := session.Prompt(context.Background(), "second"); err != nil {
		t.Fatalf("Prompt 2: %v", err)
	}
	if got := envBlocksIn(provider.requests[1].Messages); len(got) != 1 {
		t.Fatalf("unchanged env should not re-inject; got %d blocks", len(got))
	}

	// Changed environment: a fresh block is appended, old one retained.
	env = "<environment_context>\ncwd: /repo\ngit: branch feature\n</environment_context>"
	if _, err := session.Prompt(context.Background(), "third"); err != nil {
		t.Fatalf("Prompt 3: %v", err)
	}
	got := envBlocksIn(provider.requests[2].Messages)
	if len(got) != 2 || got[1] != env {
		t.Fatalf("changed env should append a fresh block; got %#v", got)
	}
}

func TestPromptWithoutEnvContextInjectsNothing(t *testing.T) {
	provider := &recordingProvider{turns: [][]ProviderEvent{textTurn("ok")}}
	agent := NewAgent(AgentOptions{Provider: provider})
	session, err := agent.Session(context.Background(), "plain")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if _, err := session.Prompt(context.Background(), "hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := envBlocksIn(provider.requests[0].Messages); len(got) != 0 {
		t.Fatalf("no EnvContext configured but request has env blocks: %#v", got)
	}
}

func TestEnvContextPersistsTagged(t *testing.T) {
	provider := &recordingProvider{turns: [][]ProviderEvent{textTurn("ok")}}
	agent := NewAgent(AgentOptions{
		Provider:   provider,
		EnvContext: func() string { return "<environment_context>x</environment_context>" },
	})
	session, err := agent.Session(context.Background(), "persist")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if _, err := session.Prompt(context.Background(), "hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	var tagged int
	for _, m := range session.Messages() {
		if IsEnvContextMessage(m) {
			tagged++
			if !strings.Contains(lastEnvContextText([]Message{m}), "environment_context") {
				t.Fatalf("tagged message lost its text: %#v", m)
			}
		}
	}
	if tagged != 1 {
		t.Fatalf("transcript should retain exactly one tagged env message, got %d", tagged)
	}
}

func TestIsEnvContextMessage(t *testing.T) {
	if IsEnvContextMessage(Message{Role: MessageRoleUser}) {
		t.Fatal("untagged message misdetected")
	}
	if IsEnvContextMessage(Message{Metadata: map[string]any{MetadataKeyEnvContext: "yes"}}) {
		t.Fatal("non-bool tag misdetected")
	}
	if !IsEnvContextMessage(Message{Metadata: map[string]any{MetadataKeyEnvContext: true}}) {
		t.Fatal("tagged message not detected")
	}
}
