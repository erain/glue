package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/erain/glue"
)

// goalCLITurn scripts one assistant turn whose final message carries text,
// as the goal loop's PromptJSON parsing expects.
func goalCLITurn(text string) []glue.ProviderEvent {
	msg := glue.Message{
		Role:    glue.MessageRoleAssistant,
		Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: text}},
	}
	return []glue.ProviderEvent{
		{Type: glue.ProviderEventStart},
		{Type: glue.ProviderEventTextDelta, Delta: text},
		{Type: glue.ProviderEventDone, Message: &msg},
	}
}

func TestGoalCommandAchieved(t *testing.T) {
	t.Parallel()
	store := t.TempDir()
	provider := &scriptedProvider{turns: [][]glue.ProviderEvent{
		goalCLITurn(`{"items":[{"title":"A"}]}`),
		goalCLITurn("did A"),
		goalCLITurn(`{"done":true,"items":[{"title":"A","done":true,"evidence":"A.go"}],"summary":"all done"}`),
	}}

	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), []string{"goal", "--store", store, "ship", "A"}, &stdout, &stderr, fakeFactory(provider))
	if code != goalExitAchieved {
		t.Fatalf("exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"goal goal-", "ship A", "plan (1 items):", "— iteration 1", "verdict: 1/1", "[x] A — A.go", "goal achieved after 1 iteration(s)", "all done"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q\n%s", want, out)
		}
	}
	// No --model flag: the provider's default model must be resolved, not
	// left empty (live providers reject empty-model requests).
	if len(provider.requests) == 0 || provider.requests[0].Model == "" {
		t.Fatalf("requests carry no model: %+v", provider.requests)
	}
}

func TestGoalCommandExitCodesAndResume(t *testing.T) {
	t.Parallel()
	store := t.TempDir()

	// Round 1: one iteration, checker leaves B undone → max_iterations (3).
	provider := &scriptedProvider{turns: [][]glue.ProviderEvent{
		goalCLITurn(`{"items":[{"title":"A"},{"title":"B"}]}`),
		goalCLITurn("did A"),
		goalCLITurn(`{"done":false,"items":[{"title":"A","done":true},{"title":"B"}],"summary":"B remains"}`),
	}}
	var stdout bytes.Buffer
	code := runCLI(context.Background(), []string{"goal", "--store", store, "--max-iterations", "1", "ship A and B"}, &stdout, io.Discard, fakeFactory(provider))
	if code != goalExitMaxIterations {
		t.Fatalf("exit = %d, want %d\n%s", code, goalExitMaxIterations, stdout.String())
	}

	// --list sees the stored record.
	stdout.Reset()
	code = runCLI(context.Background(), []string{"goal", "--store", store, "--list"}, &stdout, io.Discard, fakeFactory(nil))
	if code != 0 || !strings.Contains(stdout.String(), "max_iterations") || !strings.Contains(stdout.String(), "ship A and B") {
		t.Fatalf("--list: exit=%d out=%q", code, stdout.String())
	}

	// --resume continues from the verified checklist: no planning call, the
	// scripted turns are maker + verdict only, and iteration numbering
	// continues at 2.
	resumeProvider := &scriptedProvider{turns: [][]glue.ProviderEvent{
		goalCLITurn("did B"),
		goalCLITurn(`{"done":true,"items":[{"title":"A","done":true},{"title":"B","done":true}],"summary":"done"}`),
	}}
	stdout.Reset()
	code = runCLI(context.Background(), []string{"goal", "--store", store, "--resume"}, &stdout, io.Discard, fakeFactory(resumeProvider))
	if code != goalExitAchieved {
		t.Fatalf("--resume exit = %d, want 0\n%s", code, stdout.String())
	}
	out := stdout.String()
	for _, want := range []string{"resuming goal-", "(iteration 2)", "— iteration 2", "goal achieved"} {
		if !strings.Contains(out, want) {
			t.Errorf("resume stdout missing %q\n%s", want, out)
		}
	}
	if len(resumeProvider.requests) != 2 {
		t.Fatalf("provider calls = %d, want 2 (no planning call on resume)", len(resumeProvider.requests))
	}
}

func TestGoalCommandUsageErrors(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	code := runCLI(context.Background(), []string{"goal", "--store", t.TempDir()}, io.Discard, &stderr, fakeFactory(&scriptedProvider{}))
	if code != goalExitErrored || !strings.Contains(stderr.String(), "usage: glue goal") {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}

	stderr.Reset()
	code = runCLI(context.Background(), []string{"goal", "--store", t.TempDir(), "--worktree", "x"}, io.Discard, &stderr, fakeFactory(&scriptedProvider{}))
	if code != goalExitErrored || !strings.Contains(stderr.String(), "--worktree needs --coding") {
		t.Fatalf("worktree gating: exit=%d stderr=%q", code, stderr.String())
	}

	stderr.Reset()
	code = runCLI(context.Background(), []string{"goal", "--store", t.TempDir(), "--resume"}, io.Discard, &stderr, fakeFactory(&scriptedProvider{}))
	if code != goalExitErrored || !strings.Contains(stderr.String(), "no unfinished goal") {
		t.Fatalf("resume with empty store: exit=%d stderr=%q", code, stderr.String())
	}
}
