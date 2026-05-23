# ADR 0009: Executor, Permission, And Hook Interfaces

## Status

Accepted. Filed under tracker
[#110](https://github.com/erain/glue/issues/110) / issue
[#141](https://github.com/erain/glue/issues/141) as the first M2 design
issue for Peggy v0.2 ("she can code").

Implementation issues that follow this ADR:

- `glue.Executor` + `LocalExecutor`.
- `glue.Permission`, `AllowAll`, `DenyAll`, `Hook`, and loop
  composition.
- `tools/shell.Exec`.
- `tools/fs.FileWrite`.
- `glue.SubagentTool`.
- `agents/peggy`: coding tools + CLI permission prompter.
- `agents/peggy/channels/telegram`: inline-keyboard permission
  prompter.

## Context

Peggy v0.1 can remember, recall, summarize long sessions, and receive
messages from the CLI or Telegram. It cannot yet do the defining M2
thing: read a problem, edit code, run tests, inspect failures, and
iterate.

That work needs three framework contracts to land together:

1. **Executor.** Shell tools need a way to run commands without baking
   `os/exec` directly into every tool. Sandboxing remains deferred, but
   the tool surface must already have a seam where a future Docker,
   Firecracker, remote, or policy-enforcing runner can plug in.
2. **Permission.** Tools with side effects must not run just because the
   model asked. The host product decides whether a specific request is
   allowed. Peggy's CLI can prompt inline; Telegram can use an inline
   keyboard; a future daemon client can route the prompt over SSE.
3. **Hook.** Hosts need a lifecycle point around tool calls for
   logging, auditing, policy overlays, metrics, or short-circuiting.
   Hooks compose with permissions: a single `shell.Exec` call first
   passes hook inspection, then permission, then execution, then
   post-hook inspection.

ADR-0005's purity rule is still the load-bearing constraint:

> Every product concern enters glue only as an interface the host fills
> in, never as a default behavior with UI, channel, or policy baked in.

That means core `glue` may define the interfaces and loop composition,
but it must not render a permission prompt, decide a policy, ship a
default channel UX, or claim to sandbox commands.

## Decision

### 1. `Executor`

Core `glue` defines the command-execution contract:

```go
type Executor interface {
    Run(ctx context.Context, cmd ExecCommand) (ExecResult, error)
}

type ExecCommand struct {
    Argv           []string
    Dir            string
    Env            []string
    Stdin          io.Reader
    Timeout        time.Duration
    MaxOutputBytes int
}

type ExecResult struct {
    Stdout    []byte
    Stderr    []byte
    ExitCode  int
    TimedOut  bool
    Truncated bool
}
```

`ExecCommand.Argv` is required and is always argv-style, never a shell
string. `Dir` is the working directory. `Env` is an explicit pass-through
subset; nil means inherit none beyond whatever minimum the executor
requires. `Timeout == 0` means no executor-level timeout.
`MaxOutputBytes == 0` means the executor default.

`LocalExecutor{}` is the default implementation and wraps `os/exec`.
It is not a sandbox. It should:

- reject an empty `Argv`;
- honor `ctx` cancellation and `Timeout`;
- capture stdout and stderr separately;
- truncate each stream to `MaxOutputBytes` or a documented default;
- return a populated `ExecResult` for process exit status, including
  non-zero exits;
- return an error for setup failures, context cancellation, and other
  non-process failures.

Future sandboxed or remote executors implement the same interface. Tool
code and loop code must not need to change when such an executor lands.

### 2. `Permission`

Core `glue` defines the per-tool permission contract:

```go
type Permission interface {
    Decide(ctx context.Context, req PermissionRequest) (PermissionDecision, error)
}

type PermissionRequest struct {
    Tool      string
    Action    string
    Target    string
    Args      json.RawMessage
    SessionID string
}

type PermissionDecision struct {
    Allow       bool
    Reason      string
    RememberFor RememberScope
}

type RememberScope int

const (
    RememberNever RememberScope = iota
    RememberSession
    RememberSessionTarget
    RememberForever
)
```

`Tool` is the model-callable tool name. `Action` is a short verb such as
`exec`, `write_file`, or `network_request`. `Target` is the human-readable
object of the action: an argv preview, a path, or an endpoint. `Args` is
the normalized JSON argument object from the tool call so richer UIs can
display details. `SessionID` is populated by `Session.Prompt` through a
new `loop.RunRequest.SessionID` field.

Glue ships no permission UI. The host supplies a `Permission`:

- Peggy CLI asks inline on stderr/stdin.
- Peggy Telegram sends an inline-keyboard confirmation.
- A future daemon emits a permission-request event and waits for a
  client reply.

Core glue may ship `AllowAll{}` and `DenyAll{}` as small test and demo
helpers. They are not product policy.

`RememberFor` is part of the decision shape because hosts need to express
"yes for this session" and similar UX choices. Core glue does not own a
permission cache in this ADR. The `Permission` implementation may cache
its own decisions before returning, and future product layers may persist
them.

### 3. Side-effect declaration on tools

`ToolSpec` gains local-only permission metadata:

```go
type ToolSpec struct {
    Name        string          `json:"name"`
    Description string          `json:"description"`
    Parameters  json.RawMessage `json:"parameters,omitempty"`

    RequiresPermission bool                     `json:"-"`
    PermissionAction   string                   `json:"-"`
    PermissionTarget   func(ToolCall) string    `json:"-"`
}
```

`RequiresPermission` defaults to false. Existing tools keep their current behavior.
Read-only tools such as `tools/fs.FileRead`, git log/diff, and Peggy's
memory tools do not require permission by default. Side-effect tools
such as shell exec, file write, network mutation, and future deploy tools
set it true.

`PermissionAction` is a short verb such as `exec` or `write_file`.
When empty, the loop uses the tool name. `PermissionTarget` formats the
normalized tool call into a human-readable target such as an argv preview
or path. When nil or empty, the loop uses a compact rendering of
`ToolCall.Arguments`.

These fields are deliberately excluded from provider JSON. They are local
loop metadata, not instructions to the model.

### 4. `Hook`

Core `glue` defines tool-call hooks:

```go
var ErrSkipTool = errors.New("glue: skip tool")

type Hook interface {
    PreTool(ctx context.Context, call ToolCall) error
    PostTool(ctx context.Context, call ToolCall, result *ToolResult) error
}
```

Hooks are registered with `AgentOptions.Hooks []Hook` and passed through
to `loop.RunRequest.Hooks`. Per-tool filtering is the hook's job: check
`call.Name` and return nil for tools the hook does not care about. Glue
does not add a routing layer.

`PreTool` runs before permission and execution. If it returns
`ErrSkipTool`, the executor is not run and the loop returns the
canonical skipped tool result:

```text
tool skipped by hook
```

with `IsError: true`. Any other `PreTool` error aborts the run and
surfaces as `StopReasonError`, matching provider and context failures.

`PostTool` runs after execution or permission denial and receives a
mutable `*ToolResult`. It may annotate metadata, redact output, or turn a
result into an error. If a post-hook returns an error, the run aborts
with `StopReasonError`.

This ADR intentionally keeps hook v1 tool-scoped. Prompt hooks,
compaction hooks, and daemon-level lifecycle hooks are additive future
extensions; M2's coding tools only need the tool-call boundary.

### 5. Loop composition

The loop owns side-effect composition so every tool does not repeat the
same boilerplate. For each normalized tool call:

1. Run `PreTool` hooks in registration order.
2. If the selected tool has `RequiresPermission == true`, build a
   `PermissionRequest` from the selected tool and normalized call, then
   call `Permission.Decide`.
3. If permission is denied, return an `IsError: true` tool result using
   `PermissionDecision.Reason` or a default denial message.
4. Run the tool's `Execute`.
5. Run `PostTool` hooks in reverse registration order, allowing each hook
   to mutate the result before it is appended to the transcript.

The new loop request fields are:

```go
type RunRequest struct {
    // existing fields...
    SessionID  string
    Permission Permission
    Hooks      []Hook
}
```

`AgentOptions` mirrors the host-owned pieces:

```go
type AgentOptions struct {
    // existing fields...
    Permission Permission
    Hooks      []Hook
}
```

`Session.Prompt` copies the agent permission and hooks into
`loop.RunRequest`, and sets `SessionID` to `s.ID()`.

The default behavior remains unchanged:

- no hooks registered means no hook work;
- nil `Permission` denies side-effect tools with a clear error result
  rather than silently allowing them;
- tools without `RequiresPermission` continue to run without a
  permission callback.

When `RunRequest.Parallel` is false, this sequence runs in assistant
source order. When `Parallel` is true, each tool call's composition chain
may run concurrently, preserving the existing guarantee that appended tool
messages and `EventToolEnd` are emitted in assistant source order. Hook
and Permission implementations used with parallel mode must therefore be
concurrency-safe.

### 6. Shell and file tools consume the contracts

`tools/shell.Exec` is the first executor-aware tool. It should be
constructed with options that include an `Executor`:

```go
type ExecOptions struct {
    Executor       glue.Executor
    WorkDir        string
    AllowedBinaries []string
    Timeout        time.Duration
    MaxOutputBytes int
}
```

The tool sets `RequiresPermission: true`, `PermissionAction: "exec"`,
and a `PermissionTarget` formatter that renders the argv. It validates
its argv and binary allowlist inside `Execute`, after the loop-owned
permission gate approves the call. If no executor is supplied, the
constructor uses `glue.LocalExecutor{}`.

`tools/fs.FileWrite` sets `RequiresPermission: true` but does not need an
executor. It sets `PermissionAction: "write_file"` and a target formatter
that renders the workspace-relative path. It writes directly after the
loop permission gate approves the call. It must remain workspace-rooted,
refuse symlink escape, use temp-file-plus-rename, and refuse overwrites
by default.

### 7. Sandboxing stays deferred

This ADR does not add a sandbox, container runtime, seccomp profile,
namespace isolation, or remote execution service. `LocalExecutor` runs on
the local machine. That is acceptable because M2's safety layer is the
combination of:

- explicit side-effect declarations;
- host-supplied permission prompts;
- command validation inside shell tools;
- the `Executor` interface as the replacement point for future sandboxing.

Any future sandbox implementation must fit behind `Executor` and must not
force tool or loop rewrites.

## Consequences

- Glue gains the minimal framework contracts needed for coding-capable
  agents without gaining product UI or policy.
- Existing read-only tools and Peggy's v0.1 memory tools do not change
  behavior because `RequiresPermission` defaults false.
- Side-effect tools become uniform: hooks, permission, execution, and
  post-processing are composed once in the loop.
- Peggy can implement product-specific permission UX in `agents/peggy`
  and in channel packages without leaking channel concepts into core
  `glue`.
- The M3 daemon protocol has a clear shape for remote permission UX:
  surface `PermissionRequest`, wait for `PermissionDecision`, then resume
  the loop.
- Permission caching is intentionally not a core concern yet. The
  decision type reserves the language; product implementations decide how
  to remember and expire choices.
- The next implementation PR should start with `glue.Executor` and
  `LocalExecutor`, then layer permission/hook loop composition before
  introducing side-effect tools.
