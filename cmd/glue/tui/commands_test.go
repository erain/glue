package tui

import (
	"strings"
	"testing"
)

func TestParseSlashCommand(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in       string
		wantOK   bool
		wantName string
		wantArg  string
	}{
		{"", false, "", ""},
		{"hello world", false, "", ""},
		{"/", false, "", ""},     // bare slash is not a command
		{"  /", false, "", ""},   // whitespace tolerated, still nothing
		{"/help", true, "help", ""},
		{"  /help  ", true, "help", ""},
		{"/HELP", true, "help", ""}, // case-insensitive name
		{"/model gpt-5", true, "model", "gpt-5"},
		{"/session cli:work  ", true, "session", "cli:work"},
		{"/session   spaced  arg", true, "session", "spaced  arg"}, // arg verbatim past first space
	}
	for _, c := range cases {
		c := c
		t.Run(c.in, func(t *testing.T) {
			got, ok := parseSlashCommand(c.in)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !c.wantOK {
				return
			}
			if got.Name != c.wantName || got.Arg != c.wantArg {
				t.Fatalf("got %#v, want name=%q arg=%q", got, c.wantName, c.wantArg)
			}
		})
	}
}

func TestDescribeCommandsContainsCoreEntries(t *testing.T) {
	t.Parallel()
	body := describeCommands()
	for _, want := range []string{"/help", "/exit", "/clear", "/usage", "/tools", "/model", "/session"} {
		if !strings.Contains(body, want) {
			t.Errorf("describeCommands missing %q\n%s", want, body)
		}
	}
}
