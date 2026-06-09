package loop

import (
	"strings"
	"testing"
)

func toolCallMsg(model string, calls ...ToolCall) Message {
	parts := make([]ContentPart, 0, len(calls))
	for i := range calls {
		c := calls[i]
		parts = append(parts, ContentPart{Type: ContentTypeToolCall, ToolCall: &c})
	}
	return Message{Role: MessageRoleAssistant, Model: model, Content: parts}
}

func toolResultMsg(id, name, text string) Message {
	return Message{
		Role:       MessageRoleTool,
		ToolCallID: id,
		ToolName:   name,
		Content:    []ContentPart{{Type: ContentTypeText, Text: text}},
	}
}

func textMsg(role MessageRole, text string) Message {
	return Message{Role: role, Content: []ContentPart{{Type: ContentTypeText, Text: text}}}
}

func TestHardenSynthesizesMissingToolResult(t *testing.T) {
	history := []Message{
		textMsg(MessageRoleUser, "run the tests"),
		toolCallMsg("m", ToolCall{ID: "c1", Name: "shell_exec"}),
		// interrupted: no tool result for c1
		textMsg(MessageRoleUser, "continue"),
	}
	out := HardenHistory(history, "m")
	if len(out) != 4 {
		t.Fatalf("len = %d, want 4: %#v", len(out), out)
	}
	syn := out[2]
	if syn.Role != MessageRoleTool || syn.ToolCallID != "c1" || !syn.IsError {
		t.Fatalf("synthesized result wrong: %#v", syn)
	}
	if !strings.Contains(syn.Content[0].Text, "interrupted") {
		t.Fatalf("synthesized text = %q", syn.Content[0].Text)
	}
	if syn.Metadata["glue/synthetic"] != true {
		t.Fatalf("missing synthetic marker: %#v", syn.Metadata)
	}
}

func TestHardenKeepsCompletePairsUntouched(t *testing.T) {
	history := []Message{
		textMsg(MessageRoleUser, "hi"),
		toolCallMsg("m", ToolCall{ID: "c1", Name: "read_file"}),
		toolResultMsg("c1", "read_file", "content"),
		textMsg(MessageRoleAssistant, "done"),
	}
	out := HardenHistory(history, "m")
	if len(out) != 4 {
		t.Fatalf("len = %d, want 4", len(out))
	}
	for i := range history {
		if out[i].Role != history[i].Role {
			t.Fatalf("message %d role changed", i)
		}
	}
}

func TestHardenDropsOrphanedToolResult(t *testing.T) {
	history := []Message{
		textMsg(MessageRoleUser, "hi"),
		toolResultMsg("ghost", "read_file", "stale"),
		textMsg(MessageRoleAssistant, "ok"),
	}
	out := HardenHistory(history, "m")
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2: %#v", len(out), out)
	}
	for _, m := range out {
		if m.Role == MessageRoleTool {
			t.Fatal("orphaned tool result survived")
		}
	}
}

func TestHardenDropsEmptyAssistantTurn(t *testing.T) {
	history := []Message{
		textMsg(MessageRoleUser, "hi"),
		{Role: MessageRoleAssistant, Content: []ContentPart{{Type: ContentTypeText, Text: "  "}}},
		textMsg(MessageRoleAssistant, "real answer"),
	}
	out := HardenHistory(history, "m")
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2: %#v", len(out), out)
	}
	if out[1].Content[0].Text != "real answer" {
		t.Fatalf("kept wrong assistant turn: %#v", out[1])
	}
}

func TestHardenNormalizesToolCallIDs(t *testing.T) {
	bad := "call|with spaces!"
	history := []Message{
		toolCallMsg("m", ToolCall{ID: bad, Name: "grep"}),
		toolResultMsg(bad, "grep", "hits"),
	}
	out := HardenHistory(history, "m")
	call := collectToolCalls(out[0])[0]
	if strings.ContainsAny(call.ID, "| !") {
		t.Fatalf("call ID not normalized: %q", call.ID)
	}
	if out[1].ToolCallID != call.ID {
		t.Fatalf("result ID %q != call ID %q", out[1].ToolCallID, call.ID)
	}
}

func TestHardenNormalizationAvoidsCollision(t *testing.T) {
	history := []Message{
		toolCallMsg("m",
			ToolCall{ID: "call_1", Name: "a"},
			ToolCall{ID: "call 1", Name: "b"}, // sanitizes to call_1
		),
		toolResultMsg("call_1", "a", "ra"),
		toolResultMsg("call 1", "b", "rb"),
	}
	out := HardenHistory(history, "m")
	calls := collectToolCalls(out[0])
	if calls[0].ID == calls[1].ID {
		t.Fatalf("collision not avoided: %q", calls[0].ID)
	}
	if out[1].ToolCallID != calls[0].ID || out[2].ToolCallID != calls[1].ID {
		t.Fatalf("results remapped wrong: %q %q vs %q %q", out[1].ToolCallID, out[2].ToolCallID, calls[0].ID, calls[1].ID)
	}
}

func TestHardenStripsForeignModelArtifacts(t *testing.T) {
	foreign := Message{
		Role:  MessageRoleAssistant,
		Model: "other-model",
		Content: []ContentPart{
			{Type: ContentTypeThinking, Thinking: "secret reasoning", Signature: "sig"},
			{Type: ContentTypeText, Text: "answer", Signature: "sig2"},
		},
	}
	out := HardenHistory([]Message{foreign}, "active-model")
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	for _, p := range out[0].Content {
		if p.Type == ContentTypeThinking {
			t.Fatal("foreign thinking block survived")
		}
		if p.Signature != "" {
			t.Fatal("foreign signature survived")
		}
	}
	// Same model: untouched.
	out = HardenHistory([]Message{foreign}, "other-model")
	if out[0].Content[0].Type != ContentTypeThinking || out[0].Content[1].Signature != "sig2" {
		t.Fatal("same-model turn should be untouched")
	}
}

func TestHardenDoesNotModifyInput(t *testing.T) {
	history := []Message{
		toolCallMsg("m", ToolCall{ID: "bad id", Name: "x"}),
	}
	_ = HardenHistory(history, "m")
	if collectToolCalls(history[0])[0].ID != "bad id" {
		t.Fatal("input slice was mutated")
	}
}

func TestHardenLeavesLaterNonContiguousResultAlone(t *testing.T) {
	history := []Message{
		toolCallMsg("m", ToolCall{ID: "c1", Name: "x"}),
		textMsg(MessageRoleUser, "interruption"),
		toolResultMsg("c1", "x", "late result"),
	}
	out := HardenHistory(history, "m")
	// No synthesis: the result exists, just not contiguously.
	count := 0
	for _, m := range out {
		if m.Role == MessageRoleTool && m.ToolCallID == "c1" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one c1 result, got %d: %#v", count, out)
	}
}
