package codex

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/erain/glue/loop"
)

func TestMessagesToInput_UserAndAssistantText(t *testing.T) {
	msgs := []loop.Message{
		{Role: loop.MessageRoleUser, Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "hi"}}},
		{Role: loop.MessageRoleAssistant, Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "hello!"}}},
	}
	got, err := messagesToInput(msgs)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d items: %+v", len(got), got)
	}
	if got[0].Type != "message" || got[0].Role != "user" || got[0].Content[0].Type != "input_text" || got[0].Content[0].Text != "hi" {
		t.Errorf("user item wrong: %+v", got[0])
	}
	if got[1].Type != "message" || got[1].Role != "assistant" || got[1].Content[0].Type != "output_text" || got[1].Content[0].Text != "hello!" {
		t.Errorf("assistant item wrong: %+v", got[1])
	}
}

func TestMessagesToInput_FunctionCallRoundTrip(t *testing.T) {
	args := json.RawMessage(`{"q":"weather"}`)
	msgs := []loop.Message{
		{Role: loop.MessageRoleUser, Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "ask"}}},
		{
			Role: loop.MessageRoleAssistant,
			Content: []loop.ContentPart{
				{Type: loop.ContentTypeText, Text: "I'll check."},
				{Type: loop.ContentTypeToolCall, ToolCall: &loop.ToolCall{
					ID: "call_42", Name: "weather", Arguments: args,
				}},
			},
		},
		{
			Role:       loop.MessageRoleTool,
			ToolCallID: "call_42",
			ToolName:   "weather",
			Content:    []loop.ContentPart{{Type: loop.ContentTypeText, Text: "sunny"}},
		},
	}
	got, err := messagesToInput(msgs)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	// Expect: user message, assistant message, function_call, function_call_output
	if len(got) != 4 {
		t.Fatalf("got %d items: %+v", len(got), got)
	}
	if got[2].Type != "function_call" || got[2].CallID != "call_42" || got[2].Name != "weather" {
		t.Errorf("function_call item wrong: %+v", got[2])
	}
	if !strings.Contains(got[2].Arguments, "weather") {
		t.Errorf("function_call arguments not forwarded: %q", got[2].Arguments)
	}
	if got[3].Type != "function_call_output" || got[3].CallID != "call_42" || got[3].Output != "sunny" {
		t.Errorf("function_call_output item wrong: %+v", got[3])
	}
}

func TestMessagesToInput_ToolResultWithoutCallIDErrors(t *testing.T) {
	msgs := []loop.Message{{Role: loop.MessageRoleTool, Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "x"}}}}
	if _, err := messagesToInput(msgs); err == nil {
		t.Fatal("want error for tool result without call id")
	}
}

func TestMessagesToInput_RefusesImageContent(t *testing.T) {
	msgs := []loop.Message{{
		Role:    loop.MessageRoleUser,
		Content: []loop.ContentPart{{Type: loop.ContentTypeImage, Image: &loop.ImageContent{Data: "Zg==", MIMEType: "image/png"}}},
	}}
	if _, err := messagesToInput(msgs); err == nil {
		t.Fatal("want error for image content (out of scope v0.1)")
	}
}

func TestMessagesToInput_DropsEmptyUserText(t *testing.T) {
	msgs := []loop.Message{{Role: loop.MessageRoleUser, Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: ""}}}}
	got, err := messagesToInput(msgs)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty result, got %+v", got)
	}
}

func TestMessagesToInput_AssistantWithOnlyToolCall(t *testing.T) {
	msgs := []loop.Message{
		{
			Role: loop.MessageRoleAssistant,
			Content: []loop.ContentPart{{Type: loop.ContentTypeToolCall, ToolCall: &loop.ToolCall{
				ID: "c", Name: "fn", Arguments: json.RawMessage(`{}`),
			}}},
		},
	}
	got, err := messagesToInput(msgs)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	// Only function_call should be emitted; no empty assistant message.
	if len(got) != 1 || got[0].Type != "function_call" {
		t.Errorf("expected single function_call, got %+v", got)
	}
}

func TestToolSpecsToTools(t *testing.T) {
	specs := []loop.ToolSpec{
		{Name: "weather", Description: "look up", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "no_params", Description: ""}, // empty parameters → "{}"
	}
	got := toolSpecsToTools(specs)
	if len(got) != 2 {
		t.Fatalf("got %d", len(got))
	}
	if got[0].Type != "function" || got[0].Name != "weather" || got[0].Description != "look up" {
		t.Errorf("first tool: %+v", got[0])
	}
	if string(got[1].Parameters) != "{}" {
		t.Errorf("empty params should default to '{}', got %q", got[1].Parameters)
	}
}

func TestToolSpecsToTools_NilForNoSpecs(t *testing.T) {
	if got := toolSpecsToTools(nil); got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}
