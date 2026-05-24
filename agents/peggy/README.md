# peggy

> Your always-on assistant — runs from the CLI or Telegram,
> remembers you across sessions, and curates facts on her own.

A long-running personal-assistant agent built on the
[glue](../..) framework. **v0.4** ships:

- a single-prompt **CLI** (`peggy`)
- a **Telegram bot** binary (`peggy-telegram`) with a chat-id allowlist
- a local **HTTP+SSE daemon** (`peggy serve`) shared by terminal and
  Telegram clients
- model-callable **`remember` / `recall` tools** for durable
  cross-session memory
- **token-aware summarizing compaction** so long sessions don't blow
  the context window
- **FTS5 session search** under the hood, exposed via
  `Agent.SearchSessions`
- opt-in local **coding tools** for reading files, writing files,
  running allowlisted commands, and inspecting git branch context
- per-call permission prompts for side-effecting coding tools in the
  CLI, Telegram, and daemon clients
- per-channel permission tiers (`prompt`, `read_only`, `trusted`)
- opt-in MCP stdio/HTTP tools plus resource and prompt inspection
- local readiness status for config, identity, memory, coding, and MCP setup
- four model backends: Codex (ChatGPT subscription), Gemini,
  OpenRouter, NVIDIA build

Tracker: [#110](https://github.com/erain/glue/issues/110). M4
("ecosystem") is the v0.4 release milestone.

## Quickstart

```sh
# 1. Install.
go install github.com/erain/glue/agents/peggy/cmd/peggy@latest

# 2. One-time auth (Codex subscription is the default provider).
codex login

# 3. Optional: drop an identity file so Peggy knows who you are.
mkdir -p ~/.config/peggy
$EDITOR ~/.config/peggy/SOUL.md         # see "SOUL.md" below
$EDITOR ~/.config/peggy/settings.json   # see "settings.json" below

# 4. Talk to her.
peggy "Hello — what should I be working on today?"

# 5. Optional: let Peggy work in a trusted repo.
cd /path/to/repo
peggy --coding --workdir . "read the failing test and propose a fix"

# 6. Optional: keep one Peggy process running and connect to it.
go install github.com/erain/glue/cmd/glue@latest  # if needed
peggy serve --coding --workdir .
glue connect --inspect
glue connect --prompt "what should I do next?" --id cli:daily
```

To reach her from your phone, set up Telegram next — see
[Channels](#channels) below.

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
  },
  "permissions": {
    "default_tier": "prompt",
    "channels": {
      "cli": "trusted",
      "telegram": "prompt"
    }
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
| `coding.enabled` | `false` | Register local coding tools. Enable only for trusted local workspaces. |
| `coding.work_dir` | current directory | Workspace root for `read_file`, `write_file`, `shell_exec`, and git helpers. `~` and `$HOME` expand. |
| `coding.allowed_binaries` | `go`, `git`, `make`, `node`, `npm`, `python`, `python3` | Basename allowlist for `shell_exec`; model calls cannot run arbitrary paths. |
| `coding.allow_overwrite` | `false` | Host policy for replacing existing files. The model must still pass `overwrite: true`, and the permission prompt must allow the call. |
| `mcp.servers.<name>.enabled` | `false` | Register tools from a configured MCP server. Enable only for trusted local servers or explicitly trusted services. |
| `mcp.servers.<name>.transport` | `stdio` | `stdio` or Streamable HTTP (`http`). |
| `mcp.servers.<name>.command` | none | Executable path or basename for stdio MCP servers. This is argv-based, not a shell string. |
| `mcp.servers.<name>.args` | `[]` | Stdio server argv arguments. |
| `mcp.servers.<name>.env` | `[]` | Explicit `KEY=value` environment entries passed to the stdio server. Peggy does not inherit the full parent env by default. |
| `mcp.servers.<name>.work_dir` | current process dir | Working directory for the stdio server. `~` and `$HOME` expand. |
| `mcp.servers.<name>.url` | none | Streamable HTTP MCP endpoint URL. Required for `transport: "http"`. |
| `mcp.servers.<name>.headers_env` | `{}` | Map HTTP header names to env var names. Peggy resolves the env vars at startup and does not write secret values back to settings. |
| `mcp.servers.<name>.timeout_seconds` | `30` | Timeout for initialize, `tools/list`, `tools/call`, `resources/list`, and `resources/read` requests. |
| `permissions.default_tier` | `prompt` | Permission tier for side-effecting tools when a channel has no override. One of `prompt`, `read_only`, or `trusted`. |
| `permissions.channels.<name>` | inherited | Channel override keyed by `cli`, `telegram`, or a future daemon client prefix. |

Missing `settings.json` is non-fatal — Peggy uses the built-in
defaults above and emits a stderr diagnostic.

## CLI

```
peggy [flags] "<prompt text>"
peggy mcp prompt [flags]
peggy mcp prompts [flags]
peggy mcp read [flags]
peggy mcp resources [flags]
peggy mcp tools [flags]
peggy serve [flags]
peggy status [flags]

  --config <path>    Override the settings.json path.
  --soul <path>      Override the SOUL.md path.
  --session <id>     Session id (default "default"). File-backed
                     transcripts key off this; a fresh id starts a
                     new conversation while still allowing search
                     across all sessions.
  --coding           Enable local coding tools for this prompt.
  --workdir <path>   Workspace root for --coding (default ".").
  --coding-allow-overwrite
                     Allow write_file to replace existing files after
                     model intent and user permission.
  --version          Print the version and exit.
  --help             Print this help.
```

The prompt is whatever non-flag args you pass — quoting is your
shell's job. Multi-word prompts work without quoting too.

`peggy status` prints a local readiness summary without constructing a
provider, starting a prompt, or connecting to MCP servers:

```sh
peggy status --config ~/.config/peggy/settings.json
peggy status --config ~/.config/peggy/settings.json --json
```

## Daemon Mode

`peggy serve` starts one local Peggy process behind the shared
HTTP+SSE daemon protocol from
[`ADR-0010`](../../docs/adr/0010-daemon-protocol.md). It loads the
same `settings.json` and `SOUL.md` as the single-prompt CLI, keeps one
provider/store in memory, and lets daemon clients share the same
session history and memory store.

```sh
peggy serve --config ~/.config/peggy/settings.json
glue connect --inspect
glue connect --mcp-resources
glue connect --mcp-prompts
glue connect --mcp-read --server filesystem --uri file:///workspace/README.md
glue connect --mcp-prompt --server linear --name summarize_issue --arg issue=GLUE-123
glue connect --prompt "summarize today's plan" --id cli:daily --usage
```

Useful `serve` flags:

- `--listen` — local listen address (default `127.0.0.1:0`).
- `--token` — bearer token. Defaults to `GLUE_DAEMON_TOKEN` or a
  generated token.
- `--metadata` — connection metadata path. Defaults to the same
  `glue/daemon.json` user-config path that `glue connect` reads.
  Pass an empty value only with `--token` or `GLUE_DAEMON_TOKEN`.
- `--permission-timeout` — maximum time a side-effecting tool waits
  for a client decision.
- `--coding`, `--workdir`, `--coding-allow-overwrite` — same coding
  tool controls as the prompt CLI, but permission prompts are emitted
  over the daemon protocol for the connected client to answer.

Startup output prints the `base_url` and metadata path, never the
bearer token. `glue connect --inspect` includes status, tools, and any
daemon-advertised MCP resource/prompt catalogs. Use
`--mcp-resources-json`, `--mcp-prompts-json`, `--mcp-read-json`, or
`--mcp-prompt-json` when another client needs the MCP payload as data.
Add `--usage` to prompt-mode `glue connect` when you want
provider-reported token usage on stderr. Stop the daemon with
SIGINT/SIGTERM.

Telegram can attach to the same daemon:

```sh
peggy-telegram --daemon
```

In daemon-client mode, Telegram keeps its chat allowlist and inline
permission buttons, but Peggy, memory, coding tools, and remembered
permission scopes live in the daemon process.

Permission tiers apply in daemon mode by daemon `client_id`: `cli:*`
uses the `cli` tier and `telegram:<chat_id>` uses the `telegram` tier.
Use this to keep a trusted local terminal fast while making Telegram
ask every time, or to make Telegram read-only.

Recommended v0.3 daily-driver shape:

```json
{
  "store": {
    "type": "sqlite",
    "path": "~/.peggy/peggy.db"
  },
  "coding": {
    "enabled": true,
    "work_dir": "/path/to/trusted/repo",
    "allowed_binaries": ["go", "git", "make"],
    "allow_overwrite": false
  },
  "permissions": {
    "default_tier": "prompt",
    "channels": {
      "cli": "trusted",
      "telegram": "prompt"
    }
  }
}
```

Run `peggy serve --config ~/.config/peggy/settings.json`, inspect the
live daemon with `glue connect --inspect`, connect from a terminal with
`glue connect --prompt "..." --id cli:daily`, and start Telegram with
`peggy-telegram --daemon`. The daemon prints the base URL and metadata
path but never prints the bearer token.

## Coding Tools

Coding mode is opt-in. Enable it in `settings.json` with
`"coding": {"enabled": true, "work_dir": "/path/to/repo"}` or for one
CLI call with:

```sh
peggy --coding --workdir . "run the tests and fix the failure"
```

Persistent config for a single trusted workspace:

```json
{
  "coding": {
    "enabled": true,
    "work_dir": "/path/to/repo",
    "allowed_binaries": ["go", "git", "make"],
    "allow_overwrite": false
  }
}
```

Peggy registers these model-callable tools:

- `read_file` — read UTF-8 text inside the workspace. Read-only, no
  permission prompt.
- `write_file` — write UTF-8 text inside the workspace. Permission
  required. Existing files require both `coding.allow_overwrite` /
  `--coding-allow-overwrite` and model argument `overwrite: true`.
- `shell_exec` — run argv-style commands inside the workspace.
  Permission required. `argv[0]` must be a configured binary basename.
- `git_diff_branch` / `git_log_branch` — read-only branch context via
  the local `git` binary.

For `write_file` and `shell_exec`, the CLI asks on stderr/stdin:

```text
Allow? [y] once, [s] session, [t] target, [n] deny:
```

Remembered choices last only for the current `peggy` process. If stdin
is unavailable or reaches EOF, Peggy denies the side effect and surfaces
that denial to the model as a tool error.

## MCP Tools

Peggy can register tools from local stdio MCP servers and Streamable
HTTP MCP endpoints. MCP tools are permission-gated by default because
the server is external code or a remote service boundary: even a tool
named `search` may read private data, mutate state, exfiltrate content,
or spend money.

```json
{
  "mcp": {
    "servers": {
      "filesystem": {
        "enabled": true,
        "transport": "stdio",
        "command": "mcp-server-filesystem",
        "args": ["/path/to/workspace"],
        "env": ["LOG_LEVEL=warn"],
        "work_dir": "/path/to/workspace",
        "timeout_seconds": 30
      }
    }
  }
}
```

For HTTP servers, keep tokens in environment variables and point
`headers_env` at those variable names:

```json
{
  "mcp": {
    "servers": {
      "linear": {
        "enabled": true,
        "transport": "http",
        "url": "https://example.invalid/mcp",
        "headers_env": {
          "Authorization": "LINEAR_MCP_AUTH_HEADER"
        }
      }
    }
  }
}
```

Discovered MCP tools are exposed to the model as namespaced tools such
as `mcp_filesystem_read_file`. Resource-capable servers also expose a
permission-gated `mcp_<server>_read_resource` tool that reads a URI
from that server. Permission prompts use the existing channel tier
settings: `prompt` asks the owning client, `read_only` denies before
execution, and `trusted` allows the call subject to the server
configuration and timeout.

Inspect the configured MCP surface without constructing a model
provider or running a prompt:

```sh
peggy mcp read --config ~/.config/peggy/settings.json --server filesystem --uri file:///workspace/README.md
peggy mcp read --config ~/.config/peggy/settings.json --server filesystem --uri file:///workspace/README.md --json
peggy mcp prompts --config ~/.config/peggy/settings.json
peggy mcp prompt --config ~/.config/peggy/settings.json --server linear --name summarize_issue --arg issue=GLUE-123
peggy mcp resources --config ~/.config/peggy/settings.json
peggy mcp resources --config ~/.config/peggy/settings.json --json
peggy mcp tools --config ~/.config/peggy/settings.json
peggy mcp tools --config ~/.config/peggy/settings.json --json
```

`peggy mcp resources` lists resource metadata only. `peggy mcp read`
fetches one resource URI for operator inspection. `peggy mcp prompts`
lists prompt templates and `peggy mcp prompt` renders one prompt with
repeatable `--arg key=value` values. Servers that do not advertise the
requested MCP capability are skipped for listing and cannot serve that
request.

When Peggy is already running as a daemon, remote clients can inspect
the same initialized MCP catalog through the daemon protocol:

```sh
glue connect --mcp-resources
glue connect --mcp-resources-json
glue connect --mcp-prompts
glue connect --mcp-prompts-json
glue connect --mcp-read --server filesystem --uri file:///workspace/README.md
glue connect --mcp-read-json --server filesystem --uri file:///workspace/README.md
glue connect --mcp-prompt --server linear --name summarize_issue --arg issue=GLUE-123
glue connect --mcp-prompt-json --server linear --name summarize_issue --arg issue=GLUE-123
```

The permission choices are intentionally small:

- `deny` — return a model-visible tool error.
- `allow once` — run only this side-effecting call.
- `allow session` — remember this tool/action for the current process
  and session id.
- `allow target` — remember this tool/action/target for the current
  process and session id.

Peggy also supports product-level permission tiers:

- `prompt` — current behavior. Ask the owning client and honor
  remembered scopes.
- `read_only` — deny side-effecting tools before any terminal prompt
  or Telegram inline keyboard is shown.
- `trusted` — allow side-effecting tools without prompting. Existing
  tool controls still apply: workspace root, binary allowlist,
  overwrite policy, timeouts, and output limits.

Example:

```json
{
  "permissions": {
    "default_tier": "prompt",
    "channels": {
      "cli": "trusted",
      "telegram": "read_only"
    }
  }
}
```

Remembered daemon permission decisions are scoped to the daemon client
that made them, so a Telegram allow does not silently authorize a
terminal request.

Coding mode is a trusted-local workflow. The tool layer constrains
paths, binaries, overwrites, output size, and permissions, but Peggy
uses the host process via `glue.LocalExecutor`; it is not a container
or VM sandbox.

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
`Options.DisableMemoryTools` (library) — there's no CLI flag today.

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

## What Peggy supports today

- **Single-prompt CLI** (`peggy`), **daemon mode** (`peggy serve`),
  terminal daemon client (`glue connect`), and **Telegram bot**
  (`peggy-telegram`) in standalone or daemon-client mode.
- File or SQLite session persistence. SQLite enables cross-session
  FTS5 search.
- **Model-callable `remember` / `recall` tools** for durable
  cross-session memory.
- **Cross-session FTS5 search** via `Agent.SearchSessions` library
  API (a `peggy recall` subcommand is a near-term follow-up).
- **Token-aware summarizing compaction** via the configured provider.
- Identity injected from `SOUL.md` into the system prompt.
- Opt-in **local coding mode** for CLI and Telegram: read files, write
  files, run allowlisted commands, and inspect git branch context with
  per-call permission prompts for side effects.
- **Permission tiers** by channel/client: prompt, read-only, or trusted.
- All four shipped providers: `codex` (ChatGPT subscription),
  `gemini`, `openrouter`, `nvidia`. Codex is the default and uses
  your existing ChatGPT subscription via `codex login` — no per-token
  bill.

See [`CHANGELOG.md`](CHANGELOG.md) for the full v0.3 summary and
known limitations.

## Channels

Beyond the single-prompt CLI, Peggy is reachable from any number of
external transports. The pattern is designed in
[`docs/adr/0008-channel-adapter.md`](../../docs/adr/0008-channel-adapter.md):

- Each channel lives in its own package under
  `agents/peggy/channels/<name>`. Telegram is the first concrete
  channel — see [`channels/telegram/README.md`](channels/telegram/README.md)
  for the bot setup, standalone mode, and daemon-client mode.
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

Per tracker [#110](https://github.com/erain/glue/issues/110), in
priority order:

- **M4 — ecosystem.** MCP client (stdio + HTTP), cost tracking,
  `providers/anthropic` when budget allows.

Near-term follow-ups that may slip into v0.3.x patches: a `peggy
memories` subcommand (list / export / forget), edit-in-place
Telegram streaming, FTS5 prefix-match on session ids for
channel-scoped recall.

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
