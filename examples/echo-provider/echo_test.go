package echo

import (
	"context"
	"testing"

	"github.com/erain/glue"
)

func TestEchoProviderRoundTripThroughAgent(t *testing.T) {
	t.Parallel()

	agent := glue.NewAgent(glue.AgentOptions{Provider: New(), Model: "echo-v1"})
	session, err := agent.Session(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	res, err := session.Prompt(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Text != "hello world" {
		t.Fatalf("Text = %q, want hello world", res.Text)
	}
	if res.Message == nil || res.Message.StopReason != glue.StopReasonStop {
		t.Fatalf("StopReason = %v, want stop", res.Message)
	}
}

func TestEchoProviderImplementsProviderInterface(t *testing.T) {
	t.Parallel()

	var _ glue.Provider = New()
}

func TestEchoProviderEmptyTranscriptDoesNotPanic(t *testing.T) {
	t.Parallel()

	p := New()
	events, err := p.Stream(context.Background(), glue.ProviderRequest{Model: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var sawDone bool
	for ev := range events {
		if ev.Type == glue.ProviderEventDone {
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatal("never saw ProviderEventDone")
	}
}

func TestEchoProviderPrefixIsApplied(t *testing.T) {
	t.Parallel()

	agent := glue.NewAgent(glue.AgentOptions{Provider: &Provider{Prefix: "echo: "}, Model: "echo-v1"})
	session, _ := agent.Session(context.Background(), "x")
	res, err := session.Prompt(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "echo: hi" {
		t.Fatalf("Text = %q, want echo: hi", res.Text)
	}
}
