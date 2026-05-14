# glue

[![CI](https://github.com/erain/glue/actions/workflows/ci.yml/badge.svg)](https://github.com/erain/glue/actions/workflows/ci.yml)

Glue is a Go agent harness for building local and programmable agents,
inspired by [Flue](https://github.com/withastro/flue) and
[pi-mono](https://github.com/badlogic/pi-mono). It is built around a reusable
provider-agnostic agent loop, a code-first `Agent` / `Session` API, and
pluggable providers — Gemini, plus OpenAI-compatible NVIDIA build and
OpenRouter out of the box.

GitHub issues are the source of truth for the roadmap and implementation
order:

- Project tracker: <https://github.com/erain/glue/issues/1>
- Design doc: [docs/design.md](docs/design.md)
- Project plan: [docs/project-plan.md](docs/project-plan.md)
- Contributor workflow: [CONTRIBUTING.md](CONTRIBUTING.md)

## Status

The harness is feature-complete for the `0.x` series and is in active
use behind the [`agents/glue-review`](agents/glue-review/README.md)
reference agent (single GitHub comment per PR with a fenced
` ```markdown ` fix block downstream coding agents can paste). The
library itself remains pre-1.0 — the public `Agent` / `Session`
surface is stable in practice, but minor versions may still break API.
Shipped today:

- Normalized loop types and the provider-agnostic agent loop in `loop/`,
  with deterministic sequential tool execution, opt-in
  `RunRequest.Parallel`, and `StopReasonMaxTurns` for budget-exhaustion
  detection.
- Public `Agent` / `Session` API: per-prompt event streaming with
  `WithStreamWriter` / `WithToolLogger`, structured JSON output
  (`PromptJSON`), Markdown-driven skills/roles/`AGENTS.md` discovery,
  opt-in `Compactor` interface, typed `NewTool[T]` helper.
- Providers: `gemini` (Google `genai` SDK), `nvidia` and `openrouter`
  (OpenAI-compatible, sharing the `providers/openaicompat` core), a
  driver-style registry under `providers/`, and `glue.WithFailover`.
- Storage: file-backed session store at `stores/file`. Tools: shared
  `tools/fs` and `tools/git` extension packages. CLI: `cmd/glue` runner
  plus `cli.RegisterStandardFlags` for downstream agents. Versioned
  prompts via `prompts.NewCatalog`.

See [`CHANGELOG.md`](CHANGELOG.md) for library-level notes.

## Install

```sh
go get github.com/erain/glue
```

The module path is `github.com/erain/glue`. Subpackages:
`github.com/erain/glue/loop` (reusable agent loop),
`github.com/erain/glue/providers/{gemini,nvidia,openrouter}` (with the
shared OpenAI-compatible core in `providers/openaicompat` and the
driver-style registry in `providers/`),
`github.com/erain/glue/stores/file` (file-backed session store),
`github.com/erain/glue/tools/{fs,git}` (extension tool packages),
`github.com/erain/glue/prompts` (versioned-prompt catalog), and
`github.com/erain/glue/cli` (shared standard flags).

## Quickstart: Gemini

Set a Gemini API key:

```sh
export GEMINI_API_KEY=...
```

Send a prompt:

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
		Model:    "gemini-2.5-flash",
	})

	session, err := agent.Session(ctx, "local-dev")
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

The session keeps an in-memory transcript, so a second `session.Prompt(...)`
continues the conversation. Pass `AgentOptions.Store` (e.g.
[`stores/file`](stores/file)) to persist transcripts across processes.

## Quickstart: NVIDIA build (Kimi K2 and friends)

The `providers/nvidia` package speaks the OpenAI-compatible API exposed at
[`build.nvidia.com`](https://build.nvidia.com), so any model listed there
(Kimi K2 family, Llama, Qwen, etc.) can be driven through Glue without a
separate SDK.

```sh
export NVIDIA_API_KEY=nvapi-...
```

```go
import (
	"github.com/erain/glue"
	"github.com/erain/glue/providers/nvidia"
)

agent := glue.NewAgent(glue.AgentOptions{
	Provider: nvidia.New(nvidia.Options{}),
	Model:    "moonshotai/kimi-k2.6",
})
```

The model id matches the `org/name` path on build.nvidia.com (e.g.
`moonshotai/kimi-k2.6`, `meta/llama-3.3-70b-instruct`). Cold-start latency
on Kimi K2 can reach tens of seconds for the first chunk; configure your
HTTP client and context timeouts accordingly.

## Quickstart: OpenRouter

The `providers/openrouter` package speaks the OpenAI-compatible API at
[`openrouter.ai`](https://openrouter.ai), which aggregates many upstream
model providers behind a single endpoint. The meta-route `openrouter/free`
auto-picks a free underlying model — handy for tests and examples.

```sh
export OPENROUTER_API_KEY=sk-or-v1-...
```

```go
import (
	"github.com/erain/glue"
	"github.com/erain/glue/providers/openrouter"
)

agent := glue.NewAgent(glue.AgentOptions{
	Provider: openrouter.New(openrouter.Options{}),
	Model:    "openrouter/free",
})
```

The provider sends `HTTP-Referer` and `X-Title` attribution headers by
default; override them via `Options.Headers` for your own application.
OpenRouter emits SSE comment-line keep-alives during cold routing — the
provider drops them silently — so first-byte latency may be a few seconds
even when the underlying model is fast.

### Streaming events

`Session.Subscribe` registers a session-scoped handler that fires on every
loop event for every prompt run on that session. `glue.WithEvents` registers
a per-prompt handler that fires alongside it.

For the two most common cases — mirror text deltas and log tool starts
to a writer — use the convenience options:

```go
_, err := session.Prompt(ctx, "Stream a haiku about glue.",
	glue.WithStreamWriter(os.Stdout),
	glue.WithToolLogger(os.Stderr),
)
```

`WithStreamWriter` writes `EventTextDelta.Delta` straight to the writer;
`WithToolLogger` emits `[tool] <name>\n` on `EventToolStart`. Both nil-safe
and silently drop writer errors. They compose additively with `WithEvents`
and each other — adding one does not displace any other handler.

For richer formatting, use `WithEvents` directly:

```go
unsubscribe := session.Subscribe(func(e glue.Event) {
	if e.Type == glue.EventTextDelta {
		fmt.Print(e.Delta)
	}
})
defer unsubscribe()

_, err := session.Prompt(ctx, "Stream a haiku about glue.")
if err != nil {
	log.Fatal(err)
}
```

### Per-prompt overrides

```go
result, err := session.Prompt(ctx, "Be concise.",
	glue.WithModel("gemini-2.5-pro"),
	glue.WithSystemPrompt("Reply in five words or fewer."),
	glue.WithMaxTurns(4),
)
```

### Provider failover

`glue.WithFailover(provs...)` returns a Provider that tries each
underlying provider in order until one accepts a Stream — useful when
your CLI agent supports multiple LLM backends and you want it to skip
providers whose API keys aren't set rather than fail. Pre-filter via
the small registry under `providers`:

```go
import (
	"github.com/erain/glue"
	"github.com/erain/glue/providers"
	_ "github.com/erain/glue/providers/gemini"      // registers "gemini"
	_ "github.com/erain/glue/providers/nvidia"      // registers "nvidia"
	_ "github.com/erain/glue/providers/openrouter"  // registers "openrouter"
)

var provs []glue.Provider
for _, name := range []string{"nvidia", "openrouter", "gemini"} {
	if !providers.KeyAvailable(name) {
		continue
	}
	p, _, _, err := providers.New(name)
	if err == nil {
		provs = append(provs, p)
	}
}
agent := glue.NewAgent(glue.AgentOptions{
	Provider: glue.WithFailover(provs...),
	Model:    "", // let each provider use its DefaultModel
})
```

`WithFailover` only falls through *before* the first event commits to
the consumer (Stream error, immediate `ProviderEventError`, or empty
stream). Once any non-error event is observed, it commits to that
provider for the rest of the turn. All-providers-failed surfaces as a
typed `*glue.FailoverError` with per-provider attempts.

### Roles

A role is a named instruction profile with an optional model override.
Pass roles via `AgentOptions.Roles` or load them from
`<WorkDir>/roles/*.md` with simple `name:` / `description:` / `model:`
frontmatter.

```go
agent := glue.NewAgent(glue.AgentOptions{
	Provider: gemini.New(gemini.Options{}),
	Model:    "gemini-2.5-flash",
	Roles: []glue.Role{
		{Name: "reviewer", Model: "gemini-2.5-pro", Instructions: "Review for SQL safety."},
		{Name: "writer", Instructions: "Write in plain English."},
	},
	Role: "writer", // agent default
})

session, _ := agent.Session(ctx, "review", glue.WithSessionRole("reviewer"))
result, _ := session.Prompt(ctx, "Review this PR.", glue.WithRole("reviewer"))
```

Effective role precedence: `WithRole` (call) > `WithSessionRole` (session)
> `AgentOptions.Role` (agent). Effective model precedence: `WithModel`
(call) > effective role's `Model` > `AgentOptions.Model`. Unknown role
names return a typed error.

### Project context and skills

Set `AgentOptions.WorkDir` to enable Markdown context discovery:

- `<WorkDir>/AGENTS.md` is appended to the system prompt for every prompt
  on the agent's sessions (missing file is non-fatal).
- `<WorkDir>/.agents/skills/<name>/SKILL.md` is loaded as a `glue.Skill`
  with optional `name:` and `description:` frontmatter.

```go
agent := glue.NewAgent(glue.AgentOptions{
	Provider: gemini.New(gemini.Options{}),
	Model:    "gemini-2.5-flash",
	WorkDir:  ".",
})
session, _ := agent.Session(ctx, "skills")
result, err := session.Skill(ctx, "triage", map[string]int{"issue": 12})
```

`Session.Skill` renders the skill instructions, appends the args as
formatted JSON, and runs the result through `Session.Prompt`. Unknown skill
names return a typed error. Skills supplied via `AgentOptions.Skills` win on
name collision over disk-discovered skills.

### Versioned prompts

`prompts.NewCatalog(fsys, dir, defaultVersion)` wraps an `embed.FS` of
`<version>.md` files so agents can A/B-test prompts and roll back without
rebuilding history. Unknown versions return an error that lists every
available version verbatim — silent fallback would hide A/B test
misconfiguration.

```go
import (
	"embed"

	"github.com/erain/glue/prompts"
)

//go:embed prompts/*.md
var promptFS embed.FS

cat, err := prompts.NewCatalog(promptFS, "prompts", "v2")
if err != nil { /* default version must exist at construction time */ }

systemPrompt, err := cat.Get("v1") // or cat.Get("") for the default
```

The catalog is read-only and concurrency-safe. Templating and variable
substitution are intentionally out of scope; rendering is the caller's
job.

### Structured JSON results

```go
var out struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

_, err := session.PromptJSON(ctx, "Return a project name and count.", &out)
```

`PromptJSON` augments the prompt with JSON-only instructions and sets
`response_mime_type: application/json` on the provider request. Pass
`glue.WithJSONSchema(schema)` to forward an explicit JSON Schema (Gemini:
`response_json_schema`). V1 validation is JSON decoding into the caller's Go
type.

### Tools

`glue.NewTool[Args]` decodes `ToolCall.Arguments` into a typed Go value
before invoking the executor, so most tools no longer need a manual
`json.Unmarshal`. Pair it with `glue.TextResult` / `glue.ErrorResult` for
the result side:

```go
type weatherArgs struct {
	City string `json:"city"`
}

weather := glue.NewTool[weatherArgs](
	glue.ToolSpec{
		Name:        "weather",
		Description: "Lookup current weather for a city.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": { "city": { "type": "string" } },
  "required": ["city"]
}`),
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

Malformed arguments surface to the model as an error `ToolResult` rather
than crashing the loop. Schema generation from `Args` is intentionally
out of scope; supply `Parameters` explicitly.

## Testing without Gemini

The `glue.Provider` interface is small, so tests can drive sessions with a
fake provider — no credentials required:

```go
type fakeProvider struct{}

func (fakeProvider) Stream(_ context.Context, _ glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	events := make(chan glue.ProviderEvent, 3)
	events <- glue.ProviderEvent{Type: glue.ProviderEventStart}
	events <- glue.ProviderEvent{Type: glue.ProviderEventTextDelta, Delta: "hello"}
	events <- glue.ProviderEvent{Type: glue.ProviderEventDone}
	close(events)
	return events, nil
}

func ExampleSession_Prompt() {
	ctx := context.Background()
	agent := glue.NewAgent(glue.AgentOptions{Provider: fakeProvider{}})
	session, _ := agent.Session(ctx, "test")
	result, _ := session.Prompt(ctx, "say hi")
	fmt.Println(result.Text)
	// Output: hello
}
```

The repository's own tests (`glue/agent_test.go`, `loop/run_test.go`,
`loop/tool_exec_test.go`) use this pattern.

## Run the tests

```sh
go build ./...
go vet ./...
go test ./...
```

CI runs the same commands on every PR. The Gemini provider has a gated live
smoke test:

```sh
GEMINI_API_KEY=... go test ./providers/gemini -run Live
```

## Agents

Real agents built on the harness live under `agents/` (peer of the harness
itself), not `examples/` (which holds tutorial-grade demos only).

- [`agents/glue-review`](agents/glue-review) — a free, local
  pre-push branch reviewer. Reads the diff against `main`, deep-reads
  files when context demands it, and posts **one** sticky GitHub
  comment per PR — a short headline, ≤ 5 severity bullets, and a
  fenced ` ```markdown ` fix-instruction block downstream coding
  agents (Claude Code, Codex, Cursor, Aider, …) can paste and act on.
  Defaults to OpenRouter's free `inclusionai/ring-2.6-1t:free`; flags
  swap to NVIDIA `build.nvidia.com` or Gemini, with automatic
  provider failover.

  As a CLI:

  ```sh
  export OPENROUTER_API_KEY=sk-or-v1-...
  go run ./agents/glue-review              # review current branch vs main
  go run ./agents/glue-review --provider nvidia
  ```

  As a GitHub Action — drop into any repo:

  ```yaml
  - uses: erain/glue/agents/glue-review@main
    with:
      openrouter-api-key: ${{ secrets.OPENROUTER_API_KEY }}
  ```

  See [`agents/glue-review/README.md`](agents/glue-review/README.md)
  for the full input/output contract and the eval evidence behind
  the current prompt.

## Examples

- [`examples/local-agent`](examples/local-agent) is a small Gemini-backed
  tutorial CLI that registers a `local_time` tool, streams text to stdout,
  and persists sessions through `stores/file`. It's the shortest path from
  zero to "Glue agent that calls a Go function":

  ```sh
  export GEMINI_API_KEY=...
  go run ./examples/local-agent --prompt "Use local_time for America/Toronto." --id demo
  ```

## CLI

A thin local CLI is built on the same library API:

```sh
go run ./cmd/glue run --prompt "Say hi" --id local-dev --store .glue/sessions
```

Flags:

- `--id` — session id (default `"default"`).
- `--prompt` — prompt text (required).
- `--model` — model id or `gemini/<model>` (default `gemini-2.5-flash`).
- `--store` — file session store directory (default `.glue/sessions`).
- `--env` — `.env` file to load before reading `GEMINI_API_KEY`. Repeatable;
  shell environment wins on conflict.

The CLI streams text deltas to stdout, persists sessions through
`stores/file`, and uses `WorkDir="."` so `AGENTS.md`, `.agents/skills`, and
`roles/` discovery work from the invocation directory. Errors return a
non-zero exit code; missing `GEMINI_API_KEY` produces a clear message.

### Standard flags for downstream agents

Agents that ship their own CLI binary share the same six flags
(`--provider`, `--model`, `--id`, `--store`, `--work`, `--max-turns`).
`cli.RegisterStandardFlags` wires them onto a `flag.FlagSet` and returns
a getter:

```go
import "github.com/erain/glue/cli"

fs := flag.NewFlagSet("my-agent", flag.ContinueOnError)
get := cli.RegisterStandardFlags(fs, nil) // pass &cli.StandardConfig{...} to override defaults
fs.Parse(os.Args[1:])
cfg := get() // cfg.Provider, cfg.Model, cfg.ID, cfg.Store, cfg.Work, cfg.MaxTurns
```

`--provider` accepts a comma-separated list (e.g. `nvidia,openrouter,gemini`)
which agents are expected to handle by chaining the providers registry
with `glue.WithFailover`.

## Adding a provider

Glue's `Provider` interface is small. See
[`docs/provider-guide.md`](docs/provider-guide.md) for the contract and
common pitfalls, and [`examples/echo-provider`](examples/echo-provider)
for the shortest possible runnable implementation.

## Roadmap

P0–P2 are shipped: the reusable loop, public `Agent` / `Session` API,
file-backed sessions, structured JSON, skills, roles, the CLI runner,
parallel tool execution, opt-in context compaction, the shell/filesystem
tool extension packages (`tools/fs`, `tools/git` per
[`docs/adr/0003-shell-filesystem-tools.md`](docs/adr/0003-shell-filesystem-tools.md)),
the provider plugin guide ([`docs/provider-guide.md`](docs/provider-guide.md)),
and the GitHub issue automation workflow
([`docs/issue-automation.md`](docs/issue-automation.md)). The current
focus is hardening through dogfooding `agents/glue-review` and closing
the agent-ergonomics wishlist (typed tools, provider failover, prompt
catalog, stream writer, standard flags) plus the broader gaps in
[`docs/flue-gap-analysis.md`](docs/flue-gap-analysis.md): multi-target
deployment, sandbox connectors, subagent orchestration, MCP. See
[`docs/project-plan.md`](docs/project-plan.md) and the project tracker
(#1) for the next recommended issue.
