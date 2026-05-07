package glue

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

type streamFakeProvider struct {
	deltas   []string
	toolName string
	emitTool bool
	turn     int
}

func (p *streamFakeProvider) Stream(_ context.Context, _ ProviderRequest) (<-chan ProviderEvent, error) {
	ch := make(chan ProviderEvent, len(p.deltas)+4)
	ch <- ProviderEvent{Type: ProviderEventStart}
	if p.turn == 0 {
		for _, d := range p.deltas {
			ch <- ProviderEvent{Type: ProviderEventTextDelta, Delta: d}
		}
		if p.emitTool {
			ch <- ProviderEvent{Type: ProviderEventToolCall, ToolCall: &ToolCall{ID: "1", Name: p.toolName, Arguments: []byte(`{}`)}}
		}
	}
	ch <- ProviderEvent{Type: ProviderEventDone}
	close(ch)
	p.turn++
	return ch, nil
}

func TestWithStreamWriter_MirrorsDeltas(t *testing.T) {
	var buf bytes.Buffer
	agent := NewAgent(AgentOptions{Provider: &streamFakeProvider{deltas: []string{"hel", "lo"}}})
	session, _ := agent.Session(context.Background(), "test")
	if _, err := session.Prompt(context.Background(), "go", WithStreamWriter(&buf)); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "hello" {
		t.Fatalf("expected mirrored text 'hello', got %q", got)
	}
}

func TestWithStreamWriter_NilWriterIsNoOp(t *testing.T) {
	agent := NewAgent(AgentOptions{Provider: &streamFakeProvider{deltas: []string{"x"}}})
	session, _ := agent.Session(context.Background(), "test")
	// Should not panic.
	if _, err := session.Prompt(context.Background(), "go", WithStreamWriter(nil)); err != nil {
		t.Fatal(err)
	}
}

func TestWithToolLogger_MirrorsToolStart(t *testing.T) {
	var buf bytes.Buffer
	agent := NewAgent(AgentOptions{
		Provider: &streamFakeProvider{deltas: []string{"a"}, emitTool: true, toolName: "echo"},
		Tools: []Tool{
			NewTool[struct{}](ToolSpec{Name: "echo"}, func(_ context.Context, _ struct{}) (ToolResult, error) {
				return TextResult("ok"), nil
			}),
		},
		MaxTurns: 2,
	})
	session, _ := agent.Session(context.Background(), "test")
	if _, err := session.Prompt(context.Background(), "go", WithToolLogger(&buf)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "[tool] echo\n") {
		t.Fatalf("expected '[tool] echo' in log, got %q", buf.String())
	}
}

func TestStreamHelpers_ComposeWithWithEvents(t *testing.T) {
	var (
		streamBuf bytes.Buffer
		toolBuf   bytes.Buffer
		evCount   int
	)
	agent := NewAgent(AgentOptions{
		Provider: &streamFakeProvider{deltas: []string{"hi"}, emitTool: true, toolName: "echo"},
		Tools: []Tool{
			NewTool[struct{}](ToolSpec{Name: "echo"}, func(_ context.Context, _ struct{}) (ToolResult, error) {
				return TextResult("ok"), nil
			}),
		},
		MaxTurns: 2,
	})
	session, _ := agent.Session(context.Background(), "test")
	_, err := session.Prompt(context.Background(), "go",
		WithStreamWriter(&streamBuf),
		WithToolLogger(&toolBuf),
		WithEvents(func(Event) { evCount++ }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if streamBuf.String() != "hi" {
		t.Fatalf("stream writer dropped events: %q", streamBuf.String())
	}
	if !strings.Contains(toolBuf.String(), "[tool] echo") {
		t.Fatalf("tool logger dropped events: %q", toolBuf.String())
	}
	if evCount == 0 {
		t.Fatal("WithEvents should still fire alongside the helpers")
	}
}
