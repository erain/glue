# Glue Project Plan

This plan is bootstrapping material. The pinned tracker issue is
<https://github.com/erain/glue/issues/1>. After the initial GitHub issues and
pinned tracker are created, GitHub issues become the source of truth for
execution order, status, and acceptance criteria.

## Operating Model

1. Start each work session by reading the pinned tracker issue.
2. Pick the next open issue by milestone priority: P0, then P1, then P2.
3. Implement exactly one issue per pull request.
4. Run the issue's verification commands.
5. Update docs when behavior, architecture, package layout, or status changes —
   in the same PR.
6. Open a PR whose title or body references the issue (`Closes #N`).
7. After the PR merges, comment on the issue with implementation notes and
   verification output, then close it (or let `Closes #N` close it).
8. Update the pinned tracker with completed work and the next recommended issue.

The detailed contributor protocol — including branching conventions, PR shape,
closing-comment format, and CI expectations — is in [`../CONTRIBUTING.md`](../CONTRIBUTING.md).

## Milestones

### P0: Foundation And Reusable Loop

P0 establishes the repository, documentation, source-of-truth issue workflow,
core types, reusable loop, public session wrapper, Gemini text streaming, and
README quickstart.

P0 issues:

- #2: Create initial design docs and project tracker.
- #3: Scaffold Go module and package boundaries.
- #22: Add CI workflow (build, vet, test) on PRs and main.
- #4: Define normalized message, event, tool, and provider types.
- #5: Implement reusable pi-mono-style agent loop.
- #6: Add deterministic sequential tool execution.
- #7: Wire public Agent and Session over the loop.
- #8: Implement Gemini text streaming provider.
- #9: Add README quickstart.

### P1: Persistence, Tools, Structured Output, And CLI

P1 makes the framework useful locally: function calling, file-backed sessions,
structured JSON output, skills, roles, CLI runner, and examples.

P1 issues:

- #10: Add Gemini function calling support.
- #11: Implement file-backed session store.
- #12: Add structured JSON result API.
- #13: Add AGENTS.md and skill loading.
- #14: Add role support.
- #15: Build local CLI runner.
- #16: Add example local CLI agent.

### P2: Hardening And Expansion

P2 improves runtime behavior, extensibility, and maintenance workflows.

P2 issues:

- #17: Add parallel tool execution option.
- #18: Add context compaction design and first implementation.
- #19: Add shell/filesystem tool design.
- #20: Add provider plugin guide.
- #21: Add GitHub issue automation workflow.

## Issue Requirements

Every issue must include:

- Goal
- Scope
- Out of scope
- Implementation notes
- Docs update required
- Verification commands
- Acceptance criteria

Each issue should be small enough for one focused implementation-and-verification
session.

## Documentation Requirements

- `docs/design.md` is the canonical architecture document.
- `docs/project-plan.md` tracks the roadmap shape and operating model.
- `docs/adr/` records durable architectural decisions.
- `CONTRIBUTING.md` records the per-issue contributor protocol.
- Documentation updates are part of the definition of done when work changes
  public API, package boundaries, architecture, or project status.

## Current Status

P0 is complete. The repository contains the design docs, project plan, ADR 0001,
CONTRIBUTING, a CI workflow, the Go module + package scaffold, normalized loop
types, the reusable agent loop with deterministic sequential tool execution,
the public `glue.Agent` / `glue.Session` API, the Gemini text streaming
provider, and a README quickstart. P1 starts with #10 (Gemini function calling)
and continues through file-backed sessions, structured JSON, skills, roles,
and the CLI runner. See the pinned tracker for the next recommended issue.
