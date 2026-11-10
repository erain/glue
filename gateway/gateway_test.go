// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/internal/eventlog"
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
