# ADR 0005: Foundation Expansion For Long-Running Agents

## Status

Accepted. Filed under tracker [#110](https://github.com/erain/glue/issues/110)
(Peggy — personal-assistant agent on glue) as the first design issue
that unblocks the rest of M1.

## Context

The `0.x` glue surface was scoped for short-lived, single-purpose agents
(see `agents/glue-review`: emit one PR comment and exit). That scope was
correct for the original roadmap and is reflected in the explicit P0/P1
non-goals in `docs/design.md`: no sandboxing, no shell execution, no
write-side filesystem, no MCP, no HTTP server, no automatic compaction.
ADR-0003 partially opened the shell/filesystem door for an extension
package (`tools/fs`, `tools/git`) but kept write-side and shell exec
deferred.

The next milestone (Peggy, tracker [#110](https://github.com/erain/glue/issues/110))
is a different category of agent:

- **Long-running.** A single process that lives for days, not seconds.
- **Multi-channel.** Reachable from a REPL today, Telegram next, and
  later TUI/web/IDE clients on the same brain.
- **Memory-bearing.** Remembers facts, decisions, and people across
  sessions.
- **Capable enough to code.** Reads, edits, and runs code when the model
  is good enough.

That category is incompatible with the existing non-goals as written. We
cannot ship Peggy without lifting most of them. This ADR records that
pivot before any code does so, so the contributor protocol's
scope-discipline rule (`CONTRIBUTING.md` §"Scope Discipline") does not
treat the M1 implementation PRs as smuggled scope expansion.

Inspirations the Peggy work draws on:

- [OpenClaw](https://github.com/openclaw/openclaw) — gateway control
  plane that multiplexes channels onto one event bus; per-session
  sandbox tiering instead of per-call permission prompts.
- [Hermes-Agent](https://github.com/NousResearch/hermes-agent) —
  skills-as-files for procedural memory; FTS5 over past sessions for
  cross-session recall; tools-as-RPC unification of built-ins, MCP, and
  subagents behind one transport.

We borrow the patterns, not the code.

## Decision

### 1. Glue stays a framework, not a product.

The single rule that protects glue's purity through this expansion:

> Every product concern enters glue **only as an interface the host
> fills in** — never as a default behavior with UI, channel, or policy
> baked in.

Concretely: glue ships `Permission`, `Hook`, `Executor`, `Searcher`, and
similar interfaces. It does not ship a default permission UI, a default
shell prompter, a default channel, or a default storage backend that
implies a UI. The Peggy product (under `agents/peggy`) implements those
interfaces. A different product could implement them differently and
share zero code with Peggy.

This is the discipline that lets Peggy land without turning glue into
"the Peggy library."

### 2. Non-goals: lift, defer, preserve.

The `docs/design.md` Non-Goals list is updated as part of this ADR.
Specifically:

**Lifted** (now in scope, but only behind interfaces glue exposes):

- **Shell execution.** Lands as a `tools/shell` extension package gated
  by an `Executor` interface (see (3) below). ADR-0003's deferral is
  resolved.
- **Write-side filesystem.** Lands as additions to `tools/fs` gated by
  the `Permission` interface.
- **MCP integration.** Lands as a client (`tools/mcp` or similar)
  exposing remote tools through the same `Tool` interface as built-ins,
  per Hermes-Agent's tools-as-RPC pattern.
- **HTTP server / daemon mode.** Lands as `cmd/glue serve` exposing
  Sessions and Events over HTTP+SSE so multiple channel adapters (TUI,
  Telegram, future IDE) can attach to one running brain.
- **Automatic compaction trigger.** The `Compactor` interface and
  threshold remain opt-in (no behavior change to existing callers), but
  a token-aware `SummarizingCompactor` ships as a drop-in policy (see
  ADR-0007).

**Deferred** (still out of scope for now, behind a clean interface so
they can land later without rework):

- **Sandboxing / containerization.** No process isolation, namespace
  isolation, or container runtime in glue. The `Executor` interface (3)
  is the seam: the default executor runs locally; a future
  docker/sandbox executor can drop in without touching tool code or
  the loop.

**Preserved** (still firm non-goals):

- No dynamic Go plugin loading.
- No deploy target.
- No implicit parallel tool execution. `RunRequest.Parallel` remains
  opt-in.
- Glue does not learn about channels (Telegram, Slack, web, etc.).
  Channels live in product packages and bind to glue Sessions through
  the daemon protocol or in-process API.

### 3. New contracts (interfaces only, no implementations in this ADR).

The implementations land in their own issues. This ADR records the
contracts the Peggy work will introduce so reviewers know what to
expect:

- **`Executor`** — abstracts how shell commands run. Default is local
  `os/exec`; sandbox/docker can drop in later. Lives in core `glue` so
  the loop can pass it to executor-aware tools.
- **`Permission`** — host-supplied callback consulted before tools with
  side effects run (write-fs, shell, network). The host returns
  allow/deny/ask-once-then-allow. Glue does not render the prompt.
- **`Hook`** — pre/post tool, pre/post prompt, on-compaction lifecycle
  hooks the host can register. Mirrors Claude Code's hook system as Go
  interfaces; no shell-out semantics.
- **`Searcher`** — optional capability a `Store` may implement to
  support cross-session search. `Agent.SearchSessions` returns
  not-supported when the active store does not implement it.
- **Subagent (`Agent`-as-`Tool`)** — a primitive that lets a parent
  agent expose a child agent as a `Tool` with explicit context
  isolation. Used for "spawn a coder" patterns. In-process for v0.1; no
  process or sandbox isolation.

The Permission/Hook/Executor signatures are designed in their own ADR
(M2 — "Peggy v0.2: she can code"). The Searcher signature is designed
in ADR-0007 (memory layer).

### 4. ADR sequence under tracker #110.

To make this ordering unambiguous:

1. **ADR-0005 (this).** Foundation expansion — what changes about scope.
2. **ADR-0006.** Codex provider — subscription auth + Responses API
   transport.
3. **ADR-0007.** Memory layer — `SummarizingCompactor` +
   `stores/sqlite` + `Searcher` interface.
4. **ADR-0008.** Channel adapter pattern — how external channels bind
   to glue Sessions without channel logic leaking into glue.
5. (M2) ADR for `Executor` / `Permission` / `Hook` interfaces.
6. (M3) ADR for the daemon protocol (HTTP+SSE).

Implementation issues for each ADR follow the same one-issue-per-PR
pattern as the original roadmap.

## Consequences

- `docs/design.md` Non-Goals is updated in this PR. Reviewers of M1
  implementation PRs must check changes against the updated
  Non-Goals — not the historical version — when applying
  `CONTRIBUTING.md` §"Scope Discipline".
- Glue gains optional dependencies (`modernc.org/sqlite` under
  `stores/sqlite`, an HTTP+SSE server under `cmd/glue serve`, a
  Telegram bot library under `agents/peggy`). All sit behind extension
  packages; the core `glue` import surface gains nothing heavy.
- The "interface, not default" rule is load-bearing. A PR that adds a
  default channel, default permission UI, or default policy-laden
  behavior to core `glue` is out of scope and should be re-shaped as
  (a) an interface in glue + (b) a default in `agents/peggy`.
- Sandboxing is the one capability we explicitly leave for later. The
  `Executor` interface is the placeholder; whoever ships sandbox
  support implements `Executor` against docker/firecracker/whatever
  without touching the rest of glue.
- The original glue roadmap (P0/P1/P2 under tracker #1) remains
  closed. Tracker #110 is the active source of truth for everything
  that follows.
