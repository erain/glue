// Command glue-review is a free, local pre-push branch reviewer built on
// the Glue agent harness. Point it at a Git repo and it walks the diff
// against a base branch (default `main`), reads the files it cares
// about, and emits structured review notes.
//
// Defaults to moonshotai/kimi-k2.6 via the NVIDIA provider — the strongest
// free model exposed through build.nvidia.com. Swap with --provider /
// --model to use OpenRouter or Gemini.
//
// Usage:
//
//	export NVIDIA_API_KEY=nvapi-...
//	glue-review                              # review current branch vs main
//	glue-review --base origin/main           # review vs a remote ref
//	glue-review --provider openrouter        # use OpenRouter instead
//	glue-review --provider gemini --model gemini-2.5-flash
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/erain/glue"
	"github.com/erain/glue/providers/gemini"
	"github.com/erain/glue/providers/nvidia"
	"github.com/erain/glue/providers/openrouter"
	filestore "github.com/erain/glue/stores/file"
)

// The system prompt is loaded from embedded prompts/<version>.md files
// at run time so we can A/B and roll back prompt revisions without a
// rebuild. See prompt.go for the loader.

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

type config struct {
	base          string
	provider      string
	model         string
	id            string
	store         string
	work          string
	maxTurns      int
	prompt        string
	inlineJSON    string
	promptVersion string
	blockedPaths  []string
	paths         []string
	pathsIgnore   []string
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cfg, err := parseFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, err)
		return 2
	}

	providers, err := resolveProviders(cfg.provider)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	// Try each configured provider in order. The first one whose key is
	// available AND whose Prompt() succeeds wins. We hold all streamed
	// text in a per-attempt buffer so a half-streamed failed attempt
	// doesn't pollute stdout.
	var (
		successCaptured bytes.Buffer
		attemptErrors   []string
		succeeded       bool
		usedProvider    string
	)
	for i, p := range providers {
		// Probe the env var the provider package reads. Without this, an
		// upstream missing-key error would only surface mid-stream and
		// we'd burn time on a doomed attempt.
		if !providerKeyAvailable(p.name) {
			fmt.Fprintf(stderr, "[failover] skip %s: %s not set in env\n", p.name, p.envName)
			continue
		}
		fmt.Fprintf(stderr, "[failover] attempt %d/%d: provider=%s\n", i+1, len(providers), p.name)

		var buf bytes.Buffer
		err := runOnce(ctx, cfg, p, &buf, stderr)
		if err == nil {
			succeeded = true
			successCaptured = buf
			usedProvider = p.name
			break
		}
		attemptErrors = append(attemptErrors, fmt.Sprintf("%s: %v", p.name, err))
		fmt.Fprintf(stderr, "[failover] %s failed: %v\n", p.name, err)
	}

	if !succeeded {
		if len(attemptErrors) == 0 {
			fmt.Fprintln(stderr, "no provider had its API key set; aborting")
		} else {
			fmt.Fprintln(stderr, "all providers failed:")
			for _, e := range attemptErrors {
				fmt.Fprintf(stderr, "  - %s\n", e)
			}
		}
		return 1
	}
	fmt.Fprintf(stderr, "[failover] succeeded with %s\n", usedProvider)

	// Stream the captured text through to stdout once.
	if successCaptured.Len() > 0 {
		fmt.Fprint(stdout, successCaptured.String())
		if !strings.HasSuffix(successCaptured.String(), "\n") {
			fmt.Fprintln(stdout)
		}
	}

	if cfg.inlineJSON != "" {
		entries := parseInlineComments(successCaptured.String())
		// Citation validation: re-fetch the diff once and drop any
		// inline comments whose path:line is not reachable on the new
		// side. Defends against the model fabricating plausibly-shaped
		// file:line citations — a real failure mode we observed with
		// Kimi K2.6 on PR #72. Validation is best-effort: a diff parse
		// error degrades to "skip validation" rather than failing the
		// whole run, so a flaky git invocation cannot red the workflow.
		if diff, dErr := fetchValidationDiff(ctx, cfg); dErr == nil {
			lineMap := parseDiffLineMap(diff)
			kept, dropped := validateInlineComments(entries, lineMap)
			if len(dropped) > 0 {
				fmt.Fprintf(stderr, "[validate] dropping %d/%d inline comments with unverifiable file:line citations\n",
					len(dropped), len(entries))
				fmt.Fprint(stderr, describeDropped(dropped, lineMap))
			}
			entries = kept
		} else {
			fmt.Fprintf(stderr, "[validate] skipped: %v\n", dErr)
		}
		raw, mErr := json.MarshalIndent(entries, "", "  ")
		if mErr != nil {
			fmt.Fprintf(stderr, "marshal inline JSON: %v\n", mErr)
			return 1
		}
		if wErr := os.WriteFile(cfg.inlineJSON, raw, 0o644); wErr != nil {
			fmt.Fprintf(stderr, "write inline JSON: %v\n", wErr)
			return 1
		}
	}
	return 0
}

