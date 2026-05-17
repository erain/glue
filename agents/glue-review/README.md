# glue-review

A free, local code-review agent that posts **one** GitHub comment per PR — written for the AI coding agent that will paste the fix, not for a human skimming sections.

Built on the [glue](https://github.com/erain/glue) agent harness as the framework's reference agent.

## What you get on every PR

A single sticky comment with:

- A one-line headline.
- ≤ 5 severity bullets (`critical` / `high` / `medium` / `low` / `nit`) pointing at `file:line`.
- One fenced ` ```markdown ` fix-instruction block with verb-first directives and an `Acceptance:` line per item — ready to paste into Claude Code / Cursor / Codex / Aider / Cline / Gemini CLI / OpenCode / your coding agent of choice.

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
  issues: write           # the sticky comment lives on the PR's issues thread
  pull-requests: write

jobs:
  review:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          ref: ${{ github.event.pull_request.head.sha }}
          fetch-depth: 0
      - uses: erain/glue/agents/glue-review@main
        with:
          openrouter-api-key: ${{ secrets.OPENROUTER_API_KEY }}
```

That's it. Grab a free [OpenRouter](https://openrouter.ai) key, add it as a repo secret, and every PR you open gets a review comment with a paste-ready fix block. Two minutes of setup.

Want a different provider? Pass `provider: nvidia` (with `nvidia-api-key`) for [build.nvidia.com](https://build.nvidia.com)'s free tier, or `provider: gemini` (with `gemini-api-key`) for Google's. Or chain them with comma-separation for free-tier reliability:

```yaml
- uses: erain/glue/agents/glue-review@main
  with:
    provider: openrouter,nvidia,gemini
    openrouter-api-key: ${{ secrets.OPENROUTER_API_KEY }}
    nvidia-api-key:     ${{ secrets.NVIDIA_API_KEY }}
    gemini-api-key:     ${{ secrets.GEMINI_API_KEY }}
```

The action tries each provider in order; the first one with a key in env *and* a successful call wins. Re-runs **update** the existing sticky comment instead of stacking duplicates. A transient upstream rate-limit on a re-run will *not* overwrite a previous good review.

### Fork PRs

GitHub doesn't pass repo secrets to `pull_request` workflows from forks, so the workflow above won't review fork PRs. The fix is a **second workflow** triggered by an `issue_comment` a maintainer types:

```yaml
# .github/workflows/glue-review-comment.yml
on:
  issue_comment:
    types: [created]

permissions:
  contents: read
  issues: write
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
      - uses: erain/glue/agents/glue-review@main
        with:
          base-ref: ${{ steps.pr.outputs.base_ref }}
          pr-number: ${{ github.event.issue.number }}
          openrouter-api-key: ${{ secrets.OPENROUTER_API_KEY }}
```

`issue_comment` runs in the **base-repo context** with full secret access, fires only when a maintainer (`OWNER` / `MEMBER` / `COLLABORATOR`) types `/glue-review`, and pins the head SHA at trigger time so a fork pushing immediately after the trigger comment can't swap code into the run.

This repo runs both workflows. See [.github/workflows/glue-review.yml](../../.github/workflows/glue-review.yml) and [.github/workflows/glue-review-comment.yml](../../.github/workflows/glue-review-comment.yml).

## Evidence

The current prompt is iterated against a 28-case planted-bug suite at [erain/glue-review-eval](https://github.com/erain/glue-review-eval) — Go / Python / TypeScript host projects with SQL injection, missing auth, off-by-one, missing tests, stale docs, rejected-direction refactors, multi-bug PRs, and clean PRs. Every case ships a YAML sidecar with the planted bug, expected findings, and a machine-checkable acceptance test. Eval runs go through `openrouter/free` (the meta-router) so the suite survives free-tier churn.

| signal              | initial prompt | current prompt | delta       |
|---------------------|---------------:|---------------:|------------:|
| `has_fix_block`     |           0.11 |           0.82 | **+70.4pp** |
| `no_false_positives`|           0.96 |           1.00 |     +3.7pp  |
| `flagged_file`      |           0.93 |           0.85 |     −7.4pp  |
| `flagged_concept`   |           0.90 |           0.86 |     −4.0pp  |

`has_fix_block` is the product-shaping metric: does the comment carry a fenced markdown block downstream coding agents can paste? It went from "essentially never" to **82%**. The recall regressions are the price of a tighter format.

**Downstream-fix success** (the closer-to-truth product KPI): we run `codex exec` against the fix block in a tmpdir and check whether the planted-bug acceptance test transitions red → green. **6 / 8 = 75%** on cases that have a fix block and where the executor doesn't error mid-run.

Repro: see the eval repo's [`results/FINAL_REPORT.md`](https://github.com/erain/glue-review-eval/blob/main/results/FINAL_REPORT.md).

## CLI usage

```sh
go install github.com/erain/glue/agents/glue-review@latest

# OpenRouter (default):
export OPENROUTER_API_KEY=sk-or-v1-...
glue-review

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
| `--provider` | `openrouter` | Provider name, or comma-separated failover chain (`openrouter,nvidia,gemini`). |
| `--model` | provider-specific | OpenRouter: `openrouter/free` (meta-router); NVIDIA: `moonshotai/kimi-k2.6`; Gemini: `gemini-2.5-flash`. |
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

## Building blocks

The agent is ~300 LOC of Go on top of the [`glue`](https://github.com/erain/glue) framework. The system prompt is one embedded file at [`prompts/default.md`](prompts/default.md). If you want a security-focused / performance-focused / docs-focused variant, fork [`main.go`](main.go), swap the embedded prompt, and ship it as your own action. See the parent repo's [docs/design.md](../../docs/design.md) for the framework's surface.

## What it is not

- Not a replacement for human review. The model misses things; treat the comment as a first pass and a sanity check.
- Not opinionated about your style guide. If you want a specific lens ("always check for SQL injection", "always check for missing tests"), pass it via `--prompt` / the `prompt:` Action input.
- Not a CI gate. It posts a comment; merge decisions remain yours.
