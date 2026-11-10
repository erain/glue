# `glue-gateway`

`gateway/` is a standalone Go module (`github.com/erain/glue/gateway`) that provides one front door for Glue coding jobs across backends. It owns the persistent SQLite job registry and event cache, hosts the embedded browser UI (`webui`), authenticates CLI and browser requests, and proxies `/v1/jobs...` requests and SSE event streams to configured backends.

Keeping `gateway/` in its own nested module isolates future Agent Substrate / AX dependencies (Go 1.27) from the root `github.com/erain/glue` module.

## Building and Running

```sh
cd gateway
go build -o /tmp/glue-gateway ./cmd/glue-gateway
/tmp/glue-gateway --config /path/to/gateway.yaml
```

`glue-gateway` listens on the configured TCP address and shuts down cleanly on `SIGINT` or `SIGTERM`.

## YAML Configuration Reference

Every configuration field is validated strictly (`KnownFields(true)` rejects unknown keys):

```yaml
# TCP address for the gateway HTTP server (optional; defaults to "127.0.0.1:8800").
listen: 127.0.0.1:8800

# External base URL used when constructing public links (optional; trailing "/" trimmed).
public_url: http://127.0.0.1:8800

# Directory storing the SQLite registry database (events.sqlite) (required).
state_dir: /var/lib/glue-gateway

# Client authentication configuration (required).
auth:
  # Authentication mode: "github" (OAuth App + allowlist) or "dev-token" (local dev only; refused on https:// public_url unless --insecure-dev is passed).
  mode: github
  # GitHub OAuth App credentials (required when mode: github; or set GLUE_GITHUB_CLIENT_ID / GLUE_GITHUB_CLIENT_SECRET).
  client_id: replace-with-github-oauth-client-id
  client_secret: replace-with-github-oauth-client-secret
  # Case-insensitive allowlist of GitHub usernames permitted to sign in (required when mode: github; or comma-separated GLUE_ALLOWED_LOGINS).
  allowed_logins:
    - your-github-login
  # Server-side browser session TTLs (optional; default to 168h absolute and 24h idle).
  session_absolute_ttl: 168h
  session_idle_ttl: 24h
  # Per-IP authentication rate limit per minute (optional; defaults to 30).
  rate_limit_per_min: 30
  # Shared bearer / login token for mode: dev-token (or read from GLUE_GATEWAY_TOKEN).
  # token: replace-with-gateway-token

# One or more execution backends (required; unique names).
backends:
  - # Logical backend identifier used in job specs and ?backend=<name> (required).
    name: local
    # Backend implementation type ("local" or "ax").
    type: local
    # Base HTTP URL of the upstream glue serve instance (required for type: local).
    url: http://127.0.0.1:8787
    # Bearer token for authenticating upstream requests to glue serve (required for type: local).
    token: replace-with-local-daemon-token

  - name: ax
    type: ax
    # AX atespace where Workspaces, Tasks, and EgressPolicies are created (defaults to "default").
    atespace: default
    # Digest-pinned AX task image reference (required for type: ax).
    image: localhost:5001/glue-task@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    # Per-task resource limits (defaults to cpu: "4", memory: "8Gi").
    resources:
      cpu: "4"
      memory: "8Gi"
    # Optional TLS passthrough port-443 egress allowlist (defaults to GitHub, Go module proxy, Gemini, and OpenRouter hosts).
    egress_hosts:
      - github.com
      - api.github.com
      - codeload.github.com
      - proxy.golang.org
      - sum.golang.org
      - storage.googleapis.com
      - generativelanguage.googleapis.com
      - openrouter.ai
    # Control-plane and router endpoints (optional; default to in-cluster service DNS names).
    ax_server: ax-server.ax-system.svc.cluster.local:8080
    substrate: api.ate-system.svc.cluster.local:443
    router: atenet-router.ate-system.svc.cluster.local:8444

# Default backend when a job submission omits backend (optional; defaults to backends[0].name).
default_backend: local

# Idle duration in waiting_for_input (with no live SSE viewer) before AX SuspendTask (optional; defaults to 2m).
suspend_after: 2m

# Maximum wait for AX ResumeTask + Running/WorkspaceReady on demand (optional; defaults to 1m).
resume_timeout: 1m

# Whether to call AX SuspendTask after caching Result when a job finishes (optional; defaults to true).
suspend_on_finish: true

# Duration to retain terminal AX Tasks/Workspaces/EgressPolicies before deleting them (optional; defaults to 168h / GLUE_RETAIN_TASKS).
retain_tasks: 168h
```

