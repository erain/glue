# ADR 0003: Shell And Filesystem Tools

## Status

Accepted. Implemented in #89 — read-side filesystem and git helpers
ship as `tools/fs` (`SafeJoin`, `Truncate`, `Blocklist`, `ReadFileTool`)
and `tools/git` (`RunGit`, `BuildPathspec`, `DiffBranchTool`,
`LogBranchTool`). Write-side filesystem and arbitrary shell execution
remain out of scope until a follow-up ADR designs the safety boundary.

## Context

LLM agents commonly want to read files, write files, and run shell
commands. Glue does not ship any of these tools today. The question for
this ADR is not whether we'll eventually want them but where they live,
how they're configured, and what safety boundary they enforce.

The non-goals stated in `docs/design.md` for P0 / P1 — "no sandboxing,
shell execution, container runtime, or remote connector" — were a scope
guard, not a permanent ban. Now that the tool execution path is stable
(see #6, #10, #17), it is safe to ship explicit local tools as long as
they remain opt-in and do not weaken the loop's existing guarantees.

## Decision

1. **Shell and filesystem tools live in a dedicated extension package,
   not in the core `glue` package.** The proposed module path is
   `glue/tools/local`. Importing the package brings the tools into a
   user's program; no auto-registration, no init-time side effects.

2. **Each tool is constructed with explicit caller-supplied bounds.**
   Tools never run with the agent process's full filesystem or shell
   privileges by default. Constructors accept a typed configuration
   object that carries the trust boundary (see API section below). A
   tool with an empty configuration refuses to execute and returns an
   error tool result.

3. **Safety is enforced inside Glue, not delegated to the OS.** The
   tools do not assume a sandbox, container, or chroot. They open and
   resolve every path, then check it against the configured allow list
   before any I/O. They run shell commands only with an exec-style
   `argv []string` invocation (no `sh -c`) and only when the binary
   matches a configured allow list. Any uncertainty is treated as a
   refusal.

4. **Untrusted-by-default vs. opt-in trusted.** All path access is
   read-only unless the caller explicitly grants write access; all shell
   tools require an explicit binary allow list to be useful. The
   "default deny everything" stance is what keeps these tools usable
   inside an agent that can be prompt-injected.

5. **The tools surface output as text content with a `Metadata` block,
   never via the model's interpretation of stdout.** Tool results
   include `exit_code`, `stdout_truncated`, and `path` keys when
   relevant so downstream observers (and tests) can act on structured
   facts.

## Why an extension package, not core

Putting these tools in the core `glue` package would couple the library
to the host's POSIX surface and would tempt callers to wire them up
without thinking. Putting them in `examples/` would mean callers copy and
modify them, which is a textbook way to introduce subtle path-traversal
bugs. A dedicated extension package keeps the surface small and lets the
package's tests carry the safety burden — every Glue app that imports the
package gets the same path checks.

## Why not just one tool

Shell, file-read, and file-write each have different threat models:

- **File read** is the lowest-risk and the most useful in a code-aware
  agent. It can ship with just a workspace root.
- **File write** has higher blast radius (overwrite, delete, symlink
  attacks) and benefits from atomic writes plus an explicit
  no-traversal check.
- **Shell exec** is a different category: argv-only invocation, binary
  allow list, captured-and-truncated output, deadline enforcement,
  optional environment subset.

Wrapping all three behind a single `local_actions` tool would force the
tightest constraint on every action. Three tools is honest about the
distinct threat models.

## Proposed API (sketch)

This is a design contract, not the implementation; the actual code
lands behind a follow-up issue.

```go
package local

// FileRead returns a glue.Tool that reads files inside Workspace. Calls
// referencing paths outside Workspace return an error tool result.
type FileReadOptions struct {
    Workspace string // absolute root; required
    MaxBytes  int    // truncation cap; 0 means default 256 KiB
}

func FileRead(opts FileReadOptions) (glue.Tool, error)

// FileWrite returns a glue.Tool that writes files inside Workspace. By
// default it refuses overwrites; set AllowOverwrite to relax. Writes are
// atomic (temp + rename) and refuse to follow symlinks out of Workspace.
type FileWriteOptions struct {
    Workspace      string
    AllowOverwrite bool
    MaxBytes       int
}

func FileWrite(opts FileWriteOptions) (glue.Tool, error)

// Exec returns a glue.Tool that runs an argv via os/exec. Only binaries
// in AllowedBinaries are accepted (matched by basename). Working
// directory is fixed to Workspace. Output is truncated to MaxBytes per
// stream. Timeout is enforced.
type ExecOptions struct {
    Workspace        string
    AllowedBinaries  []string  // e.g. {"go", "git"}; empty = refuse all
    Env              []string  // pass-through subset; nil = inherit none
    Timeout          time.Duration // 0 = default 30s
    MaxBytes         int       // per stream; 0 = default 64 KiB
}

func Exec(opts ExecOptions) (glue.Tool, error)
```

All three return `(glue.Tool, error)`. The error path covers
construction-time misconfiguration (e.g., empty `Workspace`); execution-
time refusals are surfaced as `IsError: true` tool results so the model
can react.

### Tool result metadata keys

- `path` — the absolute path that was read/written (post-resolution).
- `bytes` — number of bytes the tool returned (post-truncation).
- `truncated` — bool, true when the file or stream was longer than
  `MaxBytes`.
- `exit_code` — int, exec only.
- `stderr` — string, exec only; truncated to `MaxBytes`.
- `command` — string, exec only; the resolved binary and argv.

These are kept under `ToolResult.Metadata` so the model sees the textual
content and the host code (or tests) can branch on the structured fields.

## Boundary with sandboxing

This ADR explicitly does *not* introduce sandboxing. The local tools run
in the same process and with the same OS privileges as the agent. The
`Workspace` allow list and the binary allow list are user-space safety
nets, not security guarantees against a hostile binary. If a caller
needs sandboxing they should:

1. Run the agent in a container or namespace.
2. Or inject their own `glue.Tool` whose executor RPCs into a sandboxed
   helper.

Glue's role is to expose tools that are safe enough for typical local
agent loops (read this repo, run `go test`, etc.), not to be the last
line of defense against an attacker.

## Implementation order

A follow-up issue should land tools in this sequence so each ships with
its own focused tests:

1. `FileRead` — workspace-resolution + traversal-refusal tests.
2. `FileWrite` — atomic write + overwrite-by-default-refused tests.
3. `Exec` — allow-list + timeout + output-truncation tests.

Each is independently useful; later tools cannot be built on top of
earlier ones.

## Consequences

- The core `glue` package stays free of POSIX coupling.
- Callers who want shell/file tools must opt in by importing the
  extension package and writing the configuration explicitly. The
  configuration surface is the contract.
- Tests for these tools become the canonical safety doc — every refusal
  case is a test, not a comment.
- `docs/design.md`'s P0 / P1 non-goal language ("no shell / filesystem")
  is now scoped to "not in core", not "never". The extension package is
  the safe place for that capability.
