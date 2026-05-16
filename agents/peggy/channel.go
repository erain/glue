package peggy

import (
	"context"
	"strings"
)

// Channel is one binding from an external transport (Telegram, future
// TUI / web / IDE clients) to a Peggy agent. Implementations live in
// agents/peggy/channels/<name>; this package owns the contract.
//
// Channels are constructed with the *Peggy and the channel's own
// config (decoded from settings.json's channels.<name> subtree).
// Run blocks until the supplied context is cancelled or a fatal
// error occurs.
//
// Designed in docs/adr/0008-channel-adapter.md.
type Channel interface {
	// Name returns a stable, lowercase, single-word identifier
	// ("telegram", "slack", "tui", …). Used in session-id prefixes
	// and in settings.json's channels map.
	Name() string

	// Run drives the channel's event loop. It returns nil on graceful
	// shutdown (ctx cancelled) and a non-nil error only on fatal
	// setup or steady-state failure the channel cannot recover from.
	// Run must be safe to call exactly once per Channel value.
	Run(ctx context.Context) error
}

// ChannelSessionID returns the conventional namespaced session id for
// a given channel and channel-native id. Channels are free to ignore
// this and pick their own scheme; the convention exists so a single
// store can hold CLI sessions, channel sessions, and the curated
// __memories__ session without collision.
//
// The channel name is lower-cased and trimmed; the id is used as-is.
// An empty channel name returns the id unchanged so calling
// ChannelSessionID("", x) is a no-op (useful in tests).
func ChannelSessionID(channel, id string) string {
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel == "" {
		return id
	}
	return channel + ":" + id
}
