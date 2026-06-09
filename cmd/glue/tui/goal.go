package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/erain/glue"
)

// goalMaxIterations mirrors the library default for GoalSpec.MaxIterations.
// The TUI sets it explicitly on the spec so the status bar can render an
// honest "iter 2/10" denominator without reaching into glue internals.
const goalMaxIterations = 10

// goalState tracks the single in-TUI goal pursuit. The loop itself runs in
// a background goroutine (Agent.PursueGoal on its own session ids); this
// struct only mirrors what the Emit events report, for the live card and
// the status bar.
type goalState struct {
	objective string
	cancel    context.CancelFunc

	running bool
	paused  bool            // user ran /goal pause; cancel maps to "paused", not an error
	status  glue.GoalStatus // terminal status once finished; "" while running or paused

	iteration     int
	maxIterations int
	checklist     []glue.ChecklistItem
	usage         glue.Usage
	summary       string // last checker verdict summary

	// cardIdx is the transcript index of the live goal card, updated in
	// place as events arrive. -1 means not created yet (or detached after
	// a transcript reset — the next event re-appends it).
	cardIdx int
}

// handleSlashGoal dispatches the /goal command family. A bare single-word
// subcommand manages the current goal; anything else is a new objective.
func (m *Model) handleSlashGoal(arg string) (tea.Model, tea.Cmd) {
	switch strings.ToLower(arg) {
	case "":
		if m.goal != nil {
			m.appendBlock("Goal", m.goal.cardBody())
		} else {
			m.appendSystem("usage: /goal <objective> — then /goal status · pause · resume · clear")
		}
	case "status":
		if m.goal == nil {
			m.appendSystem("/goal status: no goal yet — start one with /goal <objective>")
		} else {
			m.appendBlock("Goal", m.goal.cardBody())
		}
	case "pause":
		switch {
		case m.goal == nil || !m.goal.running:
			m.appendSystem("/goal pause: no goal running")
		default:
			m.goal.paused = true
			m.goal.cancel()
			m.appendSystem("pausing goal after the in-flight call returns…")
		}
	case "resume":
		switch {
		case m.goal == nil:
			m.appendSystem("/goal resume: no goal to resume — start one with /goal <objective>")
		case m.goal.running:
			m.appendSystem("/goal resume: goal is already running")
		case len(m.goal.checklist) == 0:
			m.appendSystem("/goal resume: no checklist captured; start over with /goal <objective>")
		default:
			return m.startGoal(m.goal.objective, m.goal.checklist)
		}
	case "clear", "stop":
		if m.goal == nil {
			m.appendSystem("/goal clear: no goal to clear")
		} else {
			if m.goal.running {
				m.goal.paused = true // suppress the "goal error" line from the cancel
				m.goal.cancel()
			}
			m.goal = nil
			m.appendSystem("goal cleared.")
		}
	default:
		return m.startGoal(arg, nil)
	}
	m.rerender()
	return m, nil
}

// startGoal launches Agent.PursueGoal in the background. A non-empty seed
// is the resume path: the loop skips planning and continues from the last
// verified checklist.
func (m *Model) startGoal(objective string, seed []glue.ChecklistItem) (tea.Model, tea.Cmd) {
	if m.goal != nil && m.goal.running {
		m.appendSystem("/goal: a goal is already running — /goal status to inspect, /goal pause or /goal clear first.")
		m.rerender()
		return m, nil
	}
	if m.send == nil {
		m.appendSystem("tui: program send not initialised")
		m.rerender()
		return m, nil
	}

	ctx, cancel := context.WithCancel(m.ctx)
	g := &goalState{
		objective:     objective,
		cancel:        cancel,
		running:       true,
		maxIterations: goalMaxIterations,
		checklist:     append([]glue.ChecklistItem(nil), seed...),
		cardIdx:       -1,
	}
	m.goal = g

	send := m.send
	agent := m.cfg.Agent
	spec := glue.GoalSpec{
		Objective:     objective,
		SessionPrefix: "goal-" + shortID(),
		Model:         m.cfg.Model,
		MaxIterations: goalMaxIterations,
		Checklist:     seed,
		Permission:    m.perm,
		Emit:          func(ev glue.GoalEvent) { send(goalEventMsg{Ev: ev}) },
	}
	go func() {
		defer cancel()
		res, err := agent.PursueGoal(ctx, spec)
		send(goalDoneMsg{Res: res, Err: err})
	}()

	verb := "pursuing"
	if len(seed) > 0 {
		verb = "resuming"
	}
	m.appendSystem(fmt.Sprintf("%s goal: %s  (/goal status · pause · clear)", verb, objective))
	m.rerender()
	return m, m.spinner.Tick
}

// goalRunning reports whether a goal loop is in flight (keeps the spinner
// ticking even when no chat turn is active).
func (m *Model) goalRunning() bool {
	return m.goal != nil && m.goal.running
}

// handleGoalEvent mirrors one Emit event into the goal state and refreshes
// the live card.
func (m *Model) handleGoalEvent(ev glue.GoalEvent) {
	g := m.goal
	if g == nil {
		return
	}
	switch ev.Type {
	case glue.GoalEventPlan:
		g.checklist = ev.Checklist
	case glue.GoalEventIterationStart:
		g.iteration = ev.Iteration
	case glue.GoalEventVerdict:
		g.checklist = ev.Checklist
		g.summary = ev.Message
	}
	g.usage = ev.Usage
	m.updateGoalCard()
}

