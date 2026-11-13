// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// JobRow is one remote job in the /jobs panel.
type JobRow struct {
	ID, Status, Objective string
	Updated               time.Time
}

// JobsBridge gives the TUI access to remote jobs. cmd/glue implements it
// with the glue job commands.
type JobsBridge interface {
	// List returns recent jobs. It must not wake suspended tasks: the panel
	// and the status bar call it on a timer.
	List(ctx context.Context) ([]JobRow, error)
	// Transcript renders a job's history so far, as glue job logs does.
	Transcript(ctx context.Context, id string) (string, error)
	// Pull brings the job's result into the workspace (glue job pull) and
	// describes what it did.
	Pull(ctx context.Context, id string) (string, error)
}

const (
	jobsListEvery       = 10 * time.Second
	jobsTranscriptEvery = 2 * time.Second
	jobsCountEvery      = 30 * time.Second
	jobsTranscriptLines = 18
)

type jobsPanel struct {
	rows       []JobRow
	cursor     int
	err        string
	viewing    string // job whose transcript is shown, if any
	transcript string
	gen        int // ignores ticks from an earlier panel
}

type (
	jobsListMsg struct {
		rows []JobRow
		err  error
	}
	jobsTranscriptMsg struct {
		id, text string
		err      error
	}
	jobsTickMsg      struct{ gen int }
	jobsCountTickMsg struct{}
)

// activeJobStatus reports whether a job counts as running in the status bar.
func activeJobStatus(s string) (running, waiting bool) {
	switch s {
	case "running", "created", "queued":
		return true, false
	case "waiting_for_input", "waiting_for_credentials":
		return true, true
	}
	return false, false
}

func (m *Model) jobsListCmd() tea.Cmd {
	bridge, parent := m.cfg.Jobs, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, 30*time.Second)
		defer cancel()
		rows, err := bridge.List(ctx)
		return jobsListMsg{rows: rows, err: err}
	}
}

func (m *Model) jobsTranscriptCmd(id string) tea.Cmd {
	bridge, parent := m.cfg.Jobs, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, 30*time.Second)
		defer cancel()
		text, err := bridge.Transcript(ctx, id)
		return jobsTranscriptMsg{id: id, text: text, err: err}
	}
}

// tickAfter is tea.Tick unless a test replaced it.
func (m *Model) tickAfter(d time.Duration, fn func(time.Time) tea.Msg) tea.Cmd {
	if m.tick != nil {
		return m.tick(d, fn)
	}
	return tea.Tick(d, fn)
}

// jobsCountTick schedules the status-bar job count refresh.
func (m *Model) jobsCountTick(after time.Duration) tea.Cmd {
	if m.cfg.Jobs == nil {
		return nil
	}
	return m.tickAfter(after, func(time.Time) tea.Msg { return jobsCountTickMsg{} })
}

func (m *Model) runSlashJobs() (tea.Model, tea.Cmd) {
	if m.cfg.Jobs == nil {
		m.appendSystem("/jobs unavailable: remote jobs are not wired into this TUI")
		m.rerender()
		return m, nil
	}
	m.jobsGen++
	m.jobs = &jobsPanel{gen: m.jobsGen}
	m.rerender()
	gen := m.jobsGen
	return m, tea.Batch(m.jobsListCmd(), m.tickAfter(jobsListEvery, func(time.Time) tea.Msg { return jobsTickMsg{gen: gen} }))
}

// updateJobs handles the /jobs panel's messages.
func (m *Model) updateJobs(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case jobsListMsg:
		if msg.err == nil {
			m.jobsRunning, m.jobsWaiting = 0, 0
			for _, r := range msg.rows {
				running, waiting := activeJobStatus(r.Status)
				if running {
					m.jobsRunning++
				}
				if waiting {
					m.jobsWaiting++
				}
			}
		}
		if p := m.jobs; p != nil {
			if msg.err != nil {
				p.err = msg.err.Error()
			} else {
				p.err, p.rows = "", msg.rows
				p.cursor = min(p.cursor, max(len(p.rows)-1, 0))
			}
		}
		m.rerender()
		return nil, true
	case jobsTranscriptMsg:
		if p := m.jobs; p != nil && p.viewing == msg.id {
			if msg.err != nil {
				p.transcript = "error: " + msg.err.Error()
			} else {
				p.transcript = msg.text
			}
			m.rerender()
		}
		return nil, true
	case jobsTickMsg:
		p := m.jobs
		if p == nil || msg.gen != p.gen {
			return nil, true
		}
		gen := msg.gen
		if p.viewing != "" {
			return tea.Batch(m.jobsTranscriptCmd(p.viewing), m.tickAfter(jobsTranscriptEvery, func(time.Time) tea.Msg { return jobsTickMsg{gen: gen} })), true
		}
		return tea.Batch(m.jobsListCmd(), m.tickAfter(jobsListEvery, func(time.Time) tea.Msg { return jobsTickMsg{gen: gen} })), true
	case jobsCountTickMsg:
		return tea.Batch(m.jobsListCmd(), m.jobsCountTick(jobsCountEvery)), true
	}
	return nil, false
}

