package peggy

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/daemon"
)

// Version is the package version string surfaced by `peggy --version`.
// Bumped by hand at release time.
const Version = "0.2.0"

const (
	defaultDaemonListenAddr      = "127.0.0.1:0"
	defaultDaemonShutdownTimeout = 5 * time.Second
)

type serveFunc func(context.Context, serveConfig, http.Handler, io.Writer) error

type serveConfig struct {
	ListenAddr        string
	Token             string
	TokenSource       string
	MetadataPath      string
	PermissionTimeout time.Duration
	ShutdownTimeout   time.Duration
}

// Run is the top-level CLI entry point. It parses args, loads the
// settings and identity files, constructs a Peggy, and dispatches a
// single prompt. Returns a process exit code.
//
// Tests can drive Run directly with a synthetic args slice and
// captured stdout/stderr writers. For programmatic use prefer
// constructing a Peggy via New.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return RunWithInput(ctx, args, os.Stdin, stdout, stderr)
}

// RunWithInput is like [Run] but lets tests and embedded callers provide the
// stdin reader used by the CLI permission prompt.
func RunWithInput(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runWithDeps(ctx, args, stdin, stdout, stderr, servePeggyDaemon)
}

func runWithDeps(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, serve serveFunc) int {
	if len(args) > 0 && args[0] == "serve" {
		return runServe(ctx, args[1:], stdout, stderr, serve)
	}

	fs := flag.NewFlagSet("peggy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		configPath           = fs.String("config", "", "path to settings.json (overrides $PEGGY_CONFIG / XDG / ~/.config/peggy)")
		soulPath             = fs.String("soul", "", "path to identity Markdown (overrides $PEGGY_SOUL / XDG / ~/.config/peggy/SOUL.md)")
		sessionID            = fs.String("session", "default", "session id (file-backed transcripts key off this)")
		showVersion          = fs.Bool("version", false, "print version and exit")
		enableCoding         = fs.Bool("coding", false, "enable local coding tools for this prompt")
		codingWorkDir        = fs.String("workdir", "", "workspace root for --coding (default current directory)")
		codingAllowOverwrite = fs.Bool("coding-allow-overwrite", false, "allow write_file to replace existing files after model and permission approval")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, `peggy — long-running personal-assistant agent built on glue.

Usage:
  peggy [flags] "<prompt text>"
  peggy serve [flags]

Examples:
  peggy "hello"
  peggy --session work "remind me about the migration plan"
  peggy --config /tmp/peggy.json "what do you know about my Aussie?"
  peggy --coding --workdir . "run the tests and fix the failure"
  peggy serve --config ~/.config/peggy/settings.json

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
	applyCodingFlags(&settings, *enableCoding, *codingWorkDir, *codingAllowOverwrite)

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

	var permission glue.Permission
	if settings.Coding.Enabled {
		permission = NewCLIPermission(CLIPermissionOptions{Stdin: stdin, Stderr: stderr})
		workDir := settings.Coding.WorkDir
		if strings.TrimSpace(workDir) == "" {
			workDir = "."
		}
		fmt.Fprintf(stderr, "peggy: coding tools enabled for %s\n", workDir)
	}

	p, err := New(Options{
		Settings:   settings,
		Soul:       soul,
		Stderr:     stderr,
		Permission: permission,
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

func runServe(ctx context.Context, args []string, stdout, stderr io.Writer, serve serveFunc) int {
	fs := flag.NewFlagSet("peggy serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		configPath           = fs.String("config", "", "path to settings.json (overrides $PEGGY_CONFIG / XDG / ~/.config/peggy)")
		soulPath             = fs.String("soul", "", "path to identity Markdown (overrides $PEGGY_SOUL / XDG / ~/.config/peggy/SOUL.md)")
		listenAddr           = fs.String("listen", defaultDaemonListenAddr, "local listen address")
		tokenFlag            = fs.String("token", "", "bearer token; defaults to GLUE_DAEMON_TOKEN or a generated token")
		metadataPath         = fs.String("metadata", daemon.DefaultMetadataPath(), "connection metadata JSON path; empty disables metadata file")
		permissionTimeout    = fs.Duration("permission-timeout", 0, "permission decision timeout; 0 uses daemon default")
		showVersion          = fs.Bool("version", false, "print version and exit")
		enableCoding         = fs.Bool("coding", false, "enable local coding tools for this daemon")
		codingWorkDir        = fs.String("workdir", "", "workspace root for --coding (default current directory)")
		codingAllowOverwrite = fs.Bool("coding-allow-overwrite", false, "allow write_file to replace existing files after model and permission approval")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, `peggy serve — run Peggy as a local HTTP+SSE daemon.

Usage:
  peggy serve [flags]

Then connect from another terminal:
  glue connect --prompt "hello"

Flags:
`)
		fs.PrintDefaults()
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
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "peggy serve: positional args not supported")
		return 2
	}

	settings, settingsPath, err := LoadSettings(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "peggy serve: %v\n", err)
		return 1
	}
	if settingsPath == "" {
		fmt.Fprintln(stderr, "peggy serve: no settings.json found; using built-in defaults")
	}
	applyCodingFlags(&settings, *enableCoding, *codingWorkDir, *codingAllowOverwrite)

	soul, soulPathUsed, err := LoadSoul(*soulPath)
	if err != nil {
		fmt.Fprintf(stderr, "peggy serve: %v\n", err)
		return 1
	}
	if soul == "" {
		fmt.Fprintln(stderr, "peggy serve: no SOUL.md found; running without identity context")
	} else {
		fmt.Fprintf(stderr, "peggy serve: identity loaded from %s (%d bytes)\n", soulPathUsed, len(soul))
	}
	if settings.Coding.Enabled {
		workDir := settings.Coding.WorkDir
		if strings.TrimSpace(workDir) == "" {
			workDir = "."
		}
		fmt.Fprintf(stderr, "peggy serve: coding tools enabled for %s\n", workDir)
	}

	token, tokenSource, err := daemon.ResolveToken(*tokenFlag)
	if err != nil {
		fmt.Fprintf(stderr, "peggy serve: %v\n", err)
		return 1
	}
	if strings.TrimSpace(*metadataPath) == "" && tokenSource == "generated" {
		fmt.Fprintln(stderr, "peggy serve: metadata disabled requires --token or GLUE_DAEMON_TOKEN")
		return 1
	}

	p, err := New(Options{
		Settings: settings,
		Soul:     soul,
		Stderr:   stderr,
	})
	if err != nil {
		fmt.Fprintf(stderr, "peggy serve: setup: %v\n", err)
		return 1
	}
	defer p.Close()

	handler, err := daemon.New(daemon.Options{
		Host:              p.Agent(),
		Token:             token,
		PermissionTimeout: *permissionTimeout,
	})
	if err != nil {
		fmt.Fprintf(stderr, "peggy serve: setup: %v\n", err)
		return 1
	}

	if err := serve(ctx, serveConfig{
		ListenAddr:        *listenAddr,
		Token:             token,
		TokenSource:       tokenSource,
		MetadataPath:      *metadataPath,
		PermissionTimeout: *permissionTimeout,
		ShutdownTimeout:   defaultDaemonShutdownTimeout,
	}, handler, stdout); err != nil {
		fmt.Fprintf(stderr, "peggy serve: %v\n", err)
		return 1
	}
	return 0
}

func applyCodingFlags(settings *Settings, enable bool, workDir string, allowOverwrite bool) {
	if settings == nil {
		return
	}
	if enable || workDir != "" || allowOverwrite {
		settings.Coding.Enabled = true
	}
	if workDir != "" {
		settings.Coding.WorkDir = workDir
	}
	if allowOverwrite {
		settings.Coding.AllowOverwrite = true
	}
}

func servePeggyDaemon(ctx context.Context, cfg serveConfig, handler http.Handler, stdout io.Writer) error {
	return daemon.ServeLocal(ctx, daemon.LocalConfig{
		Name:            "peggy daemon",
		ListenAddr:      cfg.ListenAddr,
		Token:           cfg.Token,
		TokenSource:     cfg.TokenSource,
		MetadataPath:    cfg.MetadataPath,
		ShutdownTimeout: cfg.ShutdownTimeout,
	}, handler, stdout)
}
