// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
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

	"github.com/erain/glue"
	"github.com/erain/glue/daemon"
	"github.com/erain/glue/internal/eventlog"
	"github.com/erain/glue/internal/jobclient"
	"github.com/erain/glue/jobs"
	filestore "github.com/erain/glue/stores/file"
)

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
func newTestJobDaemon(t *testing.T, provider glue.Provider) (*httptest.Server, *jobs.Manager) {
	t.Helper()
	stateDir := t.TempDir()
	mgr := must(jobs.New(jobs.Options{StateDir: stateDir, ProviderFactory: fakeFactory(provider)}))
	t.Cleanup(func() { _ = mgr.Close() })
	host := glue.NewAgent(glue.AgentOptions{Provider: provider, Model: "fake", Store: filestore.New(t.TempDir())})
	srv := must(daemon.New(daemon.Options{Host: host, Token: "job-token", StateDir: stateDir, Jobs: mgr}))
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, mgr
}
func runJobCLI(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), append([]string{"job"}, args...), strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), serveDaemon, nil)
	return stdout.String(), stderr.String(), code
}
func mustJobCLI(t *testing.T, args ...string) string {
	t.Helper()
	out, errOut, code := runJobCLI(t, args...)
	if code != 0 {
		t.Fatalf("glue job %v failed (code %d): %s", args, code, errOut)
	}
	return out
}
func TestJobLoginAndServerResolution(t *testing.T) {
	for _, k := range []string{"XDG_CONFIG_HOME", "GLUE_CONFIG_DIR", "GLUE_SERVER", "GLUE_TOKEN", "GLUE_DAEMON_TOKEN"} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", t.TempDir())
	mustJobCLI(t, "login", "--server", "https://gateway.example.test/", "--token", "gw-secret-token")
	if info := must(os.Stat(jobclient.ServersPath())); info.Mode().Perm() != 0o600 {
		t.Fatalf("servers.json mode=%v", info.Mode().Perm())
	}
	if cfg := must(jobclient.ResolveConfig(jobclient.Config{ServerURL: "https://gateway.example.test"})); cfg.Token != "gw-secret-token" {
		t.Fatalf("ResolveConfig from servers.json = %+v", cfg)
	}
	metaPath := filepath.Join(t.TempDir(), "daemon.json")
	if err := daemon.WriteMetadata(metaPath, daemon.Metadata{Version: 1, BaseURL: "http://127.0.0.1:43210", Token: "meta-token", PID: 1}); err != nil {
		t.Fatal(err)
	}
	if cfg := must(jobclient.ResolveConfig(jobclient.Config{MetadataPath: metaPath})); cfg.ServerURL != "http://127.0.0.1:43210" || cfg.Token != "meta-token" {
		t.Fatalf("ResolveConfig from metadata = %+v", cfg)
	}
}
func TestJobCLIControlAndResultFlow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := serveJobRepo(t)
	p := &scriptedProvider{turns: [][]glue.ProviderEvent{
		textTurn(`{"items":[{"title":"add Makefile"}]}`),
		toolCallTurn("call-1", "write_file", `{"path":"Makefile","content":"test:\n\t@true\n"}`),
		toolCallTurn("call-2", "write_file", `{"path":"extra.txt","content":"skip\n"}`),
		textTurn("wrote Makefile"),
		textTurn(`{"done":false,"items":[{"title":"add Makefile","done":false}],"summary":"waiting for confirmation"}`),
		textTurn("confirmed Makefile"),
		textTurn(`{"done":true,"items":[{"title":"add Makefile","done":true,"evidence":"Makefile present"}],"summary":"Add Makefile test target"}`),
	}}
	ts, mgr := newTestJobDaemon(t, p)
	t.Setenv("GLUE_SERVER", ts.URL)
	t.Setenv("GLUE_TOKEN", "job-token")
	jobID := strings.TrimSpace(mustJobCLI(t, "submit", "--repo", repo, "--objective", "add a Makefile with a test target", "--policy", "ask-all", "--verify", "git status --porcelain", "--deliver", "none", "--backend", "local", "--max-iterations", "1", "--start=false"))
	if !strings.Contains(mustJobCLI(t, "list", "--status", "created"), jobID) || !strings.Contains(mustJobCLI(t, "start", jobID), jobID) {
		t.Fatal("list/start missing jobID")
	}
	perm1 := waitJob(t, mgr, jobID, jobs.WaitingForInput)
	if showOut := mustJobCLI(t, "show", jobID); !strings.Contains(showOut, "status: waiting_for_input") || !strings.Contains(showOut, perm1) {
		t.Fatalf("show out=%q", showOut)
	}
	mustJobCLI(t, "say", jobID, "keep Makefile minimal")
	mustJobCLI(t, "approve", jobID, perm1, "--remember", "session")
	mustJobCLI(t, "deny", jobID, waitJob(t, mgr, jobID, jobs.WaitingForInput), "--reason", "not needed")
	waitJob(t, mgr, jobID, jobs.MaxIterations)
	mustJobCLI(t, "continue", jobID, "proceed with existing Makefile", "--max-iterations", "1")
	waitJob(t, mgr, jobID, jobs.Achieved)
	if !strings.Contains(fmt.Sprint(p.requests[len(p.requests)-2].Messages), "keep Makefile minimal") {
		t.Fatal("guidance from say not delivered to maker")
	}
	var res jobs.Result
	if err := json.Unmarshal([]byte(mustJobCLI(t, "result", jobID, "--json")), &res); err != nil || res.JobID != jobID || res.Status != jobs.Achieved || res.Branch != "glue/"+jobID || len(res.Verify) != 1 || res.Verify[0].ExitCode != 0 {
		t.Fatalf("unexpected result: %+v err=%v", res, err)
	}
	if resText := mustJobCLI(t, "result", jobID); !strings.Contains(resText, "status: achieved") || !strings.Contains(resText, "verify: git status --porcelain") {
		t.Fatalf("result text=%q", resText)
	}
	job2ID := strings.TrimSpace(mustJobCLI(t, "submit", "--repo", repo, "--objective", "cancel me", "--start=false"))
	mustJobCLI(t, "cancel", job2ID)
	waitJob(t, mgr, job2ID, jobs.Canceled)
	var gotPurge string
	delTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPurge = r.Method + " " + r.URL.RequestURI()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(delTS.Close)
	mustJobCLI(t, "delete", "--server", delTS.URL, "--token", "t", job2ID)
	if gotPurge != "DELETE /v1/jobs/"+job2ID+"?purge=true" {
		t.Fatalf("gotPurge = %q", gotPurge)
	}
}
func TestJobSubmitSelect(t *testing.T) {
	repo := serveJobRepo(t)
	ts, mgr := newTestJobDaemon(t, &scriptedProvider{})
	t.Setenv("GLUE_SERVER", ts.URL)
	t.Setenv("GLUE_TOKEN", "job-token")
	if _, errOut, code := runJobCLI(t, "submit", "--repo", repo, "--objective", "test select", "--select", "--start=false"); code == 0 || !strings.Contains(errOut, "requires terminal") {
		t.Fatalf("expected non-TTY error, got code=%d err=%q", code, errOut)
	}
	origTTY, origSelect := jobIsTTY, jobSelectModel
	t.Cleanup(func() { jobIsTTY, jobSelectModel = origTTY, origSelect })
	jobIsTTY = func(any) bool { return true }
	var gotInitial modelSelection
	jobSelectModel = func(_ context.Context, _ io.Reader, _ io.Writer, initial modelSelection) (modelSelection, error) {
		gotInitial = initial
		return modelSelection{Provider: "deepseek", Model: "deepseek-flash", Thinking: "high"}, nil
	}
	out := mustJobCLI(t, "submit", "--repo", repo, "--objective", "test select", "--select", "--start=false")
	if job := must(mgr.Get(context.Background(), strings.TrimSpace(out))); gotInitial.Provider != "gemini" || gotInitial.Model != "gemini-3.8-flash" || job.Spec.Provider != "deepseek" || job.Spec.Model != "deepseek-flash" || job.Spec.Thinking != "high" {
		t.Fatalf("initial=%+v job=%+v", gotInitial, job)
	}
}

