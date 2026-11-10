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

### 4. Status and tool catalog

Daemon clients can inspect authenticated runtime status:

```http
GET /v1/status
```

Response:

```json
{
  "ok": true,
  "version": 1,
  "active_runs": 0,
  "tools_count": 4,
  "capabilities": ["runs", "events", "permissions", "tools", "skills", "roles", "memories", "memory_forget", "recall", "status"]
}
```

`GET /v1/health` remains unauthenticated and intentionally minimal;
`GET /v1/status` is for clients that already have daemon credentials.

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

Daemon clients can also inspect reusable host skills:

```http
GET /v1/skills
```

Response:

```json
{
  "skills": [
    {
      "name": "triage",
      "description": "Triage one issue"
    }
  ]
}
```

Hosts that do not expose a skill catalog return an empty `skills`
array. A host that does expose a catalog advertises the `skills`
capability in `/v1/status`.

Daemon clients can inspect reusable host roles:

```http
GET /v1/roles
```

Response:

```json
{
  "roles": [
    {
      "name": "reviewer",
      "description": "Review diffs carefully",
      "model": "gpt-5-codex"
    }
  ]
}
```

Hosts that do not expose a role catalog return an empty `roles` array.
A host that exposes a catalog advertises the `roles` capability in
`/v1/status`. Runs apply roles through the existing `role` field on
the run-start request.

Daemon clients can ask recall-capable hosts to search stored session
history without starting a run:

```http
POST /v1/recall
Authorization: Bearer <token>
Content-Type: application/json

{
  "query": "Australian Shepherd",
  "limit": 5,
  "memories_only": false
}
```

Response:

```json
{
  "hits": [
    {
      "session_id": "__memories__",
      "index": 0,
      "snippet": "The user's Australian Shepherd is named Inkblot.",
      "score": 1.23,
      "timestamp": "2026-05-24T12:00:00Z"
    }
  ]
}
```

Hosts that do not expose recall return `404`. A host that exposes
recall advertises the `recall` capability in `/v1/status`.

Daemon clients can inspect curated host memories without starting a
run:

```http
GET /v1/memories?limit=20
Authorization: Bearer <token>
```

Response:

```json
{
  "memories": [
    {
      "id": "mem_1779638400000000000_ab12cd34",
      "content": "The user prefers terse responses.",
      "tags": ["preference"],
      "timestamp": "2026-05-24T12:00:00Z"
    }
  ]
}
```

Hosts that do not expose memories return an empty `memories` array. A
host that exposes memories advertises the `memories` capability in
`/v1/status`.

Daemon clients can delete one curated host memory by stable id:

```http
DELETE /v1/memories/mem_1779638400000000000_ab12cd34
Authorization: Bearer <token>
```

Response:

```json
{
  "memory": {
    "id": "mem_1779638400000000000_ab12cd34",
    "content": "The user prefers terse responses.",
    "tags": ["preference"],
    "timestamp": "2026-05-24T12:00:00Z"
  }
}
```

Hosts that do not support memory deletion return `404`. A host that
supports deletion advertises the `memory_forget` capability in
`/v1/status`.

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

A skill run uses the same endpoint and event stream, but sends `skill`
and optional string arguments instead of `text`:

```json
{
  "skill": "triage",
  "arguments": {
    "issue": "GLUE-123"
  },
  "client_id": "cli:tty-1234",
  "role": "",
  "model": "",
  "max_turns": 0,
  "options": {}
}
```

`text` and `skill` are mutually exclusive. Skill runs forward through
`Session.Skill`, so they use the same provider streaming,
permission-request, usage, role, model, and session behavior as prompt
runs.

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

## Amendment: Protocol v2: durable, resumable, detached runs

Protocol v2 replaces v1. Section 8's disconnect cancellation rule no longer
applies: disconnecting a stream leaves its run running. Explicit DELETE
cancels it; `glue connect` sends DELETE on Ctrl-C.

Events are committed to SQLite before publication. SSE `id:` is the decimal
sequence; the envelope retains its random `id`. Reconnect with
`Last-Event-ID: <seq>` or `?after=<seq>` (query takes precedence) to replay
strictly later events and then follow live output. Invalid or negative
cursors return HTTP 400. Quiet streams send `: ping` every 15 seconds.

`glue serve --state-dir` defaults to `.glue/state`. Finished-run events
remain readable after restart. Recovering in-flight runs and pending
permissions is a separate step. Library hosts can supply `Options.StateDir`
and must close the server after stopping handlers and runs; omitting the
directory uses an isolated in-memory database.

### Run registry and interruption

