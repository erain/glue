package loop

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestMessageJSONRoundTrip(t *testing.T) {
	t.Parallel()

	createdAt := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	msg := Message{
		ID:   "msg_1",
		Role: MessageRoleAssistant,
		Content: []ContentPart{
			{Type: ContentTypeText, Text: "Checking the weather."},
			{
				Type: ContentTypeToolCall,
				ToolCall: &ToolCall{
					ID:        "call_1",
					Name:      "weather",
					Arguments: json.RawMessage(`{"city":"Toronto"}`),
				},
			},
		},
		Provider:   "gemini",
		Model:      "gemini-2.5-flash",
		StopReason: StopReasonToolUse,
		Usage:      &Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
		CreatedAt:  createdAt,
		Metadata:   map[string]any{"response_id": "resp_1"},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}

	var got Message
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}

	if got.Role != MessageRoleAssistant {
		t.Fatalf("role = %q, want %q", got.Role, MessageRoleAssistant)
	}
	if got.StopReason != StopReasonToolUse {
		t.Fatalf("stop reason = %q, want %q", got.StopReason, StopReasonToolUse)
	}
	if len(got.Content) != 2 {
		t.Fatalf("content len = %d, want 2", len(got.Content))
	}
	call := got.Content[1].ToolCall
	if call == nil {
		t.Fatal("tool call missing after round trip")
	}
	if call.ID != "call_1" || call.Name != "weather" {
		t.Fatalf("tool call = %#v, want id call_1 and name weather", call)
	}
	var args map[string]string
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		t.Fatalf("unmarshal tool args: %v", err)
	}
	if args["city"] != "Toronto" {
		t.Fatalf("city arg = %q, want Toronto", args["city"])
	}
	if got.Usage == nil || got.Usage.TotalTokens != 15 {
		t.Fatalf("usage = %#v, want total tokens 15", got.Usage)
	}
	if !got.CreatedAt.Equal(createdAt) {
		t.Fatalf("created at = %s, want %s", got.CreatedAt, createdAt)
	}
}

func TestToolMarshalExcludesExecutor(t *testing.T) {
	t.Parallel()

	tool := Tool{
		ToolSpec: ToolSpec{
			Name:        "weather",
			Description: "Look up weather for a city.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		},
		Execute: func(context.Context, ToolCall) (ToolResult, error) {
			return ToolResult{Content: []ContentPart{{Type: ContentTypeText, Text: "sunny"}}}, nil
		},
	}

	data, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("marshal tool: %v", err)
	}
	if !json.Valid(data) {
		t.Fatalf("tool JSON is invalid: %s", data)
	}
	if strings.Contains(string(data), "Execute") {
		t.Fatalf("tool JSON leaks Execute: %s", data)
	}

	var got ToolSpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal tool spec: %v", err)
	}
	if got.Name != "weather" {
		t.Fatalf("name = %q, want weather", got.Name)
	}
	if len(got.Parameters) == 0 {
		t.Fatal("parameters missing after round trip")
	}
}

func TestToolResultJSONRoundTrip(t *testing.T) {
	t.Parallel()

	res := ToolResult{
		Content:  []ContentPart{{Type: ContentTypeText, Text: "sunny, 21C"}},
		IsError:  false,
		Metadata: map[string]any{"source": "fake"},
	}
	data, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ToolResult
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Content) != 1 || got.Content[0].Text != "sunny, 21C" {
		t.Fatalf("content = %#v, want one text part 'sunny, 21C'", got.Content)
	}
	if got.Metadata["source"] != "fake" {
		t.Fatalf("metadata[source] = %v, want fake", got.Metadata["source"])
	}
}

func TestProviderEventJSONRoundTrip(t *testing.T) {
	t.Parallel()

	ev := ProviderEvent{Type: ProviderEventTextDelta, Delta: "hello"}
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ProviderEvent
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != ProviderEventTextDelta || got.Delta != "hello" {
		t.Fatalf("event = %#v, want text delta 'hello'", got)
	}
}

func TestLoopEventJSONRoundTrip(t *testing.T) {
	t.Parallel()

	ev := Event{
		Type:       EventToolEnd,
		ToolCallID: "call_1",
		ToolName:   "weather",
		ToolResult: &ToolResult{
			Content: []ContentPart{{Type: ContentTypeText, Text: "sunny"}},
		},
	}
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Event
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != EventToolEnd {
		t.Fatalf("type = %q, want %q", got.Type, EventToolEnd)
	}
	if got.ToolResult == nil || len(got.ToolResult.Content) != 1 {
		t.Fatalf("tool result = %#v, want one content part", got.ToolResult)
	}
	if got.ToolName != "weather" || got.ToolCallID != "call_1" {
		t.Fatalf("tool name/id = %q/%q, want weather/call_1", got.ToolName, got.ToolCallID)
	}
}

func TestProviderInterfaceCompiles(t *testing.T) {
	t.Parallel()

	var _ Provider = staticProvider{}
}

type staticProvider struct{}

func (staticProvider) Stream(_ context.Context, _ ProviderRequest) (<-chan ProviderEvent, error) {
	ch := make(chan ProviderEvent)
	close(ch)
	return ch, nil
}
