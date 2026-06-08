package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestInputHasNoInternalPrompt(t *testing.T) {
	t.Parallel()
	m := makeTestModel(t)
	if got := m.input.Prompt; got != "" {
		t.Fatalf("input prompt = %q, want empty (the box border is the only vertical line)", got)
	}
}

func TestInputStartsAtSingleRow(t *testing.T) {
	t.Parallel()
	m := makeTestModel(t)
	if got := m.input.Height(); got != 1 {
		t.Fatalf("input height = %d, want 1 (grows on multi-line content)", got)
	}
}

func TestInputPlaceholderInvitesUser(t *testing.T) {
	t.Parallel()
	m := makeTestModel(t)
	if !strings.Contains(m.input.Placeholder, "Ask") {
		t.Fatalf("placeholder = %q, want something inviting", m.input.Placeholder)
	}
}

func TestInsertNewlineBoundToCtrlJ(t *testing.T) {
	t.Parallel()
	m := makeTestModel(t)
	keys := m.input.KeyMap.InsertNewline.Keys()
	if len(keys) != 1 || keys[0] != "ctrl+j" {
		t.Fatalf("InsertNewline keys = %v, want exactly [ctrl+j] (Enter is reserved for submit)", keys)
	}
	// Sanity: ensure Enter is NOT in the InsertNewline bindings.
	for _, k := range keys {
		if k == "enter" || k == "ctrl+m" {
			t.Fatalf("Enter still bound to InsertNewline (%q); should submit instead", k)
		}
	}
}

func TestEnterSubmitsAndClearsInput(t *testing.T) {
	t.Parallel()
	m := makeTestModel(t)
	m.input.SetValue("hello")
	_, _ = m.handleInputKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.input.Value() != "" {
		t.Fatalf("input not cleared after submit: %q", m.input.Value())
	}
	// A user message should now be the most recent non-welcome entry.
	found := false
	for _, it := range m.transcript {
		if it.Kind == itemUser && it.Text == "hello" {
			found = true
		}
	}
	if !found {
		t.Fatalf("user message not appended; transcript = %+v", m.transcript)
	}
}

func TestEscOutsideTurnIsNoOp(t *testing.T) {
	t.Parallel()
	m := makeTestModel(t)
	before := len(m.transcript)
	_, _ = m.handleInputKey(tea.KeyMsg{Type: tea.KeyEsc})
	if len(m.transcript) != before {
		t.Fatalf("Esc outside a turn modified transcript: before=%d after=%d", before, len(m.transcript))
	}
}
