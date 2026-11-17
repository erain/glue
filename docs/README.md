# Glue Documentation

Start here to find the right guide for using the **Glue coding agent** (`cmd/glue`), building on the **Go agent framework** (`github.com/erain/glue`), or reading architecture and historical planning records.

## Coding Agent Guides (`cmd/glue`)

| Guide | What it covers |
| :--- | :--- |
| **[Remote Quickstart](remote-quickstart.md)** | Run your first remote job on AX in 5 minutes: `glue job init`, `--snapshot`, phone `ntfy` push, `glue remote setup`, and `glue job teleport`. |
| **[CLI, TUI & Local Goals](cli.md)** | Complete command map, interactive TUI slash commands & hotkeys, local `/goal` worktrees, model/thinking selection, coding tools, and harness reliability. |
| **[Remote Jobs Reference](jobs.md)** | `jobs.Spec`, `.glue/remote.yaml` repo config, `setup`/`verify`/delivery lifecycle, `--attempts`/`compare`/`pick`, `--issue`, and `continue --from-pr`. |
| **[Permission Policies](policies.md)** | `auto`, `workspace-auto`, `ask-all`, `yolo`, and custom YAML policy files (`--policy-file`). |
| **[Public Gateway Runbook](runbooks/public-gateway.md)** | Expose `glue-gateway` + `webui` over Cloudflare Tunnel and Cloudflare Access (`glue remote setup` or host `systemd`). |

## Go Agent Framework Guides (`github.com/erain/glue`)

| Guide | What it covers |
| :--- | :--- |
| **[Building Agents](building-agents.md)** | Step-by-step Go SDK tutorial: typed tools, SQLite FTS5 persistence, streaming, `AGENTS.md` skills, subagents, structured JSON, failover, and testing. |
| **[Adding a Provider](provider-guide.md)** | Implement `glue.Provider`, register model capabilities, and test streaming/tool round-trips. |
| **[Architecture & Internals](design.md)** | Core loop, session state, compaction, MCP client integration, daemon protocol, and job manager internals. |

## Architecture Decision Records & Roadmaps

- **[`adr/`](adr/)** — Numbered Architecture Decision Records (`ADR-0001` through `ADR-0021`), including:
  - [ADR-0014: Coding Agent TUI](adr/0014-coding-agent-tui.md) · [ADR-0015: Session Tree](adr/0015-session-tree.md) · [ADR-0016: Goal Loop](adr/0016-goal-loop.md)
  - [ADR-0017: Loop Retry & Overflow Recovery](adr/0017-loop-retry-overflow-recovery.md)
  - [ADR-0020: AX-First Direct Execution](adr/0020-ax-first-direct-execution.md) · [ADR-0021: Opt-In Cluster Gateway & Cloudflare Access](adr/0021-opt-in-cluster-gateway-cloudflare-access.md)
- **[Project Plan](project-plan.md)** — Product direction and execution boundaries (`#392`).
- **Historical Design & Showcase Records** (kept as engineering evidence):
  - [Demo: Use AX to Develop AX](demo-ax-develops-ax.md) · [Manual AX Runbook](runbooks/ax-manual-job.md)
  - [Remote Mode Roadmap](remote-mode-roadmap.md) · [Phase 3 UX Design](remote-mode-phase3-design.md) · [Phase 4 Anywhere UX Design](remote-mode-phase4-ux.md)
  - [Coding Harness Analysis](coding-harness-roadmap.md) · [Flue Gap Analysis](flue-gap-analysis.md) · [Issue Automation](issue-automation.md)
