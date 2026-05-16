package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erain/glue/loop"
	"github.com/erain/glue/providers"
	"github.com/erain/glue/providers/codex/auth"
)

// makeJWT builds an unsigned JWT with the given claims. Mirror of the
// helper in providers/codex/auth/tokens_test.go.
func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding.EncodeToString
	return enc(header) + "." + enc(payload) + "."
}

// writeAuthFile drops a minimal auth.json into dir with a fresh access
// token and the given refresh token. Returns the file path.
func writeAuthFile(t *testing.T, dir, refresh string) string {
	t.Helper()
	id := makeJWT(t, map[string]any{"chatgpt_account_id": "acct-7"})
	access := makeJWT(t, map[string]any{"exp": time.Now().Add(24 * time.Hour).Unix()})
	body := map[string]any{
		"tokens": map[string]any{
			"id_token":      id,
			"access_token":  access,
			"refresh_token": refresh,
			"account_id":    "acct-7",
		},
		"last_refresh": time.Now().UTC().Format(time.RFC3339),
	}
	raw, _ := json.MarshalIndent(body, "", "  ")
	path := filepath.Join(dir, "auth.json")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
	return path
}

// canonicalSSE is a representative SSE stream covering the four events
// glue cares about. Each block ends with a blank line.
func canonicalSSE() string {
	return "" +
		"event: response.created\n" +
		"data: {\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5-codex\"}}\n\n" +
		"event: response.output_text.delta\n" +
		"data: {\"delta\":\"Hello\"}\n\n" +
		"event: response.output_text.delta\n" +
		"data: {\"delta\":\" world\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5-codex\",\"usage\":{\"input_tokens\":12,\"output_tokens\":3,\"total_tokens\":15}}}\n\n"
}

func TestStream_HappyPath_HeadersAndEvents(t *testing.T) {
	authDir := t.TempDir()
	authPath := writeAuthFile(t, authDir, "rtk")

	var captured *http.Request
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Clone(r.Context())
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, canonicalSSE())
	}))
	defer srv.Close()

	p := New(Options{BaseURL: srv.URL, AuthFile: authPath})
	ev, err := p.Stream(context.Background(), loop.ProviderRequest{
		Model:        "gpt-5-codex",
		SystemPrompt: "be helpful",
		Messages: []loop.Message{{
			Role:    loop.MessageRoleUser,
			Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "Hi"}},
		}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := collectEvents(t, ev)

	// Event sequence: Start, TextDelta, TextDelta, Done.
	if len(events) != 4 {
		t.Fatalf("expected 4 events, got %d: %+v", len(events), events)
	}
	if events[0].Type != loop.ProviderEventStart {
		t.Errorf("first event = %s", events[0].Type)
	}
	if events[1].Type != loop.ProviderEventTextDelta || events[1].Delta != "Hello" {
		t.Errorf("second event = %+v", events[1])
	}
	if events[2].Type != loop.ProviderEventTextDelta || events[2].Delta != " world" {
		t.Errorf("third event = %+v", events[2])
	}
	if events[3].Type != loop.ProviderEventDone {
		t.Fatalf("final event = %s", events[3].Type)
	}
	final := events[3].Message
	if final == nil {
		t.Fatal("Done event missing message")
	}
	if final.Provider != "codex" {
		t.Errorf("provider = %s", final.Provider)
	}
	if final.Model != "gpt-5-codex" {
		t.Errorf("model = %s", final.Model)
	}
	if final.Usage == nil || final.Usage.InputTokens != 12 || final.Usage.OutputTokens != 3 || final.Usage.TotalTokens != 15 {
		t.Errorf("usage = %+v", final.Usage)
	}
	if got, _ := final.Metadata["response_id"].(string); got != "resp_1" {
		t.Errorf("response_id = %q", got)
	}
	if final.StopReason != loop.StopReasonStop {
		t.Errorf("stop_reason = %s", final.StopReason)
	}
	if len(final.Content) != 1 || final.Content[0].Text != "Hello world" {
		t.Errorf("content = %+v", final.Content)
	}

	// Headers required by ADR-0006 §3.
	if captured == nil {
		t.Fatal("no request captured")
	}
	want := map[string]string{
		"Accept":             "text/event-stream",
		"Content-Type":       "application/json",
		"Openai-Beta":        "responses=experimental",
		"Originator":         "codex_cli_rs",
		"Chatgpt-Account-Id": "acct-7",
	}
	for h, v := range want {
		if got := captured.Header.Get(h); got != v {
			t.Errorf("header %s = %q, want %q", h, got, v)
		}
	}
	if got := captured.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
		t.Errorf("Authorization not Bearer: %q", got)
	}
	for _, h := range []string{"User-Agent", "version", "session_id", "conversation_id"} {
		if captured.Header.Get(h) == "" {
			t.Errorf("missing header %s", h)
		}
	}

	// Request body sanity.
	var sent map[string]any
	if err := json.Unmarshal(capturedBody, &sent); err != nil {
		t.Fatalf("body json: %v", err)
	}
	if sent["model"] != "gpt-5-codex" {
		t.Errorf("model in body = %v", sent["model"])
	}
	if sent["instructions"] != "be helpful" {
		t.Errorf("instructions in body = %v", sent["instructions"])
	}
	if sent["stream"] != true {
		t.Errorf("stream should be true")
	}
	if sent["store"] != false {
		t.Errorf("store should be false")
	}
}

