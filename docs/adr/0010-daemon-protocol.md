# ADR 0010: Local Daemon Protocol For Multi-Channel Agents

## Status

Accepted. Filed under tracker
[#110](https://github.com/erain/glue/issues/110) / issue
[#159](https://github.com/erain/glue/issues/159) as the first M3 design
issue for Peggy v0.3 ("multi-channel daemon").

Implementation issues that follow this ADR:

- Neutral daemon server package and `cmd/glue serve`.
- `cmd/glue connect` local client / REPL.
- Peggy daemon wiring so CLI, Telegram, and future clients can share
  one Peggy process and one store.
- Permission request routing over the daemon protocol.

## Context

Peggy v0.2 can remember, use Telegram, and code with permission-gated
local tools, but every channel still owns its own process. The CLI runs
one prompt and exits; `peggy-telegram` runs a Telegram-only process.
That means:

- no shared in-memory permission decisions across channels;
- no single event stream that a TUI, CLI, or IDE can attach to;
- no one process where all channels share the same loaded provider,
  compactor, store handle, and lifecycle;
- no clean route for future clients to approve side effects unless
  they are embedded in the same process as the tool call.

OpenClaw's gateway model is the product inspiration: one long-running
agent process multiplexes multiple front doors. Hermes-Agent's
tool/RPC style is the protocol inspiration: clients should interact
with a narrow session-shaped API, not import the whole product.

ADR-0005's purity rule still controls the design:

> Every product concern enters glue only as an interface the host fills
> in, never as a default behavior with UI, channel, or policy baked in.

The daemon may be a reusable local transport layer, but it must not own
Peggy identity, Telegram policy, provider choice, or permission UI.

## Decision

### 1. Process model

M3 introduces a local daemon that owns exactly one constructed agent
host for the process lifetime. The primary product host is Peggy. The
daemon:

- owns the provider, store, compactor, tools, hooks, and permission
  adapter for that host;
- accepts prompt runs from local clients;
- streams loop events back to clients;
- routes side-effect permission requests to the client that owns the
  interaction;
- shuts down cleanly on SIGINT / SIGTERM.

Clients are thin:

- `cmd/glue connect` is the first local terminal client / REPL.
- Telegram may either keep the current in-process mode or become a
  daemon client in a later issue.
- Future TUI, IDE, and web clients speak the same protocol.

Core `glue` remains channel-blind. The reusable part is a daemon
transport over a small host interface:

```go
type DaemonHost interface {
    Session(ctx context.Context, id string) (*glue.Session, error)
    Close() error
}
```

The concrete interface may grow during implementation, but the
direction is fixed: the daemon calls session-shaped APIs; products
construct the host and own policy.

### 2. Transport

The v1 protocol is HTTP plus Server-Sent Events (SSE).

Reasons:

- HTTP is easy for CLI/TUI/web/IDE clients to consume.
- SSE is enough for one-way run events from daemon to client.
- Permission decisions and cancellation are ordinary HTTP writes.
- The protocol can be served on localhost without WebSocket state
  machinery.

All v1 routes live under `/v1`. JSON requests and responses use
snake_case fields. Timestamps are RFC3339 UTC strings. Unknown JSON
fields are ignored by the server so additive client changes do not
break older daemons.

### 3. Local security defaults

The daemon is local-first, not a hosted multi-user server.

Default behavior:

- bind only to `127.0.0.1`;
- choose an ephemeral port unless `--listen` is explicit;
- generate a random bearer token on startup;
- write connection metadata to a mode-`0600` file under the user's
  runtime/config directory, e.g. `~/.local/share/glue/daemon.json`;
- require `Authorization: Bearer <token>` on every route except
  `GET /v1/health`.

Explicit hosted or LAN use must opt in with flags such as `--listen`
and an explicit token source. There is no unauthenticated public mode
in M3.

The metadata file shape is:

```json
{
  "version": 1,
  "base_url": "http://127.0.0.1:43129",
  "token": "redacted-random-token",
  "pid": 12345
}
```

The daemon must never log bearer tokens, provider keys, Telegram bot
tokens, or raw permission args unless a future explicit debug flag says
so.

### 4. Tool catalog

Daemon clients can inspect the hosted agent's current tool surface:

```http
GET /v1/tools
```

Response:

```json
{
  "tools": [
    {
      "name": "mcp_filesystem_read_file",
      "description": "MCP filesystem: Read a file",
      "parameters": { "type": "object" },
      "requires_permission": true,
      "permission_action": "mcp_call",
      "permission_target_preview": "filesystem.read_file"
    }
  ]
}
```

`permission_target_preview` is best-effort because some local tools
derive the final target from call arguments. Hosts that do not expose a
catalog return an empty `tools` array.

### 5. Runs and sessions

A prompt run is started with:

```http
POST /v1/sessions/{session_id}/runs
Authorization: Bearer <token>
Content-Type: application/json

{
  "text": "run the tests and fix the failure",
  "client_id": "cli:tty-1234",
  "role": "",
  "model": "",
  "max_turns": 0,
  "options": {}
}
```

The response is:

```json
{
  "run_id": "run_01HY...",
  "session_id": "default",
  "events_url": "/v1/runs/run_01HY.../events"
}
```

Then the client connects:

```http
GET /v1/runs/{run_id}/events
Accept: text/event-stream
Authorization: Bearer <token>
```

The daemon streams every event for that run and closes the SSE stream
after `run_done` or `run_error`.

Session ids keep the existing convention:

- CLI sessions use bare ids such as `default` or `work`.
- Telegram sessions use `telegram:<chat_id>`.
- Future channels use `<channel>:<channel-native-id>`.

The underlying `glue.Session` already serializes one prompt at a time
per session. The daemon may accept concurrent runs for different
sessions, but same-session runs are serialized by the session lock.

### 6. Event envelope

Every SSE message uses the event type as the SSE `event:` name and a
JSON envelope in `data:`.

```json
{
  "version": 1,
  "id": "evt_01HY...",
  "seq": 12,
  "run_id": "run_01HY...",
  "session_id": "default",
  "time": "2026-05-23T20:46:00Z",
  "type": "text_delta",
  "payload": {
    "delta": "hello"
  }
}
```

`seq` is monotonically increasing within one run. Clients use it for
debugging and duplicate suppression; v1 does not require event replay
after disconnect.

The daemon forwards existing `glue.Event` values as protocol events
using their current names where possible:

- `run_start`
- `loop_start`
- `turn_start`
- `message_start`
- `text_delta`
- `tool_start`
- `tool_end`
- `message_end`
- `turn_end`
- `loop_end`
- `run_done`
- `run_error`

`run_start`, `run_done`, and `run_error` are daemon-level events. The
others are loop events.

`thinking_delta` is reserved even though `glue.Event` does not expose
it today. Providers may emit thinking internally, and a future
additive loop event can map to this protocol name.

### 7. Permission flow

The daemon installs a `glue.Permission` implementation for the hosted
agent. That implementation does not decide policy itself. It brokers a
request to the owning client and waits.

When a side-effecting tool reaches permission, the daemon emits:

```text
event: permission_request
data: {
  "version": 1,
  "id": "evt_01HY...",
  "seq": 18,
  "run_id": "run_01HY...",
  "session_id": "default",
  "time": "2026-05-23T20:46:03Z",
  "type": "permission_request",
  "payload": {
    "permission_id": "perm_01HY...",
    "request": {
      "tool": "shell_exec",
      "action": "exec",
      "target": "go test ./...",
      "args": {"argv": ["go", "test", "./..."]},
      "session_id": "default"
    },
    "expires_at": "2026-05-23T20:56:03Z"
  }
}
```

The owning client answers:

```http
POST /v1/runs/{run_id}/permissions/{permission_id}/decision
Authorization: Bearer <token>
Content-Type: application/json

{
  "allow": true,
  "reason": "",
  "remember_for": "session"
}
```

`remember_for` uses string values on the wire:

- `never`
- `session`
- `session_target`
- `forever`

The daemon maps these to `glue.RememberScope`. The daemon may cache
positive remembered decisions in memory; persistent permission policy
is a later issue.

Only the run owner may answer a permission request in v1. If a
non-owner client attempts to answer, the daemon returns `403`.

Safe defaults:

- If the owner stream disconnects while a permission request is
  pending, the daemon denies with a model-visible reason.
- If the request times out, the daemon denies with a model-visible
  reason.
- If the run is canceled, the permission wait returns context
  cancellation and the run ends as canceled.
- If the daemon is shutting down, pending permission requests are
  denied before shutdown when possible.

This mirrors v0.2's CLI and Telegram behavior: side effects never run
because a UI disappeared.

### 8. Cancellation and disconnects

The run owner can cancel with:

```http
DELETE /v1/runs/{run_id}
Authorization: Bearer <token>
```

The daemon cancels the run context, emits `run_error` with code
`canceled`, and closes the SSE stream.

For v1, disconnecting the owner SSE stream also cancels the run. This
keeps resource ownership simple for terminal clients. Detached
background runs are a future additive feature and will need replay or
query endpoints.

### 9. Error envelope

HTTP errors use:

```json
{
  "error": {
    "code": "invalid_request",
    "message": "text is required",
    "retryable": false
  }
}
```

Initial v1 codes:

- `invalid_request`
- `unauthorized`
- `forbidden`
- `not_found`
- `conflict`
- `canceled`
- `provider_error`
- `internal`

`run_error` SSE events use the same shape in `payload.error`.

### 10. Session lookup

M3 needs enough session lookup for clients to render history and switch
sessions:

```http
GET /v1/sessions/{session_id}
GET /v1/sessions/{session_id}/messages
GET /v1/sessions?query=<text>&limit=20
```

The search route maps to existing `Agent.SearchSessions` when the
store implements it. Prefix search for channel ids remains a store
follow-up; v1 may support exact session id filters first.

### 11. Consequences

- Core `glue` does not gain channel concepts or UI policy.
- Permission UX remains in the client: CLI can render `[y/s/t/n]`,
  Telegram can render inline buttons, and a TUI can render a modal.
- `cmd/glue serve` can be tested with fake hosts before Peggy is wired
  in.
- The protocol is intentionally small enough for curl-based smoke tests
  and for a future browser/TUI client.

## Implementation Sequence

1. Add the neutral daemon server package with host/session adapters,
   bearer-token auth, run lifecycle, SSE event streaming, and focused
   tests against a fake host.
2. Add `cmd/glue serve` for a local daemon using the existing default
   agent runner shape as the first smoke path.
3. Add `cmd/glue connect` as a local terminal client that starts runs,
   consumes SSE, displays text/tool events, answers permission requests,
   and cancels cleanly.
4. Add Peggy daemon wiring so a single Peggy process can serve CLI and
   Telegram clients against one store.
5. Move Telegram to optional daemon-client mode while keeping the
   existing standalone `peggy-telegram` path as a simple deployment.

## Non-Goals

- No public hosted multi-user auth model.
- No WebSocket protocol in v1.
- No detached background runs or event replay in v1.
- No persistent permission policy in this ADR.
- No MCP client support in this ADR.
