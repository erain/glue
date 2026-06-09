# Glue Project Plan

This document records the roadmap shape and operating model. GitHub
issues are the source of truth for execution order, status, and
acceptance criteria.

- The original `0.x` framework roadmap was tracked in the now-closed
  pinned issue [#1](https://github.com/erain/glue/issues/1); it remains
  the historical record for the P0/P1/P2 milestones below.
- Active execution now runs under the
  [#110](https://github.com/erain/glue/issues/110) tracker — the Peggy
  milestone, which has broadened into the **M7 dual-track** effort
  (Glue's binary as a coding agent + Peggy as a long-running assistant).

## Operating Model

1. Start each work session by reading the active tracker issue
   ([#110](https://github.com/erain/glue/issues/110)).
2. Pick the next open issue from the tracker's work queue by priority.
3. Implement exactly one issue per pull request.
4. Run the issue's verification commands.
5. Update docs when behavior, architecture, package layout, or status changes —
   in the same PR.
6. Open a PR whose title or body references the issue (`Closes #N`).
7. After the PR merges, comment on the issue with implementation notes and
   verification output, then close it (or let `Closes #N` close it).
8. Update the active tracker with completed work and the next recommended issue.

The detailed contributor protocol — including branching conventions, PR shape,
closing-comment format, and CI expectations — is in [`../CONTRIBUTING.md`](../CONTRIBUTING.md).

## Milestones (bootstrap history)

P0–P2 below are the completed `0.x` bootstrap milestones, preserved as
the historical record. Work since then is summarized in
[Current Status](#current-status).

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
- `docs/building-agents.md` is the end-to-end guide for building an
  agent on Glue (the primary entry point for new framework users).
- `docs/project-plan.md` tracks the roadmap shape and operating model.
- `docs/adr/` records durable architectural decisions.
- `CONTRIBUTING.md` records the per-issue contributor protocol.
- Documentation updates are part of the definition of done when work changes
  public API, package boundaries, architecture, or project status.

## Current Status

The project now pursues two goals together: **Glue as an agent framework
whose binary is a capable coding agent**, and **Peggy as a long-running,
multi-channel personal assistant built on Glue**. Both are tracked under
[#110](https://github.com/erain/glue/issues/110) (the M7 dual-track
milestone).

### Foundation (shipped)

- **Bootstrap (P0–P2).** Normalized loop types and the reusable agent
  loop; public `glue.Agent` / `glue.Session` API; sequential and opt-in
  parallel tool execution; `StopReasonMaxTurns`; structured JSON output
  (`PromptJSON` / `WithJSONSchema`); Markdown skills, roles, and
  `AGENTS.md` discovery; opt-in `Compactor`
  ([ADR 0002](adr/0002-context-compaction.md)); the `cmd/glue` CLI; and
  the `examples/local-agent` tutorial.
- **Providers & ergonomics.** Four providers — `gemini`, `codex`
  (ChatGPT subscription, [ADR 0006](adr/0006-codex-provider.md)),
  `nvidia`, `openrouter` (latter two over the shared
  `providers/openaicompat` core) — a driver-style registry plus
  `glue.WithFailover`; the typed `glue.NewTool[T]` helper;
  `glue.SubagentTool` for delegation; `WithStreamWriter` /
  `WithToolLogger`; the `prompts` versioned-prompt catalog; and the
  `cli` standard-flags helper.
- **Long-running foundation (ADR 0005).** The pivot from "narrow library
  for short-lived agents" to "foundation for long-running multi-channel
  agents" is recorded in
  [`adr/0005-foundation-expansion.md`](adr/0005-foundation-expansion.md)
  (design.md non-goals updated there): the
  Executor / Permission / Hook trio
  ([ADR 0009](adr/0009-executor-permission-hook.md)), the summarizing
  compactor + FTS5 `stores/sqlite` search
  ([ADR 0007](adr/0007-memory-layer.md)), the local daemon protocol
  ([ADR 0010](adr/0010-daemon-protocol.md)), the channel-adapter pattern
  ([ADR 0008](adr/0008-channel-adapter.md)), and the MCP client in
  `tools/mcp` ([ADR 0011](adr/0011-mcp-client-integration.md)).

### M7 dual-track (in progress)

- **Track A — Glue coding-agent binary.** `tools/coding` assembles the
  reusable local coding bundle (`read_file`, `write_file`, `edit_file`,
  `list_dir`, `find_files`, `grep`, `shell_exec`, git helpers) over
  `tools/fs` / `tools/git` / `tools/shell` / `glue.Executor`;
  `cmd/glue run|serve --provider <name> --coding` runs it on any
  registered provider (codex for a ChatGPT-subscription coding agent).
  Boundary recorded in
  [ADR 0012](adr/0012-sdk-coding-agent-peggy-boundary.md). Since then
  the binary became a full interactive coding agent: the bubbletea TUI
  with streaming, tool cards, inline permission prompts, and slash
  commands ([ADR 0014](adr/0014-coding-agent-tui.md)); session
  fork/clone/tree ([ADR 0015](adr/0015-session-tree.md)); and the
  autonomous goal loop — `Agent.PursueGoal`, TUI `/goal` (durable,
  resumable, optionally worktree-isolated), and the headless
  `glue goal` subcommand for cron/CI
  ([ADR 0016](adr/0016-goal-loop.md), `v1.10.0`–`v1.12.0`). The coding
  agent ships as its own product face with a homepage
  (<https://glue-coding-agent-site.vercel.app>, repo
  [glue-coding-agent-site](https://github.com/erain/glue-coding-agent-site)).
  Next: **harness quality** — a source-verified analysis of pi, Cline,
  Codex CLI, and Gemini CLI distilled into
  [`coding-harness-roadmap.md`](coding-harness-roadmap.md) (edit-repair
  ladder, structured truncation, history hardening, retry/overflow
  recovery, compaction upgrades, Gemini loop polish), prioritized for
  Gemini 3.x first and open-weight OpenRouter/NVIDIA models second.
  Still planned beyond that: daemon goal endpoints, a sandboxed
  `Executor` backend (container/VM), and TUI-on-`glue connect`.
- **Track B — Peggy.** Peggy v0.1–v0.5 plus dogfood hardening (M1–M6)
  shipped: single-prompt CLI, Telegram channel, durable sqlite+FTS5
  memory with curated recall, opt-in coding tools, MCP servers, the
  shared HTTP+SSE daemon (`peggy serve` / `glue connect`), per-channel
  permission tiers, dashboard, doctor, and **scheduled/proactive runs**.
  Surfacing schedules over daemon/Telegram/dashboard, richer Telegram
  UX, and dashboard actions remain. See
  [`../agents/peggy/README.md`](../agents/peggy/README.md) and the
  tracker for the live work queue.

The reference agent [`agents/glue-review`](../agents/glue-review)
continues as a dogfood target. Most gaps once listed in
[`flue-gap-analysis.md`](flue-gap-analysis.md) — subagent orchestration
and MCP tooling — have shipped; multi-target deployment and additional
sandbox connectors remain open behind the ADR-0005 interfaces.
