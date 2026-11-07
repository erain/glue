package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/providers/openrouter"
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

	// NewTool surfaces tool failures as an error ToolResult (IsError),
	// not a Go error, so the model can recover instead of crashing the loop.
	tool := localTimeTool()
	result, err := tool.Execute(context.Background(), glue.ToolCall{Name: "local_time", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("Execute: unexpected Go error %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content[0].Text, "timezone") {
		t.Fatalf("result = %#v, want IsError mentioning timezone", result)
	}
}

func TestLocalTimeToolInvalidArgs(t *testing.T) {
	t.Parallel()

	// Malformed JSON arguments are decoded by NewTool and returned as an
	// error ToolResult, again without a Go error.
	tool := localTimeTool()
	result, err := tool.Execute(context.Background(), glue.ToolCall{Name: "local_time", Arguments: json.RawMessage(`not-json`)})
	if err != nil {
		t.Fatalf("Execute: unexpected Go error %v", err)
	}
	if !result.IsError {
		t.Fatalf("result = %#v, want IsError for malformed arguments", result)
	}
}

// TestLiveLocalAgent exercises the example's tool loop end-to-end against
// OpenRouter's openrouter/free meta-router when OPENROUTER_API_KEY is set.
// OPENROUTER_LIVE_MODEL pins a specific model when determinism matters.
func TestLiveLocalAgent(t *testing.T) {
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		t.Skip("OPENROUTER_API_KEY is not set; skipping live example test")
	}
	model := os.Getenv("OPENROUTER_LIVE_MODEL")
	if model == "" {
		model = openrouter.DefaultModel
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	agent := glue.NewAgent(glue.AgentOptions{
		Provider: openrouter.New(openrouter.Options{APIKey: apiKey}),
		Model:    model,
		Tools:    []glue.Tool{localTimeTool()},
	})
	session, err := agent.Session(ctx, "live")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	response, err := session.Prompt(ctx, "Use the local_time tool for America/Toronto, then reply with a short sentence.")
	if err != nil {
		if msg := err.Error(); strings.Contains(msg, "http 429") || strings.Contains(msg, "Rate limit exceeded") {
			t.Skipf("upstream rate-limited (free tier): %v", err)
		}
		t.Fatalf("prompt: %v", err)
	}
	if response.Text == "" {
		t.Fatal("response text is empty")
	}
}
