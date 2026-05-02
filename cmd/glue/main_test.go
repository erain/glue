package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"glue"
)

type scriptedProvider struct {
	turns    [][]glue.ProviderEvent
	requests []glue.ProviderRequest
	err      error
}

func (p *scriptedProvider) Stream(_ context.Context, req glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	if p.err != nil {
		return nil, p.err
	}
	if len(p.requests) >= len(p.turns) {
		return nil, errors.New("scriptedProvider: unexpected call")
	}
	p.requests = append(p.requests, req)
	events := p.turns[len(p.requests)-1]
	ch := make(chan glue.ProviderEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func fakeFactory(provider glue.Provider) providerFactory {
	return func() (glue.Provider, error) { return provider, nil }
}

func textTurn(text string) []glue.ProviderEvent {
	return []glue.ProviderEvent{
		{Type: glue.ProviderEventStart},
		{Type: glue.ProviderEventTextDelta, Delta: text},
		{Type: glue.ProviderEventDone},
	}
}

func TestRunCLIHelp(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	code := runCLI(context.Background(), []string{"--help"}, &stdout, io.Discard, fakeFactory(nil))
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "glue run") {
		t.Fatalf("help missing 'glue run': %q", stdout.String())
	}
}

func TestRunCLINoArgsPrintsHelp(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	code := runCLI(context.Background(), nil, &stdout, io.Discard, fakeFactory(nil))
	if code != 0 || !strings.Contains(stdout.String(), "glue run") {
		t.Fatalf("code=%d stdout=%q", code, stdout.String())
	}
}

func TestRunCLIUnknownCommand(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), []string{"nope"}, &stdout, &stderr, fakeFactory(nil))
	if code == 0 {
		t.Fatal("code = 0, want nonzero")
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("stderr = %q, want 'unknown command'", stderr.String())
	}
}

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

func TestRunCLIUnknownAgent(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), []string{"run", "weird", "--prompt", "x"}, &stdout, &stderr, fakeFactory(&scriptedProvider{}))
	if code == 0 {
		t.Fatal("code = 0, want nonzero")
	}
	if !strings.Contains(stderr.String(), "unknown agent") {
		t.Fatalf("stderr = %q, want 'unknown agent'", stderr.String())
	}
}

func TestRunCLIMissingGeminiAPIKey(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")

	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), []string{
		"run", "--prompt", "go", "--store", t.TempDir(),
	}, &stdout, &stderr, defaultGeminiFactory)
	if code == 0 {
		t.Fatal("code = 0, want nonzero for missing GEMINI_API_KEY")
	}
	if !strings.Contains(stderr.String(), "GEMINI_API_KEY") {
		t.Fatalf("stderr = %q, want GEMINI_API_KEY hint", stderr.String())
	}
}
