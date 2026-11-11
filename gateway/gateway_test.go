// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package gateway

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/daemon"
	"github.com/erain/glue/internal/eventlog"
	"github.com/erain/glue/internal/jobclient"
	"github.com/erain/glue/jobs"
)

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
func TestConfigParseAndValidate(t *testing.T) {
	t.Setenv("GLUE_GATEWAY_TOKEN", "env-secret")
	t.Setenv("GLUE_GITHUB_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "ghp_fallback_token")
	t.Setenv("GLUE_AX_IMAGE", "")
	t.Setenv("GLUE_TASK_IMAGE", "localhost:5001/glue-task@sha256:defaultimg")
	cfgPath := filepath.Join(t.TempDir(), "gateway.yaml")
	raw := "public_url: http://gw.example.com/\nstate_dir: /var/lib/glue-gateway\nauth:\n  mode: dev-token\ndefault_backend: local\nbackends:\n  - name: local\n    type: local\n    url: http://127.0.0.1:8787/\n    token: backend-token\n  - name: ax\n    type: ax\n    kube_config: /tmp/kubeconfig\n    kube_context: kind-glue-ax\n    resources:\n      limits:\n        cpu: \"2\"\n        memory: 4Gi\n"
	must(0, os.WriteFile(cfgPath, []byte(raw), 0o600))
	cfg := must(LoadConfig(cfgPath))
	if cfg.Listen != "127.0.0.1:8800" || cfg.PublicURL != "http://gw.example.com" || cfg.Auth.Token != "env-secret" || cfg.Credentials.GitHubToken != "ghp_fallback_token" || cfg.DefaultBackend != "local" || cfg.Backends[0].URL != "http://127.0.0.1:8787" {
		t.Fatalf("unexpected normalized config: %+v", cfg)
	}
	if axCfg := cfg.Backends[1]; axCfg.Atespace != "default" || axCfg.AXServer != "" || axCfg.KubeConfig != "/tmp/kubeconfig" || axCfg.KubeContext != "kind-glue-ax" || axCfg.Image != "localhost:5001/glue-task@sha256:defaultimg" || axCfg.Resources.CPU != "2" || axCfg.Resources.Memory != "4Gi" || len(axCfg.EgressHosts) != len(DefaultEgressHosts) {
		t.Fatalf("unexpected normalized ax backend config: %+v", axCfg)
	}
	ghCfg := must(ParseConfig([]byte("public_url: https://gw.example.com/\nstate_dir: /tmp\nauth:\n  mode: github\n  client_id: cid\n  client_secret: csec\n  allowed_logins: [alice]\nbackends:\n  - name: l\n    type: local\n    url: http://x\n    token: t\n")))
	if ghCfg.Auth.Mode != "github" || ghCfg.Auth.SessionAbsoluteTTL != 168*time.Hour || ghCfg.Auth.SessionIdleTTL != 24*time.Hour {
		t.Fatalf("unexpected github auth config: %+v", ghCfg.Auth)
	}
	t.Setenv("GLUE_GATEWAY_TOKEN", "")
	t.Setenv("GLUE_TASK_IMAGE", "")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	for _, tc := range []struct{ yaml, want string }{
		{"unknown_key: 1\n", "field unknown_key"},
		{"state_dir: /tmp\nauth: {mode: dev-token, token: t}\nbackends: [{name: ax, type: ax, image: img, substrate: api.ate-system:443}]\n", "field substrate"},
		{"state_dir: /tmp\nauth: {mode: dev-token, token: t}\nbackends: [{name: ax, type: ax, image: img, router: atenet-router.ate-system:8444}]\n", "field router"},
		{"state_dir: /tmp\nauth: {mode: dev-token, token: t}\nbackends: [{name: ax, type: ax, image: img, cert_file: /tmp/c}]\n", "field cert_file"},
		{"state_dir: /tmp\nauth: {mode: dev-token, token: t}\nbackends: [{name: ax, type: ax, image: img, ca_file: /tmp/ca}]\n", "field ca_file"},
		{"state_dir: /tmp\nauth: {mode: dev-token, token: t}\nbackends: [{name: ax, type: ax, image: img, token_file: /tmp/t}]\n", "field token_file"},
		{"auth: {mode: dev-token, token: t}\nbackends: [{name: l, type: local, url: http://x, token: t}]\n", "state_dir=\"\""},
		{"public_url: https://gw.example.com\nstate_dir: /tmp\nauth: {mode: dev-token, token: t}\nbackends: [{name: l, type: local, url: http://x, token: t}]\n", "--insecure-dev"},
		{"state_dir: /tmp\nauth: {mode: github, token: t}\nbackends: [{name: l, type: local, url: http://x, token: t}]\n", "auth.mode=\"github\""},
		{"state_dir: /tmp\nauth: {mode: dev-token}\nbackends: [{name: l, type: local, url: http://x, token: t}]\n", "has_token=false"},
		{"state_dir: /tmp\nauth: {mode: dev-token, token: t}\n", "backends=0"},
		{"state_dir: /tmp\nauth: {mode: dev-token, token: t}\nbackends: [{name: l, type: local, url: http://x, token: t}, {name: l, type: local, url: http://y, token: t}]\n", "duplicate=true"},
		{"state_dir: /tmp\nauth: {mode: dev-token, token: t}\nbackends: [{name: l, type: ax, url: http://x, token: t}]\n", "type=\"ax\""},
		{"state_dir: /tmp\nauth: {mode: dev-token, token: t}\nbackends: [{name: ax, type: ax}]\n", "type=\"ax\""},
		{"state_dir: /tmp\nauth: {mode: dev-token, token: t}\nbackends: [{name: ax, type: ax, image: img, egress_hosts: [\"\"]}]\n", "type=\"ax\""},
		{"state_dir: /tmp\nauth: {mode: dev-token, token: t}\ndefault_backend: other\nbackends: [{name: l, type: local, url: http://x, token: t}]\n", "default_backend"},
	} {
		if _, err := ParseConfig([]byte(tc.yaml)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("yaml=%q: got err=%v, want substring %q", tc.yaml, err, tc.want)
		}
	}
}
func TestRegistryLifecycleAndEventDedup(t *testing.T) {
	ctx, t0, reg := context.Background(), time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC), must(OpenRegistry(t.TempDir()))
	t.Cleanup(func() { _ = reg.Close() })
	spec := jobs.Spec{Objective: "test registry", Repo: jobs.RepoSpec{Path: "/tmp/repo"}, Provider: "gemini", Model: "gemini-3.8-flash"}
	rec := must(reg.CreateJob(ctx, Record{ID: "job-000000000001", Backend: "local", BackendRef: "job-000000000001", Owner: "dev", Spec: spec, CreatedAt: t0}))
	if _, err := reg.CreateJob(ctx, Record{ID: rec.ID, Backend: "local", Spec: spec}); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("expected conflict on duplicate job ID, got %v", err)
	}
	for _, ev := range []eventlog.Envelope{
		{Version: 2, ID: rec.ID + ":1", RunID: rec.ID, Seq: 1, Time: t0, Type: "job_created", Payload: spec},
		{Version: 2, ID: rec.ID + ":2", RunID: rec.ID, Seq: 2, Time: t0.Add(time.Second), Type: "job_started"},
		{Version: 2, ID: rec.ID + ":3", RunID: rec.ID, Seq: 3, Time: t0.Add(2 * time.Second), Type: "plan", Payload: glue.GoalEvent{Iteration: 1, Checklist: []glue.ChecklistItem{{Title: "step 1"}}}},
		{Version: 2, ID: rec.ID + ":4", RunID: rec.ID, Seq: 4, Time: t0.Add(3 * time.Second), Type: "permission_request", Payload: map[string]any{"permission_id": "perm-1", "request": glue.PermissionRequest{Tool: "write_file", Action: "write_file", Target: "a.txt"}}},
	} {
		if !must(reg.AppendEvent(ctx, rec.ID, ev)) {
			t.Fatalf("AppendEvent seq=%d not inserted", ev.Seq)
		}
	}
	if must(reg.AppendEvent(ctx, rec.ID, eventlog.Envelope{Version: 2, ID: rec.ID + ":3", RunID: rec.ID, Seq: 3, Time: t0, Type: "plan"})) {
		t.Fatal("duplicate seq should be ignored")
	}
	if detail := must(reg.Inspect(ctx, rec.ID)); detail.Status != jobs.WaitingForInput || detail.LastSeq != 4 || detail.PendingPermissionCount != 1 || detail.Spec.Backend != "local" {
		t.Fatalf("Inspect waiting: %+v", detail)
	}
	if pending := must(reg.Permissions(ctx, rec.ID, "pending")); len(pending) != 1 || pending[0].ID != "perm-1" {
		t.Fatalf("Permissions pending: %+v", pending)
	}
	_, _ = reg.AppendEvent(ctx, rec.ID, eventlog.Envelope{Version: 2, ID: rec.ID + ":5", RunID: rec.ID, Seq: 5, Time: t0.Add(4 * time.Second), Type: "permission_decision", Payload: map[string]any{"permission_id": "perm-1", "decision": glue.PermissionDecision{Allow: true}}})
	_, _ = reg.AppendEvent(ctx, rec.ID, eventlog.Envelope{Version: 2, ID: rec.ID + ":6", RunID: rec.ID, Seq: 6, Time: t0.Add(5 * time.Second), Type: "job_done", Payload: map[string]any{"status": jobs.Achieved, "result": glue.GoalResult{Iterations: 1, Summary: "done", Checklist: []glue.ChecklistItem{{Title: "step 1", Done: true}}}}})
	must(0, reg.SaveResult(ctx, rec.ID, jobs.Result{JobID: rec.ID, Status: jobs.Achieved, Summary: "done", Iterations: 1}))
	if got := must(reg.GetJob(ctx, rec.ID)); got.Status != jobs.Achieved || got.LastSeq != 6 || got.Result == nil || got.Result.Summary != "done" {
		t.Fatalf("GetJob finished: %+v", got)
	}
	if replayed := must(reg.Events(ctx, rec.ID, 4)); len(replayed) != 2 || replayed[0].Seq != 5 || replayed[1].Seq != 6 {
		t.Fatalf("Events(after=4): %+v", replayed)
	}
	if list := must(reg.QueryJobs(ctx, jobs.Filter{Status: jobs.Achieved, Limit: 10})); len(list) != 1 || list[0].ID != rec.ID {
		t.Fatalf("QueryJobs: %+v", list)
	}
}

