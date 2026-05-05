package nvidia

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/erain/glue/loop"
)

// captureProviderEvents drains every event from the provider stream into a
// slice for assertion, with a generous safety timeout so a hung test fails
// loudly instead of blocking CI.
func captureProviderEvents(t *testing.T, ch <-chan loop.ProviderEvent) []loop.ProviderEvent {
	t.Helper()
	var out []loop.ProviderEvent
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("timed out reading provider events; got %d so far", len(out))
		case event, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, event)
		}
	}
}

// fakeServer wraps an httptest.Server and captures the last request body so
// tests can assert against the on-the-wire payload.
type fakeServer struct {
	*httptest.Server
	lastBody []byte
}

func newFakeServer(t *testing.T, body string) *fakeServer {
	t.Helper()
	fs := &fakeServer{}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		fs.lastBody = raw
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(fs.Close)
	return fs
}

func newProvider(t *testing.T, server *httptest.Server) *Provider {
	t.Helper()
	return New(Options{
		APIKey:     "test-key",
		BaseURL:    server.URL,
		HTTPClient: server.Client(),
	})
}

func TestStreamEmitsTextDeltasAndDone(t *testing.T) {
	body := strings.Join([]string{
		`data: {"id":"abc","model":"test","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}`,
		``,
		`data: {"id":"abc","model":"test","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}`,
		``,
		`data: {"id":"abc","model":"test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	server := newFakeServer(t, body)

	provider := newProvider(t, server.Server)
	ch, err := provider.Stream(context.Background(), loop.ProviderRequest{
		Model:    "test",
		Messages: []loop.Message{{Role: loop.MessageRoleUser, Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := captureProviderEvents(t, ch)

	var deltas []string
	var done *loop.Message
	var sawStart bool
	for _, e := range events {
		switch e.Type {
		case loop.ProviderEventStart:
			sawStart = true
		case loop.ProviderEventTextDelta:
			deltas = append(deltas, e.Delta)
		case loop.ProviderEventDone:
			done = e.Message
		case loop.ProviderEventError:
			t.Fatalf("unexpected error event: %s", e.Error)
		}
	}
	if !sawStart {
		t.Fatalf("missing Start event")
	}
	if got, want := strings.Join(deltas, ""), "Hello world"; got != want {
		t.Fatalf("text delta concat: got %q want %q", got, want)
	}
	if done == nil {
		t.Fatalf("missing Done event")
	}
	if done.StopReason != loop.StopReasonStop {
		t.Fatalf("StopReason: got %q want stop", done.StopReason)
	}
	if done.Usage == nil || done.Usage.InputTokens != 3 || done.Usage.OutputTokens != 2 || done.Usage.TotalTokens != 5 {
		t.Fatalf("Usage: got %+v", done.Usage)
	}
	if done.Metadata["response_id"] != "abc" {
		t.Fatalf("response_id metadata: %+v", done.Metadata)
	}
}

func TestStreamEmitsThinkingDeltas(t *testing.T) {
	body := strings.Join([]string{
		`data: {"id":"a","model":"x","choices":[{"index":0,"delta":{"reasoning_content":"thinking..."},"finish_reason":null}]}`,
		``,
		`data: {"id":"a","model":"x","choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	server := newFakeServer(t, body)

	ch, err := newProvider(t, server.Server).Stream(context.Background(), loop.ProviderRequest{Model: "x"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := captureProviderEvents(t, ch)

	var thinking string
	for _, e := range events {
		if e.Type == loop.ProviderEventThinkingDelta {
			thinking += e.Delta
		}
	}
	if thinking != "thinking..." {
		t.Fatalf("thinking delta: got %q", thinking)
	}
}

func TestStreamAccumulatesToolCalls(t *testing.T) {
	body := strings.Join([]string{
		`data: {"id":"a","model":"x","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		``,
		`data: {"id":"a","model":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"add"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"a","model":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"a","model":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1,\"b\":2}"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"a","model":"x","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	server := newFakeServer(t, body)

	ch, err := newProvider(t, server.Server).Stream(context.Background(), loop.ProviderRequest{Model: "x"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := captureProviderEvents(t, ch)

	var toolCalls []*loop.ToolCall
	var done *loop.Message
	for _, e := range events {
		if e.Type == loop.ProviderEventToolCall {
			toolCalls = append(toolCalls, e.ToolCall)
		}
		if e.Type == loop.ProviderEventDone {
			done = e.Message
		}
	}
	if len(toolCalls) != 1 {
		t.Fatalf("tool calls: got %d want 1", len(toolCalls))
	}
	tc := toolCalls[0]
	if tc.ID != "call_1" || tc.Name != "add" {
		t.Fatalf("tool call identity: %+v", tc)
	}
	var args struct{ A, B int }
	if err := json.Unmarshal(tc.Arguments, &args); err != nil || args.A != 1 || args.B != 2 {
		t.Fatalf("tool args: %s err=%v", string(tc.Arguments), err)
	}
	if done == nil || done.StopReason != loop.StopReasonToolUse {
		t.Fatalf("StopReason: got %+v", done)
	}
}

func TestStreamPropagatesHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"bad key"}`)
	}))
	t.Cleanup(server.Close)

	provider := New(Options{APIKey: "x", BaseURL: server.URL, HTTPClient: server.Client()})
	_, err := provider.Stream(context.Background(), loop.ProviderRequest{Model: "x"})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error should mention status: %v", err)
	}
}

func TestStreamMissingModel(t *testing.T) {
	provider := New(Options{APIKey: "x"})
	_, err := provider.Stream(context.Background(), loop.ProviderRequest{})
	if err == nil || !strings.Contains(err.Error(), "model") {
		t.Fatalf("expected model error, got %v", err)
	}
}

func TestStreamMissingAPIKey(t *testing.T) {
	t.Setenv("NVIDIA_API_KEY", "")
	provider := New(Options{})
	_, err := provider.Stream(context.Background(), loop.ProviderRequest{Model: "x"})
	if err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("expected API key error, got %v", err)
	}
}

func TestRequestBodyIncludesMessagesAndTools(t *testing.T) {
	server := newFakeServer(t, "data: [DONE]\n\n")
	provider := newProvider(t, server.Server)
	ch, err := provider.Stream(context.Background(), loop.ProviderRequest{
		Model:        "x",
		SystemPrompt: "be concise",
		Messages: []loop.Message{
			{Role: loop.MessageRoleUser, Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "hi"}}},
		},
		Tools: []loop.ToolSpec{{
			Name:        "add",
			Description: "add two ints",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"}}}`),
		}},
		Options: map[string]any{"temperature": 0.4, "max_tokens": 16},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	captureProviderEvents(t, ch)

	var got map[string]any
	if err := json.Unmarshal(server.lastBody, &got); err != nil {
		t.Fatalf("decode body: %v\n%s", err, string(server.lastBody))
	}
	if got["model"] != "x" {
		t.Fatalf("model: %v", got["model"])
	}
	if got["stream"] != true {
		t.Fatalf("stream flag missing: %v", got["stream"])
	}
	messages, _ := got["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("expected system + user message, got %d: %v", len(messages), messages)
	}
	system, _ := messages[0].(map[string]any)
	if system["role"] != "system" || system["content"] != "be concise" {
		t.Fatalf("system message: %+v", system)
	}
	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools: %+v", tools)
	}
	tool0, _ := tools[0].(map[string]any)
	fn, _ := tool0["function"].(map[string]any)
	if fn["name"] != "add" {
		t.Fatalf("tool name: %+v", fn)
	}
	if got["temperature"].(float64) != 0.4 {
		t.Fatalf("temperature: %+v", got["temperature"])
	}
	if got["max_tokens"].(float64) != 16 {
		t.Fatalf("max_tokens: %+v", got["max_tokens"])
	}
}

func TestStreamCancelStopsCleanly(t *testing.T) {
	// Server that sends one chunk, then blocks until the request context is
	// canceled. Verifies the goroutine exits without leaking when the caller
	// cancels mid-stream.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `data: {"id":"a","model":"x","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`+"\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	provider := New(Options{APIKey: "x", BaseURL: server.URL, HTTPClient: server.Client()})
	ch, err := provider.Stream(ctx, loop.ProviderRequest{Model: "x"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	// Drain the first text delta then cancel.
	var sawText bool
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				if !sawText {
					t.Fatalf("channel closed before any event")
				}
				return
			}
			if e.Type == loop.ProviderEventTextDelta {
				sawText = true
				cancel()
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("provider did not close channel after cancel")
		}
	}
}

// TestLiveSmoke runs against the real NVIDIA endpoint when NVIDIA_API_KEY is
// set. It calls moonshotai/kimi-k2.6 with a single non-streaming-style prompt
// and asserts that we get any text back. Skipped quietly otherwise.
func TestLiveSmoke(t *testing.T) {
	apiKey := os.Getenv("NVIDIA_API_KEY")
	if apiKey == "" {
		t.Skip("NVIDIA_API_KEY not set; skipping live smoke")
	}

	model := os.Getenv("NVIDIA_LIVE_MODEL")
	if model == "" {
		model = "moonshotai/kimi-k2.6"
	}

	// Cold-start latency on kimi-k2.6 can be ~30s.
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	provider := New(Options{APIKey: apiKey, HTTPClient: &http.Client{Timeout: 240 * time.Second}})
	ch, err := provider.Stream(ctx, loop.ProviderRequest{
		Model: model,
		Messages: []loop.Message{
			{Role: loop.MessageRoleUser, Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "Reply with the single word: glue."}}},
		},
		Options: map[string]any{"max_tokens": 16, "temperature": 0.0},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var text strings.Builder
	var done *loop.Message
	deadline := time.After(240 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("live smoke timed out; partial=%q", text.String())
		case event, ok := <-ch:
			if !ok {
				if done == nil {
					t.Fatalf("channel closed without Done event; partial=%q", text.String())
				}
				if strings.TrimSpace(text.String()) == "" {
					t.Fatalf("expected non-empty assistant text; got %q", text.String())
				}
				t.Logf("%s reply: %q (usage=%+v)", model, text.String(), done.Usage)
				return
			}
			switch event.Type {
			case loop.ProviderEventTextDelta:
				text.WriteString(event.Delta)
			case loop.ProviderEventDone:
				done = event.Message
			case loop.ProviderEventError:
				t.Fatalf("provider error: %s", event.Error)
			}
		}
	}
}

// sanity: make sure the default base URL is exposed for reference docs.
func TestDefaultBaseURL(t *testing.T) {
	if defaultBaseURL != "https://integrate.api.nvidia.com/v1" {
		t.Fatalf("defaultBaseURL drift: %s", defaultBaseURL)
	}
}

// sanity: tool message round-trips through convert layer without panicking.
func TestConvertToolMessageRequiresID(t *testing.T) {
	_, err := convertMessage(loop.Message{Role: loop.MessageRoleTool, Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "out"}}})
	if err == nil {
		t.Fatalf("expected error for missing tool_call_id")
	}
	got, err := convertMessage(loop.Message{
		Role:       loop.MessageRoleTool,
		ToolCallID: "id-1",
		ToolName:   "add",
		Content:    []loop.ContentPart{{Type: loop.ContentTypeText, Text: "42"}},
	})
	if err != nil {
		t.Fatalf("convert tool message: %v", err)
	}
	if got.Role != "tool" || got.ToolCallID != "id-1" || got.Content != "42" {
		t.Fatalf("converted tool message: %+v", got)
	}
}

// guard against accidental change to mapFinishReason.
func TestMapFinishReason(t *testing.T) {
	cases := map[string]loop.StopReason{
		"":               loop.StopReasonStop,
		"stop":           loop.StopReasonStop,
		"length":         loop.StopReasonLength,
		"tool_calls":     loop.StopReasonToolUse,
		"function_call":  loop.StopReasonToolUse,
		"content_filter": loop.StopReasonError,
	}
	for input, want := range cases {
		if got := mapFinishReason(input); got != want {
			t.Fatalf("mapFinishReason(%q) = %q, want %q", input, got, want)
		}
	}
}

