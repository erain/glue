# ADR-0018: Framework, Coding Agent, and AX Execution Direction

## Status

Accepted direction, 2026-11-07. Remote implementation design is pending.
Supersedes ADR-0012's Peggy product commitment and ADR-0005's Peggy-led
expansion as active roadmap drivers; their reusable framework boundaries stand.

## Context

The Peggy tracker accumulated both personal-assistant and coding-agent work.
The owner has retired Peggy and chosen to focus on the reusable agent framework
and coding agent, with local execution and future remote task execution via AX.
Other coding agents are developing framework features concurrently.

## Decision

- Keep the Go framework and `cmd/glue` coding agent as the product surfaces.
- Retire Peggy's CLI, Telegram, dashboard, scheduling, and curated assistant
  memory product. Cancel unfinished work and remove its source separately.
  Preserve shared providers, tools, stores, daemon, permissions, and goal APIs.
- Call execution choices local and remote **modes**, independently of LLM
  provider/model choice. Existing local execution remains the default.
- Target AX for remote orchestration. Run a complete Glue coding job inside
  its remote workspace; a remote shell executor alone is insufficient.
- First produce the inventory, AX probe, job contract, decisions, and detailed
  roadmap in [project-plan.md](../project-plan.md). No remote API or CLI syntax
  is approved by this direction decision.
- Preserve active framework work and use isolated worktrees. All PRs remain
  drafts until the human decides otherwise.

## Consequences

Glue will not maintain a personal-assistant product or recreate AX's cluster
scheduler/sandbox lifecycle. Removing Peggy does not require removing reusable
capabilities developed during that work. Old ADRs and release notes remain a
historical record, with current entry points referring to this direction.

Remote planning must establish workspace transfer, credentials, noninteractive
permissions, durable job identity, cancellation, completion, artifact retrieval,
and safe local review. AX task readiness alone is insufficient evidence of a
successful coding job; verify the pinned runner's completion behavior.

No remote integration is shipped by this ADR. The first implementation roadmap
must require live submit/disconnect/reattach/result evidence, including failure,
and preserve local operation without AX dependencies at runtime.
