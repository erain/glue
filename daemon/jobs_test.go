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
	"sync/atomic"
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
	patchReq := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path+"/patch", nil)
		req.Header.Set("Authorization", "Bearer token")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		return w
	}
	if w := patchReq(); w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/x-diff") || !strings.Contains(w.Body.String(), "hello.txt") {
		t.Fatalf("patch = %d %q %q", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	orig := jobs.MaxPatchBytes
	jobs.MaxPatchBytes = 8
	t.Cleanup(func() { jobs.MaxPatchBytes = orig })
	if w := patchReq(); w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), "glue/"+id) {
		t.Fatalf("oversized patch = %d %s", w.Code, w.Body.String())
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

func TestCredentialsEndpointRequiresAuth(t *testing.T) {
	srv, ts, _ := jobHTTPFixture(t, &turnProvider{})
	body := `{"github_token":"ghp_AuthEndpointToken123456","provider_keys":{"GEMINI_API_KEY":"AIzaSyAuthEndpointKey123456"}}`
	for _, tok := range []string{"", "wrong-token"} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/credentials", strings.NewReader(body))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token=%q resp=%v err=%v", tok, resp, err)
		}
		resp.Body.Close()
	}
	jobHTTP(t, srv, http.MethodGet, "/v1/credentials", "", http.StatusMethodNotAllowed)
	jobHTTP(t, srv, http.MethodPost, "/v1/credentials", body, http.StatusOK)
	if !srv.jobs.HasCredentials() {
		t.Fatal("expected HasCredentials() = true")
	}
}

func TestDaemonPostJobPROpensAndPersists(t *testing.T) {
	var posts atomic.Int32
	routes := map[string]string{"GET /repos/erain/glue-sandbox": `{"default_branch":"main"}`, "GET /repos/erain/glue-sandbox/pulls": `[]`}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := routes[r.Method+" "+r.URL.Path]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		posts.Add(1)
		_, _ = w.Write([]byte(`{"html_url":"https://github.com/erain/glue-sandbox/pull/15"}`))
	}))
	t.Cleanup(gh.Close)
	t.Setenv("GLUE_GITHUB_API_URL", gh.URL)
	t.Setenv("GLUE_GITHUB_TOKEN", "gh-tok")
	s, _, _ := jobHTTPFixture(t, nil)
	specData, _ := json.Marshal(jobs.Spec{Objective: "add a.txt", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}})
	id := jobHTTP(t, s, http.MethodPost, "/v1/jobs", string(specData), 201)["job"].(map[string]any)["id"].(string)
	jobHTTP(t, s, http.MethodPost, "/v1/jobs/"+id+"/pr", "{}", http.StatusConflict)
	jobHTTP(t, s, http.MethodDelete, "/v1/jobs/"+id, "", http.StatusAccepted)
	wLogin := httptest.NewRecorder()
	s.ServeHTTP(wLogin, httptest.NewRequest(http.MethodPost, "/v1/session/login", strings.NewReader(`{"token":"token"}`)))
	cookie, csrf := wLogin.Result().Cookies()[0], s.csrfToken()
	for _, tc := range []struct {
		csrf string
		want int
	}{{"", http.StatusForbidden}, {csrf, http.StatusOK}, {csrf, http.StatusOK}} {
		w, req := httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/jobs/"+id+"/pr", nil)
		req.AddCookie(cookie)
		if tc.csrf != "" {
			req.Header.Set(csrfHeaderName, tc.csrf)
		}
		if s.ServeHTTP(w, req); w.Code != tc.want {
			t.Fatalf("POST pr csrf=%q: code=%d body=%s", tc.csrf, w.Code, w.Body.String())
		}
	}
	if posts.Load() != 1 || jobHTTP(t, s, http.MethodGet, "/v1/jobs/"+id+"/result", "", 200)["pr_url"] != "https://github.com/erain/glue-sandbox/pull/15" || jobHTTP(t, s, http.MethodGet, "/v1/jobs/"+id, "", 200)["pr_url"] != "https://github.com/erain/glue-sandbox/pull/15" {
		t.Fatalf("posts=%d or pr_url not persisted", posts.Load())
	}
}