## AX Backend (`gateway/backend/ax`)

For `type: ax` backends, `gateway/backend/ax` executes each Glue job as a one-shot AX `Task` (`glue-<job-id>`) backed by an AX `Workspace` (`glue-<job-id>-ws`):

1. **Ordered provisioning:** `Create` calls `UpdateWorkspace` (`dir: "."`, `depth: 1`, and `.glue/token`) and `CreateTask` (`glue serve --coding --jobs --job-from-ax --listen :8080 --state-dir /workspace/.glue/state --token-file /workspace/.glue/token`, with `spec.ID` in `Task.spec.config` and no task bearer token in `Task.spec.config`). `Start` applies `CreateActorEgressPolicy` (`tls_passthrough:443`) before calling `ResumeTask`, polls `GetTask` until `Running` + `WorkspaceReady=True`, and calls `POST /v1/jobs/{id}/start` through the router.
2. **Ambiguous submit reconciliation:** If `CreateTask` fails with a deadline or transport error, the job is marked `submit_unknown` and reconciled via `GetTask` before retrying so duplicate tasks are never created.
3. **mTLS HTTP `CONNECT` router transport:** `RouterTransport` dials `atenet-router.ate-system.svc.cluster.local:8444` with the gateway pod's `podidentity.podcert.ate.dev/identity` mTLS certificate and sends `CONNECT glue-<job-id>:8080 HTTP/1.1` with header `ate-target-actor: <atespace>/glue-<job-id>`. Status reads (`GET /v1/jobs`, `GET /v1/jobs/{id}`, `GET /v1/jobs/{id}/permissions`, cached transcript) read only the gateway SQLite registry and AX `GetTask` so inspecting a job never wakes a suspended task.
4. **Idle & terminal suspend and on-demand resume:** When a job stays in `waiting_for_input` longer than `suspend_after` (default `2m`) with no live SSE viewer attached, the watcher calls `SuspendTask`, marks the job `suspended` (`status_reason: "waiting_for_input"`), and appends `task_suspended`. When a job reaches a terminal state (`job_done`) and `suspend_on_finish` is enabled (default `true`), the watcher caches `Result` and calls `SuspendTask` (`status_reason: "finished"`). Any state-changing action (`approve`/`deny`, `messages`, `continue`, `cancel`) or live SSE stream attachment calls `ResumeTask`, waits for `Running` + `WorkspaceReady` within `resume_timeout`, appends `task_resumed`, reconnects the upstream stream using the last upstream sequence number, and (for permission decisions) matches the user's decision to the re-issued `permission_request` with the same tool and target while recording both permission IDs in `permission_decision`.
5. **Result persistence, task retention, purge, and orphan reconciliation:** On terminal status, the gateway fetches `GET /v1/jobs/{id}/result` once, persists it in SQLite (`result_json`), and serves `GET /v1/jobs/{id}/result` from SQLite thereafter. After `retain_tasks` (default `168h`, or `GLUE_RETAIN_TASKS`) in a terminal state, `deleteJobTask` ensures the completion result is stored in SQLite before calling `DeleteTask`, `DeleteWorkspace`, and `DeleteActorEgressPolicy`, then marks the registry record `task_deleted: true` while keeping the job row, events, and result intact. `DELETE /v1/jobs/{id}?purge=true` (`glue job delete <id>`) deletes the backend task (if any) and purges the job row and event history from SQLite. On startup (and via `GET /v1/diagnostics`), the gateway reconciles AX tasks in the configured atespace (`ListTasks`) and reports any `glue-*` tasks without a registry row in logs and `{"orphan_tasks": [...]}` without deleting them automatically.

