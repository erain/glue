package tui

import (
	_ "embed"
	"strings"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
)

// Glamour Catppuccin theme files. Mocha for dark terminals, Latte for
// light. We pick at construction time based on lipgloss's terminal
// background heuristic.
//
//go:embed glamour-mocha.json
var glamourMochaJSON []byte

//go:embed glamour-latte.json
var glamourLatteJSON []byte

// markdownRenderer wraps charmbracelet/glamour to render assistant
// markdown at the current viewport width. It is created lazily after
// the first WindowSizeMsg (glamour needs a width) and rebuilt on
// resize.
type markdownRenderer struct {
	width int
	glam  *glamour.TermRenderer
}

func newMarkdownRenderer(width int) *markdownRenderer {
	r := &markdownRenderer{}
	r.Resize(width)
	return r
}

// Resize rebuilds the underlying renderer at a new width. Glamour's
// word-wrap is fixed at construction.
func (r *markdownRenderer) Resize(width int) {
	if width < 20 {
		width = 20
	}
	if r.width == width && r.glam != nil {
		return
	}
	// Pick the Catppuccin flavor matching the terminal background. If
	// lipgloss can't decide we default to Mocha (the more common case
	// for coding-agent users).
	styleJSON := glamourMochaJSON
	if !lipgloss.HasDarkBackground() {
		styleJSON = glamourLatteJSON
	}
	g, err := glamour.NewTermRenderer(
		glamour.WithStylesFromJSONBytes(styleJSON),
		glamour.WithWordWrap(width),
		glamour.WithEmoji(),
	)
	if err != nil {
		// Fall back to the auto style if our JSON is malformed — better
		// to render plain than to crash the TUI.
		g, err = glamour.NewTermRenderer(
			glamour.WithAutoStyle(),
			glamour.WithWordWrap(width),
			glamour.WithEmoji(),
		)
		if err != nil {
			r.glam = nil
			return
		}
	}
	r.glam = g
	r.width = width
}

// Render returns the styled markdown. On any failure it falls back to
// the original input so the assistant text is never lost.
func (r *markdownRenderer) Render(s string) string {
	if r == nil || r.glam == nil || strings.TrimSpace(s) == "" {
		return s
	}
	out, err := r.glam.Render(s)
	if err != nil {
		return s
	}
	// Glamour likes to add leading/trailing blank lines; trim so the
	// output sits cleanly under the "assistant" header.
	return strings.Trim(out, "\n")
}