// fetchValidationDiff re-runs the same `git diff` the agent saw, so
// the validation step can match inline comments against the same set
// of new-side line numbers the model was looking at. Honors --paths
// and --paths-ignore so out-of-scope files are excluded from
// validation as well as from the agent's input.
func fetchValidationDiff(ctx context.Context, cfg config) (string, error) {
	// cfg.base may already include `origin/` (CI passes it that way) or
	// be a bare branch name (local dev). Use it verbatim — Git resolves
	// either form against `HEAD`.
	gitArgs := []string{"diff", "--no-color", cfg.base + "...HEAD"}
	pathspec := buildPathspec(cfg.paths, cfg.pathsIgnore)
	if len(pathspec) > 0 {
		gitArgs = append(gitArgs, "--")
		gitArgs = append(gitArgs, pathspec...)
	}
	return runGit(ctx, cfg.work, gitArgs...)
}

// runOnce executes one Prompt against a specific provider, buffering
// streamed text into `into`. Returning a non-nil error tells the caller
// to drop the buffer and move on to the next provider.
func runOnce(ctx context.Context, cfg config, p providerEntry, into io.Writer, stderr io.Writer) error {
	prov, defaultModel, err := newProvider(p.name)
	if err != nil {
		return err
	}
	model := cfg.model
	if model == "" {
		model = defaultModel
	}

	systemPrompt, err := systemPromptFor(cfg.promptVersion)
	if err != nil {
		return err
	}

	agent := glue.NewAgent(glue.AgentOptions{
		Provider:     prov,
		Model:        model,
		Tools:        reviewTools(cfg.work, cfg.blockedPaths, cfg.paths, cfg.pathsIgnore),
		SystemPrompt: systemPrompt,
		Store:        filestore.New(cfg.store),
		WorkDir:      cfg.work,
		MaxTurns:     cfg.maxTurns,
	})
	// Per-attempt session id so a failed attempt's transcript does not
	// poison the next attempt's session.
	sessionID := fmt.Sprintf("%s-%s", cfg.id, p.name)
	session, err := agent.Session(ctx, sessionID)
	if err != nil {
		return err
	}

	_, err = session.Prompt(ctx, cfg.prompt,
		glue.WithEvents(func(e glue.Event) {
			switch e.Type {
			case glue.EventTextDelta:
				if e.Delta != "" {
					fmt.Fprint(into, e.Delta)
				}
			case glue.EventToolStart:
				if e.ToolName != "" {
					fmt.Fprintf(stderr, "[tool] %s\n", e.ToolName)
				}
			}
		}),
	)
	return err
}