type detachAfterFirstApprovalReader struct {
	sent   bool
	cancel context.CancelFunc
	ctx    context.Context
}

func (r *detachAfterFirstApprovalReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, "a\n"), nil
	}
	r.cancel()
	<-r.ctx.Done()
	return 0, io.EOF
}
func TestJobAttachReplayPermissionsAndDetach(t *testing.T) {
	for _, k := range []string{"XDG_CONFIG_HOME", "GLUE_CONFIG_DIR"} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", t.TempDir())
	repo := serveJobRepo(t)
	p := &scriptedProvider{turns: [][]glue.ProviderEvent{
		textTurn(`{"items":[{"title":"add Makefile"}]}`),
		toolCallTurn("call-1", "write_file", `{"path":"Makefile","content":"test:\n\t@true\n"}`),
		toolCallTurn("call-2", "write_file", `{"path":"extra.txt","content":"skip\n"}`),
		textTurn("wrote Makefile"),
		textTurn(`{"done":false,"items":[{"title":"add Makefile","done":false}],"summary":"waiting for confirmation"}`),
		textTurn("confirmed Makefile"),
		textTurn(`{"done":true,"items":[{"title":"add Makefile","done":true,"evidence":"Makefile present"}],"summary":"Add Makefile test target"}`),
	}}
	ts, mgr := newTestJobDaemon(t, p)
	t.Setenv("GLUE_SERVER", ts.URL)
	t.Setenv("GLUE_TOKEN", "job-token")
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	var out1, err1 bytes.Buffer
	if code := runCLIWithDeps(ctx1, []string{"job", "submit", "--repo", repo, "--objective", "add a Makefile with a test target", "--policy", "ask-all", "--max-iterations", "1", "--attach"}, &detachAfterFirstApprovalReader{cancel: cancel1, ctx: ctx1}, &out1, &err1, fakeFactory(nil), serveDaemon, nil); code != 0 {
		t.Fatalf("first attach failed (%d): %s", code, err1.String())
	}
	jobID := strings.TrimSpace(strings.Split(out1.String(), "\n")[0])
	if !strings.Contains(mustJobCLI(t, "list"), jobID+"\twaiting_for_input") {
		t.Fatalf("expected job still waiting_for_input after detach, got: %s", mustJobCLI(t, "list"))
	}
	if info := must(os.Stat(jobclient.SeqPath(ts.URL, jobID))); info.Mode().Perm() != 0o600 {
		t.Fatalf("seq file mode=%v", info.Mode().Perm())
	}
	mustJobCLI(t, "say", jobID, "keep Makefile minimal")

	var out2, err2 bytes.Buffer
	if code := runCLIWithDeps(context.Background(), []string{"job", "attach", jobID}, strings.NewReader("d\n"), &out2, &err2, fakeFactory(nil), serveDaemon, nil); code != 0 {
		t.Fatalf("second attach failed (%d): %s", code, err2.String())
	}
	for _, line := range strings.Split(strings.TrimSpace(out1.String()), "\n") {
		// Compare events, not the job ID line or the elapsed-time prefix.
		if _, rest, ok := strings.Cut(line, "] "); ok {
			line = rest
		}
		if line = strings.TrimSpace(line); line != "" && line != jobID && strings.Contains(out2.String(), line) {
			t.Fatalf("duplicate line %q across attaches:\nout1:\n%s\nout2:\n%s", line, out1.String(), out2.String())
		}
	}
	if !strings.Contains(out1.String(), "plan (1 items):") || !strings.Contains(out1.String(), "✓ write_file Makefile") {
		t.Fatalf("out1=%q", out1.String())
	}
	if !strings.Contains(out2.String(), "operator: keep Makefile minimal") || !strings.Contains(out2.String(), "✗ write_file extra.txt") || !strings.Contains(out2.String(), "permission denied by user") || !strings.Contains(out2.String(), "wrote Makefile") || !strings.Contains(out2.String(), "job max_iterations") {
		t.Fatalf("out2=%q", out2.String())
	}
	mustJobCLI(t, "continue", jobID, "proceed", "--max-iterations", "1")
	var out3, err3 bytes.Buffer
	if code := runCLIWithDeps(context.Background(), []string{"job", "attach", jobID}, strings.NewReader(""), &out3, &err3, fakeFactory(nil), serveDaemon, nil); code != 0 {
		t.Fatalf("third attach failed (%d): %s", code, err3.String())
	}
	if !strings.Contains(out3.String(), "resumed: iteration 2 (continue)") || !strings.Contains(out3.String(), "confirmed Makefile") || !strings.Contains(out3.String(), "job achieved — Add Makefile test target") || strings.Contains(out3.String(), "] iteration 1 ") || strings.Contains(out3.String(), "wrote Makefile") {
		t.Fatalf("out3=%q", out3.String())
	}
	if out4 := mustJobCLI(t, "attach", jobID); out4 != "" {
		t.Fatalf("attach after terminal seq should be empty, got %q", out4)
	}
	_ = mgr
}
func TestAttachReconnectsAfterDrop(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var attempts atomic.Int32
	var gotLastEventID atomic.Value
	events := []eventlog.Envelope{
		{Version: 2, Seq: 1, Type: string(glue.GoalEventPlan), Payload: glue.GoalEvent{Type: glue.GoalEventPlan, Checklist: []glue.ChecklistItem{{Title: "add Makefile"}}}},
		{Version: 2, Seq: 2, Type: string(glue.GoalEventIterationStart), Payload: glue.GoalEvent{Type: glue.GoalEventIterationStart, Iteration: 1}},
		{Version: 2, Seq: 3, Type: "run_interrupted", Payload: map[string]any{"reason": "node restart"}},
		{Version: 2, Seq: 4, Type: "job_resumed", Payload: map[string]any{"from_iteration": 1, "reason": "restart"}},
		{Version: 2, Seq: 5, Type: string(glue.EventToolStart), Payload: glue.Event{Type: glue.EventToolStart, ToolName: "write_file", ToolCall: &glue.ToolCall{Name: "write_file", Arguments: json.RawMessage(`{"path":"Makefile"}`)}}},
		{Version: 2, Seq: 6, Type: string(glue.EventToolEnd), Payload: glue.Event{Type: glue.EventToolEnd, ToolName: "write_file", ToolResult: &glue.ToolResult{Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: "ok"}}}}},
		{Version: 2, Seq: 7, Type: string(glue.EventTextDelta), Phase: "maker", Payload: glue.Event{Type: glue.EventTextDelta, Delta: "wrote Makefile\n"}},
		{Version: 2, Seq: 8, Type: string(glue.GoalEventVerdict), Payload: glue.GoalEvent{Type: glue.GoalEventVerdict, Checklist: []glue.ChecklistItem{{Title: "add Makefile", Done: true}}, Message: "done"}},
		{Version: 2, Seq: 9, Type: "job_done", Payload: map[string]any{"status": jobs.Achieved, "result": glue.GoalResult{Summary: "Makefile ready"}}},
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/events") {
			http.NotFound(w, r)
			return
		}
		n := attempts.Add(1)
		gotLastEventID.Store(r.Header.Get("Last-Event-ID"))
		var after int64
		_, _ = fmt.Sscan(r.Header.Get("Last-Event-ID"), &after)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, ev := range events {
			if ev.Seq <= after {
				continue
			}
			data, _ := json.Marshal(ev)
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.Seq, data)
			flusher.Flush()
			if n == 1 && ev.Seq == 4 {
				return
			}
		}
	}))
	t.Cleanup(ts.Close)
	t.Setenv("GLUE_SERVER", ts.URL)
	t.Setenv("GLUE_TOKEN", "job-token")
	out := mustJobCLI(t, "attach", "job-drop-1")
	if attempts.Load() < 2 || gotLastEventID.Load() != "4" {
		t.Fatalf("attempts=%d Last-Event-ID=%v", attempts.Load(), gotLastEventID.Load())
	}
	for _, want := range []string{
		"plan (1 items):",
		"] iteration 1 (0 tokens so far)",
		"interrupted: node restart",
		"resumed: iteration 1 (restart)",
		"✓ write_file Makefile",
		"wrote Makefile",
		"verdict: 1/1 done — done",
		"job achieved — Makefile ready",
	} {
		if c := strings.Count(out, want); c != 1 {
			t.Fatalf("expected %q once (got %d) in:\n%s", want, c, out)
		}
	}
}
func waitJob(t *testing.T, mgr *jobs.Manager, jobID string, want jobs.Status) string {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(15 * time.Millisecond) {
		if job, err := mgr.Get(context.Background(), jobID); err == nil && job.Status == want {
			if perms, _ := mgr.Permissions(context.Background(), jobID, "pending"); want != jobs.WaitingForInput || len(perms) > 0 {
				if len(perms) > 0 {
					return perms[0].ID
				}
				return ""
			}
		}
	}
	t.Fatalf("timed out waiting for %s status %s", jobID, want)
	return ""
}

