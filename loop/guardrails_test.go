package loop

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// guardTool is a scriptable tool whose executor returns errors or
// successes per call.
func guardTool(name string, results []bool) Tool {
	i := 0
	return Tool{
		ToolSpec: ToolSpec{Name: name},
		Execute: func(_ context.Context, _ ToolCall) (ToolResult, error) {
			ok := true
			if i < len(results) {
				ok = results[i]
			}
			i++
			if ok {
				return ToolResult{Content: []ContentPart{{Type: ContentTypeText, Text: "fine"}}}, nil
			}
			return ToolResult{IsError: true, Content: []ContentPart{{Type: ContentTypeText, Text: "boom"}}}, nil
		},
	}
}

// callTurn scripts an assistant turn making one tool call.
func callTurn(id, name, args string) []ProviderEvent {
	return []ProviderEvent{
		{Type: ProviderEventToolCall, ToolCall: &ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}},
		{Type: ProviderEventDone},
	}
}

func TestGuardrailRepeatNudgeThenHalt(t *testing.T) {
	t.Parallel()
	// The model repeats the exact same call every turn, ignoring the nudge.
	turns := [][]ProviderEvent{}
	for i := 0; i < 6; i++ {
		turns = append(turns, callTurn("c", "probe", `{"x":1}`))
	}
	provider := &fakeProvider{turns: turns}
	var events []Event
	_, err := Run(context.Background(), RunRequest{
		Provider: provider,
		Tools:    []Tool{guardTool("probe", nil)},
		Messages: []Message{{Role: MessageRoleUser, Content: []ContentPart{{Type: ContentTypeText, Text: "go"}}}},
		Emit: func(e Event) {
			if e.Type == EventGuardrail {
				events = append(events, e)
			}
		},
	})
	if !errors.Is(err, ErrRepeatedToolCalls) {
		t.Fatalf("err = %v, want ErrRepeatedToolCalls", err)
	}
	if provider.calls != 5 {
		t.Fatalf("provider calls = %d, want 5 (halt at the 5th identical call)", provider.calls)
	}
	if len(events) != 2 {
		t.Fatalf("guardrail events = %d, want 2 (nudge then halt)", len(events))
	}
	if events[0].Metadata["action"] != "nudge" || events[1].Metadata["action"] != "halt" {
		t.Fatalf("event actions = %v / %v", events[0].Metadata["action"], events[1].Metadata["action"])
	}
}