type fakeHost struct {
	mu    sync.Mutex
	turns [][]glue.ProviderEvent
}

func (*fakeHost) Session(context.Context, string, ...glue.SessionOption) (*glue.Session, error) {
	return nil, errors.New("unused")
}
func (h *fakeHost) Stream(_ context.Context, _ glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan glue.ProviderEvent, len(h.turns[0]))
	for _, ev := range h.turns[0] {
		ch <- ev
	}
	h.turns = h.turns[1:]
	close(ch)
	return ch, nil
}
func newBackendFixture(t *testing.T) (*daemon.Server, *jobs.Manager, *httptest.Server, string) {
	repo, state := t.TempDir(), t.TempDir()
	must(exec.Command("sh", "-c", "cd "+repo+" && git init -qb main && git -c user.name=t -c user.email=t@e commit --allow-empty -qm init").CombinedOutput())
	turn := func(s string) []glue.ProviderEvent {
		return []glue.ProviderEvent{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventTextDelta, Delta: s}, {Type: glue.ProviderEventDone}}
	}
	prov := &fakeHost{turns: [][]glue.ProviderEvent{
		turn(`{"items":[{"title":"write hello"}]}`),
		{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "call-1", Name: "write_file", Arguments: json.RawMessage(`{"path":"hello.txt","content":"hi"}`)}}, {Type: glue.ProviderEventDone}},
		turn("wrote hello.txt"),
		turn(`{"done":true,"items":[{"title":"write hello","done":true}],"summary":"created hello.txt"}`),
	}}
	mgr := must(jobs.New(jobs.Options{StateDir: state, ProviderFactory: func(string) (glue.Provider, error) { return prov, nil }}))
	srv := must(daemon.New(daemon.Options{Host: prov, Token: "local-token", StateDir: state, Jobs: mgr, KeepaliveInterval: 20 * time.Millisecond}))
	ts := httptest.NewServer(srv)
	t.Cleanup(func() { ts.Close(); _ = srv.Close(); _ = mgr.Close() })
	return srv, mgr, ts, repo
}
func newGatewayFixture(t *testing.T, backendURL string) (*httptest.Server, *jobclient.Client) {
	gw := must(NewServer(Config{StateDir: t.TempDir(), Auth: AuthConfig{Mode: "dev-token", Token: "gw-token"}, Backends: []BackendConfig{{Name: "local", Type: "local", URL: backendURL, Token: "local-token"}}, DefaultBackend: "local"}))
	ts := httptest.NewServer(gw)
	t.Cleanup(func() { ts.Close(); _ = gw.Close() })
	return ts, &jobclient.Client{BaseURL: ts.URL, Token: "gw-token", HTTP: ts.Client()}
}
func verifyJobRun(t *testing.T, cli *jobclient.Client, jobID string, stopOnDone bool) {
	var seqs []int64
	must(0, cli.StreamEvents(t.Context(), jobID, 0, func(ev eventlog.Envelope) error {
		seqs = append(seqs, ev.Seq)
		if ev.Type == "permission_request" {
			var p struct {
				PermissionID string `json:"permission_id"`
			}
			_ = json.Unmarshal([]byte(mustJSON(ev.Payload)), &p)
			must(0, cli.Decide(t.Context(), jobID, p.PermissionID, true, "", "ok"))
		}
		if stopOnDone && ev.Type == "job_done" {
			return jobclient.ErrDone
		}
		return nil
	}))
	for i, seq := range seqs {
		if seq != int64(i+1) || len(seqs) < 4 {
			t.Fatalf("invalid seqs=%v", seqs)
		}
	}
	if res := must(cli.Result(t.Context(), jobID)); res.JobID != jobID || res.Status != jobs.Achieved || res.Summary != "created hello.txt" {
		t.Fatalf("Result: %+v", res)
	}
}

