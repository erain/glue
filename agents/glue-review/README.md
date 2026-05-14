# glue-review

A free, local code-review agent that posts **one** GitHub comment per PR — written for the AI coding agent that will paste the fix, not for a human skimming sections.

Built on the [glue](https://github.com/erain/glue) agent harness as the framework's reference agent.

## What you get on every PR

A single sticky comment with:

- A one-line headline.
- ≤ 5 severity bullets (`critical` / `high` / `medium` / `low` / `nit`) pointing at `file:line`.
- One fenced ` ```markdown ` fix-instruction block with verb-first directives and an `Acceptance:` line per item, ready to paste into Claude Code / Cursor / Codex / Aider / Cline / Gemini CLI / whatever your coding agent of choice is.

````markdown
## glue-review

SQL injection in /links/{short_id}/stats — short_id is f-stringed into raw SQL.

- **critical** — app/routes.py:88 — short_id is interpolated into a raw SQL WHERE clause; `' OR '1'='1` leaks every non-deleted row.
- **medium** — app/routes.py:46–57 — fresh in-memory SQLite DB on every call; O(n) per invocation for a simple LIKE filter.

---

### Fix instructions — paste into your coding agent

```markdown
Fix the following in this PR before merging.

1. **app/routes.py:88** — `/links/{short_id}/stats` interpolates a user-controlled path parameter directly into a SQL string.
   - Replace the f-string with a parameterised query: `db.execute(text("... WHERE short_id = :sid AND deleted_at IS NULL"), {"sid": short_id})`, or use the ORM `select(Link.hits, Link.created_at).where(...)` pattern from `_load_active_link`.
   Acceptance: `pytest tests/test_stats.py::test_stats_rejects_sql_metacharacters` passes.
```
````

That fix block is the product. A real coding agent reads it, applies the change, and closes the loop. Live example on a real PR: [erain/glue-review-eval#1](https://github.com/erain/glue-review-eval/pull/1).

When there are no real issues, the comment is just:

```
## glue-review

No concerns — LGTM.
```

When the *approach* is wrong (not the lines):

````markdown
## glue-review

**Pushback on approach** — <one-line summary>

<2–4 sentences on why and what to do instead>

---

### Fix instructions — paste into your coding agent

```markdown
Do NOT apply the current diff. Instead:
1. <high-level redirection step>
   Acceptance: <property the redesigned change must satisfy>
```
````

## Install as a GitHub Action

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
      - uses: erain/glue/agents/glue-review@v2     # pin v2 for the new comment format
        with:
          openrouter-api-key: ${{ secrets.OPENROUTER_API_KEY }}
          provider: openrouter
```

That's it. The `openrouter-api-key` is the cheapest path (free with sign-up at <https://openrouter.ai>); `provider: openrouter` selects the `inclusionai/ring-2.6-1t:free` model by default — what the eval below was scored against. Substitute `nvidia-api-key` / `provider: nvidia` for NVIDIA's free build.nvidia.com tier, or `gemini-api-key` / `provider: gemini` for Gemini 2.5 Flash.

Re-runs **update** the existing sticky comment instead of stacking duplicates. A transient upstream rate-limit on a re-run will *not* overwrite a previous good review.

### Pinning

| Tag | Format |
|---|---|
| `@v2` (floating, recommended) | v3 prompt — single comment + fenced markdown fix block. |
| `@v2.0.0` (strict) | First v3-default release. |
| `@v1` (floating, legacy) | v2 prompt — multi-section format with per-bullet `Fix:` inline-comment payload. |

The `v2` floating tag advances on every backwards-compatible release. See [CHANGELOG.md](CHANGELOG.md).

### Fork PRs

GitHub doesn't pass repo secrets to `pull_request` workflows from forks, so the workflow above won't review fork PRs. The standard fix is a **second workflow** triggered by an `issue_comment` that a maintainer types:

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
          ref: ${{ steps.pr.outputs.head_sha }}
          fetch-depth: 0
      - uses: erain/glue/agents/glue-review@v2
        with:
          base-ref: ${{ steps.pr.outputs.base_ref }}
          pr-number: ${{ github.event.issue.number }}
          openrouter-api-key: ${{ secrets.OPENROUTER_API_KEY }}
          provider: openrouter
```

`issue_comment` runs in the **base-repo context** with full secret access, fires only when a maintainer (`OWNER` / `MEMBER` / `COLLABORATOR`) types `/glue-review`, and pins the head SHA at trigger time so a fork pushing immediately after the trigger comment can't swap code into the run.

This repo runs both workflows. See [.github/workflows/glue-review.yml](../../.github/workflows/glue-review.yml) and [.github/workflows/glue-review-comment.yml](../../.github/workflows/glue-review-comment.yml).

## Evidence

The current default prompt (`v3`) is a deliberate iteration on top of v1/v2, measured against a 28-case eval suite at [erain/glue-review-eval](https://github.com/erain/glue-review-eval) covering Go / Python / TypeScript host projects with planted security bugs, logic bugs, missing-test PRs, doc drift, rejected-direction refactors, multi-bug PRs, and clean PRs.

|                     | v2 baseline | v3 (current) | delta       |
|---------------------|------------:|-------------:|------------:|
| `has_fix_block`     |        0.11 |         0.82 | **+70.4pp** |
| `no_false_positives`|        0.96 |         1.00 |     +3.7pp  |
| `flagged_file`      |        0.93 |         0.85 |     −7.4pp  |
| `flagged_concept`   |        0.90 |         0.86 |     −4.0pp  |

`has_fix_block` is the product-shaping metric: it asks whether the comment carries a fenced markdown block downstream coding agents can paste. The recall regressions are the cost of the tighter format — model spends some of its turn budget on the structural rubric — and they're modest.

**Downstream-fix success** (the closer-to-truth product KPI): we run `codex exec` against the v3 fix block in a tmpdir and check whether the planted-bug acceptance test transitions red → green. 6 / 8 = **75%** on cases that have a fix block and where the executor doesn't error mid-run.

Repro: see the eval repo's [`results/FINAL_REPORT.md`](https://github.com/erain/glue-review-eval/blob/main/results/FINAL_REPORT.md).

## CLI usage

```sh
go install github.com/erain/glue/agents/glue-review@v2

# OpenRouter free tier (the default mode the eval polishes toward):
export OPENROUTER_API_KEY=sk-or-v1-...
glue-review --provider openrouter

# NVIDIA's free build.nvidia.com tier:
export NVIDIA_API_KEY=nvapi-...
glue-review --provider nvidia

# Gemini:
export GEMINI_API_KEY=...
glue-review --provider gemini --model gemini-2.5-flash

# Pick a base ref other than `main`:
glue-review --base origin/release
```

## Flags

| Flag | Default | Notes |
|---|---|---|
| `--base` | `main` | Base ref to diff against. |
| `--provider` | `nvidia` | Provider name, or comma-separated failover chain (`openrouter,nvidia,gemini`). First with an API key in env AND a successful call wins. |
| `--model` | provider-specific | NVIDIA: `moonshotai/kimi-k2.6`; OpenRouter: `inclusionai/ring-2.6-1t:free`; Gemini: `gemini-2.5-flash`. |
| `--prompt-version` | `v3` | System prompt to load. `v1` and `v2` remain embedded for opt-back. |
| `--max-turns` | `16` | Loop budget. |
| `--paths` / `--paths-ignore` | (none) | Git pathspec globs to include / exclude. |
| `--prompt` | (default) | Override the user message (one-off focused runs: "only check for SQL injection"). |
| `--id` | `glue-review` | Session id. File-backed under `--store`. |
| `--store` | `.glue/review-sessions` | Where the session store lives. |
| `--work` | `.` | Working directory (must be inside a Git repo). |

## Tools wired

| Name | Purpose |
|---|---|
| `git_diff_branch(base?, max_bytes?)` | Diff of `<base>...HEAD`. |
| `git_log_branch(base?, limit?)` | Commits on the branch with full messages. |
| `read_file(path, max_bytes?)` | Read a file, capped, traversal-rejected, secret-shaped paths blocked. |

All output is capped so a runaway tool call cannot blow up the model's context window. Paths are validated against `--work` to reject `..` traversal. The [`blocklist`](blocklist.go) rejects secret-shaped paths (`.env*`, `id_rsa*`, `*.pem`, `*.key`, `credentials.json`, `service-account*.json`, `secret.*`, `.aws/.gcloud/.azure`) — extendable via `--blocked-paths` / `extra-blocked-paths`.

## Path filters

Restrict the diff the agent sees with Git pathspec globs:

- CLI: `--paths "src/**,*.go" --paths-ignore "vendor/**,*.gen.go"`
- Action inputs: `paths` / `paths-ignore` (comma-separated).

When `paths` is empty every changed file is in scope. `paths-ignore` applies after `paths`. Excludes-only is fine — the agent injects an implicit `*` include so "everything except testdata" works.

## Citation validation

After the model emits its review, glue-review re-fetches the diff and validates each parsed `[severity] path:line` citation against the new side of the diff. Entries pointing at files not in the diff, or at line numbers outside any added/context hunk, are dropped and logged to stderr; they never reach an inline annotation. (v3 emits a single sticky comment so this codepath is mostly a safety net; v1/v2 used it for the inline-comments flow.)

## Prompt versioning

The system prompt lives at [`prompts/v3.md`](prompts/v3.md), embedded into the binary via `//go:embed`. Each version is a separate file (`prompts/vN.md`); the default version is set in [`prompt.go`](prompt.go). The bot-identity tag in the sticky comment marker is independent of the prompt version — it changes only when a prompt-shape revision would mangle an in-place edit.

To pin a specific shape:

```yaml
- uses: erain/glue/agents/glue-review@v2
  with:
    prompt-version: v2  # multi-section + per-bullet Fix:
    openrouter-api-key: ${{ secrets.OPENROUTER_API_KEY }}
    provider: openrouter
```

## Building blocks

The agent is ~300 LOC of Go on top of the [`glue`](https://github.com/erain/glue) framework. If you want a security-focused / performance-focused / docs-focused variant, fork [`main.go`](main.go), swap the embedded prompt, and ship it as your own action. See the parent repo's [docs/design.md](../../docs/design.md) for the framework's surface.

## What it is not

- Not a replacement for human review. The model misses things; treat the comment as a first pass and a sanity check.
- Not opinionated about your style guide. If you want a specific lens ("always check for SQL injection", "always check for missing tests"), pass it via `--prompt` / the `prompt:` Action input.
- Not a CI gate. It posts a comment; merge decisions remain yours. (Setting `exit-code` outputs `1` on internal failure, but a v3 comment with severity bullets still exits `0` — it's commentary, not a verdict.)
