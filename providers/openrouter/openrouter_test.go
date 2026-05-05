package openrouter

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
		APIKey:     "test-key",
		BaseURL:    server.URL,
		HTTPClient: server.Client(),
	})
}

func TestStreamEmitsTextDeltasAndDone(t *testing.T) {
	body := strings.Join([]string{
		`: OPENROUTER PROCESSING`,
		``,
		`data: {"id":"abc","model":"openrouter/free","provider":"OpenAI","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}`,
		``,
		`data: {"id":"abc","model":"openrouter/free","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}`,
		``,
		`data: {"id":"abc","model":"openrouter/free","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	server := newFakeServer(t, body)

	provider := newProvider(t, server.Server)
	ch, err := provider.Stream(context.Background(), loop.ProviderRequest{
		Model:    "openrouter/free",
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
			t.Fatalf("unexpected error event: %s", e.Error)
		}
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
	if done.Usage == nil || done.Usage.InputTokens != 3 || done.Usage.OutputTokens != 2 {
		t.Fatalf("Usage: got %+v", done.Usage)
	}
	if done.Metadata["upstream_provider"] != "OpenAI" {
		t.Fatalf("upstream_provider metadata: %+v", done.Metadata)
	}
}

func TestStreamMapsReasoningDelta(t *testing.T) {
	body := strings.Join([]string{
		`data: {"id":"a","model":"x","choices":[{"index":0,"delta":{"reasoning":"thinking..."},"finish_reason":null}]}`,
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

func TestStreamFiltersCommentLines(t *testing.T) {
	// Three comment lines interleaved with two real chunks. The provider
	// should treat every ":..." line as a no-op keep-alive.
	body := strings.Join([]string{
		`: OPENROUTER PROCESSING`,
		``,
		`: keep-alive`,
		``,
		`data: {"id":"a","model":"x","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`,
		``,
		`: another comment`,
		``,
		`data: {"id":"a","model":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
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

	for _, e := range events {
		if e.Type == loop.ProviderEventError {
			t.Fatalf("comment line raised error: %s", e.Error)
		}
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
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 error, got %v", err)
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
	t.Setenv("OPENROUTER_API_KEY", "")
	provider := New(Options{})
	_, err := provider.Stream(context.Background(), loop.ProviderRequest{Model: "x"})
	if err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("expected API key error, got %v", err)
	}
}

func TestRequestSendsAttributionHeaders(t *testing.T) {
	server := newFakeServer(t, "data: [DONE]\n\n")
	provider := newProvider(t, server.Server)
	ch, err := provider.Stream(context.Background(), loop.ProviderRequest{Model: "x"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	captureProviderEvents(t, ch)

	if got := server.lastHeaders.Get("HTTP-Referer"); got != defaultRefererURL {
		t.Fatalf("HTTP-Referer: got %q want %q", got, defaultRefererURL)
	}
	if got := server.lastHeaders.Get("X-Title"); got != defaultTitle {
		t.Fatalf("X-Title: got %q want %q", got, defaultTitle)
	}
	if got := server.lastHeaders.Get("Authorization"); got != "Bearer test-key" {
		t.Fatalf("Authorization: got %q", got)
	}
}

func TestUserHeadersOverrideDefaults(t *testing.T) {
	server := newFakeServer(t, "data: [DONE]\n\n")
	provider := New(Options{
		APIKey:     "test-key",
		BaseURL:    server.URL,
		HTTPClient: server.Client(),
		Headers:    map[string]string{"X-Title": "my-app"},
	})
	ch, err := provider.Stream(context.Background(), loop.ProviderRequest{Model: "x"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	captureProviderEvents(t, ch)
	if got := server.lastHeaders.Get("X-Title"); got != "my-app" {
		t.Fatalf("X-Title override: got %q", got)
	}
	if got := server.lastHeaders.Get("HTTP-Referer"); got != defaultRefererURL {
		t.Fatalf("HTTP-Referer should keep default: got %q", got)
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
	if got["model"] != "x" || got["stream"] != true {
		t.Fatalf("model/stream: %+v", got)
	}
	messages, _ := got["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("expected system + user message, got %d", len(messages))
	}
	system, _ := messages[0].(map[string]any)
	if system["role"] != "system" || system["content"] != "be concise" {
		t.Fatalf("system message: %+v", system)
	}
	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools: %+v", tools)
	}
}

func TestStreamCancelStopsCleanly(t *testing.T) {
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

func TestDefaultBaseURL(t *testing.T) {
	if defaultBaseURL != "https://openrouter.ai/api/v1" {
		t.Fatalf("defaultBaseURL drift: %s", defaultBaseURL)
	}
}

func TestMapFinishReason(t *testing.T) {
	cases := map[string]loop.StopReason{
		"":               loop.StopReasonStop,
		"stop":           loop.StopReasonStop,
		"length":         loop.StopReasonLength,
		"tool_calls":     loop.StopReasonToolUse,
		"content_filter": loop.StopReasonError,
	}
	for input, want := range cases {
		if got := mapFinishReason(input); got != want {
			t.Fatalf("mapFinishReason(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestLiveSmoke runs against the real OpenRouter endpoint when
// OPENROUTER_API_KEY is set. Defaults to the openrouter/free meta-route
// so the test doesn't burn paid tokens. Skipped quietly otherwise.
func TestLiveSmoke(t *testing.T) {
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		t.Skip("OPENROUTER_API_KEY not set; skipping live smoke")
	}
	model := os.Getenv("OPENROUTER_LIVE_MODEL")
	if model == "" {
		model = "openrouter/free"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
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
				upstream, _ := done.Metadata["upstream_provider"].(string)
				t.Logf("%s reply: %q (upstream=%s usage=%+v)", model, text.String(), upstream, done.Usage)
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
