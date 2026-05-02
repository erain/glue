# Contributing to Glue

Glue is built one GitHub issue at a time. The pinned project tracker
(<https://github.com/erain/glue/issues/1>) is the source of truth for what to
work on next and what is done.

This document is the operating contract. The most important rule:

> **No issue is closed without a merged PR that references it.**

If you find a closed issue that has no PR linked to it, treat it as not done
and reopen it.

## Per-Issue Workflow

Every unit of work follows the same shape:

1. **Read the tracker.** Open issue #1 and pick the next unchecked issue in
   the priority order listed there. Work P0 before P1 before P2 unless the
   tracker says otherwise.
2. **Read the issue.** Verify it has all seven required sections (Goal, Scope,
   Out of scope, Implementation notes, Docs update required, Verification
   commands, Acceptance criteria). If anything is missing or stale, edit the
   issue first — do not start coding against an incomplete spec.
3. **Branch.** Create a feature branch named `issue/<number>-<short-slug>`
   (e.g. `issue/3-go-module-scaffold`). Branch from the latest `main`.
4. **Implement.** Stay inside the issue's scope. If you discover work that
   belongs to a different issue, file a follow-up issue rather than expanding
   the current PR.
5. **Update docs in the same PR.** When behavior, architecture, package
   layout, or project status changes, update `docs/design.md`,
   `docs/project-plan.md`, an ADR under `docs/adr/`, or `README.md`
   alongside the code change.
6. **Run verification locally.** Run every command listed in the issue's
   Verification Commands section. Capture the output — you will paste it into
   the closing comment.
7. **Open a PR.** Title format: `<short summary> (closes #<number>)`. PR body
   should include:
   - a one-paragraph summary of the change,
   - a `Closes #<number>` line so GitHub auto-closes the issue on merge,
   - the verification command output,
   - any follow-up issues you opened.
8. **CI must be green.** PRs cannot merge until the CI workflow (`go build`,
   `go vet`, `go test ./...`) passes.
9. **Merge.** Squash-merge into `main`. Delete the feature branch.
10. **Close the loop.** If the PR didn't auto-close the issue (because of a
    typo or because the issue was already closed), close it manually. Then post
    a summary comment on the issue containing:
    - the PR number,
    - the verification command output (or a faithful summary if it is large),
    - any caveats or follow-ups for the next session.
11. **Update the tracker.** Edit issue #1's body to:
    - check off the completed item,
    - update the Current Phase if a milestone advanced,
    - update the Next Recommended Issue,
    - move the closed item into the Completed Work section with a one-line
      summary linking the merged PR.

## Why The Closing Comment Matters

Glue is built to be picked up by a fresh agent or contributor with no prior
context. The closing comment on each issue, plus the tracker, plus the
contents of `docs/`, must be enough to fully reconstruct progress. Don't
treat the closing comment as bookkeeping — it's the handoff to the next
session.

A good closing comment includes:

- one or two sentences on what the PR shipped,
- the literal verification commands and their output,
- any non-obvious decisions or trade-offs made,
- pointers to follow-up issues you filed.

## Verification Discipline

- The verification commands listed in an issue are the minimum, not the maximum.
  If you found a tricky edge case while implementing, add a test for it.
- For loop and runtime work, prefer fake providers over live API calls.
- Live Gemini tests must be gated behind `GEMINI_API_KEY` and must not run in
  CI. Use a build tag, `t.Skip` based on the env var, or both.
- Never close an issue with red tests, broken builds, or a failing CI run.

## Scope Discipline

Glue's design doc lists explicit non-goals for P0 and P1 (no sandboxing, no
dynamic plugins, no MCP, no HTTP server, no auto-compaction, no parallel
tools). Don't quietly expand scope to add any of these. If you think one
should change, propose it as an ADR or a tracker update first, not as a PR.

## Reference Implementation

A prior implementation pass is preserved at `/home/ubuntu/src/glue-reference/`
on the development machine. It is allowed as a guide — the API shapes,
package layout, and test patterns there are useful — but it must not be
copy-pasted. Each issue still requires its own real review and verification.

## Tooling Requirements

- Go: latest stable.
- `gh` CLI: required for opening PRs, commenting on issues, and updating the
  tracker.
- The CI workflow (issue #22) is the canonical verification surface; mirror
  its commands locally before opening a PR.
