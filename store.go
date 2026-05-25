package glue

import (
	"context"
	"time"
)

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
