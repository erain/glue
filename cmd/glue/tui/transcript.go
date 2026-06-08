package tui

import (
	"encoding/json"
	"fmt"
	"strings"
)

// itemKind discriminates transcript entries.
type itemKind int

const (
	itemUser itemKind = iota
	itemAssistant
	itemTool
	itemSystem
	itemBlock // titled multi-line block (e.g. /help, /tools)
)

// toolPhase tracks where a tool call is in its lifecycle.
type toolPhase int

const (
	tsPending toolPhase = iota // permission requested, awaiting answer
	tsRunning                  // executing
	tsDone                     // completed; ToolErr distinguishes ok / failed
	tsDenied                   // permission denied
)

// transcriptItem is one entry in the rendered conversation. Tool items
// are mutable (state transitions in place) — every other kind is
// append-only.
type transcriptItem struct {
	Kind itemKind

	// User / Assistant / System text.
	Text string
	// Rendered, if non-empty, replaces Text in the View pass. Used so
	// the assistant item can show plain text while streaming and switch
	// to glamour-rendered markdown once the turn completes.
	Rendered string

	// itemBlock fields.
	BlockTitle string
	BlockBody  string // already-formatted multi-line content (rendered via lipgloss before storing)

	// Tool fields.
	ToolCallID string
	ToolName   string
	ToolArgs   string // pretty-printed JSON
	ToolResult string
	ToolErr    bool
	ToolPhase  toolPhase
}

// renderCtx carries dynamic state into a per-item render so the
// transcript stays a pure projection of model state.
type renderCtx struct {
	width   int
	spinner string // current spinner frame, used for in-flight tools
}

// render returns the wrapped, styled string for the item.
func (it *transcriptItem) render(ctx renderCtx) string {
	switch it.Kind {
	case itemUser:
		return userPrefix.Render("user > ") + it.Text
	case itemAssistant:
		head := asstPrefix.Render("assistant")
		body := it.Rendered
		if body == "" {
			body = strings.TrimRight(it.Text, "\n")
		}
		if body == "" {
			return head
		}
		return head + "\n" + body
	case itemTool:
		return renderTool(it, ctx)
	case itemSystem:
		return sysLine.Render("· " + it.Text)
	case itemBlock:
		return renderBlock(it, ctx.width)
	}
	return ""
}

func renderTool(it *transcriptItem, ctx renderCtx) string {
	icon, suffix := "▸", ""
	switch it.ToolPhase {
	case tsPending:
		icon = "▸"
		suffix = " " + toolWarning.Render("awaiting permission")
	case tsRunning:
		// Spinner replaces the static dot so the user can see the tool is alive.
		if ctx.spinner != "" {
			icon = ctx.spinner
		}
		suffix = " " + toolWarning.Render("running")
	case tsDone:
		if it.ToolErr {
			icon = "✗"
			suffix = " " + toolErrSty.Render("failed")
		} else {
			icon = "✓"
			suffix = " " + toolOk.Render("done")
		}
	case tsDenied:
		icon = "⊘"
		suffix = " " + toolErrSty.Render("denied")
	}

	argLine := truncate(flattenArgs(it.ToolArgs), maxArgLen(ctx.width))
	header := toolHeader.Render(fmt.Sprintf("%s %s  %s", icon, it.ToolName, argLine)) + suffix

	var parts []string
	parts = append(parts, header)

	// Pre-execution preview for edit_file: show the proposed change.
	if (it.ToolPhase == tsPending || it.ToolPhase == tsRunning) && it.ToolName == "edit_file" {
		if preview := editDiffPreview(it.ToolArgs); preview != "" {
			parts = append(parts, preview)
		}
	}

	// Inline permission prompt: nudged INTO the tool card so the user's
	// eye doesn't have to ping-pong between a floating box and the
	// transcript to know what they're approving.
	if it.ToolPhase == tsPending {
		parts = append(parts, renderInlinePermPrompt())
	}

	// Post-execution body.
	if (it.ToolPhase == tsDone || it.ToolPhase == tsDenied) && it.ToolResult != "" {
		parts = append(parts, toolBody.Render(indentResult(it.ToolResult, 8)))
	}

	return strings.Join(parts, "\n")
}

func renderInlinePermPrompt() string {
	const indent = "        "
	choices := []string{
		permKey.Render("[a]") + " allow once",
		permKey.Render("[s]") + " session",
		permKey.Render("[t]") + " target",
		permKey.Render("[n]") + " no",
		keyHint.Render("(Esc denies)"),
	}
	return indent + strings.Join(choices, "    ")
}