`GET /v1/runs` returns `{ "runs": [...] }`, newest first by creation time
and ID. Optional `session_id` and `status` filters combine; `limit` defaults
to 50 (1–1000). `before` is the ID of a preceding row, used as an exclusive
cursor over that order. Invalid filters/cursors return 400.
`GET /v1/runs/{id}` returns the persisted row or 404, including client,
session, timestamps, last sequence, status, error, and pending permissions.
The pending list includes persisted requests awaiting a decision.

Statuses are `running`, `done`, `error`, `canceled`, and `interrupted`.
Registry changes commit with their events. Startup marks running rows
interrupted and appends one `run_interrupted` event in the same transaction;
repeating startup does not append another. Prompt runs are not resumed.

### Durable permission inspection

`GET /v1/runs/{id}/permissions` returns `{ "permissions": [...] }`, optionally
filtered by `state=pending|allowed|denied|expired`. Each record includes its
ID, run ID, request, creation/expiry timestamps, and state. Requests commit
with their SSE event; decisions persist before releasing the waiting tool.
The run detail includes `pending_permissions`.

Unknown, already decided, and expired decisions return 409. Timeout and
cancellation expire pending records. Startup expires requests belonging to
interrupted runs in the same transaction as interruption. Storage errors
return errors without releasing an unrecorded approval.

### Run retention and shutdown

`glue serve --retain-runs` defaults to `720h`. A background sweep removes
terminal runs older than that duration, along with their events and
permissions, in one transaction. Running runs and terminal rows exactly
at the cutoff remain. Deleted runs also leave the live index.
Library hosts can set `Options.RetainRuns` and `Options.SweepInterval`;
negative durations are invalid. Closing the server stops the sweep,
cancels and joins executions, and then closes SQLite. Executions stopped
by daemon shutdown are interrupted; explicit DELETE remains cancellation.

### Job inspection data

Job detail includes the persisted specification and workspace metadata plus
`iteration`, `checklist`, `usage`, and `pending_permission_count`, projected
from a consistent metadata/event snapshot. Permission history records expose
`id`, `request`, `state`, `created_at`, and `updated_at`; states are pending,
allowed, denied, and expired. Terminal completion expires any unsettled request.
Job lists use creation time and ID descending, with an exclusive `before`
job ID cursor. Validation identifies the snake_case field. Canceling a job
that has not started writes a canceled completion without preparing a workspace.

### Model catalog and shared streams

Authenticated `GET /v1/models` returns `providers` (provider name,
`default_model`, and curated `models` with thinking levels) and `defaults`
with `chat`/`coding` objects containing `provider` and `model`. Catalog reads
never construct providers or check credentials. Other methods return 405.

Runs and jobs use one resumable SSE writer: decimal sequence IDs,
Last-Event-ID/query precedence, keepalives, and detached disconnects. Readers
without a notification channel are polled every 100ms; notified run streams
remain event driven. Keepalives do not replay the preceding event batch.

### Authenticated job HTTP routes

- `POST /v1/jobs`: spec body; 201 `{ "job": ... }`; `?start=true` also starts it.
- `POST /v1/jobs/{id}/start`: 200 `{ "job": ... }`; running/waiting starts are no-ops.
- `GET /v1/jobs?status=&limit=&before=`: `{ "jobs": [...] }`; limit defaults to 50.
- `GET /v1/jobs/{id}`: the job detail object described above.
- `GET /v1/jobs/{id}/events`: durable resumable SSE with keepalives.
- `GET /v1/jobs/{id}/permissions?state=pending`: `{ "permissions": [...] }`.
- `POST /v1/jobs/{id}/permissions/{pid}/decision`: required boolean `allow`,
  optional `reason` and `remember_for`; returns `permission_id` and `accepted`.
- `GET /v1/jobs/{id}/result`: durable completion record from `<state>/jobs/<id>/result.json`
  for terminal jobs (`409 conflict` while `created`, `running`, or `waiting_for_input`).
- `DELETE /v1/jobs/{id}`: 202 with `job_id` and `canceled`.

Unknown jobs return 404, invalid requests 400 `invalid_request`, and conflicting
transitions 409. Job JSON bodies are limited to 1 MiB; unknown fields and
trailing JSON are rejected. A missing job manager leaves these routes disabled.

### CLI job hosting

`glue serve --jobs` enables job routes. `--coding` enables them by default;
explicit `--jobs=false` disables them. The manager shares `--state-dir` and
starts new jobs on request and recovers active jobs on restart. Ordinary-run
provider construction is deferred; idle startup needs no model credentials. Each job spec
selects its own coding provider/model independently of ordinary daemon runs.
Shutdown drains both run and job managers before returning storage errors.
`glue serve` accepts `--policy` (default `workspace-auto`) or `--policy-file`
(mutually exclusive) and applies the policy across daemon runs and jobs;
automatic `allow`/`deny` decisions emit `permission_auto {tool, target, decision, rule}`.

