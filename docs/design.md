# Glue Design

Glue is a Go agent harness for building local and programmable agents. It is
inspired by Flue's agent/session/skill model and pi-mono's reusable agent loop:
providers stream assistant events, the loop executes tools, tool results are
fed back into the conversation, and the process repeats until the model stops.

This document is the canonical design reference. GitHub issues are the source
of truth for implementation order and status after the initial bootstrap.

## Goals

- Provide a code-first Go library for defining agents, sessions, tools, skills,
  and providers.
- Make the pi-mono-style agent loop a reusable module that is independent of
  Gemini, CLI code, stores, and filesystem conventions.
- Support local CLI agents as a first-class use case without making the CLI the
  core abstraction.
- Start with Gemini as the only built-in provider while keeping provider
  integration pluggable.
- Persist local sessions to files so CLI sessions can resume.
- Treat documentation and verification as required work for every issue.

## Non-Goals For P0/P1

- No sandboxing, shell execution, container runtime, or remote connector
  in the core `glue` package. Shell and filesystem tools land in a
  dedicated extension package per [`adr/0003-shell-filesystem-tools.md`](adr/0003-shell-filesystem-tools.md);
  the core stays free of POSIX coupling.
- No dynamic Go plugin loading.
- No MCP integration.
- No HTTP server or deploy target.
- (P0/P1 only) No automatic context compaction; opt-in compaction lands
  in P2 — see [`adr/0002-context-compaction.md`](adr/0002-context-compaction.md).