func TestLocalBackendEndToEnd(t *testing.T) {
	_, _, ts, repo := newBackendFixture(t)
	b, ctx, id := NewLocalBackend(BackendConfig{Name: "local", Type: "local", URL: ts.URL, Token: "local-token"}, ts.Client()), t.Context(), "job-abcdef012345"
	if created := must(b.Create(ctx, id, jobs.Spec{Objective: "write hello.txt", Repo: jobs.RepoSpec{Path: repo}})); created.ID != id || must(b.Status(ctx, id)) != jobs.Created {
		t.Fatalf("Create: %+v", created)
	}
	if code, _, err := b.Do(ctx, http.MethodPost, id+"/messages", []byte(`{"text":"note"}`)); err != nil || code != http.StatusAccepted {
		t.Fatalf("Do messages: code=%d err=%v", code, err)
	}
	must(b.Start(ctx, id))
	_ = b.Stream(ctx, id, 0, func(eventlog.Envelope) error { return errors.New("stop") })
	var resumed []int64
	if err := b.Stream(ctx, id, 1, func(ev eventlog.Envelope) error { resumed = append(resumed, ev.Seq); return nil }); err != nil || len(resumed) == 0 || resumed[0] != 2 {
		t.Fatalf("resumed=%v err=%v", resumed, err)
	}
	if res := must(b.Result(ctx, id)); res.JobID != id || res.Status != jobs.Achieved || res.Summary != "created hello.txt" {
		t.Fatalf("Result: %+v", res)
	}
}

func TestGatewaySubmitThroughLocalBackend(t *testing.T) {
	_, mgr, backendTS, repo := newBackendFixture(t)
	gwTS, cli := newGatewayFixture(t, backendTS.URL)
	if resp := must(gwTS.Client().Get(gwTS.URL + "/")); resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("webui GET /: status=%d", resp.StatusCode)
	}
	job := must(cli.Submit(t.Context(), jobs.Spec{Objective: "write hello.txt", Repo: jobs.RepoSpec{Path: repo}, Policy: "ask-all"}, true))
	if !strings.HasPrefix(job.ID, "job-") || job.Spec.Backend != "local" || must(mgr.Get(t.Context(), job.ID)).ID != job.ID {
		t.Fatalf("unexpected submitted job: %+v", job)
	}
	verifyJobRun(t, cli, job.ID, true)
}

func TestGatewayUpstreamDropNoGapNoDup(t *testing.T) {
	srv, mgr, _, repo := newBackendFixture(t)
	var attempts atomic.Int32
	flakyTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") && attempts.Add(1) == 1 {
			evs := must(mgr.Events(r.Context(), strings.Split(r.URL.Path, "/")[3], 0))
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "id: 1\nevent: %s\ndata: %s\n\n", evs[0].Type, mustJSON(evs[0]))
			return
		}
		srv.ServeHTTP(w, r)
	}))
	t.Cleanup(flakyTS.Close)
	_, cli := newGatewayFixture(t, flakyTS.URL)
	job := must(cli.Submit(t.Context(), jobs.Spec{Objective: "write hello.txt", Repo: jobs.RepoSpec{Path: repo}}, true))
	verifyJobRun(t, cli, job.ID, true)
	if attempts.Load() < 2 {
		t.Fatalf("expected upstream reconnect, got attempts=%d", attempts.Load())
	}
}

func TestGatewayServesCachedEventsWhenBackendDown(t *testing.T) {
	_, _, backendTS, repo := newBackendFixture(t)
	_, cli := newGatewayFixture(t, backendTS.URL)
	job := must(cli.Submit(t.Context(), jobs.Spec{Objective: "write hello.txt", Repo: jobs.RepoSpec{Path: repo}}, true))
	verifyJobRun(t, cli, job.ID, true)
	backendTS.Close()
	verifyJobRun(t, cli, job.ID, false)
	if detail := must(cli.Show(t.Context(), job.ID)); detail.Status != jobs.Achieved || detail.Spec.Backend != "local" {
		t.Fatalf("Show with backend down: %+v", detail)
	}
}

type fakeAXHarness struct {
	mu                                             sync.Mutex
	now                                            atomic.Int64
	pendingPerm, decidedID                         string
	suspends, resumes, deletes, routerHits, closes atomic.Int32
	failResult                                     atomic.Bool
	tasks                                          map[string]bool
	onDelete                                       func(string)
	events                                         []eventlog.Envelope
}

func (h *fakeAXHarness) Close() error { h.closes.Add(1); return nil }