// renderBlock formats a titled, multi-line block (used for /help, /tools,
// the welcome card). The body is rendered verbatim with a 2-space indent.
func renderBlock(it *transcriptItem, width int) string {
	w := width - 4
	if w < 30 {
		w = 30
	}
	if w > 100 {
		w = 100
	}
	head := blockTitle.Render(" " + it.BlockTitle + " ")
	body := strings.TrimRight(it.BlockBody, "\n")
	body = indentLines(body, "  ")
	return blockBox.Width(w).Render(head + "\n" + body)
}

func indentLines(s, prefix string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = prefix + ln
	}
	return strings.Join(lines, "\n")
}

func maxArgLen(width int) int {
	if width <= 0 {
		return 60
	}
	n := width - 20
	if n < 30 {
		n = 30
	}
	if n > 120 {
		n = 120
	}
	return n
}

// flattenArgs collapses JSON args to a single human-readable line.
func flattenArgs(args string) string {
	if args == "" {
		return ""
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		return strings.ReplaceAll(args, "\n", " ")
	}
	// Common keys we lift to the top of the line; everything else follows.
	priority := []string{"path", "pattern", "argv", "url", "name", "old_string", "query"}
	var parts []string
	used := map[string]bool{}
	for _, k := range priority {
		if v, ok := parsed[k]; ok {
			parts = append(parts, formatArgValue(k, v))
			used[k] = true
		}
	}
	// Append remaining keys in stable order.
	rest := make([]string, 0, len(parsed))
	for k := range parsed {
		if !used[k] {
			rest = append(rest, k)
		}
	}
	// no sort import needed; small N — bubble sort by string
	for i := 0; i < len(rest); i++ {
		for j := i + 1; j < len(rest); j++ {
			if rest[j] < rest[i] {
				rest[i], rest[j] = rest[j], rest[i]
			}
		}
	}
	for _, k := range rest {
		parts = append(parts, formatArgValue(k, parsed[k]))
	}
	return strings.Join(parts, " ")
}

func formatArgValue(k string, v any) string {
	switch x := v.(type) {
	case string:
		// Quote multi-word / contains-space strings, otherwise bare.
		return k + "=" + shortQuote(x)
	case bool:
		if x {
			return k
		}
		return k + "=false"
	case float64:
		return fmt.Sprintf("%s=%g", k, x)
	case []any:
		// arrays (e.g. argv): space-joined.
		var s []string
		for _, e := range x {
			if str, ok := e.(string); ok {
				s = append(s, str)
			} else {
				s = append(s, fmt.Sprint(e))
			}
		}
		return k + "=" + shortQuote(strings.Join(s, " "))
	default:
		b, _ := json.Marshal(v)
		return k + "=" + string(b)
	}
}

func shortQuote(s string) string {
	s = strings.ReplaceAll(s, "\n", "⏎")
	if strings.ContainsAny(s, " \t") {
		return strconvQuote(s)
	}
	return s
}

// strconvQuote is a stdlib-free quote for terminal display only.
func strconvQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func truncate(s string, n int) string {
	if n <= 1 {
		return s
	}
	// rune-count truncate.
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
}

// indentResult prefixes every line with `n` spaces; truncates the output
// to a reasonable size so a single huge tool result doesn't fill the
// screen.
func indentResult(s string, indent int) string {
	const maxLines = 12
	pad := strings.Repeat(" ", indent)
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	more := 0
	if len(lines) > maxLines {
		more = len(lines) - maxLines
		lines = lines[:maxLines]
	}
	var b strings.Builder
	for i, ln := range lines {
		b.WriteString(pad)
		b.WriteString(ln)
		if i < len(lines)-1 || more > 0 {
			b.WriteByte('\n')
		}
	}
	if more > 0 {
		b.WriteString(pad)
		b.WriteString(keyHint.Render(fmt.Sprintf("[%d more line(s)]", more)))
	}
	return b.String()
}

// editDiffPreview renders a small unified-style preview of an edit_file
// pending call: the old_string in red, the new_string in green, each
// truncated to ~6 lines.
func editDiffPreview(args string) string {
	var p struct {
		Path      string `json:"path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return ""
	}
	if p.OldString == "" && p.NewString == "" {
		return ""
	}
	old := splitPreview(p.OldString, 6)
	new_ := splitPreview(p.NewString, 6)
	var b strings.Builder
	b.WriteString("        ")
	b.WriteString(sysLine.Render("─── " + p.Path + " ───"))
	b.WriteByte('\n')
	for _, ln := range old {
		b.WriteString("        ")
		b.WriteString(diffDelSty.Render("- " + ln))
		b.WriteByte('\n')
	}
	for i, ln := range new_ {
		b.WriteString("        ")
		b.WriteString(diffAddSty.Render("+ " + ln))
		if i < len(new_)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func splitPreview(s string, max int) []string {
	lines := strings.Split(s, "\n")
	if len(lines) > max {
		lines = append(lines[:max], "…")
	}
	for i, ln := range lines {
		lines[i] = truncate(ln, 100)
	}
	return lines
}
