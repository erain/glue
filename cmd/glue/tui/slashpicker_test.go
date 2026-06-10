package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestDetectSlashTrigger(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		// Opens
		{"/", "", true},
		{"/h", "h", true},
		{"/model", "model", true},

		// Does NOT open
		{"", "", false},
		{"hello", "", false},
		{"/model gpt-5", "", false}, // space → entering an argument
		{" /help", "", false},       // leading space: not a bare slash token
		{"a/b", "", false},
		{"/multi\nline", "", false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.in, func(t *testing.T) {
			got, ok := detectSlashTrigger(c.in)
			if ok != c.wantOK || got != c.want {
				t.Fatalf("detectSlashTrigger(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.wantOK)
			}
		})
	}
}

func TestSlashRefilterEmptyMatchesAll(t *testing.T) {
	t.Parallel()
	p := newSlashPicker()
	if len(p.matches) != len(p.specs) || len(p.specs) == 0 {
		t.Fatalf("empty query matched %d of %d specs", len(p.matches), len(p.specs))
	}
}

func TestSlashRefilterPrefixMatch(t *testing.T) {
	t.Parallel()
	p := newSlashPicker()

	p.refilter("mo")
	if got, ok := p.selected(); !ok || got.Name != "model" {
		t.Fatalf("/mo selected %+v (ok=%v), want model", got, ok)
	}
	if len(p.matches) != 1 {
		t.Fatalf("/mo matched %d, want 1", len(p.matches))
	}

	// Alias prefixes match too: "qui" → /exit (alias /quit).
	p.refilter("qui")
	if got, ok := p.selected(); !ok || got.Name != "exit" {
		t.Fatalf("/qui selected %+v (ok=%v), want exit via alias", got, ok)
	}

	// No command starts with "zzz".
	p.refilter("zzz")
	if len(p.matches) != 0 {
		t.Fatalf("/zzz matched %d, want 0", len(p.matches))
	}
	if _, ok := p.selected(); ok {
		t.Fatal("selected() should be !ok with no matches")
	}
}

func TestSlashMatchesStayAlphabetical(t *testing.T) {
	t.Parallel()
	p := newSlashPicker()
	p.refilter("c") // clear, clone, compact
	var names []string
	for _, idx := range p.matches {
		names = append(names, p.specs[idx].Name)
	}
	want := []string{"clear", "clone", "compact"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("/c matches = %v, want %v (alphabetical, stable)", names, want)
	}
}

func TestSlashExactMatch(t *testing.T) {
	t.Parallel()
	p := newSlashPicker()
	p.refilter("help")
	if !p.exactMatch() {
		t.Fatal("exactMatch() = false for fully-typed /help")
	}
	p.refilter("hel")
	if p.exactMatch() {
		t.Fatal("exactMatch() = true for partial /hel")
	}
}

func TestApplySlashSelection(t *testing.T) {
	t.Parallel()
	if got := applySlashSelection("model"); got != "/model " {
		t.Fatalf("got %q, want %q", got, "/model ")
	}
}

func TestSlashPickerUpDownClamps(t *testing.T) {
	t.Parallel()
	p := newSlashPicker()
	for i := 0; i < 50; i++ {
		p.down()
	}
	if p.cursor != len(p.matches)-1 {
		t.Fatalf("down past end: cursor = %d, want %d", p.cursor, len(p.matches)-1)
	}
	for i := 0; i < 50; i++ {
		p.up()
	}
	if p.cursor != 0 {
		t.Fatalf("up past start: cursor = %d", p.cursor)
	}
}

func TestSlashPickerOpensAndTabCompletes(t *testing.T) {
	t.Parallel()
	m := makeTestModel(t)

	m.input.SetValue("/mo")
	m.refreshPickers()
	if m.slashPicker == nil {
		t.Fatal("slashPicker did not open for /mo")
	}
	if m.atPicker != nil {
		t.Fatal("atPicker should be closed while slashPicker is open")
	}

	if _, _ = m.slashPickerAccept(); m.input.Value() != "/model " {
		t.Fatalf("after accept, input = %q, want %q", m.input.Value(), "/model ")
	}
	if m.slashPicker != nil {
		t.Fatal("slashPicker should close after accept")
	}
}

