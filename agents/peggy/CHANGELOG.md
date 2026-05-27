# Changelog — agents/peggy

All notable changes to the `peggy` agent. Format roughly follows
[Keep a Changelog](https://keepachangelog.com); this project does
not formally follow SemVer until v1.0.

## Unreleased

### Added

- **Scheduled / proactive runs.** `peggy serve` now runs a scheduler so
  Peggy initiates on her own — recurring (`--every`) or one-shot
  (`--at`) prompt or skill runs, managed with `peggy schedule
  add|remove` and `peggy schedules`. Schedules persist to
  `~/.peggy/schedules.json` (override `schedules.path`, `"off"` to
  disable), survive restarts, and fire-forward past missed ticks. Each
  schedule carries a permission tier for its unattended run — `trusted`
  (default) or `read_only`; `prompt` is rejected because there is no
  client to answer. Output is written to the schedule's session
  (visible via `peggy sessions`/recall). Daemon/Telegram/dashboard
  schedule surfacing is a planned follow-up.
- **Reusable coding execution seam.** Peggy now consumes Glue's
  `tools/coding` SDK bundle and exposes a runtime `CodingExecutor`
  injection point, so future VM or container-backed execution can plug
  in without changing Peggy's personal-assistant layer.
- **Dogfood doctor command.** `peggy doctor` now checks local Peggy
  dogfood readiness without starting a model run, reports required and
  recommended setup state in text or JSON, and exits non-zero when
  required prerequisites are missing.
- **First-run dogfood quickstart.** Peggy README now includes a
  fresh-setup path covering install, Codex auth, starter workspace,
  minimal dogfood settings, `peggy doctor`, daemon smoke checks,
  recall, and optional Telegram daemon wiring.
- **Persistent daemon permission grants.** `peggy serve` now persists
  remembered daemon permission grants to `permissions.remember_path`,
  and terminal daemon clients can list or revoke them with
  `glue connect --permissions` and `--forget-permission`.
- **Telegram daemon reply UX.** Telegram daemon-client runs now send
  an immediate progress message, split long final replies into multiple
  Telegram messages instead of truncating them, and expose `/help` for
  daemon chat commands.
- **Session history browser.** `peggy sessions` lists recent stored
  sessions without starting a provider, with prefix filtering and JSON
  output for scriptable local history inspection.
- **Daemon ops diagnostics.** Peggy daemon mode now advertises
  authenticated non-secret runtime diagnostics, and `glue connect
  --diagnose` distinguishes missing metadata, stale metadata, bad
  tokens, unreachable daemons, and healthy daemon state.
- **Memory backup and restore.** `peggy memories export` writes curated
  memories to a versioned JSON backup, and `peggy memories import`
  validates, dry-runs, and restores those memories while skipping
  existing duplicates by id or content.
- **Local dashboard.** `peggy dashboard` runs a localhost web control
  surface over existing daemon endpoints plus local session listing, so
  dogfood users can inspect health, tools, skills, roles, memories,
  recall, and recent sessions without remembering every CLI command.
- **Dashboard prompt runs.** The local dashboard can now start a prompt
  run through the existing daemon SSE endpoint and render the final
  assistant response without exposing the bearer token in the page.
- **Telegram daemon status command.** In daemon-client mode,
  allowlisted Telegram chats can use `/status` to check daemon health,
  active runs, tool count, and advertised capabilities without opening
  a terminal.

## v0.5.0 — 2026-05-25

Peggy now has a workflow/runtime surface for reusable personal-assistant
operations. The release completes the first M5 lane from tracker
[#110](https://github.com/erain/glue/issues/110): workspace roles,
file-backed skills, starter workspace scaffolding, priced usage
summaries, local and daemon memory controls, recall, and Telegram
daemon commands for roles, skills, and memory.

### Added

- **Daemon MCP catalogs.** `peggy serve` now exposes authenticated MCP
  resource and prompt catalog endpoints through the daemon host, and
  `glue connect --mcp-resources`, `--mcp-prompts`, plus JSON variants
  render those catalogs for remote clients. `glue connect --inspect`
  includes MCP catalog sections when the daemon advertises them.
- **Daemon MCP read/render actions.** Remote clients can read one MCP
  resource with `glue connect --mcp-read --server <name> --uri <uri>`
  or render one MCP prompt with
  `glue connect --mcp-prompt --server <name> --name <prompt> --arg key=value`;
  both modes have JSON variants.
- **Workspace file skills.** Peggy settings now support
  `context.work_dir` for loading Glue workspace context, including
  `.agents/skills/<name>/SKILL.md`. `peggy skills` lists discovered
  skills without constructing a provider. `peggy skill` runs one
  skill through a Peggy session with repeatable `--arg key=value`
  values, and library callers can use `Peggy.Skill`.
- **Daemon file skills.** `peggy serve` now exposes authenticated
  daemon skill catalogs and skill-mode runs. `glue connect --skills`
  lists discovered skills, `--skills-json` returns the catalog as
  data, `glue connect --skill <name> --arg key=value` runs a skill
  over the existing SSE stream, and `glue connect --inspect` includes
  skills when the daemon advertises them.
- **Workspace roles for Peggy.** `peggy roles` lists role files from
  `context.work_dir`, local `peggy --role <name>` and
  `peggy skill --role <name>` apply a workspace role to a run, and
  daemon clients can inspect roles with `glue connect --roles` /
  `--roles-json`. `glue connect --inspect` includes roles when the
  daemon advertises them.
- **Starter workspace scaffolding.** `peggy init --workdir <path>`
  creates a safe starter `AGENTS.md`, reviewer/operator roles, and
  triage/daily-plan/implementation-plan skills. Existing files are
  skipped unless `--force` is explicit.
- **Priced usage estimates.** `glue run --usage` and prompt-mode
  `glue connect --usage` can append `cost_usd=...` when users supply
  USD-per-1M-token price flags for input, output, and cache tokens.
- **Memory inspection.** `peggy memories` lists curated memories from
  the configured store without starting a provider, and `--json`
  exports the same list for scripts or backups.
- **Memory forgetting.** Curated memories now have stable IDs, and
  `peggy memories forget <id>` removes one memory from the dedicated
  `__memories__` session without touching ordinary conversation
  history.
- **Recall search command.** `peggy recall <query>` searches the
  configured SQLite store without starting a provider, with
  memories-only, limit, and JSON modes.
- **Daemon recall.** `peggy serve` now exposes authenticated recall
  search through the daemon host, and `glue connect --recall <query>`
  can search the live Peggy store with JSON, memories-only, and limit
  modes without starting a model run.
- **Daemon memory catalog.** `peggy serve` now exposes authenticated
  curated-memory listing through the daemon host, and
  `glue connect --memories` can render or JSON-export the live memory
  catalog with an optional limit.
- **Daemon memory deletion.** `glue connect --forget-memory <id>` now
  removes one curated memory from a Peggy daemon and can return the
  removed record as JSON.
- **Inspect memory panel.** `glue connect --inspect` now includes
  daemon-advertised memories, with `--memory-limit` to cap the section.
- **Telegram daemon memory commands.** In daemon-client mode,
  allowlisted Telegram chats can use `/memories`, `/recall`,
  `/recall_memories`, and `/forget_memory` to list, search, and delete
  curated daemon memory without starting a model run.
- **Telegram daemon skill commands.** In daemon-client mode,
  allowlisted Telegram chats can use `/skills` to inspect reusable
  workspace skills and `/skill <name> key=value` to run one through the
  shared daemon.
- **Telegram daemon role commands.** In daemon-client mode,
  allowlisted Telegram chats can use `/roles` to inspect workspace
  roles and `/role <name> <prompt>` to start a role-shaped daemon run.

### Changed

- `peggy.Version` is now `0.5.0`.
- Peggy README now frames v0.5 as the workflow/runtime release across
  local CLI, terminal daemon clients, and Telegram daemon clients.

## v0.4.0 — 2026-05-24

Peggy now has an ecosystem surface for external tools and operator
preflight. The release completes M4 from tracker
[#110](https://github.com/erain/glue/issues/110): Peggy can register
configured MCP servers over stdio or Streamable HTTP, expose MCP tools
through existing permission gates, inspect resource and prompt
catalogs, and give local/daemon clients status and catalog views
before a run starts.

### Added

- **MCP client integration.** `tools/mcp` implements the MCP
  2025-11-25 lifecycle, JSON-RPC request handling, stdio transport,
  Streamable HTTP transport, explicit env/header handling, and
  deterministic fake-server coverage.
- **MCP tools as Peggy tools.** Configured MCP tools are namespaced as
  `mcp_<server>_<tool>`, preserve input/output schema metadata, and
  use Peggy's existing `mcp_call` permission path.
- **MCP resource support.** `peggy mcp resources` lists resource
  metadata, `peggy mcp read` reads one resource URI, and
  resource-capable servers expose a permission-gated
  `mcp_<server>_read_resource` tool.
- **MCP prompt inspection.** `peggy mcp prompts` lists prompt
  templates and `peggy mcp prompt` renders one prompt with repeatable
  `--arg key=value` values.
- **Daemon/client inspection.** The daemon exposes authenticated
  status and tool-catalog endpoints; `glue connect --status`,
  `--tools`, and `--inspect` render those views from the terminal.
- **Usage summaries.** `glue run --usage` and prompt-mode
  `glue connect --usage` print provider-reported token usage on
  stderr without disturbing streamed stdout.
- **Local readiness status.** `peggy status` and `--json` summarize
  config, identity, provider/model, store, compaction, coding,
  permissions, channels, and MCP setup without starting providers or
  MCP servers.

### Changed

- `peggy.Version` is now `0.4.0`.
- Peggy README now documents the ecosystem preflight loop around
  `peggy status`, `peggy mcp ...`, `peggy serve`, and
  `glue connect --inspect`.
- Live OpenRouter fixture coverage was hardened for recent
  `openrouter/free` upstream flake modes while keeping real regressions
  visible.

### Known limitations

- MCP resource reads and prompt rendering are request/response only;
  subscriptions, prompt auto-use policy, OAuth, elicitation, sampling,
  and dynamic discovery remain deferred.
- `peggy status` is intentionally config-only. It does not prove that
  provider credentials or MCP endpoints are live.
- MCP subscriptions, prompt auto-use policy, OAuth, elicitation,
  sampling, and dynamic discovery remain deferred.

## v0.3.0 — 2026-05-24

Peggy can now run as one local daemon shared by terminal and Telegram
clients. The release completes M3 from tracker
[#110](https://github.com/erain/glue/issues/110): one long-running
Peggy process owns the provider, memory/session store, coding tools,
and permission cache while clients attach over the local HTTP+SSE
daemon protocol.

### Added

- **Local daemon mode.** `peggy serve` starts Peggy behind the
  ADR-0010 HTTP+SSE daemon protocol, writes local connection metadata,
  supports generated or explicit bearer tokens, and shuts down cleanly
  on SIGINT/SIGTERM.
- **Terminal daemon client.** `glue connect` attaches to `peggy serve`,
  starts runs, streams assistant text, brokers permission requests in
  the terminal, and cancels the run on SIGINT.
- **Telegram daemon-client mode.** `peggy-telegram --daemon` keeps the
  Telegram allowlist and inline permission buttons while using the
  shared Peggy daemon for provider, memory, coding tools, and
  permission cache.
- **Daemon-brokered permissions.** Side-effecting coding tools emit
  `permission_request` events and wait for the owning client to answer
  over the daemon protocol.
- **Channel-scoped permission tiers.** Peggy settings can assign
  `prompt`, `read_only`, or `trusted` behavior per channel/client,
  e.g. trusted local terminal with prompted or read-only Telegram.
- **Client-scoped remembered daemon decisions.** A remembered Telegram
  allow no longer silently authorizes terminal requests, or vice versa.

### Changed

- `peggy.Version` is now `0.3.0`.
- Peggy README, Telegram README, and the top-level README now document
  the multi-channel daemon happy path and permission-tier setup.
- Release smoke coverage now drives the daemon path with trusted CLI
  and read-only Telegram tiers using a fake provider.

### Known limitations

- The daemon is local-first and protected by a bearer token; it is not
  a hosted multi-user auth model.
- Runs are still stream-owned. Detached/background runs and replay
  endpoints are future work.
- Remembered permission decisions are in-memory and disappear when the
  daemon restarts. Persistent per-user policy remains a follow-up.
- Telegram replies are still one final message per turn; edit-in-place
  streaming is deferred.
- No MCP client yet; that remains M4.

## v0.2.0 — 2026-05-23

Peggy can now act as a permission-gated coding assistant in trusted
local workspaces. The release completes M2 from tracker
[#110](https://github.com/erain/glue/issues/110): she can read files,
write files, run allowlisted commands, inspect git branch context, and
ask before side effects from both CLI and Telegram.

### Added

- **Opt-in local coding mode.** `peggy --coding --workdir <repo>` and
  the `coding` block in `settings.json` register five tools:
  `read_file`, `write_file`, `shell_exec`, `git_diff_branch`, and
  `git_log_branch`.
- **Permission-gated side effects.** `write_file` and `shell_exec`
  require a `glue.Permission` decision. Read-only file and git tools
  do not prompt.
- **CLI permission prompter.** The single-prompt CLI asks on stderr /
  stdin with deny, allow-once, allow-for-session, and
  allow-for-target choices. Remembered decisions are process-local.
- **Telegram permission prompter.** `peggy-telegram` handles
  `callback_query` updates and confirms side-effecting coding tools
  with inline keyboard buttons in the same allowlisted chat that
  triggered the request.
- **Coding safety defaults.** `shell_exec` is argv-only, refuses
  pathful binaries, and is constrained by a basename allowlist.
  `write_file` is workspace-rooted, blocks sensitive path patterns,
  refuses symlink escapes, writes atomically, and refuses overwrites
  unless both host policy and model intent allow them.
- **Framework primitives for future sandboxes and orchestration.**
  `glue.Executor`, `glue.Permission`, `glue.Hook`, loop composition,
  `tools/shell.Exec`, `tools/fs.FileWrite`, and `glue.SubagentTool`
  landed behind Peggy's product surface.

### Changed

- `peggy.Version` is now `0.2.0`.
- Peggy README and Telegram README now document coding-mode setup,
  permission behavior, and the local-only trust boundary.
- Release smoke coverage now drives read/write/shell/git coding tools
  through Peggy with a fake provider and permission recorder.

### Known limitations

- Coding mode is a trusted-local workflow. v0.2 uses
  `glue.LocalExecutor`; stronger sandboxing remains deferred behind
  the `Executor` interface.
- Remembered permission choices are process-local. Persistent
  per-user / per-channel permission policy is a follow-up.
- Peggy is still one channel process at a time. The M3 daemon will let
  CLI, Telegram, and future clients share one long-running process.
- No MCP client yet; that remains M4.

## v0.1.0 — 2026-05-16

First public release. Peggy is a long-running personal-assistant
agent built on the [glue](../..) framework. She remembers across
sessions and is reachable from the CLI or Telegram. Tracker:
[#110](https://github.com/erain/glue/issues/110).

### Added

- **Single-prompt CLI** (`agents/peggy/cmd/peggy`). One-shot prompt
  with `--config`, `--soul`, `--session`, `--version` flags. Loads
  `settings.json` and `SOUL.md` from a documented resolution chain
  (`--flag` > env > XDG > `~/.config/peggy`).
- **Identity via Markdown.** `SOUL.md` content is embedded in the
  system prompt verbatim. Missing identity is non-fatal.
- **Model-callable memory.**
  - `remember(content, tags?)` persists curated facts to a dedicated
    `__memories__` session — immune to per-session compaction.
  - `recall(query, limit?, only_memories?)` searches prior history
    and curated memories via FTS5; returns a numbered hit list with
    session id, snippet, and BM25 score.
  - Memory tools are registered by default; disable with
    `Options.DisableMemoryTools`.
- **Cross-session FTS5 search** via `Agent.SearchSessions` /
  `Session.Search` (designed in
  [ADR-0007](../../docs/adr/0007-memory-layer.md), implemented in
  `stores/sqlite`).
- **Token-aware summarizing compaction** via the configured
  provider. `KeepRecentMessages` remains the cheap default; the new
  `SummarizingCompactor` takes over when sessions get long.
- **Codex provider** (`providers/codex`). Authenticates against the
  upstream Codex CLI's `auth.json` and routes through
  `chatgpt.com/backend-api/codex/responses`. Subscription auth, no
  per-token billing. Designed in
  [ADR-0006](../../docs/adr/0006-codex-provider.md).
- **Telegram channel adapter**
  (`agents/peggy/channels/telegram` + the `peggy-telegram` binary).
  Long-polling Bot API client, chat-id allowlist (refuse-all
  default), session-id namespacing (`telegram:<chat_id>`). First
  concrete `peggy.Channel` on the
  [ADR-0008](../../docs/adr/0008-channel-adapter.md) pattern.
- **SQLite session store** (`stores/sqlite`). Pure-Go via
  `modernc.org/sqlite`; FTS5 external-content schema with triggers;
  WAL mode. `stores/file` remains the simple option.
- **Documentation.** README quickstart, Telegram README,
  CHANGELOG (this file). Eight ADRs designed and accepted under
  this milestone (ADRs 0005–0008 plus contributions to ADRs
  0001–0004 retained from the original glue roadmap).

### Known limitations

- **No coding tools yet.** Shell exec, write-side filesystem, and
  the subagent primitive arrive in M2.
- **No always-on daemon yet.** v0.1 is one channel per process. The
  M3 daemon will let TUI / Telegram / future clients share one
  Peggy.
- **No MCP client yet.** Coming in M4.
- **No Anthropic provider yet.** Codex / Gemini / NVIDIA / OpenRouter
  ship today. Anthropic lands in M4 when budget allows.
- **Telegram streaming is one-message-per-turn**, not delta-edit.
  Telegram rate-limits message edits; a follow-up will revisit.
- **API-key OpenAI fallback** is additive but not in v0.1. Codex
  subscription auth is the only OpenAI path today.
- **`Agent.SearchSessions` does exact-match on `SessionID`.** A
  prefix-match extension ("all `telegram:*` sessions") is a planned
  follow-up.

### Architectural decisions captured

- [ADR-0005](../../docs/adr/0005-foundation-expansion.md) — foundation expansion for long-running agents (lifts shell exec, write-fs, MCP, HTTP server, automatic compaction trigger behind framework interfaces; defers sandboxing behind `Executor`).
- [ADR-0006](../../docs/adr/0006-codex-provider.md) — Codex provider auth + transport.
- [ADR-0007](../../docs/adr/0007-memory-layer.md) — memory layer (SummarizingCompactor + sqlite/FTS5 + Searcher).
- [ADR-0008](../../docs/adr/0008-channel-adapter.md) — channel adapter pattern.

### Notes for downstream consumers

- Library API at `github.com/erain/glue/agents/peggy` is stable for
  the v0.1.x series. Channel package APIs (e.g.
  `agents/peggy/channels/telegram`) are also stable but
  underspecified on purpose — additive changes are expected.
- `peggy.Version` is the source of truth for the version string. Bumped
  by hand at release time and pinned by a test.

[Tracker #110](https://github.com/erain/glue/issues/110) is the
canonical roadmap. M4 ("ecosystem") is the next milestone.
