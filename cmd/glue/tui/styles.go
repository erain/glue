package tui

import "github.com/charmbracelet/lipgloss"

// Palette — mirrors the homepage purple accent so the brand is coherent
// across web and terminal. AdaptiveColor handles dark vs. light terminals.
var (
	accent   = lipgloss.Color("#6d28d9")
	ink      = lipgloss.AdaptiveColor{Light: "#0f172a", Dark: "#e2e8f0"}
	inkSoft  = lipgloss.AdaptiveColor{Light: "#475569", Dark: "#94a3b8"}
	inkMuted = lipgloss.AdaptiveColor{Light: "#94a3b8", Dark: "#64748b"}
	border   = lipgloss.AdaptiveColor{Light: "#e2e8f0", Dark: "#334155"}
	okColor  = lipgloss.Color("#10b981")
	errColor = lipgloss.Color("#ef4444")
	warnCol  = lipgloss.Color("#f59e0b")
)

var (
	headerStyle = lipgloss.NewStyle().
			Foreground(ink).
			Padding(0, 1)

	headerBrand = lipgloss.NewStyle().Foreground(accent).Bold(true)

	statusStyle = lipgloss.NewStyle().
			Foreground(inkMuted).
			Padding(0, 1)

	inputBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(border).
			Padding(0, 1)

	userPrefix = lipgloss.NewStyle().Foreground(accent).Bold(true)
	asstPrefix = lipgloss.NewStyle().Foreground(inkSoft)
	sysLine    = lipgloss.NewStyle().Foreground(inkMuted)

	toolHeader  = lipgloss.NewStyle().Foreground(inkSoft)
	toolOk      = lipgloss.NewStyle().Foreground(okColor)
	toolErrSty  = lipgloss.NewStyle().Foreground(errColor)
	toolWarning = lipgloss.NewStyle().Foreground(warnCol)
	toolBody    = lipgloss.NewStyle().Foreground(inkSoft)

	diffAddSty = lipgloss.NewStyle().Foreground(okColor)
	diffDelSty = lipgloss.NewStyle().Foreground(errColor)

	permBox = lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(warnCol).
		Padding(0, 1)

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

	// welcomeAccent is the soft purple used for example prompts.
	welcomeAccent = lipgloss.NewStyle().Foreground(accent).Bold(true)
)