## SQLite Registry and Event Cache

`OpenRegistry(state_dir)` opens `<state_dir>/events.sqlite` (WAL mode) with four tables:

- `jobs(id, backend, backend_ref, owner, spec_json, status, created_at, updated_at, last_seq, result_json)`: tracks each gateway-allocated job ID, its owning backend and backend reference, owning user login (`owner`), job specification, derived lifecycle status, highest mirrored sequence number, and cached completion `jobs.Result`. Users only see and act on jobs where `owner` matches their authenticated login.
- `events(run_id, seq, payload_json, PRIMARY KEY(run_id, seq))`: stores an immutable copy of every `eventlog.Envelope` streamed from the backend.
- `sessions(id, login, github_id, csrf_token, created_at, last_seen_at, expires_at)`: stores server-side browser sessions with random 256-bit IDs, `HttpOnly; Secure; SameSite=Lax` cookies, 7-day absolute TTL, and 24-hour idle TTL. State-changing cookie requests require the `X-Glue-CSRF` double-submit header.
- `api_tokens(id, name, owner, token_hash, prefix, created_at, last_used_at)`: stores personal API tokens as SHA-256 hashes (`token_hash`) for CLI `Authorization: Bearer <token>` authentication.

## Creating a GitHub OAuth App and Personal API Tokens

1. In GitHub **Settings -> Developer settings -> OAuth Apps -> New OAuth App**, set **Homepage URL** to `<public_url>` and **Authorization callback URL** to `<public_url>/auth/github/callback`.
2. Configure `auth.mode: github`, `client_id`, `client_secret` (or `GLUE_GITHUB_CLIENT_ID` / `GLUE_GITHUB_CLIENT_SECRET`), and `allowed_logins` in `gateway.yaml`.
3. Open `<public_url>` in a browser, click **Sign in with GitHub**, navigate to **Tokens** (`#/tokens`), and create a personal API token (shown once).
4. Store the token for the CLI with `glue job login --server <public_url> --token <glue_pat_...>`.

## Event Stream Proxying and Offline Replay

1. **End-to-end job IDs:** On `POST /v1/jobs`, the gateway allocates the job ID (`job-<12 hex chars>`) and forwards it in `Spec.ID` to the backend so the gateway ID and backend ID match.
2. **Single upstream watcher per active job:** `ensureWatcher` maintains one upstream `GET /v1/jobs/{id}/events` stream per non-terminal job, appending events idempotently into SQLite and broadcasting to downstream SSE subscribers.
3. **Seamless upstream reconnect:** If the upstream connection drops before a terminal event, the watcher reconnects with `Last-Event-ID: <last_seq>`. Downstream clients read from the gateway's SQLite log and never observe gaps or duplicate sequence numbers.
4. **Offline inspection and replay:** When `job_done` arrives, the watcher caches `result_json` in SQLite. `GET /v1/jobs/{id}`, `GET /v1/jobs/{id}/events`, `GET /v1/jobs/{id}/permissions`, and `GET /v1/jobs/{id}/result` continue to work from SQLite even when the backend is stopped.

## Using with `glue job` and the Browser UI

```sh
export GLUE_SERVER=http://127.0.0.1:8800
export GLUE_TOKEN=replace-with-gateway-token

glue job submit --repo /path/to/repo --objective "Add unit tests"
glue job attach <job-id>
glue job approve <job-id> <perm-id>
glue job result <job-id>
```

Open `http://127.0.0.1:8800` in a browser and sign in with the gateway token to view, steer, and approve jobs through the embedded `webui`.
