# glue-review

A free, local pre-push branch reviewer built on the Glue agent harness. Run
it before opening a PR and get structured review notes from a real LLM, no
cloud account required beyond a free API key.

## Use it as a GitHub Action

```yaml
# .github/workflows/glue-review.yml
on:
  pull_request:
    types: [opened, synchronize, reopened, ready_for_review]
permissions:
  contents: read
  pull-requests: write
jobs:
  review:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          ref: ${{ github.event.pull_request.head.sha }}
          fetch-depth: 0
      - uses: erain/glue/agents/glue-review@main  # pin to a tag in production
        with:
          nvidia-api-key: ${{ secrets.NVIDIA_API_KEY }}
```

That posts a sticky review comment on every PR. Use `provider: openrouter` or
`provider: gemini` (with the matching `*-api-key` input) to swap backends.

Inputs and outputs are documented in [`action.yml`](action.yml). Re-runs
update the existing sticky comment instead of stacking duplicates.

## What it does

Given a Git working directory, the agent:

1. Reads the diff of `HEAD` versus a base ref (default `main`).
2. Reads the commit history on the branch.
3. Reads specific files when the diff alone lacks context.
4. Emits a single Markdown review with sections for issues, suggestions,
   things that look good, and open questions.

The model decides which files in the diff warrant a deep read — that is the
whole point of running it through an agent loop instead of stuffing the
entire diff into one prompt.

## Quickstart

```sh
go install github.com/erain/glue/agents/glue-review@latest

# Default: NVIDIA build + moonshotai/kimi-k2.6 (the strongest free model).
export NVIDIA_API_KEY=nvapi-...
glue-review

# Pick a different base ref:
glue-review --base origin/main

# Use OpenRouter free tier instead:
export OPENROUTER_API_KEY=sk-or-v1-...
glue-review --provider openrouter

# Use Gemini:
export GEMINI_API_KEY=...
glue-review --provider gemini --model gemini-2.5-flash
```

## Flags

| Flag | Default | Notes |
|---|---|---|
| `--base` | `main` | Base ref to diff against. |
| `--provider` | `nvidia` | One of `nvidia`, `openrouter`, `gemini`. |
| `--model` | provider-specific | NVIDIA: `moonshotai/kimi-k2.6`; OpenRouter: `inclusionai/ling-2.6-1t:free`; Gemini: `gemini-2.5-flash`. |
| `--id` | `glue-review` | Session id. Sessions are file-backed under `--store`. |
| `--store` | `.glue/review-sessions` | Where the file-backed session store lives. |
| `--work` | `.` | Working directory; must be inside the Git repo. |
| `--max-turns` | `16` | Loop budget (cap on assistant turns). |
| `--prompt` | (default review prompt) | Override the user message sent to the agent. |

## Tools wired

| Name | Purpose |
|---|---|
| `git_diff_branch(base?, max_bytes?)` | Diff of `<base>...HEAD`. |
| `git_log_branch(base?, limit?)`      | Commits on the branch with full messages. |
| `read_file(path, max_bytes?)`        | Read a file, capped, traversal-rejected. |

All output is capped so a runaway tool call cannot blow up the model's
context window. Paths are validated against `--work` to reject `..`
traversal.

## Why this is interesting

- **Daily-driver utility.** Every PR gets one; this is something you run.
- **Real agentic flow.** Not a single-shot prompt — the model picks files
  to read based on the diff.
- **Hackable.** ~300 LOC across two files; copy and adapt for your own
  reviewer flavor (security-focused, performance-focused, doc-focused).
- **Free.** Defaults to NVIDIA's free Kimi K2.6 (1T params); also works on
  OpenRouter free routes and Gemini Flash.

## Sample output

```
## Summary
This branch adds a new main.go containing only a stub that panics with "todo".

## Issues
[major] main.go:2 — The program panics immediately on startup, making it
unusable and failing any runtime tests or deployments.

## Suggestions
- Replace the panic stub with a valid entrypoint.
- Add tests and build checks before marking the feature complete.

## Open questions
- Is this intended as a temporary scaffold, or should main provide
  production behavior now?
```

## What it is not

- Not a replacement for human review. The model misses things; treat the
  output as a first pass and a sanity check.
- Not a CI bot. It is a local CLI; if you want CI integration, the Go code
  is ~300 LOC and easy to call from a workflow.
- Not opinionated about your repo. The system prompt is generic on purpose
  — if you want a specific style (e.g., "always check for SQL injection"),
  pass it via `--prompt`.
