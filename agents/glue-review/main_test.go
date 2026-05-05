package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/erain/glue"
)

func TestReviewToolsRegistered(t *testing.T) {
	t.Parallel()
	tools := reviewTools(".", nil)
	wantNames := []string{"git_diff_branch", "git_log_branch", "read_file"}
	if len(tools) != len(wantNames) {
		t.Fatalf("got %d tools, want %d", len(tools), len(wantNames))
	}
	for i, want := range wantNames {
		if tools[i].Name != want {
			t.Fatalf("tools[%d].Name = %q, want %q", i, tools[i].Name, want)
		}
		if tools[i].Description == "" {
			t.Fatalf("tools[%d] missing description", i)
		}
		if tools[i].Execute == nil {
			t.Fatalf("tools[%d] missing executor", i)
		}
		// Parameters must be valid JSON; the model's tool spec layer expects this.
		var probe map[string]any
		if err := json.Unmarshal(tools[i].Parameters, &probe); err != nil {
			t.Fatalf("tools[%d] parameters: %v", i, err)
		}
	}
}

func TestNewProviderSelectionAndDefaults(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":           "moonshotai/kimi-k2.6",
		"nvidia":     "moonshotai/kimi-k2.6",
		"openrouter": "inclusionai/ling-2.6-1t:free",
		"gemini":     "gemini-2.5-flash",
	}
	for input, wantModel := range cases {
		_, model, err := newProvider(input)
		if err != nil {
			t.Fatalf("newProvider(%q): %v", input, err)
		}
		if model != wantModel {
			t.Fatalf("default model for %q: got %q want %q", input, model, wantModel)
		}
	}
	if _, _, err := newProvider("bogus"); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestSafeJoinRejectsTraversalAndAbsolute(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	cases := []struct {
		path string
		want bool // true = should reject
	}{
		{"../etc/passwd", true},
		{"/etc/passwd", true},
		{"", true},
		{"a/b/../c", false}, // resolves inside base
		{"file.go", false},
	}
	for _, c := range cases {
		_, err := safeJoin(base, c.path)
		if (err != nil) != c.want {
			t.Fatalf("safeJoin(%q): err=%v, want-reject=%v", c.path, err, c.want)
		}
	}
}

func TestTruncateAddsNote(t *testing.T) {
	t.Parallel()
	out := truncate(strings.Repeat("a", 1000), 100)
	if len(out) != 100 {
		t.Fatalf("len = %d, want 100", len(out))
	}
	if !strings.HasSuffix(out, "[... truncated]") {
		t.Fatalf("missing truncation note: %q", out[len(out)-30:])
	}
	short := truncate("hi", 100)
	if short != "hi" {
		t.Fatalf("short input mutated: %q", short)
	}
}

