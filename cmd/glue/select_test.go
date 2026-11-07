package main

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/erain/glue"
)

func selectionKey(t *testing.T, m selectionMenu, key tea.KeyMsg) selectionMenu {
	t.Helper()
	next, _ := m.Update(key)
	return next.(selectionMenu)
}
func chooseSelection(t *testing.T, m selectionMenu, value string) selectionMenu {
	t.Helper()
	for i, choice := range m.choices {
		if choice == value {
			m.cursor = i
			return selectionKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
		}
	}
	t.Fatalf("choice %q absent from %v", value, m.choices)
	return m
}

func TestSelectionFlow(t *testing.T) {
	initial := modelSelection{Provider: "gemini", Model: "gemini-4-argon", Thinking: "high"}
	m := newSelectionMenu(initial)
	m = chooseSelection(t, m, "gemini")
	if m.choices[m.cursor] != "gemini-4-argon" {
		t.Fatal("initial model lost")
	}
	m = chooseSelection(t, m, "gemini-4-argon")
	if !reflect.DeepEqual(m.choices, []string{"default", "low", "medium", "high"}) || m.choices[m.cursor] != "high" {
		t.Fatal(m.choices)
	}
	m = chooseSelection(t, m, "medium")
	if !m.done || m.selection != (modelSelection{"gemini", "gemini-4-argon", "medium"}) {
		t.Fatalf("selection=%#v", m)
	}
}

func TestSelectionChangesProviderAndEffort(t *testing.T) {
	m := newSelectionMenu(modelSelection{"gemini", "gemini-4-argon", "medium"})
	m = chooseSelection(t, m, "deepseek")
	if m.choices[m.cursor] != "deepseek-flash" || m.selection.Model != "deepseek-flash" {
		t.Fatal("provider change retained foreign model")
	}
	m = chooseSelection(t, m, "deepseek-flash")
	if !reflect.DeepEqual(m.choices, []string{"default", "off", "low", "high", "max"}) || m.cursor != 0 {
		t.Fatal("unsupported effort did not reset", m.choices, m.cursor)
	}
	m = chooseSelection(t, m, "max")
	if !m.done || m.selection.Provider != "deepseek" || m.selection.Thinking != "max" {
		t.Fatal(m.selection)
	}
}

func TestSelectionCustomModel(t *testing.T) {
	m := newSelectionMenu(modelSelection{"gemini", "custom-existing", "high"})
	m = chooseSelection(t, m, "gemini")
	if m.choices[m.cursor] != "custom-existing" {
		t.Fatal("custom flag not preselected")
	}
	m = chooseSelection(t, m, "")
	m = selectionKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.enteringCustom {
		t.Fatal("blank custom accepted")
	}
	m = selectionKey(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("new-model")})
	m = selectionKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.selection.Model != "new-model" || !reflect.DeepEqual(m.choices, []string{"default"}) {
		t.Fatal(m.selection, m.choices)
	}
	m = chooseSelection(t, m, "default")
	if !m.done {
		t.Fatal("selection incomplete")
	}
}

func TestSelectionCancellationAndNavigation(t *testing.T) {
	for _, key := range []tea.KeyMsg{{Type: tea.KeyEsc}, {Type: tea.KeyCtrlC}} {
		for stage := 0; stage < 3; stage++ {
			m := newSelectionMenu(modelSelection{"gemini", "gemini-4-argon", "high"})
			if stage > 0 {
				m = chooseSelection(t, m, "gemini")
			}
			if stage > 1 {
				m = chooseSelection(t, m, "gemini-4-argon")
			}
			m = selectionKey(t, m, key)
			if !m.cancelled || m.done {
				t.Fatalf("stage %d did not cancel", stage)
			}
		}
	}
	m := newSelectionMenu(modelSelection{})
	m = selectionKey(t, m, tea.KeyMsg{Type: tea.KeyUp})
	if m.cursor != 0 {
		t.Fatal("moved above first row")
	}
	for range len(m.choices) + 2 {
		m = selectionKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	}
	if m.cursor != len(m.choices)-1 {
		t.Fatal("moved below final row")
	}
}

func TestSelectRequiresTerminalBeforeProviderConstruction(t *testing.T) {
	var errs bytes.Buffer
	err := runCommand(context.Background(), []string{"--select", "--prompt", "hello"}, strings.NewReader(""), io.Discard, &errs, func(string) (glue.Provider, error) { t.Fatal("provider constructed"); return nil, nil })
	if err == nil || !strings.Contains(err.Error(), "requires terminal") {
		t.Fatal(err)
	}
}