func TestSlashPickerClosesOnSpace(t *testing.T) {
	t.Parallel()
	m := makeTestModel(t)
	m.input.SetValue("/model")
	m.refreshPickers()
	if m.slashPicker == nil {
		t.Fatal("expected open picker for /model")
	}
	m.input.SetValue("/model ") // typed a space → now entering an argument
	m.refreshPickers()
	if m.slashPicker != nil {
		t.Fatal("slashPicker should close once an argument is being typed")
	}
}

func TestSlashPickerEnterRunsFullyTypedCommand(t *testing.T) {
	t.Parallel()
	m := makeTestModel(t)
	m.input.SetValue("/help")
	m.refreshPickers()
	if m.slashPicker == nil {
		t.Fatal("expected open picker for /help")
	}
	// Enter on an exact match runs the command (does not re-complete).
	_, _ = m.handleInputKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.input.Value() != "" {
		t.Fatalf("input not cleared after running command: %q", m.input.Value())
	}
	if m.slashPicker != nil {
		t.Fatal("slashPicker should be closed after running")
	}
	var sawCommands bool
	for _, it := range m.transcript {
		if it.Kind == itemBlock && strings.Contains(it.render(renderCtx{width: 80}), "Commands") {
			sawCommands = true
		}
	}
	if !sawCommands {
		t.Fatal("/help did not append the Commands block")
	}
}

func TestSlashPickerEnterRunsSelectedNoArgCommand(t *testing.T) {
	t.Parallel()
	m := makeTestModel(t)
	// "/cl" highlights /clear (a no-arg command) but isn't an exact match.
	m.input.SetValue("/cl")
	m.refreshPickers()
	sel, ok := m.slashPicker.selected()
	if !ok || sel.Name != "clear" || sel.Args != "" {
		t.Fatalf("precondition: selected = %+v (ok=%v), want no-arg clear", sel, ok)
	}
	_, _ = m.handleInputKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.input.Value() != "" {
		t.Fatalf("no-arg command should run on Enter; input = %q", m.input.Value())
	}
	if m.cfg.SessionID == "tui:test" {
		t.Fatal("/clear did not run (session id unchanged)")
	}
}

func TestSlashPickerEnterCompletesArgCommand(t *testing.T) {
	t.Parallel()
	m := makeTestModel(t)
	// "/mod" highlights /model, which takes an <id> argument.
	m.input.SetValue("/mod")
	m.refreshPickers()
	_, _ = m.handleInputKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.input.Value() != "/model " {
		t.Fatalf("arg command should complete (not run) on Enter; input = %q, want %q", m.input.Value(), "/model ")
	}
	if m.slashPicker != nil {
		t.Fatal("picker should close after completing")
	}
}

func TestSlashPickerEscClosesKeepingText(t *testing.T) {
	t.Parallel()
	m := makeTestModel(t)
	m.input.SetValue("/mo")
	m.refreshPickers()
	_, _ = m.handleInputKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.slashPicker != nil {
		t.Fatal("Esc should close the slash picker")
	}
	if m.input.Value() != "/mo" {
		t.Fatalf("Esc altered input: %q, want /mo preserved", m.input.Value())
	}
}

func TestDescribeCommandsAndPickerShareSpecs(t *testing.T) {
	t.Parallel()
	body := describeCommands()
	for _, s := range slashSpecs() {
		if !strings.Contains(body, "/"+s.Name) {
			t.Errorf("describeCommands missing /%s", s.Name)
		}
	}
}

// TestSlashPickerShowsEveryCommandOnBareSlash pins the fix for #356: the
// popup must list the entire (small, bounded) command set with no scroll
// window — hiding entries made `/` look like incomplete autocomplete.
func TestSlashPickerShowsEveryCommandOnBareSlash(t *testing.T) {
	t.Parallel()
	p := newSlashPicker()
	if got := p.popupRows(); got != len(slashSpecs()) {
		t.Fatalf("popupRows = %d, want %d (every command visible)", got, len(slashSpecs()))
	}
	out := renderSlashPicker(p, 120)
	for _, s := range slashSpecs() {
		if !strings.Contains(out, "/"+s.Name) {
			t.Errorf("bare / popup missing /%s:\n%s", s.Name, out)
		}
	}
	if strings.Contains(out, "more") {
		t.Fatalf("slash popup should never show a scroll indicator:\n%s", out)
	}
}
