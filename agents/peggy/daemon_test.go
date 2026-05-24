package peggy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/daemon"
	filestore "github.com/erain/glue/stores/file"
)

func TestPeggyDaemonCodingPermissionViaDaemon(t *testing.T) {
	workDir := t.TempDir()
	provider := &scriptedProvider{turns: [][]glue.ProviderEvent{
		toolCallTurn("c1", "write_file", `{"path":"note.txt","content":"hello from daemon"}`),
		peggyTextTurn("done"),
	}}
	p, err := New(Options{
		Settings: Settings{Coding: CodingSettings{
			Enabled:        true,
			WorkDir:        workDir,
			AllowOverwrite: true,
		}},
		Provider: provider,
		Store:    filestore.New(filepath.Join(t.TempDir(), "sessions")),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	srv, err := daemon.New(daemon.Options{
		Host:              p.Agent(),
		Token:             "tok",
		PermissionTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	start := startPeggyDaemonRun(t, ts.URL)
	events := collectPeggyDaemonEvents(t, ts.URL, start.RunID, start.EventsURL)
	if !eventSeen(events, "permission_request") {
		t.Fatalf("events = %v, want permission_request", eventTypes(events))
	}
	if got := readFileString(t, filepath.Join(workDir, "note.txt")); got != "hello from daemon" {
		t.Fatalf("written file = %q", got)
	}
	if types := eventTypes(events); types[len(types)-1] != "run_done" {
		t.Fatalf("events = %v, want terminal run_done", types)
	}
}

func TestPeggyV03ReleaseSmoke(t *testing.T) {
	workDir := t.TempDir()
	provider := &scriptedProvider{turns: [][]glue.ProviderEvent{
		toolCallTurn("c1", "write_file", `{"path":"cli.txt","content":"trusted cli"}`),
		peggyTextTurn("cli done"),
		toolCallTurn("c2", "write_file", `{"path":"telegram.txt","content":"blocked telegram"}`),
		peggyTextTurn("telegram done"),
	}}
	p, err := New(Options{
		Settings: Settings{
			Coding: CodingSettings{
				Enabled:        true,
				WorkDir:        workDir,
				AllowOverwrite: true,
			},
			Permissions: PermissionSettings{
				DefaultTier: string(PermissionTierPrompt),
				Channels: map[string]string{
					PermissionChannelCLI:      string(PermissionTierTrusted),
					PermissionChannelTelegram: string(PermissionTierReadOnly),
				},
			},
		},
		Provider: provider,
		Store:    filestore.New(filepath.Join(t.TempDir(), "sessions")),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	srv, err := daemon.New(daemon.Options{
		Host:             p.Agent(),
		Token:            "tok",
		PermissionPolicy: NewDaemonPermissionPolicy(p.Settings().Permissions),
	})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	cliStart := startPeggyDaemonRunWithClient(t, ts.URL, "default", "cli:test")
	cliEvents := collectPeggyDaemonEvents(t, ts.URL, cliStart.RunID, cliStart.EventsURL)
	if eventSeen(cliEvents, "permission_request") {
		t.Fatalf("cli events = %v, want trusted tier without prompt", eventTypes(cliEvents))
	}
	if got := readFileString(t, filepath.Join(workDir, "cli.txt")); got != "trusted cli" {
		t.Fatalf("cli.txt = %q", got)
	}

	telegramStart := startPeggyDaemonRunWithClient(t, ts.URL, "telegram:123", "telegram:123")
	telegramEvents := collectPeggyDaemonEvents(t, ts.URL, telegramStart.RunID, telegramStart.EventsURL)
	if eventSeen(telegramEvents, "permission_request") {
		t.Fatalf("telegram events = %v, want read_only tier without prompt", eventTypes(telegramEvents))
	}
	if _, err := os.Stat(filepath.Join(workDir, "telegram.txt")); err == nil {
		t.Fatal("telegram.txt exists; read_only tier should deny write_file")
	}
	if !strings.Contains(toolEndTextFromDaemon(t, telegramEvents), "telegram channel is read-only") {
		t.Fatalf("telegram tool error missing read_only reason")
	}
}

type startRunResponse struct {
	RunID     string `json:"run_id"`
	EventsURL string `json:"events_url"`
}

func startPeggyDaemonRun(t *testing.T, baseURL string) startRunResponse {
	t.Helper()
	return startPeggyDaemonRunWithClient(t, baseURL, "default", "cli:test")
}

func startPeggyDaemonRunWithClient(t *testing.T, baseURL, sessionID, clientID string) startRunResponse {
	t.Helper()
	body := bytes.NewBufferString(`{"text":"write the note","client_id":` + strconv.Quote(clientID) + `,"max_turns":3}`)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/sessions/"+sessionID+"/runs", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("start status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}
	var start startRunResponse
	if err := json.NewDecoder(resp.Body).Decode(&start); err != nil {
		t.Fatal(err)
	}
	if start.RunID == "" || start.EventsURL == "" {
		t.Fatalf("start response = %+v", start)
	}
	return start
}

func collectPeggyDaemonEvents(t *testing.T, baseURL, runID, eventsURL string) []daemon.EventEnvelope {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+eventsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var events []daemon.EventEnvelope
	scan := bufio.NewScanner(resp.Body)
	for scan.Scan() {
		line := scan.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event daemon.EventEnvelope
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
		if event.Type == "permission_request" {
			postPeggyDaemonDecision(t, baseURL, runID, permissionID(t, event))
		}
		if event.Type == "run_done" || event.Type == "run_error" {
			break
		}
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func postPeggyDaemonDecision(t *testing.T, baseURL, runID, permissionID string) {
	t.Helper()
	url := baseURL + "/v1/runs/" + runID + "/permissions/" + permissionID + "/decision"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(`{"allow":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("X-Glue-Client-ID", "cli:test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("decision status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func permissionID(t *testing.T, event daemon.EventEnvelope) string {
	t.Helper()
	payload, ok := event.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload = %T, want object", event.Payload)
	}
	id, ok := payload["permission_id"].(string)
	if !ok || id == "" {
		t.Fatalf("payload = %#v, want permission_id", payload)
	}
	return id
}

func eventSeen(events []daemon.EventEnvelope, want string) bool {
	for _, event := range events {
		if event.Type == want {
			return true
		}
	}
	return false
}

func eventTypes(events []daemon.EventEnvelope) []string {
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type)
	}
	return types
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func toolEndTextFromDaemon(t *testing.T, events []daemon.EventEnvelope) string {
	t.Helper()
	for _, event := range events {
		if event.Type != "tool_end" {
			continue
		}
		payload, ok := event.Payload.(map[string]any)
		if !ok {
			t.Fatalf("tool_end payload = %#v", event.Payload)
		}
		result, ok := payload["tool_result"].(map[string]any)
		if !ok {
			t.Fatalf("tool_end missing result: %#v", payload)
		}
		content, ok := result["content"].([]any)
		if !ok || len(content) == 0 {
			t.Fatalf("tool_end missing content: %#v", result)
		}
		part, ok := content[0].(map[string]any)
		if !ok {
			t.Fatalf("tool_end content = %#v", content[0])
		}
		text, _ := part["text"].(string)
		return text
	}
	t.Fatal("missing tool_end event")
	return ""
}
