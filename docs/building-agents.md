# Building Agents on Glue

This is the end-to-end guide for building your own agent on the Glue
framework. It starts from a mental model, then builds up a real agent
one capability at a time: a typed tool, persistence, streaming,
project context, delegation, structured output, multiple providers,
packaging as a CLI, and testing.

If you just want to send one prompt, the [README](../README.md)
Quickstart is shorter. Come here when you want to *build something*.

- New to the concepts? Start with [Mental model](#mental-model).
- Want runnable code? [`examples/local-agent`](../examples/local-agent)
  is the smallest complete agent; [`agents/glue-review`](../agents/glue-review)
  and [`agents/peggy`](../agents/peggy) are real ones.

## Mental model

Glue has a small set of types. Once these click, the rest is API
surface.

| Type | What it is |
|------|-----------|
| **`Provider`** | A model backend that streams assistant events. Gemini, Codex, NVIDIA, OpenRouter ship in `providers/*`; you can add your own. |
| **`Agent`** | The configured unit: a provider, model, tools, store, work dir, roles. Construct once with `glue.NewAgent`. |
| **`Session`** | A named conversation with its own transcript, opened from an Agent with `agent.Session(ctx, id)`. Drive turns with `session.Prompt`. |
| **`Tool`** | A function the model can call. Define typed tools with `glue.NewTool[Args]`. |
| **`Store`** | Where transcripts persist. `stores/file` (JSON) or `stores/sqlite` (FTS5 search). Optional — sessions are in-memory without one. |
| **`Skill` / `Role`** | Markdown-driven reusable instructions (`Skill`) and named instruction profiles with optional model overrides (`Role`). |
| **`loop`** | The provider-agnostic engine underneath: stream → run tools → append results → repeat until the model stops. You rarely call it directly. |

The turn flow, every time you call `session.Prompt`:

```text
prompt ─▶ provider streams assistant events ─▶ text? emit deltas
                                            └─▶ tool calls? run tools,
                                                append results, loop again
                                            └─▶ stop ─▶ return final text
```

Two rules worth internalizing:

- **The loop is provider-agnostic.** Everything from streaming to tool
  execution works the same whichever provider you pick.
- **Product concerns enter as interfaces.** Glue ships a local
  executor, a file/sqlite store, and so on — but sandboxing, channels,
  scheduling, and policy live in *your* code (or in `agents/peggy`),
  not in core glue. See [ADR-0005](adr/0005-foundation-expansion.md).

## Step 1 — A minimal agent

Pick a provider, open a session, send a prompt.

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
		Provider: gemini.New(gemini.Options{}), // reads GEMINI_API_KEY
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

The session holds an in-memory transcript, so a second
`session.Prompt(...)` continues the conversation. Nothing persists yet
— that's [Step 3](#step-3--persistence).

## Step 2 — Add a typed tool

Tools are how the model reaches into your process. Define them with
`glue.NewTool[Args]`: it decodes `ToolCall.Arguments` into your Go type
before the executor runs, and turns malformed arguments into an error
result the model can recover from — not a panic.

```go
import "encoding/json"

type weatherArgs struct {
	City string `json:"city"`
}

func weatherTool() glue.Tool {
	return glue.NewTool[weatherArgs](
		glue.ToolSpec{
			Name:        "weather",
			Description: "Look up current weather for a city.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": { "city": { "type": "string" } },
  "required": ["city"]
}`),
		},
		func(ctx context.Context, a weatherArgs) (glue.ToolResult, error) {
			if a.City == "" {
				return glue.ErrorResult(errors.New("city is required")), nil
			}
			report, err := lookup(ctx, a.City)
			if err != nil {
				return glue.ErrorResult(err), nil // model sees the error and can retry
			}
			return glue.TextResult(report), nil
		},
	)
}
```

Register it on the agent:

```go
agent := glue.NewAgent(glue.AgentOptions{
	Provider: gemini.New(gemini.Options{}),
	Model:    "gemini-3.1-pro-preview",
	Tools:    []glue.Tool{weatherTool()},
})
```

Convention: return `glue.ErrorResult(err)` for *recoverable* tool
failures (bad input, upstream 404) so the model can adjust; return a Go
`error` only for failures that should stop the whole run (context
cancellation). Schema generation from `Args` is intentionally out of
scope — write `Parameters` by hand.

Ready-made tool bundles live under `tools/`: `tools/fs`, `tools/git`,
`tools/shell`, and the assembled `tools/coding` bundle (`read_file`,
`write_file`, `edit_file`, `list_dir`, `find_files`, `grep`,
`shell_exec`, git helpers) — see [Step 11](#going-further).

## Step 3 — Persistence

Pass a `Store` and transcripts survive across processes. `stores/file`
is the simple default:

```go
import filestore "github.com/erain/glue/stores/file"

agent := glue.NewAgent(glue.AgentOptions{
	Provider: gemini.New(gemini.Options{}),
	Model:    "gemini-3.1-pro-preview",
	Store:    filestore.New(".glue/sessions"),
})
```

Re-opening the same session id resumes the conversation.

For long-running agents that need cross-session recall ("what did I say
about my dog last week?"), use `stores/sqlite`, which implements the
same interface with FTS5 search:

```go
import "github.com/erain/glue/stores/sqlite"

