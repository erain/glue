# glue

[![CI](https://github.com/erain/glue/actions/workflows/ci.yml/badge.svg)](https://github.com/erain/glue/actions/workflows/ci.yml)

Glue is a Go agent harness for building local and programmable agents,
inspired by [Flue](https://github.com/withastro/flue) and
[pi-mono](https://github.com/badlogic/pi-mono). It is built around a reusable
provider-agnostic agent loop, a code-first `Agent` / `Session` API, and an
initial Gemini provider.

GitHub issues are the source of truth for the roadmap and implementation
order:

- Project tracker: <https://github.com/erain/glue/issues/1>
- Design doc: [docs/design.md](docs/design.md)
- Project plan: [docs/project-plan.md](docs/project-plan.md)
- Contributor workflow: [CONTRIBUTING.md](CONTRIBUTING.md)

## Status

P0 is complete: normalized loop types, reusable agent loop with deterministic
sequential tool execution, public `Agent` / `Session` API, and a Gemini text
streaming provider. Function calling, file-backed sessions, structured JSON,
skills, roles, and a CLI runner are tracked under P1 in the project plan.

## Install

```sh
go get github.com/erain/glue
```

The module path is `github.com/erain/glue`; subpackages are
`github.com/erain/glue/loop`, `github.com/erain/glue/providers/gemini`, and
`github.com/erain/glue/stores/file`.

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
continues the conversation. File-backed sessions land in P1.

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

## Examples

- [`examples/glue-review`](examples/glue-review) — a free, local pre-push
  branch reviewer. Reads the diff against `main`, deep-reads files when
  context demands it, and emits structured review notes. Defaults to
  NVIDIA's free Kimi K2.6; flags swap to OpenRouter or Gemini. This is
  the recommended starting point if you want to see what a real Glue
  agent looks like.

  ```sh
  export NVIDIA_API_KEY=nvapi-...
  go run ./examples/glue-review            # review current branch vs main
  go run ./examples/glue-review --provider openrouter
  ```

- [`examples/local-agent`](examples/local-agent) is a smaller Gemini-backed
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

## Adding a provider

Glue's `Provider` interface is small. See
[`docs/provider-guide.md`](docs/provider-guide.md) for the contract and
common pitfalls, and [`examples/echo-provider`](examples/echo-provider)
for the shortest possible runnable implementation.

## Roadmap

P2 covers parallel tool execution, context compaction, an opt-in shell/
filesystem tool design, a provider plugin guide, and the GitHub issue
automation workflow. See [docs/project-plan.md](docs/project-plan.md) and
the project tracker (#1).
