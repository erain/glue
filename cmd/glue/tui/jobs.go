// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package tui

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/erain/glue"
)

// JobRow is one remote job in the /jobs panel.
type JobRow struct {
	ID, Status, Objective string
	Updated               time.Time
}

type JobQuestion struct {
	ID, Question string
	Options      []string
}
type JobPermission struct{ ID, Tool, Action, Target string }
type JobDetail struct {
	ID, Status, Objective, Summary, PRURL string
	Question                              *JobQuestion
	Permissions                           []JobPermission
}

type TeleportResult struct{ SessionID, Notice string }

// JobsBridge gives the TUI access to remote jobs. cmd/glue implements it
// with the glue job commands.
type JobsBridge interface {
	// List returns recent jobs. It must not wake suspended tasks: the panel
	// and the status bar call it on a timer.
	List(ctx context.Context) ([]JobRow, error)
	Detail(ctx context.Context, id string) (JobDetail, error)
	// Transcript renders a job's history so far, as glue job logs does.
	Transcript(ctx context.Context, id string) (string, error)
	Diff(ctx context.Context, id string) (string, error)
	Answer(ctx context.Context, id, questionID, text string) error
	DecidePermission(ctx context.Context, id, permID string, allow bool, rememberFor string) error
	Say(ctx context.Context, id, text string, interrupt bool) error
	OpenPR(ctx context.Context, id string) (string, error)
	Cancel(ctx context.Context, id string) error
	// Pull brings the job's result into the workspace (glue job pull) and
	// describes what it did.
	Pull(ctx context.Context, id string, apply bool) (string, error)
	Teleport(ctx context.Context, id string) (TeleportResult, error)
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

	detail                                   JobDetail
	diffMode, follow                         bool
	scroll                                   int
	diff, prompt, target, qid, input, notice string
}

type (
	jobsListMsg struct {
		rows []JobRow
		err  error
	}
	jobsTranscriptMsg struct {
		id, text            string
		notice, sys         string
		diff, body, refresh bool
		detail              *JobDetail
	}
	jobsTickMsg      struct{ gen int }
	jobsCountTickMsg struct{}
)

func pick[T any](ok bool, a, b T) T {
	if ok {
		return a
	}
	return b
}

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

func (p *jobsPanel) selected() (id, st string, term, hasPerm bool) {
	id, st = p.viewing, p.detail.Status
	if id == "" && p.cursor >= 0 && p.cursor < len(p.rows) {
		id, st = p.rows[p.cursor].ID, p.rows[p.cursor].Status
	}
	return id, st, !slices.Contains([]string{"", "created", "queued", "submit_unknown", "running", "waiting_for_input", "waiting_for_credentials", "suspended"}, st), p.viewing != "" && len(p.detail.Permissions) > 0
}

func (p *jobsPanel) viewWindow() (lines []string, off, maxOff int) {
	if raw := strings.TrimRight(pick(p.diffMode, p.diff, p.transcript), "\n"); raw != "" {
		lines = strings.Split(raw, "\n")
		maxOff = max(len(lines)-jobsTranscriptLines, 0)
	}
	return lines, pick(p.follow, maxOff, min(max(p.scroll, 0), maxOff)), maxOff
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

func (m *Model) jobsTranscriptCmd(id string, diff, bodyOnly bool) tea.Cmd {
	bridge, parent := m.cfg.Jobs, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, 30*time.Second)
		defer cancel()
		var det *JobDetail
		if !bodyOnly {
			if d, err := bridge.Detail(ctx, id); err == nil {
				det = &d
			}
		}
		text, err := pick(diff, bridge.Diff, bridge.Transcript)(ctx, id)
		if err != nil {
			text = "error: " + err.Error()
		}
		return jobsTranscriptMsg{id: id, text: cmp.Or(strings.TrimRight(text, "\n"), pick(diff, "(no changes)", "")), diff: diff, body: true, detail: det}
	}
}