func (h *fakeAXHarness) Now() time.Time { return time.Unix(0, h.now.Load()).UTC() }
func (h *fakeAXHarness) Advance(d time.Duration, gw *Server) {
	h.now.Add(int64(d))
	gw.Poke()
}
func (h *fakeAXHarness) emit(pairs ...any) {
	for i := 0; i < len(pairs); i += 2 {
		h.events = append(h.events, eventlog.Envelope{Version: 2, Seq: int64(len(h.events) + 1), Time: h.Now(), Type: pairs[i].(string), Payload: pairs[i+1]})
	}
}
func (h *fakeAXHarness) Create(_ context.Context, id string, spec jobs.Spec) (jobs.Job, error) {
	h.mu.Lock()
	if h.tasks == nil {
		h.tasks = map[string]bool{}
	}
	h.tasks["glue-"+id] = true
	h.mu.Unlock()
	return jobs.Job{ID: id, Spec: spec, Status: jobs.Created, CreatedAt: h.Now()}, nil
}
func (h *fakeAXHarness) Start(ctx context.Context, id string) (jobs.Job, error) {
	_ = h.Resume(ctx, id)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pendingPerm = "perm-1"
	h.emit("job_created", jobs.Spec{Objective: "ax test"}, "job_started", nil, "permission_request", map[string]any{"permission_id": "perm-1", "request": glue.PermissionRequest{Tool: "write_file", Action: "write_file", Target: "hello.txt"}}, "job_waiting", nil)
	return jobs.Job{ID: id, Status: jobs.Running}, nil
}
func (h *fakeAXHarness) Suspend(context.Context, string) error { h.suspends.Add(1); return nil }
func (h *fakeAXHarness) Resume(context.Context, string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.resumes.Add(1) > 1 && h.pendingPerm == "perm-1" {
		h.pendingPerm = "perm-2"
		h.emit("permission_expired", map[string]any{"permission_id": "perm-1"}, "job_resumed", map[string]any{"from_iteration": 1, "reason": "restart"}, "permission_request", map[string]any{"permission_id": "perm-2", "request": glue.PermissionRequest{Tool: "write_file", Action: "write_file", Target: "hello.txt"}}, "job_waiting", nil)
	}
	return nil
}
func (h *fakeAXHarness) Stream(_ context.Context, _ string, after int64, fn func(eventlog.Envelope) error) error {
	h.routerHits.Add(1)
	h.mu.Lock()
	evs := append([]eventlog.Envelope(nil), h.events...)
	h.mu.Unlock()
	for _, ev := range evs {
		if ev.Seq > after && fn(ev) != nil {
			break
		}
	}
	return nil
}
func (h *fakeAXHarness) Do(_ context.Context, _, subpath string, _ []byte) (int, []byte, error) {
	h.routerHits.Add(1)
	h.mu.Lock()
	defer h.mu.Unlock()
	if permID := strings.Split(subpath, "/")[2]; permID == h.pendingPerm {
		h.decidedID, h.pendingPerm = permID, ""
		h.emit("permission_decision", map[string]any{"permission_id": permID, "decision": glue.PermissionDecision{Allow: true}}, "job_done", map[string]any{"status": jobs.Achieved, "result": glue.GoalResult{Iterations: 1, Summary: "done"}})
		return http.StatusOK, []byte(`{"ok":true}`), nil
	}
	return http.StatusConflict, []byte(`{"error":{"message":"stale permission"}}`), nil
}
func (h *fakeAXHarness) Result(_ context.Context, id string) (jobs.Result, error) {
	h.routerHits.Add(1)
	h.mu.Lock()
	alive := h.tasks["glue-"+id]
	h.mu.Unlock()
	if h.failResult.Load() || !alive {
		return jobs.Result{}, errors.New("workspace unavailable")
	}
	return jobs.Result{JobID: id, Status: jobs.Achieved, Summary: "done", Iterations: 1}, nil
}
func (h *fakeAXHarness) Cancel(context.Context, string) error { h.routerHits.Add(1); return nil }
func (h *fakeAXHarness) Status(context.Context, string) (jobs.Status, error) {
	return jobs.Running, nil
}
func (h *fakeAXHarness) DeleteTask(_ context.Context, id string) error {
	h.deletes.Add(1)
	h.mu.Lock()
	delete(h.tasks, "glue-"+id)
	fn := h.onDelete
	h.mu.Unlock()
	if fn != nil {
		fn(id)
	}
	return nil
}
func (h *fakeAXHarness) ListTasks(context.Context) ([]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for name := range h.tasks {
		out = append(out, name)
	}
	return out, nil
}

func waitJob(t *testing.T, gw *Server, jobID string, st jobs.Status, reason string) Record {
	t.Helper()
	for range 100 {
		if rec, err := gw.reg.GetJob(t.Context(), jobID); err == nil && rec.Status == st && (reason == "" || rec.StatusReason == reason) {
			return rec
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach status=%s reason=%s", jobID, st, reason)
	return Record{}
}

func newAXGatewayHarness(t *testing.T, suspendIdle bool) (*fakeAXHarness, *Server, *httptest.Server, *jobclient.Client, jobs.Job) {
	t.Helper()
	h := &fakeAXHarness{}
	h.now.Store(time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC).UnixNano())
	gw := must(NewServer(Config{StateDir: t.TempDir(), Auth: AuthConfig{Mode: "dev-token", Token: "gw-token"}, SuspendAfter: 2 * time.Minute, ResumeTimeout: 5 * time.Second, Now: h.Now, Backends: []BackendConfig{{Name: "ax", Type: "ax", Image: "localhost:5001/glue-task@sha256:11"}}, CustomBackends: map[string]Backend{"ax": h}}))
	ts := httptest.NewServer(gw)
	t.Cleanup(func() { ts.Close(); _ = gw.Close() })
	cli := &jobclient.Client{BaseURL: ts.URL, Token: "gw-token", HTTP: ts.Client()}
	job := must(cli.Submit(t.Context(), jobs.Spec{Objective: "ax test", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}, Policy: "ask-all"}, true))
	waitJob(t, gw, job.ID, jobs.WaitingForInput, "")
	if suspendIdle {
		h.Advance(2*time.Minute+time.Second, gw)
		waitJob(t, gw, job.ID, jobs.Suspended, "waiting_for_input")
	}
	return h, gw, ts, cli, job
}

func TestSuspendAfterIdleWaiting(t *testing.T) {
	h, gw, ts, _, job := newAXGatewayHarness(t, true)
	if evs := must(gw.reg.Events(t.Context(), job.ID, 0)); h.suspends.Load() != 1 || evs[len(evs)-1].Type != "task_suspended" || !strings.Contains(mustJSON(evs[len(evs)-1].Payload), "waiting_for_input") {
		t.Fatalf("suspends=%d evs=%+v", h.suspends.Load(), evs)
	}
	ts.Close()
	must(0, gw.Close())
	if h.closes.Load() == 0 {
		t.Fatal("expected Server.Close to close io.Closer backends")
	}
}

func TestNoSuspendWhileLiveViewerAttached(t *testing.T) {
	h, gw, _, cli, job := newAXGatewayHarness(t, false)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	attached := make(chan struct{})
	go func() {
		_ = cli.StreamEvents(ctx, job.ID, 0, func(ev eventlog.Envelope) error {
			if ev.Type == "job_waiting" {
				close(attached)
			}
			return nil
		})
	}()
	<-attached
	h.Advance(5*time.Minute, gw)
	time.Sleep(30 * time.Millisecond)
	if d := must(cli.Show(t.Context(), job.ID)); d.Status != jobs.WaitingForInput || h.suspends.Load() != 0 {
		t.Fatalf("expected job to stay waiting_for_input with viewer attached, got status=%s suspends=%d", d.Status, h.suspends.Load())
	}
	cancel()
	time.Sleep(30 * time.Millisecond)
	h.Advance(2*time.Minute+time.Second, gw)
	waitJob(t, gw, job.ID, jobs.Suspended, "waiting_for_input")
}

