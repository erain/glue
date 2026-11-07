package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/erain/glue/providers"
)

type modelSelection struct{ Provider, Model, Thinking string }

type selectionMenu struct {
	selection      modelSelection
	initial        modelSelection
	stage          int
	cursor         int
	choices        []string
	labels         []string
	custom         textinput.Model
	enteringCustom bool
	cancelled      bool
	done           bool
}

func newSelectionMenu(initial modelSelection) selectionMenu {
	input := textinput.New()
	input.Placeholder = "Model ID"
	input.CharLimit = 256
	m := selectionMenu{initial: initial, selection: initial, custom: input}
	m.setChoices(providers.Known(), initial.Provider)
	return m
}

func (m *selectionMenu) setChoices(choices []string, selected string) {
	m.choices = choices
	m.labels = append([]string(nil), choices...)
	m.cursor = 0
	for i, value := range choices {
		if value == selected {
			m.cursor = i
		}
	}
}

func (m selectionMenu) Init() tea.Cmd { return nil }
func (m selectionMenu) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "ctrl+c", "esc":
			m.cancelled = true
			return m, tea.Quit
		}
		if m.enteringCustom {
			if key.String() == "enter" {
				value := normalizeModel(strings.TrimSpace(m.custom.Value()))
				if value != "" {
					m.selection.Model = value
					m.enteringCustom = false
					m.thinkingChoices()
				}
				return m, nil
			}
			var cmd tea.Cmd
			m.custom, cmd = m.custom.Update(msg)
			return m, cmd
		}
		switch key.String() {
		case "q":
			m.cancelled = true
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor+1 < len(m.choices) {
				m.cursor++
			}
		case "enter":
			chosen := m.choices[m.cursor]
			switch m.stage {
			case 0:
				m.selection.Provider = chosen
				f, _ := providers.Lookup(chosen)
				selected := f.DefaultModel
				if chosen == m.initial.Provider && m.initial.Model != "" {
					selected = normalizeModel(m.initial.Model)
				}
				choices := []string{}
				found := false
				for _, model := range providers.Models(chosen) {
					choices = append(choices, model.ID)
					found = found || model.ID == selected
				}
				if !found && selected != "" {
					choices = append(choices, selected)
				}
				choices = append(choices, "") // empty ID opens custom model entry
				m.selection.Model = selected
				m.stage = 1
				m.setChoices(choices, selected)
				m.labels[len(choices)-1] = "Enter a custom model ID…"
				for i, choice := range choices {
					if choice == f.DefaultModel {
						m.labels[i] += " (provider default)"
					}
				}
			case 1:
				if chosen == "" {
					m.enteringCustom = true
					return m, m.custom.Focus()
				}
				m.selection.Model = chosen
				m.thinkingChoices()
			case 2:
				m.selection.Thinking = chosen
				m.done = true
				return m, tea.Quit
			}
		}
	}
	if m.enteringCustom {
		var cmd tea.Cmd
		m.custom, cmd = m.custom.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *selectionMenu) thinkingChoices() {
	m.stage = 2
	choices := []string{"default"}
	for _, model := range providers.Models(m.selection.Provider) {
		if model.ID == m.selection.Model {
			choices = append(choices, model.ThinkingLevels...)
		}
	}
	m.setChoices(choices, m.initial.Thinking)
	m.labels[0] = "Provider default"
}

func (m selectionMenu) View() string {
	if m.done || m.cancelled {
		return ""
	}
	var b strings.Builder
	titles := []string{"Choose provider", "Choose model", "Choose thinking level"}
	fmt.Fprintf(&b, "\n%s\n", titles[m.stage])
	if m.stage > 0 {
		fmt.Fprintf(&b, "%s / %s\n", m.selection.Provider, m.selection.Model)
	}
	if m.enteringCustom {
		return b.String() + m.custom.View() + "\nEnter to confirm · Esc to cancel\n"
	}
	for i, label := range m.labels {
		marker := "  "
		if i == m.cursor {
			marker = "> "
		}
		fmt.Fprintf(&b, "%s%s\n", marker, label)
	}
	b.WriteString("↑/↓ to move · Enter to select · Esc to cancel\n")
	return b.String()
}

func selectModel(ctx context.Context, stdin io.Reader, stderr io.Writer, initial modelSelection) (modelSelection, error) {
	result, err := tea.NewProgram(newSelectionMenu(initial), tea.WithInput(stdin), tea.WithOutput(stderr), tea.WithContext(ctx)).Run()
	if err != nil {
		return modelSelection{}, fmt.Errorf("model selection: %w", err)
	}
	menu := result.(selectionMenu)
	if menu.cancelled || !menu.done {
		return modelSelection{}, fmt.Errorf("model selection cancelled")
	}
	if _, err := providers.ThinkingOptions(menu.selection.Provider, menu.selection.Model, menu.selection.Thinking); err != nil {
		return modelSelection{}, err
	}
	return menu.selection, nil
}
