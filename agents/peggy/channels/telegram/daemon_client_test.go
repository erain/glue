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

func TestDaemonClientSkillCommandsUseDaemon(t *testing.T) {
	var (
		start      daemonStartRunPayload
		starts     int
		skillsSeen int
	)
	daemonServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer tok" {
			t.Errorf("auth = %q", auth)
		}
		switch r.URL.Path {
		case "/v1/skills":
			if r.Method != http.MethodGet {
				t.Errorf("skills method = %s", r.Method)
			}
			skillsSeen++
			writeJSON(t, w, http.StatusOK, daemonSkillCatalog{Skills: []daemon.SkillCatalogEntry{{
				Name:        "triage",
				Description: "Triage one issue",
			}}})
		case "/v1/sessions/telegram:123/runs":
			if r.Method != http.MethodPost {
				t.Errorf("skill method = %s", r.Method)
			}
			starts++
			if err := json.NewDecoder(r.Body).Decode(&start); err != nil {
				t.Fatal(err)
			}
			writeJSON(t, w, http.StatusCreated, daemonStartRunResponse{RunID: "run_1", EventsURL: "/v1/runs/run_1/events"})
		case "/v1/runs/run_1/events":
			writeSSE(t, w, daemon.EventEnvelope{Type: "text_delta", Payload: map[string]any{"delta": "skill done"}})
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

	ch.handleUpdate(context.Background(), messageUpdate(1, 123, "/skills@PeggyBot"))
	if skillsSeen != 1 {
		t.Fatalf("skills requests = %d, want 1", skillsSeen)
	}
	if starts != 0 {
		t.Fatalf("daemon starts after /skills = %d, want 0", starts)
	}
	if got := tg.lastSendText(); !strings.Contains(got, "triage") || !strings.Contains(got, "Triage one issue") {
		t.Fatalf("skills reply = %q", got)
	}

	ch.handleUpdate(context.Background(), messageUpdate(2, 123, "/skill triage issue=GLUE-123 priority=high"))
	if starts != 1 {
		t.Fatalf("daemon starts = %d, want 1", starts)
	}
	if start.Text != "" || start.Skill != "triage" || start.ClientID != "telegram:123" {
		t.Fatalf("skill start payload = %+v", start)
	}
	if start.Arguments["issue"] != "GLUE-123" || start.Arguments["priority"] != "high" {
		t.Fatalf("skill start arguments = %+v", start.Arguments)
	}
	if got := tg.lastSendText(); got != "skill done" {
		t.Fatalf("skill reply = %q", got)
	}
}

func TestDaemonClientSkillCommandValidationDoesNotCallDaemon(t *testing.T) {
	var requests int
	daemonServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
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

	ch.handleUpdate(context.Background(), messageUpdate(1, 123, "/skill"))
	if requests != 0 {
		t.Fatalf("daemon requests = %d, want 0", requests)
	}
	if got := tg.lastSendText(); !strings.Contains(got, "Command error: /skill name is required") {
		t.Fatalf("validation reply = %q", got)
	}

	ch.handleUpdate(context.Background(), messageUpdate(2, 123, "/skill triage issue"))
	if requests != 0 {
		t.Fatalf("daemon requests = %d, want 0", requests)
	}
	if got := tg.lastSendText(); !strings.Contains(got, "Command error: usage: /skill <name> [key=value ...]") {
		t.Fatalf("argument validation reply = %q", got)
	}
}

func TestDaemonClientRoleCommandsUseDaemon(t *testing.T) {
	var (
		start     daemonStartRunPayload
		starts    int
		rolesSeen int
	)
	daemonServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer tok" {
			t.Errorf("auth = %q", auth)
		}
		switch r.URL.Path {
		case "/v1/roles":
			if r.Method != http.MethodGet {
				t.Errorf("roles method = %s", r.Method)
			}
			rolesSeen++
			writeJSON(t, w, http.StatusOK, daemonRoleCatalog{Roles: []daemon.RoleCatalogEntry{{
				Name:        "reviewer",
				Description: "Review code changes",
				Model:       "openrouter/free",
			}}})
		case "/v1/sessions/telegram:123/runs":
			if r.Method != http.MethodPost {
				t.Errorf("role method = %s", r.Method)
			}
			starts++
			if err := json.NewDecoder(r.Body).Decode(&start); err != nil {
				t.Fatal(err)
			}
			writeJSON(t, w, http.StatusCreated, daemonStartRunResponse{RunID: "run_1", EventsURL: "/v1/runs/run_1/events"})
		case "/v1/runs/run_1/events":
			writeSSE(t, w, daemon.EventEnvelope{Type: "text_delta", Payload: map[string]any{"delta": "role done"}})
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

	ch.handleUpdate(context.Background(), messageUpdate(1, 123, "/roles@PeggyBot"))
	if rolesSeen != 1 {
		t.Fatalf("roles requests = %d, want 1", rolesSeen)
	}
	if starts != 0 {
		t.Fatalf("daemon starts after /roles = %d, want 0", starts)
	}
	if got := tg.lastSendText(); !strings.Contains(got, "reviewer") || !strings.Contains(got, "Review code changes") || !strings.Contains(got, "openrouter/free") {
		t.Fatalf("roles reply = %q", got)
	}

	ch.handleUpdate(context.Background(), messageUpdate(2, 123, "/role reviewer summarize the diff"))
	if starts != 1 {
		t.Fatalf("daemon starts = %d, want 1", starts)
	}
	if start.Text != "summarize the diff" || start.Role != "reviewer" || start.ClientID != "telegram:123" {
		t.Fatalf("role start payload = %+v", start)
	}
	if start.Skill != "" || len(start.Arguments) != 0 {
		t.Fatalf("unexpected skill fields in role start payload = %+v", start)
	}
	if got := tg.lastSendText(); got != "role done" {
		t.Fatalf("role reply = %q", got)
	}
}