store, err := sqlite.Open(sqlite.Options{Path: "agent.db"})
if err != nil { log.Fatal(err) }
defer store.Close()

agent := glue.NewAgent(glue.AgentOptions{Provider: prov, Store: store})

hits, err := agent.SearchSessions(ctx, "Australian Shepherd", glue.WithLimit(5))
for _, h := range hits {
	fmt.Printf("[%s#%d] %s\n", h.SessionID, h.Index, h.Snippet)
}
```

Search is the `Searcher` capability: `stores/file` deliberately does not
implement it, so both `Agent.SearchSessions` and `Session.Search` return
`glue.ErrSearchNotSupported` on a file store — picking `stores/sqlite`
is the explicit signal that you want search.

## Step 4 — Stream output

For a CLI, mirror assistant text to stdout as it arrives:

```go
_, err := session.Prompt(ctx, "Write a haiku about glue.",
	glue.WithStreamWriter(os.Stdout),  // writes EventTextDelta to the writer
	glue.WithToolLogger(os.Stderr),    // logs "[tool] <name>" on tool start
)
```

For richer handling, subscribe to events directly:

```go
unsubscribe := session.Subscribe(func(e glue.Event) {
	switch e.Type {
	case glue.EventTextDelta:
		fmt.Print(e.Delta)
	case glue.EventToolStart:
		fmt.Printf("\n[calling %s]\n", e.ToolName)
	}
})
defer unsubscribe()
```

`Subscribe` fires for every prompt on the session; `glue.WithEvents`
registers a per-prompt handler. They compose — adding one never
displaces another.

## Step 5 — Project context: AGENTS.md, skills, roles

Set `WorkDir` and Glue discovers Markdown context on disk:

- `<WorkDir>/AGENTS.md` is appended to the system prompt for every
  prompt (missing file is fine).
- `<WorkDir>/.agents/skills/<name>/SKILL.md` becomes a runnable skill.
- `<WorkDir>/roles/*.md` become roles (named instruction profiles with
  optional `model:` overrides).

```go
agent := glue.NewAgent(glue.AgentOptions{
	Provider: prov,
	Model:    "gemini-3.1-pro-preview",
	WorkDir:  ".",
	Roles: []glue.Role{
		{Name: "reviewer", Model: "gemini-2.5-pro", Instructions: "Review for SQL safety."},
	},
})

// Run a skill with structured args:
res, _ := session.Skill(ctx, "triage", map[string]int{"issue": 12})

// Apply a role (precedence: WithRole > WithSessionRole > AgentOptions.Role):
res, _ = session.Prompt(ctx, "Review this PR.", glue.WithRole("reviewer"))
```

## Step 6 — Delegate to a subagent

`glue.SubagentTool` wraps a *child* agent as a tool, so a parent agent
can hand off a self-contained task to a fresh, isolated transcript:

```go
researcher := glue.NewAgent(glue.AgentOptions{Provider: prov, Model: "gemini-2.5-pro"})

researchTool, err := glue.SubagentTool(glue.SubagentOptions{
	Name:        "research",
	Description: "Delegate a focused research question; returns a summary.",
	Agent:       researcher,
})
if err != nil { log.Fatal(err) }

orchestrator := glue.NewAgent(glue.AgentOptions{
	Provider: prov,
	Model:    "gemini-3.1-pro-preview",
	Tools:    []glue.Tool{researchTool},
})
```

Each call forwards only the explicit `prompt` argument into a fresh
child session, returns the child's final text, and surfaces child
failures as model-visible error results (except context cancellation,
which stops the parent promptly).

## Step 7 — Structured JSON output

When you need typed data back, not prose:

```go
var out struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}
_, err := session.PromptJSON(ctx, "Return a project name and count.", &out)
```

`PromptJSON` adds JSON-only instructions and sets the provider's JSON
response mode. Pass `glue.WithJSONSchema(schema)` to forward an explicit
schema. Validation is JSON decoding into your Go type.

## Step 8 — Multiple providers and failover

Beyond Gemini, Glue ships `codex` (ChatGPT subscription via
`codex login`), `nvidia` (build.nvidia.com), and `openrouter`. The
driver-style registry lets you select by name, and `glue.WithFailover`
tries providers in order until one accepts the stream:

```go
import (
	"github.com/erain/glue"
	"github.com/erain/glue/providers"
	_ "github.com/erain/glue/providers/codex"      // registers "codex"
	_ "github.com/erain/glue/providers/gemini"     // registers "gemini"
	_ "github.com/erain/glue/providers/nvidia"     // registers "nvidia"
	_ "github.com/erain/glue/providers/openrouter" // registers "openrouter"
)

var provs []glue.Provider
for _, name := range []string{"codex", "nvidia", "gemini"} {
	if p, _, _, err := providers.New(name); err == nil {
		provs = append(provs, p)
	}
}
agent := glue.NewAgent(glue.AgentOptions{
	Provider: glue.WithFailover(provs...),
	Model:    "", // let each provider use its DefaultModel
})
```

Failover only falls through *before* the first event commits to the
consumer; once any non-error event arrives it stays on that provider for
the turn. Adding your own provider? See
[`docs/provider-guide.md`](provider-guide.md) and
[`examples/echo-provider`](../examples/echo-provider).

## Step 9 — Package it as a CLI

Agents that ship a binary share six standard flags
(`--provider`, `--model`, `--id`, `--store`, `--work`, `--max-turns`)
via `cli.RegisterStandardFlags`:

```go
import (
	"flag"
	"github.com/erain/glue/cli"
)

fs := flag.NewFlagSet("my-agent", flag.ContinueOnError)
get := cli.RegisterStandardFlags(fs, nil) // pass &cli.StandardConfig{...} to override defaults
_ = fs.Parse(os.Args[1:])
cfg := get() // cfg.Provider, cfg.Model, cfg.ID, cfg.Store, cfg.Work, cfg.MaxTurns
```

`cfg.Provider` accepts a comma-separated list (e.g.
`nvidia,openrouter,gemini`); resolve it through the registry and
`glue.WithFailover`. [`examples/local-agent`](../examples/local-agent)
is a complete, ~100-line CLI you can copy.

**Want a real interactive TUI for your own agent?** The shipped
`cmd/glue/tui` package is a bubbletea TUI bound to `glue.Agent` /
`glue.Session` / `glue.Permission` — exactly the interfaces your agent
already exposes. You can lift it (or copy-modify it) and call
`tui.Run(ctx, tui.Config{Agent: ...})` from your binary's interactive
branch. See [ADR-0014](adr/0014-coding-agent-tui.md) for the
architecture and the permission-bridging trick that lets the loop
block on user keystrokes without knowing it.

## Step 10 — Test without API keys

The `Provider` interface is tiny, so tests drive sessions with a fake —
no credentials, no network:

```go
type fakeProvider struct{}

func (fakeProvider) Stream(_ context.Context, _ glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	ch := make(chan glue.ProviderEvent, 3)
	ch <- glue.ProviderEvent{Type: glue.ProviderEventStart}
	ch <- glue.ProviderEvent{Type: glue.ProviderEventTextDelta, Delta: "hello"}
	ch <- glue.ProviderEvent{Type: glue.ProviderEventDone}
	close(ch)
	return ch, nil
}

func TestAgent(t *testing.T) {
	agent := glue.NewAgent(glue.AgentOptions{Provider: fakeProvider{}})
	session, _ := agent.Session(context.Background(), "test")
	res, _ := session.Prompt(context.Background(), "hi")
	if res.Text != "hello" {
		t.Fatalf("got %q", res.Text)
	}
}
```

Test tools by calling `tool.Execute(ctx, glue.ToolCall{...})` directly
and asserting on the `ToolResult` (including `IsError` for the recovery
path). Keep live provider tests gated behind an env-var check and out of
CI.

Two loop defaults to know when scripting *failures* in tests: transient-
looking provider errors ("rate limit", "503", dropped streams) are
retried with backoff, and pathological tool patterns (the same call
repeated, all-error rounds) trigger guardrails. When a fake provider
should fail *fast*, use error text that classifies as fatal (e.g.
"invalid request"); callers driving `loop.Run` directly can also pass
`Retry: loop.RetryPolicy{Disabled: true}` or
`Guardrails: loop.GuardrailPolicy{Disabled: true}`.

## Going further

You now have the full shape of a Glue agent. The advanced surfaces, each
opt-in behind an interface:

- **Long context** — `AgentOptions.Compactor` + `CompactionThreshold`.
  `glue.KeepRecentMessages(n)` is the zero-dependency default;
  `SummarizingCompactor` is token-aware. See
  [ADR-0002](adr/0002-context-compaction.md) /
  [ADR-0007](adr/0007-memory-layer.md).
- **Versioned prompts** — `prompts.NewCatalog(embedFS, dir, default)` to
  A/B-test and roll back system prompts.
- **Coding tools** — `tools/coding.Tools(...)` assembles a
  permission-gated local coding bundle over `glue.Executor`. See
  [ADR-0012](adr/0012-sdk-coding-agent-peggy-boundary.md).
- **Sandboxed execution** — implement `glue.Executor` to run
  `shell_exec` in a container/VM instead of the local process.
- **MCP servers** — `tools/mcp` consumes Model Context Protocol servers
  (stdio / Streamable HTTP) as glue tools. See
  [ADR-0011](adr/0011-mcp-client-integration.md).
- **Daemon + channels** — `cmd/glue serve` (and `agents/peggy`) expose
  an agent over a local HTTP+SSE protocol so terminal, Telegram, or
  custom channels share one process. See
  [ADR-0010](adr/0010-daemon-protocol.md) /
  [ADR-0008](adr/0008-channel-adapter.md).

For a fully worked, long-running agent that uses most of these — memory,
channels, permissions, scheduling — read [`agents/peggy`](../agents/peggy).
The canonical architecture reference is [`docs/design.md`](design.md).
