# Issue Automation

The Glue project uses GitHub issues as its source of truth (see
[`CONTRIBUTING.md`](../CONTRIBUTING.md) and the active tracker
[`#110`](https://github.com/erain/glue/issues/110); the closed
[`#1`](https://github.com/erain/glue/issues/1) is the historical
bootstrap tracker). This document covers
the small amount of automation we ship to keep that source of truth
honest, plus the `gh` cheatsheet for the common operations.

## `scripts/check-tracker.sh` — drift detector

The contributor protocol's load-bearing invariant is:

> **No issue is closed without a merged PR that references it.**

`scripts/check-tracker.sh` audits that invariant by fetching every
closed issue and checking that GitHub reports at least one merged PR in
each issue's `closedByPullRequestsReferences`. Issues closed without a
merged PR — whether by mistake, by human override, or by a prior agent
that forgot to land code — are listed.

```sh
# Dry-run report (default):
scripts/check-tracker.sh

# Fail on any violation. Use this in CI once we want strict gating.
scripts/check-tracker.sh --strict

# Cap how many closed issues are scanned:
scripts/check-tracker.sh --limit 100
```

Output shape:

```
Closed issues scanned: 20
Violations (closed but no merged PR linked): 0
```

If there are violations, each is printed on its own line as
`#<number> <title>`.

The script is intentionally read-only. It does not reopen issues, post
comments, or modify labels — those steps are deliberate and should be
done with the operator's own judgement.

Requirements: `gh` (authenticated) and `jq`.

## When to run the script

- **Locally, before merging a PR** that closes an issue: catches typos
  in `Closes #N` references.
- **After bulk operations** like the bootstrap reset that opened this
  project — the original mess (issues closed without PRs) was exactly
  what this checker would have flagged.
- **As a CI cron** once we want strict enforcement. A weekly
  `scripts/check-tracker.sh --strict` job is the obvious shape; not
  wired up by default since it requires repo settings.

## `gh` cheatsheet

The recurring per-issue operations from `CONTRIBUTING.md`:

```sh
# Post the closing comment on an issue.
gh issue comment 17 --body "$(cat closing-comment.md)"

# Reopen an issue (e.g., the bootstrap reset).
gh issue reopen 17

# Edit the body of the active tracker after a PR merges. Reads from a
# file so the heredoc-formatted body survives shell quoting.
gh issue edit 110 --body "$(cat tracker.md)"

# Confirm linkage on an issue you just closed via Closes #N.
gh issue view 17 --json closedByPullRequestsReferences
```

For PRs:

```sh
# Open a PR with HEREDOC body.
gh pr create --title "Add foo (closes #N)" --body "$(cat <<'EOF'
## Summary
...
Closes #N.
EOF
)"

# Watch CI on a PR and exit when it finishes.
gh pr checks <num>

# Squash-merge and delete the branch.
gh pr merge <num> --squash --delete-branch
```

## Roadmap-from-docs sync (deferred)

The original issue body for #21 also mentioned "create/update roadmap
issues from docs". We considered shipping a YAML-driven script that
reads a roadmap file and creates missing issues, but the project's
actual workflow has been the opposite: humans write the issue, the
tracker links to it. Inverting that to be docs-first would add a step
without removing one. If a future milestone has many similar issues
(e.g., the FileRead/FileWrite/Exec implementation issues that ADR 0003
calls out), a thin wrapper around `gh issue create` is a one-evening
job; document it here when it exists.

## CI: live Gemini job

The CI workflow has two jobs:

- **`test`** (always runs): `go build` + `go vet` + `go test ./...` on every
  PR and push to `main`. This is the merge gate.
- **`live (gemini)`** (manual only, via `workflow_dispatch`): runs the gated
  live tests `go test ./providers/gemini -run Live -count=1` and
  `go test ./examples/local-agent -run Live -count=1` against the real
  Gemini API. Reads the API key from the `GEMINI_API_KEY` repo secret.

The live job does not run on push or PR — it would burn API tokens on
every commit for a check that is not a merge gate. Trigger it manually
when you want CI-side confirmation:

```sh
gh workflow run ci.yml --ref <branch>
```

or use the "Run workflow" button on the Actions tab. For routine
verification, run the same `go test -run Live` commands locally with
`GEMINI_API_KEY` exported.

Rotating the key:

```sh
gh secret set GEMINI_API_KEY --repo erain/glue
```

## Live OpenRouter model

The `live (openrouter)` CI job (and the `openrouter` provider's
`DefaultModel`) point at the meta-route **`openrouter/free`**.
OpenRouter's meta-route auto-selects from the currently-available
free models on every call — non-deterministic by design, but
resilient to free-tier churn.

We learned this the painful way: pinning a specific free model led
to repeated CI breakage every few weeks as upstreams went paid or
404'd (see #115, May 2026 / `inclusionai/ring-2.6-1t:free` — second
time the same kind of break in a quarter). The meta-route trades
determinism for the right behavior in CI.

**If the meta-route itself ever breaks** (it never has, but in
principle): browse [openrouter.ai/models?max_price=0](https://openrouter.ai/models?max_price=0)
and pick a pinned free model with a consistently-available upstream
(InclusionAI's Ling family historically holds up; NVIDIA Nemotron
free routes are also stable but over-subscribed). Update three
places:

- `providers/openrouter/openrouter.go` — the `DefaultModel` constant.
- `.github/workflows/ci.yml` — the `OPENROUTER_LIVE_MODEL` env on
  the `live (openrouter)` job.
- The test fallback in `providers/openrouter/openrouter_test.go`
  uses `DefaultModel`, so it picks up the constant automatically.

Verify locally:

```sh
OPENROUTER_API_KEY=sk-or-v1-... go test ./providers/openrouter -run Live -count=1
```

The provider-level smoke is the canonical check. The
`agents/glue-review` fixture replay is a separate concern.

## Why so little automation

Glue's discipline is "one issue per PR" with a human-readable closing
comment. The closing comment is the load-bearing artifact — it carries
verification output and notes for the next session. Automating
comment generation would erode the value of the comment by making it
boilerplate. The drift detector is the right amount of automation: it
catches operator mistakes without taking over the operator's job.