func TestDaemonClientRoleCommandValidationDoesNotCallDaemon(t *testing.T) {
	var requests int
	daemonServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
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

	ch.handleUpdate(context.Background(), messageUpdate(1, 123, "/role"))
	if requests != 0 {
		t.Fatalf("daemon requests = %d, want 0", requests)
	}
	if got := tg.lastSendText(); !strings.Contains(got, "Command error: /role name is required") {
		t.Fatalf("validation reply = %q", got)
	}

	ch.handleUpdate(context.Background(), messageUpdate(2, 123, "/role reviewer"))
	if requests != 0 {
		t.Fatalf("daemon requests = %d, want 0", requests)
	}
	if got := tg.lastSendText(); !strings.Contains(got, "Command error: /role prompt is required") {
		t.Fatalf("prompt validation reply = %q", got)
	}
}

func TestDaemonClientMemoryCommandsUseDaemonWithoutRun(t *testing.T) {
	var (
		starts         int
		memoryLimit    string
		deletePath     string
		recallRequests []daemon.RecallRequest
	)
	daemonServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer tok" {
			t.Errorf("auth = %q", auth)
		}
		switch r.URL.Path {
		case "/v1/sessions/telegram:123/runs":
			starts++
			http.Error(w, "commands should not start runs", http.StatusInternalServerError)
		case "/v1/memories":
			if r.Method != http.MethodGet {
				t.Errorf("memories method = %s", r.Method)
			}
			memoryLimit = r.URL.Query().Get("limit")
			writeJSON(t, w, http.StatusOK, daemon.MemoryCatalogResponse{Memories: []daemon.MemoryEntry{{
				ID:        "mem_1",
				Content:   "Project launch is Friday.",
				Tags:      []string{"project"},
				Timestamp: time.Date(2026, 5, 25, 1, 2, 3, 0, time.UTC),
			}}})
		case "/v1/recall":
			if r.Method != http.MethodPost {
				t.Errorf("recall method = %s", r.Method)
			}
			var req daemon.RecallRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			recallRequests = append(recallRequests, req)
			writeJSON(t, w, http.StatusOK, daemon.RecallResponse{Hits: []daemon.RecallHit{{
				SessionID: "telegram:123",
				Index:     7,
				Role:      glue.MessageRoleAssistant,
				Snippet:   "The launch checklist is ready.",
				Score:     1.5,
			}}})
		case "/v1/memories/mem_1":
			if r.Method != http.MethodDelete {
				t.Errorf("forget method = %s", r.Method)
			}
			deletePath = r.URL.Path
			writeJSON(t, w, http.StatusOK, daemon.MemoryForgetResponse{Memory: daemon.MemoryEntry{
				ID:      "mem_1",
				Content: "Project launch is Friday.",
			}})
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

	ch.handleUpdate(context.Background(), messageUpdate(1, 123, "/memories@PeggyBot 2"))
	if memoryLimit != "2" {
		t.Fatalf("memory limit = %q, want 2", memoryLimit)
	}
	if got := tg.lastSendText(); !strings.Contains(got, "mem_1") || !strings.Contains(got, "Project launch is Friday") {
		t.Fatalf("memories reply = %q", got)
	}

	ch.handleUpdate(context.Background(), messageUpdate(2, 123, "/recall project launch"))
	if len(recallRequests) != 1 {
		t.Fatalf("recall requests = %d, want 1", len(recallRequests))
	}
	if recallRequests[0].Query != "project launch" || recallRequests[0].Limit != defaultTelegramRecallLimit || recallRequests[0].MemoriesOnly {
		t.Fatalf("recall request = %+v", recallRequests[0])
	}
	if got := tg.lastSendText(); !strings.Contains(got, "telegram:123#7") || !strings.Contains(got, "launch checklist") {
		t.Fatalf("recall reply = %q", got)
	}

	ch.handleUpdate(context.Background(), messageUpdate(3, 123, "/recall_memories launch"))
	if len(recallRequests) != 2 {
		t.Fatalf("recall requests = %d, want 2", len(recallRequests))
	}
	if recallRequests[1].Query != "launch" || recallRequests[1].Limit != defaultTelegramRecallLimit || !recallRequests[1].MemoriesOnly {
		t.Fatalf("recall memories request = %+v", recallRequests[1])
	}

	ch.handleUpdate(context.Background(), messageUpdate(4, 123, "/forget_memory mem_1"))
	if deletePath != "/v1/memories/mem_1" {
		t.Fatalf("delete path = %q", deletePath)
	}
	if got := tg.lastSendText(); !strings.Contains(got, "Forgot mem_1") || !strings.Contains(got, "Project launch is Friday") {
		t.Fatalf("forget reply = %q", got)
	}
	if starts != 0 {
		t.Fatalf("daemon runs started = %d, want 0", starts)
	}
}

func TestDaemonClientMemoryCommandValidationDoesNotCallDaemon(t *testing.T) {
	var requests int
	daemonServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
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

	ch.handleUpdate(context.Background(), messageUpdate(1, 123, "/recall"))
	if requests != 0 {
		t.Fatalf("daemon requests = %d, want 0", requests)
	}
	if got := tg.lastSendText(); !strings.Contains(got, "Command error: /recall query is required") {
		t.Fatalf("validation reply = %q", got)
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
