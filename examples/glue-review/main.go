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
	"context"
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

// systemPrompt frames the agent as a senior code reviewer. It tells the
// model how to use the tools — start with the diff and log, then read
// files only when context demands it — and what shape the final review
// should take. Keep this prompt short: the model already understands the
// review task, the lift here is to constrain the output format.
const systemPrompt = `You are a senior software engineer reviewing a Git branch before it is pushed for review.

Workflow:
1. Call git_diff_branch first to see the full diff against the base branch.
2. Call git_log_branch to see the commit history; this often explains intent.
3. For files where the diff alone is not enough context (large refactors, new files, subtle invariants), call read_file on specific paths to look at the surrounding code. Skim purposefully — do not read every file.
4. Emit a single, final review. Do not chat between tool calls.

Output format (Markdown, in this order, omit empty sections):

## Summary
One sentence on what this branch does.

## Issues
Bugs, regressions, or correctness problems. Include file:line references when possible. Prefix each with severity: [critical] [major] [minor].

## Suggestions
Style, design, or maintainability improvements. Be specific; cite file paths.

## Looks good
Things the change got right that are worth calling out (only when meaningful — do not pad).

## Open questions
Things you cannot decide from the diff alone and want the author to clarify.

Be direct. Cite file paths. Never invent code that is not in the diff.`

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

type config struct {
	base     string
	provider string
	model    string
	id       string
	store    string
	work     string
	maxTurns int
	prompt   string
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

	provider, defaultModel, err := newProvider(cfg.provider)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	model := cfg.model
	if model == "" {
		model = defaultModel
	}

	agent := glue.NewAgent(glue.AgentOptions{
		Provider:     provider,
		Model:        model,
		Tools:        reviewTools(cfg.work),
		SystemPrompt: systemPrompt,
		Store:        filestore.New(cfg.store),
		WorkDir:      cfg.work,
		MaxTurns:     cfg.maxTurns,
	})
	session, err := agent.Session(ctx, cfg.id)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	wrote := false
	_, err = session.Prompt(ctx, cfg.prompt,
		glue.WithEvents(func(e glue.Event) {
			switch e.Type {
			case glue.EventTextDelta:
				if e.Delta != "" {
					fmt.Fprint(stdout, e.Delta)
					wrote = true
				}
			case glue.EventToolStart:
				if e.ToolName != "" {
					fmt.Fprintf(stderr, "[tool] %s\n", e.ToolName)
				}
			}
		}),
	)
	if wrote {
		fmt.Fprintln(stdout)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func parseFlags(args []string, stderr io.Writer) (config, error) {
	flags := flag.NewFlagSet("glue-review", flag.ContinueOnError)
	flags.SetOutput(stderr)

	base := flags.String("base", "main", "base branch / ref to diff against")
	provider := flags.String("provider", "nvidia", "provider: nvidia, openrouter, or gemini")
	model := flags.String("model", "", "model id (defaults vary by provider)")
	id := flags.String("id", "glue-review", "session id (file-backed sessions key off this)")
	store := flags.String("store", ".glue/review-sessions", "session store directory")
	work := flags.String("work", ".", "working directory (must be inside the Git repo)")
	maxTurns := flags.Int("max-turns", 16, "loop budget — caps total assistant turns")
	prompt := flags.String("prompt", "", "override the default review prompt")

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
		base:     *base,
		provider: strings.ToLower(strings.TrimSpace(*provider)),
		model:    *model,
		id:       *id,
		store:    *store,
		work:     *work,
		maxTurns: *maxTurns,
		prompt:   *prompt,
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
