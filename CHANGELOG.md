# Changelog

The library at `github.com/erain/glue` is **pre-1.0**; the `0.x`
series may break API on minor bumps. See
[`docs/adr/0013-pre-1-0-stability-stance.md`](docs/adr/0013-pre-1-0-stability-stance.md)
for the policy, and pin a tag in your `go.mod` if you need stability.
Breaking changes always land with a `**Breaking:**` entry under a
minor-bump section — never on a patch release.

This file tracks library-level changes. The reference agents version
independently:
[`agents/glue-review`](agents/glue-review/README.md) (release notes in
its [GitHub Releases](https://github.com/erain/glue/releases?q=agents%2Fglue-review)),
and [`agents/peggy/CHANGELOG.md`](agents/peggy/CHANGELOG.md).

## Unreleased

- Added `cmd/glue` interactive TUI. `glue run` with no `--prompt` and a
  terminal on stdin+stdout now opens a bubbletea-based interactive
  coding agent: scrollable transcript, multi-line input, streaming
  text, tool-call cards with inline permission prompts, and a small
  `edit_file` diff preview. Slash commands: `/help`, `/exit`,
  `/clear`, `/usage`, `/tools`, `/model <id>`, `/session [id]`. Ctrl+C
  cancels the current turn on first press, quits on second. Designed
  in [ADR-0014](docs/adr/0014-coding-agent-tui.md). Scripted and
  one-shot paths (`glue run --prompt ...`, `echo ... | glue run`) are
  preserved exactly. The new charmbracelet dependencies live under
  `cmd/glue/tui/`; the library import graph is unchanged.
- **Session tree (`glue` + `cmd/glue/tui`).** New `Agent.ForkSession`
  and `Agent.CloneSession` write child sessions whose metadata records
  their place in the lineage; `SessionParent` reads it back. New typed
  `ErrSessionNotFound`. Metadata keys are namespaced under
  `glue/tree:`. The TUI gains three slash commands: `/fork [N]`
  (defaults to "branch from just before my last user message"),
  `/clone`, and `/tree` (modal lineage view with `↑/↓` navigate, Enter
  switch, Esc cancel; current node marked `◉`, others `●`, non-root
  nodes tagged `forked@N`). Designed in
  [ADR-0015](docs/adr/0015-session-tree.md). Additive only — no
  existing API or on-disk format changes.
- **Daily-driver workflow polish (`cmd/glue` + TUI):**
  - **`--yolo` flag** on `glue run`: auto-allows every side-effecting
    tool call (`write_file` / `edit_file` / `shell_exec` / MCP) without
    surfacing a permission prompt. Implies `--coding-allow-overwrite`.
    Stderr banner at startup and a yellow `⚠ --yolo enabled` row in the
    TUI welcome card + a `yolo` chip in the status bar so it's never
    invisible. Use on a feature branch.
  - **`@` autocomplete file picker** in the TUI input. Type `@` after
    whitespace to open an inline rounded-purple popup of workspace
    files (walked once, capped at 5000, `.git` / `node_modules` /
    secret-shaped paths skipped). Type more chars to fuzzy-filter
    (case-insensitive, basename-start ranked first); `↑/↓` navigate;
    `Tab` / `Enter` insert the path (the textarea ends up with the full
    `@<path> `); `Esc` removes the `@<query>` and closes.
  - **`@file` argument expansion.** In `glue run --prompt`, in piped
    stdin, and in the interactive TUI's submit handler, `@<path>`
    inlines the file contents under a `--- @path ---` header so the
    model sees the actual code. Supports `@"path with space"` and
    `@@literal` escape. Workspace-rooted and refuses path-escape,
    secret-shaped (`.env`), symlinks, directories, and oversized
    files. Lives in the new `cmd/glue/atmentions` package.
  - **`--tools <list>` and `--no-tools` flags on `glue run`.** Filter
    the registered tool set: `--tools read_file,grep` restricts the
    model to that allowlist; `--no-tools` strips them all for a
    text-only run. Unknown names error with the list of available
    tools (no silent typos).
  - **`--mode json` output mode for one-shot runs.** Emits stable
    JSON-Lines events on stdout (`start`, `text`, `tool_start`,
    `tool_end`, `done`, `error`) for scripting and IDE integration.
    The interactive TUI ignores `--mode`; piping or `--prompt` enables
    it.
  - **`/compact` slash command in the TUI.** Triggers a token-aware
    `SummarizingCompactor` over the current session's transcript,
    persists the compacted state to the store, and reports
    `before → after` message counts as a system message.
  - **`/resume` session picker in the TUI.** Opens a modal list of the
    ten most-recent stored sessions (↑/↓ navigate, Enter select, Esc
    cancel) and replays the chosen session's transcript into the TUI.
    Needs a `Store` that implements `SessionLister`; the file store
    qualifies.
- Added `glue.Agent.ListSessions` (mirrors `Agent.SearchSessions`):
  returns `[]SessionSummary` from a `SessionLister`-capable store, or
  the new `ErrSessionListingNotSupported` sentinel.
- TUI input layer polish (`cmd/glue/tui`):
  - Dropped the textarea's internal `│ ` prompt — the box border was the
    only vertical line you should see.
  - Default to **1-row input that grows to 6** as you type (was a
    3-row minimum that felt heavy for short prompts).
  - **Ctrl+J inserts a newline; Enter submits** (Ctrl+J is ASCII LF and
    works on every terminal — Shift+Enter does not). The old Alt+Enter
    binding is gone.
  - **Accent (purple) rounded border** on the input box so "type here"
    is unambiguous.
  - **Italic muted placeholder** ("Ask anything · / for commands") and
    **accent-colored cursor**.
  - **Input box capped at 100 columns and centered** on wider terminals
    so it doesn't feel disconnected from the conversation above.
  - Bracketed paste was already on by default — pasting multi-line text
    no longer fires multiple submits.
  - Welcome card + status bar updated with the new key hints.
- TUI polish (`cmd/glue/tui` v1.1):
  - **Markdown rendering** for assistant text via `charmbracelet/glamour`,
    applied after each turn completes (streaming stays plain to avoid
    partial-markdown flicker).
  - **Sticky scroll**: streaming deltas no longer yank you to the bottom
    if you scrolled up to read older context; the status bar shows
    `↓ more below` when there's content past the viewport.
  - **Permission prompts moved inside the relevant tool card** instead
    of a separate floating box, so the action keys appear next to the
    diff/args they're about.
  - **Welcome card** with example prompts replaces the bare-system
    startup state; rebuilt after `/clear`.
  - **`/help` and `/tools` render as rounded, titled blocks** instead of
    cramped one-line system messages.
  - **Per-tool spinner** for in-flight tool calls; **"thinking…"
    spinner in the status bar** between turn start and first stream
    chunk; spinner only animates during a turn (no idle ticking).
  - **Esc cancels the current turn** in addition to Ctrl+C.
  - **`/clear` now clears the transcript** and starts a fresh session id
    + welcome card (was: only changed the session id). `/new` is an alias.
  - **Mouse wheel scrolls the transcript** via the existing mouse-cell
    motion handler.
  - **Visual rule between turns** so user → assistant → user cadence is
    scannable on long sessions.

## 0.1.0 — 2026-05-27

First tagged release. Brings the framework to launch shape and stabilizes
the public surface for `go get github.com/erain/glue@v0.1.0`.

### Added (M7 dual-track surface)

- `tools/coding`, a reusable SDK package that assembles the local
  coding-agent tool bundle (`read_file`, `write_file`, `edit_file`,
  `list_dir`, `find_files`, `grep`, `shell_exec`, `git_diff_branch`,
  `git_log_branch`) over the existing `tools/fs`, `tools/git`,
  `tools/shell`, and `glue.Executor` primitives.
- `cmd/glue` coding-agent mode: `glue run --coding` and
  `glue serve --coding` register the SDK coding bundle, with local
  terminal permission prompts for one-shot runs and daemon-brokered
  permissions for served runs.
- `cmd/glue --provider`: `run` and `serve` select any registered
  provider (`codex`, `gemini`, `nvidia`, `openrouter`) through the
  `providers` registry instead of being hardwired to Gemini, so the
  binary can run as a coding agent on a ChatGPT subscription
  (`glue run --provider codex --coding`). `--model` defaults to the
  selected provider's registry default model.
- `tools/fs.FileEdit` (`edit_file`), a permission-gated surgical
  exact-string replacement tool with a unique-match guard and optional
  `replace_all`.
- Read-only navigation tools `tools/fs.ListDirTool` (`list_dir`),
  `FindTool` (`find_files`), and `GrepTool` (`grep`).
  Workspace-scoped and escape-safe; `grep` skips secret-shaped
  (Blocklist) and oversized files, and all three skip `.git`.

### Public surface present at this tag

For completeness, this first tagged release also stabilizes everything
shipped during the bootstrap and the long-running foundation
(ADR-0005). The full surface — `Agent` / `Session` / `Tool` /
`Provider` types, the `loop` package, the four providers, both stores
(`stores/file`, `stores/sqlite` with FTS5 search), every `tools/*`
package, subagents (`glue.SubagentTool`), skills/roles/AGENTS.md,
structured JSON, opt-in parallel tool execution, the `Compactor`
interface and `SummarizingCompactor`, the `prompts` versioned-prompt
catalog, the `cli` standard-flags helper, and the `cmd/glue`
`run` / `serve` / `connect` daemon protocol — is documented in
[`README.md`](README.md), [`docs/building-agents.md`](docs/building-agents.md),
and [`docs/design.md`](docs/design.md).

### Notes

- The Codex provider authenticates via `codex login` (subscription
  auth path OpenAI does not formally document). Intended for personal
  use; see [`SECURITY.md`](SECURITY.md) for the scope statement.
- The local executor is permission-gated, not sandboxed. Implement
  `glue.Executor` against a container/VM if you need isolation
  ([ADR-0009](docs/adr/0009-executor-permission-hook.md)).

## Initial bootstrap (pre-0.1.0)

The library was under active development as a Go agent harness
inspired by [pi-mono](https://github.com/badlogic/pi-mono) and
[Flue](https://github.com/withastro/flue) before this first tag. The
detailed history lives in the git log; the surface that survived into
`v0.1.0` is listed above under "Public surface present at this tag."
