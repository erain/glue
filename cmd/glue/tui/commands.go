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

// slashSpec describes one slash command for both /help and the inline
// autocomplete picker, so the two never drift. Name is the primary form
// (no leading slash); Aliases are equivalents that dispatch the same way.
type slashSpec struct {
	Name    string
	Aliases []string
	Args    string // e.g. "<id>", "[N]", or "" when the command takes none
	Desc    string
}

// display renders the command for the /help table and the picker, e.g.
// "/exit, /quit, /q" or "/model <id>".
func (s slashSpec) display() string {
	name := "/" + s.Name
	for _, a := range s.Aliases {
		name += ", /" + a
	}
	if s.Args != "" {
		name += " " + s.Args
	}
	return name
}

// slashSpecs is the canonical command list. Keep it in sync with the
// dispatch in (*Model).handleSlash.
func slashSpecs() []slashSpec {
	return []slashSpec{
		{Name: "help", Desc: "show this list"},
		{Name: "exit", Aliases: []string{"quit", "q"}, Desc: "leave the TUI"},
		{Name: "clear", Aliases: []string{"new"}, Desc: "clear the transcript and start a fresh session id"},
		{Name: "usage", Desc: "show this turn's token usage (when the provider reports it)"},
		{Name: "tools", Desc: "list registered tools"},
		{Name: "model", Args: "<id>", Desc: "switch model for subsequent turns"},
		{Name: "session", Args: "[id]", Desc: "print current session id, or switch to <id>"},
		{Name: "compact", Desc: "summarize older messages to free context window"},
		{Name: "resume", Desc: "pick a past session and replay it into the transcript"},
		{Name: "fork", Args: "[N]", Desc: "branch from message N (default: last user turn) into a new session"},
		{Name: "clone", Desc: "duplicate the current session into a fresh id"},
		{Name: "tree", Desc: "visualize and switch between branches in this session's lineage"},
	}
}

// describeCommands renders the /help body.
func describeCommands() string {
	specs := slashSpecs()
	sort.SliceStable(specs, func(i, j int) bool { return specs[i].display() < specs[j].display() })
	var b strings.Builder
	for i, s := range specs {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(fmt.Sprintf("  %-22s  %s", s.display(), s.Desc))
	}
	return b.String()
}
