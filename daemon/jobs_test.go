// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/jobs"
)

func jobHTTPFixture(t *testing.T, p glue.Provider) (*Server, *httptest.Server, string) {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-qb", "main"}, {"config", "user.email", "test@example.com"}, {"config", "user.name", "test"}, {"commit", "--allow-empty", "-qm", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v %s", err, out)
		}
	}
	state := t.TempDir()
	m, err := jobs.New(jobs.Options{StateDir: state, ProviderFactory: func(string) (glue.Provider, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	s, err := New(Options{Host: sessionOnlyHost{}, Token: "token", StateDir: state, Jobs: m, KeepaliveInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return s, ts, repo
}
func jobHTTP(t *testing.T, s *Server, method, path, body string, want int) map[string]any {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != want {
		t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func createHTTPJob(t *testing.T, s *Server, repo string, start bool) string {
	t.Helper()
	data, _ := json.Marshal(jobs.Spec{Objective: "write a file", Repo: jobs.RepoSpec{Path: repo}, Policy: "ask-all"})
	path := "/v1/jobs"
	if start {
		path += "?start=true"
	}
	body := jobHTTP(t, s, http.MethodPost, path, string(data), 201)
	return body["job"].(map[string]any)["id"].(string)
}
func TestJobHTTPCreateInspectResultAndCancel(t *testing.T) {
	s, _, repo := jobHTTPFixture(t, nil)
	jobHTTP(t, eventServer(t, t.TempDir()), http.MethodGet, "/v1/jobs", "", 404)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/jobs", nil))
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	for _, path := range []string{"/v1/jobs/missing", "/v1/jobs/missing/events", "/v1/jobs/missing/permissions", "/v1/jobs/missing/result"} {
		jobHTTP(t, s, http.MethodGet, path, "", 404)
	}
	jobHTTP(t, s, http.MethodPost, "/v1/jobs/missing/start", "", 404)
	jobHTTP(t, s, http.MethodDelete, "/v1/jobs/missing", "", 404)
	invalid := jobHTTP(t, s, http.MethodPost, "/v1/jobs", "{}", 400)
	if !strings.Contains(invalid["error"].(map[string]any)["message"].(string), "objective") {
		t.Fatal(invalid)
	}
	jobHTTP(t, s, http.MethodPost, "/v1/jobs?start=bad", "{}", 400)
	for _, body := range []string{`{"unknown":true}`, `{} {}`, `{"objective":`} {
		jobHTTP(t, s, http.MethodPost, "/v1/jobs", body, 400)
	}
	id := createHTTPJob(t, s, repo, false)
	path := "/v1/jobs/" + id
	detail := jobHTTP(t, s, http.MethodGet, path, "", 200)
	if detail["status"] != "created" || detail["pending_permission_count"] != float64(0) {
		t.Fatal(detail)
	}
	list := jobHTTP(t, s, http.MethodGet, "/v1/jobs?status=created&limit=1", "", 200)
	if len(list["jobs"].([]any)) != 1 {
		t.Fatal(list)
	}
	jobHTTP(t, s, http.MethodGet, "/v1/jobs?before="+id, "", 200)
	jobHTTP(t, s, http.MethodGet, "/v1/jobs?limit=-1", "", 400)
	jobHTTP(t, s, http.MethodGet, "/v1/jobs?status=bad", "", 400)
	permissions := jobHTTP(t, s, http.MethodGet, path+"/permissions?state=pending", "", 200)
	if len(permissions["permissions"].([]any)) != 0 {
		t.Fatal(permissions)
	}
	jobHTTP(t, s, http.MethodGet, path+"/permissions?state=bad", "", 400)
	jobHTTP(t, s, http.MethodGet, path+"/result", "", 409)
	jobHTTP(t, s, http.MethodDelete, path, "", 202)
	detail = jobHTTP(t, s, http.MethodGet, path, "", 200)
	if detail["status"] != "canceled" {
		t.Fatal(detail)
	}
	canceledResult := jobHTTP(t, s, http.MethodGet, path+"/result", "", 200)
	if canceledResult["job_id"] != id || canceledResult["status"] != "canceled" {
		t.Fatal(canceledResult)
	}
	jobHTTP(t, s, http.MethodPost, path+"/start", "", 409)
	for _, tc := range []struct{ method, suffix string }{{http.MethodGet, "/start"}, {http.MethodPost, "/events"}, {http.MethodPost, "/permissions"}, {http.MethodPost, "/result"}} {
		jobHTTP(t, s, tc.method, path+tc.suffix, "", 405)
	}
}
func TestJobHTTPStartCancelAndReplay(t *testing.T) {
	p := &blockingProvider{started: make(chan struct{}), canceled: make(chan struct{})}
	s, ts, repo := jobHTTPFixture(t, p)
	id := createHTTPJob(t, s, repo, true)
	path := "/v1/jobs/" + id
	select {
	case <-p.started:
	case <-time.After(10 * time.Second):
		t.Fatal("provider did not start")
	}
	jobHTTP(t, s, http.MethodPost, path+"/start", "", 200)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+path+"/events", nil)
	req.Header.Set("Authorization", "Bearer token")
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	stream.Body.Close()
	select {
	case <-p.canceled:
		t.Fatal("disconnect canceled job")
	default:
	}
	jobHTTP(t, s, http.MethodDelete, path, "", 202)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+path+"/events?after=2", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Last-Event-ID", "0")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	scanner := bufio.NewScanner(response.Body)
	next := int64(3)
	done := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			var event EventEnvelope
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
				t.Fatal(err)
			}
			if event.Seq != next {
				t.Fatalf("seq %d want %d", event.Seq, next)
			}
			next++
			if event.Type == "job_done" {
				done = true
			}
		}
	}
	if err := scanner.Err(); err != nil || !done {
		t.Fatalf("done %v err %v", done, err)
	}
	detail := jobHTTP(t, s, http.MethodGet, path, "", 200)
	if detail["status"] != "canceled" {
		t.Fatal(detail)
	}
}
func TestJobHTTPPermissionDecision(t *testing.T) {
	p := &turnProvider{turns: [][]glue.ProviderEvent{textTurn(`{"items":[{"title":"write"}]}`), {{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "write", Name: "write_file", Arguments: []byte(`{"path":"hello.txt","content":"hello"}`)}}, {Type: glue.ProviderEventDone}}, textTurn("done"), textTurn(`{"done":true,"items":[{"title":"write","done":true}]}`)}}
	s, ts, repo := jobHTTPFixture(t, p)
	id := createHTTPJob(t, s, repo, true)
	path := "/v1/jobs/" + id
	deadline := time.Now().Add(10 * time.Second)
	permissionID := ""
	for permissionID == "" {
		pending := jobHTTP(t, s, http.MethodGet, path+"/permissions?state=pending", "", 200)["permissions"].([]any)
		if len(pending) > 0 {
			permissionID = pending[0].(map[string]any)["id"].(string)
		}
		if time.Now().After(deadline) {
			t.Fatal("no pending request")
		}
		time.Sleep(5 * time.Millisecond)
	}
	decisionPath := path + "/permissions/" + permissionID + "/decision"
	jobHTTP(t, s, http.MethodPost, decisionPath, "{}", 400)
	jobHTTP(t, s, http.MethodPost, decisionPath, `{"allow":true,"remember_for":"bad"}`, 400)
	jobHTTP(t, s, http.MethodPost, path+"/permissions/missing/decision", `{"allow":true}`, 409)
	jobHTTP(t, s, http.MethodPost, decisionPath, `{"allow":true}`, 200)
	jobHTTP(t, s, http.MethodPost, decisionPath, `{"allow":true}`, 409)
	events := getSSE(t, ts.URL+path+"/events", "token")
	if len(events) == 0 || events[len(events)-1].Type != "job_done" {
		t.Fatal(eventTypes(events))
	}
	allowed := jobHTTP(t, s, http.MethodGet, path+"/permissions?state=allowed", "", 200)["permissions"].([]any)
	if len(allowed) != 1 {
		t.Fatal(allowed)
	}
	detail := jobHTTP(t, s, http.MethodGet, path, "", 200)
	if detail["status"] != "achieved" {
		t.Fatal(detail)
	}
	result := jobHTTP(t, s, http.MethodGet, path+"/result", "", 200)
	if result["job_id"] != id || result["status"] != "achieved" || result["branch"] != "glue/"+id || !strings.Contains(result["diffstat"].(string), "hello.txt") {
		t.Fatal(result)
	}
}
func TestJobHTTPIdleKeepalive(t *testing.T) {
	s, ts, repo := jobHTTPFixture(t, nil)
	id := createHTTPJob(t, s, repo, false)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/jobs/"+id+"/events", nil)
	req.Header.Set("Authorization", "Bearer token")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		if scanner.Text() == ": ping" {
			return
		}
	}
	t.Fatal("no keepalive")
}

