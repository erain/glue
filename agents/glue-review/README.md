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

### Fork PRs (production setup)

GitHub does not pass repository secrets to `pull_request` workflows from
forks, for security reasons. The `pull_request`-triggered workflow above
will skip any fork PR — by design, since otherwise a fork could exfiltrate
the LLM API key just by opening a PR.

The standard fix is a **second workflow** triggered by an `issue_comment`
that a maintainer types on the PR:

```yaml
# .github/workflows/glue-review-comment.yml
on:
  issue_comment:
    types: [created]
permissions:
  contents: read
  pull-requests: write
jobs:
  review:
    if: |
      github.event.issue.pull_request &&
      contains(github.event.comment.body, '/glue-review') &&
      contains(fromJSON('["OWNER","MEMBER","COLLABORATOR"]'),
               github.event.comment.author_association)
    runs-on: ubuntu-latest
    steps:
      - id: pr
        env: { GH_TOKEN: ${{ github.token }} }
        run: |
          json=$(gh api "repos/${{ github.repository }}/pulls/${{ github.event.issue.number }}")
          echo "head_sha=$(echo "$json" | jq -r .head.sha)" >> "$GITHUB_OUTPUT"
          echo "base_ref=$(echo "$json" | jq -r .base.ref)" >> "$GITHUB_OUTPUT"
      - uses: actions/checkout@v4
        with:
          ref: ${{ steps.pr.outputs.head_sha }}  # pin SHA at trigger time
          fetch-depth: 0
      - uses: erain/glue/agents/glue-review@main
        with:
          base-ref: ${{ steps.pr.outputs.base_ref }}
          pr-number: ${{ github.event.issue.number }}
          nvidia-api-key: ${{ secrets.NVIDIA_API_KEY }}
```

`issue_comment` runs in the **base-repo context** with full secret access,
but only fires when a maintainer (`OWNER`/`MEMBER`/`COLLABORATOR`) comments
`/glue-review` on the PR. Untrusted commenters are filtered out by
`author_association`. The head SHA is resolved at trigger time and pinned
on checkout so a fork pushing immediately after the trigger comment cannot
swap code into the run.

This repo runs both workflows. See [.github/workflows/glue-review.yml](../../.github/workflows/glue-review.yml)
and [.github/workflows/glue-review-comment.yml](../../.github/workflows/glue-review-comment.yml).

## Sensitive-file blocklist

`read_file` refuses to open paths that match any of a built-in
secret-shaped pattern list — `.env*`, `id_rsa*`, `*.pem`, `*.key`,
`credentials.json`, `service-account*.json`, `secret.*`, `secrets.*`,
files inside `.aws`/`.gcloud`/`.azure`, and others (full list in
[blocklist.go](blocklist.go)). The match runs before the file is
opened, so blocked paths can never reach the model's context window
or the public review comment.

Extend the blocklist with repo-specific patterns:

- CLI: `--blocked-paths "*.token,internal/secrets/*"`
- Action input: `extra-blocked-paths: "*.token,internal/secrets/*"`

Patterns use Go's `filepath.Match` glob syntax (`*`, `?`, character
classes). They are matched against the relative path, the basename, and
each path component (case-insensitive), so `infra/secrets/foo.yaml` is
caught by the pattern `secrets`. You cannot subtract a default — only
add.

## Prompt versioning

The system prompt lives at [`prompts/v1.md`](prompts/v1.md), embedded
into the binary via `//go:embed`. Bump to a new file (`prompts/v2.md`,
etc.) when iterating on the review style; pass `--prompt-version v2` to
opt in. The default version is set in `prompt.go`.

The sticky comment / PR Review marker carries the prompt version
(`<!-- glue-review:promptv1:do-not-edit -->`) so a prompt-shape change
starts fresh comments instead of editing old ones into a different
format.

## Fixture replay tests

`fixture_test.go` defines small synthetic-repo scenarios (`panic-stub`,
`subtle-bug`, `cosmetic-only`). Running the test suite with
`OPENROUTER_API_KEY` (or `NVIDIA_API_KEY` / `GEMINI_API_KEY`) set
replays each scenario through a real free model and asserts structural
invariants — section presence, severity tags on the right files, no
fabricated paths. Add a new fixture when locking in a prompt behavior:

```go
{
    name: "your-scenario",
    seed: func(t *testing.T, repo string) { /* set up the repo */ },
    expect: func(t *testing.T, review string) { /* invariants */ },
}
```

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