func TestStream_ToolCallRoundTrip(t *testing.T) {
	authPath := writeAuthFile(t, t.TempDir(), "rtk")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ""+
			"event: response.created\ndata: {\"response\":{\"id\":\"r2\"}}\n\n"+
			"event: response.output_item.done\n"+
			"data: {\"item\":{\"type\":\"function_call\",\"call_id\":\"call_X\",\"name\":\"weather\",\"arguments\":{\"q\":\"NYC\"}}}\n\n"+
			"event: response.completed\ndata: {\"response\":{\"id\":\"r2\"}}\n\n")
	}))
	defer srv.Close()

	p := New(Options{BaseURL: srv.URL, AuthFile: authPath})
	ev, err := p.Stream(context.Background(), loop.ProviderRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := collectEvents(t, ev)

	var sawCall *loop.ToolCall
	var final *loop.Message
	for _, e := range events {
		if e.Type == loop.ProviderEventToolCall {
			sawCall = e.ToolCall
		}
		if e.Type == loop.ProviderEventDone {
			final = e.Message
		}
	}
	if sawCall == nil {
		t.Fatal("missing tool_call event")
	}
	if sawCall.ID != "call_X" || sawCall.Name != "weather" {
		t.Errorf("tool call: %+v", sawCall)
	}
	if !strings.Contains(string(sawCall.Arguments), "NYC") {
		t.Errorf("args: %s", sawCall.Arguments)
	}
	if final == nil || final.StopReason != loop.StopReasonToolUse {
		t.Errorf("stop_reason should be tool_use, got %+v", final)
	}
}

func TestStream_StreamClosedBeforeCompleted(t *testing.T) {
	authPath := writeAuthFile(t, t.TempDir(), "rtk")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// No completed event; just text + close.
		_, _ = io.WriteString(w, "event: response.created\ndata: {}\n\nevent: response.output_text.delta\ndata: {\"delta\":\"x\"}\n\n")
	}))
	defer srv.Close()

	p := New(Options{BaseURL: srv.URL, AuthFile: authPath})
	ev, err := p.Stream(context.Background(), loop.ProviderRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := collectEvents(t, ev)
	var sawErr bool
	for _, e := range events {
		if e.Type == loop.ProviderEventError && strings.Contains(e.Error, "stream closed before response.completed") {
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatalf("expected stream-closed error, got %+v", events)
	}
}

func TestStream_FailedEvent(t *testing.T) {
	authPath := writeAuthFile(t, t.TempDir(), "rtk")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ""+
			"event: response.created\ndata: {}\n\n"+
			"event: response.failed\ndata: {\"response\":{\"error\":{\"message\":\"upstream boom\"}}}\n\n")
	}))
	defer srv.Close()

	p := New(Options{BaseURL: srv.URL, AuthFile: authPath})
	ev, _ := p.Stream(context.Background(), loop.ProviderRequest{Model: "m"})
	events := collectEvents(t, ev)
	if events[len(events)-1].Type != loop.ProviderEventError {
		t.Fatalf("expected error as last event, got %+v", events)
	}
	if !strings.Contains(events[len(events)-1].Error, "upstream boom") {
		t.Errorf("error message: %q", events[len(events)-1].Error)
	}
}

func TestStream_401RefreshAndRetry(t *testing.T) {
	authDir := t.TempDir()
	authPath := writeAuthFile(t, authDir, "rtk-original")

	// Refresh server returns rotated tokens.
	refreshHits := int32(0)
	refreshSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&refreshHits, 1)
		_ = json.NewEncoder(w).Encode(auth.RefreshResponse{AccessToken: makeJWT(t, map[string]any{"exp": time.Now().Add(time.Hour).Unix()})})
	}))
	defer refreshSrv.Close()

	// Responses server: 401 first, then 200 with valid completion.
	hits := int32(0)
	respSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":"expired"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, canonicalSSE())
	}))
	defer respSrv.Close()

	mgr := &auth.Manager{PathOverride: authPath, RefreshURLOverride: refreshSrv.URL}
	// Force the manager to think tokens are stale on next EnsureFresh so
	// refresh runs.
	mgr.Now = func() time.Time { return time.Now().Add(10 * 24 * time.Hour) } // > 8d cadence
	p := New(Options{BaseURL: respSrv.URL, Auth: mgr})

	ev, err := p.Stream(context.Background(), loop.ProviderRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := collectEvents(t, ev)
	if events[len(events)-1].Type != loop.ProviderEventDone {
		t.Fatalf("expected Done, got %+v", events[len(events)-1])
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Errorf("expected 2 responses hits (401 then 200), got %d", hits)
	}
	// At least one refresh call from the proactive cadence; the 401-driven
	// path also triggers one. Either way the refresh server is exercised.
	if atomic.LoadInt32(&refreshHits) < 1 {
		t.Errorf("expected at least one refresh call, got %d", refreshHits)
	}
}

