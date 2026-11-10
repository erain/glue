// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/daemon"
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