func TestPatchEndpointWhileRunning(t *testing.T) {
	p := &turnProvider{turns: [][]glue.ProviderEvent{
		textTurn(`{"items":[{"title":"write"}]}`),
		{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "w1", Name: "write_file", Arguments: []byte(`{"path":"live.txt","content":"live http\n"}`)}}, {Type: glue.ProviderEventDone}},
		{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "s1", Name: "shell_exec", Arguments: []byte(`{"argv":["git","push","origin","main"]}`)}}, {Type: glue.ProviderEventDone}},
	}}
	s, _, repo := jobHTTPFixture(t, p)
	specData, _ := json.Marshal(jobs.Spec{Objective: "write", Repo: jobs.RepoSpec{Path: repo}, Policy: "workspace-auto"})
	id := jobHTTP(t, s, http.MethodPost, "/v1/jobs", string(specData), 201)["job"].(map[string]any)["id"].(string)
	jobHTTP(t, s, http.MethodGet, "/v1/jobs/"+id+"/patch", "", http.StatusConflict)
	jobHTTP(t, s, http.MethodPost, "/v1/jobs/"+id+"/start", "", http.StatusOK)
	for deadline := time.Now().Add(10 * time.Second); len(jobHTTP(t, s, http.MethodGet, "/v1/jobs/"+id+"/permissions?state=pending", "", 200)["permissions"].([]any)) == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for pending permission")
		}
	}
	req, w := httptest.NewRequest(http.MethodGet, "/v1/jobs/"+id+"/patch", nil), httptest.NewRecorder()
	req.Header.Set("Authorization", "Bearer token")
	if s.ServeHTTP(w, req); w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/x-diff") || !strings.Contains(w.Body.String(), "+live http") {
		t.Fatalf("GET patch = %d (%q): %s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	jobHTTP(t, s, http.MethodPost, "/v1/jobs/"+id+"/messages?interrupt=true", `{}`, http.StatusAccepted)
}

func TestPostJobsResolvesGitHubIssue(t *testing.T) {
	var seenAuth atomic.Value
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth.Store(r.Header.Get("Authorization"))
		if strings.HasSuffix(r.URL.Path, "/comments") {
			_, _ = w.Write([]byte(`[{"body":"check phone width","user":{"login":"bob"}}]`))
			return
		}
		_, _ = w.Write([]byte(`{"title":"Ship #/new view","body":"Details here","state":"open","comments":1}`))
	}))
	t.Cleanup(gh.Close)
	t.Setenv("GLUE_GITHUB_API_URL", gh.URL)
	s, _, _ := jobHTTPFixture(t, nil)
	s.jobs.SetCredentials(jobs.Credentials{GitHubToken: "pushed-gh-tok"})
	for _, raw := range []string{`{"issue":"erain/glue-sandbox#42"}`, `{"issue":"#42","repo":{"url":"https://github.com/erain/glue-sandbox"}}`, `{"issue":"https://github.com/erain/glue-sandbox/issues/42","delivery":"branch"}`} {
		job := jobHTTP(t, s, http.MethodPost, "/v1/jobs", raw, http.StatusCreated)["job"].(map[string]any)
		spec := job["spec"].(map[string]any)
		wantDel := "draft_pr"
		if strings.Contains(raw, `"branch"`) {
			wantDel = "branch"
		}
		if spec["objective"] != "Ship #/new view" || spec["issue"] != "erain/glue-sandbox#42" || spec["delivery"] != wantDel || spec["repo"].(map[string]any)["url"] != "https://github.com/erain/glue-sandbox" || !strings.Contains(spec["context"].(string), "@bob: check phone width") || seenAuth.Load() != "Bearer pushed-gh-tok" {
			t.Fatalf("raw=%s spec=%+v auth=%v", raw, spec, seenAuth.Load())
		}
	}
}

func TestPostJobsValidationErrors(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/7") {
			_, _ = w.Write([]byte(`{"title":"PR","state":"open","pull_request":{}}`))
		} else {
			_, _ = w.Write([]byte(`{"title":"Closed","state":"closed"}`))
		}
	}))
	t.Cleanup(gh.Close)
	t.Setenv("GLUE_GITHUB_API_URL", gh.URL)
	t.Setenv("GLUE_GITHUB_TOKEN", "tok")
	s, _, repo := jobHTTPFixture(t, nil)
	for _, bad := range []string{`{"issue":"erain/glue-sandbox#7"}`, `{"issue":"erain/glue-sandbox#8"}`, `{"issue":"#42"}`, `{"mode":"interactive","issue":"erain/glue-sandbox#42"}`, `{"issue":"bad-ref"}`} {
		jobHTTP(t, s, http.MethodPost, "/v1/jobs", bad, http.StatusBadRequest)
	}
	wLogin := httptest.NewRecorder()
	s.ServeHTTP(wLogin, httptest.NewRequest(http.MethodPost, "/v1/session/login", strings.NewReader(`{"token":"token"}`)))
	ck, csrf := wLogin.Result().Cookies()[0], s.csrfToken()
	for _, tc := range []struct {
		csrf string
		want int
	}{{"", http.StatusForbidden}, {"wrong", http.StatusForbidden}, {csrf, http.StatusCreated}} {
		w, req := httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(`{"objective":"csrf","repo":{"path":`+`"`+repo+`"}}`))
		req.AddCookie(ck)
		if tc.csrf != "" {
			req.Header.Set(csrfHeaderName, tc.csrf)
		}
		if s.ServeHTTP(w, req); w.Code != tc.want {
			t.Fatalf("csrf=%q: got %d (%s), want %d", tc.csrf, w.Code, w.Body.String(), tc.want)
		}
	}
}
