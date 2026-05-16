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

P0, P1, and P2 are complete. In addition to the P0 foundation (design
docs, ADR 0001, CI workflow, normalized loop types, public `glue.Agent`
/ `glue.Session` API, Gemini text streaming, README quickstart), the
following has shipped:

- P1 — Gemini function calling, file-backed session store at
  `stores/file`, structured JSON output (`PromptJSON` /
  `WithJSONSchema`), Markdown skills and `AGENTS.md` discovery, roles
  with frontmatter and effective-model precedence, the `cmd/glue` CLI
  runner, and the `examples/local-agent` tutorial.
- P2 — opt-in parallel tool execution (`RunRequest.Parallel`), opt-in
  `Compactor` interface with the `KeepRecentMessages` policy
  ([ADR 0002](adr/0002-context-compaction.md)), the shell/filesystem
  tool extension packages `tools/fs` and `tools/git`
  ([ADR 0003](adr/0003-shell-filesystem-tools.md)), the provider
  plugin guide at [`provider-guide.md`](provider-guide.md), and the
  GitHub issue automation workflow at
  [`issue-automation.md`](issue-automation.md).
- Beyond the original plan — additional providers (`providers/nvidia`,
  `providers/openrouter`) sharing the `providers/openaicompat` core, a
  driver-style provider registry under `providers/` plus
  `glue.WithFailover`, `StopReasonMaxTurns`, the typed `glue.NewTool[T]`
  helper, `glue.WithStreamWriter` / `WithToolLogger`, the
  `glue/prompts` versioned-prompt catalog, the `glue/cli` standard
  flags helper, live CI smoke jobs gated on API keys, and a real
  downstream agent at [`agents/glue-review`](../agents/glue-review)
  (stable at `v1.1.0`).

The next focus is the **Peggy milestone** — a long-running, multi-channel,
memory-bearing personal-assistant agent built on glue. Tracker:
[#110](https://github.com/erain/glue/issues/110). The architectural
pivot from "narrow library for short-lived agents" to "foundation for
long-running multi-channel agents" is recorded in
[`adr/0005-foundation-expansion.md`](adr/0005-foundation-expansion.md);
the framework non-goals listed in [`design.md`](design.md) were updated
in the same ADR. Tracker [#110](https://github.com/erain/glue/issues/110)
is the source of truth for the next recommended issue under that
milestone; the original glue tracker (#1) remains closed and is the
historical record for the `0.x` roadmap.

Continuing dogfood of `agents/glue-review`, hand-coded-helper
migration, and the gaps surfaced in
[`flue-gap-analysis.md`](flue-gap-analysis.md) (multi-target
deployment, sandbox connectors, subagent orchestration, MCP tooling)
proceed under the Peggy milestone — most of those gaps are now in
scope behind interfaces (see ADR-0005).