func (m *Model) jobsAct(id, sys, notice string, refresh bool, fn func(context.Context, JobsBridge) (string, error)) (tea.Model, tea.Cmd) {
	bridge, parent := m.cfg.Jobs, m.ctx
	m.rerender()
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
		defer cancel()
		out, err := fn(ctx, bridge)
		if err != nil {
			return jobsTranscriptMsg{id: id, notice: "error: " + err.Error(), sys: pick(sys != "", sys+" failed: "+err.Error(), "")}
		}
		return jobsTranscriptMsg{id: id, notice: notice + strings.TrimSpace(out), sys: pick(sys != "", notice+strings.TrimSpace(out), ""), refresh: refresh}
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
		if msg.sys != "" {
			m.appendSystem(msg.sys)
		}
		var next tea.Cmd
		if p := m.jobs; p != nil {
			if msg.notice != "" {
				p.notice = msg.notice
				if u, ok := strings.CutPrefix(msg.notice, "draft PR: "); ok && p.viewing == msg.id {
					p.detail.PRURL = u
				}
			}
			if msg.body && p.viewing == msg.id {
				if d := msg.detail; d != nil {
					p.detail = JobDetail{ID: cmp.Or(d.ID, msg.id), Status: cmp.Or(d.Status, p.detail.Status), Objective: cmp.Or(d.Objective, p.detail.Objective), Summary: d.Summary, PRURL: cmp.Or(d.PRURL, p.detail.PRURL), Question: d.Question, Permissions: d.Permissions}
					if i := slices.IndexFunc(p.rows, func(r JobRow) bool { return r.ID == msg.id }); i >= 0 && p.detail.Status != "" {
						p.rows[i].Status = p.detail.Status
					}
				}
				*pick(msg.diff, &p.diff, &p.transcript) = msg.text
			}
			if msg.refresh {
				next = tea.Batch(m.jobsListCmd(), pick(p.viewing != "", m.jobsTranscriptCmd(p.viewing, p.diffMode, false), nil))
			}
		}
		m.rerender()
		return next, true
	case jobsTickMsg:
		p := m.jobs
		if p == nil || msg.gen != p.gen {
			return nil, true
		}
		gen := msg.gen
		if p.viewing != "" {
			return tea.Batch(m.jobsTranscriptCmd(p.viewing, p.diffMode, false), m.tickAfter(jobsTranscriptEvery, func(time.Time) tea.Msg { return jobsTickMsg{gen: gen} })), true
		}
		return tea.Batch(m.jobsListCmd(), m.tickAfter(jobsListEvery, func(time.Time) tea.Msg { return jobsTickMsg{gen: gen} })), true
	case jobsCountTickMsg:
		return tea.Batch(m.jobsListCmd(), m.jobsCountTick(jobsCountEvery)), true
	}
	return nil, false
}

