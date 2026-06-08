# ADR-0015: Session tree — sessions-as-leaves

## Status

Accepted.

## Context

`cmd/glue` reached parity with most of pi-coding-agent's surface in
[#298](https://github.com/erain/glue/issues/297) (`@file`, `--tools`,
`--mode json`, `/compact`, `/resume`). The last big remaining gap was
**session branching**: pi's `/fork` / `/clone` / `/tree` let you back up
to any past message and try a different path without losing the
original. This is the core iterative-debugging power feature for a
coding agent — "let me try this fix instead" without burning the
original transcript.

The design decision for glue is how to represent the branching in the
session model.

## Decision

**Sessions-as-leaves.** Each fork creates a new session id whose
`SessionState.Metadata` carries `parent_session_id` and
`parent_message_index`. The `Store` interface and `SessionState`
schema are unchanged. The tree is layered on top via metadata; a
query walks the parent pointers up to find the root and walks down by
listing sessions and filtering on the parent key.

### Rejected alternative: tree-in-one-session

Storing the tree inside a single `SessionState` (messages with parent
refs, branches navigable via a different cursor field) would have been
the more "interesting" model, but it required:

- A breaking change to `SessionState.Messages` (slice → tree).
- A breaking change to every existing store implementation.
- A breaking change to every existing consumer that walked messages.

Per ADR-0013, breaking changes are allowed pre-1.0, but the gain here
is questionable. Branches are user concepts visible only in the TUI;
the loop, providers, and tool execution all see a linear transcript.
Bending the durable schema to match a UI concept is the wrong way
around. Sessions-as-leaves keeps the schema unchanged and the tree
view becomes a metadata read on top.

### Metadata schema

Namespaced to avoid colliding with user metadata:

- `glue/tree:parent_session_id` (string)
- `glue/tree:parent_message_index` (int)

`SessionParent(state)` returns these typed, handling the
`int → float64` round trip the file store imposes on JSON-decoded
integers.

### Public API

- `Agent.ForkSession(ctx, srcID string, atMessage int, newID string) error` —
  copies `messages[0:atMessage]` from `srcID` into a fresh `newID`,
  writes the parent metadata. Validates `atMessage` in `[0, len]`.
  Returns `ErrSessionNotFound` if the source is absent.
- `Agent.CloneSession(ctx, srcID, newID string) error` — full copy,
  preserves any existing parent linkage so a clone of a forked
  session remains attributable to the same root.
- `SessionParent(state) (id string, atIndex int, ok bool)` — reads the
  metadata back. `ok=false` for a root session or malformed metadata.
- `ErrSessionNotFound` — new typed sentinel returned by both methods.

The additions are pure surface — nothing existing changes. Per
ADR-0013 this is not a `**Breaking:**` CHANGELOG entry.

### TUI integration

Three slash commands in `cmd/glue/tui`:

- `/fork [N]` — defaults to "branch from the message immediately
  before the most recent user turn" (the "redo my last prompt"
  workflow); `N` overrides with an explicit message index. Auto-generates
  a `tui:<shortid>` id.
- `/clone` — full copy; preserves parent chain.
- `/tree` — modal showing the lineage of the current session. Walks
  the parent pointers up to a root, then descends DFS to build a
  flat-with-indentation rendering with `├─ / └─` glyphs, marks the
  current node with `◉` (others `●`), tags non-root nodes with
  `forked@N`. `↑/↓` navigate; Enter switches; Esc cancels.

The tree builder uses `Agent.ListSessions` + per-session `Store.Load`
to read parent pointers. For deeply-nested trees this is O(N²) but N
is small in practice (a few dozen related sessions at most for a
coding-agent workflow); an indexed parent→child lookup is a follow-up
if it ever matters.

## Loopholes and Fixes

- **Loophole: a fork at message N duplicates `messages[0:N]` into the
  new session.** This is a real space cost. For typical coding-agent
  transcripts (dozens to low hundreds of messages, each a few KB) it's
  fine; the alternative (shared message storage with copy-on-write)
  isn't worth the complexity. Not fixed.

- **Loophole: tree query is O(sessions × parent-walk-depth).** Fine
  for typical use, but the sqlite store could index
  `glue/tree:parent_session_id` for fast lookup. Not fixed in this
  ADR; filed as follow-up if it ever matters.

- **Loophole: `/clear` nukes the transcript with no fork linkage.**
  Today `/clear` starts a fresh session id with the welcome card.
  Tomorrow it could record a fork from message 0 of the current
  session, keeping the cleared session reachable in `/tree`. Not
  fixed in this ADR.

- **Loophole: stale-parent reference.** If the parent session is
  deleted from the store (e.g. `Store.Delete`), a fork's metadata
  still points at the missing id. `buildSessionTree` handles this
  gracefully by treating any node whose parent isn't in the store as a
  root.

- **Loophole: no daemon protocol surface for `/tree`.** A future
  `glue connect --tree` could render the same view remotely. Not
  fixed; filed as a follow-up if the daemon path gets used for coding
  agents.

## Consequences

- Branching becomes a first-class part of `cmd/glue`'s coding-agent
  workflow without breaking any consumer of the library.
- The `glue.Store` interface stays as small as it was; tree semantics
  live in metadata + a small Agent surface and a TUI renderer.
- Locking `v1.0.0` later (per ADR-0013) doesn't get harder because of
  this ADR; the additions are forward-compatible.
