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

## What v0.1 supports

- Single-prompt CLI with file or sqlite session persistence.
- Cross-session FTS5 search via `Agent.SearchSessions` (library API
  only in v0.1; a `peggy recall` subcommand is a near-term follow-up).
- Token-aware summarizing compaction via the provider you configured.
- Identity injected from `SOUL.md` into the system prompt.
- All four shipped providers: `codex` (ChatGPT subscription), `gemini`,
  `openrouter`, `nvidia`.

## What's coming

Per tracker [#110](https://github.com/erain/glue/issues/110):

- `remember-this` / `recall` skills that surface the FTS index to the model.
- Telegram channel adapter (sender allowlist + per-channel permission tier).
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
