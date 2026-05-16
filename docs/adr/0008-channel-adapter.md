# ADR 0008: Channel Adapter Pattern

## Status

Accepted. Implementation issues that follow:

- `agents/peggy/channels/telegram` — first concrete channel adapter
  (M1 of tracker [#110](https://github.com/erain/glue/issues/110)).
- A future `agents/peggy/channels/<…>` per channel we add.

The same pattern is the basis for the M3 daemon: daemon clients are
"channels" that speak HTTP+SSE instead of MTProto.

## Context

Peggy v0.1 is a single-prompt CLI. The product brief
([#110](https://github.com/erain/glue/issues/110)) calls for being
"reachable from REPL today, Telegram next, TUI/web/IDE later" — and
v0.3 puts a long-running daemon underneath all of them. We need a
pattern for binding external transports to the agent that:

1. **Keeps core `glue` channel-blind.** ADR-0005 §1 is the load-bearing
   rule: every product concern lands in `glue` only as an interface
   the host fills in, never as a default. Channels are the cleanest
   case of that rule — there is no reason for the framework to know
   about Telegram, Slack, MTProto, WebSockets, or HTTP+SSE.
2. **Lets channels land incrementally.** Telegram first; web / IDE /
   daemon-attached TUI later. Each is independent of the others, and
   no new channel forces a refactor of the existing ones.
3. **Survives the daemon transition.** When `cmd/glue serve` arrives
   in M3, daemon clients should fit the same shape — they're just
   another channel.

OpenClaw's gateway model is the inspiration: in their architecture
the gateway owns the agent and channels register against it. We
borrow the multiplexing idea but invert ownership: in glue, the
agent (Peggy) is supreme and channels call into it, not the other
way around.

## Decision

### 1. Channels live in `agents/peggy/channels/<name>`

A channel adapter is a small Go package under
`agents/peggy/channels/<name>` that imports
`github.com/erain/glue/agents/peggy` and the upstream client library
for that transport (e.g. a Telegram bot SDK).

Core `glue` never imports a channel. `agents/peggy` itself does not
import any specific channel either — the wiring happens in the
binary command (e.g. `agents/peggy/cmd/peggy` for the CLI, and a
future `agents/peggy/cmd/peggyd` or `cmd/peggy-telegram` for a
Telegram-only daemon).

The dependency direction is:

```text
agents/peggy/cmd/<bin> -> agents/peggy -> glue -> loop
agents/peggy/cmd/<bin> -> agents/peggy/channels/<name> -> agents/peggy
agents/peggy/channels/<name> -> upstream SDK (telegram-bot-go, etc.)
```

`agents/peggy` itself stays free of channel-specific dependencies.

### 2. The `Channel` interface

Defined inside `agents/peggy`, NOT in core `glue`:

```go
package peggy

// Channel is one binding from an external transport to a Peggy
// agent. Implementations live in agents/peggy/channels/<name>; this
// package owns the contract.
//
// Channels are constructed with the *Peggy and the channel's own
// config (decoded from settings.json's channels.<name> subtree).
// Run blocks until the supplied context is cancelled or a fatal
// error occurs.
type Channel interface {
    // Name returns a stable, lowercase, single-word identifier
    // (e.g. "telegram"). Used in session-id prefixes and in
    // settings.json's channels map.
    Name() string

    // Run drives the channel's event loop. It returns nil on graceful
    // shutdown (ctx cancelled) and a non-nil error only on fatal
    // setup or steady-state failure the channel cannot recover from.
    // Run must be safe to call exactly once per Channel value.
    Run(ctx context.Context) error
}
```

Channels that need cleanup separate from `ctx`-cancellation should
also implement `io.Closer`. A future `RunChannels(ctx, ...Channel)`
helper can run multiple channels concurrently — the contract is
already goroutine-safe.

### 3. How channels reach Peggy

Channels are constructed with a `*Peggy`. Inbound messages are mapped
to `peggy.Prompt(ctx, sessionID, text, ...)`. Outbound events are
read from `Session.Subscribe(func(glue.Event))` on the
per-conversation session.

A typical Telegram adapter loop:

```go
for update := range bot.Updates() {
    if !allowed(update.From) { continue }
    sessionID := peggy.ChannelSessionID("telegram", update.ChatID)
    text, err := p.Prompt(ctx, sessionID, update.Text, replyWriter(bot, update))
    if err != nil { bot.Reply(update, "(error: %s)", err) }
}
```

Channels do not need a new method on `*Peggy`. They use the same
`Prompt` the CLI uses. Streaming text reaches the channel through
whatever `io.Writer` the channel passes as the `stdout` argument
(typically an adapter that batches deltas into per-message edits or
new messages).

### 4. Session-id convention

Channels namespace session ids: `<channel>:<channel-specific-id>`,
e.g. `telegram:12345`. This is a convention, not enforced — channels
are free to ignore it. A helper:

```go
package peggy

// ChannelSessionID returns the conventional session id for a given
// channel and channel-native id. Channels are free to ignore this
// and use their own scheme.
func ChannelSessionID(channel, id string) string { ... }
```

The single-prompt CLI keeps using bare ids (`default`, `--session
foo`). The namespace prefix is what distinguishes a Telegram-driven
session in the sqlite store from a CLI-driven one of the same number.

This convention also makes `Agent.SearchSessions` queries easy:
"search only my Telegram conversations" is `WithSessionID(...)` with
a `LIKE 'telegram:%'` extension that the Searcher impl can grow
later (the v0.1 contract is exact-match only — a later additive
revision can add prefix matching).

### 5. Permissions and allowlists

Channels that accept input from anyone reachable (Telegram bots,
webhook endpoints, public chat rooms) MUST gate the inbound side on
an allowlist of identities the user has configured. There is no
single `peggy.Allowlist` type — the identity shape differs per
channel (Telegram uses chat / user ids, Slack uses workspace user
ids, etc.) — so allowlist enforcement lives inside each channel
package.

The CLI binary is **not** subject to allowlisting (running a CLI is
its own permission). The Telegram channel **is**. The default
behavior of a misconfigured channel — allowlist empty, no
allow-everyone flag — is to refuse all inbound messages with a clear
error logged to stderr.

A formal `Permission` interface in core glue arrives in M2 (per
ADR-0005). When it lands, channels gain a second gate: even
allowlisted users can be denied per-tool. For now the allowlist is
the only gate.

### 6. Configuration

Channel config lives under a top-level `channels` map in
`settings.json`, keyed by channel name:

```json
{
  "channels": {
    "telegram": {
      "bot_token_env": "PEGGY_TELEGRAM_TOKEN",
      "allow_chats": [12345, 67890]
    }
  }
}
```

`peggy.Settings` stores `Channels map[string]json.RawMessage` and
forwards each subtree to the channel package's decoder
(`telegram.DecodeConfig(raw json.RawMessage) (telegram.Config, error)`).
Channel packages own their schema.

### 7. Lifecycle (v0.1)

v0.1 runs **one channel per process**. A binary like
`agents/peggy/cmd/peggy-telegram` constructs a `*Peggy`, decodes
`settings.Channels["telegram"]`, constructs the channel, and calls
`Run(ctx)`. SIGINT / SIGTERM cancel the context; the channel returns
nil; the process exits.

Multi-channel-in-one-process is an M3 concern; the `Channel`
contract is already goroutine-safe so a future
`peggy.RunChannels(ctx, channels...)` helper can land without
revisiting this ADR.

### 8. What does NOT move into glue

- No `glue.Channel` type.
- No built-in channel adapters in core glue, `tools/`, or `cmd/`.
- No daemon-protocol coupling in this ADR — the daemon (M3) is a
  separate layer that channels may bind to instead of binding
  directly to a process-local `*Peggy`.

ADR-0005 §1's purity rule continues to hold: every channel concern
lives in the product (Peggy) packages, not the framework.

## Consequences

- Telegram lands as `agents/peggy/channels/telegram` plus an
  `agents/peggy/cmd/peggy-telegram` binary. Existing
  `agents/peggy/cmd/peggy` keeps working unchanged.
- Future channels (web, IDE, TUI) follow the same template — a
  one-package adapter plus a one-file binary.
- The M3 daemon ships its own channel adapter
  (`agents/peggy/channels/daemon`?) that speaks the HTTP+SSE
  protocol. The same `Run(ctx)` contract applies.
- Session-id namespacing means a single sqlite store can hold CLI
  sessions, Telegram conversations, future channel sessions, **and**
  the curated `__memories__` session without collision. Cross-channel
  search via `Agent.SearchSessions` works for free.
- Channel-specific permission policy is in the channel package; the
  M2 `Permission` interface (when it lands) is additive — channels
  consult it for tool-level decisions, not for inbound-allowlist
  decisions.
- This ADR underspecifies on purpose. If channels need a typed
  inbound event type, an outbound `Send()` method, or a multi-
  channel runner, those are additive changes — none invalidate this
  contract.
