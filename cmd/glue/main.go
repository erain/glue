// Command glue is the local CLI runner for Glue agents.
//
// Run a single prompt:
//
//	glue run --prompt "Say hi" --id local-dev --store .glue/sessions
//
// The default subcommand uses Glue's Gemini-backed agent. Streaming text
// is written to stdout; provider, store, or flag errors exit non-zero.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/erain/glue"
	"github.com/erain/glue/internal/modelselection"
	"github.com/erain/glue/jobs"
	"github.com/erain/glue/providers"
	filestore "github.com/erain/glue/stores/file"
	toolscoding "github.com/erain/glue/tools/coding"

	// Register the shipped providers so they resolve through the
	// providers registry by name (--provider). Importing for side
	// effects only; the binary selects providers at runtime.
	_ "github.com/erain/glue/providers/codex"
	_ "github.com/erain/glue/providers/deepseek"
	_ "github.com/erain/glue/providers/gemini"
	_ "github.com/erain/glue/providers/nvidia"
	_ "github.com/erain/glue/providers/openrouter"
)

const defaultListenAddr = "127.0.0.1:0"

const defaultShutdownTimeout = 5 * time.Second

// providerFactory constructs a [glue.Provider] for the named provider, or
// returns an error. The error is the hook the default factory uses to
// surface a missing API key before any API call is attempted. Tests inject
// a factory that returns a canned provider and ignores the name.
type providerFactory func(name string) (glue.Provider, error)

type envFiles []string

func (e *envFiles) String() string { return strings.Join(*e, ",") }

func (e *envFiles) Set(value string) error {
	*e = append(*e, value)
	return nil
}

type repeatedStrings []string

func (r *repeatedStrings) String() string { return strings.Join(*r, ",") }

func (r *repeatedStrings) Set(value string) error {
	*r = append(*r, value)
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(runCLI(ctx, os.Args[1:], os.Stdout, os.Stderr, defaultProviderFactory))
}

// defaultProviderFactory resolves a provider from the registry by name.
// For providers that authenticate through an environment variable, it
// surfaces a missing-key error before any API call. Subscription-auth
// providers (e.g. codex) carry no env key and validate their own auth at
// request time.
func defaultProviderFactory(name string) (glue.Provider, error) {
	provider, _, envKey, err := providers.New(name)
	if err != nil {
		return nil, err
	}
	if envKey != "" && strings.TrimSpace(os.Getenv(envKey)) == "" {
		return nil, fmt.Errorf("%s is required for provider %q (set in shell or pass --env <file>)", envKey, name)
	}
	return provider, nil
}

// isTTY reports whether v is backed by a terminal. Tests override it to
// exercise TTY and non-TTY CLI paths deterministically.
var isTTY func(any) bool = isFileTTY

func runCLI(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer, newProvider providerFactory) int {
	return runCLIWithDeps(ctx, args, os.Stdin, stdout, stderr, newProvider, serveDaemon, http.DefaultClient)
}

func runCLIWithServe(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer, newProvider providerFactory, serve serveFunc) int {
	return runCLIWithDeps(ctx, args, os.Stdin, stdout, stderr, newProvider, serve, http.DefaultClient)
}

