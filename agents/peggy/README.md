# peggy

A long-running personal-assistant agent built on the
[glue](../..) framework. v0.1 ships as a single-prompt CLI that
remembers you across invocations via SQLite + FTS5 session search and
a token-aware summarizing compactor. Telegram, REPL, and coding
skills land in later milestones (tracker
[#110](https://github.com/erain/glue/issues/110)).

## Quickstart

```sh
go install github.com/erain/glue/agents/peggy/cmd/peggy@latest

# One-time auth (subscription-mode default provider).
codex login

# Edit your identity and config (optional but recommended).
mkdir -p ~/.config/peggy
$EDITOR ~/.config/peggy/SOUL.md
$EDITOR ~/.config/peggy/settings.json

# Run a single prompt.
peggy "Hello — what should I be working on today?"
```

## SOUL.md

`SOUL.md` is the identity file. Its contents are embedded verbatim
into the agent's system prompt on every turn, so the model sees who
Peggy is and who you are. The loader doesn't enforce a structure;
the convention is:

```markdown
# Identity
You are Peggy, a personal assistant. Be concise; ask clarifying
questions only when the cost of guessing is high.

# About me
I'm Yu, an engineer. I write Go and TypeScript, run Linux, and
prefer terse responses with concrete file paths and line numbers.

# People
- "Alex" is my partner.
- "Inkblot" is my Australian Shepherd, 4 years old.

# Projects
- glue / Peggy — the framework + this agent.
- (other ongoing work)

# Preferences
- No emoji unless I ask for them.
- Prefer code diffs over prose when explaining changes.
- When summarizing meetings, lead with decisions and owners.
```

Missing `SOUL.md` is non-fatal — Peggy runs with no identity context
and emits a stderr diagnostic.

## settings.json

```json
{
  "provider": "codex",
  "model": "gpt-5-codex",
  "store": {
    "type": "sqlite",
    "path": "~/.peggy/peggy.db"
  },
  "compaction": {
    "threshold": 200,
    "target_tokens": 8000,
    "keep_recent": 8
  }
}
```

| Field | Default | Notes |
|---|---|---|
| `provider` | `codex` | One of `codex` / `gemini` / `openrouter` / `nvidia`. `codex` is the daily-driver default and uses your ChatGPT subscription via `codex login` (no env key). |
| `model` | provider-specific | Override the provider's default model. |
| `store.type` | `sqlite` | `sqlite` (FTS5 cross-session search) or `file` (one JSON per session). |
| `store.path` | `~/.peggy/peggy.db` | File for sqlite, directory for file. `~` and `$HOME` expand. |
| `compaction.threshold` | `200` | Message-count gate. Compaction only runs when the in-memory transcript exceeds this. |
| `compaction.target_tokens` | `8000` | Soft cap on transcript size before summarization. Word-count heuristic; not a real tokenizer. |
| `compaction.keep_recent` | `8` | Most-recent messages retained verbatim through compaction. |

Missing `settings.json` is non-fatal — Peggy uses the built-in
defaults above and emits a stderr diagnostic.

## CLI

```
peggy [flags] "<prompt text>"

  --config <path>    Override the settings.json path.
  --soul <path>      Override the SOUL.md path.
  --session <id>     Session id (default "default"). File-backed
                     transcripts key off this; a fresh id starts a
                     new conversation while still allowing search
                     across all sessions.
  --version          Print the version and exit.
  --help             Print this help.
```

The prompt is whatever non-flag args you pass — quoting is your
shell's job. Multi-word prompts work without quoting too.

### Config resolution

- Settings: `--config` > `$PEGGY_CONFIG` > `$XDG_CONFIG_HOME/peggy/settings.json` > `~/.config/peggy/settings.json`.
- Identity: `--soul` > `$PEGGY_SOUL` > `$XDG_CONFIG_HOME/peggy/SOUL.md` > `~/.config/peggy/SOUL.md`.

An explicit `--config` / `--soul` or `$PEGGY_CONFIG` / `$PEGGY_SOUL`
that points at a missing file is an error. The XDG / HOME fallbacks
quietly fall through to defaults when missing.

## Memory

Peggy ships two **model-callable** tools that let the model decide
when to persist and when to look up across sessions:

- `remember(content, tags?)` — append a curated fact to the
  `__memories__` session. Phrase content in third person ("the user
  prefers …"). Tags are optional. The model is told (via the system
  prompt) to use this sparingly — for facts worth keeping across many
  future conversations, not one-off context.
- `recall(query, limit?, only_memories?)` — search prior history and
  curated memories via FTS5. Default returns up to 5 hits across all
  sessions; pass `only_memories: true` to restrict to the curated
  list. Requires a `Searcher`-capable store (`stores/sqlite`); the
  `file` store surfaces a clear "use sqlite" error.

The model invokes these on its own as the conversation requires. The
tools are registered by default; disable them via
`Options.DisableMemoryTools` (library) — there's no CLI flag in v0.1.

A short hint paragraph is appended to your `SOUL.md` content in the
system prompt so the model knows the tools exist; override via
`Options.MemoryHint` if you want different phrasing.

Inspecting memories programmatically:

```go
mems, _ := p.ListMemories(ctx) // newest-first
for _, m := range mems {
    fmt.Printf("[%s] %s  (tags=%v)\n",
        m.Timestamp.Format(time.RFC3339), m.Content, m.Tags)
}
```

A `peggy memories` subcommand for list / forget / export is a
near-term follow-up.

## What v0.1 supports

- Single-prompt CLI with file or sqlite session persistence.
- **Model-callable `remember` and `recall` tools** for cross-session memory.
- Cross-session FTS5 search via `Agent.SearchSessions` (library API
  available directly; CLI subcommand is a near-term follow-up).
- Token-aware summarizing compaction via the provider you configured.
- Identity injected from `SOUL.md` into the system prompt.
- All four shipped providers: `codex` (ChatGPT subscription), `gemini`,
  `openrouter`, `nvidia`.

## Channels

Beyond the single-prompt CLI, Peggy is reachable from any number of
external transports. The pattern is designed in
[`docs/adr/0008-channel-adapter.md`](../../docs/adr/0008-channel-adapter.md):

- Each channel lives in its own package under
  `agents/peggy/channels/<name>`. Telegram is the first concrete
  channel — see [`channels/telegram/README.md`](channels/telegram/README.md)
  for the bot setup and the `peggy-telegram` binary.
- Channels satisfy a small `peggy.Channel` interface and call into
  the existing `Peggy.Prompt` API. They never modify core `glue`.
- Channels namespace their session ids (`telegram:12345` etc.) so a
  single sqlite store cleanly holds CLI sessions, channel sessions,
  and the curated `__memories__` session without collision.
- Channels accepting input from anyone reachable (Telegram, public
  webhooks) gate inbound traffic on an allowlist configured in
  `settings.json` under `channels.<name>`. Empty allowlist =
  refuse-all (the safe default).

## What's coming

Per tracker [#110](https://github.com/erain/glue/issues/110):

- Telegram channel adapter (sender allowlist; first concrete channel
  on the ADR-0008 pattern).
- Coding ability — `tools/shell`, `tools/fs.FileWrite`, the subagent primitive.
- Multi-channel daemon (`cmd/glue serve`) so one always-on Peggy serves a TUI, Telegram, and future clients.

## As a library

The package is importable. Tests and out-of-tree integrations can
construct a Peggy directly:

```go
p, err := peggy.New(peggy.Options{
    Settings: peggy.Settings{Provider: "openrouter"},
    Soul:     "You are Peggy. Be terse.",
    Provider: myFakeProvider, // optional override
    Store:    myStore,        // optional override
})
if err != nil { /* … */ }
defer p.Close()

text, err := p.Prompt(ctx, "session-id", "hello", os.Stdout)
```

See [`peggy.go`](peggy.go) for the full API.
