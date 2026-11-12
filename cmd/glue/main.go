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
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
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
			return 1
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
	case "run":
		if err := runCommand(ctx, args[1:], stdin, stdout, stderr, newProvider); err != nil {
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
	fmt.Fprint(w, `Usage:
  glue --remote "<objective>" [--attach] [--repo <url|dir>] [--base <ref>] [--ignore-local] [--deliver <none|branch|pr>] [--verify <cmd>] [--policy <name>] [--env <path>]
  glue run --prompt <text> [--provider <name>] [--id <id> | --continue] [--model <model>] [--store <dir>] [--work <dir>] [--coding] [--env <path>]
  glue goal "<objective>" [--provider <name>] [--model <model>] [--store <dir>] [--work <dir>] [--coding] [--yolo] [--worktree] [--max-iterations <n>] [--budget <tokens>] [--env <path>]
  glue goal --resume [<id>] | --list [--store <dir>]
  glue job <login|submit|start|list|show|attach|result|approve|deny|say|continue|cancel|delete> [flags]
  glue serve [--jobs] [--job-from-ax] [--provider <name>] [--listen 127.0.0.1:0] [--token-file <path>] [--metadata <path>] [--model <model>] [--store <dir>] [--work <dir>] [--coding] [--env <path>]
  glue connect --prompt <text> [--id <id>] [--metadata <path>] [--base-url <url>] [--token <token>]
  glue connect --skill <name> [--arg key=value] [--id <id>] [--metadata <path>] [--base-url <url>] [--token <token>]
  glue connect --inspect [--inspect-json] [--metadata <path>] [--base-url <url>] [--token <token>]
  glue connect --status [--status-json] [--metadata <path>] [--base-url <url>] [--token <token>]
  glue connect --diagnose [--diagnose-json] [--metadata <path>] [--base-url <url>] [--token <token>]
  glue connect --tools [--tools-json] [--metadata <path>] [--base-url <url>] [--token <token>]
  glue connect --skills [--skills-json] [--metadata <path>] [--base-url <url>] [--token <token>]
  glue connect --roles [--roles-json] [--metadata <path>] [--base-url <url>] [--token <token>]
  glue connect --mcp-resources [--mcp-resources-json] [--metadata <path>] [--base-url <url>] [--token <token>]
  glue connect --mcp-prompts [--mcp-prompts-json] [--metadata <path>] [--base-url <url>] [--token <token>]
  glue connect --mcp-read --server <name> --uri <uri> [--mcp-read-json] [--metadata <path>] [--base-url <url>] [--token <token>]
  glue connect --mcp-prompt --server <name> --name <prompt> [--arg key=value] [--mcp-prompt-json] [--metadata <path>] [--base-url <url>] [--token <token>]
  glue connect --recall <query> [--recall-json] [--recall-memories] [--recall-limit <n>] [--metadata <path>] [--base-url <url>] [--token <token>]
  glue connect --memories [--memories-json] [--memory-limit <n>] [--metadata <path>] [--base-url <url>] [--token <token>]
  glue connect --forget-memory <id> [--forget-memory-json] [--metadata <path>] [--base-url <url>] [--token <token>]
  glue connect --permissions [--permissions-json] [--metadata <path>] [--base-url <url>] [--token <token>]
  glue connect --forget-permission <id> [--forget-permission-json] [--metadata <path>] [--base-url <url>] [--token <token>]

Commands:
  models   List curated providers, models, and supported thinking levels (no credentials needed).
  run      Run a local agent on any registered provider, optionally with coding tools.
  goal     Run an autonomous goal loop headlessly (plan → make → verify until done or a guardrail trips); exit code reflects the outcome.
  job      Submit, start, list, inspect, steer, and fetch results for durable coding jobs.
  serve    Start a local HTTP+SSE daemon for Glue sessions, optionally with coding tools.
  connect  Start a daemon prompt/skill run, or inspect daemon status/tools/skills/roles/MCP/recall surfaces.
  version  Print the binary's version, git revision, and Go toolchain (also: --version, -v).

Defaults: chat uses provider openrouter, model openrouter/free.
  --coding and goal use provider gemini, model gemini-3.8-flash.
Explicit --provider and --model override defaults.

Flags:
  --id       Session id. Defaults to "default".
  --prompt   Prompt text. Required unless --skill is set or connect is in an inspection mode.
  --skill    Connect mode: run one daemon skill instead of --prompt.
  --provider Run/serve provider: codex, deepseek, gemini, nvidia, or openrouter. Defaults to gemini.
             Use --provider codex for a ChatGPT-subscription coding agent (run "codex login" first).
  --select   Run mode: choose provider, model, and thinking level in terminal menus.
  --thinking Run/serve/goal thinking level for the selected model; see glue models.
             Omit or use default to preserve API defaults.
  --model    Model id. Defaults to the selected provider's default model. gemini/<model> accepted.
  --store    File session store directory. Defaults to .glue/sessions.
  --work     Workspace for AGENTS.md, skills, roles, and --coding tools. Defaults to ".".
  --coding
             Run/serve mode: register Glue's local coding tool bundle.
  --allow-binary
             Run/serve/goal --coding mode: allowed shell_exec binary basename; repeatable.
  --shell-env-pass / --shell-env / --shell-env-secret-allow / --shell-timeout
             Configure shell_exec environment pass-through, explicit KEY=VALUE vars, secret allowlist, and timeout.
  --coding-allow-overwrite
             Run/serve --coding mode: allow write_file overwrites when the model also sets overwrite=true.
  --listen   Serve listen address. Defaults to 127.0.0.1:0.
  --token    Serve bearer token. Defaults to GLUE_DAEMON_TOKEN or a generated token.
  --token-file
             Serve mode: durable bearer token file path (created with mode 0600 if missing).
  --job-from-ax
             Serve mode: create a job from AX task metadata or AX_TASK_YAML on startup (idempotent).
  --metadata Serve connection metadata JSON. Defaults to the user config directory.
  --base-url Connect daemon base URL override.
  --role     Connect role override.
  --max-turns Connect loop turn budget override.
  --usage   Run/connect mode: print token usage summary to stderr when available.
  --inspect  Connect mode: show daemon status and tools without starting a run.
  --inspect-json
             Connect --inspect mode: print JSON.
  --status   Connect mode: show daemon status and exit without starting a run.
  --status-json
             Connect --status mode: print JSON.
  --diagnose
             Connect mode: diagnose daemon metadata, auth, reachability, and runtime state.
  --diagnose-json
             Connect --diagnose mode: print JSON.
  --tools    Connect mode: list daemon tools and exit without starting a run.
  --tools-json
             Connect --tools mode: print JSON.
  --skills   Connect mode: list daemon skills and exit without starting a run.
  --skills-json
             Connect --skills mode: print JSON.
  --roles    Connect mode: list daemon roles and exit without starting a run.
  --roles-json
             Connect --roles mode: print JSON.
  --mcp-resources
             Connect mode: list daemon MCP resources and exit without starting a run.
  --mcp-resources-json
             Connect --mcp-resources mode: print JSON.
  --mcp-prompts
             Connect mode: list daemon MCP prompts and exit without starting a run.
  --mcp-prompts-json
             Connect --mcp-prompts mode: print JSON.
  --mcp-read
             Connect mode: read one daemon MCP resource and exit without starting a run.
  --mcp-read-json
             Connect --mcp-read mode: print JSON.
  --mcp-prompt
             Connect mode: render one daemon MCP prompt and exit without starting a run.
  --mcp-prompt-json
             Connect --mcp-prompt mode: print JSON.
  --recall   Connect mode: search daemon recall history and exit without starting a run.
  --recall-json
             Connect --recall mode: print JSON. With no --recall flag, one positional query is accepted.
  --recall-memories
             Connect --recall mode: search only curated memories.
  --recall-limit
             Connect --recall mode: maximum hits; 0 uses daemon default.
  --memories Connect mode: list daemon memories and exit without starting a run.
  --memories-json
             Connect --memories mode: print JSON.
  --memory-limit
             Connect --memories/--inspect mode: maximum memories; 0 means no limit.
  --forget-memory
             Connect mode: delete one daemon memory by id and exit without starting a run.
  --forget-memory-json
             Connect --forget-memory mode: print JSON.
  --permissions
             Connect mode: list daemon permission grants and exit without starting a run.
  --permissions-json
             Connect --permissions mode: print JSON.
  --forget-permission
             Connect mode: delete one daemon permission grant by id and exit without starting a run.
  --forget-permission-json
             Connect --forget-permission mode: print JSON.
  --server   MCP server name for --mcp-read or --mcp-prompt.
  --uri      MCP resource URI for --mcp-read.
  --name     MCP prompt name for --mcp-prompt.
  --arg      Skill or MCP prompt argument key=value. Repeatable.
  --env      Load env vars from a .env file. Repeatable; shell env wins.
`)
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
