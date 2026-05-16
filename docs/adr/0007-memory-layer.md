# ADR 0007: Memory Layer — Summarizing Compactor + SQLite/FTS5 Store

## Status

Accepted. First implementation issues that follow this ADR (filed
under tracker [#110](https://github.com/erain/glue/issues/110) / M1):

- `glue.SummarizingCompactor` — token-aware compactor that summarizes
  older messages via the configured `Provider`.
- `stores/sqlite` — SQLite-backed `Store` with FTS5 over message text.
- `glue.Agent.SearchSessions` / `glue.Session.Search` — public
  cross-session retrieval API on top of an optional `Searcher`
  capability.

## Context

Peggy's headline goal (tracker
[#110](https://github.com/erain/glue/issues/110)) is "she remembers
across sessions." Two pieces of the framework need to grow:

1. **Within a session.** The existing `KeepRecentMessages` compactor
   ([ADR-0002](0002-context-compaction.md)) drops older context
   silently. That is correct for a one-shot agent like
   `glue-review`; it is wrong for a long-running assistant where the
   user mentions a fact in turn 30 that needs to influence turn 200.
   ADR-0002 already anticipated a token-aware drop-in; this ADR
   designs it.
2. **Across sessions.** The current `Store` interface only loads and
   saves a single session by id. Recall like "what did I tell you
   about my Australian Shepherd in March?" requires content-addressed
   retrieval across all stored sessions. Hermes-Agent uses SQLite +
   FTS5 for this, and it is fast, dependency-light, and well-trodden;
   we adopt the same pattern.

ADR-0005 placed both pieces in scope (compaction trigger lifted,
storage backends in scope as extension packages). This ADR specifies
the contracts and storage layout so the three implementation issues
can be small.

## Decision

### 1. `SummarizingCompactor` — token-aware, provider-summarized

A new `Compactor` implementation (lives in the core `glue` package
alongside `KeepRecentMessages`):

```go
type SummarizingCompactor struct {
    Provider       Provider      // required; reused from the agent in v1
    Model          string        // optional override; falls back to agent default
    TargetTokens   int           // soft cap on the post-compaction transcript
    KeepRecent     int           // last N messages always retained verbatim
    SystemPrompt   string        // override for the summarizer's system prompt
}

func (c *SummarizingCompactor) Compact(ctx context.Context, in []Message) ([]Message, error)
```

Behavior:

- Use a **word-count proxy for token counting** in v1: ≈0.75 tokens
  per word for English (a widely-used heuristic that is fine for a
  compaction trigger; it does not need to be exact). The function is
  unexported and replaceable; a future PR can swap in a real
  tokenizer per provider without changing the public surface.
- If the estimated total tokens are below `TargetTokens`, return the
  input unchanged. The Agent's existing `CompactionThreshold` is a
  message-count gate; this compactor adds the token-budget gate on
  top of it.
- Otherwise, partition into `older = in[:len(in)-KeepRecent]` and
  `kept = in[len(in)-KeepRecent:]`. Build a single-turn prompt asking
  the provider to summarize `older` while preserving facts,
  decisions, names, dates, and outcomes (the system prompt is
  documented and overrideable). Call `Provider.Stream` with that
  prompt, gather the text response, and emit a single assistant-role
  marker message that combines:
  - the summary text as a `text` content part, and
  - `Metadata["compaction"] = "summarizing"`,
  - `Metadata["original_message_count"] = len(older)`,
  - `Metadata["original_first_ts"] = ...`, `original_last_ts = ...`
    (if the older messages carry timestamps in their metadata).
- Return `[marker] ++ kept`.
- If summarization fails, **do not silently fall back** to dropping
  context. Return the error. The Agent should treat compaction
  errors as prompt errors; callers can choose to retry or to wire
  `KeepRecentMessages` as a fallback explicitly.
- Cancellation: the summarization `Provider.Stream` runs under the
  caller's `ctx`. A cancelled compaction aborts the prompt.

Composes with `Compactor`/`CompactorFunc` — no changes to the
existing interface or `AgentOptions`. The default behavior of glue
remains "no compactor configured." `KeepRecentMessages` stays as the
cheap default for short-lived agents.

### 2. `stores/sqlite` — SQLite + FTS5 Store

New extension package `stores/sqlite` that implements `glue.Store`
and (optionally) the new `Searcher` capability. `stores/file` stays
as the simple default; both implement the same `Store` contract.

**Dependency**: `modernc.org/sqlite` (pure Go, CGO-free). The binary
size cost is ~6 MB statically linked, accepted to keep glue's "go
build anywhere" property. CGo-based `mattn/go-sqlite3` is faster but
breaks cross-compilation and adds a C toolchain requirement; the
trade is not worth it for an assistant agent.

**One DB file per Store instance.** Multiple sessions share a single
file. The DB lives wherever the constructor is told to put it:

```go
package sqlite

type Options struct {
    Path     string        // required; ":memory:" allowed for tests
    Timeout  time.Duration // busy timeout; default 5s
}

func Open(opts Options) (*Store, error)
func (s *Store) Close() error
```

**WAL mode** is set on open (`PRAGMA journal_mode=WAL`) for
concurrent readers (an active writer plus the future search-while-
writing case from the daemon).

**Schema**:

```sql
CREATE TABLE IF NOT EXISTS sessions (
    id            TEXT PRIMARY KEY,
    created_at    INTEGER NOT NULL,    -- unix seconds
    updated_at    INTEGER NOT NULL,
    metadata_json TEXT
);

CREATE TABLE IF NOT EXISTS messages (
    rowid        INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id   TEXT NOT NULL,
    ord          INTEGER NOT NULL,     -- index in session
    role         TEXT NOT NULL,
    content_text TEXT NOT NULL,        -- concatenated text parts, for FTS
    content_json TEXT NOT NULL,        -- full []ContentPart serialized
    ts           INTEGER NOT NULL,     -- unix seconds (== session.updated_at at insert)
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE,
    UNIQUE (session_id, ord)
);

CREATE INDEX IF NOT EXISTS messages_session_idx ON messages(session_id);
CREATE INDEX IF NOT EXISTS messages_ts_idx      ON messages(ts);

CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
    content_text,
    content='messages',
    content_rowid='rowid',
    tokenize='unicode61 remove_diacritics 2'
);

-- Triggers keep FTS5 in sync with messages.
CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
  INSERT INTO messages_fts(rowid, content_text) VALUES (new.rowid, new.content_text);
END;
CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, content_text) VALUES('delete', old.rowid, old.content_text);
END;
CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, content_text) VALUES('delete', old.rowid, old.content_text);
  INSERT INTO messages_fts(rowid, content_text)                 VALUES (new.rowid, new.content_text);
END;
```

We use FTS5's **external-content** form so message text is stored
once (in `messages`) and FTS5 maintains a pure index. The triggers
keep them in sync. The tokenizer is `unicode61` with diacritic
removal — robust enough for English / European text without
sacrificing accent-insensitive search.

**Save semantics** (`Store.Save(ctx, id, SessionState)`):

- Single `BEGIN IMMEDIATE` transaction.
- Upsert into `sessions` (`id`, `created_at` preserved, `updated_at`
  bumped, `metadata_json`).
- `DELETE FROM messages WHERE session_id = ?` and re-insert all
  `state.Messages` with their content text concatenated from
  `ContentTypeText` parts only (tool calls and tool results
  preserve `content_json` but their text contribution is empty —
  function-call JSON is not what the user is searching for).
- Commit.

We chose "replace all messages on Save" rather than append-delta for
simplicity and to match the existing `stores/file` semantics. The
overhead is acceptable for the message counts a single session
realistically holds; if it becomes a problem (very long sessions, hot
save loop), a follow-up can introduce append-only path.

**Load semantics**: read `sessions` row + ordered `messages` rows;
materialize `SessionState`.

**Delete semantics**: `DELETE FROM sessions WHERE id = ?` (cascades
to messages via the FK).

### 3. `Searcher` capability + public `SearchSessions` API

A new optional interface in the core `glue` package:

```go
type Searcher interface {
    Search(ctx context.Context, query string, opts SearchOptions) ([]SearchHit, error)
}

type SearchOptions struct {
    SessionID string    // empty = all sessions
    Limit     int       // 0 = default 20; capped at 100
    Offset    int
    Since     time.Time // zero = no lower bound
    Until     time.Time // zero = no upper bound
}

type SearchHit struct {
    SessionID string
    Index     int          // message ord within the session
    Role      MessageRole
    Snippet   string       // FTS5 snippet() output (highlighted span)
    Score     float64      // BM25; lower = better (matches FTS5 convention)
    Timestamp time.Time
}

// ErrSearchNotSupported is returned by Agent.SearchSessions when the
// configured Store does not implement Searcher.
var ErrSearchNotSupported = errors.New("glue: store does not support search")
```

The Agent and Session expose:

```go
func (a *Agent) SearchSessions(ctx context.Context, query string, opts ...SearchOption) ([]SearchHit, error)
func (s *Session) Search       (ctx context.Context, query string, opts ...SearchOption) ([]SearchHit, error)
```

`Session.Search` is sugar for `Agent.SearchSessions` with
`SessionID` forced to `s.id`. Both return `ErrSearchNotSupported`
when the store does not implement `Searcher` — typed errors so
callers can fall back gracefully.

Functional-options pattern matches the rest of the API (`WithLimit`,
`WithSessionID`, `WithSince`, `WithUntil`, `WithOffset`).

`stores/sqlite` implements `Searcher` by issuing:

```sql
SELECT m.session_id, m.ord, m.role, m.ts,
       snippet(messages_fts, 0, '<<', '>>', '...', 16) AS snippet,
       bm25(messages_fts) AS score
FROM messages_fts
JOIN messages m ON m.rowid = messages_fts.rowid
WHERE messages_fts MATCH ?
  AND (:session_id = '' OR m.session_id = :session_id)
  AND (:since = 0    OR m.ts >= :since)
  AND (:until = 0    OR m.ts <= :until)
ORDER BY score ASC
LIMIT :limit OFFSET :offset;
```

`stores/file` does **not** implement `Searcher`. Callers using the
file store get `ErrSearchNotSupported`; that is intended — the file
store stays simple, and "I want search" is a signal to switch to
`stores/sqlite`.

### 4. Migration story

Moving an existing file-store session into the SQLite store:

- For v0.1, **no automatic migration.** A user who switches store
  backends starts fresh.
- The Peggy product can ship a one-shot tool (`peggy migrate-store
  --from file --to sqlite`) under M1's `agents/peggy` issues if it
  becomes needed. The tool reads with one store, writes with the
  other; that's it.

Encryption-at-rest is **out of scope** for v0.1 and noted as a
follow-up. SQLite supports SEE for encryption but it is a paid
extension; SQLCipher requires CGo. Either is a deliberate later
decision.

### 5. Test posture

`stores/sqlite` tests use `:memory:` and `t.TempDir()`-backed files;
no external dependency. The test set must cover:

- `Save` then `Load` round-trips messages, metadata, and timestamps.
- Re-`Save` replaces messages (no duplicates, FTS row count tracks).
- `Delete` removes session and cascades messages and FTS rows.
- Concurrent `Save` calls for different ids do not deadlock (WAL
  permits reads during writes; the busy timeout handles overlapping
  writers).
- FTS round-trip: insert messages with known text, search returns
  expected ids in BM25 order.
- Search filters: `SessionID`, `Since`, `Until`, `Limit`, `Offset`
  each tested in isolation.
- Tool-call messages with no text content: indexed with empty
  `content_text`; FTS5 must not return them for non-empty queries.

For `SummarizingCompactor` the test set must cover:

- Below-budget input returned unchanged.
- Over-budget input partitions into `older` and `kept`, calls the
  provider (use a fake provider that asserts the prompt shape),
  produces a marker message with the documented metadata keys.
- Summarization provider error propagates (no silent
  `KeepRecentMessages`-style fallback).
- Cancellation: cancelling `ctx` aborts the in-flight summarization.

For the new `Agent.SearchSessions` / `Session.Search` API:

- Without a `Searcher` store: returns `ErrSearchNotSupported`.
- With a `Searcher` store (use a fake that records calls): forwards
  query and options correctly; `Session.Search` overrides
  `SessionID`.

## Consequences

- One new optional dependency (`modernc.org/sqlite`) under a new
  extension package. The core `glue` import stays free of it.
- `KeepRecentMessages` and `stores/file` are **not** deprecated; they
  remain the right defaults for short-lived agents like
  `glue-review`. The new pieces are additive.
- `Searcher` is an optional capability check via interface type
  assertion in `Agent.SearchSessions`. Adding more optional store
  capabilities later (e.g., `Lister`, `Archiver`) follows the same
  pattern.
- The schema is the contract once shipped. Future schema changes
  must ship a migration; we will track this with a `schema_version`
  table starting at version 1.
- Token estimation is intentionally a heuristic; we will revisit
  when one of:
  - a provider's per-call token usage diverges materially from the
    estimate (we already capture usage on `Message.Metadata`), or
  - a real tokenizer becomes cheap enough to add without dragging in
    CGo or a 30 MB binary blob.
- `stores/file` plus `SummarizingCompactor` is itself a useful
  combination: an agent that compacts intelligently but does not
  need cross-session search.
- The Peggy product (`agents/peggy`) implements the `remember-this`
  and `recall` skills on top of this surface — no Peggy-specific
  glue changes required for memory.