func TestDaemonPermissionPolicyAutoEvents(t *testing.T) {
	ws := t.TempDir()
	policy := jobs.Policy{
		Name:    "test-policy",
		Default: jobs.DecisionAsk,
		Rules: []jobs.PolicyRule{
			{Name: "deny-secret", Tool: "write_file", Target: "secret.txt", Decision: jobs.DecisionDeny},
			{Name: "allow-readme", Tool: "write_file", Target: "README.md", Decision: jobs.DecisionAllow},
		},
	}
	writeTool := glue.NewTool[struct {
		Path string `json:"path"`
	}](glue.ToolSpec{
		Name:               "write_file",
		RequiresPermission: true,
		PermissionAction:   "write_file",
		PermissionTarget: func(call glue.ToolCall) string {
			var a struct {
				Path string `json:"path"`
			}
			_ = json.Unmarshal(call.Arguments, &a)
			return a.Path
		},
		Parameters: json.RawMessage(`{"type":"object"}`),
	}, func(context.Context, struct {
		Path string `json:"path"`
	}) (glue.ToolResult, error) {
		return glue.TextResult("written"), nil
	})
	agent := glue.NewAgent(glue.AgentOptions{
		Provider: &turnProvider{turns: [][]glue.ProviderEvent{
			{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "1", Name: "write_file", Arguments: []byte(`{"path":"README.md"}`)}}, {Type: glue.ProviderEventDone}},
			{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "2", Name: "write_file", Arguments: []byte(`{"path":"secret.txt"}`)}}, {Type: glue.ProviderEventDone}},
			textTurn("done"),
		}},
		Tools: []glue.Tool{writeTool},
	})
	srv := newTestServerWithPolicy(t, agent, NewPermissionPolicy(policy, ws))
	ts := httptest.NewServer(srv)
	defer ts.Close()
	start := startRunWithClient(t, ts.URL, "default", "cli:test")
	events := getSSE(t, ts.URL+start.EventsURL, "token")
	var autos []map[string]any
	for _, e := range events {
		if e.Type == "permission_auto" {
			autos = append(autos, e.Payload.(map[string]any))
		}
	}
	if len(autos) != 2 || autos[0]["decision"] != "allow" || autos[0]["rule"] != "allow-readme" || autos[1]["decision"] != "deny" || autos[1]["rule"] != "deny-secret" {
		t.Fatalf("permission_auto events = %+v", autos)
	}
}
