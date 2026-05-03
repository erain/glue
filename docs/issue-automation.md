# Issue Automation

The Glue project uses GitHub issues as its source of truth (see
[`CONTRIBUTING.md`](../CONTRIBUTING.md) and the pinned tracker
[`#1`](https://github.com/erain/glue/issues/1)). This document covers
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

# Edit the body of the pinned tracker after a PR merges. Reads from a
# file so the heredoc-formatted body survives shell quoting.
gh issue edit 1 --body "$(cat tracker.md)"

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

## Why so little automation

Glue's discipline is "one issue per PR" with a human-readable closing
comment. The closing comment is the load-bearing artifact — it carries
verification output and notes for the next session. Automating
comment generation would erode the value of the comment by making it
boilerplate. The drift detector is the right amount of automation: it
catches operator mistakes without taking over the operator's job.