// TestGitToolsAgainstFakeRepo creates a tiny git repo on disk, runs the
// git tools through their Execute path, and asserts the agent-visible
// output. This exercises the shell-out flow end-to-end without hitting
// any LLM.
func TestGitToolsAgainstFakeRepo(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repo := t.TempDir()
	mustGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		// Disable any system / user gitconfig and signing so this test runs
		// hermetically.
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_GLOBAL=/dev/null",
		)
		var errBuf bytes.Buffer
		cmd.Stderr = &errBuf
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, errBuf.String())
		}
	}

	mustGit("init", "-q", "-b", "main")
	mustGit("commit", "--allow-empty", "-q", "-m", "init")
	mustGit("checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo, "hello.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit("add", "hello.txt")
	mustGit("commit", "-q", "-m", "add greeting")

	ctx := context.Background()

	diff := gitDiffBranchTool(repo)
	res, err := diff.Execute(ctx, glue.ToolCall{Name: diff.Name, Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("diff Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("diff returned error: %s", res.Content[0].Text)
	}
	if !strings.Contains(res.Content[0].Text, "+hello") {
		t.Fatalf("diff missing change: %s", res.Content[0].Text)
	}

	log := gitLogBranchTool(repo)
	res, err = log.Execute(ctx, glue.ToolCall{Name: log.Name, Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("log Execute: %v", err)
	}
	if !strings.Contains(res.Content[0].Text, "add greeting") {
		t.Fatalf("log missing commit: %s", res.Content[0].Text)
	}

	read := readFileTool(repo, nil)
	res, err = read.Execute(ctx, glue.ToolCall{Name: read.Name, Arguments: json.RawMessage(`{"path":"hello.txt"}`)})
	if err != nil {
		t.Fatalf("read Execute: %v", err)
	}
	if res.Content[0].Text != "hello\n" {
		t.Fatalf("read content = %q", res.Content[0].Text)
	}

	// read_file should reject traversal.
	res, err = read.Execute(ctx, glue.ToolCall{Name: read.Name, Arguments: json.RawMessage(`{"path":"../leak"}`)})
	if err != nil {
		t.Fatalf("read traversal Execute: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected traversal rejection, got: %s", res.Content[0].Text)
	}
}

// TestRunRejectsBogusProvider exercises the run() entrypoint with an
// unknown --provider flag. Also doubles as a smoke for the overall flag
// parsing path.
func TestRunRejectsBogusProvider(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	rc := run(context.Background(), []string{"--provider", "bogus"}, &out, &errBuf)
	if rc == 0 {
		t.Fatalf("expected non-zero exit, got 0; stderr=%q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "unknown provider") {
		t.Fatalf("stderr should explain bogus provider: %q", errBuf.String())
	}
}

// TestLiveReviewSmoke runs the example end-to-end against a real free
// model when its key is available. Defaults to NVIDIA + Llama 3.3 70B
// (faster than Kimi K2.6 for CI), with OpenRouter as a fallback. The
// fake repo is exactly what the agent reviews — keeps the diff tiny so
// the smoke completes in seconds.
func TestLiveReviewSmoke(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repo := t.TempDir()
	for _, c := range [][]string{
		{"init", "-q", "-b", "main"},
		{"commit", "--allow-empty", "-q", "-m", "init"},
		{"checkout", "-q", "-b", "feature"},
	} {
		cmd := exec.Command("git", c...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_GLOBAL=/dev/null",
		)
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %v: %v", c, err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\nfunc main() { panic(\"todo\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][]string{
		{"add", "main.go"},
		{"commit", "-q", "-m", "scaffold"},
	} {
		cmd := exec.Command("git", c...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_GLOBAL=/dev/null",
		)
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %v: %v", c, err)
		}
	}

	// Pick a provider that has a key in env. Prefer OpenRouter (fastest
	// free path) and fall back to NVIDIA.
	var args []string
	switch {
	case os.Getenv("OPENROUTER_API_KEY") != "":
		args = []string{"--provider", "openrouter", "--model", "inclusionai/ling-2.6-1t:free"}
	case os.Getenv("NVIDIA_API_KEY") != "":
		args = []string{"--provider", "nvidia", "--model", "meta/llama-3.3-70b-instruct"}
	default:
		t.Skip("no provider key in env (set NVIDIA_API_KEY or OPENROUTER_API_KEY)")
	}

	store := filepath.Join(repo, ".glue")
	args = append(args,
		"--work", repo,
		"--base", "main",
		"--store", store,
		"--id", fmt.Sprintf("smoke-%d", time.Now().UnixNano()),
		"--max-turns", "8",
	)

	// Bound the live call so a slow upstream cannot wedge the test.
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	var out, errBuf bytes.Buffer
	rc := run(ctx, args, &out, &errBuf)
	if rc != 0 {
		t.Fatalf("rc=%d stderr=%s", rc, errBuf.String())
	}
	combined := out.String() + errBuf.String()
	if !strings.Contains(combined, "main.go") {
		t.Fatalf("expected review to mention main.go; out=%q err=%q", out.String(), errBuf.String())
	}
	t.Logf("review output (%d bytes):\n%s", out.Len(), out.String())
}

