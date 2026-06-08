package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestLayoutCapsViewportAtBodyMaxWidth asserts that on a wide terminal
// the transcript viewport gets capped at bodyMaxWidth instead of
// stretching across the screen — fixes #307.
func TestLayoutCapsViewportAtBodyMaxWidth(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		termW    int
		wantVPW  int
	}{
		{"narrow stays uncapped", 60, 60},
		{"at exactly cap", bodyMaxWidth, bodyMaxWidth},
		{"wide gets capped", 200, bodyMaxWidth},
		{"very wide gets capped", 400, bodyMaxWidth},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			m := newTestModel(t)
			m.width = c.termW
			m.height = 40
			m.layout()
			if m.viewport.Width != c.wantVPW {
				t.Fatalf("viewport.Width = %d, want %d (term=%d)",
					m.viewport.Width, c.wantVPW, c.termW)
			}
		})
	}
}

// TestViewCentersBodyOnWideTerminal asserts that when the terminal is
// wider than bodyMaxWidth, the rendered View output has leading
// horizontal padding (lipgloss centers the body) so the conversation
// doesn't pin to the left edge.
func TestViewCentersBodyOnWideTerminal(t *testing.T) {
	t.Parallel()
	m := newTestModel(t)
	// Simulate WindowSizeMsg for a wide terminal.
	m.Update(tea.WindowSizeMsg{Width: 200, Height: 40})

	out := m.View()
	if out == "" {
		t.Fatal("View returned empty on a ready model")
	}
	// On a 200-col terminal the body is rendered at 100 cols and centered.
	// We check that at least one of the body rows starts with whitespace
	// (the centering pad) rather than a glyph at column 0.
	lines := strings.Split(out, "\n")
	hasLeadingPad := false
	for _, line := range lines {
		// Skip header and status which run edge-to-edge.
		if line == "" || strings.HasPrefix(line, " glue") {
			continue
		}
		// Strip ANSI escape sequences before checking the leading column.
		stripped := stripANSI(line)
		if stripped == "" {
			continue
		}
		if strings.HasPrefix(stripped, "          ") { // 10+ spaces = centered pad
			hasLeadingPad = true
			break
		}
	}
	if !hasLeadingPad {
		t.Errorf("expected centering pad on wide terminal; got View:\n%s",
			truncate(out, 400))
	}
}

// TestLayoutKeepsViewportFullOnNarrowTerminal asserts that a terminal
// narrower than bodyMaxWidth still gets the viewport at full width —
// no horizontal pad on narrow terminals.
func TestLayoutKeepsViewportFullOnNarrowTerminal(t *testing.T) {
	t.Parallel()
	m := newTestModel(t)
	m.width = 80
	m.height = 40
	m.layout()
	if m.viewport.Width != 80 {
		t.Fatalf("viewport.Width = %d, want 80 (no pad on narrow)", m.viewport.Width)
	}
}

// newTestModel builds a minimal Model wired enough to call Update and
// View without panicking — no real Agent, no real store. We rely on
// the fact that layout/view paths only need the textarea, viewport,
// spinner, and width fields to be initialized.
func newTestModel(t *testing.T) *Model {
	t.Helper()
	return newModel(context.Background(), Config{})
}

// (uses stripANSI from transcript_test.go)
