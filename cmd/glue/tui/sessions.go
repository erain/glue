package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/erain/glue"
)

// sessionPicker is the modal state used by /resume: a small list view
// pinned where the input box normally lives, navigated with ↑/↓ and
// committed with Enter (or canceled with Esc).
type sessionPicker struct {
	items  []glue.SessionSummary
	cursor int
}

func (p *sessionPicker) up() {
	if p == nil || len(p.items) == 0 {
		return
	}
	if p.cursor > 0 {
		p.cursor--
	}
}

func (p *sessionPicker) down() {
	if p == nil || len(p.items) == 0 {
		return
	}
	if p.cursor < len(p.items)-1 {
		p.cursor++
	}
}

func (p *sessionPicker) selected() (glue.SessionSummary, bool) {
	if p == nil || p.cursor < 0 || p.cursor >= len(p.items) {
		return glue.SessionSummary{}, false
	}
	return p.items[p.cursor], true
}

// pickerStyles & rendering --------------------------------------------------

var (
	pickerBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(accent).
			Padding(0, 1)
	pickerTitle = lipgloss.NewStyle().Foreground(accent).Bold(true)
	pickerRow   = lipgloss.NewStyle().Foreground(inkSoft)
	pickerSel   = lipgloss.NewStyle().Foreground(accent).Bold(true)
	pickerHint  = lipgloss.NewStyle().Foreground(inkMuted)
)

// renderPicker returns the picker overlay. width is the full TUI width.
func renderPicker(p *sessionPicker, width int) string {
	w := width - 4
	if w > 100 {
		w = 100
	}
	if w < 30 {
		w = 30
	}
	var b strings.Builder
	b.WriteString(pickerTitle.Render(" Resume session "))
	b.WriteByte('\n')
	if len(p.items) == 0 {
		b.WriteString(pickerRow.Render("  (no past sessions found)"))
	} else {
		for i, s := range p.items {
			line := formatPickerRow(s, w-4)
			if i == p.cursor {
				b.WriteString(pickerSel.Render("› " + line))
			} else {
				b.WriteString(pickerRow.Render("  " + line))
			}
			if i < len(p.items)-1 {
				b.WriteByte('\n')
			}
		}
	}
	b.WriteByte('\n')
	b.WriteString(pickerHint.Render("  ↑/↓ navigate · Enter select · Esc cancel"))

	rendered := pickerBox.Width(w).Render(b.String())
	if w < width {
		return lipgloss.PlaceHorizontal(width, lipgloss.Center, rendered)
	}
	return rendered
}

func formatPickerRow(s glue.SessionSummary, max int) string {
	updated := "unknown"
	if !s.UpdatedAt.IsZero() {
		updated = humanAge(time.Since(s.UpdatedAt))
	}
	stats := fmt.Sprintf("%d msg", s.Messages)
	row := fmt.Sprintf("%-32s  %12s  %s", truncate(s.ID, 32), updated, stats)
	return truncate(row, max)
}

func humanAge(d time.Duration) string {
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	days := int(d.Hours() / 24)
	if days < 30 {
		return fmt.Sprintf("%dd ago", days)
	}
	return fmt.Sprintf("%dmo ago", days/30)
}

// transcriptFromMessages converts a loaded session's messages into the
// TUI's display transcript so /resume's selected session reads
// naturally. Tool messages are collapsed into the preceding assistant
// message's tool card (a single round of tool calls per assistant turn).
func transcriptFromMessages(msgs []glue.Message) []transcriptItem {
	out := make([]transcriptItem, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case glue.MessageRoleUser:
			out = append(out, transcriptItem{Kind: itemUser, Text: collectText(m.Content)})
		case glue.MessageRoleAssistant:
			text := collectText(m.Content)
			if strings.TrimSpace(text) != "" {
				out = append(out, transcriptItem{Kind: itemAssistant, Text: text})
			}
			// Tool calls emitted in the assistant message arrive as
			// content parts. Render each as a "done" card; the next tool
			// message will fill the result.
			for _, p := range m.Content {
				if p.Type == glue.ContentTypeToolCall && p.ToolCall != nil {
					out = append(out, transcriptItem{
						Kind:       itemTool,
						ToolCallID: p.ToolCall.ID,
						ToolName:   p.ToolCall.Name,
						ToolArgs:   string(p.ToolCall.Arguments),
						ToolPhase:  tsDone,
					})
				}
			}
		case glue.MessageRoleTool:
			// Match this tool result back to the pending tool card.
			text := collectText(m.Content)
			for i := len(out) - 1; i >= 0; i-- {
				if out[i].Kind == itemTool && out[i].ToolCallID == m.ToolCallID && out[i].ToolResult == "" {
					out[i].ToolResult = text
					out[i].ToolErr = m.IsError
					break
				}
			}
		}
	}
	return out
}

func collectText(parts []glue.ContentPart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Type == glue.ContentTypeText && p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}
