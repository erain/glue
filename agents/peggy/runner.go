package peggy

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

// Version is the package version string surfaced by `peggy --version`.
// Bumped by hand at release time.
const Version = "0.1.0-dev"

// Run is the top-level CLI entry point. It parses args, loads the
// settings and identity files, constructs a Peggy, and dispatches a
// single prompt. Returns a process exit code.
//
// Tests can drive Run directly with a synthetic args slice and
// captured stdout/stderr writers. For programmatic use prefer
// constructing a Peggy via New.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("peggy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		configPath  = fs.String("config", "", "path to settings.json (overrides $PEGGY_CONFIG / XDG / ~/.config/peggy)")
		soulPath    = fs.String("soul", "", "path to identity Markdown (overrides $PEGGY_SOUL / XDG / ~/.config/peggy/SOUL.md)")
		sessionID   = fs.String("session", "default", "session id (file-backed transcripts key off this)")
		showVersion = fs.Bool("version", false, "print version and exit")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, `peggy — long-running personal-assistant agent built on glue.

Usage:
  peggy [flags] "<prompt text>"

Examples:
  peggy "hello"
  peggy --session work "remind me about the migration plan"
  peggy --config /tmp/peggy.json "what do you know about my Aussie?"

Flags:
`)
		fs.PrintDefaults()
		fmt.Fprintf(stderr, `
Config resolution: --config > $PEGGY_CONFIG > $XDG_CONFIG_HOME/peggy/settings.json > ~/.config/peggy/settings.json.
Identity (SOUL.md) resolution: --soul > $PEGGY_SOUL > $XDG_CONFIG_HOME/peggy/SOUL.md > ~/.config/peggy/SOUL.md. Missing identity is non-fatal.
`)
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if *showVersion {
		fmt.Fprintln(stdout, Version)
		return 0
	}

	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if prompt == "" {
		fs.Usage()
		return 2
	}

	settings, settingsPath, err := LoadSettings(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "peggy: %v\n", err)
		return 1
	}
	if settingsPath == "" {
		fmt.Fprintln(stderr, "peggy: no settings.json found; using built-in defaults")
	}

	soul, soulPathUsed, err := LoadSoul(*soulPath)
	if err != nil {
		fmt.Fprintf(stderr, "peggy: %v\n", err)
		return 1
	}
	if soul == "" {
		fmt.Fprintln(stderr, "peggy: no SOUL.md found; running without identity context")
	} else {
		fmt.Fprintf(stderr, "peggy: identity loaded from %s (%d bytes)\n", soulPathUsed, len(soul))
	}

	p, err := New(Options{
		Settings: settings,
		Soul:     soul,
		Stderr:   stderr,
	})
	if err != nil {
		fmt.Fprintf(stderr, "peggy: setup: %v\n", err)
		return 1
	}
	defer p.Close()

	if _, err := p.Prompt(ctx, *sessionID, prompt, stdout); err != nil {
		fmt.Fprintf(stderr, "\npeggy: prompt: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout) // trailing newline so shell prompts don't run on
	return 0
}
