# glue

[![CI](https://github.com/erain/glue/actions/workflows/ci.yml/badge.svg)](https://github.com/erain/glue/actions/workflows/ci.yml)

**Glue is a Go framework for building agents.** It gives you a reusable,
provider-agnostic agent loop, a small code-first `Agent` / `Session`
API, typed tools, pluggable model providers, and optional persistence —
so you can build anything from a one-shot CLI to a long-running,
multi-channel assistant without rewriting the loop each time.

Inspired by [Flue](https://github.com/withastro/flue) and
[pi-mono](https://github.com/badlogic/pi-mono).

```go
agent := glue.NewAgent(glue.AgentOptions{
	Provider: gemini.New(gemini.Options{}),
	Model:    "gemini-3.1-pro-preview",
	Tools:    []glue.Tool{weatherTool},
})
session, _ := agent.Session(ctx, "demo")
result, _ := session.Prompt(ctx, "What's the weather in Toronto?")
fmt.Println(result.Text)
```

- **New here and want to build an agent?** → [docs/building-agents.md](docs/building-agents.md)
- **Just want to send a prompt?** → [Quickstart](#quickstart)
- **Want the architecture?** → [Concepts](#concepts) · [docs/design.md](docs/design.md)

## Install

```sh
go get github.com/erain/glue
```

Module path: `github.com/erain/glue`. Key subpackages:

| Import | Purpose |
|--------|---------|
| `github.com/erain/glue` | Public API: `Agent`, `Session`, `Tool`, options. |
| `.../loop` | The provider-agnostic agent loop. |
| `.../providers/{gemini,codex,nvidia,openrouter}` | Model providers (+ shared `openaicompat` core, driver registry in `providers`). |
| `.../stores/{file,sqlite}` | Session persistence (sqlite adds FTS5 search). |
| `.../tools/{fs,git,shell,coding,mcp}` | Reusable tool bundles. |
| `.../prompts` | Versioned-prompt catalog. |
| `.../cli` | Shared standard flags for agent binaries. |

## Quickstart

Pick a provider and send a prompt. With Gemini:

```sh
export GEMINI_API_KEY=...
```

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/erain/glue"
	"github.com/erain/glue/providers/gemini"
)

func main() {
	ctx := context.Background()
	agent := glue.NewAgent(glue.AgentOptions{
		Provider: gemini.New(gemini.Options{}),
		Model:    "gemini-3.1-pro-preview",
	})
	session, err := agent.Session(ctx, "demo")
	if err != nil {
		log.Fatal(err)
	}
	result, err := session.Prompt(ctx, "Reply with the single word: glue.")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(result.Text)
}
```

The session keeps an in-memory transcript, so a second `Prompt`
continues the conversation. Other providers are one import away — see
[Providers](#providers). To go from here to a real tool-calling,
persistent agent, follow **[docs/building-agents.md](docs/building-agents.md)**.

## Concepts

Glue has a small vocabulary. Once these click, the rest is API surface.

| Type | What it is |
|------|-----------|
| **`Provider`** | A model backend that streams assistant events. |
| **`Agent`** | The configured unit: provider, model, tools, store, work dir, roles. Built with `glue.NewAgent`. |
| **`Session`** | A named conversation with its own transcript, opened from an Agent; driven with `session.Prompt`. |
| **`Tool`** | A function the model can call. Define with `glue.NewTool[Args]`. |
| **`Store`** | Where transcripts persist (`stores/file` or `stores/sqlite`). Optional. |
| **`Skill` / `Role`** | Markdown-driven reusable instructions and named instruction profiles. |
| **`loop`** | The engine: stream → run tools → append results → repeat until the model stops. |

Every `session.Prompt` runs the same loop:

```text
prompt ─▶ provider streams events ─▶ text? emit deltas
                                  └─▶ tool calls? run tools, append results, loop
                                  └─▶ stop ─▶ return final text
```

The loop is provider-agnostic, and product concerns (sandboxing,
channels, scheduling, policy) enter only as interfaces you fill in —
they are not baked into core glue ([ADR-0005](docs/adr/0005-foundation-expansion.md)).

## Build your own agent

The full walkthrough — typed tools, persistence, streaming, project
context, subagents, structured output, multi-provider failover,
packaging as a CLI, and testing — lives in one place:

➡️ **[docs/building-agents.md](docs/building-agents.md)**

The shortest complete example is
[`examples/local-agent`](examples/local-agent) (~100 lines: provider +
store + a typed `local_time` tool + streaming). Real agents live under
[`agents/`](#reference-agents).

The sections below are a feature reference for when you need the
specifics.

## Providers

Glue ships four providers and a driver-style registry. Construct one
directly, or select by name via `providers.New`.

| Provider | Import | Auth | Notes |
|----------|--------|------|-------|
| **Gemini** | `providers/gemini` | `GEMINI_API_KEY` | Google `genai` SDK. |
| **Codex** | `providers/codex` | ChatGPT subscription (`codex login`) | No per-token bill; reuses the upstream Codex CLI's `auth.json`. |
| **NVIDIA build** | `providers/nvidia` | `NVIDIA_API_KEY` | OpenAI-compatible; Kimi K2, Llama, Qwen, etc. by `org/name`. |
| **OpenRouter** | `providers/openrouter` | `OPENROUTER_API_KEY` | OpenAI-compatible aggregator; `openrouter/free` auto-picks a free model. |

```go
// Codex — bill against your ChatGPT subscription instead of an API key:
agent := glue.NewAgent(glue.AgentOptions{
	Provider: codex.New(codex.Options{}),
	Model:    codex.DefaultModel, // "gpt-5-codex"
})
```

Codex quarantines all subscription-auth fragility (OAuth, token refresh,
Cloudflare cookies) to its package — run `codex login` once; Glue reads
`~/.codex/auth.json` (override with `$GLUE_CODEX_AUTH` / `$CODEX_HOME`).
Subscription-auth via third-party tools is not formally documented by
OpenAI; the provider is intended for personal use. See
[ADR-0006](docs/adr/0006-codex-provider.md).

NVIDIA and OpenRouter share the `providers/openaicompat` core. Both can
have multi-second first-byte latency on cold routing. To add your own
provider, see [docs/provider-guide.md](docs/provider-guide.md) and
[`examples/echo-provider`](examples/echo-provider).

### Failover across providers

`glue.WithFailover(provs...)` tries providers in order until one accepts
the stream — handy when a CLI supports several backends and should skip
those whose keys aren't set:

```go
import (
	"github.com/erain/glue"
	"github.com/erain/glue/providers"
	_ "github.com/erain/glue/providers/codex"
	_ "github.com/erain/glue/providers/gemini"
	_ "github.com/erain/glue/providers/nvidia"
)

var provs []glue.Provider
for _, name := range []string{"codex", "nvidia", "gemini"} {
	if p, _, _, err := providers.New(name); err == nil {
		provs = append(provs, p)
	}
}
agent := glue.NewAgent(glue.AgentOptions{Provider: glue.WithFailover(provs...)})
```

Failover only falls through *before* the first event commits to the
consumer; once a non-error event arrives it stays on that provider for
the turn. All-providers-failed surfaces as a typed `*glue.FailoverError`.

## Tools

Define typed tools with `glue.NewTool[Args]`. It decodes
`ToolCall.Arguments` into your Go type before the executor runs and
turns malformed arguments into a model-visible error result instead of a
panic. Pair it with `glue.TextResult` / `glue.ErrorResult`:

```go
type weatherArgs struct {
	City string `json:"city"`
}

weather := glue.NewTool[weatherArgs](
	glue.ToolSpec{
		Name:        "weather",
		Description: "Look up current weather for a city.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
	},
	func(ctx context.Context, a weatherArgs) (glue.ToolResult, error) {
		report, err := lookup(ctx, a.City)
		if err != nil {
			return glue.ErrorResult(err), nil
		}
		return glue.TextResult(report), nil
	},
)
```

Return `ErrorResult` for recoverable failures (the model can retry); a
Go `error` only for failures that should stop the run. Schema generation
is out of scope — write `Parameters` by hand.

**Ready-made bundles** under `tools/`: `tools/fs` (read/write/edit, plus
read-only `list_dir`/`find_files`/`grep`), `tools/git`, `tools/shell`,
and the assembled `tools/coding` bundle. See [Coding tools](#coding-tools).

**Subagents.** `glue.SubagentTool` wraps a child `*glue.Agent` as a
tool, so a parent can delegate a focused task to a fresh, isolated
transcript:

```go
researchTool, _ := glue.SubagentTool(glue.SubagentOptions{
	Name:        "research",
	Description: "Delegate a focused research question.",
	Agent:       researcher, // a *glue.Agent
})
```

**MCP servers.** `tools/mcp` consumes [Model Context
Protocol](https://modelcontextprotocol.io) servers (stdio / Streamable
HTTP), mapping their tools to permission-gated `glue.Tool` values. See
[ADR-0011](docs/adr/0011-mcp-client-integration.md).

## Persistent sessions with search

`stores/file` writes one JSON file per session — the dependency-free
default. `stores/sqlite` implements the same `glue.Store` against a
pure-Go SQLite DB with FTS5 over message text, for cross-session recall:

```go
store, err := sqlite.Open(sqlite.Options{Path: "agent.db"})
defer store.Close()
agent := glue.NewAgent(glue.AgentOptions{Provider: prov, Store: store})

hits, _ := agent.SearchSessions(ctx, "Australian Shepherd", glue.WithLimit(5))
for _, h := range hits {
	fmt.Printf("[%s#%d] %s\n", h.SessionID, h.Index, h.Snippet)
}
```

Search options: `WithLimit`, `WithOffset`, `WithSessionID`, `WithSince`,
`WithUntil`. The query is FTS5 `MATCH` syntax; hits come back by BM25
score. `stores/file` does not implement search, so both
`Agent.SearchSessions` and `Session.Search` return
`glue.ErrSearchNotSupported` there — picking `stores/sqlite` is the
signal that you want it. Uses
[`modernc.org/sqlite`](https://gitlab.com/cznic/sqlite) (no CGo). Schema
details in [ADR-0007](docs/adr/0007-memory-layer.md).

## Streaming, roles, skills, structured output

**Streaming.** Mirror text deltas with the convenience options, or
subscribe for full control:

```go
session.Prompt(ctx, "Stream a haiku.",
	glue.WithStreamWriter(os.Stdout),  // EventTextDelta → writer
	glue.WithToolLogger(os.Stderr),    // "[tool] <name>" on tool start
)

unsubscribe := session.Subscribe(func(e glue.Event) {
	if e.Type == glue.EventTextDelta { fmt.Print(e.Delta) }
})
defer unsubscribe()
```

**Per-prompt overrides:** `glue.WithModel`, `glue.WithSystemPrompt`,
`glue.WithMaxTurns`.

**Roles** are named instruction profiles with optional model overrides,
from `AgentOptions.Roles` or `<WorkDir>/roles/*.md`. Precedence:
`WithRole` (call) > `WithSessionRole` (session) > `AgentOptions.Role`.

**Project context & skills.** Set `AgentOptions.WorkDir`:
`<WorkDir>/AGENTS.md` is appended to the system prompt;
`<WorkDir>/.agents/skills/<name>/SKILL.md` becomes a runnable skill via
`session.Skill(ctx, name, args)`.

**Structured output.** `session.PromptJSON(ctx, prompt, &out)` requests
JSON-only and decodes into your Go type; `glue.WithJSONSchema(schema)`
forwards an explicit schema.

**Versioned prompts.** `prompts.NewCatalog(embedFS, dir, default)` wraps
an `embed.FS` of `<version>.md` files so you can A/B-test and roll back
system prompts; unknown versions error with the available list.

**Long context.** `AgentOptions.Compactor` + `CompactionThreshold`.
`glue.KeepRecentMessages(n)` is the zero-dependency default;
`SummarizingCompactor` is token-aware
([ADR-0002](docs/adr/0002-context-compaction.md),
[ADR-0007](docs/adr/0007-memory-layer.md)).

## Coding tools

`tools/coding.Tools(...)` assembles a permission-gated local coding
bundle — `read_file`, `write_file`, `edit_file`, `list_dir`,
`find_files`, `grep`, `shell_exec`, `git_diff_branch`, `git_log_branch`
— over `tools/fs`, `tools/git`, `tools/shell`, and `glue.Executor`. The
`glue` binary exposes it directly:

```sh
go run ./cmd/glue run --provider codex --coding --work . \
  --prompt "Run the tests and fix the first failure."
```

Side-effecting tools (`write_file`, `edit_file`, `shell_exec`) are
permission-gated; reads and navigation are not. Execution defaults to
the local process via `glue.Executor` — not a sandbox. Implement your
own `Executor` to run in a container/VM. See
[ADR-0012](docs/adr/0012-sdk-coding-agent-peggy-boundary.md).

## The `glue` CLI

A thin CLI over the same library API, for trying things without writing
a `main.go`:

```sh
# Interactive TUI (default when stdin/stdout are a terminal and no --prompt):
go run ./cmd/glue run --provider codex --coding --work .

# One-shot run (any registered provider; default gemini):
go run ./cmd/glue run --prompt "Say hi" --id demo --store .glue/sessions
go run ./cmd/glue run --provider codex --coding --work . --prompt "Fix the failing test."

# Scripted: pipe the prompt in.
echo "summarize main.go" | go run ./cmd/glue run --provider codex --coding

# Local HTTP+SSE daemon + a client that streams and brokers permissions:
go run ./cmd/glue serve --store .glue/sessions
go run ./cmd/glue connect --inspect
go run ./cmd/glue connect --prompt "Say hi" --id demo

# Headless goal loop (schedulable from cron/CI; exit code reflects the
# outcome: 0 achieved · 2 blocked · 3 max-iterations · 4 budget · 1 error):
go run ./cmd/glue goal --coding --yolo --worktree "Make the linter pass on ./..."
go run ./cmd/glue goal --list
go run ./cmd/glue goal --resume
```

**Interactive mode** (designed in [ADR-0014](docs/adr/0014-coding-agent-tui.md)).
With no `--prompt` and a terminal on both stdin and stdout, `glue run`
opens a bubbletea TUI: scrollable transcript with sticky-scroll,
multi-line input, streaming text (re-rendered as markdown after each
turn settles via `charmbracelet/glamour`), tool-call cards with a
moving spinner while running and an inline `[a] [s] [t] [n]` permission
prompt right inside the card when a side-effecting tool needs approval,
and a small `edit_file` diff preview. Slash commands: `/help`,
`/exit`, `/clear` / `/new`, `/usage`, `/tools`, `/model <id>`,
`/session [id]`, **`/compact`** (token-aware summarization of older
messages to free context window), **`/resume`** (modal picker over
past sessions; ↑/↓ navigate, Enter replays the chosen one into the
transcript), **`/fork [N]`** (branch from message N — defaults to
"just before my last user turn" — into a new session id, keeping the
original intact), **`/clone`** (full duplicate of the current
session), **`/tree`** (visualize the session lineage with
`├─ / └─` glyphs, current node marked `◉`; pick one to switch — see
[ADR-0015](docs/adr/0015-session-tree.md)), and **`/goal <objective>`**
(pursue a goal autonomously in the background via `Agent.PursueGoal` —
plan → maker → independent checker per iteration, with a live `[x]/[ ]`
checklist card in the transcript, a `◎ goal · iter 2/10 · 1/4 ✓` status-bar
segment, and `/goal status` / `pause` / `resume` (continues from the verified
checklist without re-planning — even in a new process, since progress is
checkpointed to the session store) / `list` (recent goals with status and
age) / `clear` subcommands; `/goal -w <objective>` isolates the run in its
own git worktree at `.glue/worktrees/<goal-id>` on branch `goal/<id>`, so
the loop never touches your checkout and the result is a reviewable
branch — see [ADR-0016](docs/adr/0016-goal-loop.md)). Anywhere in a prompt,
**`@<path>`** inlines that file's
contents (`@"path with space"` for spaces, `@@literal` to escape — and
the workspace blocklist refuses `.env` / `id_rsa` / etc.). Typing
**`@`** in the TUI input also opens an inline file-picker popup that
fuzzy-matches workspace files; `↑/↓` to navigate, `Tab`/`Enter` to
insert. **Enter**
sends; **Ctrl+J** inserts a newline (works on every terminal —
Shift+Enter does not). **Esc** cancels the
current turn; **Ctrl+C** once cancels (and a second press quits);
mouse wheel scrolls the transcript; PgUp/PgDn does too. The TUI
dependencies
(`charmbracelet/{bubbletea,bubbles,lipgloss,glamour}`) live under
`cmd/glue/tui/` only — `go get github.com/erain/glue` consumers pull
zero TUI code.

`run` flags include `--provider`, `--model`, `--id`, `--store`,
`--work`, `--coding` (+ `--allow-binary`, `--coding-allow-overwrite`),
`--tools name1,name2` (allowlist) / `--no-tools` (text-only),
`--mode text|json` (one-shot output format; `json` emits stable
JSONL events for scripting), **`--yolo`** (auto-approve every
side-effecting tool call — daily-driver mode for trusted feature
branches), `--usage`, and repeatable `--env`. `serve` brokers coding-tool
permission requests to the connected `connect` client; it writes
connection metadata to the user config dir (never the bearer token).
The daemon protocol is [ADR-0010](docs/adr/0010-daemon-protocol.md).

**Standard flags for your own binary.** `cli.RegisterStandardFlags`
wires the same six flags (`--provider`, `--model`, `--id`, `--store`,
`--work`, `--max-turns`) onto a `flag.FlagSet`:

```go
fs := flag.NewFlagSet("my-agent", flag.ContinueOnError)
get := cli.RegisterStandardFlags(fs, nil)
fs.Parse(os.Args[1:])
cfg := get() // cfg.Provider, cfg.Model, cfg.ID, cfg.Store, cfg.Work, cfg.MaxTurns
```

## Reference agents

Real agents built on the framework live under `agents/` (peer of the
harness), not `examples/` (tutorial demos only).

- **[`agents/glue-review`](agents/glue-review)** — a free, local
  pre-push branch reviewer. Reads the diff against `main`, deep-reads
  files when needed, and posts **one** sticky GitHub comment with a
  fenced ` ```markdown ` fix block downstream coding agents can paste.
  Runs as a CLI or a GitHub Action. Defaults to `openrouter/free` with
  automatic provider failover.

- **[`agents/peggy`](agents/peggy)** — a long-running personal-assistant
  agent: CLI + Telegram + a shared HTTP+SSE daemon, durable
  sqlite+FTS5 memory with curated recall, opt-in coding tools, MCP
  servers, scheduled/proactive runs, and per-channel permission tiers.
  The best reference for a feature-rich agent. Tracker:
  [#110](https://github.com/erain/glue/issues/110).

  ```sh
  go install github.com/erain/glue/agents/peggy/cmd/peggy@latest
  codex login
  peggy "Hello — what should I be working on today?"
  ```

## Testing without API keys

The `Provider` interface is tiny, so tests drive sessions with a fake —
no credentials, no network. Test tools by calling
`tool.Execute(ctx, glue.ToolCall{...})` and asserting on the
`ToolResult` (including `IsError`). See the
[testing step](docs/building-agents.md#step-10--test-without-api-keys)
of the build guide for a copy-paste fake.

## Run the tests

```sh
go build ./...
go vet ./...
go test ./...
```

CI runs the same commands on every PR. Live provider tests are gated
behind their API keys and skipped in CI, e.g.:

```sh
GEMINI_API_KEY=... go test ./providers/gemini -run Live
```

## Project status & contributing

The project advances on three fronts: the **framework** (the library
you `go get` — feature-complete and stable in practice), the **`glue`
binary as a coding agent** (interactive TUI, coding tools, and the
autonomous goal loop, [ADR-0016](docs/adr/0016-goal-loop.md)), and
**Peggy**, the long-running personal assistant built on top
([`agents/peggy`](agents/peggy)). Releases are tagged on a `1.x` line
(currently well past `v1.10`), but the stability stance is still the
pre-stability one recorded in
[ADR-0013](docs/adr/0013-pre-1-0-stability-stance.md) (see its
addendum for why the tags say `1.x`): the public `Agent` / `Session`
surface is stable in practice, but **minor versions may still break
API** until a deliberate surface-review pass. Breaking changes always
land with a `**Breaking:**` entry in [`CHANGELOG.md`](CHANGELOG.md),
never on a patch release. Security reports go through
[`SECURITY.md`](SECURITY.md).

Glue is built one GitHub issue at a time. The contributor workflow,
branch/PR conventions, and the active tracker are documented in
[`CONTRIBUTING.md`](CONTRIBUTING.md); the roadmap shape lives in
[`docs/project-plan.md`](docs/project-plan.md), and durable design
decisions are recorded as ADRs under [`docs/adr/`](docs/adr). The
canonical architecture reference is [`docs/design.md`](docs/design.md).
