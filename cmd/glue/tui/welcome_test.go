package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/erain/glue"
)

// makeTestModel builds a Model wired to nil Agent/perm — fine because we
// drive only the bits that don't touch the agent loop.
func makeTestModel(t *testing.T) *Model {
	t.Helper()
	return newModel(context.Background(), Config{
		Agent:     nil,
		SessionID: "tui:test",
		Provider:  "codex",
		Model:     "gpt-5-codex",
		WorkDir:   "/work",
		Tools: []glue.Tool{
			{ToolSpec: glue.ToolSpec{Name: "read_file"}},
			{ToolSpec: glue.ToolSpec{Name: "write_file"}},
			{ToolSpec: glue.ToolSpec{Name: "grep"}},
		},
	})
}

func TestInitSeedsWelcomeCard(t *testing.T) {
	t.Parallel()
	m := makeTestModel(t)
	_ = m.Init()
	if len(m.transcript) != 1 || m.transcript[0].Kind != itemBlock {
		t.Fatalf("transcript = %#v, want 1 itemBlock", m.transcript)
	}
	plain := stripANSI(m.transcript[0].render(renderCtx{width: 80}))
	for _, want := range []string{"Welcome to glue", "Try:", "Enter sends", "/ for commands", "Esc cancels", "session tui:test", "codex/gpt-5-codex", "/work"} {
		if !strings.Contains(plain, want) {
			t.Errorf("welcome missing %q\n%s", want, plain)
		}
	}
}

func TestSlashHelpAppendsBlock(t *testing.T) {
	t.Parallel()
	m := makeTestModel(t)
	_ = m.Init()
	_, _ = m.handleSlash(slashCommand{Name: "help"})
	last := m.transcript[len(m.transcript)-1]
	if last.Kind != itemBlock || last.BlockTitle != "Commands" {
		t.Fatalf("last item = %#v, want Commands block", last)
	}
	plain := stripANSI(last.render(renderCtx{width: 80}))
	for _, want := range []string{"Commands", "/help", "/exit", "/clear", "/usage", "/tools", "/model"} {
		if !strings.Contains(plain, want) {
			t.Errorf("/help block missing %q\n%s", want, plain)
		}
	}
}

func TestSlashToolsAppendsBlock(t *testing.T) {
	t.Parallel()
	m := makeTestModel(t)
	_ = m.Init()
	_, _ = m.handleSlash(slashCommand{Name: "tools"})
	last := m.transcript[len(m.transcript)-1]
	if last.Kind != itemBlock || last.BlockTitle != "Registered tools" {
		t.Fatalf("last item = %#v, want Registered tools block", last)
	}
	plain := stripANSI(last.render(renderCtx{width: 80}))
	for _, want := range []string{"Registered tools", "grep", "read_file", "write_file"} {
		if !strings.Contains(plain, want) {
			t.Errorf("/tools block missing %q\n%s", want, plain)
		}
	}
}

func TestSlashClearNukesTranscriptAndResetsSession(t *testing.T) {
	t.Parallel()
	m := makeTestModel(t)
	_ = m.Init()
	oldSession := m.cfg.SessionID
	// Add some content so we can see it disappear.
	m.transcript = append(m.transcript,
		transcriptItem{Kind: itemUser, Text: "hi"},
		transcriptItem{Kind: itemAssistant, Text: "hello"},
	)
	m.turnNum = 5
	_, _ = m.handleSlash(slashCommand{Name: "clear"})

	// User/assistant content should be gone.
	for _, it := range m.transcript {
		if it.Kind == itemUser || it.Kind == itemAssistant {
			t.Fatalf("clear left a %v item behind: %+v", it.Kind, it)
		}
	}
	// New welcome card present.
	hasWelcome := false
	for _, it := range m.transcript {
		if it.Kind == itemBlock && it.BlockTitle == "Welcome to glue" {
			hasWelcome = true
		}
	}
	if !hasWelcome {
		t.Fatal("clear didn't re-add the welcome card")
	}
	// New session id.
	if m.cfg.SessionID == oldSession {
		t.Fatalf("session id unchanged after /clear: %q", m.cfg.SessionID)
	}
	// Turn counter reset.
	if m.turnNum != 0 {
		t.Fatalf("turnNum = %d, want 0", m.turnNum)
	}
}

func TestRenderToolIncludesInlinePermissionWhenPending(t *testing.T) {
	t.Parallel()
	it := transcriptItem{
		Kind:      itemTool,
		ToolName:  "edit_file",
		ToolArgs:  `{"path":"u.go","old_string":"a","new_string":"b"}`,
		ToolPhase: tsPending,
	}
	plain := stripANSI(it.render(renderCtx{width: 120}))
	for _, want := range []string{"edit_file", "awaiting permission", "[a]", "[s]", "[t]", "[n]", "Esc"} {
		if !strings.Contains(plain, want) {
			t.Errorf("pending tool missing %q\n%s", want, plain)
		}
	}
}

func TestRenderToolRunningShowsSpinnerFrame(t *testing.T) {
	t.Parallel()
	it := transcriptItem{
		Kind:      itemTool,
		ToolName:  "shell_exec",
		ToolArgs:  `{"argv":["go","test"]}`,
		ToolPhase: tsRunning,
	}
	plain := stripANSI(it.render(renderCtx{width: 80, spinner: "⠋"}))
	if !strings.Contains(plain, "⠋") {
		t.Errorf("running tool missing spinner frame\n%s", plain)
	}
	if !strings.Contains(plain, "running") {
		t.Errorf("running tool missing label\n%s", plain)
	}
}