func TestReadOnlyViewsDoNotResume(t *testing.T) {
	h, _, ts, cli, job := newAXGatewayHarness(t, true)
	h.routerHits.Store(0)
	resumesBefore := h.resumes.Load()
	if list, d, perms := must(cli.List(t.Context(), jobs.Filter{})), must(cli.Show(t.Context(), job.ID)), must(cli.Permissions(t.Context(), job.ID, "pending")); len(list) != 1 || list[0].Status != jobs.Suspended || d.Status != jobs.Suspended || len(perms) != 1 {
		t.Fatalf("list=%+v d=%+v perms=%+v", list, d, perms)
	}
	for _, p := range []string{"?cached=1", ""} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/v1/jobs/"+job.ID+"/events"+p, nil)
		req.Header.Set("Authorization", "Bearer gw-token")
		if resp := must(ts.Client().Do(req)); resp.Body.Close() != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET events%s: %d", p, resp.StatusCode)
		}
	}
	if hits, r := h.routerHits.Load(), h.resumes.Load()-resumesBefore; hits != 0 || r != 0 {
		t.Fatalf("routerHits=%d resumes=%d, want 0/0", hits, r)
	}
}

func TestActionResumesThenForwardsDecision(t *testing.T) {
	h, gw, _, cli, job := newAXGatewayHarness(t, true)
	must(0, cli.Decide(t.Context(), job.ID, "perm-1", true, "session", "approved after suspend"))
	if h.decidedID != "perm-2" {
		t.Fatalf("decidedID = %q, want re-issued perm-2", h.decidedID)
	}
	waitJob(t, gw, job.ID, jobs.Achieved, "finished")
	evs := must(gw.reg.Events(t.Context(), job.ID, 0))
	for i, ev := range evs {
		if ev.Seq != int64(i+1) {
			t.Fatalf("non-continuous seq at %d: got %d", i, ev.Seq)
		}
	}
	all := mustJSON(evs)
	for _, want := range []string{"task_suspended", "task_resumed", "job_resumed", `"permission_id":"perm-2"`, `"original_permission_id":"perm-1"`, "job_done"} {
		if !strings.Contains(all, want) {
			t.Fatalf("missing %q in %s", want, all)
		}
	}
}

func TestSuspendOnFinish(t *testing.T) {
	h, gw, _, cli, job := newAXGatewayHarness(t, false)
	must(0, cli.Decide(t.Context(), job.ID, "perm-1", true, "", "ok"))
	rec := waitJob(t, gw, job.ID, jobs.Achieved, "finished")
	if h.suspends.Load() != 1 || rec.Result == nil || rec.Result.Summary != "done" {
		t.Fatalf("unexpected finished state: suspends=%d rec=%+v", h.suspends.Load(), rec)
	}
	h.routerHits.Store(0)
	if res := must(cli.Result(t.Context(), job.ID)); res.Status != jobs.Achieved || h.routerHits.Load() != 0 {
		t.Fatalf("Result: %+v routerHits=%d", res, h.routerHits.Load())
	}
}

func TestResultStoredBeforeTaskDeletion(t *testing.T) {
	h, gw, _, cli, job := newAXGatewayHarness(t, false)
	h.failResult.Store(true)
	must(0, cli.Decide(t.Context(), job.ID, "perm-1", true, "", "ok"))
	if rec := waitJob(t, gw, job.ID, jobs.Achieved, "finished"); rec.Result != nil {
		t.Fatalf("expected nil Result while failResult=true, got %+v", rec.Result)
	}
	var storedBeforeDelete bool
	h.onDelete = func(id string) {
		rec := must(gw.reg.GetJob(t.Context(), id))
		storedBeforeDelete = rec.Result != nil && rec.Result.Summary == "done"
	}
	if err := gw.deleteJobTask(t.Context(), job.ID); err == nil || h.deletes.Load() != 0 {
		t.Fatalf("deleteJobTask should abort when Result cannot be saved: err=%v deletes=%d", err, h.deletes.Load())
	}
	h.failResult.Store(false)
	must(0, gw.deleteJobTask(t.Context(), job.ID))
	if !storedBeforeDelete || h.deletes.Load() != 1 {
		t.Fatalf("storedBeforeDelete=%v deletes=%d", storedBeforeDelete, h.deletes.Load())
	}
	if res := must(cli.Result(t.Context(), job.ID)); res.Status != jobs.Achieved || res.Summary != "done" {
		t.Fatalf("Result after task deletion: %+v", res)
	}
}

func TestRetentionDeletesTaskKeepsRecord(t *testing.T) {
	h, gw, _, cli, job := newAXGatewayHarness(t, false)
	must(0, cli.Decide(t.Context(), job.ID, "perm-1", true, "", "ok"))
	waitJob(t, gw, job.ID, jobs.Achieved, "finished")
	h.Advance(167*time.Hour, gw)
	if d := must(cli.Show(t.Context(), job.ID)); d.TaskDeleted || h.deletes.Load() != 0 {
		t.Fatalf("deleted too early: TaskDeleted=%v deletes=%d", d.TaskDeleted, h.deletes.Load())
	}
	h.Advance(2*time.Hour, gw)
	d := must(cli.Show(t.Context(), job.ID))
	if !d.TaskDeleted || d.Status != jobs.Achieved || h.deletes.Load() != 1 {
		t.Fatalf("expected task_deleted=true after 168h: detail=%+v deletes=%d", d, h.deletes.Load())
	}
	h.routerHits.Store(0)
	var replayed int
	must(0, cli.StreamEvents(t.Context(), job.ID, 0, func(ev eventlog.Envelope) error { replayed++; return nil }))
	if res := must(cli.Result(t.Context(), job.ID)); res.Summary != "done" || replayed < 5 || h.routerHits.Load() != 0 {
		t.Fatalf("res=%+v replayed=%d routerHits=%d", res, replayed, h.routerHits.Load())
	}
}

