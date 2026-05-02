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

// SessionState is the durable representation of a session.
type SessionState struct {
	Version   int            `json:"version"`
	ID        string         `json:"id"`
	Messages  []Message      `json:"messages,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}
