package glue

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSubagentToolValidation(t *testing.T) {
	t.Parallel()

	agent := NewAgent(AgentOptions{Provider: &recordingProvider{turns: [][]ProviderEvent{textTurn("ok")}}})
	tests := []struct {
		name string
		opts SubagentOptions
		want string
	}{
		{
			name: "missing name",
			opts: SubagentOptions{Description: "delegate", Agent: agent},
			want: "name",
		},
		{
			name: "missing description",
			opts: SubagentOptions{Name: "delegate", Agent: agent},
			want: "description",
		},
		{
			name: "missing agent",
			opts: SubagentOptions{Name: "delegate", Description: "delegate work"},
			want: "agent",
		},
		{
			name: "negative max turns",
			opts: SubagentOptions{Name: "delegate", Description: "delegate work", Agent: agent, MaxTurns: -1},
			want: "MaxTurns",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := SubagentTool(tc.opts); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestSubagentToolCanBeCalledByParentAgent(t *testing.T) {
	t.Parallel()

	childProvider := &recordingProvider{turns: [][]ProviderEvent{textTurn("child answer")}}
	child := NewAgent(AgentOptions{Provider: childProvider})
	tool, err := SubagentTool(SubagentOptions{
		Name:        "researcher",
		Description: "Delegate research work",
		Agent:       child,
	})
	if err != nil {
		t.Fatalf("SubagentTool: %v", err)
	}

	parentProvider := &recordingProvider{turns: [][]ProviderEvent{
		{
			{Type: ProviderEventStart},
			{Type: ProviderEventToolCall, ToolCall: &ToolCall{ID: "c1", Name: "researcher", Arguments: []byte(`{"prompt":"check this"}`)}},
			{Type: ProviderEventDone},
		},
		textTurn("parent done"),
	}}
	parent := NewAgent(AgentOptions{Provider: parentProvider, Tools: []Tool{tool}})
	session, err := parent.Session(context.Background(), "parent")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}

	result, err := session.Prompt(context.Background(), "delegate")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if result.Text != "parent done" {
		t.Fatalf("Text = %q, want parent done", result.Text)
	}
	if got := childProvider.requests[0].Messages[0].Content[0].Text; got != "check this" {
		t.Fatalf("child prompt = %q, want check this", got)
	}
	gotMessages := parentProvider.requests[1].Messages
	if len(gotMessages) != 3 {
		t.Fatalf("parent continuation messages = %d, want 3", len(gotMessages))
	}
	if got := gotMessages[2].Content[0].Text; got != "child answer" {
		t.Fatalf("tool result text = %q, want child answer", got)
	}
}

func TestSubagentToolUsesFreshSessionPerCall(t *testing.T) {
	t.Parallel()

	childProvider := &recordingProvider{turns: [][]ProviderEvent{textTurn("one"), textTurn("two")}}
	child := NewAgent(AgentOptions{Provider: childProvider})
	tool, err := SubagentTool(SubagentOptions{
		Name:        "coder",
		Description: "Write code",
		Agent:       child,
	})
	if err != nil {
		t.Fatalf("SubagentTool: %v", err)
	}

	first, err := tool.Execute(context.Background(), ToolCall{Name: "coder", Arguments: []byte(`{"prompt":"first task"}`)})
	if err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	second, err := tool.Execute(context.Background(), ToolCall{Name: "coder", Arguments: []byte(`{"prompt":"second task"}`)})
	if err != nil {
		t.Fatalf("second Execute: %v", err)
	}

	if first.IsError || second.IsError {
		t.Fatalf("unexpected error results: first=%q second=%q", first.Content[0].Text, second.Content[0].Text)
	}
	if got := childProvider.requests[0].Messages; len(got) != 1 || got[0].Content[0].Text != "first task" {
		t.Fatalf("first child messages = %#v, want one explicit prompt", got)
	}
	if got := childProvider.requests[1].Messages; len(got) != 1 || got[0].Content[0].Text != "second task" {
		t.Fatalf("second child messages = %#v, want fresh transcript", got)
	}
	firstID, _ := first.Metadata["session_id"].(string)
	secondID, _ := second.Metadata["session_id"].(string)
	if firstID == "" || secondID == "" || firstID == secondID {
		t.Fatalf("session ids first=%q second=%q, want distinct non-empty ids", firstID, secondID)
	}
	if !strings.HasPrefix(firstID, "subagent:coder:") || !strings.HasPrefix(secondID, "subagent:coder:") {
		t.Fatalf("session ids first=%q second=%q, want default subagent prefix", firstID, secondID)
	}
}

func TestSubagentToolUsesUniqueSessionIDsAcrossToolInstances(t *testing.T) {
	t.Parallel()

	childProvider := &recordingProvider{turns: [][]ProviderEvent{textTurn("one"), textTurn("two")}}
	child := NewAgent(AgentOptions{Provider: childProvider})
	firstTool, err := SubagentTool(SubagentOptions{
		Name:        "coder_a",
		Description: "Write code",
		Agent:       child,
		SessionID:   "worker",
	})
	if err != nil {
		t.Fatalf("first SubagentTool: %v", err)
	}
	secondTool, err := SubagentTool(SubagentOptions{
		Name:        "coder_b",
		Description: "Write code",
		Agent:       child,
		SessionID:   "worker",
	})
	if err != nil {
		t.Fatalf("second SubagentTool: %v", err)
	}

	first, err := firstTool.Execute(context.Background(), ToolCall{Name: "coder_a", Arguments: []byte(`{"prompt":"first task"}`)})
	if err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	second, err := secondTool.Execute(context.Background(), ToolCall{Name: "coder_b", Arguments: []byte(`{"prompt":"second task"}`)})
	if err != nil {
		t.Fatalf("second Execute: %v", err)
	}

	if got := childProvider.requests[1].Messages; len(got) != 1 || got[0].Content[0].Text != "second task" {
		t.Fatalf("second child messages = %#v, want fresh transcript across tool instances", got)
	}
	firstID, _ := first.Metadata["session_id"].(string)
	secondID, _ := second.Metadata["session_id"].(string)
	if firstID == secondID || !strings.HasPrefix(firstID, "worker:") || !strings.HasPrefix(secondID, "worker:") {
		t.Fatalf("session ids first=%q second=%q, want distinct worker-prefixed ids", firstID, secondID)
	}
}

func TestSubagentToolCustomSessionIDAndSystemPrompt(t *testing.T) {
	t.Parallel()

	childProvider := &recordingProvider{turns: [][]ProviderEvent{textTurn("done")}}
	child := NewAgent(AgentOptions{Provider: childProvider})
	tool, err := SubagentTool(SubagentOptions{
		Name:         "coder",
		Description:  "Write code",
		Agent:        child,
		SessionID:    "worker",
		SystemPrompt: "child-only instructions",
	})
	if err != nil {
		t.Fatalf("SubagentTool: %v", err)
	}

	result, err := tool.Execute(context.Background(), ToolCall{Name: "coder", Arguments: []byte(`{"prompt":"use custom prompt"}`)})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.IsError {
		t.Fatalf("IsError = true: %q", result.Content[0].Text)
	}
	if got := childProvider.requests[0].SystemPrompt; got != "child-only instructions" {
		t.Fatalf("SystemPrompt = %q, want child-only instructions", got)
	}
	sessionID, _ := result.Metadata["session_id"].(string)
	if !strings.HasPrefix(sessionID, "worker:") {
		t.Fatalf("session id = %q, want worker prefix", sessionID)
	}
	session, err := child.Session(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("Session %q: %v", sessionID, err)
	}
	if session.ID() != sessionID {
		t.Fatalf("child session id = %q, want %q", session.ID(), sessionID)
	}
}

func TestSubagentToolMaxTurnsIsErrorResult(t *testing.T) {
	t.Parallel()

	childProvider := &recordingProvider{turns: [][]ProviderEvent{
		{
			{Type: ProviderEventStart},
			{Type: ProviderEventToolCall, ToolCall: &ToolCall{ID: "c1", Name: "echo", Arguments: []byte(`{"value":"x"}`)}},
			{Type: ProviderEventDone},
		},
	}}
	child := NewAgent(AgentOptions{
		Provider: childProvider,
		Tools: []Tool{NewTool[struct {
			Value string `json:"value"`
		}](ToolSpec{
			Name: "echo",
			Parameters: []byte(`{
  "type": "object",
  "properties": { "value": { "type": "string" } },
  "required": ["value"]
}`),
		}, func(context.Context, struct {
			Value string `json:"value"`
		}) (ToolResult, error) {
			return TextResult("echoed"), nil
		})},
	})
	tool, err := SubagentTool(SubagentOptions{
		Name:        "coder",
		Description: "Write code",
		Agent:       child,
		MaxTurns:    1,
	})
	if err != nil {
		t.Fatalf("SubagentTool: %v", err)
	}

	result, err := tool.Execute(context.Background(), ToolCall{Name: "coder", Arguments: []byte(`{"prompt":"loop once"}`)})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content[0].Text, "maximum turns exceeded (1)") {
		t.Fatalf("result = %#v, want max-turn error result", result)
	}
	sessionID, _ := result.Metadata["session_id"].(string)
	if !strings.HasPrefix(sessionID, "subagent:coder:") {
		t.Fatalf("session id = %q, want default subagent prefix", sessionID)
	}
}

func TestSubagentToolChildPromptErrorIsToolError(t *testing.T) {
	t.Parallel()

	child := NewAgent(AgentOptions{Provider: errorProvider{err: errors.New("provider unavailable")}})
	tool, err := SubagentTool(SubagentOptions{
		Name:        "worker",
		Description: "Delegate work",
		Agent:       child,
	})
	if err != nil {
		t.Fatalf("SubagentTool: %v", err)
	}

	result, err := tool.Execute(context.Background(), ToolCall{Name: "worker", Arguments: []byte(`{"prompt":"do it"}`)})
	if err != nil {
		t.Fatalf("Execute error = %v, want nil", err)
	}
	if !result.IsError || !strings.Contains(result.Content[0].Text, "provider unavailable") {
		t.Fatalf("result = %#v, want provider error result", result)
	}
}

func TestSubagentToolPromptValidation(t *testing.T) {
	t.Parallel()

	childProvider := &recordingProvider{turns: [][]ProviderEvent{textTurn("unused")}}
	child := NewAgent(AgentOptions{Provider: childProvider})
	tool, err := SubagentTool(SubagentOptions{
		Name:        "worker",
		Description: "Delegate work",
		Agent:       child,
	})
	if err != nil {
		t.Fatalf("SubagentTool: %v", err)
	}

	result, err := tool.Execute(context.Background(), ToolCall{Name: "worker", Arguments: []byte(`{"prompt":"   "}`)})
	if err != nil {
		t.Fatalf("Execute error = %v, want nil", err)
	}
	if !result.IsError || !strings.Contains(result.Content[0].Text, "prompt is required") {
		t.Fatalf("result = %#v, want prompt validation error", result)
	}
	if childProvider.calls != 0 {
		t.Fatalf("child provider calls = %d, want 0", childProvider.calls)
	}
}

func TestSubagentToolContextCancellationPropagates(t *testing.T) {
	t.Parallel()

	child := NewAgent(AgentOptions{Provider: &recordingProvider{turns: [][]ProviderEvent{textTurn("unused")}}})
	tool, err := SubagentTool(SubagentOptions{
		Name:        "worker",
		Description: "Delegate work",
		Agent:       child,
	})
	if err != nil {
		t.Fatalf("SubagentTool: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = tool.Execute(ctx, ToolCall{Name: "worker", Arguments: []byte(`{"prompt":"do it"}`)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

type errorProvider struct {
	err error
}

func (p errorProvider) Stream(context.Context, ProviderRequest) (<-chan ProviderEvent, error) {
	return nil, p.err
}
