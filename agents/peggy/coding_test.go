package peggy

import (
	"context"
	"errors"
	"os"
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

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