func TestPurgeRemovesEverything(t *testing.T) {
	h, gw, _, cli, job := newAXGatewayHarness(t, false)
	must(0, cli.Decide(t.Context(), job.ID, "perm-1", true, "", "ok"))
	waitJob(t, gw, job.ID, jobs.Achieved, "finished")
	must(0, cli.Delete(t.Context(), job.ID))
	if h.deletes.Load() != 1 || len(must(cli.List(t.Context(), jobs.Filter{}))) != 0 {
		t.Fatalf("deletes=%d list not empty", h.deletes.Load())
	}
	var se *jobclient.StatusError
	if _, err := cli.Show(t.Context(), job.ID); !errors.As(err, &se) || se.Code != http.StatusNotFound {
		t.Fatalf("Show after purge: err=%v", err)
	}
	if _, err := cli.Result(t.Context(), job.ID); !errors.As(err, &se) || se.Code != http.StatusNotFound {
		t.Fatalf("Result after purge: err=%v", err)
	}
	if _, err := gw.reg.Events(t.Context(), job.ID, 0); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Events after purge: err=%v", err)
	}
}

func TestOrphanTasksReportedNotDeleted(t *testing.T) {
	stateDir := t.TempDir()
	reg := must(OpenRegistry(stateDir))
	must(reg.CreateJob(t.Context(), Record{ID: "job-known0000002", Backend: "ax", BackendRef: "job-known0000002", Owner: "dev", Spec: jobs.Spec{Objective: "known"}, Status: jobs.Running}))
	_ = reg.Close()
	h := &fakeAXHarness{tasks: map[string]bool{"glue-job-orphan000001": true, "glue-job-known0000002": true, "unrelated-task": true}}
	var logBuf bytes.Buffer
	origOut := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(origOut) })
	gw := must(NewServer(Config{StateDir: stateDir, Auth: AuthConfig{Mode: "dev-token", Token: "gw-token"}, Backends: []BackendConfig{{Name: "ax", Type: "ax", Image: "localhost:5001/glue-task@sha256:11"}}, CustomBackends: map[string]Backend{"ax": h}}))
	ts := httptest.NewServer(gw)
	t.Cleanup(func() { ts.Close(); _ = gw.Close() })
	if !strings.Contains(logBuf.String(), "orphan task glue-job-orphan000001") || h.deletes.Load() != 0 {
		t.Fatalf("log=%q deletes=%d", logBuf.String(), h.deletes.Load())
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/v1/diagnostics", nil)
	req.Header.Set("Authorization", "Bearer gw-token")
	resp := must(ts.Client().Do(req))
	defer resp.Body.Close()
	var diag struct {
		OrphanTasks []string `json:"orphan_tasks"`
	}
	must(0, json.NewDecoder(resp.Body).Decode(&diag))
	if resp.StatusCode != http.StatusOK || !reflect.DeepEqual(diag.OrphanTasks, []string{"glue-job-orphan000001"}) || h.deletes.Load() != 0 {
		t.Fatalf("status=%d diag=%+v deletes=%d", resp.StatusCode, diag, h.deletes.Load())
	}
}

func newOAuthGatewayFixture(t *testing.T, login string, allowed []string) (*fakeAXHarness, *Server, *httptest.Server, *http.Client) {
	t.Helper()
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/login/oauth/access_token" && r.FormValue("client_id") == "cid" && r.FormValue("client_secret") == "csec" && r.FormValue("code") == "code-1" {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "gho_test_token"})
		} else if r.URL.Path == "/user" && r.Header.Get("Authorization") == "Bearer gho_test_token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 4242, "login": login})
		} else {
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(gh.Close)
	h := &fakeAXHarness{}
	h.now.Store(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC).UnixNano())
	gw := must(NewServer(Config{
		PublicURL: "https://gw.example.com", StateDir: t.TempDir(), Now: h.Now,
		Auth:           AuthConfig{Mode: "github", ClientID: "cid", ClientSecret: "csec", AllowedLogins: allowed, AuthorizeURL: gh.URL + "/login/oauth/authorize", TokenURL: gh.URL + "/login/oauth/access_token", UserURL: gh.URL + "/user", RateLimitPerMin: 5},
		Backends:       []BackendConfig{{Name: "ax", Type: "ax", Image: "localhost:5001/glue-task@sha256:11"}},
		CustomBackends: map[string]Backend{"ax": h},
	}))
	ts := httptest.NewServer(gw)
	t.Cleanup(func() { ts.Close(); _ = gw.Close() })
	return h, gw, ts, &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func doCookie(t *testing.T, cli *http.Client, method, rawURL string, c *http.Cookie, csrf, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), method, rawURL, strings.NewReader(body))
	if c != nil {
		req.AddCookie(c)
	}
	if csrf != "" {
		req.Header.Set("X-Glue-CSRF", csrf)
	}
	return must(cli.Do(req))
}
func oauthLogin(t *testing.T, ts *httptest.Server, cli *http.Client) (*http.Response, *http.Cookie, string) {
	t.Helper()
	r1 := must(cli.Get(ts.URL + "/auth/github/login"))
	r1.Body.Close()
	u := must(url.Parse(r1.Header.Get("Location")))
	if r1.StatusCode != http.StatusFound || u.Query().Get("redirect_uri") != "https://gw.example.com/auth/github/callback" || len(r1.Cookies()) == 0 {
		t.Fatalf("login redirect: status=%d loc=%s cookies=%v", r1.StatusCode, r1.Header.Get("Location"), r1.Cookies())
	}
	r2 := doCookie(t, cli, http.MethodGet, ts.URL+"/auth/github/callback?code=code-1&state="+u.Query().Get("state"), r1.Cookies()[0], "", "")
	r2.Body.Close()
	for _, c := range r2.Cookies() {
		if c.Name == "glue_session" && c.Value != "" {
			sresp := doCookie(t, cli, http.MethodGet, ts.URL+"/v1/session", c, "", "")
			defer sresp.Body.Close()
			var body struct {
				CSRFToken string `json:"csrf_token"`
			}
			_ = json.NewDecoder(sresp.Body).Decode(&body)
			return r2, c, body.CSRFToken
		}
	}
	return r2, nil, ""
}

func TestOAuthAllowlistedLoginSucceeds(t *testing.T) {
	_, _, ts, cli := newOAuthGatewayFixture(t, "alice", []string{"alice"})
	resp, c, csrf := oauthLogin(t, ts, cli)
	if resp.StatusCode != http.StatusFound || c == nil || len(c.Value) != 64 || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || csrf == "" {
		t.Fatalf("oauth callback: status=%d cookie=%+v csrf=%q", resp.StatusCode, c, csrf)
	}
}

func TestOAuthNonAllowlistedRejected(t *testing.T) {
	_, _, ts, cli := newOAuthGatewayFixture(t, "mallory", []string{"alice"})
	if resp, c, _ := oauthLogin(t, ts, cli); resp.StatusCode != http.StatusForbidden || c != nil {
		t.Fatalf("expected 403 and no session cookie, got status=%d cookie=%+v", resp.StatusCode, c)
	}
}

