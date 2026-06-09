package loop

import (
	"context"
	"encoding/json"
	"testing"
)

func TestStallIntent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		text string
		want bool
	}{
		{"I will now update the router to log requests.", true},
		{"Let me run the tests.", true},
		{"I'm going to refactor the helper next.", true},
		{"Next, I'll wire the handler.", true},
		{"The tests pass and the feature is complete.", false},
		{"I will need your API key — what is it?", false},
		{"Done. Anything else?", false},
		{"", false},
		// Future intent earlier, but the closing sentence concludes.
		{"I will check the file.\nThe file looks correct, no change needed.", false},
	}
	for _, c := range cases {
		m := Message{Role: MessageRoleAssistant, Content: []ContentPart{{Type: ContentTypeText, Text: c.text}}}
		if got := stallIntent(m); got != c.want {
			t.Errorf("stallIntent(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

func TestStallIntentIgnoresTurnsWithToolCalls(t *testing.T) {
	t.Parallel()
	call := ToolCall{ID: "c", Name: "x", Arguments: json.RawMessage(`{}`)}
	m := Message{Role: MessageRoleAssistant, Content: []ContentPart{
		{Type: ContentTypeText, Text: "I will now edit the file."},
		{Type: ContentTypeToolCall, ToolCall: &call},
	}}
	if stallIntent(m) {
		t.Fatal("a turn that did call a tool is not a stall")
	}
}

func TestAutoContinueNudgesStalledTurn(t *testing.T) {
	t.Parallel()
	provider := &fakeProvider{turns: [][]ProviderEvent{
		{{Type: ProviderEventTextDelta, Delta: "I will now run the tests."}, {Type: ProviderEventDone}},
		{{Type: ProviderEventTextDelta, Delta: "All tests pass; nothing else to do."}, {Type: ProviderEventDone}},
	}}
	var nudges int
	res, err := Run(context.Background(), RunRequest{
		Provider:     provider,
		AutoContinue: true,
		Tools:        []Tool{{ToolSpec: ToolSpec{Name: "shell_exec"}}},
		Messages:     []Message{{Role: MessageRoleUser, Content: []ContentPart{{Type: ContentTypeText, Text: "run the tests"}}}},
		Emit: func(e Event) {
			if e.Type == EventAutoContinue {
				nudges++
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if nudges != 1 {
		t.Fatalf("auto-continue events = %d, want 1", nudges)
	}
	// Transcript: user, stalled assistant, synthetic user, final assistant.
	if len(res.NewMessages) != 3 {
		t.Fatalf("new messages = %d, want 3 (stall, nudge, final)", len(res.NewMessages))
	}
	nudge := res.NewMessages[1]
	if nudge.Role != MessageRoleUser || nudge.Content[0].Text != autoContinueMessage {
		t.Fatalf("nudge = %#v", nudge)
	}
	if nudge.Metadata["glue/auto-continue"] != true {
		t.Fatalf("nudge metadata = %#v", nudge.Metadata)
	}
	final := res.NewMessages[2]
	if final.Content[0].Text != "All tests pass; nothing else to do." {
		t.Fatalf("final text = %q", final.Content[0].Text)
	}
}

func TestAutoContinueBounded(t *testing.T) {
	t.Parallel()
	stall := []ProviderEvent{{Type: ProviderEventTextDelta, Delta: "I will now do the thing."}, {Type: ProviderEventDone}}
	provider := &fakeProvider{turns: [][]ProviderEvent{stall, stall, stall, stall}}
	res, err := Run(context.Background(), RunRequest{
		Provider:     provider,
		AutoContinue: true,
		Tools:        []Tool{{ToolSpec: ToolSpec{Name: "shell_exec"}}},
		Messages:     []Message{{Role: MessageRoleUser, Content: []ContentPart{{Type: ContentTypeText, Text: "go"}}}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 2 nudges max: stall, nudge, stall, nudge, stall — then give up.
	if provider.calls != 3 {
		t.Fatalf("provider calls = %d, want 3 (bounded nudging)", provider.calls)
	}
	last := res.NewMessages[len(res.NewMessages)-1]
	if last.Role != MessageRoleAssistant {
		t.Fatalf("run must end on the assistant turn, got %s", last.Role)
	}
}

func TestAutoContinueOffByDefault(t *testing.T) {
	t.Parallel()
	provider := &fakeProvider{turns: [][]ProviderEvent{
		{{Type: ProviderEventTextDelta, Delta: "I will now run the tests."}, {Type: ProviderEventDone}},
	}}
	_, err := Run(context.Background(), RunRequest{
		Provider: provider,
		Tools:    []Tool{{ToolSpec: ToolSpec{Name: "shell_exec"}}},
		Messages: []Message{{Role: MessageRoleUser, Content: []ContentPart{{Type: ContentTypeText, Text: "go"}}}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want 1 (no auto-continue by default)", provider.calls)
	}
}

func TestAutoContinueRequiresTools(t *testing.T) {
	t.Parallel()
	provider := &fakeProvider{turns: [][]ProviderEvent{
		{{Type: ProviderEventTextDelta, Delta: "I will now think about this."}, {Type: ProviderEventDone}},
	}}
	_, err := Run(context.Background(), RunRequest{
		Provider:     provider,
		AutoContinue: true,
		Messages:     []Message{{Role: MessageRoleUser, Content: []ContentPart{{Type: ContentTypeText, Text: "go"}}}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want 1 (no tools, no nudge)", provider.calls)
	}
}