func runCLIWithDeps(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer, newProvider providerFactory, serve serveFunc, client httpDoer) int {
	if os.Getenv("GLUE_GIT_ASKPASS") == "1" {
		return jobs.Askpass(args, stdout)
	}
	if len(args) == 0 {
		if !isTTY(stdin) || !isTTY(stdout) {
			printUsage(stdout)
			return 0
		}
		if err := runAgentCommand(ctx, "glue", true, nil, stdin, stdout, stderr, newProvider); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	if args[0] == "help" && len(args) > 1 && !strings.HasPrefix(args[1], "-") {
		args = []string{args[1], "--help"}
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "-help" || args[0] == "--help" {
		printUsage(stdout)
		return 0
	}
	if args[0] == "version" || args[0] == "-v" || args[0] == "--version" {
		printVersion(stdout)
		return 0
	}
	if strings.HasPrefix(args[0], "-") && hasRemoteFlag(args) {
		if err := jobCommand(ctx, append([]string{"submit"}, args...), stdin, stdout, stderr, client); err != nil {
			fmt.Fprintln(stderr, err)
			return exitCodeOf(err)
		}
		return 0
	}

	switch args[0] {
	case "git-askpass":
		return jobs.Askpass(args[1:], stdout)
	case "models":
		if err := modelsCommand(args[1:], stdout, stderr); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	case "code", "run":
		if err := runAgentCommand(ctx, "glue "+args[0], args[0] == "code", args[1:], stdin, stdout, stderr, newProvider); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	case "connect":
		if err := connectCommand(ctx, args[1:], stdin, stdout, stderr, client); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	case "serve":
		if err := serveCommand(ctx, args[1:], stdout, stderr, newProvider, serve); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	case "goal":
		code, err := goalCommand(ctx, args[1:], stdin, stdout, stderr, newProvider)
		if err != nil {
			fmt.Fprintln(stderr, err)
		}
		return code
	case "job":
		if err := jobCommand(ctx, args[1:], stdin, stdout, stderr, client); err != nil {
			fmt.Fprintln(stderr, err)
			return exitCodeOf(err)
		}
		return 0
	case "remote":
		if err := remoteCommand(ctx, args[1:], stdout, stderr, client); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		printUsage(stderr)
		return 1
	}
}

func defaultSelection(coding bool) (string, string) { return modelselection.Default(coding) }
func resolveSelection(coding bool, name, model string) (string, string, error) {
	return modelselection.Resolve(coding, name, model)
}
func resolveProvider(name, model string) (string, string, error) {
	return modelselection.Provider(name, model)
}

// isFileTTY reports whether the given reader/writer is an os.File backed
// by a terminal. Returns false for non-files (test buffers, pipes) and
// for closed/invalid fds.
func isFileTTY(v any) bool {
	f, ok := v.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

func newAgent(newProvider providerFactory, cfg agentConfig) (*glue.Agent, error) {
	provider, err := newProvider(cfg.Provider)
	if err != nil {
		return nil, err
	}
	systemPrompt, autoContinue := capabilityDefaults(cfg.Provider, cfg.Tools, cfg.Coding)
	return glue.NewAgent(glue.AgentOptions{
		Provider:     provider,
		Options:      cfg.Options,
		Model:        normalizeModel(cfg.Model),
		Tools:        append([]glue.Tool(nil), cfg.Tools...),
		Store:        filestore.New(cfg.StoreDir),
		WorkDir:      cfg.WorkDir,
		Permission:   cfg.Permission,
		SystemPrompt: systemPrompt,
		AutoContinue: autoContinue,
	}), nil
}

// capabilityDefaults derives capability-driven agent settings from the
// providers registry: the coding system prompt is assembled from the
// active toolset in the provider's preferred variant, and the
// narrate-then-stop nudge is enabled only for providers that declare
// the stall (today: gemini).
func capabilityDefaults(providerName string, tools []glue.Tool, coding bool) (systemPrompt string, autoContinue bool) {
	caps := providers.CapabilitiesFor(providerName)
	if coding && len(tools) > 0 {
		systemPrompt = toolscoding.SystemPrompt(tools, caps.PromptVariant)
	}
	return systemPrompt, caps.AutoContinue && len(tools) > 0
}

func normalizeModel(model string) string {
	return strings.TrimPrefix(model, "gemini/")
}

// printVersion writes a short version banner sourced from the linker-
// embedded Go build info: module version, git revision, build time, and
// Go toolchain version. Identical content to what `go version -m
// $(which glue)` exposes but reachable as `glue version` / `--version`
// so users can self-diagnose stale binaries without running a side
// command.
func printVersion(w io.Writer) {
	mod := "(devel)"
	rev := ""
	when := ""
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" {
			mod = v
		}
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
				if len(rev) > 12 {
					rev = rev[:12]
				}
			case "vcs.time":
				when = s.Value
			}
		}
	}
	fmt.Fprintf(w, "glue %s", mod)
	if rev != "" {
		fmt.Fprintf(w, " (%s)", rev)
	}
	if when != "" {
		fmt.Fprintf(w, " · built %s", when)
	}
	fmt.Fprintf(w, " · %s\n", runtime.Version())
}