func parseFlags(args []string, stderr io.Writer) (config, error) {
	flags := flag.NewFlagSet("glue-review", flag.ContinueOnError)
	flags.SetOutput(stderr)

	base := flags.String("base", "main", "base branch / ref to diff against")
	provider := flags.String("provider", "nvidia", "provider list (single name or comma-separated for failover, e.g. 'nvidia,openrouter,gemini'); first provider with its API key in env wins")
	model := flags.String("model", "", "model id (defaults vary by provider)")
	id := flags.String("id", "glue-review", "session id (file-backed sessions key off this)")
	store := flags.String("store", ".glue/review-sessions", "session store directory")
	work := flags.String("work", ".", "working directory (must be inside the Git repo)")
	maxTurns := flags.Int("max-turns", 16, "loop budget — caps total assistant turns")
	prompt := flags.String("prompt", "", "override the default review prompt")
	inlineJSON := flags.String("inline-json", "", "if set, write parsed inline-comment entries to this path as JSON (one array of {path,line,severity,body}); the final markdown still goes to stdout")
	promptVersion := flags.String("prompt-version", defaultPromptVersion, fmt.Sprintf("system-prompt version to load (available: %s)", strings.Join(availablePromptVersions(), ", ")))
	blockedPaths := flags.String("blocked-paths", "", "comma-separated extra path patterns the read_file tool will refuse to open (extends the built-in blocklist of secret-shaped files; cannot subtract defaults)")
	paths := flags.String("paths", "", "comma-separated Git pathspec globs; only files matching at least one pattern are reviewed. When empty, all changed files are in scope.")
	pathsIgnore := flags.String("paths-ignore", "", "comma-separated Git pathspec globs to exclude from review; applied after --paths")

	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: glue-review [flags]")
		fmt.Fprintln(stderr, "")
		fmt.Fprintln(stderr, "Reviews the current Git branch against a base branch using a free LLM.")
		fmt.Fprintln(stderr, "")
		fmt.Fprintln(stderr, "Flags:")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return config{}, err
	}

	cfg := config{
		base:          *base,
		provider:      strings.TrimSpace(*provider),
		model:         *model,
		id:            *id,
		store:         *store,
		work:          *work,
		maxTurns:      *maxTurns,
		prompt:        *prompt,
		inlineJSON:    *inlineJSON,
		promptVersion: strings.TrimSpace(*promptVersion),
		blockedPaths:  splitCommaList(*blockedPaths),
		paths:         splitCommaList(*paths),
		pathsIgnore:   splitCommaList(*pathsIgnore),
	}
	if cfg.prompt == "" {
		cfg.prompt = fmt.Sprintf("Review the current Git branch against base ref %q. Use the tools to gather context, then output the final review only.", cfg.base)
	}
	return cfg, nil
}

func newProvider(name string) (glue.Provider, string, error) {
	switch name {
	case "", "nvidia":
		return nvidia.New(nvidia.Options{}), "moonshotai/kimi-k2.6", nil
	case "openrouter":
		return openrouter.New(openrouter.Options{}), "inclusionai/ling-2.6-1t:free", nil
	case "gemini":
		return gemini.New(gemini.Options{}), "gemini-2.5-flash", nil
	default:
		return nil, "", fmt.Errorf("unknown provider %q (want nvidia, openrouter, or gemini)", name)
	}
}

// providerEntry pairs a provider name with the env var the provider
// package reads when no explicit APIKey is supplied. We probe the env
// var before constructing the provider so missing-key skips are
// instantaneous instead of mid-stream errors.
type providerEntry struct {
	name    string
	envName string
}

// resolveProviders parses the comma-separated --provider list into the
// ordered try list. Single-provider users (`--provider nvidia`) and
// failover users (`--provider nvidia,openrouter,gemini`) share the
// same flag — back-compat preserved.
func resolveProviders(raw string) ([]providerEntry, error) {
	if strings.TrimSpace(raw) == "" {
		raw = "nvidia"
	}
	parts := strings.Split(raw, ",")
	out := make([]providerEntry, 0, len(parts))
	for _, name := range parts {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		env, ok := envForProvider(name)
		if !ok {
			return nil, fmt.Errorf("unknown provider %q in --provider list", name)
		}
		out = append(out, providerEntry{name: name, envName: env})
	}
	if len(out) == 0 {
		return nil, errors.New("no providers configured")
	}
	return out, nil
}

func envForProvider(name string) (string, bool) {
	switch name {
	case "nvidia":
		return "NVIDIA_API_KEY", true
	case "openrouter":
		return "OPENROUTER_API_KEY", true
	case "gemini":
		return "GEMINI_API_KEY", true
	default:
		return "", false
	}
}

func providerKeyAvailable(name string) bool {
	env, ok := envForProvider(name)
	if !ok {
		return false
	}
	return strings.TrimSpace(os.Getenv(env)) != ""
}
