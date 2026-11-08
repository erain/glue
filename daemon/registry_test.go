package daemon

import (
	"encoding/json"
	"fmt"
	"github.com/erain/glue"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func registryRequest(t *testing.T, s *Server, path string, status int) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != status {
		t.Fatalf("GET %s: %d: %s", path, w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}
func TestRunRegistryListAndGet(t *testing.T) {
	s := eventServer(t, t.TempDir())
	s.newID = sequenceIDs()
	s.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	first := finishedEvents(t, s)
	second := finishedEvents(t, s)
	detail := registryRequest(t, s, "/v1/runs/"+first.id, 200)
	if detail["status"] != "done" || detail["session_id"] != "session" || detail["last_seq"] != float64(6) {
		t.Fatal(detail)
	}
	list := registryRequest(t, s, "/v1/runs?session_id=session&status=done&limit=1", 200)
	rows := list["runs"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["id"] != second.id {
		t.Fatal(list)
	}
	page := registryRequest(t, s, fmt.Sprintf("/v1/runs?before=%s&limit=1", second.id), 200)
	if rows := page["runs"].([]any); len(rows) != 1 || rows[0].(map[string]any)["id"] != first.id {
		t.Fatal(page)
	}
	empty := registryRequest(t, s, "/v1/runs?session_id=missing", 200)
	if len(empty["runs"].([]any)) != 0 {
		t.Fatal(empty)
	}
	registryRequest(t, s, "/v1/runs?limit=-1", 400)
	registryRequest(t, s, "/v1/runs?status=invalid", 400)
	registryRequest(t, s, "/v1/runs/missing", 404)
}
func TestRestartMarksRunsInterrupted(t *testing.T) {
	dir := t.TempDir()
	s := eventServer(t, dir)
	r := s.newRun("session", "client", func() {})
	r.emit("run_start", nil)
	r.emit("permission_request", permissionRequestPayload{PermissionID: "pending", ExpiresAt: s.now().Add(time.Hour)})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	other := eventServer(t, dir)
	detail := registryRequest(t, other, "/v1/runs/"+r.id, 200)
	if w := eventRequest(other, r.id, "", ""); !strings.Contains(w.Body.String(), "event: run_interrupted") {
		t.Fatal(w.Body.String())
	}
	if detail["status"] != "interrupted" {
		t.Fatal(detail)
	}
	events, err := other.log.read(r.id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[2].Type != "run_interrupted" || events[2].Seq != 3 {
		t.Fatal(events)
	}
	expired := registryRequest(t, other, "/v1/runs/"+r.id+"/permissions?state=expired", 200)
	if len(expired["permissions"].([]any)) != 1 {
		t.Fatal(expired)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	third := eventServer(t, dir)
	events, err = third.log.read(r.id, 0)
	if err != nil || len(events) != 3 {
		t.Fatalf("events %v err %v", events, err)
	}
}

func TestRunRegistryTerminalErrors(t *testing.T) {
	for _, code := range []string{"canceled", "internal"} {
		t.Run(code, func(t *testing.T) {
			s := eventServer(t, t.TempDir())
			r := s.newRun("s", "client", func() {})
			r.emit("run_error", map[string]any{"error": protocolError{Code: code, Message: "failed"}})
			r.finish()
			record := registryRequest(t, s, "/v1/runs/"+r.id, 200)
			want := "error"
			if code == "canceled" {
				want = "canceled"
			}
			if record["status"] != want || record["error"].(map[string]any)["code"] != code {
				t.Fatal(record)
			}
		})
	}
}
func TestRunRegistryInitialWriteFailure(t *testing.T) {
	s, err := New(Options{Host: glue.NewAgent(glue.AgentOptions{Provider: scriptedProvider{}}), Token: "token", StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.log.db.Exec("DROP TABLE runs"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/s/runs", strings.NewReader(`{"text":"go"}`))
	req.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != 500 || s.activeRunCount() != 0 {
		t.Fatalf("status %d active %d", w.Code, s.activeRunCount())
	}
}

func TestRegistryEventTransactionRollsBack(t *testing.T) {
	s := eventServer(t, t.TempDir())
	r := s.newRun("s", "client", func() {})
	if _, err := s.log.db.Exec("DROP TABLE runs"); err != nil {
		t.Fatal(err)
	}
	r.emit("run_start", nil)
	events, err := s.log.read(r.id, 0)
	if err != nil || len(events) != 0 || r.persistErr == nil {
		t.Fatalf("events %v err %v persistErr %v", events, err, r.persistErr)
	}
}
