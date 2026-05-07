package git

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erain/glue"
)

func TestBuildPathspec(t *testing.T) {
	cases := []struct {
		name     string
		includes []string
		excludes []string
		want     []string
	}{
		{"both empty", nil, nil, nil},
		{"includes only", []string{"*.go", "cmd/..."}, nil, []string{"*.go", "cmd/..."}},
		{"excludes only adds catch-all", nil, []string{"vendor/*"}, []string{"*", ":(exclude)vendor/*"}},
		{"include + exclude", []string{"*.go"}, []string{"*_test.go"}, []string{"*.go", ":(exclude)*_test.go"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildPathspec(tc.includes, tc.excludes)
			if !equalSlice(got, tc.want) {
				t.Fatalf("BuildPathspec(%v, %v) = %v, want %v", tc.includes, tc.excludes, got, tc.want)
			}
		})
	}
}

func TestRunGit_VersionWorks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	out, err := RunGit(context.Background(), RunOptions{WorkDir: t.TempDir()}, "--version")
	if err != nil {
		t.Fatalf("git --version: %v", err)
	}
	if !strings.HasPrefix(out, "git version") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestRunGit_ErrorIncludesStderr(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	_, err := RunGit(context.Background(), RunOptions{WorkDir: t.TempDir()}, "log")
	if err == nil {
		t.Fatal("expected error from git log in empty dir")
	}
	if !strings.Contains(err.Error(), "git log") {
		t.Fatalf("error should reference command: %v", err)
	}
}

func TestDiffBranchTool_AgainstScratchRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := initScratchRepo(t)

	tool := DiffBranchTool(DiffBranchOptions{WorkDir: repo})
	res, err := tool.Execute(context.Background(), glue.ToolCall{
		Arguments: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content[0].Text)
	}
	if !strings.Contains(res.Content[0].Text, "feature.go") {
		t.Fatalf("expected diff to mention feature.go; got %q", res.Content[0].Text)
	}
}

func TestLogBranchTool_AgainstScratchRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := initScratchRepo(t)

	tool := LogBranchTool(LogBranchOptions{WorkDir: repo})
	res, err := tool.Execute(context.Background(), glue.ToolCall{
		Arguments: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content[0].Text)
	}
	if !strings.Contains(res.Content[0].Text, "add feature") {
		t.Fatalf("expected log to mention 'add feature'; got %q", res.Content[0].Text)
	}
}

func initScratchRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	mustGit(t, repo, "init", "-q", "-b", "main")
	mustGit(t, repo, "commit", "--allow-empty", "-q", "-m", "init")
	mustGit(t, repo, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo, "feature.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "feature.go")
	mustGit(t, repo, "commit", "-q", "-m", "add feature")
	return repo
}

func mustGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, out)
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
