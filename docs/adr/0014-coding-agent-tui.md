# ADR-0014: Coding-agent TUI (cmd/glue/tui)

## Status

Accepted.

## Context

`cmd/glue` shipped through `v0.1.0` as a strictly one-shot binary:
`glue run --prompt "..."` runs the loop until the model stops, prints
the final text, and exits. ADR-0012 left this loophole open
(*"interactive multi-turn `glue run` session UX over the existing
session plumbing"*) and the tracker carried it as a `*(planned)*` Track
A item.

Every other comparable coding agent in 2026 (Claude Code, Codex CLI,
Aider, Cline, OpenCode, etc.) is interactive by default. Without an
interactive surface, the homepage's framing — "Agent = Provider + Loop
+ Tools" — invites users to a binary that can't have a conversation.

## Decision

Build a real bubbletea-based TUI in a new package `cmd/glue/tui/` and
make it the default mode for `glue run` when no `--prompt` is given and
the process is attached to a terminal. Preserve all existing one-shot
and scripted invocations exactly.

### Interactive-mode detection (one-way door reversed at zero cost)

`glue run` resolves the run mode like this:

| `--prompt` | stdin | stdout | Mode |
|-----------|-------|--------|------|
| set | any | any | one-shot, as today |
| unset | not a TTY | any | read all of stdin as the prompt, one-shot |
| unset | TTY | TTY | **interactive TUI** |
| unset | TTY | not a TTY | error: "interactive mode unavailable" |

The auto-detect keeps `echo "fix" \| glue run` scriptable and keeps
`glue run --prompt "..."` unchanged. The TTY check uses
`golang.org/x/term.IsTerminal` against the actual `*os.File` stdin/stdout
in `main()`; tests pass `strings.Reader`-like values, which are never
TTYs, so the test suite is not steered into the TUI path by accident.

### Library + dependency boundary

The TUI lives strictly under `cmd/glue/tui/`. The `glue` library
package and every existing reusable subpackage remain free of
TUI-library dependencies. The TUI imports:

- `github.com/charmbracelet/bubbletea` — the Elm-style event loop.
- `github.com/charmbracelet/bubbles` — `viewport` (scrollable
  transcript) and `textarea` (multi-line input).
- `github.com/charmbracelet/lipgloss` — terminal styling.
- `golang.org/x/term` — TTY detection in `cmd/glue/main.go`.

`charmbracelet/glamour` was considered for inline markdown rendering of
assistant text but is **not adopted in this first PR** — it adds a sizable
syntax-highlighter (Chroma) transitive surface and the streaming-render
trade-offs (re-flowing partial markdown on every delta) deserve their
own pass. Plain text + a small inline diff renderer for `edit_file` is
the v1.

### Surface in this PR

- Alt-screen layout: header (session / provider/model / workdir),
  scrollable transcript, multi-line input box with growing height,
  status bar (turn count + last-turn token totals when available).
- Transcript items: user messages, streaming assistant text,
  tool-call cards with states (pending → running → done / failed /
  denied), inline 2-pane unified-diff preview for pending `edit_file`
  calls.
- **Permission bridging.** `cmd/glue/tui.permissionBridge` implements
  `glue.Permission`; on `Decide`, it sends a `permRequestMsg` to the
  bubbletea program and blocks on a per-request response channel. The
  agent loop is unaware the host is a TUI. Cancellation via context
  releases the bridge with a denial reason. The bridge is wired into
  each prompt via `glue.WithPermission` rather than mutating the
  agent's options, so the agent stays reusable across turns.
- Slash commands: `/help`, `/exit` (and `/quit`, `/q`), `/clear` (new
  session id), `/usage`, `/tools`, `/model <id>`, `/session [id]`.
- Keys: Enter sends, ↑/↓ in single-line input scrolls history,
  PgUp/PgDn scrolls the transcript, Ctrl+C cancels the current turn on
  first press and quits on second.

### Concurrency

Each user submit spawns a goroutine that:

1. Subscribes to the session's events via `Session.Subscribe`.
2. Calls `session.Prompt(ctx, prompt, glue.WithPermission(bridge))`.
3. Translates each event into a typed `tea.Msg` (`textDeltaMsg`,
   `toolStartMsg`, `toolEndMsg`) and forwards via `tea.Program.Send`,
   which is goroutine-safe.
4. Emits `turnDoneMsg{Err}` when `Prompt` returns; unsubscribes.

The TUI's Update never blocks; the goroutine is the producer and
`tea.Program.Send` the queue. Ctrl+C cancels the goroutine's context.

## Loopholes and Fixes

- **Loophole: scripted invocations break if TUI auto-detects too
  eagerly.** Fixed by the four-row detection table above and by
  preserving `io.Reader` (not `*os.File`) plumbing through test seams:
  tests never trigger TUI, period.

- **Loophole: agent-level permission and TUI permission fight.** Fixed
  by leaving `AgentOptions.Permission` nil in interactive mode and
  injecting the TUI bridge via `glue.WithPermission` per prompt.

- **Loophole: charmbracelet deps leak into library users.** Not
  possible by construction — `cmd/glue/tui` is in the binary, not the
  library import graph. A `go get github.com/erain/glue` consumer pulls
  zero charmbracelet code.

- **Loophole: a long-running model call blocks the TUI.** Not fixed in
  this ADR. Streaming events keep the TUI responsive during a turn;
  truly long blocking phases (cold-start latency on a free OpenRouter
  model, say) appear as a `running…` status. A spinner per-tool-card is
  a follow-up.

- **Loophole: integration test coverage is thin.** Unit tests cover
  the slash-command parser, transcript rendering for every tool phase,
  the edit_file diff preview, and the permission bridge's allow / deny
  / context-cancel paths. End-to-end TUI behavior (bubbletea Update
  with synthetic key events via `teatest`) is a deliberate follow-up;
  the first PR ships with manual-smoke verification on a real terminal.

- **Loophole: TUI on `glue connect` is missing.** Out of scope for
  this PR. The daemon-backed path is a separate surface; a future PR
  can lift the same `tui` package to drive `connect` and the model
  becomes "one TUI shell, two transports."

## Consequences

`glue run` becomes an interactive coding agent without breaking any
existing usage. The TUI is opinionated but small enough that the next
iteration (glamour markdown, spinners, per-hunk diff approval, file
tree, @-mention picker) is incremental, not a rewrite. The dependency
surface expansion is confined to the binary.
