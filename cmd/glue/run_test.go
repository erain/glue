// Tests for the "glue run" subcommand.

package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erain/glue"
)

func TestRunCLIStreamsOutputAndLoadsEnv(t *testing.T) {
	// Not parallel: mutates process env.
	t.Setenv("GLUE_TEST_ENV", "")
	os.Unsetenv("GLUE_TEST_ENV")

	envPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envPath, []byte("GLUE_TEST_ENV=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	provider := &scriptedProvider{turns: [][]glue.ProviderEvent{{
		{Type: glue.ProviderEventStart},
		{Type: glue.ProviderEventTextDelta, Delta: "hello"},
		{Type: glue.ProviderEventTextDelta, Delta: " cli"},
		{Type: glue.ProviderEventDone},
	}}}
	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), []string{
		"run",
		"--prompt", "say hi",
		"--store", t.TempDir(),
		"--env", envPath,
	}, &stdout, &stderr, fakeFactory(provider))
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if stdout.String() != "hello cli\n" {
		t.Fatalf("stdout = %q, want streamed text", stdout.String())
	}
	if got := os.Getenv("GLUE_TEST_ENV"); got != "from-file" {
		t.Fatalf("env GLUE_TEST_ENV = %q, want from-file", got)
	}
	t.Cleanup(func() { os.Unsetenv("GLUE_TEST_ENV") })
}

func TestRunCLIUsageReportsTokens(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{turns: [][]glue.ProviderEvent{{
		{Type: glue.ProviderEventStart},
		{Type: glue.ProviderEventTextDelta, Delta: "hello"},
		{Type: glue.ProviderEventDone, Message: &glue.Message{
			Role: glue.MessageRoleAssistant,
			Usage: &glue.Usage{
				InputTokens:     3,
				OutputTokens:    2,
				CacheReadTokens: 1,
				TotalTokens:     5,
			},
		}},
	}}}
	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), []string{
		"run",
		"--prompt", "say hi",
		"--store", t.TempDir(),
		"--usage",
	}, &stdout, &stderr, fakeFactory(provider))
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if stdout.String() != "hello\n" {
		t.Fatalf("stdout = %q, want streamed text", stdout.String())
	}
	if got, want := stderr.String(), "usage: input=3 output=2 cache_read=1 total=5\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func TestRunCLIUsageReportsEstimatedCost(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{turns: [][]glue.ProviderEvent{{
		{Type: glue.ProviderEventStart},
		{Type: glue.ProviderEventTextDelta, Delta: "hello"},
		{Type: glue.ProviderEventDone, Message: &glue.Message{
			Role: glue.MessageRoleAssistant,
			Usage: &glue.Usage{
				InputTokens:      1_000_000,
				OutputTokens:     500_000,
				CacheReadTokens:  250_000,
				CacheWriteTokens: 100_000,
				TotalTokens:      1_850_000,
			},
		}},
	}}}
	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), []string{
		"run",
		"--prompt", "say hi",
		"--store", t.TempDir(),
		"--usage",
		"--usage-input-price", "1",
		"--usage-output-price", "2",
		"--usage-cache-read-price", "0.25",
		"--usage-cache-write-price", "3",
	}, &stdout, &stderr, fakeFactory(provider))
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if got, want := stderr.String(), "usage: input=1000000 output=500000 cache_read=250000 cache_write=100000 total=1850000 cost_usd=2.362500\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func TestRunCLIUsageSilentWhenMissing(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{turns: [][]glue.ProviderEvent{textTurn("ok")}}
	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), []string{
		"run",
		"--prompt", "go",
		"--store", t.TempDir(),
		"--usage",
		"--usage-input-price", "1",
	}, &stdout, &stderr, fakeFactory(provider))
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if stdout.String() != "ok\n" {
		t.Fatalf("stdout = %q, want ok", stdout.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want no usage output", stderr.String())
	}
}

func TestRunCLIUsagePriceRejectsNegative(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), []string{
		"run",
		"--prompt", "go",
		"--usage",
		"--usage-input-price", "-1",
	}, &stdout, &stderr, fakeFactory(&scriptedProvider{}))
	if code == 0 {
		t.Fatal("code = 0, want failure")
	}
	if !strings.Contains(stderr.String(), "--usage-input-price must be non-negative") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunCLIMultipleEnvFilesShellEnvWins(t *testing.T) {
	t.Setenv("GLUE_TEST_FROM_SHELL", "shell-value")
	os.Unsetenv("GLUE_TEST_FROM_FILE_A")
	os.Unsetenv("GLUE_TEST_FROM_FILE_B")
	t.Cleanup(func() {
		os.Unsetenv("GLUE_TEST_FROM_FILE_A")
		os.Unsetenv("GLUE_TEST_FROM_FILE_B")
	})

	dir := t.TempDir()
	a := filepath.Join(dir, "a.env")
	b := filepath.Join(dir, "b.env")
	if err := os.WriteFile(a, []byte("GLUE_TEST_FROM_FILE_A=A\nGLUE_TEST_FROM_SHELL=ignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("GLUE_TEST_FROM_FILE_B=B\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	provider := &scriptedProvider{turns: [][]glue.ProviderEvent{textTurn("ok")}}
	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), []string{
		"run",
		"--prompt", "go",
		"--store", t.TempDir(),
		"--env", a, "--env", b,
	}, &stdout, &stderr, fakeFactory(provider))
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if got := os.Getenv("GLUE_TEST_FROM_FILE_A"); got != "A" {
		t.Fatalf("FILE_A = %q, want A", got)
	}
	if got := os.Getenv("GLUE_TEST_FROM_FILE_B"); got != "B" {
		t.Fatalf("FILE_B = %q, want B", got)
	}
	if got := os.Getenv("GLUE_TEST_FROM_SHELL"); got != "shell-value" {
		t.Fatalf("FROM_SHELL = %q, want shell-value (env file should not override)", got)
	}
}