func doCookieStatus(t *testing.T, cli *http.Client, method, rawURL string, c *http.Cookie, csrf, body string) int {
	resp := doCookie(t, cli, method, rawURL, c, csrf, body)
	resp.Body.Close()
	return resp.StatusCode
}
func TestOAuthStateMismatchRejected(t *testing.T) {
	_, _, ts, cli := newOAuthGatewayFixture(t, "alice", []string{"alice"})
	r1 := must(cli.Get(ts.URL + "/auth/github/login"))
	r1.Body.Close()
	if st := doCookieStatus(t, cli, http.MethodGet, ts.URL+"/auth/github/callback?code=code-1&state=tampered", r1.Cookies()[0], "", ""); st != http.StatusBadRequest {
		t.Fatalf("expected 400 on state mismatch, got %d", st)
	}
}

func TestSessionExpiry(t *testing.T) {
	h, gw, ts, cli := newOAuthGatewayFixture(t, "alice", []string{"alice"})
	_, c, _ := oauthLogin(t, ts, cli)
	check := func() int { return doCookieStatus(t, cli, http.MethodGet, ts.URL+"/v1/session", c, "", "") }
	h.Advance(23*time.Hour, gw)
	if st := check(); st != http.StatusOK {
		t.Fatalf("before idle TTL: status=%d", st)
	}
	h.Advance(25*time.Hour, gw)
	if st := check(); st != http.StatusUnauthorized {
		t.Fatalf("after 24h idle TTL: status=%d, want 401", st)
	}
	_, c, _ = oauthLogin(t, ts, cli)
	for range 8 {
		h.Advance(20*time.Hour, gw)
		_ = check()
	}
	if check() != http.StatusOK {
		t.Fatal("expected 200 before 7d absolute TTL")
	}
	h.Advance(10*time.Hour, gw)
	if st := check(); st != http.StatusUnauthorized {
		t.Fatalf("after 7d absolute TTL: status=%d, want 401", st)
	}
}

func TestLogout(t *testing.T) {
	_, _, ts, cli := newOAuthGatewayFixture(t, "alice", []string{"alice"})
	_, c, csrf := oauthLogin(t, ts, cli)
	if doCookieStatus(t, cli, http.MethodPost, ts.URL+"/v1/session/logout", c, csrf, "") != http.StatusOK || doCookieStatus(t, cli, http.MethodGet, ts.URL+"/v1/session", c, "", "") != http.StatusUnauthorized {
		t.Fatal("unexpected logout statuses")
	}
}

func TestCSRFRequiredForCookieAuth(t *testing.T) {
	_, _, ts, cli := newOAuthGatewayFixture(t, "alice", []string{"alice"})
	_, c, csrf := oauthLogin(t, ts, cli)
	post := func(hdr string) int {
		return doCookieStatus(t, cli, http.MethodPost, ts.URL+"/v1/jobs", c, hdr, `{"objective":"csrf","repo":{"url":"https://github.com/erain/glue-sandbox"}}`)
	}
	if post("") != http.StatusForbidden || post("wrong") != http.StatusForbidden || post(csrf) != http.StatusCreated {
		t.Fatal("unexpected CSRF statuses")
	}
}

func TestAuthRateLimit(t *testing.T) {
	_, _, ts, cli := newOAuthGatewayFixture(t, "alice", []string{"alice"})
	for range 5 {
		_ = doCookieStatus(t, cli, http.MethodGet, ts.URL+"/auth/github/login", nil, "", "")
	}
	if st := doCookieStatus(t, cli, http.MethodGet, ts.URL+"/auth/github/login", nil, "", ""); st != http.StatusTooManyRequests {
		t.Fatalf("expected 429 on 6th auth request, got %d", st)
	}
}

func TestAPITokenCreateUseRevoke(t *testing.T) {
	_, _, ts, cli := newOAuthGatewayFixture(t, "alice", []string{"alice"})
	_, c, csrf := oauthLogin(t, ts, cli)
	cresp := doCookie(t, cli, http.MethodPost, ts.URL+"/v1/tokens", c, csrf, `{"name":"laptop"}`)
	defer cresp.Body.Close()
	var created APIToken
	must(0, json.NewDecoder(cresp.Body).Decode(&created))
	if cresp.StatusCode != http.StatusCreated || !strings.HasPrefix(created.Token, "glue_pat_") || created.Name != "laptop" || created.Owner != "alice" {
		t.Fatalf("create token: status=%d tok=%+v", cresp.StatusCode, created)
	}
	lresp := doCookie(t, cli, http.MethodGet, ts.URL+"/v1/tokens", c, "", "")
	defer lresp.Body.Close()
	var listed struct{ Tokens []APIToken }
	must(0, json.NewDecoder(lresp.Body).Decode(&listed))
	if len(listed.Tokens) != 1 || listed.Tokens[0].ID != created.ID || listed.Tokens[0].Token != "" {
		t.Fatalf("list tokens: %+v", listed.Tokens)
	}
	patCli := &jobclient.Client{BaseURL: ts.URL, Token: created.Token, HTTP: ts.Client()}
	job := must(patCli.Submit(t.Context(), jobs.Spec{Objective: "pat test", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}}, false))
	if job.ID == "" {
		t.Fatal("expected job created with PAT")
	}
	if st := doCookieStatus(t, cli, http.MethodDelete, ts.URL+"/v1/tokens/"+created.ID, c, csrf, ""); st != http.StatusOK {
		t.Fatalf("revoke status=%d", st)
	}
	if _, err := patCli.List(t.Context(), jobs.Filter{}); err == nil {
		t.Fatal("expected revoked token to fail")
	}
}

func TestTokensStoredHashed(t *testing.T) {
	_, gw, ts, cli := newOAuthGatewayFixture(t, "alice", []string{"alice"})
	_, c, csrf := oauthLogin(t, ts, cli)
	cresp := doCookie(t, cli, http.MethodPost, ts.URL+"/v1/tokens", c, csrf, `{"name":"hashed"}`)
	defer cresp.Body.Close()
	var created APIToken
	must(0, json.NewDecoder(cresp.Body).Decode(&created))
	var storedHash string
	must(0, gw.reg.store.DB.QueryRowContext(t.Context(), "SELECT token_hash FROM api_tokens WHERE id=?", created.ID).Scan(&storedHash))
	if storedHash == "" || storedHash == created.Token || storedHash != hashToken(created.Token) {
		t.Fatalf("storedHash=%q want sha256(%q)=%q", storedHash, created.Token, hashToken(created.Token))
	}
}