func TestGuardrailRepeatResetsOnDifferentArgs(t *testing.T) {
	t.Parallel()
	turns := [][]ProviderEvent{
		callTurn("a", "probe", `{"x":1}`),
		callTurn("b", "probe", `{"x":2}`),
		callTurn("c", "probe", `{"x":3}`),
		callTurn("d", "probe", `{"x":4}`),
		{{Type: ProviderEventTextDelta, Delta: "done"}, {Type: ProviderEventDone}},
	}
	provider := &fakeProvider{turns: turns}
	res, err := Run(context.Background(), RunRequest{
		Provider: provider,
		Tools:    []Tool{guardTool("probe", nil)},
		Messages: []Message{{Role: MessageRoleUser, Content: []ContentPart{{Type: ContentTypeText, Text: "go"}}}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, m := range res.NewMessages {
		if m.Metadata["glue/guardrail"] != nil {
			t.Fatalf("unexpected guardrail message: %#v", m)
		}
	}
}

func TestGuardrailMistakesNudgeThenHalt(t *testing.T) {
	t.Parallel()
	// Different args each turn (no repeat trigger), but every result
	// fails. Nudge after 3 failed rounds, halt after 6.
	turns := [][]ProviderEvent{}
	for i := 0; i < 7; i++ {
		turns = append(turns, callTurn("c", "probe", `{"attempt":`+string(rune('0'+i))+`}`))
	}
	provider := &fakeProvider{turns: turns}
	fails := make([]bool, 10) // all false = all errors
	var nudges, halts int
	_, err := Run(context.Background(), RunRequest{
		Provider: provider,
		Tools:    []Tool{guardTool("probe", fails)},
		Messages: []Message{{Role: MessageRoleUser, Content: []ContentPart{{Type: ContentTypeText, Text: "go"}}}},
		Emit: func(e Event) {
			if e.Type == EventGuardrail {
				switch e.Metadata["action"] {
				case "nudge":
					nudges++
				case "halt":
					halts++
				}
			}
		},
	})
	if !errors.Is(err, ErrTooManyMistakes) {
		t.Fatalf("err = %v, want ErrTooManyMistakes", err)
	}
	if provider.calls != 6 {
		t.Fatalf("provider calls = %d, want 6", provider.calls)
	}
	if nudges != 1 || halts != 1 {
		t.Fatalf("nudges = %d halts = %d, want 1/1", nudges, halts)
	}
}

func TestGuardrailMistakesResetOnSuccess(t *testing.T) {
	t.Parallel()
	turns := [][]ProviderEvent{
		callTurn("a", "probe", `{"x":1}`),
		callTurn("b", "probe", `{"x":2}`),
		callTurn("c", "probe", `{"x":3}`), // success resets the streak
		callTurn("d", "probe", `{"x":4}`),
		{{Type: ProviderEventTextDelta, Delta: "done"}, {Type: ProviderEventDone}},
	}
	provider := &fakeProvider{turns: turns}
	res, err := Run(context.Background(), RunRequest{
		Provider: provider,
		Tools:    []Tool{guardTool("probe", []bool{false, false, true, false})},
		Messages: []Message{{Role: MessageRoleUser, Content: []ContentPart{{Type: ContentTypeText, Text: "go"}}}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, m := range res.NewMessages {
		if m.Metadata["glue/guardrail"] != nil {
			t.Fatalf("streak should have reset, got nudge: %#v", m)
		}
	}
}

func TestGuardrailDisabled(t *testing.T) {
	t.Parallel()
	turns := [][]ProviderEvent{}
	for i := 0; i < 7; i++ {
		turns = append(turns, callTurn("c", "probe", `{"x":1}`))
	}
	turns = append(turns, []ProviderEvent{{Type: ProviderEventTextDelta, Delta: "done"}, {Type: ProviderEventDone}})
	provider := &fakeProvider{turns: turns}
	_, err := Run(context.Background(), RunRequest{
		Provider:   provider,
		Tools:      []Tool{guardTool("probe", nil)},
		Guardrails: GuardrailPolicy{Disabled: true},
		Messages:   []Message{{Role: MessageRoleUser, Content: []ContentPart{{Type: ContentTypeText, Text: "go"}}}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if provider.calls != 8 {
		t.Fatalf("provider calls = %d, want 8 (no guardrails)", provider.calls)
	}
}

func TestGuardrailNudgeMessageShape(t *testing.T) {
	t.Parallel()
	turns := [][]ProviderEvent{
		callTurn("a", "probe", `{"x":1}`),
		callTurn("b", "probe", `{"x":1}`),
		callTurn("c", "probe", `{"x":1}`),
		{{Type: ProviderEventTextDelta, Delta: "ok I'll stop"}, {Type: ProviderEventDone}},
	}
	provider := &fakeProvider{turns: turns}
	res, err := Run(context.Background(), RunRequest{
		Provider: provider,
		Tools:    []Tool{guardTool("probe", nil)},
		Messages: []Message{{Role: MessageRoleUser, Content: []ContentPart{{Type: ContentTypeText, Text: "go"}}}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var nudge *Message
	for i := range res.NewMessages {
		if res.NewMessages[i].Metadata["glue/guardrail"] == "repeat" {
			nudge = &res.NewMessages[i]
		}
	}
	if nudge == nil {
		t.Fatalf("no guardrail nudge in transcript: %#v", res.NewMessages)
	}
	if nudge.Role != MessageRoleUser {
		t.Fatalf("nudge role = %s", nudge.Role)
	}
}
