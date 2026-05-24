package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/agents/peggy"
	"github.com/erain/glue/daemon"
)

func TestResolveDaemonClientConfigUsesMetadataAndOverrides(t *testing.T) {
	metadataPath := filepath.Join(t.TempDir(), "daemon.json")
	if err := daemon.WriteMetadata(metadataPath, daemon.Metadata{
		Version: 1,
		BaseURL: "http://metadata",
		Token:   "meta-token",
		PID:     123,
	}); err != nil {
		t.Fatal(err)
	}

	cfg, err := ResolveDaemonClientConfig(DaemonClientConfig{MetadataPath: metadataPath})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "http://metadata" || cfg.Token != "meta-token" {
		t.Fatalf("metadata config = %+v", cfg)
	}

	cfg, err = ResolveDaemonClientConfig(DaemonClientConfig{
		MetadataPath: metadataPath,
		BaseURL:      "http://override/",
		Token:        "override-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "http://override" || cfg.Token != "override-token" {
		t.Fatalf("override config = %+v", cfg)
	}

	t.Setenv("GLUE_DAEMON_TOKEN", "env-token")
	cfg, err = ResolveDaemonClientConfig(DaemonClientConfig{
		MetadataPath: filepath.Join(t.TempDir(), "missing.json"),
		BaseURL:      "http://explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "http://explicit" || cfg.Token != "env-token" {
		t.Fatalf("env fallback config = %+v", cfg)
	}
}

func TestDaemonClientMessageStartsRunAndSendsText(t *testing.T) {
	var start daemonStartRunPayload
	var startPath string
	daemonServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sessions/telegram:123/runs":
			startPath = r.URL.Path
			if auth := r.Header.Get("Authorization"); auth != "Bearer tok" {
				t.Fatalf("auth = %q", auth)
			}
			if err := json.NewDecoder(r.Body).Decode(&start); err != nil {
				t.Fatal(err)
			}
			writeJSON(t, w, http.StatusCreated, daemonStartRunResponse{RunID: "run_1", EventsURL: "/v1/runs/run_1/events"})
		case "/v1/runs/run_1/events":
			writeSSE(t, w, daemon.EventEnvelope{Type: "text_delta", Payload: map[string]any{"delta": "hello from daemon"}})
			writeSSE(t, w, daemon.EventEnvelope{Type: "run_done"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer daemonServer.Close()

	tg := newTelegramFixture(t, nil)
	dc, err := NewDaemonClient(DaemonClientConfig{BaseURL: daemonServer.URL, Token: "tok"}, daemonServer.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := New(Options{
		Daemon: dc,
		Config: Config{APIBaseURL: tg.server.URL, AllowChats: []int64{123}},
		Token:  "telegram-token",
	})
	if err != nil {
		t.Fatal(err)
	}

	ch.handleUpdate(context.Background(), messageUpdate(1, 123, "hello"))
	if startPath != "/v1/sessions/telegram:123/runs" {
		t.Fatalf("start path = %q", startPath)
	}
	if start.Text != "hello" || start.ClientID != "telegram:123" {
		t.Fatalf("start payload = %+v", start)
	}
	if got := tg.lastSendText(); got != "hello from daemon" {
		t.Fatalf("telegram reply = %q", got)
	}
}

func TestDaemonClientAllowlistBeforeDaemonRequest(t *testing.T) {
	var starts int
	daemonServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		starts++
	}))
	defer daemonServer.Close()
	tg := newTelegramFixture(t, nil)
	dc, err := NewDaemonClient(DaemonClientConfig{BaseURL: daemonServer.URL, Token: "tok"}, daemonServer.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := New(Options{
		Daemon: dc,
		Config: Config{APIBaseURL: tg.server.URL, AllowChats: []int64{999}},
		Token:  "telegram-token",
	})
	if err != nil {
		t.Fatal(err)
	}

	ch.handleUpdate(context.Background(), messageUpdate(1, 123, "hello"))
	if starts != 0 {
		t.Fatalf("daemon starts = %d, want 0", starts)
	}
	if tg.sendCount() != 0 {
		t.Fatalf("telegram sends = %d, want 0", tg.sendCount())
	}
}

func TestDaemonClientPermissionCallbackPostsDecision(t *testing.T) {
	var (
		mu             sync.Mutex
		decisionBody   daemonPermissionDecision
		decisionClient string
		decisionCh     = make(chan struct{})
	)
	daemonServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sessions/telegram:123/runs":
			writeJSON(t, w, http.StatusCreated, daemonStartRunResponse{RunID: "run_1", EventsURL: "/v1/runs/run_1/events"})
		case "/v1/runs/run_1/events":
			writeSSE(t, w, daemon.EventEnvelope{
				Type: "permission_request",
				Payload: daemonPermissionPayload{
					PermissionID: "perm_1",
					Request: glue.PermissionRequest{
						SessionID: peggy.ChannelSessionID(ChannelName, "123"),
						Tool:      "write_file",
						Action:    "write_file",
						Target:    "note.txt",
					},
				},
			})
			select {
			case <-decisionCh:
			case <-time.After(time.Second):
				t.Error("timed out waiting for decision")
			}
			writeSSE(t, w, daemon.EventEnvelope{Type: "text_delta", Payload: map[string]any{"delta": "done"}})
			writeSSE(t, w, daemon.EventEnvelope{Type: "run_done"})
		case "/v1/runs/run_1/permissions/perm_1/decision":
			mu.Lock()
			defer mu.Unlock()
			decisionClient = r.Header.Get("X-Glue-Client-ID")
			if err := json.NewDecoder(r.Body).Decode(&decisionBody); err != nil {
				t.Fatal(err)
			}
			close(decisionCh)
			writeJSON(t, w, http.StatusOK, map[string]any{"accepted": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer daemonServer.Close()

	tg := newTelegramFixture(t, nil)
	dc, err := NewDaemonClient(DaemonClientConfig{BaseURL: daemonServer.URL, Token: "tok"}, daemonServer.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := New(Options{
		Daemon: dc,
		Config: Config{APIBaseURL: tg.server.URL, AllowChats: []int64{123}},
		Token:  "telegram-token",
	})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		ch.handleUpdate(context.Background(), messageUpdate(1, 123, "write file"))
		close(done)
	}()
	waitForSendCount(t, tg, 1)
	ch.handleCallback(context.Background(), CallbackQuery{
		ID:      "cb_1",
		Message: &Message{Chat: Chat{ID: 123, Type: "private"}},
		Data:    permissionCallback("1", "session"),
	})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handleUpdate did not finish")
	}

	mu.Lock()
	defer mu.Unlock()
	if decisionClient != "telegram:123" {
		t.Fatalf("decision client = %q", decisionClient)
	}
	if !decisionBody.Allow || decisionBody.RememberFor != "session" {
		t.Fatalf("decision = %+v", decisionBody)
	}
	if got := tg.lastSendText(); got != "done" {
		t.Fatalf("final reply = %q", got)
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatal(err)
	}
}

func writeSSE(t *testing.T, w http.ResponseWriter, event daemon.EventEnvelope) {
	t.Helper()
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func waitForSendCount(t *testing.T, tg *telegramFixture, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if tg.sendCount() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	var bodies []string
	tg.mu.Lock()
	for _, body := range tg.sendBodies {
		bodies = append(bodies, string(body))
	}
	tg.mu.Unlock()
	t.Fatalf("send count = %d, want %d; bodies=%s", tg.sendCount(), want, strings.Join(bodies, "\n"))
}
