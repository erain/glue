package peggy

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/erain/glue"
	filestore "github.com/erain/glue/stores/file"
)

type scriptedProvider struct {
	turns    [][]glue.ProviderEvent
	calls    int
	requests []glue.ProviderRequest
}

func (p *scriptedProvider) Stream(_ context.Context, req glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	if p.calls >= len(p.turns) {
		return nil, errors.New("scriptedProvider: unexpected call")
	}
	p.requests = append(p.requests, req)
	events := p.turns[p.calls]
	p.calls++
	ch := make(chan glue.ProviderEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

type recordingPermission struct {
	requests []glue.PermissionRequest
	decision glue.PermissionDecision
}

func (p *recordingPermission) Decide(_ context.Context, req glue.PermissionRequest) (glue.PermissionDecision, error) {
	p.requests = append(p.requests, req)
	return p.decision, nil
}

func toolCallTurn(id, name, args string) []glue.ProviderEvent {
	return []glue.ProviderEvent{
		{Type: glue.ProviderEventStart},
		{Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: id, Name: name, Arguments: []byte(args)}},
		{Type: glue.ProviderEventDone},
	}
}

func peggyTextTurn(text string) []glue.ProviderEvent {
	return []glue.ProviderEvent{
		{Type: glue.ProviderEventStart},
		{Type: glue.ProviderEventTextDelta, Delta: text},
		{Type: glue.ProviderEventDone},
	}
}

func newCodingTestPeggy(t *testing.T, provider glue.Provider, workDir string, perm glue.Permission) *Peggy {
	t.Helper()
	p, err := New(Options{
		Settings: Settings{Coding: CodingSettings{
			Enabled:         true,
			WorkDir:         workDir,
			AllowOverwrite:  true,
			AllowedBinaries: []string{"go"},
		}},
		Provider:   provider,
		Store:      filestore.New(filepath.Join(t.TempDir(), "sessions")),
		Permission: perm,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func TestCodingToolsRegisteredWhenEnabled(t *testing.T) {
	fp := &fakeProvider{text: "ok"}
	p := newCodingTestPeggy(t, fp, t.TempDir(), glue.AllowAll{})

	if _, err := p.Prompt(context.Background(), "s", "what can you do?", nil); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(fp.requests) == 0 {
		t.Fatal("provider not called")
	}
	var got []string
	for _, tool := range fp.requests[0].Tools {
		got = append(got, tool.Name)
	}
	sort.Strings(got)
	for _, want := range []string{"git_diff_branch", "git_log_branch", "read_file", "shell_exec", "write_file"} {
		if !containsString(got, want) {
			t.Fatalf("tools = %v, missing %s", got, want)
		}
	}
	if !filepath.IsAbs(p.Settings().Coding.WorkDir) {
		t.Fatalf("coding workdir = %q, want absolute", p.Settings().Coding.WorkDir)
	}
}

func TestPeggyCodingWriteFileAsksPermissionAndWrites(t *testing.T) {
	workDir := t.TempDir()
	provider := &scriptedProvider{turns: [][]glue.ProviderEvent{
		toolCallTurn("c1", "write_file", `{"path":"note.txt","content":"hello"}`),
		peggyTextTurn("done"),
	}}
	perm := &recordingPermission{decision: glue.PermissionDecision{Allow: true}}
	p := newCodingTestPeggy(t, provider, workDir, perm)

	if _, err := p.Prompt(context.Background(), "s", "write a note", nil); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(perm.requests) != 1 {
		t.Fatalf("permission requests = %d, want 1", len(perm.requests))
	}
	if got := perm.requests[0].Tool; got != "write_file" {
		t.Fatalf("permission tool = %q, want write_file", got)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "note.txt"))
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("written content = %q, want hello", data)
	}
}

func TestPeggyCodingReadOnlyToolDoesNotAskPermission(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "note.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{turns: [][]glue.ProviderEvent{
		toolCallTurn("c1", "read_file", `{"path":"note.txt"}`),
		peggyTextTurn("done"),
	}}
	perm := &recordingPermission{decision: glue.PermissionDecision{Allow: true}}
	p := newCodingTestPeggy(t, provider, workDir, perm)

	if _, err := p.Prompt(context.Background(), "s", "read a note", nil); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(perm.requests) != 0 {
		t.Fatalf("permission requests = %d, want 0", len(perm.requests))
	}
	toolResult := provider.requests[1].Messages[len(provider.requests[1].Messages)-1]
	if toolResult.Role != glue.MessageRoleTool || !strings.Contains(toolResult.Content[0].Text, "hello") {
		t.Fatalf("tool result = %#v, want read_file content", toolResult)
	}
}

