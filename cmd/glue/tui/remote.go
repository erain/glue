// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package tui

import (
	"context"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/erain/glue"
)

// RemoteRequest is what /remote hands to Config.SubmitRemote.
type RemoteRequest struct {
	Objective string
	// Context is background from this session: the latest compaction
	// summary, or the tail of the conversation.
	Context string
	// Snapshot sends the working tree as it is (glue --remote --snapshot).
	Snapshot bool
}

// remoteContextLimit caps the session context /remote sends, in characters.
const remoteContextLimit = 4000

// remoteContext builds the background for a remote job from a session: the
// latest compaction summary with what followed it, or else the last messages,
// keeping the most recent remoteContextLimit characters.
func remoteContext(msgs []glue.Message) string {
	start := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if _, ok := msgs[i].Metadata["compaction"]; ok {
			start = i
			break
		}
	}
	var b strings.Builder
	for _, msg := range msgs[start:] {
		// Environment context describes this machine, not the task.
		if msg.Role != glue.MessageRoleUser && msg.Role != glue.MessageRoleAssistant || glue.IsEnvContextMessage(msg) {
			continue
		}
		var text []string
		for _, p := range msg.Content {
			if p.Type == glue.ContentTypeText && strings.TrimSpace(p.Text) != "" {
				text = append(text, strings.TrimSpace(p.Text))
			}
		}
		if len(text) == 0 {
			continue
		}
		role := "developer"
		if msg.Role == glue.MessageRoleAssistant {
			role = "assistant"
		}
		b.WriteString(role + ": " + strings.Join(text, "\n") + "\n\n")
	}
	out := []rune(strings.TrimSpace(b.String()))
	if len(out) > remoteContextLimit {
		out = append([]rune("…"), out[len(out)-remoteContextLimit+1:]...)
	}
	return string(out)
}

// runSlashRemote hands an objective to a remote job with this session's context.
func (m *Model) runSlashRemote(arg string) (tea.Model, tea.Cmd) {
	if m.cfg.SubmitRemote == nil {
		m.appendSystem("/remote unavailable: remote jobs are not wired into this TUI")
		m.rerender()
		return m, nil
	}
	if m.turn != nil {
		m.appendSystem("/remote: a turn is running; Esc to cancel first.")
		m.rerender()
		return m, nil
	}
	req := RemoteRequest{}
	fields := strings.Fields(arg)
	if len(fields) > 0 && fields[0] == "--snapshot" {
		req.Snapshot = true
		arg = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(arg), "--snapshot"))
	}
	if req.Objective = strings.TrimSpace(arg); req.Objective == "" {
		m.appendSystem("usage: /remote [--snapshot] <objective>")
		m.rerender()
		return m, nil
	}
	if m.cfg.Agent != nil {
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
		if s, err := m.cfg.Agent.Session(ctx, m.cfg.SessionID); err == nil {
			req.Context = remoteContext(s.Messages())
		}
		cancel()
	}
	m.appendSystem("/remote: submitting to a remote sandbox…")
	m.rerender()
	submit, parent, send := m.cfg.SubmitRemote, m.ctx, m.send
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, 3*time.Minute)
		defer cancel()
		progress := func(line string) {
			if send != nil && strings.TrimSpace(line) != "" {
				send(systemMsg("  " + line))
			}
		}
		id, err := submit(ctx, req, progress)
		if err != nil {
			text := "/remote failed: " + err.Error()
			if strings.Contains(text, "--ignore-local") {
				text += "\nIn the TUI, /remote --snapshot <objective> sends your working tree as it is."
			}
			return systemMsg(text)
		}
		return systemMsg("remote job " + id + " is running · watch: glue job attach " + id + " · result: glue job result " + id)
	}
}
