# Changelog

The library at `github.com/erain/glue` versions on a `1.x` line but
has **not** locked its API: minor bumps may still break API, exactly
as the pre-1.0 stance in
[`docs/adr/0013-pre-1-0-stability-stance.md`](docs/adr/0013-pre-1-0-stability-stance.md)
describes (its addendum records how the `1.x` line started). Pin a
tag in your `go.mod` if you need stability. Breaking changes always
land with a `**Breaking:**` entry under a minor-bump section — never
on a patch release.

This file tracks library-level changes. The reference agents version
independently:
[`agents/glue-review`](agents/glue-review/README.md) (release notes in
its [GitHub Releases](https://github.com/erain/glue/releases?q=agents%2Fglue-review)),
and [`agents/peggy/CHANGELOG.md`](agents/peggy/CHANGELOG.md).

## Unreleased

- **TUI: `/` picker shows every command; `@` picker gains scroll
  indicators (`cmd/glue/tui`).** The slash-command popup previously
  reused the file picker's 8-row scroll window with no indicator, so a
  bare `/` silently hid 5 of the 13 commands. It now lists the whole
  (bounded) command set, and the `@`-file picker — which keeps its
  window because workspaces are unbounded — shows `↑/↓ N more` lines
  when matches are clipped. (#356)

## 1.13.0 — 2026-06-09

- **Per-model capability registry + tool-owned prompt assembly
  (`providers`, `loop`, `tools/*`, `cmd/glue`).** The providers
  registry now carries declarative `Capabilities` per provider
  (context window, parallel-tool safety, prompt variant, auto-continue
  proneness; `providers.CapabilitiesFor(name)`), replacing
  if-provider-name switches — the `glue` binary's Gemini auto-continue
  gating now reads the registry. Tools own their prompt text: the new
  `ToolSpec.PromptSnippet` / `PromptGuidelines` fields (set across
  `tools/fs`, `tools/shell`, `tools/git`) feed
  `coding.SystemPrompt(tools, variant)`, which assembles a coding
  system prompt from the active toolset — one line per tool plus
  deduplicated guidelines, in a terse variant for frontier models
  (gemini, codex) and an explicit variant for open-weight models. The
  `glue` binary previously ran `--coding` with **no** system prompt;
  it now gets the assembled one, and the prompt can never drift from
  the registered toolset (snapshot-tested)
  ([docs/coding-harness-roadmap.md](docs/coding-harness-roadmap.md)
  P1.5). Closes #345.

- **Loop guardrails (`loop`).** Two graduated detectors now watch every
  tool round, on by default (`RunRequest.Guardrails`, zero value =
  defaults; `Disabled` opts out): repeating the **same tool call with
  identical arguments** draws a corrective user message at 3
  consecutive occurrences and ends the run with the typed
  `ErrRepeatedToolCalls` at 5; **consecutive all-error tool rounds**
  draw a corrective message at 3 and end the run with
  `ErrTooManyMistakes` at 6. Streaks reset on any change of arguments
  or any successful result; injected messages are marked
  `glue/guardrail` in metadata and `EventGuardrail` reports
  kind/count/action. These are the failure shapes that waste tokens
  fastest on open-weight models — and unattended `glue goal` runs
  ([docs/coding-harness-roadmap.md](docs/coding-harness-roadmap.md)
  P2.8). Closes #344.

- **Next-speaker stall recovery (`loop`, `glue`, `cmd/glue`).** Gemini's
  classic stall — narrating "I will now update the file." and stopping
  without calling a tool — is now recovered: with the new opt-in
  `RunRequest.AutoContinue` / `AgentOptions.AutoContinue`, an assistant
  turn whose closing sentence announces a future action (and asks no
  question) gets a synthetic "Please continue." user message (marked
  `glue/auto-continue`, at most twice per run, `EventAutoContinue`
  emitted). The `glue` binary enables it automatically for the gemini
  provider when tools are registered. The surrogate-sanitization item
  from the roadmap was verified unnecessary back in #313 (Go's
  `encoding/json` already sanitizes invalid UTF-8)
  ([docs/coding-harness-roadmap.md](docs/coding-harness-roadmap.md)
  P2.7). Closes #343.

- **Compaction upgrade (`glue.SummarizingCompactor`).** The default
  summarization prompt now requests a structured state snapshot (Goal /
  Constraints / Progress / Key Decisions / Next Steps / Critical
  Context, exact paths and error messages preserved verbatim) with a
  prompt-injection firewall, and instructs the summarizer to integrate
  any previous snapshot instead of re-summarizing it. The snapshot is
  framed as a handoff from another assistant instance (reduces re-doing
  finished work). New: a cumulative **file ledger** — read/modified
  paths extracted from coding-tool calls, merged across compactions and
  carried in marker metadata; **safe split points** that never orphan a
  tool-call/result pair; opt-in `KeepRecentTokens` to select the
  verbatim tail by token budget instead of message count; and an
  **inflation guard** that refuses a "summary" bigger than the region
  it replaces
  ([docs/coding-harness-roadmap.md](docs/coding-harness-roadmap.md)
  P1.6). Closes #342.

- **Retry/overflow recovery (`loop`, `glue`)
  ([ADR-0017](docs/adr/0017-loop-retry-overflow-recovery.md)).**
  Transient provider failures (429/5xx, rate limits, dropped or
  never-finished streams) are now retried at the loop level with
  exponential backoff (3 retries, 2s base, 30s cap), honoring
  server-provided `Retry-After` / Gemini `RetryInfo.retryDelay` hints;
  the new `EventRetry` event reports each attempt. Auth/billing/
  invalid-request errors still fail fast, and context-window overflow
  surfaces as the new typed `*loop.OverflowError` — which
  `Session.Prompt` catches, compacts once (when a `Compactor` is
  configured), and retries once. Retries never duplicate history:
  nothing is appended until an attempt succeeds. Opt out with the new
  `RunRequest.Retry: loop.RetryPolicy{Disabled: true}`
  ([docs/coding-harness-roadmap.md](docs/coding-harness-roadmap.md)
  P1.4). Closes #341.

- **History hardening (`loop`).** Every `loop.Run` now repairs the
  transcript before anything reaches the provider, via the new exported
  `loop.HardenHistory`: assistant tool calls whose result is missing
  (Esc-cancel, crash, resumed/forked session) get a synthesized
  `IsError` result marked `glue/synthetic`; orphaned tool results and
  empty assistant turns are dropped; tool-call IDs are normalized to
  `[A-Za-z0-9_-]{1,64}` consistently across call and result; and turns
  produced by a different model lose their thinking blocks and
  provider-specific signatures. Both the Gemini and OpenAI-compatible
  APIs reject these malformed transcripts with opaque 400s
  ([docs/coding-harness-roadmap.md](docs/coding-harness-roadmap.md)
  P0.3). Closes #340.

- **Structured output truncation (`glue`, `tools/shell`, `tools/fs`).**
  `shell_exec` output is now captured head+tail instead of head-only —
  a long build log keeps both the first error and the final status,
  with an inline `[... N bytes (~M lines) omitted ...]` marker, a total
  line count, and (in the coding bundle) the complete stream spooled to
  a temp file named in the result so the model can read back what was
  dropped. Timeouts now prepend "command timed out after Ns; partial
  output below" to the kept output instead of replacing it.
  `read_file` gains `offset` / `max_lines` paging with dual line+byte
  caps; truncated reads end with `[Showing lines X-Y of Z. Use
  offset=N to continue.]`, oversized single lines and >20MB files get
  explicit shell-tool escape hatches. `ExecResult` gains additive
  `StdoutTail`/`StderrTail`, `*Omitted`, `*Lines`, and `*Spool` fields;
  `ExecCommand.SpoolDir` opts executors into spooling
  ([docs/coding-harness-roadmap.md](docs/coding-harness-roadmap.md)
  P0.2). Closes #339.

- **`edit_file` repair ladder (`tools/fs`).** `old_string` no longer has
  to match byte-for-byte: a deterministic cascade absorbs the drift
  models introduce — trailing-whitespace differences, indentation drift
  (the replacement is re-indented to the file's real indentation, with
  per-level tab/space mapping), smart-quote/Unicode-dash/exotic-space
  folding, block-anchor matching for ≥3-line blocks with a misquoted
  middle, and over-escape repair (literal `\n` sequences from
  double-escaped JSON). CRLF line endings and a UTF-8 BOM are preserved
  on write. Non-exact matches are reported in the result; success now
  echoes the updated lines (so the model needn't re-read the file);
  failures echo what was searched for with concrete advice; and
  "rest of code unchanged"-style placeholder replacements are rejected.
  Source-verified port of the consensus technique across pi, Cline,
  Codex CLI, and Gemini CLI
  ([docs/coding-harness-roadmap.md](docs/coding-harness-roadmap.md) P0.1).
  Closes #338.

## 1.12.0 — 2026-06-09

- **Headless `glue goal` subcommand — scheduled/CI goal runs (goal loop
  Phase 3c).** `glue goal "<objective>"` runs a full plan→make→verify loop
  without a TUI, streaming checklist-level progress to stdout and exiting
  with a status-mapped code (0 achieved · 2 blocked · 3 max-iterations ·
  4 budget-limited · 1 errored) so cron, CI, or a peggy schedule can branch
  on the outcome. Shares `glue run`'s infrastructure flags plus
  `--max-iterations`, `--budget`, and `--worktree` (same `.glue/worktrees/`
  + `goal/<id>` isolation as the TUI's `/goal -w`); `--yolo` for unattended
  runs. `glue goal --list` prints stored records and `glue goal --resume
  [id]` continues from the verified checklist — the store is shared with the
  TUI, so a goal started in `glue run` can be resumed headlessly and vice
  versa. The worktree helper moved to a shared `cmd/glue/worktree` package.
  Closes #328.
- **Goal worktree isolation — `/goal -w` (goal loop Phase 3b).**
  `/goal -w <objective>` (or `--worktree`) runs the goal's maker and checker
  in a dedicated git worktree at `.glue/worktrees/<goal-id>` on branch
  `goal/<id>`, leaving your checkout untouched; the finished goal names the
  branch to review and merge (never auto-merged). The run's coding tool set
  is rebuilt rooted at the worktree via the new `tui.Config.BuildTools`
  factory (wired from `glue run --coding` with the same `--allow-binary` /
  `--coding-allow-overwrite` / `--tools` / `--no-tools` flags), and the new
  `GoalSpec.WorkDir` / `GoalRecord.WorkDir` (`glue/goal:workdir`) field makes
  resume — in-process or across restarts — re-attach the same worktree and
  branch. Without `--coding` or outside a git repository, `-w` refuses with a
  clear message before any goal starts. Closes #326.
- **Durable goal state + resume across restarts (goal loop Phase 3a).**
  When the agent has a `Store`, `Agent.PursueGoal` checkpoints a `GoalRecord`
  (objective, status, verified checklist, iterations, usage, summary) as
  namespaced `glue/goal:*` metadata on the `SessionPrefix` session — after
  planning, after every checker verdict, and at the terminal state. A context
  cancellation persists as the new `GoalPaused` status (resumable); the new
  `GoalRunning` status marks in-flight records. `Agent.LoadGoal` and
  `Agent.ListGoals` (most recent first, via the optional `SessionLister`
  capability) read records back, and `GoalRecord.Resumable()` says whether a
  goal can continue from its checklist. `GoalSpec.StartIteration` lets a
  resumed run continue iteration numbering (`iter-4`, `check-4`, …) so
  maker/checker sessions stay fresh instead of appending to the previous
  run's transcripts. In the TUI, `/goal resume` with no in-process goal now
  continues the most recent unfinished record from the store — a goal
  survives quitting `glue run` — and the new `/goal list` shows recent goals
  with status, checklist fraction, and age. Checkpointing is best-effort and
  storeless agents are unaffected. Closes #324.

## 1.11.0 — 2026-06-09

- **TUI `/goal`: drive the goal loop from inside `glue run` (Phase 2).**
  `/goal <objective>` runs `Agent.PursueGoal` in the background on its own
  session ids while the chat stays usable: a live goal card in the transcript
  updates in place per iteration (verified `[x]/[ ]` checklist with evidence,
  iteration counter, token usage, last checker verdict), and the status bar
  gains a `◎ goal · iter 2/10 · 1/4 ✓ · 12.3k tok` segment. Subcommands:
  `/goal status`, `/goal pause` (cancels cleanly, keeps the verified
  checklist), `/goal resume` (continues from that checklist without
  re-planning), `/goal clear`. Goal tool calls flow through the same in-card
  permission prompt as chat turns — pending permission requests are now a FIFO
  queue, so a concurrent goal + chat request can no longer drop one (`--yolo`
  makes goals fully autonomous). Closes #322.
- **`GoalSpec.Checklist`: seed `Agent.PursueGoal` with an existing plan.**
  When non-empty, the planning step is skipped and the loop continues from the
  given items (done flags and evidence respected) — the primitive behind
  `/goal resume`, and the hook Phase 3's durable resume will reuse.
- **`GoalSpec.Permission` now applies to the planner and checker sessions too**
  (previously maker-only). Without this, a checker under an agent with no
  default permission policy — e.g. the interactive TUI — was denied every
  side-effecting tool and could not run builds or tests to verify evidence.

## 1.10.0 — 2026-06-09

- **Goal loop: `Agent.PursueGoal` — "loop engineering" / `/goal` (Phase 1).**
  A new library primitive that turns one persistent objective into an
  autonomous loop: a planner decomposes the goal into a verifiable checklist,
  then each iteration runs a **maker** (a fresh session seeded from the
  checklist — a Ralph-style loop, so memory lives in durable state, not a
  growing transcript) followed by a separate **checker** session that audits
  against real evidence (`Session.PromptJSON`) and decides completion — the
  writer never grades its own homework. Bounded by `GoalSpec` guardrails
  (`MaxIterations`, `NoProgressLimit`, `TokenBudget`) and observable via
  `GoalSpec.Emit`. Headless primitive only; the TUI `/goal` command and durable
  resume are follow-ups. Designed in
  [ADR-0016](docs/adr/0016-goal-loop.md). Closes #320.

## 1.9.0 — 2026-06-09

- **TUI: inline autocomplete for `/` slash commands (`cmd/glue/tui`).**
  Typing `/` now opens a filtering command popup (mirroring the `@`-file
  picker): `↑/↓` navigate, `Tab` completes, `Esc` closes (keeping what you
  typed), and `Enter` runs a fully-typed or no-argument command while
  completing an argument-taking one (e.g. `/model `). The command list is now
  a single source of truth shared by the picker and `/help`, so they can't
  drift. Closes #318.

## 1.8.0 — 2026-06-08

- **Fix Gemini 3.x tool calls: round-trip `thoughtSignature` (`providers/gemini`).**
  Gemini 3.x (incl. the default `gemini-3.1-pro-preview`) returns an opaque
  `thoughtSignature` on the parts it produces and **requires** it echoed back
  on replay; without it the second turn of any tool-using conversation failed
  with `400 … Function call is missing a thought_signature in functionCall
  parts`. The provider now captures the signature from function-call and
  thought parts (stored base64 on `ContentPart.Signature`) and replays it
  verbatim, sends prior thinking back as real `thought` parts, and enables
  `includeThoughts` on Gemini 3.x so reasoning streams through. Verified live
  with a two-turn tool loop. (Also points the gated live smoke test at the
  provider default; it hardcoded the now-removed `gemini-2.5-flash`.)

- **Gemini: synthetic `thoughtSignature` fallback for unsigned replays
  (`providers/gemini`).** When an active-loop model turn reaches Gemini 3.x
  without a real signature on its first function call — compacted history, a
  transcript written before signature round-tripping landed, or a turn that
  genuinely arrived unsigned — the provider now injects the sentinel
  `skip_thought_signature_validator` (the same value Google's gemini-cli uses)
  so the request still validates. Scoped to the active loop (everything after
  the most recent genuine user turn) and to Gemini 3.x ids; real signatures are
  never overwritten. Verified live.

- **Gemini: `GOOGLE_GENAI_API_VERSION` env knob (`providers/gemini`).** Pins
  the API version the client targets (e.g. `v1alpha`/`v1beta`) so users can
  reach version-gated preview features without a code change, matching
  gemini-cli. Unset leaves the SDK default in place.

## 1.7.0 — 2026-06-08

- **Fix Gemini default id: `gemini-3.1-pro-preview`, not `gemini-3.1-pro`.**
  v1.4.0 set the default to `gemini-3.1-pro`, which 404s — the public
  id on `generativelanguage.googleapis.com` v1beta carries the
  `-preview` suffix. Verified against `ListModels`. Override paths
  remain unchanged: `--model`, `GLUE_MODEL`, or
  `gemini.Options.DefaultModel`.

## 1.6.0 — 2026-06-08

- **TUI: cap transcript at 100 cols, center on wide terminals
  (`cmd/glue/tui`).** The viewport used to stretch to the full terminal
  width, so on a 200-col terminal the assistant text wrapped at the
  right edge and read as a wall. Now `bodyMaxWidth = 100` (locked to
  the input box width) caps the conversation column and `View()`
  centers it inside `m.width` via `lipgloss.PlaceHorizontal`. Glamour
  markdown renderer width inherits the cap so wrapping aligns. Header
  and status bar stay full-width — they read better edge-to-edge.
  Closes #307.
- **`glue version` / `glue --version` / `glue -v`.** Prints the module
  version, git revision, build time, and Go toolchain from the linker-
  embedded build info (same data as `go version -m $(which glue)`),
  reachable as a first-class subcommand so users can self-diagnose a
  stale binary without a side command.

## 1.5.0 — 2026-06-08

- **Catppuccin TUI theme (`cmd/glue/tui`).** Replaced the slate +
  Tailwind-primary palette with Catppuccin Mocha (dark) and Latte
  (light), picked at construction time by lipgloss's terminal-
  background heuristic. The accent role moves from #6d28d9 to mauve
  (#cba6f7 Mocha / #8839ef Latte) — still distinctly purple, but a
  pastel hue family where success/error/warn (green/red/peach) sit at
  similar lightness/saturation so the chrome no longer shouts. Glamour
  markdown output now ships under matching JSON style configs
  (`glamour-mocha.json`, `glamour-latte.json`) with chroma syntax
  highlighting via `catppuccin-mocha` / `catppuccin-latte`. All
  variable names in `cmd/glue/tui/styles.go` are unchanged; downstream
  callers (if any) keep compiling. Closes #305.

## 1.4.0 — 2026-06-08

- **Default Gemini model is now `gemini-3.1-pro`** (was `gemini-2.5-flash`).
  `gemini-2.5-flash` was removed from the v1beta `generateContent`
  API and began returning 404 for users on the current key surface.
  `gemini-3.1-pro` is the unmetered daily-driver target for this
  project. Override at any time with `--model <id>` or `GLUE_MODEL`.
  Library callers can still pass any model id through
  `loop.ProviderRequest.Model` or `gemini.Options.DefaultModel`.

## 1.3.0 — 2026-06-08

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
