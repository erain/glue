package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/providers/gemini"
)

func TestLocalTimeToolHappyPath(t *testing.T) {
	t.Parallel()

	tool := localTimeTool()
	result, err := tool.Execute(context.Background(), glue.ToolCall{
		ID:        "call_1",
		Name:      "local_time",
		Arguments: json.RawMessage(`{"timezone":"America/Toronto"}`),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.IsError {
		t.Fatalf("result is error: %#v", result)
	}
	if len(result.Content) != 1 || result.Content[0].Type != glue.ContentTypeText {
		t.Fatalf("content = %#v, want one text part", result.Content)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(result.Content[0].Text), &payload); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if payload["timezone"] != "America/Toronto" || payload["time"] == "" {
		t.Fatalf("payload = %#v, want timezone and time", payload)
	}
}

func TestLocalTimeToolMissingTimezone(t *testing.T) {
	t.Parallel()

	tool := localTimeTool()
	_, err := tool.Execute(context.Background(), glue.ToolCall{Name: "local_time", Arguments: json.RawMessage(`{}`)})
	if err == nil || !strings.Contains(err.Error(), "timezone") {
		t.Fatalf("err = %v, want timezone required", err)
	}
}

func TestLocalTimeToolInvalidArgs(t *testing.T) {
	t.Parallel()

	tool := localTimeTool()
	_, err := tool.Execute(context.Background(), glue.ToolCall{Name: "local_time", Arguments: json.RawMessage(`not-json`)})
	if err == nil {
		t.Fatal("err = nil, want unmarshal error")
	}
}

// TestLiveLocalAgent exercises the example end-to-end against Gemini when
// GEMINI_API_KEY is set. It is skipped in CI.
func TestLiveLocalAgent(t *testing.T) {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		t.Skip("GEMINI_API_KEY is not set; skipping live example test")
	}
	model := os.Getenv("GEMINI_MODEL")
	if model == "" {
		model = "gemini-2.5-flash"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	agent := glue.NewAgent(glue.AgentOptions{
		Provider: gemini.New(gemini.Options{APIKey: apiKey}),
		Model:    model,
		Tools:    []glue.Tool{localTimeTool()},
	})
	session, err := agent.Session(ctx, "live")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	response, err := session.Prompt(ctx, "Use the local_time tool for America/Toronto, then reply with a short sentence.")
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if response.Text == "" {
		t.Fatal("response text is empty")
	}
}