func (m *Model) handleJobsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := m.jobs
	key := pick(msg.Type == tea.KeyRunes, string(msg.Runes), "")
	if p.prompt != "" {
		switch {
		case p.prompt == "cancel":
			id := p.target
			p.prompt, p.target = "", ""
			if key == "y" || key == "Y" {
				p.notice = "canceling " + id + "…"
				return m.jobsAct(id, "cancel "+id, "canceled "+id, true, func(c context.Context, b JobsBridge) (string, error) { return "", b.Cancel(c, id) })
			}
		case msg.Type == tea.KeyEsc || msg.Type == tea.KeyCtrlC:
			p.prompt, p.input = "", ""
		case msg.Type == tea.KeyBackspace:
			r := []rune(p.input)
			p.input = string(r[:max(len(r)-1, 0)])
		case msg.Type == tea.KeySpace || msg.Type == tea.KeyRunes:
			p.input += pick(msg.Type == tea.KeySpace, " ", key)
		case msg.Type == tea.KeyEnter:
			kind, id, qid, text := p.prompt, p.target, p.qid, strings.TrimSpace(p.input)
			p.prompt, p.target, p.qid, p.input = "", "", "", ""
			if kind == "answer" && text != "" {
				return m.jobsAct(id, "", "answered: "+text, true, func(c context.Context, b JobsBridge) (string, error) { return "", b.Answer(c, id, qid, text) })
			} else if intr := kind == "interrupt"; intr || text != "" {
				return m.jobsAct(id, "", pick(intr, "interrupted "+id, "sent message to "+id), true, func(c context.Context, b JobsBridge) (string, error) { return "", b.Say(c, id, text, intr) })
			}
		}
		m.rerender()
		return m, nil
	}
	id, _, term, hasPerm := p.selected()
	_, cur, maxOff := p.viewWindow()
	switch {
	case msg.Type == tea.KeyEsc || msg.Type == tea.KeyCtrlC:
		if p.viewing != "" {
			*p = jobsPanel{rows: p.rows, cursor: p.cursor, gen: p.gen + 1}
			m.jobsGen = p.gen
			gen := p.gen
			m.rerender()
			return m, tea.Batch(m.jobsListCmd(), m.tickAfter(jobsListEvery, func(time.Time) tea.Msg { return jobsTickMsg{gen: gen} }))
		}
		m.jobs = nil
		m.rerender()
		return m, nil
	case (msg.Type == tea.KeyUp || key == "k") && p.viewing == "":
		p.cursor = max(p.cursor-1, 0)
	case (msg.Type == tea.KeyDown || key == "j") && p.viewing == "":
		p.cursor = min(p.cursor+1, max(len(p.rows)-1, 0))
	case msg.Type == tea.KeyEnter && p.viewing == "" && len(p.rows) > 0:
		r := p.rows[p.cursor]
		p.gen++
		m.jobsGen = p.gen
		p.viewing, p.transcript, p.diff, p.notice, p.diffMode, p.scroll, p.follow = r.ID, "loading…", "", "", false, 0, true
		p.detail = JobDetail{ID: r.ID, Status: r.Status, Objective: r.Objective}
		gen := p.gen
		m.rerender()
		return m, tea.Batch(m.jobsTranscriptCmd(p.viewing, p.diffMode, false), m.tickAfter(jobsTranscriptEvery, func(time.Time) tea.Msg { return jobsTickMsg{gen: gen} }))
	case p.viewing != "" && (msg.Type == tea.KeyUp || key == "k" || msg.Type == tea.KeyDown || key == "j" || msg.Type == tea.KeyPgUp || msg.Type == tea.KeyPgDown):
		delta := cmp.Or(map[tea.KeyType]int{tea.KeyUp: -1, tea.KeyDown: 1, tea.KeyPgUp: -jobsTranscriptLines / 2, tea.KeyPgDown: jobsTranscriptLines / 2}[msg.Type], map[string]int{"k": -1, "j": 1}[key])
		p.scroll = min(max(cur+delta, 0), maxOff)
		p.follow = !p.diffMode && p.scroll >= maxOff
	case p.viewing != "" && (msg.Type == tea.KeyHome || key == "g" || msg.Type == tea.KeyEnd || key == "G"):
		top := msg.Type == tea.KeyHome || key == "g"
		p.scroll, p.follow = pick(top, 0, maxOff), !top && !p.diffMode
	case p.viewing != "" && key == "d":
		p.diffMode = !p.diffMode
		p.scroll, p.follow, p.diff = 0, !p.diffMode, pick(p.diffMode, cmp.Or(p.diff, "loading…"), p.diff)
		m.rerender()
		return m, m.jobsTranscriptCmd(p.viewing, p.diffMode, true)
	case hasPerm && (key == "y" || key == "s" || key == "S" || key == "n"):
		p0, ok, rem := p.detail.Permissions[0].ID, key != "n", pick(key == "s" || key == "S", "session", "")
		lbl := cmp.Or(map[string]string{"y": "allowed once", "n": "denied permission"}[key], "allowed for session")
		return m.jobsAct(id, "", lbl, true, func(c context.Context, b JobsBridge) (string, error) {
			return "", b.DecidePermission(c, id, p0, ok, rem)
		})
	case p.viewing != "" && p.detail.Question != nil && len(key) == 1 && key[0] >= '1' && int(key[0]-'1') < min(len(p.detail.Question.Options), 9):
		qid, choice := p.detail.Question.ID, p.detail.Question.Options[key[0]-'1']
		return m.jobsAct(id, "", "answered: "+choice, true, func(c context.Context, b JobsBridge) (string, error) { return "", b.Answer(c, id, qid, choice) })
	case p.viewing != "" && p.detail.Question != nil && key == "a":
		p.prompt, p.target, p.qid, p.input = "answer", id, p.detail.Question.ID, ""
	case id != "" && !term && ((key == "s" && !hasPerm) || key == "i"):
		p.prompt, p.target, p.input = pick(key == "i", "interrupt", "say"), id, ""
	case key == "t" && id != "":
		return m.runSlashTeleport(id)
	case (key == "p" || key == "A") && id != "":
		apply, lbl := key == "A", pick(key == "A", "apply ", "pull ")+id
		m.appendSystem(pick(apply, "applying ", "pulling ") + id + "…")
		return m.jobsAct(id, lbl, lbl+": ", false, func(c context.Context, b JobsBridge) (string, error) { return b.Pull(c, id, apply) })
	case key == "o" && id != "" && term:
		p.notice = "opening draft PR…"
		return m.jobsAct(id, "pr "+id, "draft PR: ", true, func(c context.Context, b JobsBridge) (string, error) { return b.OpenPR(c, id) })
	case key == "c" && id != "" && !term:
		p.prompt, p.target = "cancel", id
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
	_, st, term, hasPerm := p.selected()
	if p.viewing != "" {
		title := " Job " + p.viewing + pick(p.diffMode, " [diff]", "") + pick(st != "", " · "+st, "")
		lines, off, _ := p.viewWindow()
		if len(lines) > 0 {
			title += fmt.Sprintf(" (%d-%d/%d)", off+1, min(off+jobsTranscriptLines, len(lines)), len(lines))
		}
		b.WriteString(pickerTitle.Render(title + " "))
		if p.detail.Summary != "" {
			b.WriteString("\n" + pickerRow.Render("  summary: "+truncateRunes(p.detail.Summary, w-14)))
		}
		if p.detail.PRURL != "" {
			b.WriteString("\n" + toolOk.Render("  draft PR: "+truncateRunes(p.detail.PRURL, w-14)))
		}
		for _, l := range lines[off:min(off+jobsTranscriptLines, len(lines))] {
			sty := pickerRow
			if p.diffMode && (strings.HasPrefix(l, "diff --git ") || strings.HasPrefix(l, "+++ ") || strings.HasPrefix(l, "--- ")) {
				sty = blockTitle
			} else if p.diffMode && strings.HasPrefix(l, "@@") {
				sty = welcomeAccent
			} else if p.diffMode && strings.HasPrefix(l, "+") {
				sty = diffAddSty
			} else if p.diffMode && strings.HasPrefix(l, "-") {
				sty = diffDelSty
			}
			b.WriteString("\n" + sty.Render(truncateRunes(l, w-4)))
		}
		if hasPerm {
			b.WriteString("\n" + toolWarning.Render("  ⚠ permission: "+truncateRunes(strings.TrimSpace(p.detail.Permissions[0].Tool+" "+p.detail.Permissions[0].Action+" "+p.detail.Permissions[0].Target), w-20)))
		}
		if q := p.detail.Question; q != nil {
			b.WriteString("\n" + toolWarning.Render("  ? "+truncateRunes(q.Question, w-8)))
			var opts string
			for i, o := range q.Options[:min(len(q.Options), 9)] {
				opts += fmt.Sprintf("  [%d] %s", i+1, o)
			}
			if opts != "" {
				b.WriteString("\n" + pickerRow.Render("  "+truncateRunes(opts, w-6)))
			}
		}
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
	}
	if p.notice != "" {
		b.WriteString("\n" + pickerRow.Render("  "+truncateRunes(p.notice, w-6)))
	}
	act := pick(term, "t teleport · p pull · A apply · o open PR", pick(hasPerm, "", "s say · ")+"t teleport · i interrupt · c cancel")
	hint := pick(len(p.rows) == 0, "  Esc close", "  ↑/↓ navigate · Enter open · "+act+" · Esc close")
	if p.viewing != "" {
		h := []string{"↑/↓/g/G scroll", pick(p.diffMode, "d transcript", "d diff")}
		if hasPerm {
			h = append(h, "y allow · s/S session · n deny")
		}
		if q := p.detail.Question; q != nil {
			h = append(h, pick(len(q.Options) > 0, fmt.Sprintf("1-%d/a answer", min(len(q.Options), 9)), "a answer"))
		}
		hint = "  " + strings.Join(append(h, act, "Esc back"), " · ")
	}
	if p.prompt == "cancel" {
		b.WriteString("\n" + toolWarning.Render("  Cancel "+p.target+"? (y/N)"))
		hint = "  y confirm cancel · n/Esc keep running"
	} else if p.prompt != "" {
		b.WriteString("\n" + userPrefix.Render("  "+p.prompt+" › ") + p.input + "█")
		hint = "  Enter send · Esc cancel"
	}
	b.WriteString("\n" + pickerHint.Render(hint))
	rendered := pickerBox.Width(w).Render(b.String())
	if w < width {
		return lipgloss.PlaceHorizontal(width, lipgloss.Center, rendered)
	}
	return rendered
}

