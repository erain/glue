// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/internal/eventlog"
	"github.com/erain/glue/jobs"
)

const (
	renderMaxLine      = 160 // longest rendered line outside --verbose
	renderFailureLines = 10  // output lines shown for a failed tool call
)

// jobRenderer turns a job's event stream into a readable transcript: phase
// headers, one line per tool call with its duration, and a closing summary.
// Every line except streamed model text starts with the elapsed time.
type jobRenderer struct {
	out        io.Writer
	verbose    bool
	start      time.Time
	phase      string
	wroteDelta bool
	tools      map[string]toolStart
}

// toolStart remembers a tool call's start until its tool_end arrives.
type toolStart struct {
	at   time.Time
	args json.RawMessage
}

func newJobRenderer(out io.Writer, verbose bool) *jobRenderer {
	return &jobRenderer{out: out, verbose: verbose, tools: map[string]toolStart{}}
}

func (r *jobRenderer) line(t time.Time, format string, args ...any) {
	if r.wroteDelta {
		fmt.Fprintln(r.out)
		r.wroteDelta = false
	}
	text := fmt.Sprintf(format, args...)
	if !r.verbose {
		text = truncateRunes(text, renderMaxLine-8)
	}
	fmt.Fprintf(r.out, "[%s] %s\n", r.elapsed(t), text)
}

func (r *jobRenderer) raw(text string) {
	if r.wroteDelta {
		fmt.Fprintln(r.out)
		r.wroteDelta = false
	}
	fmt.Fprintln(r.out, text)
}

