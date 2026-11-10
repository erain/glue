// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erain/glue"
)

func TestVerifyFailureRecorded(t *testing.T) {
	repo := repoFixture(t)
	p := &scriptProvider{turns: [][]glue.ProviderEvent{
		turn(`{"items":[{"title":"edit file"}]}`),
		{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "w1", Name: "write_file", Arguments: []byte(`{"path":"work.txt","content":"partial\n"}`)}}, {Type: glue.ProviderEventDone}},
		turn("wrote partial file"),
		turn(`{"done":false,"items":[{"title":"edit file"}],"summary":"still failing verify"}`),
	}}
	stateDir := t.TempDir()
	m, err := New(Options{StateDir: stateDir, ProviderFactory: func(string) (glue.Provider, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })

	job, err := m.Create(context.Background(), Spec{
		Objective: "edit file",
		Repo:      RepoSpec{Path: repo, BaseRef: "main"},
		Budgets:   Budgets{MaxIterations: 1},
		ShellEnv:  []string{"CUSTOM_VERIFY_ENV=verify-env-ok"},
		Verify: [][]string{
			{"sh", "-c", "i=1; while [ $i -le 249 ]; do echo line-$i; i=$((i+1)); done; echo $CUSTOM_VERIFY_ENV"},
			{"sh", "-c", "echo verify check failed >&2; exit 7"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Result(context.Background(), job.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("Result before start = %v, want ErrConflict", err)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if finished := waitJob(t, m, job.ID); finished.Status != MaxIterations {
		t.Fatalf("status = %s, want %s", finished.Status, MaxIterations)
	}

	res, err := m.Result(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.JobID != job.ID || res.Status != MaxIterations || res.Summary != "still failing verify" || len(res.Verify) != 2 {
		t.Fatalf("unexpected result: %+v", res)
	}
	lines := strings.Split(res.Verify[0].OutputTail, "\n")
	if res.Verify[0].ExitCode != 0 || len(lines) != 200 || lines[0] != "line-51" || lines[len(lines)-1] != "verify-env-ok" {
		t.Fatalf("verify[0] = %+v (lines=%d)", res.Verify[0], len(lines))
	}
	if res.Verify[1].ExitCode != 7 || res.Verify[1].OutputTail != "verify check failed" {
		t.Fatalf("verify[1] = %+v", res.Verify[1])
	}
	if res.HeadSHA == "" || res.HeadSHA == res.BaseSHA || !strings.Contains(res.Diffstat, "work.txt") {
		t.Fatalf("commit/diffstat missing: %+v", res)
	}
	raw, err := os.ReadFile(filepath.Join(stateDir, "jobs", job.ID, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var disk Result
	if err := json.Unmarshal(raw, &disk); err != nil || disk.Verify[1].ExitCode != 7 || disk.HeadSHA != res.HeadSHA {
		t.Fatalf("disk result mismatch: %+v err %v", disk, err)
	}
}

func TestResultCommitsWorkspaceWithAuthorAndDiffstat(t *testing.T) {
	t.Setenv("GLUE_GIT_AUTHOR_NAME", "Glue Bot")
	t.Setenv("GLUE_GIT_AUTHOR_EMAIL", "bot@example.com")

	repo := repoFixture(t)
	p := &scriptProvider{turns: [][]glue.ProviderEvent{
		turn(`{"items":[{"title":"write file"}]}`),
		{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "write", Name: "write_file", Arguments: []byte(`{"path":"created.txt","content":"hello\n"}`)}}, {Type: glue.ProviderEventDone}},
		turn("wrote file"),
		turn(`{"done":true,"items":[{"title":"write file","done":true,"evidence":"created.txt"}],"summary":"Add created.txt greeting"}`),
	}}
	m, err := New(Options{StateDir: t.TempDir(), ProviderFactory: func(string) (glue.Provider, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })

	job, err := m.Create(context.Background(), Spec{Objective: "write file", Repo: RepoSpec{Path: repo, BaseRef: "main"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	finished := waitJob(t, m, job.ID)
	res, err := m.Result(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.JobID != job.ID || res.Status != Achieved || res.BaseRef != "main" || res.Branch != "glue/"+job.ID || res.BaseSHA == "" || res.BaseSHA == res.HeadSHA || !strings.Contains(res.Diffstat, "created.txt") || res.FinishedAt.IsZero() {
		t.Fatalf("unexpected result: %+v", res)
	}
	out, err := exec.Command("git", "-C", finished.Workspace, "log", "-1", "--format=%an|%ae|%s").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "Glue Bot|bot@example.com|Add created.txt greeting" {
		t.Fatalf("git log = %q err %v", out, err)
	}
}
