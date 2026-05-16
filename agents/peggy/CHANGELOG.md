# Changelog — agents/peggy

All notable changes to the `peggy` agent. Format roughly follows
[Keep a Changelog](https://keepachangelog.com); this project does
not formally follow SemVer until v1.0.

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
canonical roadmap. M2 ("she can code") is the next milestone.
