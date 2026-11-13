// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erain/glue"
)

type countingProvider struct {
	scriptProvider
	calls atomic.Int32
}

func (p *countingProvider) Stream(ctx context.Context, req glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	p.calls.Add(1)
	return p.scriptProvider.Stream(ctx, req)
}

func setupManager(t *testing.T, p glue.Provider) *Manager {
	t.Helper()
	m, err := New(Options{StateDir: t.TempDir(), ProviderFactory: func(string) (glue.Provider, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func eventTypes(t *testing.T, m *Manager, id string) []string {
	t.Helper()
	events, err := m.Events(context.Background(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range events {
		out = append(out, ev.Type)
	}
	return out
}

func TestSetupRunsBeforePlan(t *testing.T) {
	p := &countingProvider{scriptProvider: scriptProvider{turns: [][]glue.ProviderEvent{
		turn(`{"items":[{"title":"check deps"}]}`),
		turn("deps are there"),
		turn(`{"done":true,"items":[{"title":"check deps","done":true,"evidence":"ok"}],"summary":"done"}`),
	}}}
	m := setupManager(t, p)
	repo := repoFixture(t)
	job, err := m.Create(context.Background(), Spec{Objective: "check deps", Repo: RepoSpec{Path: repo}, Setup: [][]string{{"sh", "-c", "echo installed > .deps"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	finished := waitJob(t, m, job.ID)
	if finished.Status != Achieved {
		t.Fatalf("status = %s", finished.Status)
	}
	types := strings.Join(eventTypes(t, m, job.ID), " ")
	if i, j := strings.Index(types, "setup_end"), strings.Index(types, "plan"); i < 0 || j < 0 || i > j {
		t.Fatalf("setup must finish before planning: %s", types)
	}
	if data, _ := os.ReadFile(filepath.Join(finished.Workspace, ".deps")); strings.TrimSpace(string(data)) != "installed" {
		t.Fatalf("setup did not run in the job workspace: %q", data)
	}
	res, _ := m.Result(context.Background(), job.ID)
	if len(res.Setup) != 1 || res.Setup[0].ExitCode != 0 {
		t.Fatalf("result.setup = %+v", res.Setup)
	}
}

func TestSetupFailureBlocksWithEvidence(t *testing.T) {
	p := &countingProvider{}
	m := setupManager(t, p)
	repo, bare := repoWithBareOrigin(t, "")
	job, err := m.Create(context.Background(), Spec{Objective: "build", Repo: RepoSpec{Path: repo, BaseRef: "main"}, Delivery: "branch",
		Setup: [][]string{{"sh", "-c", "touch leftover.txt; echo cannot reach registry >&2; exit 3"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if got := waitJob(t, m, job.ID); got.Status != Blocked {
		t.Fatalf("status = %s, want blocked", got.Status)
	}
	if n := p.calls.Load(); n != 0 {
		t.Fatalf("model was called %d times after a failed setup", n)
	}
	res, err := m.Result(context.Background(), job.ID)
	if err != nil || len(res.Setup) != 1 || res.Setup[0].ExitCode != 3 || !strings.Contains(res.Setup[0].OutputTail, "cannot reach registry") || !strings.Contains(res.Summary, "setup failed") {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if res.HeadSHA != res.BaseSHA || res.Partial {
		t.Fatalf("setup leftovers were committed: %+v", res)
	}
	if out, _ := gitOut(bare, "branch", "--list", "glue/*"); strings.TrimSpace(out) != "" {
		t.Fatalf("a branch was pushed after a failed setup: %q", out)
	}
}

func TestSetupNotRerunAfterRestart(t *testing.T) {
	m := setupManager(t, &countingProvider{})
	repo := repoFixture(t)
	counter := filepath.Join(t.TempDir(), "runs")
	job, err := m.Create(context.Background(), Spec{Objective: "x", Repo: RepoSpec{Path: repo}, Setup: [][]string{{"sh", "-c", "echo run >> " + counter}}})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 { // the second call stands in for the run after a restart
		if blocked, err := m.runSetup(context.Background(), job.ID, job, repo, ""); err != nil || blocked != nil {
			t.Fatalf("runSetup = %+v, %v", blocked, err)
		}
	}
	if data, _ := os.ReadFile(counter); strings.Count(string(data), "run") != 1 {
		t.Fatalf("setup ran %d times, want once", strings.Count(string(data), "run"))
	}
}

func TestSetupTimeout(t *testing.T) {
	m := setupManager(t, &countingProvider{})
	job, err := m.Create(context.Background(), Spec{Objective: "x", Repo: RepoSpec{Path: repoFixture(t)}, Setup: [][]string{{"sleep", "30"}}, SetupTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if got := waitJob(t, m, job.ID); got.Status != Blocked || time.Since(start) > 10*time.Second {
		t.Fatalf("status = %s after %s", got.Status, time.Since(start))
	}
	if _, err := m.Create(context.Background(), Spec{Objective: "x", Repo: RepoSpec{Path: repoFixture(t)}, SetupTimeout: -time.Second}); err == nil {
		t.Fatal("negative setup_timeout accepted")
	}
}

func gitOut(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	return string(out), err
}

func TestShellJoin(t *testing.T) {
	if got := ShellJoin([]string{"sh", "-c", "echo hi >&2; exit 7", "it's", ""}); got != `sh -c 'echo hi >&2; exit 7' 'it'\''s' ''` {
		t.Fatalf("ShellJoin = %s", got)
	}
	if got := ShellJoin([]string{"go", "test", "./..."}); got != "go test ./..." {
		t.Fatalf("ShellJoin = %s", got)
	}
}