### Job manager ownership

A manager acquires a nonblocking exclusive OS lock on `<state-dir>/LOCK` before
opening storage. A second manager fails with a state-ownership error. Ownership
lasts until workers drain and storage closes, including failed startup cleanup.
Process death releases the lock automatically; the file remains in place so
concurrent openers always lock the same inode. Unix flock and Windows byte-range
locks are supported; other platforms fail explicitly. This prevents duplicate
execution; automatic job recovery is a separate lifecycle step.

### Goal guidance safe points

`GoalSpec.Guidance(ctx, iteration)` is an optional host callback sampled after
guardrails and before each maker iteration. Its text is prepended under
`Operator guidance` to that maker prompt and the corresponding checker prompt.
Updates arriving after sampling wait for the next maker; checker sampling is
never independent. Callback errors stop the goal before model execution and
are returned to the host. A nil callback or blank text preserves prior prompts.
The durable operator message queue and HTTP messages route build on this hook.

### Durable job messages

`POST /v1/jobs/{id}/messages` accepts `{ "text": "..." }` for a created, running,
or waiting job and returns 202 `{ "job_id": "...", "accepted": true }`. Bearer
authentication and the existing 1 MiB strict JSON limit apply. Blank text is
400 `invalid_request`; unknown jobs return 404 and terminal jobs 409. Use the
continuation operation for terminal resumable jobs once available.

The response follows durable `operator_message{ text }` insertion. At the next
maker boundary, `operator_message_delivered{ message_id, iteration }` references
the queued event ID and is appended before model execution. Its envelope has
phase `maker` and the iteration. Events are the durable queue: unacknowledged
messages survive restarts; acknowledged guidance stays in subsequent maker and
checker prompts without duplicate delivery events. Messages arriving during a
maker wait for its successor and do not change the current checker prompt.

### Continue terminal jobs

`POST /v1/jobs/{id}/continue` accepts `{ "text": "...", "max_iterations": 3 }`
and returns 202 `{ "job_id": "...", "accepted": true }`. Only `blocked`,
`max_iterations`, `budget_limited`, and `interrupted` jobs can continue; other
states return 409. Text is optional. Zero/omitted max_iterations reuses the job
limit, positive values replace it, and negative values return 400. Existing
authentication and strict JSON rules apply. A saved goal checklist and matching
workspace are required; missing checkpoint metadata returns 409.

The job ID, worktree/branch and base SHA remain unchanged. The next iteration
is the saved GoalRecord iteration plus one. A transaction updates the spec,
logs `job_resumed{ from_iteration }` with that next iteration, and queues any
guidance before execution detaches. Iteration numbers and event sequences
continue across runs. As with `glue goal --resume`, explicit continuation gets
fresh token/wall-clock budgets and the configured number of additional
iterations. Automatic recovery of a killed process is a separate operation.

### Durable execution checkpoints

Restartable hosts can require goal checkpoints with RequirePersistence and
save a GoalRecord before repository/provider preparation through Agent.SaveGoal.
Failed required saves stop model execution and surface an error. InitialUsage
and StartIteration preserve usage and numbering across repeated recovery.
Job startup recovery builds on this API; enabling it for jobs and recovering
workspace preparation are separate implementation steps.

### Job preparation checkpoints

Starting a job saves its initial GoalRecord, then atomically records job_started
and the planned absolute workspace/goal ID before git or provider construction.
An initial save failure leaves the job created and starts no preparation. Local
repository paths become absolute at creation so a changed server working
directory cannot redirect later preparation. Jobs require goal persistence.

After preparing a checkout, the manager commits its immutable base SHA before
constructing a provider. If the process dies between checkout and metadata, a
clean checkout on the expected job branch can supply the original SHA from HEAD.
A saved base allows existing edits and commits while retaining that original SHA.
Recovery verifies the independent checkout root and branch. Invalid, missing or
incomplete workspaces fail explicitly and preserve their contents for repair;
no reset, deletion, branch switch or global worktree prune is attempted.
Active jobs recover automatically when a replacement manager opens their state.

### Restart recovery

Startup resumes running/waiting jobs from GoalRecord at iteration boundaries,
reuses their workspaces, and atomically expires dead-process permissions with
`job_resumed{ from_iteration, reason: "restart" }`. Explicit continuation uses
reason `continue`. Abandoned maker/checker session files are discarded while
their durable events and filesystem edits remain. Created/terminal jobs stay idle.
Checkpointed usage, remaining iterations and elapsed wall time (including
downtime) count toward the original run limits; explicit continuation renews them.
A saved terminal goal finishes without another model call. Invalid recovery
state becomes an errored job with its diagnostic, allowing other jobs to recover.
