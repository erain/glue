package glue

import (
	"context"
	"errors"
	"time"
)

// Searcher is the optional capability a Store may implement to
// support cross-session content search. Stores that do not implement
// Searcher cause [Agent.SearchSessions] and [Session.Search] to return
// [ErrSearchNotSupported].
//
// Designed in docs/adr/0007-memory-layer.md §3.
type Searcher interface {
	Search(ctx context.Context, query string, opts SearchOptions) ([]SearchHit, error)
}

// SearchOptions controls a Searcher.Search call. Zero values mean
// "no filter" for SessionID / Since / Until, "default" for Limit
// (20), and zero Offset.
type SearchOptions struct {
	// SessionID restricts results to one session. Empty means
	// "across all sessions".
	SessionID string

	// Limit caps the number of hits returned. Zero falls back to
	// DefaultSearchLimit; values larger than MaxSearchLimit are
	// clamped silently.
	Limit int

	// Offset skips this many hits. Useful for paging.
	Offset int

	// Since restricts results to messages with timestamps ≥ Since.
	// Zero means no lower bound.
	Since time.Time

	// Until restricts results to messages with timestamps ≤ Until.
	// Zero means no upper bound.
	Until time.Time
}

// SearchHit is one row returned by a Searcher.
type SearchHit struct {
	// SessionID identifies which session this message lives in.
	SessionID string

	// Index is the ordinal position of the message within its session
	// (zero-based).
	Index int

	// Role is the message author role.
	Role MessageRole

	// Snippet is a Searcher-supplied excerpt with highlighting around
	// the matched terms (FTS5's snippet() output for the SQLite
	// backend; markers are << and >>).
	Snippet string

	// Score is the implementation-specific relevance score. For the
	// SQLite/FTS5 backend this is BM25 — lower is better.
	Score float64

	// Timestamp is the message's CreatedAt (or the session updated_at
	// if the message had no per-message timestamp).
	Timestamp time.Time
}

// Search-related limits. Exported so callers can size paging.
const (
	DefaultSearchLimit = 20
	MaxSearchLimit     = 100
)

// ErrSearchNotSupported is returned when the active [Store] does not
// implement [Searcher]. Callers can fall back gracefully (e.g. show
// "search not configured" in a UI).
var ErrSearchNotSupported = errors.New("glue: store does not support search")

// SearchOption configures a [SearchOptions] via functional-options.
type SearchOption func(*SearchOptions)

// WithLimit overrides [SearchOptions.Limit]. Non-positive values
// fall back to [DefaultSearchLimit]; values > [MaxSearchLimit] are
// clamped.
func WithLimit(n int) SearchOption {
	return func(o *SearchOptions) { o.Limit = n }
}

// WithOffset overrides [SearchOptions.Offset]. Negative values are
// treated as zero.
func WithOffset(n int) SearchOption {
	return func(o *SearchOptions) { o.Offset = n }
}

// WithSessionID restricts the search to a single session.
func WithSessionID(id string) SearchOption {
	return func(o *SearchOptions) { o.SessionID = id }
}

// WithSince sets the lower time bound (inclusive).
func WithSince(t time.Time) SearchOption {
	return func(o *SearchOptions) { o.Since = t }
}

// WithUntil sets the upper time bound (inclusive).
func WithUntil(t time.Time) SearchOption {
	return func(o *SearchOptions) { o.Until = t }
}

// SearchSessions returns FTS-ranked hits across all sessions stored
// by the agent's Store. Returns [ErrSearchNotSupported] if the active
// store does not implement [Searcher].
//
// query is passed straight through to the underlying Searcher. For
// the SQLite/FTS5 backend the syntax is FTS5's MATCH expression — a
// bare word matches that word; quoted phrases match exactly; AND /
// OR / NOT are supported.
func (a *Agent) SearchSessions(ctx context.Context, query string, opts ...SearchOption) ([]SearchHit, error) {
	if a == nil {
		return nil, errors.New("glue: nil agent")
	}
	searcher, ok := a.store.(Searcher)
	if !ok || a.store == nil {
		return nil, ErrSearchNotSupported
	}
	options := buildSearchOptions(opts)
	return searcher.Search(ctx, query, options)
}

// Search restricts [Agent.SearchSessions] to this session. Any
// [WithSessionID] in opts is overridden with this session's id.
func (s *Session) Search(ctx context.Context, query string, opts ...SearchOption) ([]SearchHit, error) {
	if s == nil || s.agent == nil {
		return nil, errors.New("glue: nil session")
	}
	scoped := append([]SearchOption(nil), opts...)
	scoped = append(scoped, WithSessionID(s.id))
	return s.agent.SearchSessions(ctx, query, scoped...)
}

func buildSearchOptions(opts []SearchOption) SearchOptions {
	o := SearchOptions{}
	for _, fn := range opts {
		if fn != nil {
			fn(&o)
		}
	}
	if o.Limit <= 0 {
		o.Limit = DefaultSearchLimit
	}
	if o.Limit > MaxSearchLimit {
		o.Limit = MaxSearchLimit
	}
	if o.Offset < 0 {
		o.Offset = 0
	}
	return o
}