func TestJobsScopedToOwner(t *testing.T) {
	_, gw, ts, _ := newOAuthGatewayFixture(t, "alice", []string{"alice", "bob"})
	aliceTok := must(gw.reg.CreateToken(t.Context(), "alice", "alice-cli"))
	bobTok := must(gw.reg.CreateToken(t.Context(), "bob", "bob-cli"))
	aliceCli := &jobclient.Client{BaseURL: ts.URL, Token: aliceTok.Token, HTTP: ts.Client()}
	bobCli := &jobclient.Client{BaseURL: ts.URL, Token: bobTok.Token, HTTP: ts.Client()}
	ajob := must(aliceCli.Submit(t.Context(), jobs.Spec{Objective: "alice job", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}}, false))
	bjob := must(bobCli.Submit(t.Context(), jobs.Spec{Objective: "bob job", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}}, false))
	if alist := must(aliceCli.List(t.Context(), jobs.Filter{})); len(alist) != 1 || alist[0].ID != ajob.ID {
		t.Fatalf("alice list=%+v, want only %s", alist, ajob.ID)
	}
	if blist := must(bobCli.List(t.Context(), jobs.Filter{})); len(blist) != 1 || blist[0].ID != bjob.ID {
		t.Fatalf("bob list=%+v, want only %s", blist, bjob.ID)
	}
	var se *jobclient.StatusError
	if _, err := bobCli.Show(t.Context(), ajob.ID); !errors.As(err, &se) || se.Code != http.StatusNotFound {
		t.Fatalf("bob Show(alice job) err=%v, want 404", err)
	}
	if err := bobCli.Cancel(t.Context(), ajob.ID); !errors.As(err, &se) || se.Code != http.StatusNotFound {
		t.Fatalf("bob Cancel(alice job) err=%v, want 404", err)
	}
}

func TestUnauthenticatedRouteAllowlist(t *testing.T) {
	_, gw, ts, cli := newOAuthGatewayFixture(t, "alice", []string{"alice"})
	allowlist := map[string]bool{
		"GET /": true, "GET /index.html": true, "GET /static/app.js": true, "GET /static/api.js": true, "GET /static/app.css": true,
		"GET /healthz": true, "GET /auth/github/login": true, "GET /auth/github/callback": true,
	}
	rpl := strings.NewReplacer("{id}", "job-000000000001", "{permID}", "perm-1")
	for _, rt := range gw.Routes() {
		key := rt.Method + " " + rt.Path
		st := doCookieStatus(t, cli, rt.Method, ts.URL+rpl.Replace(rt.Path), nil, "", "")
		if (st != http.StatusUnauthorized) != allowlist[key] || rt.Public != allowlist[key] {
			t.Fatalf("route %s: status=%d public=%v allowlist=%v", key, st, rt.Public, allowlist[key])
		}
	}
}

func TestPublicHardeningHSTSAndForwardedProto(t *testing.T) {
	_, _, httpsTS, httpsCli := newOAuthGatewayFixture(t, "alice", []string{"alice"})
	resp := must(httpsCli.Get(httpsTS.URL + "/"))
	resp.Body.Close()
	if got := resp.Header.Get("Strict-Transport-Security"); got != HSTSHeader {
		t.Fatalf("https public_url HSTS = %q, want %q", got, HSTSHeader)
	}
	for _, trust := range []bool{false, true} {
		gw := must(NewServer(Config{
			StateDir: t.TempDir(), TrustForwardedProto: trust,
			Auth:           AuthConfig{Mode: "dev-token", Token: "gw-token"},
			Backends:       []BackendConfig{{Name: "ax", Type: "ax", Image: "localhost:5001/glue-task@sha256:11"}},
			CustomBackends: map[string]Backend{"ax": &fakeAXHarness{}},
		}))
		ts := httptest.NewServer(gw)
		t.Cleanup(func() { ts.Close(); _ = gw.Close() })
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/v1/session/login", strings.NewReader(`{"token":"gw-token"}`))
		req.Header.Set("X-Forwarded-Proto", "https")
		r := must(ts.Client().Do(req))
		r.Body.Close()
		if (r.Header.Get("Strict-Transport-Security") == HSTSHeader) != trust || len(r.Cookies()) == 0 || r.Cookies()[0].Secure != trust {
			t.Fatalf("trust=%v: HSTS=%q cookies=%+v", trust, r.Header.Get("Strict-Transport-Security"), r.Cookies())
		}
	}
}

func TestPublicHardeningBodyLimitsAndTimeouts(t *testing.T) {
	_, gw, ts, cli := newOAuthGatewayFixture(t, "alice", []string{"alice"})
	httpSrv := NewHTTPServer("127.0.0.1:0", gw)
	if httpSrv.ReadHeaderTimeout != 10*time.Second || httpSrv.ReadTimeout != 30*time.Second || httpSrv.IdleTimeout != 120*time.Second {
		t.Fatalf("unexpected server timeouts: %+v", httpSrv)
	}
	hresp := must(cli.Get(ts.URL + "/healthz"))
	defer hresp.Body.Close()
	var hbody map[string]any
	must(0, json.NewDecoder(hresp.Body).Decode(&hbody))
	if hresp.StatusCode != http.StatusOK || !reflect.DeepEqual(hbody, map[string]any{"ok": true}) {
		t.Fatalf("/healthz status=%d body=%+v", hresp.StatusCode, hbody)
	}
	tok := must(gw.reg.CreateToken(t.Context(), "alice", "cli"))
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/v1/jobs", strings.NewReader(`{"objective":"`+strings.Repeat("x", DefaultMaxBodyBytes+64)+`"}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	resp := must(cli.Do(req))
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized POST /v1/jobs status=%d, want 413", resp.StatusCode)
	}
}

func TestStructuredAccessLogRedactsSecrets(t *testing.T) {
	var logBuf bytes.Buffer
	origOut := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(origOut) })
	_, gw, ts, cli := newOAuthGatewayFixture(t, "alice", []string{"alice"})
	_, cookie, _ := oauthLogin(t, ts, cli)
	tok := must(gw.reg.CreateToken(t.Context(), "alice", "secret-check"))
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/v1/jobs?token="+tok.Token, nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	must(cli.Do(req)).Body.Close()
	logs := logBuf.String()
	for _, want := range []string{"gateway: access method=GET path=/auth/github/login status=302", "gateway: access method=GET path=/auth/github/callback status=302", "gateway: access method=GET path=/v1/jobs status=200"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("missing %q in access logs:\n%s", want, logs)
		}
	}
	for _, secret := range []string{tok.Token, cookie.Value, "code-1", "csec", "gho_test_token"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("secret %q leaked in access logs:\n%s", secret, logs)
		}
	}
}