// handleGoalDone records the terminal result. A pause shows as "paused"
// rather than surfacing the underlying context cancellation as an error.
func (m *Model) handleGoalDone(msg goalDoneMsg) {
	g := m.goal
	if g == nil {
		return // cleared while the final message was in flight
	}
	g.running = false
	if len(msg.Res.Checklist) > 0 {
		g.checklist = msg.Res.Checklist
	}
	if msg.Res.Summary != "" {
		g.summary = msg.Res.Summary
	}
	g.usage = msg.Res.Usage
	if msg.Res.Iterations > g.iteration {
		g.iteration = msg.Res.Iterations
	}
	switch {
	case g.paused && errors.Is(msg.Err, context.Canceled):
		m.appendSystem("goal paused — /goal resume continues from the verified checklist.")
	case msg.Err != nil:
		g.status = glue.GoalErrored
		m.appendSystem("goal error: " + msg.Err.Error())
	default:
		g.status = msg.Res.Status
		note := fmt.Sprintf("goal %s after %d iteration(s): %s", g.status, g.iteration, doneFraction(g.checklist))
		if g.summary != "" {
			note += " — " + g.summary
		}
		m.appendSystem(note)
	}
	m.updateGoalCard()
}

// updateGoalCard rewrites the live goal card in place, or (re)appends it
// when missing — e.g. before the first event, or after /clear or a session
// switch reset the transcript under it.
func (m *Model) updateGoalCard() {
	g := m.goal
	if g == nil {
		return
	}
	body := g.cardBody()
	if g.cardIdx >= 0 && g.cardIdx < len(m.transcript) &&
		m.transcript[g.cardIdx].Kind == itemBlock && m.transcript[g.cardIdx].BlockTitle == "Goal" {
		m.transcript[g.cardIdx].BlockBody = body
		return
	}
	m.transcript = append(m.transcript, transcriptItem{Kind: itemBlock, BlockTitle: "Goal", BlockBody: body})
	g.cardIdx = len(m.transcript) - 1
}

// detachGoalCard forgets the card's transcript index after a transcript
// reset; the next event re-appends the card instead of clobbering whatever
// now lives at the stale index.
func (m *Model) detachGoalCard() {
	if m.goal != nil {
		m.goal.cardIdx = -1
	}
}

// cardBody renders the goal card / /goal status block.
func (g *goalState) cardBody() string {
	var b strings.Builder
	b.WriteString("  " + g.objective + "\n\n")
	b.WriteString(renderGoalChecklist(g.checklist))
	b.WriteString("\n\n  " + g.stateLine())
	if g.summary != "" {
		b.WriteString("\n  " + keyHint.Render("verdict: "+truncateGoalText(g.summary, 80)))
	}
	return b.String()
}

// stateLine is the one-line progress summary shared by the card footer.
func (g *goalState) stateLine() string {
	switch {
	case g.running && g.iteration == 0:
		return toolWarning.Render("planning…")
	case g.running:
		return toolWarning.Render(fmt.Sprintf("iter %d/%d", g.iteration, g.maxIterations)) +
			keyHint.Render(fmt.Sprintf(" · %s ✓ · %s tok", doneFraction(g.checklist), formatTokens(g.usage.TotalTokens)))
	case g.paused:
		return toolWarning.Render("paused") +
			keyHint.Render(fmt.Sprintf(" · %s ✓ · /goal resume to continue", doneFraction(g.checklist)))
	default:
		return goalStatusStyle(g.status).Render(string(g.status)) +
			keyHint.Render(fmt.Sprintf(" · iter %d · %s ✓ · %s tok", g.iteration, doneFraction(g.checklist), formatTokens(g.usage.TotalTokens)))
	}
}

// statusSegment is the compact status-bar chip, e.g.
// "◎ goal · iter 2/10 · 1/4 ✓ · 12.3k tok".
func (g *goalState) statusSegment() string {
	switch {
	case g.running && g.iteration == 0:
		return toolWarning.Render("◎ goal · planning")
	case g.running:
		return toolWarning.Render(fmt.Sprintf("◎ goal · iter %d/%d · %s ✓ · %s tok",
			g.iteration, g.maxIterations, doneFraction(g.checklist), formatTokens(g.usage.TotalTokens)))
	case g.paused:
		return toolWarning.Render(fmt.Sprintf("◎ goal paused · %s ✓", doneFraction(g.checklist)))
	default:
		return goalStatusStyle(g.status).Render("◎ goal " + string(g.status))
	}
}

func goalStatusStyle(s glue.GoalStatus) lipgloss.Style {
	switch s {
	case glue.GoalAchieved:
		return toolOk
	case glue.GoalErrored, glue.GoalBlocked:
		return toolErrSty
	default:
		return toolWarning
	}
}

func renderGoalChecklist(items []glue.ChecklistItem) string {
	if len(items) == 0 {
		return keyHint.Render("  (no checklist yet)")
	}
	lines := make([]string, 0, len(items))
	for _, it := range items {
		box := keyHint.Render("[ ]")
		if it.Done {
			box = toolOk.Render("[x]")
		}
		line := "  " + box + " " + it.Title
		if it.Evidence != "" {
			line += " " + keyHint.Render("— "+truncateGoalText(it.Evidence, 60))
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// doneFraction renders "2/5" for the checklist completion count.
func doneFraction(items []glue.ChecklistItem) string {
	done := 0
	for _, it := range items {
		if it.Done {
			done++
		}
	}
	return fmt.Sprintf("%d/%d", done, len(items))
}

// formatTokens compacts a token count for chrome: 987, 12.3k, 1.2M.
func formatTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func truncateGoalText(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
