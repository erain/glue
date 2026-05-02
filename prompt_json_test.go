package glue

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func jsonTurn(text string) []ProviderEvent {
	return []ProviderEvent{
		{Type: ProviderEventStart},
		{Type: ProviderEventTextDelta, Delta: text},
		{Type: ProviderEventDone},
	}
}

func TestPromptJSONDecodesIntoStructPointer(t *testing.T) {
	t.Parallel()

	provider := &recordingProvider{turns: [][]ProviderEvent{jsonTurn(`{"name":"glue","count":2}`)}}
	agent := NewAgent(AgentOptions{Provider: provider})
	session, _ := agent.Session(context.Background(), "x")

	var out struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	res, err := session.PromptJSON(context.Background(), "give me a result", &out)
	if err != nil {
		t.Fatalf("PromptJSON: %v", err)
	}
	if out.Name != "glue" || out.Count != 2 {
		t.Fatalf("out = %#v, want glue/2", out)
	}
	if res.Text != `{"name":"glue","count":2}` {
		t.Fatalf("Text = %q, want raw JSON", res.Text)
	}

	if got := provider.requests[0].Options["response_mime_type"]; got != "application/json" {
		t.Fatalf("response_mime_type = %v, want application/json", got)
	}
	if _, ok := provider.requests[0].Options["response_json_schema"]; ok {
		t.Fatal("response_json_schema set without WithJSONSchema")
	}
	if !strings.Contains(provider.requests[0].Messages[0].Content[0].Text, "Respond with only valid JSON") {
		t.Fatalf("user prompt missing JSON-only instruction: %q", provider.requests[0].Messages[0].Content[0].Text)
	}
}

func TestPromptJSONForwardsSchema(t *testing.T) {
	t.Parallel()

	provider := &recordingProvider{turns: [][]ProviderEvent{jsonTurn(`{"name":"glue","count":2}`)}}
	agent := NewAgent(AgentOptions{Provider: provider})
	session, _ := agent.Session(context.Background(), "x")

	schema := json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"count":{"type":"integer"}}}`)
	var out struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	if _, err := session.PromptJSON(context.Background(), "make json", &out, WithJSONSchema(schema)); err != nil {
		t.Fatalf("PromptJSON: %v", err)
	}
	got := provider.requests[0].Options["response_json_schema"]
	if got == nil {
		t.Fatal("response_json_schema missing on provider request")
	}
	asMap, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("response_json_schema = %T, want map[string]any", got)
	}
	if asMap["type"] != "object" {
		t.Fatalf("schema.type = %v, want object", asMap["type"])
	}
	if !strings.Contains(provider.requests[0].Messages[0].Content[0].Text, "must conform to this schema") {
		t.Fatal("schema not embedded in user prompt")
	}
}

func TestPromptJSONInvalidJSONReturnsDecodeError(t *testing.T) {
	t.Parallel()

	provider := &recordingProvider{turns: [][]ProviderEvent{jsonTurn(`{`)}}
	agent := NewAgent(AgentOptions{Provider: provider})
	session, _ := agent.Session(context.Background(), "x")

	var out struct{ Name string }
	_, err := session.PromptJSON(context.Background(), "bad", &out)
	if err == nil || !strings.Contains(err.Error(), "decode JSON result") {
		t.Fatalf("err = %v, want decode JSON result", err)
	}
}

func TestPromptJSONTypeMismatchReturnsDecodeError(t *testing.T) {
	t.Parallel()

	provider := &recordingProvider{turns: [][]ProviderEvent{jsonTurn(`{"count":"twelve"}`)}}
	agent := NewAgent(AgentOptions{Provider: provider})
	session, _ := agent.Session(context.Background(), "x")

	var out struct {
		Count int `json:"count"`
	}
	_, err := session.PromptJSON(context.Background(), "bad", &out)
	if err == nil || !strings.Contains(err.Error(), "decode JSON result") {
		t.Fatalf("err = %v, want decode JSON result", err)
	}
}

func TestPromptJSONNilTargetErrors(t *testing.T) {
	t.Parallel()

	provider := &recordingProvider{turns: [][]ProviderEvent{jsonTurn(`{}`)}}
	agent := NewAgent(AgentOptions{Provider: provider})
	session, _ := agent.Session(context.Background(), "x")

	if _, err := session.PromptJSON(context.Background(), "x", nil); err == nil || !strings.Contains(err.Error(), "nil") {
		t.Fatalf("err = %v, want nil-target error", err)
	}
	if provider.calls != 0 {
		t.Fatalf("provider was called with nil target: calls=%d", provider.calls)
	}
}

func TestPromptJSONNonPointerErrors(t *testing.T) {
	t.Parallel()

	provider := &recordingProvider{turns: [][]ProviderEvent{jsonTurn(`{}`)}}
	agent := NewAgent(AgentOptions{Provider: provider})
	session, _ := agent.Session(context.Background(), "x")

	var out struct{}
	if _, err := session.PromptJSON(context.Background(), "x", out); err == nil || !strings.Contains(err.Error(), "non-nil pointer") {
		t.Fatalf("err = %v, want non-pointer error", err)
	}
}

func TestPromptJSONStringSchemaIsParsed(t *testing.T) {
	t.Parallel()

	provider := &recordingProvider{turns: [][]ProviderEvent{jsonTurn(`{"x":1}`)}}
	agent := NewAgent(AgentOptions{Provider: provider})
	session, _ := agent.Session(context.Background(), "x")

	var out struct {
		X int `json:"x"`
	}
	if _, err := session.PromptJSON(context.Background(), "x", &out, WithJSONSchema(`{"type":"object"}`)); err != nil {
		t.Fatal(err)
	}
	got := provider.requests[0].Options["response_json_schema"]
	if got == nil {
		t.Fatal("schema missing")
	}
	if _, ok := got.(map[string]any); !ok {
		t.Fatalf("schema type = %T, want map[string]any (decoded)", got)
	}
}

func TestPromptJSONInvalidStringSchemaErrors(t *testing.T) {
	t.Parallel()

	provider := &recordingProvider{turns: [][]ProviderEvent{jsonTurn(`{}`)}}
	agent := NewAgent(AgentOptions{Provider: provider})
	session, _ := agent.Session(context.Background(), "x")

	var out struct{}
	_, err := session.PromptJSON(context.Background(), "x", &out, WithJSONSchema("not-json"))
	if err == nil || !strings.Contains(err.Error(), "invalid JSON schema") {
		t.Fatalf("err = %v, want invalid JSON schema", err)
	}
}
