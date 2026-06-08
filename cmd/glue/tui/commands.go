package tui

import (
	"fmt"
	"sort"
	"strings"
)

// slashCommand is a parsed `/cmd arg` from the input box. Empty Name
// means the input was not a slash command.
type slashCommand struct {
	Name string
	Arg  string
}

// parseSlashCommand recognizes `/word optional rest`. Leading whitespace
// is tolerated; everything after the first space becomes Arg verbatim.
func parseSlashCommand(s string) (slashCommand, bool) {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "/") {
		return slashCommand{}, false
	}
	body := strings.TrimPrefix(t, "/")
	if body == "" {
		return slashCommand{}, false
	}
	name, arg, _ := strings.Cut(body, " ")
	return slashCommand{Name: strings.ToLower(name), Arg: strings.TrimSpace(arg)}, true
}

// describeCommands renders the /help body.
func describeCommands() string {
	type row struct{ name, doc string }
	rows := []row{
		{"/help", "show this list"},
		{"/exit, /quit, /q", "leave the TUI"},
		{"/clear, /new", "clear the transcript and start a fresh session id"},
		{"/usage", "show this turn's token usage (when the provider reports it)"},
		{"/tools", "list registered tools"},
		{"/model <id>", "switch model for subsequent turns"},
		{"/session [id]", "print current session id, or switch to <id>"},
		{"/compact", "summarize older messages to free context window"},
		{"/resume", "pick a past session and replay it into the transcript"},
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	var b strings.Builder
	for i, r := range rows {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(fmt.Sprintf("  %-22s  %s", r.name, r.doc))
	}
	return b.String()
}
