package openaicompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/erain/glue/loop"
)

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

type fakeServer struct {
	*httptest.Server
	lastBody    []byte
	lastHeaders http.Header
}

func newFakeServer(t *testing.T, body string) *fakeServer {
	t.Helper()
	fs := &fakeServer{}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		fs.lastBody = raw
		fs.lastHeaders = r.Header.Clone()
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
		Name:       "test",
		BaseURL:    server.URL,
		APIKey:     "test-key",
		HTTPClient: server.Client(),
	})
}

func TestStreamEmitsTextDeltasAndDone(t *testing.T) {
	body := strings.Join([]string{
		`data: {"id":"abc","model":"m","provider":"upstream","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}`,
		``,
		`data: {"id":"abc","model":"m","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}`,
		``,
		`data: {"id":"abc","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	server := newFakeServer(t, body)

	provider := newProvider(t, server.Server)
	ch, err := provider.Stream(context.Background(), loop.ProviderRequest{
		Model:    "m",
		Messages: []loop.Message{{Role: loop.MessageRoleUser, Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := captureProviderEvents(t, ch)

	var deltas []string
	var done *loop.Message
	for _, e := range events {
		switch e.Type {
		case loop.ProviderEventTextDelta:
			deltas = append(deltas, e.Delta)
		case loop.ProviderEventDone:
			done = e.Message
		case loop.ProviderEventError:
			t.Fatalf("unexpected error: %s", e.Error)
		}
	}
	if got := strings.Join(deltas, ""); got != "Hello world" {
		t.Fatalf("text concat: got %q", got)
	}
	if done == nil {
		t.Fatalf("missing Done event")
	}
	if done.StopReason != loop.StopReasonStop {
		t.Fatalf("StopReason: %q", done.StopReason)
	}
	if done.Provider != "test" {
		t.Fatalf("Provider name not propagated: %q", done.Provider)
	}
	if done.Usage == nil || done.Usage.InputTokens != 3 || done.Usage.OutputTokens != 2 {
		t.Fatalf("Usage: %+v", done.Usage)
	}
	if done.Metadata["upstream_provider"] != "upstream" {
		t.Fatalf("upstream_provider: %+v", done.Metadata)
	}
}

func TestStreamMapsBothReasoningFields(t *testing.T) {
	// First chunk uses delta.reasoning (OpenRouter); second uses
	// delta.reasoning_content (NVIDIA). Both should append to the same
	// thinking part on the assistant message.
	body := strings.Join([]string{
		`data: {"id":"a","model":"m","choices":[{"index":0,"delta":{"reasoning":"alpha "},"finish_reason":null}]}`,
		``,
		`data: {"id":"a","model":"m","choices":[{"index":0,"delta":{"reasoning_content":"beta"},"finish_reason":null}]}`,
		``,
		`data: {"id":"a","model":"m","choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	server := newFakeServer(t, body)

	ch, err := newProvider(t, server.Server).Stream(context.Background(), loop.ProviderRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var thinking string
	for _, e := range captureProviderEvents(t, ch) {
		if e.Type == loop.ProviderEventThinkingDelta {
			thinking += e.Delta
		}
	}
	if thinking != "alpha beta" {
		t.Fatalf("thinking concat: got %q want %q", thinking, "alpha beta")
	}
}

func TestStreamFiltersCommentLines(t *testing.T) {
	body := strings.Join([]string{
		`: keep-alive`,
		``,
		`: another`,
		``,
		`data: {"id":"a","model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`,
		``,
		`data: {"id":"a","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	server := newFakeServer(t, body)
	ch, err := newProvider(t, server.Server).Stream(context.Background(), loop.ProviderRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for _, e := range captureProviderEvents(t, ch) {
		if e.Type == loop.ProviderEventError {
			t.Fatalf("comment line raised error: %s", e.Error)
		}
	}
}

func TestStreamAccumulatesToolCalls(t *testing.T) {
	body := strings.Join([]string{
		`data: {"id":"a","model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		``,
		`data: {"id":"a","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"add"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"a","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"a","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1,\"b\":2}"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"a","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	server := newFakeServer(t, body)
	ch, err := newProvider(t, server.Server).Stream(context.Background(), loop.ProviderRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var toolCalls []*loop.ToolCall
	var done *loop.Message
	for _, e := range captureProviderEvents(t, ch) {
		if e.Type == loop.ProviderEventToolCall {
			toolCalls = append(toolCalls, e.ToolCall)
		}
		if e.Type == loop.ProviderEventDone {
			done = e.Message
		}
	}
	if len(toolCalls) != 1 {
		t.Fatalf("tool calls: got %d", len(toolCalls))
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
		t.Fatalf("StopReason: %+v", done)
	}
}

func TestStreamPropagatesHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"bad key"}`)
	}))
	t.Cleanup(server.Close)
	provider := New(Options{Name: "test", BaseURL: server.URL, APIKey: "x", HTTPClient: server.Client()})
	_, err := provider.Stream(context.Background(), loop.ProviderRequest{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 error, got %v", err)
	}
	if !strings.Contains(err.Error(), "test:") {
		t.Fatalf("error should be prefixed with provider name, got %v", err)
	}
}

func TestStreamMissingModel(t *testing.T) {
	provider := New(Options{Name: "test", BaseURL: "http://localhost", APIKey: "x"})
	_, err := provider.Stream(context.Background(), loop.ProviderRequest{})
	if err == nil || !strings.Contains(err.Error(), "model") {
		t.Fatalf("expected model error, got %v", err)
	}
}

func TestStreamMissingAPIKeyConsultsEnv(t *testing.T) {
	t.Setenv("MY_FAKE_KEY_ENV", "")
	provider := New(Options{Name: "test", BaseURL: "http://localhost", APIKeyEnv: "MY_FAKE_KEY_ENV"})
	_, err := provider.Stream(context.Background(), loop.ProviderRequest{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("expected API key error, got %v", err)
	}
	if !strings.Contains(err.Error(), "MY_FAKE_KEY_ENV") {
		t.Fatalf("error should mention configured env name, got %v", err)
	}
}

func TestStreamAPIKeyEnvFallback(t *testing.T) {
	server := newFakeServer(t, "data: [DONE]\n\n")
	t.Setenv("MY_FAKE_KEY_ENV", "from-env")
	provider := New(Options{Name: "test", BaseURL: server.URL, APIKeyEnv: "MY_FAKE_KEY_ENV", HTTPClient: server.Client()})
	ch, err := provider.Stream(context.Background(), loop.ProviderRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	captureProviderEvents(t, ch)
	if got := server.lastHeaders.Get("Authorization"); got != "Bearer from-env" {
		t.Fatalf("env-fallback auth: got %q", got)
	}
}

func TestRequestBodyIncludesMessagesAndTools(t *testing.T) {
	server := newFakeServer(t, "data: [DONE]\n\n")
	provider := newProvider(t, server.Server)
	ch, err := provider.Stream(context.Background(), loop.ProviderRequest{
		Model:        "m",
		SystemPrompt: "be concise",
		Messages: []loop.Message{
			{Role: loop.MessageRoleUser, Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "hi"}}},
		},
		Tools: []loop.ToolSpec{{
			Name:        "add",
			Description: "add",
			Parameters:  json.RawMessage(`{"type":"object"}`),
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
	if got["model"] != "m" || got["stream"] != true {
		t.Fatalf("model/stream: %+v", got)
	}
	if got["temperature"].(float64) != 0.4 || got["max_tokens"].(float64) != 16 {
		t.Fatalf("options: %+v", got)
	}
	messages, _ := got["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages: %+v", messages)
	}
	system, _ := messages[0].(map[string]any)
	if system["role"] != "system" || system["content"] != "be concise" {
		t.Fatalf("system: %+v", system)
	}
	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools: %+v", tools)
	}
}

func TestRequestForwardsConfiguredHeaders(t *testing.T) {
	server := newFakeServer(t, "data: [DONE]\n\n")
	provider := New(Options{
		Name:       "test",
		BaseURL:    server.URL,
		APIKey:     "k",
		HTTPClient: server.Client(),
		Headers:    map[string]string{"HTTP-Referer": "https://x.test", "X-Title": "x"},
	})
	ch, err := provider.Stream(context.Background(), loop.ProviderRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	captureProviderEvents(t, ch)
	if got := server.lastHeaders.Get("HTTP-Referer"); got != "https://x.test" {
		t.Fatalf("HTTP-Referer: %q", got)
	}
	if got := server.lastHeaders.Get("X-Title"); got != "x" {
		t.Fatalf("X-Title: %q", got)
	}
}

func TestStreamCancelStopsCleanly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `data: {"id":"a","model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`+"\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	provider := New(Options{Name: "test", BaseURL: server.URL, APIKey: "k", HTTPClient: server.Client()})
	ch, err := provider.Stream(ctx, loop.ProviderRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
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