func TestPeggyCodingShellDenyIsModelVisibleToolError(t *testing.T) {
	workDir := t.TempDir()
	provider := &scriptedProvider{turns: [][]glue.ProviderEvent{
		toolCallTurn("c1", "shell_exec", `{"argv":["go","version"]}`),
		peggyTextTurn("done"),
	}}
	perm := &recordingPermission{decision: glue.PermissionDecision{Allow: false, Reason: "not now"}}
	p := newCodingTestPeggy(t, provider, workDir, perm)

	if _, err := p.Prompt(context.Background(), "s", "run go version", nil); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(perm.requests) != 1 {
		t.Fatalf("permission requests = %d, want 1", len(perm.requests))
	}
	if got := perm.requests[0].Tool; got != "shell_exec" {
		t.Fatalf("permission tool = %q, want shell_exec", got)
	}
	toolResult := provider.requests[1].Messages[len(provider.requests[1].Messages)-1]
	if !toolResult.IsError || !strings.Contains(toolResult.Content[0].Text, "not now") {
		t.Fatalf("tool result = %#v, want denial tool error", toolResult)
	}
}

func TestPeggyCodingReleaseSmoke(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	workDir := t.TempDir()
	runGit(t, workDir, "init")
	runGit(t, workDir, "branch", "-M", "main")
	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "add", "README.md")
	runGit(t, workDir, "-c", "user.email=peggy@example.invalid", "-c", "user.name=Peggy Test", "commit", "-m", "base")
	runGit(t, workDir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(workDir, "feature.txt"), []byte("feature\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "add", "feature.txt")
	runGit(t, workDir, "-c", "user.email=peggy@example.invalid", "-c", "user.name=Peggy Test", "commit", "-m", "feature")

	provider := &scriptedProvider{turns: [][]glue.ProviderEvent{
		toolCallTurn("c1", "read_file", `{"path":"README.md"}`),
		toolCallTurn("c2", "write_file", `{"path":"note.txt","content":"hello from v0.2"}`),
		toolCallTurn("c3", "shell_exec", `{"argv":["go","version"],"max_output_bytes":256}`),
		toolCallTurn("c4", "git_diff_branch", `{"base":"main","max_bytes":4096}`),
		toolCallTurn("c5", "git_log_branch", `{"base":"main","limit":2}`),
		peggyTextTurn("release smoke done"),
	}}
	perm := &recordingPermission{decision: glue.PermissionDecision{Allow: true}}
	p := newCodingTestPeggy(t, provider, workDir, perm)

	if _, err := p.Prompt(context.Background(), "release-smoke", "exercise coding tools", nil); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got, want := len(perm.requests), 2; got != want {
		t.Fatalf("permission requests = %d, want %d", got, want)
	}
	if perm.requests[0].Tool != "write_file" || perm.requests[1].Tool != "shell_exec" {
		t.Fatalf("permission tools = %s, %s; want write_file, shell_exec", perm.requests[0].Tool, perm.requests[1].Tool)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "note.txt"))
	if err != nil {
		t.Fatalf("read note.txt: %v", err)
	}
	if string(data) != "hello from v0.2" {
		t.Fatalf("note.txt = %q", data)
	}
	if !strings.Contains(lastToolText(t, provider.requests[1]), "base") {
		t.Fatalf("read_file result missing content: %q", lastToolText(t, provider.requests[1]))
	}
	if !strings.Contains(lastToolText(t, provider.requests[3]), "go version") {
		t.Fatalf("shell_exec result missing go version: %q", lastToolText(t, provider.requests[3]))
	}
	if !strings.Contains(lastToolText(t, provider.requests[4]), "feature.txt") {
		t.Fatalf("git_diff_branch result missing feature file: %q", lastToolText(t, provider.requests[4]))
	}
	if !strings.Contains(lastToolText(t, provider.requests[5]), "feature") {
		t.Fatalf("git_log_branch result missing feature commit: %q", lastToolText(t, provider.requests[5]))
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, string(out))
	}
}

func lastToolText(t *testing.T, req glue.ProviderRequest) string {
	t.Helper()
	if len(req.Messages) == 0 {
		t.Fatal("provider request has no messages")
	}
	msg := req.Messages[len(req.Messages)-1]
	if msg.Role != glue.MessageRoleTool || len(msg.Content) == 0 {
		t.Fatalf("last message = %#v, want tool result", msg)
	}
	return msg.Content[0].Text
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
