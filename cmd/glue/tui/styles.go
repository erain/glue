package tui

import "github.com/charmbracelet/lipgloss"

// Palette — Catppuccin (https://github.com/catppuccin/catppuccin).
//
// Mocha for dark terminals, Latte for light terminals, picked at render
// time by lipgloss.AdaptiveColor. Both families are intentionally
// pastel: every color sits at similar lightness/saturation so accents
// don't visually shout. The role names below are stable across the two
// flavors — only the hex values change.
//
// Brand: glue's purple anchors on Catppuccin's `mauve`, which is close
// enough to the homepage #6d28d9 that the brand reads as the same color
// across web and terminal, but in a hue family that coexists with the
// rest of the palette in a terminal.
//
// Reference flavor hexes for grep-ability:
//   Mocha base #1e1e2e · text #cdd6f4 · mauve #cba6f7 · green #a6e3a1
//          red #f38ba8 · peach #fab387 · subtext0 #a6adc8 · overlay1 #7f849c
//          surface1 #45475a · lavender #b4befe
//   Latte base #eff1f5 · text #4c4f69 · mauve #8839ef · green #40a02b
//          red #d20f39 · peach #fe640b · subtext0 #6c6f85 · overlay1 #8c8fa1
//          surface1 #bcc0cc · lavender #7287fd
var (
	// accent — mauve. Brand color: user prefix, headers, focused chrome.
	accent = lipgloss.AdaptiveColor{Light: "#8839ef", Dark: "#cba6f7"}

	// accentSoft — lavender. Used for secondary brand emphasis (welcome
	// card example prompts, picker selection dim row) where bold mauve
	// would overpower.
	accentSoft = lipgloss.AdaptiveColor{Light: "#7287fd", Dark: "#b4befe"}

	// ink — text. Primary foreground for body text.
	ink = lipgloss.AdaptiveColor{Light: "#4c4f69", Dark: "#cdd6f4"}

	// inkSoft — subtext0. Secondary text (assistant prefix, body of
	// tool cards, picker rows).
	inkSoft = lipgloss.AdaptiveColor{Light: "#6c6f85", Dark: "#a6adc8"}

	// inkMuted — overlay1. Hints, key cap reminders, status bar.
	inkMuted = lipgloss.AdaptiveColor{Light: "#8c8fa1", Dark: "#7f849c"}

	// border — surface1. Block, input, and picker borders.
	border = lipgloss.AdaptiveColor{Light: "#bcc0cc", Dark: "#45475a"}

	// okColor — green. Success markers, diff `+` lines.
	okColor = lipgloss.AdaptiveColor{Light: "#40a02b", Dark: "#a6e3a1"}

	// errColor — red. Errors, diff `-` lines.
	errColor = lipgloss.AdaptiveColor{Light: "#d20f39", Dark: "#f38ba8"}

	// warnCol — peach. Warnings, --yolo chip, spinner, permission key caps.
	warnCol = lipgloss.AdaptiveColor{Light: "#fe640b", Dark: "#fab387"}
)

var (
	headerStyle = lipgloss.NewStyle().
			Foreground(ink).
			Padding(0, 1)

	headerBrand = lipgloss.NewStyle().Foreground(accent).Bold(true)

	statusStyle = lipgloss.NewStyle().
			Foreground(inkMuted).
			Padding(0, 1)

	// Accent the input box so "type here" is unambiguous. The textarea
	// is always focused while the TUI is running so we don't bother with
	// a blurred variant.
	inputBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(accent).
			Padding(0, 1)

	userPrefix = lipgloss.NewStyle().Foreground(accent).Bold(true)
	asstPrefix = lipgloss.NewStyle().Foreground(inkSoft)
	sysLine    = lipgloss.NewStyle().Foreground(inkMuted).Italic(true)

	toolHeader  = lipgloss.NewStyle().Foreground(inkSoft)
	toolOk      = lipgloss.NewStyle().Foreground(okColor)
	toolErrSty  = lipgloss.NewStyle().Foreground(errColor)
	toolWarning = lipgloss.NewStyle().Foreground(warnCol)
	toolBody    = lipgloss.NewStyle().Foreground(inkSoft)

	diffAddSty = lipgloss.NewStyle().Foreground(okColor)
	diffDelSty = lipgloss.NewStyle().Foreground(errColor)

	// permKey renders a single keyboard key cap inside the in-card
	// permission prompt: `[a]`, `[s]`, etc.
	permKey = lipgloss.NewStyle().Foreground(warnCol).Bold(true)

	keyHint = lipgloss.NewStyle().Foreground(inkMuted)

	// blockBox / blockTitle render /help, /tools, and the welcome card.
	blockBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(border).
			Padding(0, 1)
	blockTitle = lipgloss.NewStyle().Foreground(accent).Bold(true)

	// turnSeparator renders the thin rule between turns.
	turnSeparator = lipgloss.NewStyle().Foreground(border).Render

	// welcomeAccent is the soft mauve used for example prompts. We use
	// accentSoft (lavender) so it's clearly brand-adjacent but doesn't
	// compete with the bold mauve user prefix.
	welcomeAccent = lipgloss.NewStyle().Foreground(accentSoft).Bold(true)
)