func (r *jobRenderer) elapsed(t time.Time) string {
	if r.start.IsZero() || t.IsZero() || t.Before(r.start) {
		return "00:00"
	}
	d := t.Sub(r.start).Round(time.Second)
	if d >= time.Hour {
		return fmt.Sprintf("%d:%02d:%02d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
	}
	return fmt.Sprintf("%02d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}

// flush ends a partially written line of streamed model text.
func (r *jobRenderer) flush() {
	if r.wroteDelta {
		fmt.Fprintln(r.out)
		r.wroteDelta = false
	}
}

// render prints one event. It does not handle permission prompts.
func (r *jobRenderer) render(ev eventlog.Envelope) {
	if r.start.IsZero() {
		r.start = ev.Time
	}
	if ev.Phase != "" && ev.Phase != r.phase {
		switch ev.Phase {
		case "setup":
			r.line(ev.Time, "setup")
		case "planner":
			r.line(ev.Time, "planning")
		case "checker":
			r.line(ev.Time, "checking")
		}
		r.phase = ev.Phase
	}
	switch ev.Type {
	case string(glue.EventTextDelta):
		if delta := payloadString(ev.Payload, "delta"); delta != "" && (ev.Phase == "" || ev.Phase == "maker") {
			fmt.Fprint(r.out, delta)
			r.wroteDelta = !strings.HasSuffix(delta, "\n")
		}
	case string(glue.GoalEventPlan):
		ge, _ := decodePayload[glue.GoalEvent](ev.Payload)
		r.line(ev.Time, "plan (%d items):", len(ge.Checklist))
		for _, item := range ge.Checklist {
			r.raw("          [ ] " + truncateRunes(item.Title, renderMaxLine-14))
		}
	case string(glue.GoalEventIterationStart):
		ge, _ := decodePayload[glue.GoalEvent](ev.Payload)
		r.line(ev.Time, "iteration %d (%s tokens so far)", ge.Iteration, thousands(ge.Usage.TotalTokens))
	case string(glue.GoalEventVerdict):
		ge, _ := decodePayload[glue.GoalEvent](ev.Payload)
		r.line(ev.Time, "verdict: %s done — %s", plainFraction(ge.Checklist), oneLine(ge.Message))
	case string(glue.EventToolStart):
		le, _ := decodePayload[glue.Event](ev.Payload)
		st := toolStart{at: ev.Time}
		if le.ToolCall != nil {
			st.args = le.ToolCall.Arguments
		}
		r.tools[toolCallID(le)] = st
	case string(glue.EventToolEnd):
		r.toolEnd(ev)
	case string(loopEventGuardrail):
		r.line(ev.Time, "guardrail: %s", oneLine(cmp.Or(payloadString(ev.Payload, "error"), "triggered")))
	case "operator_message":
		if text := payloadString(ev.Payload, "text"); text != "" {
			r.line(ev.Time, "operator: %s", oneLine(text))
		}
	case "turn_interrupted":
		p, _ := decodePayload[struct {
			Metadata struct {
				Canceled string   `json:"canceled"`
				Tools    []string `json:"tools"`
			} `json:"metadata"`
		}](ev.Payload)
		what := "the model request"
		if p.Metadata.Canceled == "tools" {
			what = "tool call " + strings.Join(p.Metadata.Tools, ", ")
		}
		r.line(ev.Time, "interrupted %s; the next turn starts with your message", what)
	case "question":
		q, _ := decodePayload[struct {
			Question string `json:"question"`
		}](ev.Payload)
		r.line(ev.Time, "question for you: %s", oneLine(q.Question))
	case "answer":
		r.line(ev.Time, "answered: %s", oneLine(payloadString(ev.Payload, "text")))
	case "question_timeout", "question_assumed":
		r.line(ev.Time, "no answer; the agent proceeds on its best assumption")
	case "setup_end":
		var end struct {
			OK      bool                `json:"ok"`
			Results []jobs.VerifyResult `json:"results"`
			Changed []string            `json:"changed"`
		}
		raw, _ := json.Marshal(ev.Payload)
		_ = json.Unmarshal(raw, &end)
		for _, v := range end.Results {
			mark := "✓"
			if v.ExitCode != 0 {
				mark = "✗"
			}
			r.line(ev.Time, "  %s %s  (exit %d, %.1fs)", mark, jobs.ShellJoin(v.Argv), v.ExitCode, float64(v.DurationMS)/1000)
			if v.ExitCode != 0 {
				for _, l := range lastLines(v.OutputTail, renderFailureLines) {
					r.raw("            " + truncateRunes(l, renderMaxLine-12))
				}
			}
		}
		if len(end.Changed) > 0 {
			r.line(ev.Time, "  setup changed %d file(s); they are committed with the job unless .gitignore covers them", len(end.Changed))
		}
	case "operator_message_delivered":
		r.line(ev.Time, "operator message delivered to the agent")
	case "run_interrupted":
		r.line(ev.Time, "interrupted: %s", cmp.Or(payloadString(ev.Payload, "reason"), payloadString(ev.Payload, "message"), "run interrupted"))
	case "task_suspended":
		r.line(ev.Time, "task suspended (%s)", cmp.Or(payloadString(ev.Payload, "reason"), "suspended"))
	case "task_resumed":
		r.line(ev.Time, "task resumed")
	case "job_resumed":
		p, _ := decodePayload[struct {
			From   int    `json:"from_iteration"`
			Reason string `json:"reason"`
		}](ev.Payload)
		r.line(ev.Time, "resumed: iteration %d (%s)", p.From, cmp.Or(p.Reason, "resumed"))
	}
}

const loopEventGuardrail = "guardrail"

func toolCallID(le glue.Event) string {
	if le.ToolCallID != "" {
		return le.ToolCallID
	}
	if le.ToolCall != nil {
		return le.ToolCall.ID
	}
	return ""
}

func (r *jobRenderer) toolEnd(ev eventlog.Envelope) {
	le, _ := decodePayload[glue.Event](ev.Payload)
	name := le.ToolName
	if name == "" && le.ToolCall != nil {
		name = le.ToolCall.Name
	}
	var args json.RawMessage
	if le.ToolCall != nil {
		args = le.ToolCall.Arguments
	}
	failed, text := false, ""
	if le.ToolResult != nil {
		failed = le.ToolResult.IsError
		for _, p := range le.ToolResult.Content {
			text += p.Text
		}
	}
	dur := ""
	if st, ok := r.tools[toolCallID(le)]; ok {
		if len(args) == 0 {
			args = st.args
		}
		if !ev.Time.IsZero() && !st.at.IsZero() {
			dur = fmt.Sprintf("  %.1fs", ev.Time.Sub(st.at).Seconds())
		}
		delete(r.tools, toolCallID(le))
	}
	mark := "✓"
	if failed {
		mark = "✗"
	}
	r.line(ev.Time, "  %s %s%s%s", mark, name, keyArgument(args), dur)
	if !failed && !r.verbose {
		return
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if !r.verbose && len(lines) > renderFailureLines {
		lines = append(lines[:renderFailureLines], fmt.Sprintf("… %d more lines (--verbose shows all)", len(lines)-renderFailureLines))
	}
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if !r.verbose {
			l = truncateRunes(l, renderMaxLine-12)
		}
		r.raw("            " + l)
	}
}

// keyArgument picks the argument that identifies a tool call: a path, a
// command line, or a search pattern.
func keyArgument(raw json.RawMessage) string {
	var args map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &args) != nil {
		return ""
	}
	if argv, ok := args["argv"].([]any); ok && len(argv) > 0 {
		parts := make([]string, 0, len(argv))
		for _, a := range argv {
			parts = append(parts, fmt.Sprint(a))
		}
		return " " + truncateRunes(strings.Join(parts, " "), 90)
	}
	for _, k := range []string{"path", "pattern", "query", "command", "url", "name"} {
		if v, ok := args[k].(string); ok && v != "" {
			return " " + truncateRunes(v, 90)
		}
	}
	return ""
}

// summary prints the closing block for a finished job.
func (r *jobRenderer) summary(ctx context.Context, c jobAPI, id string, ev eventlog.Envelope) {
	p, _ := decodePayload[struct {
		Status jobs.Status     `json:"status"`
		Result glue.GoalResult `json:"result"`
		Error  string          `json:"error"`
	}](ev.Payload)
	if s := cmp.Or(strings.TrimSpace(p.Result.Summary), strings.TrimSpace(p.Error)); s != "" {
		r.line(ev.Time, "job %s — %s", p.Status, oneLine(s))
	} else {
		r.line(ev.Time, "job %s", p.Status)
	}
	res, err := c.Result(ctx, id)
	if err != nil {
		res = jobs.Result{Status: p.Status, Iterations: p.Result.Iterations, Usage: p.Result.Usage}
	}
	row := func(k, v string) { r.raw(fmt.Sprintf("  %-12s %s", k, v)) }
	r.raw(strings.Repeat("─", 40))
	row("status", string(cmp.Or(res.Status, p.Status)))
	row("iterations", fmt.Sprint(res.Iterations))
	if !r.start.IsZero() && !ev.Time.IsZero() {
		row("duration", ev.Time.Sub(r.start).Round(time.Second).String())
	}
	row("tokens", thousands(max(res.Usage.TotalTokens, p.Result.Usage.TotalTokens)))
	for _, v := range res.Setup {
		mark := "✓"
		if v.ExitCode != 0 {
			mark = "✗"
		}
		row("setup", fmt.Sprintf("%s %s (exit %d, %.1fs)", mark, truncateRunes(jobs.ShellJoin(v.Argv), 80), v.ExitCode, float64(v.DurationMS)/1000))
	}
	if res.Branch != "" && res.HeadSHA != "" && res.HeadSHA != res.BaseSHA {
		row("branch", res.Branch)
	}
	if res.PRURL != "" {
		row("pull request", res.PRURL)
	}
	for _, v := range res.Verify {
		mark := "✓"
		if v.ExitCode != 0 {
			mark = "✗"
		}
		row("verify", fmt.Sprintf("%s %s (exit %d, %.1fs)", mark, truncateRunes(jobs.ShellJoin(v.Argv), 80), v.ExitCode, float64(v.DurationMS)/1000))
	}
	if res.DeliveryError != "" {
		row("delivery", "failed: "+oneLine(res.DeliveryError))
	}
	for _, a := range res.Assumptions {
		row("assumed", truncateRunes(a, 120))
	}
	row("next", fmt.Sprintf("glue job result %s · glue job diff %s · glue job pull %s", id, id, id))
}

func lastLines(s string, n int) []string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

func thousands(n int64) string {
	s := fmt.Sprint(n)
	if n < 0 {
		return s
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