func (m *Model) handleJobsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := m.jobs
	switch {
	case msg.Type == tea.KeyEsc || msg.Type == tea.KeyCtrlC:
		if p.viewing != "" {
			p.viewing, p.transcript = "", ""
			gen := p.gen
			m.rerender()
			return m, tea.Batch(m.jobsListCmd(), m.tickAfter(jobsListEvery, func(time.Time) tea.Msg { return jobsTickMsg{gen: gen} }))
		}
		m.jobs = nil
		m.rerender()
		return m, nil
	case msg.Type == tea.KeyUp && p.viewing == "":
		p.cursor = max(p.cursor-1, 0)
	case msg.Type == tea.KeyDown && p.viewing == "":
		p.cursor = min(p.cursor+1, max(len(p.rows)-1, 0))
	case msg.Type == tea.KeyEnter && p.viewing == "" && len(p.rows) > 0:
		p.gen++
		m.jobsGen = p.gen
		p.viewing, p.transcript = p.rows[p.cursor].ID, "loading…"
		gen := p.gen
		m.rerender()
		return m, tea.Batch(m.jobsTranscriptCmd(p.viewing), m.tickAfter(jobsTranscriptEvery, func(time.Time) tea.Msg { return jobsTickMsg{gen: gen} }))
	case msg.Type == tea.KeyRunes && string(msg.Runes) == "p" && len(p.rows) > 0:
		id := p.rows[p.cursor].ID
		if p.viewing != "" {
			id = p.viewing
		}
		bridge, parent := m.cfg.Jobs, m.ctx
		m.appendSystem("pulling " + id + "…")
		m.rerender()
		return m, func() tea.Msg {
			ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
			defer cancel()
			out, err := bridge.Pull(ctx, id)
			if err != nil {
				return systemMsg("pull " + id + " failed: " + err.Error())
			}
			return systemMsg("pull " + id + ": " + strings.TrimSpace(out))
		}
	}
	m.rerender()
	return m, nil
}

// jobsStatusSegment is the status-bar chip, empty when nothing is running.
func (m *Model) jobsStatusSegment() string {
	if m.jobsRunning == 0 {
		return ""
	}
	s := fmt.Sprintf("⧗ %d remote job", m.jobsRunning)
	if m.jobsRunning != 1 {
		s += "s"
	}
	if m.jobsWaiting > 0 {
		s += fmt.Sprintf(" · %d waiting", m.jobsWaiting)
	}
	return toolWarning.Render(s)
}

func renderJobsPanel(p *jobsPanel, width int, now time.Time) string {
	w := min(max(width-4, 30), 120)
	var b strings.Builder
	if p.viewing != "" {
		b.WriteString(pickerTitle.Render(" Job " + p.viewing + " "))
		lines := strings.Split(strings.TrimRight(p.transcript, "\n"), "\n")
		if len(lines) > jobsTranscriptLines {
			lines = lines[len(lines)-jobsTranscriptLines:]
		}
		for _, l := range lines {
			b.WriteString("\n" + pickerRow.Render(truncateRunes(l, w-4)))
		}
		b.WriteString("\n" + pickerHint.Render("  live · p pull · Esc back"))
	} else {
		b.WriteString(pickerTitle.Render(" Remote jobs "))
		switch {
		case p.err != "":
			b.WriteString("\n" + pickerRow.Render("  "+truncateRunes(p.err, w-6)))
		case p.rows == nil:
			b.WriteString("\n" + pickerRow.Render("  loading…"))
		case len(p.rows) == 0:
			b.WriteString("\n" + pickerRow.Render("  no jobs"))
		}
		for i, r := range p.rows {
			age := "-"
			if !r.Updated.IsZero() {
				age = humanAge(now.Sub(r.Updated))
			}
			line := truncateRunes(fmt.Sprintf("%-17s %-18s %-8s %s", r.ID, r.Status, age, r.Objective), w-6)
			if i == p.cursor {
				b.WriteString("\n" + pickerSel.Render("› "+line))
			} else {
				b.WriteString("\n" + pickerRow.Render("  "+line))
			}
		}
		b.WriteString("\n" + pickerHint.Render("  ↑/↓ navigate · Enter transcript · p pull · Esc close"))
	}
	rendered := pickerBox.Width(w).Render(b.String())
	if w < width {
		return lipgloss.PlaceHorizontal(width, lipgloss.Center, rendered)
	}
	return rendered
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if n <= 1 || len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