func (m *Model) runSlashTeleport(arg string) (tea.Model, tea.Cmd) {
	if m.cfg.Jobs == nil || m.turn != nil {
		m.appendSystem(pick(m.cfg.Jobs == nil, "/teleport unavailable: remote jobs are not wired into this TUI", "/teleport: a turn is running; Esc to cancel first."))
		m.rerender()
		return m, nil
	}
	id := cmp.Or(strings.TrimSpace(arg), "latest")
	if m.jobs != nil {
		m.jobs.notice = "teleporting " + id + "…"
	}
	m.appendSystem("teleporting " + id + "…")
	m.rerender()
	cfg, parent := m.cfg, m.ctx
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
		defer cancel()
		res, err := cfg.Jobs.Teleport(ctx, id)
		if err != nil {
			return jobsTranscriptMsg{id: id, notice: "error: " + err.Error(), sys: "/teleport failed: " + err.Error()}
		}
		var msgs []glue.Message
		if cfg.Agent != nil {
			if s, err := cfg.Agent.Session(ctx, res.SessionID); err == nil {
				msgs = s.Messages()
			}
		} else if cfg.Store != nil {
			if st, ok, _ := cfg.Store.Load(ctx, res.SessionID); ok {
				msgs = st.Messages
			}
		}
		return sessionSwitchedMsg{ID: res.SessionID, Note: "teleported " + id, Messages: msgs}
	}
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if n <= 1 || len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