func TestStream_Non2xxNon401Errors(t *testing.T) {
	authPath := writeAuthFile(t, t.TempDir(), "rtk")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":"boom"}`)
	}))
	defer srv.Close()

	p := New(Options{BaseURL: srv.URL, AuthFile: authPath})
	if _, err := p.Stream(context.Background(), loop.ProviderRequest{Model: "m"}); err == nil {
		t.Fatal("expected error for 5xx")
	}
}

func TestStream_NoAuthFileErrors(t *testing.T) {
	p := New(Options{BaseURL: "http://example", AuthFile: filepath.Join(t.TempDir(), "missing.json")})
	_, err := p.Stream(context.Background(), loop.ProviderRequest{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "codex: auth") {
		t.Fatalf("want codex: auth error, got %v", err)
	}
}

func TestStream_ContextCancel(t *testing.T) {
	authPath := writeAuthFile(t, t.TempDir(), "rtk")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Send a started event then block.
		_, _ = io.WriteString(w, "event: response.created\ndata: {}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	p := New(Options{BaseURL: srv.URL, AuthFile: authPath})
	ev, err := p.Stream(ctx, loop.ProviderRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	cancel()
	// Drain until the channel closes; it must close promptly after cancel.
	timeout := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ev:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("event channel did not close after ctx cancel")
		}
	}
}

func TestRegistryRegistersCodex(t *testing.T) {
	if _, ok := providers.Lookup("codex"); !ok {
		t.Fatal("codex not registered")
	}
	prov, def, env, err := providers.New("codex")
	if err != nil {
		t.Fatalf("providers.New: %v", err)
	}
	if prov == nil {
		t.Fatal("nil provider")
	}
	if def != DefaultModel {
		t.Errorf("default model = %q", def)
	}
	if env != "" {
		t.Errorf("codex should not advertise an env key, got %q", env)
	}
}

func TestNewUUIDV4Format(t *testing.T) {
	id := newUUID()
	if len(id) != 36 {
		t.Fatalf("uuid length = %d", len(id))
	}
	parts := strings.Split(id, "-")
	if len(parts) != 5 {
		t.Fatalf("uuid groups = %d", len(parts))
	}
	if len(parts[2]) != 4 || parts[2][0] != '4' {
		t.Errorf("version nibble: %s", parts[2])
	}
	switch parts[3][0] {
	case '8', '9', 'a', 'b':
	default:
		t.Errorf("variant nibble: %s", parts[3])
	}
}

func collectEvents(t *testing.T, ch <-chan loop.ProviderEvent) []loop.ProviderEvent {
	t.Helper()
	timeout := time.After(5 * time.Second)
	var out []loop.ProviderEvent
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-timeout:
			t.Fatalf("timeout draining events; got so far: %+v", out)
		}
	}
}

// TestLiveSmoke runs against the real Codex endpoint when
// GLUE_CODEX_LIVE=1 and a token file is available. Manual /
// workflow_dispatch only — never in default CI per ADR-0006 §7.
func TestLiveSmoke(t *testing.T) {
	if os.Getenv("GLUE_CODEX_LIVE") == "" {
		t.Skip("GLUE_CODEX_LIVE not set")
	}
	mgr := auth.NewManager()
	if _, err := mgr.LoadTokens(context.Background()); err != nil {
		t.Skipf("no codex auth.json: %v", err)
	}
	p := New(Options{Auth: mgr})
	ev, err := p.Stream(context.Background(), loop.ProviderRequest{
		Model: DefaultModel,
		Messages: []loop.Message{{
			Role:    loop.MessageRoleUser,
			Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "Reply with the single word: ok."}},
		}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := collectEvents(t, ev)
	last := events[len(events)-1]
	if last.Type != loop.ProviderEventDone || last.Message == nil {
		t.Fatalf("expected Done with message, got %+v", last)
	}
	if len(last.Message.Content) == 0 {
		t.Fatalf("empty content")
	}
	t.Logf("model=%s usage=%+v text=%q", last.Message.Model, last.Message.Usage, snippet(last.Message))
}

func snippet(m *loop.Message) string {
	for _, p := range m.Content {
		if p.Type == loop.ContentTypeText {
			s := p.Text
			if len(s) > 80 {
				s = s[:80] + "…"
			}
			return s
		}
	}
	return ""
}

var _ = fmt.Sprintf // keep fmt import when not otherwise used