func TestRunCLIResumesSession(t *testing.T) {
	t.Parallel()

	storeDir := t.TempDir()

	first := &scriptedProvider{turns: [][]glue.ProviderEvent{textTurn("first")}}
	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), []string{
		"run", "--id", "same", "--prompt", "first", "--store", storeDir,
	}, &stdout, &stderr, fakeFactory(first))
	if code != 0 {
		t.Fatalf("first code = %d stderr=%q", code, stderr.String())
	}

	second := &scriptedProvider{turns: [][]glue.ProviderEvent{textTurn("second")}}
	stdout.Reset()
	stderr.Reset()
	code = runCLI(context.Background(), []string{
		"run", "--id", "same", "--prompt", "second", "--store", storeDir,
	}, &stdout, &stderr, fakeFactory(second))
	if code != 0 {
		t.Fatalf("second code = %d stderr=%q", code, stderr.String())
	}
	if len(second.requests) != 1 {
		t.Fatalf("second provider calls = %d, want 1", len(second.requests))
	}
	if got := len(second.requests[0].Messages); got != 3 {
		t.Fatalf("second request msg count = %d, want 3 (resumed user/assistant + new user)", got)
	}
}

func TestRunCLIProviderErrorExit(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{err: errors.New("provider failed")}
	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), []string{
		"run", "--prompt", "fail", "--store", t.TempDir(),
	}, &stdout, &stderr, fakeFactory(provider))
	if code == 0 {
		t.Fatal("code = 0, want nonzero")
	}
	if !strings.Contains(stderr.String(), "provider failed") {
		t.Fatalf("stderr = %q, want provider failed", stderr.String())
	}
}

func TestRunCLICodingToolsPromptAndWrite(t *testing.T) {
	workDir := t.TempDir()
	provider := &scriptedProvider{turns: [][]glue.ProviderEvent{
		toolCallTurn("c1", "write_file", `{"path":"note.txt","content":"hello from glue code"}`),
		textTurn("done"),
	}}
	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"run",
		"--coding",
		"--work", workDir,
		"--prompt", "write a note",
		"--store", filepath.Join(t.TempDir(), "sessions"),
	}, strings.NewReader("a\n"), &stdout, &stderr, fakeFactory(provider), nil, http.DefaultClient)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if stdout.String() != "done\n" {
		t.Fatalf("stdout = %q, want done", stdout.String())
	}
	if !strings.Contains(stderr.String(), "glue run: coding tools enabled") || !strings.Contains(stderr.String(), "Permission requested: write_file") {
		t.Fatalf("stderr = %q, want coding notice and permission prompt", stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(workDir, "note.txt"))
	if err != nil {
		t.Fatalf("read note: %v", err)
	}
	if string(data) != "hello from glue code" {
		t.Fatalf("note.txt = %q", data)
	}
	if len(provider.requests) == 0 {
		t.Fatal("provider not called")
	}
	var toolNames []string
	for _, tool := range provider.requests[0].Tools {
		toolNames = append(toolNames, tool.Name)
	}
	for _, want := range []string{"read_file", "write_file", "shell_exec", "git_diff_branch", "git_log_branch"} {
		if !containsString(toolNames, want) {
			t.Fatalf("tools = %v, missing %s", toolNames, want)
		}
	}
}

func TestRunCLICodingDeniesSideEffectOnDefaultPromptAnswer(t *testing.T) {
	workDir := t.TempDir()
	provider := &scriptedProvider{turns: [][]glue.ProviderEvent{
		toolCallTurn("c1", "write_file", `{"path":"note.txt","content":"should not write"}`),
		textTurn("done"),
	}}
	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"run",
		"--coding",
		"--work", workDir,
		"--prompt", "try to write",
		"--store", filepath.Join(t.TempDir(), "sessions"),
	}, strings.NewReader("\n"), &stdout, &stderr, fakeFactory(provider), nil, http.DefaultClient)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(workDir, "note.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("note.txt stat err = %v, want not exist", err)
	}
	if !strings.Contains(lastToolText(t, provider.requests[1]), "permission denied by user") {
		t.Fatalf("tool result = %q, want denial", lastToolText(t, provider.requests[1]))
	}
}

func TestRunCLIMissingPrompt(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), []string{"run"}, &stdout, &stderr, fakeFactory(&scriptedProvider{}))
	if code == 0 {
		t.Fatal("code = 0, want nonzero")
	}
	if !strings.Contains(stderr.String(), "missing required --prompt") {
		t.Fatalf("stderr = %q, want missing prompt", stderr.String())
	}
}
