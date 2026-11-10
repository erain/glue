// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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
	cfgPath := filepath.Join(t.TempDir(), "gateway.yaml")
	raw := "public_url: https://gw.example.com/\nstate_dir: /var/lib/glue-gateway\nauth:\n  mode: dev-token\nbackends:\n  - name: local\n    type: local\n    url: http://127.0.0.1:8787/\n    token: backend-token\n"
	must(0, os.WriteFile(cfgPath, []byte(raw), 0o600))
	if cfg := must(LoadConfig(cfgPath)); cfg.Listen != "127.0.0.1:8800" || cfg.PublicURL != "https://gw.example.com" || cfg.Auth.Token != "env-secret" || cfg.DefaultBackend != "local" || cfg.Backends[0].URL != "http://127.0.0.1:8787" {
		t.Fatalf("unexpected normalized config: %+v", cfg)
	}
	t.Setenv("GLUE_GATEWAY_TOKEN", "")
	for _, tc := range []struct{ yaml, want string }{
		{"unknown_key: 1\n", "field unknown_key"},
		{"auth: {mode: dev-token, token: t}\nbackends: [{name: l, type: local, url: http://x, token: t}]\n", "state_dir=\"\""},
		{"state_dir: /tmp\nauth: {mode: github, token: t}\nbackends: [{name: l, type: local, url: http://x, token: t}]\n", "auth.mode=\"github\""},
		{"state_dir: /tmp\nauth: {mode: dev-token}\nbackends: [{name: l, type: local, url: http://x, token: t}]\n", "has_token=false"},
		{"state_dir: /tmp\nauth: {mode: dev-token, token: t}\n", "backends=0"},
		{"state_dir: /tmp\nauth: {mode: dev-token, token: t}\nbackends: [{name: l, type: local, url: http://x, token: t}, {name: l, type: local, url: http://y, token: t}]\n", "duplicate=true"},
		{"state_dir: /tmp\nauth: {mode: dev-token, token: t}\nbackends: [{name: l, type: ax, url: http://x, token: t}]\n", "type=\"ax\""},
		{"state_dir: /tmp\nauth: {mode: dev-token, token: t}\ndefault_backend: other\nbackends: [{name: l, type: local, url: http://x, token: t}]\n", "default_backend"},
	} {
		if _, err := ParseConfig([]byte(tc.yaml)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("got err=%v, want substring %q", err, tc.want)
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