type pruneFakeAPI struct {
	jobAPI
	list    []jobs.Job
	deleted []string
	pruned  []string
}

func (f *pruneFakeAPI) List(context.Context, jobs.Filter) ([]jobs.Job, error) { return f.list, nil }
func (f *pruneFakeAPI) Delete(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

type pruneFakeAXAPI struct{ *pruneFakeAPI }

func (f pruneFakeAXAPI) Prune(_ context.Context, id string) error {
	f.pruned = append(f.pruned, id)
	return nil
}

func TestJobPrune(t *testing.T) {
	now := time.Now()
	list := []jobs.Job{
		{ID: "job-old-done", Status: jobs.Achieved, UpdatedAt: now.Add(-10 * 24 * time.Hour)},
		{ID: "job-old-running", Status: jobs.Running, UpdatedAt: now.Add(-10 * 24 * time.Hour)},
		{ID: "job-new-done", Status: jobs.Canceled, UpdatedAt: now.Add(-time.Hour)},
	}
	var out bytes.Buffer
	f := &pruneFakeAPI{list: list}
	if err := pruneJobs(context.Background(), f, now.Add(-7*24*time.Hour), true, &out); err != nil || len(f.deleted) != 0 || !strings.Contains(out.String(), "would delete job-old-done") || strings.Contains(out.String(), "job-new-done") || strings.Contains(out.String(), "job-old-running") {
		t.Fatalf("dry run: err=%v deleted=%v out=%q", err, f.deleted, out.String())
	}
	out.Reset()
	if err := pruneJobs(context.Background(), f, now.Add(-7*24*time.Hour), false, &out); err != nil || len(f.deleted) != 1 || f.deleted[0] != "job-old-done" {
		t.Fatalf("prune: err=%v deleted=%v out=%q", err, f.deleted, out.String())
	}
	ax := pruneFakeAXAPI{&pruneFakeAPI{list: list}}
	if err := pruneJobs(context.Background(), ax, now.Add(-7*24*time.Hour), false, &out); err != nil || len(ax.pruned) != 1 || len(ax.deleted) != 0 {
		t.Fatalf("ax prune: err=%v pruned=%v deleted=%v", err, ax.pruned, ax.deleted)
	}
	for in, want := range map[string]time.Duration{"7d": 7 * 24 * time.Hour, "12h": 12 * time.Hour, "0d": 0} {
		if got, err := parseAge(in); err != nil || got != want {
			t.Fatalf("parseAge(%q) = %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"7x", "-1h", "1.5d"} {
		if _, err := parseAge(in); err == nil {
			t.Fatalf("parseAge(%q) succeeded", in)
		}
	}
}
