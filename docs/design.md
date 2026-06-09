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

## Non-Goals

The non-goals list below was revised in
[`adr/0005-foundation-expansion.md`](adr/0005-foundation-expansion.md)
when work began on the Peggy long-running-agent milestone (tracker
[#110](https://github.com/erain/glue/issues/110)). Items lifted by
ADR-0005 are now in scope behind framework interfaces; items below are
what remains firm.

- **Sandboxing / containerization.** No process, namespace, or
  container isolation in glue. Hosts that need it implement the
  `Executor` interface against docker / firecracker / their own
  sandbox helper. Glue ships a local `Executor` only.
- **No dynamic Go plugin loading.**
- **No deploy target.** `cmd/glue serve` (introduced under M3 of
  tracker [#110](https://github.com/erain/glue/issues/110)) is a local
  daemon for channel adapters to attach to; it is not a hosted
  service abstraction.
- **No implicit parallel tool execution.** `RunRequest.Parallel`
  remains opt-in and preserves transcript order (#17, shipped).
- **No channel concepts in core `glue`.** Telegram, Slack, web,
  IDE-attach, and similar live in product packages (e.g.
  `agents/peggy`) and bind to glue Sessions through the daemon
  protocol or in-process API. Glue does not learn about them.
- **No default UI or default policy in core `glue`.** Every product
  concern enters glue only as an interface the host fills in. This is
  the rule that protects glue's purity through the Peggy expansion;
  see ADR-0005 §1.

For the historical "lifted" non-goals — shell execution, write-side
filesystem, MCP integration, HTTP server, the trigger for automatic
compaction — see ADR-0005 for which interface gates each one.

## Long-Running Agents

Glue's original `0.x` shape targeted short-lived, single-purpose agents
(see `agents/glue-review`: emit one PR comment and exit). The Peggy
milestone (tracker [#110](https://github.com/erain/glue/issues/110))
extends glue toward a different category: long-running, multi-channel,
memory-bearing agents that may live for days. The framework expansions
required to support that category — Codex provider, summarizing
compactor + FTS5 session search, daemon mode, channel adapter pattern,
Executor / Permission / Hook interfaces, and MCP client integration are
designed in ADRs 0005-0011 and the milestone ADRs that follow them. The
Executor / Permission / Hook trio is specified in
[`adr/0009-executor-permission-hook.md`](adr/0009-executor-permission-hook.md).
The local daemon protocol is specified in
[`adr/0010-daemon-protocol.md`](adr/0010-daemon-protocol.md).
MCP client integration is specified in
[`adr/0011-mcp-client-integration.md`](adr/0011-mcp-client-integration.md).
The architectural rule that holds the expansion together is in
ADR-0005 §1: every product concern enters glue only as an interface the
host fills in.

**Channels** (Telegram, future TUI / web / IDE) live in product
packages under `agents/<name>/channels/<channel>` per
[`adr/0008-channel-adapter.md`](adr/0008-channel-adapter.md). Core
`glue` stays channel-blind — channels call into `Session.Prompt` and
`Session.Subscribe` the same way the CLI does. Session-id namespacing
(`<channel>:<id>`) keeps CLI sessions, channel sessions, and the
curated memory session distinct in a single store.

## Package Boundaries

The module path is `github.com/erain/glue`.

- `glue`: public library surface. Owns `Agent`, `Session`, options, tools,
  skills, roles, and store interfaces.
- `loop`: provider-agnostic agent loop. Owns turn execution, provider event
  consumption, tool execution, transcript append behavior, and loop events.
- `providers/gemini`: Gemini provider implementation using
  `google.golang.org/genai`.
- `providers/nvidia`: OpenAI-compatible provider for the NVIDIA build
  inference API (`integrate.api.nvidia.com`), implemented over `net/http`
  with no third-party SDK dependency.
- `providers/openrouter`: OpenAI-compatible provider for OpenRouter
  (`openrouter.ai/api/v1`), aggregator of many upstream models. Sends
  attribution headers (`HTTP-Referer`, `X-Title`) and tolerates
  comment-line SSE keep-alives during cold routing.
- `providers/codex`: ChatGPT-subscription-authenticated provider that
  routes through the Codex Responses endpoint
  (`chatgpt.com/backend-api/codex/responses`). Reuses the upstream
  Codex CLI's `auth.json` (the user runs `codex login` once outside
  glue). Designed in ADR 0006; subscription-auth fragility is
  quarantined to this package.
- `providers`: driver-style registry (`Register`, `New`, `Lookup`,
  `Known`, `KeyAvailable`). Each provider sub-package self-registers
  via `init()` so importing `_ "github.com/erain/glue/providers/<name>"`
  makes that name resolvable. Holds factory functions, not constructed
  providers, so registration is cheap and credential-free.
- `stores/file`: file-backed JSON session store with atomic writes.
- `stores/sqlite`: SQLite-backed `Store` with FTS5 over message text;
  implements the optional `Searcher` capability for cross-session
  retrieval. Designed in
  [`adr/0007-memory-layer.md`](adr/0007-memory-layer.md). Pure-Go via
  `modernc.org/sqlite`; `stores/file` stays as the simple option.
- `tools/fs`: filesystem tool factories — `SafeJoin`, `Truncate`,
  `Blocklist`, plus ready-to-register `ReadFileTool`, `FileWrite`,
  `FileEdit`, and the read-only navigation tools `ListDirTool`,
  `FindTool`, `GrepTool`. Outside the core package per ADR 0003 so the
  harness stays free of POSIX coupling.
- `tools/git`: git tool factories — `RunGit`, `BuildPathspec`,
  `DiffBranchTool`, `LogBranchTool`. Shells out to the system `git`
  binary; no Git library dependency.
- `tools/shell`: a permission-gated `shell_exec` tool that runs
  argv-style commands through `glue.Executor` (default local process)
  with a binary allowlist, timeout, and output caps.
- `tools/coding`: assembles the reusable local coding-agent bundle
  (`read_file`, `write_file`, `edit_file`, `list_dir`, `find_files`,
  `grep`, `shell_exec`, git helpers) over the fs/git/shell/executor
  primitives. See [`adr/0012-sdk-coding-agent-peggy-boundary.md`](adr/0012-sdk-coding-agent-peggy-boundary.md).
- `tools/mcp`: a Model Context Protocol client (stdio / Streamable HTTP)
  that maps MCP server tools to permission-gated `glue.Tool` values. See
  [`adr/0011-mcp-client-integration.md`](adr/0011-mcp-client-integration.md).
- `cmd/glue`: local CLI runner and HTTP+SSE daemon (`run` / `serve` /
  `connect`) built on top of the public library.

The dependency direction is intentionally narrow:

```text
cmd/glue -> glue -> loop
glue    -> providers/gemini only through explicit user construction
glue    -> providers/nvidia only through explicit user construction
glue    -> providers/openrouter only through explicit user construction
glue    -> providers/codex only through explicit user construction
glue    -> stores/file only through explicit user construction
glue    -> stores/sqlite only through explicit user construction
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

When the loop exits because the turn budget (`RunRequest.MaxTurns`) is
exhausted while the assistant still has pending tool calls, it returns
the partial transcript with the last assistant message tagged
`StopReason = StopReasonMaxTurns`. Callers can use this to distinguish
budget exhaustion from a natural stop or provider truncation —
e.g. retry the prompt with a higher budget rather than treating it as a
model error.

## Public Library API

The public API is code-first. A user defines an agent in Go code, configures a
provider, registers tools, chooses a store, and opens sessions.

Target P0 shape:

```go
agent := glue.NewAgent(glue.AgentOptions{
    Provider: gemini.New(gemini.Options{APIKey: os.Getenv("GEMINI_API_KEY")}),
    Model:    "gemini-3.1-pro-preview",
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

`glue.SubagentTool` adapts a child `*glue.Agent` into a normal `glue.Tool` for
delegation patterns. Each tool call forwards only the explicit prompt argument
into a fresh child session, using `SubagentOptions.SessionID` as an optional
session-id prefix, and returns the child final text as the tool result. Child
prompt failures are visible to the parent model as error tool results; context
cancellation and deadlines still abort the parent loop.

`Agent.PursueGoal` is the goal-loop primitive — glue's "loop engineering" /
`/goal`. A `GoalSpec` objective drives an autonomous outer loop that wraps the
single-turn loop: a planner decomposes the objective into a verifiable
checklist, then each iteration runs a **maker** (a fresh session seeded from the
durable checklist — a Ralph-style loop, so memory lives in the checklist, not a
growing transcript) followed by a separate **checker** session that audits
against real evidence via `Session.PromptJSON` and decides completion. Guardrails
(max iterations, no-progress detection, token budget) bound it, and `GoalSpec.Emit`
streams progress. The maker≠checker split keeps the writer from grading its own
homework. See [ADR-0016](adr/0016-goal-loop.md). A non-empty `GoalSpec.Checklist`
seeds the loop and skips planning — that is how a paused goal resumes from its
last verified state. When the agent has a `Store`, `PursueGoal` also
checkpoints a durable [`GoalRecord`](../goal_store.go) (objective, status,
checklist, iterations, usage) as namespaced `glue/goal:*` metadata on the
`SessionPrefix` session — a context cancellation persists as `paused` — and
`Agent.LoadGoal` / `Agent.ListGoals` read records back; `GoalSpec.StartIteration`
lets a resumed run continue iteration numbering so maker/checker sessions stay
fresh. The TUI surfaces all of this as `/goal <objective>` with `status` /
`pause` / `resume` (continues the most recent unfinished record, even in a new
process) / `list` / `clear` subcommands, a live checklist card in the
transcript, and a `◎ goal` status-bar segment. `/goal -w` adds worktree
isolation: cmd/glue (which owns the git plumbing per ADR-0012) creates
`.glue/worktrees/<goal-id>` on branch `goal/<id>`, rebuilds the coding tool
set rooted there via `tui.Config.BuildTools`, and records the root in
`GoalSpec.WorkDir` so resume re-attaches the same worktree. The headless
`glue goal "<objective>"` subcommand runs the same loop without a TUI —
`--list` / `--resume` / `--worktree` / `--max-iterations` / `--budget`, with
the terminal status mapped to the exit code — which is what cron, CI, or a
peggy schedule invokes for unattended goals.

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
glue run --id <session-id> --prompt "..." --provider <name> --model <model> --store .glue/sessions --env .env
```

The built-in runner selects a provider from the `providers` registry via
`--provider` (`codex`, `gemini`, `nvidia`, `openrouter`; default `gemini`), so
the binary can run as a coding agent on a ChatGPT subscription (`--provider
codex`) without code changes. `--model` defaults to the selected provider's
registry default model. The runner streams text deltas to stdout, persists
sessions through `stores/file`, loads repeatable `.env` files, and uses
`AgentOptions.WorkDir="."` for AGENTS.md, skills, and roles. `--coding`
registers the `tools/coding` bundle behind local terminal permission prompts
(per [ADR-0012](adr/0012-sdk-coding-agent-peggy-boundary.md)). Dynamic Go
source loading and build/deploy targets remain out of scope.

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
`stores/sqlite` (designed in
[`adr/0007-memory-layer.md`](adr/0007-memory-layer.md)) is the alternative
backend for long-running agents that need cross-session search via FTS5.

Persisted data includes:

- session id
- messages
- metadata
- timestamps
- provider and model identifiers inside messages
- tool calls and tool results inside messages
- usage inside messages when available

Stores may optionally implement the `Searcher` capability for
cross-session retrieval; `Agent.SearchSessions` and `Session.Search`
return `ErrSearchNotSupported` when the active store does not implement
it. See ADR-0007.

## Context Compaction

Long-running sessions can exceed provider context windows. Glue exposes an
explicit, opt-in [`Compactor`](../compactor.go) interface that callers wire
through `AgentOptions.Compactor` and `AgentOptions.CompactionThreshold`.
The agent runs the compactor before every prompt whenever the in-memory
transcript has more than the threshold number of messages; the compactor's
output replaces the in-memory transcript before the loop runs and is
persisted by the next save.

Two built-in policies ship:

- **`KeepRecentMessages(n)`** — keeps the last `n` messages and
  replaces everything older with a single assistant-role marker
  carrying `Metadata["compaction"] = "keep_recent"`. No token model,
  no provider calls; the right default for short-lived agents.
- **`SummarizingCompactor`** — token-aware, summarizes older messages
  via the configured `Provider`. Designed in
  [`adr/0007-memory-layer.md`](adr/0007-memory-layer.md); supersedes
  ADR-0002's "token-aware policy can be added later" note.

## Verification Policy

Every issue must specify verification before work starts. At minimum:

- run `go test ./...` after code changes
- add fake-provider tests for loop behavior
- use offline conversion tests for provider payload mapping
- gate live Gemini tests behind `GEMINI_API_KEY`
- update docs when public behavior, architecture, or project status changes

The full per-issue contributor protocol — branching, PR conventions, closing
comments, and tracker updates — is documented in [`../CONTRIBUTING.md`](../CONTRIBUTING.md).
