package glue

import (
	"context"
	"errors"
	"time"
)

// ErrSessionListingNotSupported is returned when the active [Store]
// does not implement [SessionLister]. Callers (e.g. a TUI session
// picker) should treat this as "no session catalog available" and
// degrade gracefully — show only the current session id, hide the
// picker, etc.
var ErrSessionListingNotSupported = errors.New("glue: store does not support session listing")

// ErrSessionNotFound is returned by [Agent.ForkSession] and
// [Agent.CloneSession] when the source session id does not exist in
// the store. The standard [Store.Load] returns found=false on the same
// condition; this typed error lets the tree-aware operations report it
// without leaking storage internals.
var ErrSessionNotFound = errors.New("glue: session not found")

// MetadataKeyParentSessionID and MetadataKeyParentMessageIndex are the
// namespaced keys that record a session's place in the session tree.
// They are set by [Agent.ForkSession] (and copied by
// [Agent.CloneSession] when the source already had them) into
// [SessionState.Metadata]; [SessionParent] reads them back.
//
// Layered on top of the existing Store interface so the tree adds zero
// breaking changes to disk format — a fork is just a new session whose
// metadata points at its parent.
const (
	MetadataKeyParentSessionID    = "glue/tree:parent_session_id"
	MetadataKeyParentMessageIndex = "glue/tree:parent_message_index"
)

// SessionParent returns the parent session id and message index a
// forked session was branched from. ok is false for a root session
// (no parent metadata) or for malformed metadata.
func SessionParent(state SessionState) (id string, atIndex int, ok bool) {
	if state.Metadata == nil {
		return "", 0, false
	}
	rawID, hasID := state.Metadata[MetadataKeyParentSessionID]
	rawIdx, hasIdx := state.Metadata[MetadataKeyParentMessageIndex]
	if !hasID || !hasIdx {
		return "", 0, false
	}
	idStr, _ := rawID.(string)
	if idStr == "" {
		return "", 0, false
	}
	// Index may be int (from in-memory construction) or float64
	// (after a JSON round-trip through the file store).
	switch v := rawIdx.(type) {
	case int:
		atIndex = v
	case int64:
		atIndex = int(v)
	case float64:
		atIndex = int(v)
	default:
		return "", 0, false
	}
	return idStr, atIndex, true
}

// SessionStateVersion is the on-disk version tag for [SessionState].
const SessionStateVersion = 1

// Store persists Glue session state. Implementations are expected to be
// goroutine-safe across distinct session ids; concurrent calls for the same
// id within a single session are serialized by the session itself.
//
// Load returns found=false when the id is not present, with a zero-valued
// SessionState and a nil error. Save must be atomic against partial writes.
// Delete must be idempotent — removing a missing id is a no-op success.
type Store interface {
	Load(ctx context.Context, id string) (SessionState, bool, error)
	Save(ctx context.Context, id string, state SessionState) error
	Delete(ctx context.Context, id string) error
}

// SessionLister is an optional store capability for provider-free session
// history browsers.
type SessionLister interface {
	ListSessions(ctx context.Context, opts ListSessionsOptions) ([]SessionSummary, error)
}

// ListSessionsOptions filters and pages a session history listing.
type ListSessionsOptions struct {
	// Prefix restricts results to session ids beginning with Prefix.
	Prefix string

	// Limit caps returned rows. Non-positive values use the store default.
	Limit int

	// Offset skips rows after filtering and ordering. Negative values are
	// treated as zero.
	Offset int
}

// SessionSummary is provider-free metadata about one stored session.
type SessionSummary struct {
	ID                string    `json:"id"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	Messages          int       `json:"messages"`
	UserMessages      int       `json:"user_messages"`
	AssistantMessages int       `json:"assistant_messages"`
}

// SessionState is the durable representation of a session.
type SessionState struct {
	Version   int            `json:"version"`
	ID        string         `json:"id"`
	Messages  []Message      `json:"messages,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}