func hasRemoteFlag(args []string) bool {
	for _, a := range args {
		if a == "--" || a == "--remote" || a == "-remote" || strings.HasPrefix(a, "--remote=") || strings.HasPrefix(a, "-remote=") {
			return a != "--"
		}
	}
	return false
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: glue [command] [flags]

Code locally:
  glue                                Start interactive coding TUI in the current directory
  glue code [flags]                   Run coding agent (--continue, --select, --model, --prompt)
  glue run --prompt "<text>" [flags]  Run a one-shot or scripted prompt (--coding for tools)
  glue goal "<objective>" [flags]     Pursue a local goal autonomously (--coding, --worktree)

Run remotely on AX:
  glue job init                       Check setup and pair phone notifications via QR code
  glue remote setup [flags]           Deploy opt-in cluster gateway for 24/7 browser access
  glue --remote "<objective>" [flags] Submit a remote coding job (--snapshot, --verify, --attach)
  glue --remote -i "<prompt>" [flags] Start an interactive multi-turn remote workspace

Manage remote jobs:
  glue job ui [--tunnel] [--open]     Open local browser UI with #token magic link / QR
  glue job list | show <id>           List jobs or inspect status, questions, and permissions
  glue job attach [<id>]              Stream live job events (Ctrl-C detaches cleanly)
  glue job say | answer <id> "<text>" Steer a running job (--interrupt) or answer a question
  glue job diff | pull | pr [<id>]    Review patch, pull branch/changes, or open a draft PR
  glue job continue <id> [flags]      Resume a job with instructions or --from-pr feedback

Models & daemon:
  glue models [--provider <name>]     List providers, models, defaults, and thinking levels
  glue serve [flags]                  Start local HTTP+SSE daemon (--coding, --jobs)
  glue connect [flags]                Run a daemon prompt/skill or inspect daemon state
  glue version                        Print version, git revision, and Go toolchain

Defaults: coding/goal/remote use gemini/gemini-3-flash-preview (thinking low);
  glue run without --coding uses openrouter/openrouter/free.
  Run "glue <command> --help" or "glue help <command>" for command flags.
`)
}

func parseCommandFlags(flags *flag.FlagSet, args []string, stdout, stderr io.Writer, usage string) (bool, error) {
	flags.SetOutput(stderr)
	flags.Usage = func() {}
	err := flags.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		printFlagSetUsage(flags, stdout, usage)
		return true, nil
	}
	return false, err
}

func printFlagSetUsage(flags *flag.FlagSet, w io.Writer, usage string) {
	if strings.TrimSpace(usage) != "" {
		fmt.Fprintf(w, "%s\n\n", strings.TrimRight(usage, "\n"))
	}
	fmt.Fprintln(w, "Flags:")
	flags.VisitAll(func(f *flag.Flag) {
		typ, desc := flag.UnquoteUsage(f)
		spec := strings.TrimSpace(map[bool]string{true: "-", false: "--"}[len(f.Name) == 1] + f.Name + " " + typ)
		if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" && f.DefValue != "0s" {
			desc += fmt.Sprintf(" (default %s)", f.DefValue)
		}
		fmt.Fprintf(w, "  %-28s %s\n", spec, desc)
	})
}

func loadEnvFiles(files []string) error {
	shellEnv := map[string]struct{}{}
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			shellEnv[key] = struct{}{}
		}
	}

	for _, path := range files {
		if err := loadEnvFile(path, shellEnv); err != nil {
			return err
		}
	}
	return nil
}

func loadEnvFile(path string, shellEnv map[string]struct{}) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s:%d: expected KEY=VALUE", path, lineNumber)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return fmt.Errorf("%s:%d: empty key", path, lineNumber)
		}
		if _, exists := shellEnv[key]; exists {
			continue
		}
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return scanner.Err()
}
