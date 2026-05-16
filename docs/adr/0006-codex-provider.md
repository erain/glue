# ADR 0006: Codex Provider — ChatGPT-Subscription Auth + Responses Transport

## Status

Accepted. First implementation issues that follow this ADR:

- `providers/codex`: token reader + refresh (M1 of tracker [#110](https://github.com/erain/glue/issues/110)).
- `providers/codex`: streaming Responses transport (M1 of tracker [#110](https://github.com/erain/glue/issues/110)).

A separate "subscription login" subcommand that performs the full
interactive PKCE flow inside glue itself is deferred behind these two —
v0.1 piggybacks on the upstream Codex CLI's `auth.json` (the user runs
`codex login` once outside glue; glue reads the same file).

## Context

Peggy's daily-driver budget (tracker [#110](https://github.com/erain/glue/issues/110))
assumes the existing ChatGPT/Codex subscription pays for the model, not
a per-token OpenAI API key. The reference implementation for this flow
is OpenAI's open-source Codex CLI ([openai/codex](https://github.com/openai/codex),
Rust, Apache-2.0). Several other open-source agents have ported the
same pattern (notably the `opencode` plugin
[numman-ali/opencode-openai-codex-auth](https://github.com/numman-ali/opencode-openai-codex-auth),
TypeScript) so the surface is well-trodden.

ADR-0005 placed this work in scope ("HTTP server, subscription-auth
providers in scope behind framework interfaces"). This ADR pins down
the actual auth flow, token storage, transport endpoint, headers, and
streaming format so the implementation PRs can be small and verifiable.

We borrow the protocol, not the code. The Codex CLI source is read as
spec, not copy-pasted.

## Decision

### 1. Package: `providers/codex` (new top-level provider)

A new `providers/codex` package. It does **not** sit on top of
`providers/openaicompat`: the wire format (Responses API, not Chat
Completions), the auth shape (OAuth JWT bearer + `ChatGPT-Account-ID`,
not a static API key), and the endpoint host (chatgpt.com, not
api.openai.com) all differ enough that sharing code would be
counter-productive.

`providers/openaicompat` stays the basis for `providers/nvidia` and
`providers/openrouter`. A future API-key-based OpenAI provider (when
budget allows) can also reuse it; that is additive and out of scope
here.

Package boundary:

- `providers/codex/auth/` — token storage, refresh, JWT exp parsing.
  No HTTP to the model endpoint. No knowledge of glue types.
- `providers/codex` — the `glue.Provider` implementation. Imports
  `providers/codex/auth` for tokens.

The fragile parts (OAuth flow shape, header names, base URL) are
isolated to this package. Nothing else in `glue` learns about ChatGPT
auth.

### 2. Auth: read upstream Codex CLI's `auth.json`

**v0.1 does not implement an interactive login.** The user runs
`codex login` once with the upstream Codex CLI; glue reads the same
`auth.json` afterward.

This minimizes scope (no embedded PKCE loopback server, no UI for the
authorize-URL handoff) and maximizes UX (one login for both tools).
The deferred follow-up — `providers/codex login` as a glue subcommand
— can land later by porting the PKCE flow described in the appendix
of this ADR.

**File location** (in priority order):

1. `$GLUE_CODEX_AUTH` (env override, absolute path; for tests).
2. `$CODEX_HOME/auth.json` if `CODEX_HOME` is set.
3. `~/.codex/auth.json` (default upstream location).

**File schema** (Codex CLI `login/src/auth/storage.rs`):

```json
{
  "OPENAI_API_KEY": "sk-..."|null,
  "tokens": {
    "id_token": "<JWT>",
    "access_token": "<JWT>",
    "refresh_token": "<opaque>",
    "account_id": "<extracted from id_token claim>"
  },
  "last_refresh": "<RFC3339>"
}
```

We use only `tokens.access_token`, `tokens.refresh_token`, and
`tokens.account_id`. The optional `OPENAI_API_KEY` (a swapped key from
an OAuth token-exchange step) is **ignored**; glue's subscription
provider always uses the OAuth `access_token` against
chatgpt.com.

**Refresh policy**:

- Proactive: refresh when access-token JWT `exp` is in the past, or
  when `last_refresh` is older than 8 days. Match the Codex CLI's
  cadence so glue doesn't desync from the upstream tool when both are
  in use.
- Reactive: on HTTP 401 from the model endpoint, refresh once and
  retry. If retry also returns 401, return the error to the caller.

**Refresh endpoint and shape** (note: refresh body is JSON, *not*
form-urlencoded — Codex CLI is asymmetric here):

```
POST https://auth.openai.com/oauth/token
Content-Type: application/json

{
  "client_id": "app_EMoamEEZ73f0CkXaXp7hrann",
  "grant_type": "refresh_token",
  "refresh_token": "<opaque>"
}
```

Response: `{ id_token?, access_token?, refresh_token? }`. Any subset;
missing fields keep prior values. On success, atomically rewrite
`auth.json` with the merged tokens and updated `last_refresh`. On
classified-permanent failures (`refresh_token_expired`,
`refresh_token_reused`, `refresh_token_invalidated`) return an error
telling the user to re-run `codex login`.

**File-write discipline**: 0o600 permissions, temp-file + atomic
rename. Never log the file contents; redact tokens in errors.

**Override URL**: respect `CODEX_REFRESH_TOKEN_URL_OVERRIDE` for
testing parity with the upstream CLI.

### 3. Transport: Responses API over HTTP+SSE

**Base URL**: `https://chatgpt.com/backend-api/codex`

**Endpoint**: `POST /responses`

**Required headers** (subscription-auth variant):

| Header | Value | Source |
|---|---|---|
| `Authorization` | `Bearer <access_token>` (the JWT in `tokens.access_token`) | auth |
| `ChatGPT-Account-ID` | `<account_id>` from the id_token claim | auth |
| `OpenAI-Beta` | `responses=experimental` | constant |
| `Accept` | `text/event-stream` | streaming |
| `Content-Type` | `application/json` | request |
| `originator` | `codex_cli_rs` | constant (server-side allowlist) |
| `User-Agent` | `glue-codex/<glueVersion> (<GOOS> <GOARCH>) codex-compat` | constant |
| `version` | `<glueVersion>` | constant |
| `session_id` | per-turn UUIDv4 | request |
| `conversation_id` | per-session UUIDv4 | request |

**Cookie jar**: the chatgpt.com host is fronted by Cloudflare; the
upstream CLI persists cookies for the host across calls
(`default_client.rs::with_chatgpt_cloudflare_cookie_store`). Glue does
the same via `net/http/cookiejar` scoped to `chatgpt.com`.
Per-`Provider` instance, not global.

**Why `originator: codex_cli_rs` and not something glue-specific**:
the server-side `is_first_party_originator` allowlist (Codex CLI
`default_client.rs:122-131`) is what determines whether a request is
treated as anomalous. Sending a non-allowlisted originator risks
silent 403s. We accept the borrowed identity as the cost of
subscription auth; a future change should re-evaluate if OpenAI
publishes an official third-party allowlist.

**Request body** (`Responses` API shape, matches Codex CLI
`core/src/client.rs::ResponsesApiRequest`):

```json
{
  "model": "<resolved model>",
  "instructions": "<system prompt>",
  "input": [...messages...],
  "tools": [...responses-tool-schema...],
  "tool_choice": "auto",
  "parallel_tool_calls": false,
  "stream": true,
  "store": false,
  "include": ["reasoning.encrypted_content"]
}
```

Glue's normalized message types map to the Responses `input` array
(role + content parts). `tools` uses the Responses tool schema (top-
level `type`, `name`, `description`, `parameters`) — *not* the Chat
Completions `{type:"function", function:{...}}` shape.

**SSE event handling**: events arrive as `event: <name>\ndata:
<json>\n\n` blocks. Glue maps them to `ProviderEvent` types:

| SSE event | Glue event |
|---|---|
| `response.created` | `ProviderEventStart` |
| `response.output_text.delta` | `ProviderEventTextDelta` (cumulative `output_text`) |
| `response.output_item.done` with `type=function_call` | `ProviderEventToolCall` |
| `response.completed` | `ProviderEventFinish` (with usage from the event payload) |
| `response.failed` | provider-level error returned from the stream |
| any other type | logged at debug, ignored |

The stream **must** terminate with `response.completed` (or
`response.failed`). If it closes without one, the provider returns an
error matching the Codex CLI's "stream closed before
response.completed" behavior.

### 4. Cancellation

The stream reader respects `context.Context` cancellation: the request
is opened with the call's context, and the SSE reader checks `ctx.Done()`
between lines. Cancel propagates to a closed connection, not a
half-read stream.

### 5. Tool calls and tool results

- **Outbound**: glue tool definitions are converted to Responses
  `tools` items (`{type, name, description, parameters}`). The default
  `tool_choice` is `"auto"`.
- **Inbound tool calls**: a `response.output_item.done` with
  `type=function_call` carries `name`, `arguments` (JSON-string), and
  `call_id`. We map this to a normalized `ToolCall` with the
  `call_id` preserved as the `Tool Call ID` so subsequent
  `function_call_output` items round-trip correctly.
- **Tool results submitted to the next turn**: glue's `Message`
  entries of role `tool` map to `function_call_output` items in the
  next request's `input` array, keyed by `call_id`.

Image / audio / file input is **out of scope** for this ADR; the
package returns an error if a request includes non-text content. A
follow-up ADR can lift this when the multimodal need lands.

### 6. Failure modes

- **No token file**: provider returns a clear error: "Run `codex
  login` to authenticate (no token file at <path>)." with the resolved
  path.
- **Stale tokens, refresh succeeds**: transparent retry.
- **Stale tokens, refresh fails with `refresh_token_expired`**:
  permanent error with re-login instruction; do not retry.
- **Stale tokens, transient refresh failure**: retry up to twice with
  exponential backoff (1s, 4s); then surface error.
- **401 from `/responses`**: refresh once, retry once; on second 401
  surface the error.
- **429 from `/responses`**: pass through as a provider error; do not
  retry inside the provider. The caller (loop / failover) decides.
- **5xx from `/responses`**: pass through; no retry inside the
  provider.
- **Stream closes without `response.completed`**: error.
- **Unknown event type**: ignored; logged at debug only.

### 7. Testing

- **Offline conversion tests** (no network):
  - glue messages → Responses `input` array.
  - glue tool definitions → Responses `tools` schema.
  - SSE event stream (canned bytes) → glue `ProviderEvent` sequence.
  - request header building (asserts every header in §3).
  - tool-call round-trip (`call_id` preservation).
  - auth.json read and merge across all three file-location overrides.
  - refresh request body shape (JSON, not form).
  - refresh response merging (partial fields preserved).
- **Live smoke** (gated on `GLUE_CODEX_LIVE=1` *and* a readable token
  file): sends a tiny prompt, asserts a non-empty `output_text` and a
  clean `response.completed`. **Never runs in default CI.** Manual
  `workflow_dispatch` only, matching the pattern for live Gemini /
  NVIDIA / OpenRouter jobs.

### 8. Observability

Provider-level metadata exposed on `Message`:

- `provider` = `codex`
- `model` = the model id from the response
- `response_id` = `response.id` from `response.created`
- `usage` = `{input_tokens, output_tokens, cached_input_tokens?}` from
  the `response.completed` payload

Tokens themselves are never written to logs or metadata. The token
file path is fine to log; its contents are not.

## Consequences

- A new provider package and one new optional dependency: `golang.org/x/net/http2` (only if we end up needing explicit H2 control — usually the stdlib auto-upgrades). PKCE / OAuth-flow code lives behind a deferred follow-up; for v0.1, no new auth deps beyond `net/http` and `encoding/json`.
- The fragile bits — endpoint host, header names, refresh body shape, originator value — are quarantined to `providers/codex`. The rest of glue does not know about ChatGPT auth.
- Glue piggybacks on the upstream `codex login` for v0.1. If OpenAI changes the auth flow, the user's primary fix is "update the upstream Codex CLI and re-login"; glue inherits the fix on next token read.
- This is the load-bearing model backend for Peggy v0.1. Failover to Gemini / OpenRouter (already shipped) remains the contingency when subscription auth breaks.
- Subscription-auth use via third-party tools is not formally documented by OpenAI. The provider is built for personal use; multi-user / production deployments should use API-key auth instead (additive follow-up).

## Appendix: PKCE login flow (deferred)

When the v0.1 implementation is in use, a follow-up issue can add
`providers/codex login` as a glue subcommand. The protocol is
documented here so the eventual implementation has a complete spec.

- **Loopback PKCE Authorization Code flow.**
- Generate verifier (64 random bytes, base64url-no-pad) and challenge
  (SHA-256 of verifier, base64url-no-pad).
- Bind a local HTTP server on `127.0.0.1:1455` (fallback `1457`).
  These ports are baked into OpenAI's Hydra allowlist; they cannot be
  changed.
- Open the browser to
  `https://auth.openai.com/oauth/authorize?response_type=code&client_id=app_EMoamEEZ73f0CkXaXp7hrann&redirect_uri=http://localhost:1455/auth/callback&scope=openid%20profile%20email%20offline_access&code_challenge=<challenge>&code_challenge_method=S256&state=<state>&id_token_add_organizations=true&codex_cli_simplified_flow=true&originator=codex_cli_rs`.
- On callback, exchange the code (form-urlencoded body, *not* JSON):

  ```
  POST https://auth.openai.com/oauth/token
  Content-Type: application/x-www-form-urlencoded

  grant_type=authorization_code&code=<code>&redirect_uri=http://localhost:1455/auth/callback&client_id=app_EMoamEEZ73f0CkXaXp7hrann&code_verifier=<verifier>
  ```

- Response contains `id_token`, `access_token`, `refresh_token`.
- Extract `account_id` from the id_token JWT claim
  `https://api.openai.com/auth.chatgpt_account_id` (or alias
  `chatgpt_account_id`).
- Write `auth.json` to the configured glue-managed path (or to
  `$CODEX_HOME/auth.json` if the user opts to share with upstream
  Codex CLI).

This appendix is non-normative for v0.1. The implementation can
arrive in a separate ADR if the deferred follow-up issue lands.