- (P0/P1 only) No parallel tool execution; opt-in `RunRequest.Parallel`
  lands in P2 (#17).

## Package Boundaries

The module path is `github.com/erain/glue`.

- `glue`: public library surface. Owns `Agent`, `Session`, options, tools,
  skills, roles, and store interfaces.
- `loop`: provider-agnostic agent loop. Owns turn execution, provider event
  consumption, tool execution, transcript append behavior, and loop events.
- `providers/gemini`: Gemini provider implementation using
  `google.golang.org/genai`.
- `stores/file`: file-backed JSON session store with atomic writes.
- `cmd/glue`: local CLI runner built on top of the public library.

The dependency direction is intentionally narrow:

```text
cmd/glue -> glue -> loop
glue    -> providers/gemini only through explicit user construction
glue    -> stores/file only through explicit user construction
loop    -> no dependency on glue, providers, stores, CLI, or docs
```

## Core Types

Glue uses normalized provider-neutral messages.

- `Message`: user, assistant, or tool result transcript entry.
- `ContentPart`: text, thinking, image, or tool call content.
- `ToolCall`: assistant request to invoke a named tool with JSON arguments.
- `ToolResult`: result returned to the model for a tool call.
- `Tool`: name, description, JSON Schema parameters, and Go executor.
- `Provider`: model backend that streams normalized assistant events.
- `Event`: lifecycle event emitted by the loop and sessions.
- `Store`: persistence interface for session transcripts.

Provider-specific fields may be kept in metadata, but the loop must not depend
on provider-specific event or payload shapes.

## Agent Loop

The `loop` package is the architectural center. Its job is to run a transcript
until the provider stops or the context is canceled.

1. Start with a system prompt, existing messages, available tools, and provider.
2. Ask the provider to stream an assistant response.
3. Emit text/tool/lifecycle events as provider events arrive.
4. Append the final assistant message to the transcript.
5. If the assistant requested tools, execute the requested tools.
6. Append tool result messages in deterministic order.
7. Repeat from step 2 until no tool calls remain.

The concrete P0 entry point is `loop.Run(ctx, loop.RunRequest)`. It returns a
`loop.RunResult` containing both the full transcript and the messages produced by
that run. `RunRequest.Emit` receives event snapshots for callers such as sessions
and CLIs.

By default the loop executes tool calls sequentially in source order, which
keeps the transcript deterministic and simple to verify. Tool arguments
must be JSON objects; missing arguments are treated as `{}`. Unknown
tools, invalid arguments, and executor errors are represented as error
tool results that are visible to the model, rather than crashing the loop.

Setting `RunRequest.Parallel = true` opts callers into concurrent
execution: tool calls within a single assistant message are dispatched in
parallel goroutines, but `EventToolStart`, the executor invocations'
visible side effects on the transcript, and `EventToolEnd` events are
still ordered by assistant source position so the transcript is identical
to a sequential run that took the same set of inputs. Sequential remains
the default until callers explicitly opt in.

The default maximum loop length is 32 assistant turns to prevent accidental
infinite tool cycles. Callers can override this with `RunRequest.MaxTurns`.

The loop must support:

- text-only responses
- one or more tool calls
- repeated tool turns
- provider errors
- tool errors
- invalid tool arguments
- context cancellation
- event streaming for CLI output

## Public Library API

The public API is code-first. A user defines an agent in Go code, configures a
provider, registers tools, chooses a store, and opens sessions.

Target P0 shape:

```go
agent := glue.NewAgent(glue.AgentOptions{
    Provider: gemini.New(gemini.Options{APIKey: os.Getenv("GEMINI_API_KEY")}),
    Model:    "gemini-2.5-flash",
    Tools:    []glue.Tool{weatherTool},
})

session, err := agent.Session(ctx, "local-dev")
if err != nil {
    return err
}

result, err := session.Prompt(ctx, "Use the weather tool for Toronto.")
```

Sessions are in-memory in P0. `Session.Subscribe(func(glue.Event))` registers a
session-level event handler that fires for every prompt run on that session,
and `glue.WithEvents` registers a per-prompt event handler that fires
alongside (both receive the same events for the prompt). The current
`AgentOptions` exposes `Store` (typed [`Store`](#persistence) interface) for
durable session state and reserves `WorkDir`, `Role`, and `Roles` for #13 and
#14 so the public type stays stable as those features land. The default
in-memory behavior is preserved when `Store` is nil.

`PromptJSON(ctx, prompt, outPtr, opts...)` requests structured output and
unmarshals the assistant's final text into a caller-provided non-nil pointer. It
adds JSON-only instructions to the prompt and sets provider options compatible
with Gemini structured output:

- `response_mime_type: application/json`
- `response_json_schema` when `glue.WithJSONSchema(...)` is provided

V1 validation is intentionally limited to JSON decoding into the caller's Go
type. Full JSON Schema validation is out of scope for the first structured
result API.

## Gemini Provider

The first provider package is `providers/gemini`, implemented with
`google.golang.org/genai`.

Gemini behavior:

- uses an explicit API key or `GEMINI_API_KEY`
- accepts a model from `glue.AgentOptions.Model`, per-call `glue.WithModel`, or
  provider default model
- converts text user and assistant messages to Gemini `Content`
- streams Gemini text deltas (and thinking deltas) as normalized provider events
- maps final stop reason, response id metadata, model version, and usage when
  reported by the SDK
- converts Glue tools to Gemini function declarations
- converts Gemini function calls to normalized tool calls
- converts Glue tool result messages to Gemini function responses, grouping
  consecutive tool-role messages into a single Gemini content per Glue turn

Offline provider tests cover message conversion, config conversion, finish
tool conversion, function response conversion, finish reason mapping, loop
integration, and the live test skip path. Live smoke testing is gated:

```sh
GEMINI_API_KEY=... go test ./providers/gemini -run Live
```

## CLI Runner

The CLI is first-class but thin. It exercises the same library APIs that
applications use.

Target command shape:

```sh
glue run --id <session-id> --prompt "..." --model gemini/<model> --store .glue/sessions --env .env
```

The built-in runner supports the default Gemini-backed agent only. It streams
text deltas to stdout, persists sessions through `stores/file`, loads repeatable
`.env` files, and uses `AgentOptions.WorkDir="."` for AGENTS.md, skills, and
roles. Dynamic Go source loading, HTTP serving, and build/deploy targets remain
out of scope.

## Context, Skills, And Roles

Glue borrows Flue's Markdown-driven context model:

- `AgentOptions.WorkDir` enables local context discovery.
- `AGENTS.md` contributes project instructions to the system prompt.
- `.agents/skills/<name>/SKILL.md` defines reusable skill instructions.
- `Session.Skill(ctx, name, args, opts...)` renders the named skill with optional
  JSON args and runs it through `Session.Prompt`.
- skills support simple frontmatter: `name:` and `description:`.
- roles can be supplied through `AgentOptions.Roles` or loaded from
  `roles/*.md` under `AgentOptions.WorkDir`.
- roles support simple frontmatter: `name:`, `description:`, and `model:`.
- effective role precedence is call role (`glue.WithRole`) > session role
  (`glue.WithSessionRole`) > agent role (`AgentOptions.Role`).
- model precedence is explicit call model (`glue.WithModel`) > effective role
  model > agent model.

Skills and roles are layered above the loop by the `glue` package. The loop only
sees the final system prompt, messages, tools, and provider.

## Persistence

Glue exposes a small `Store` interface for durable session state. `AgentOptions`
accepts a store, and `Session.Prompt` saves the transcript after each run.

The initial durable implementation is `stores/file`. It writes normalized
session state as JSON and uses temp-file-plus-rename for atomic updates. Session
ids are URL-escaped into `<id>.json` files below the configured directory.

Persisted data includes:

- session id
- messages
- metadata
- timestamps
- provider and model identifiers inside messages
- tool calls and tool results inside messages
- usage inside messages when available

## Context Compaction

Long-running sessions can exceed provider context windows. Glue exposes an
explicit, opt-in [`Compactor`](compactor.go) interface that callers wire
through `AgentOptions.Compactor` and `AgentOptions.CompactionThreshold`.
The agent runs the compactor before every prompt whenever the in-memory
transcript has more than the threshold number of messages; the compactor's
output replaces the in-memory transcript before the loop runs and is
persisted by the next save.

The first built-in policy is `KeepRecentMessages(n)`, which keeps the last
`n` messages and replaces everything older with a single assistant-role
marker carrying `Metadata["compaction"] = "keep_recent"`. Token-aware
compaction can be added later as a separate `Compactor` implementation;
see [`adr/0002-context-compaction.md`](adr/0002-context-compaction.md).

## Verification Policy

Every issue must specify verification before work starts. At minimum:

- run `go test ./...` after code changes
- add fake-provider tests for loop behavior
- use offline conversion tests for provider payload mapping
- gate live Gemini tests behind `GEMINI_API_KEY`
- update docs when public behavior, architecture, or project status changes

The full per-issue contributor protocol — branching, PR conventions, closing
comments, and tracker updates — is documented in [`../CONTRIBUTING.md`](../CONTRIBUTING.md).
