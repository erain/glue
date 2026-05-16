// Package telegram is the Telegram-bot channel adapter for Peggy. It
// implements peggy.Channel against the Telegram Bot API using a
// stdlib-only HTTP client (no third-party SDK).
//
// v0.1 supports text messages with long-polling and a chat-id
// allowlist. Edit-in-place streaming, image / voice / file messages,
// and webhook-mode are deferred follow-ups.
//
// Design: docs/adr/0008-channel-adapter.md.
package telegram
