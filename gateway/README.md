# `glue-gateway`

`gateway/` is an optional host/VM HTTP portal (`github.com/erain/glue/gateway`) for Glue coding jobs. Laptop and CLI users can run jobs directly on AX via `glue --remote "<objective>"` and `glue job ...` without deploying a gateway; run `glue-gateway` on a host or VM when you want the mobile/desktop browser UI (`webui`), GitHub OAuth allowlist sign-in, personal API tokens, idle AX task suspension while waiting on human input, or public HTTPS access behind a [Cloudflare Tunnel](../deploy/cloudflared/README.md).

`glue-gateway` runs entirely outside Kubernetes with zero `glue-system` cluster manifests, delegating `type: ax` backends to `internal/axbackend` with pure AX configuration.

## Building and Running

```sh
cd gateway
go build -o /tmp/glue-gateway ./cmd/glue-gateway
/tmp/glue-gateway --config /path/to/gateway.yaml
```

`glue-gateway` listens on the configured TCP address and shuts down cleanly on `SIGINT` or `SIGTERM`.

## YAML Configuration Reference

Every configuration field is validated strictly (`KnownFields(true)` rejects unknown or legacy keys):

```yaml
# TCP address for the gateway HTTP server (optional; defaults to "127.0.0.1:8800").
listen: 127.0.0.1:8800

# External base URL used when constructing public links (optional; trailing "/" trimmed).
public_url: http://127.0.0.1:8800

# Trust X-Forwarded-Proto: https from Cloudflare Tunnel / reverse proxy to set HSTS and Secure cookies (optional; or GLUE_TRUST_FORWARDED_PROTO=1).
trust_forwarded_proto: false

# Maximum HTTP request body size in bytes (optional; defaults to 1048576 / 1 MiB).
max_body_bytes: 1048576

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
    name: ax
    # Backend implementation type ("ax" or "local").
    type: ax
    # AX atespace where Workspaces and Tasks are created (defaults to "default").
    atespace: default
    # Digest-pinned AX task image reference (defaults to GLUE_AX_IMAGE / GLUE_TASK_IMAGE / ~/.cache/glue/ax-dev/images.env, then the published ghcr.io/erain/glue-task image).
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
    # Optional AX server gRPC endpoint (defaults to "" so internal/axbackend auto-tunnels via kubectl).
    # ax_server: 127.0.0.1:8080
    # Optional kubeconfig path and context for AX target discovery (fall back to GLUE_AX_KUBECONFIG / GLUE_AX_CONTEXT / KUBECONFIG / ~/.kube/config).
    # kube_config: /etc/glue-gateway/kubeconfig
    # kube_context: kind-glue-ax

  - name: local
    type: local
    # Base HTTP URL of an upstream glue serve instance (required for type: local).
    url: http://127.0.0.1:8787
    # Bearer token for authenticating upstream requests to glue serve (required for type: local).
    token: replace-with-local-daemon-token

# Default backend when a job submission omits backend (optional; defaults to backends[0].name).
default_backend: ax

# Idle duration in waiting_for_input (with no live SSE viewer) before AX SuspendTask (optional; defaults to 2m).
suspend_after: 2m

# Maximum wait for AX ResumeTask + Running/WorkspaceReady on demand (optional; defaults to 1m).
resume_timeout: 1m

# Whether to call AX SuspendTask after caching Result when a job finishes (optional; defaults to true).
suspend_on_finish: true

# Duration to retain terminal AX Tasks/Workspaces before deleting them (optional; defaults to 168h / GLUE_RETAIN_TASKS).
retain_tasks: 168h
```

Runtime credentials pushed in memory to AX tasks (`github_token` and `provider_keys`) are read from `credentials` in `gateway.yaml` or picked up automatically from `GLUE_GITHUB_TOKEN` / `GITHUB_TOKEN` / `gh auth token` and `GEMINI_API_KEY`, `OPENROUTER_API_KEY`, `DEEPSEEK_API_KEY`, `NVIDIA_API_KEY`, and `OPENAI_API_KEY`.

## AX Backend (`internal/axbackend`)

For `type: ax` backends, `glue-gateway` delegates to `internal/axbackend.Backend` — the exact same AX lifecycle and transport engine used by `glue --remote`:

1. **Client auto-tunneling & shared task HMAC:** `axbackend.Connect` discovers the target Kubernetes context (`kube_config` / `kube_context` or ambient environment), manages local port-forwards and short-lived router TLS / control credentials automatically in `TunnelDir()`, and shares the cluster's `ax-system/glue-ax-task-hmac` Secret so jobs submitted via `glue-gateway` and direct `glue job --ax` interoperate seamlessly.
2. **Ordered provisioning & reconciliation:** `Create` updates the AX `Workspace` (`glue-<job-id>-ws`, `dir: "."`, `depth: 1`, `.glue/token`) and creates the AX `Task` (`glue-<job-id>`). Ambiguous transport failures record `submit_unknown` and reconcile via `GetTask` before retrying so duplicate tasks are never created. `Start` configures task egress before calling `ResumeTask`, waits for `Running` + `WorkspaceReady=True`, pushes credentials in memory (`POST /v1/credentials`), and starts the job.
3. **Zero-wakeup read-only views:** Status reads (`GET /v1/jobs`, `GET /v1/jobs/{id}`, `GET /v1/jobs/{id}/permissions`, cached `/events`, and cached `/result`) read only the gateway SQLite registry and AX `GetTask`, never waking a `Suspended` task.
4. **Idle & terminal suspend and on-demand resume:** When a job stays in `waiting_for_input` longer than `suspend_after` (default `2m`) with no live SSE viewer attached, the watcher calls `SuspendTask`, marks the job `suspended` (`status_reason: "waiting_for_input"`), and appends `task_suspended`. When a job finishes (`job_done`) and `suspend_on_finish` is enabled (default `true`), the watcher caches `Result` in SQLite and calls `SuspendTask` (`status_reason: "finished"`). State-changing actions (`approve`/`deny`, `messages` (with `?interrupt=true` forwarded), `answers`, `continue`, `cancel`) or live SSE stream attachments resume the task within `resume_timeout`, append `task_resumed`, reconnect using the last upstream sequence number, and remap re-issued permission IDs (`original_permission_id` + `permission_id`).
5. **Result persistence, task retention, purge, and orphan reconciliation:** On terminal status, the gateway persists `result_json` in SQLite before any task deletion. After `retain_tasks` (default `168h`), `deleteJobTask` deletes the AX task resources and marks `task_deleted: true` while keeping the SQLite record, events, and result. `DELETE /v1/jobs/{id}?purge=true` (`glue job delete --server ... <id>`) deletes the backend task and purges all SQLite rows for that job. Startup and `GET /v1/diagnostics` report any orphan `glue-*` tasks without deleting them.

## SQLite Registry and Event Cache

`OpenRegistry(state_dir)` opens `<state_dir>/events.sqlite` (WAL mode) with four tables:

- `jobs(id, backend, backend_ref, owner, spec_json, status, created_at, updated_at, last_seq, result_json)`: tracks each gateway-allocated job ID, owning backend, owning user login (`owner`), job specification, derived lifecycle status, highest mirrored sequence number, and cached completion `jobs.Result`.
- `events(run_id, seq, payload_json, PRIMARY KEY(run_id, seq))`: stores an immutable copy of every `eventlog.Envelope` streamed from the backend.
- `sessions(id, login, github_id, csrf_token, created_at, last_seen_at, expires_at)`: stores server-side browser sessions with random 256-bit IDs, `HttpOnly; Secure; SameSite=Lax` cookies, 7-day absolute TTL, 24-hour idle TTL, and `X-Glue-CSRF` double-submit enforcement.
- `api_tokens(id, name, owner, token_hash, prefix, created_at, last_used_at)`: stores SHA-256-hashed personal API tokens for CLI `Authorization: Bearer <token>` authentication.

## Creating a GitHub OAuth App and Personal API Tokens

1. In GitHub **Settings -> Developer settings -> OAuth Apps -> New OAuth App**, set **Homepage URL** to `<public_url>` and **Authorization callback URL** to `<public_url>/auth/github/callback`.
2. Configure `auth.mode: github`, `client_id`, `client_secret` (or `GLUE_GITHUB_CLIENT_ID` / `GLUE_GITHUB_CLIENT_SECRET`), and `allowed_logins` in `gateway.yaml`.
3. Open `<public_url>` in a browser, click **Sign in with GitHub**, navigate to **Tokens** (`#/tokens`), and create a personal API token (shown once).
4. Store the token for the CLI with `glue job login --server <public_url> --token <glue_pat_...>`.

## Public Exposure and Hardening (`deploy/cloudflared`)

When exposed publicly on a host or VM behind a named Cloudflare Tunnel (see [`deploy/cloudflared`](../deploy/cloudflared/README.md) and [`docs/runbooks/public-gateway.md`](../docs/runbooks/public-gateway.md)):

- **HSTS & trusted `X-Forwarded-Proto`:** Responses include `Strict-Transport-Security: max-age=31536000; includeSubDomains` and session cookies carry `Secure` whenever `public_url` starts with `https://`, TLS is terminated directly, or `trust_forwarded_proto: true` with `X-Forwarded-Proto: https`.
- **Request limits & HTTP timeouts:** Request bodies are capped at `max_body_bytes` (default `1 MiB`, rejecting larger payloads with `413 Request Entity Too Large`), and `NewHTTPServer` configures `ReadHeaderTimeout: 10s`, `ReadTimeout: 30s`, and `IdleTimeout: 120s`.
- **Redacted structured access logs & `/healthz`:** Every request logs `method`, `path` (without query parameters), `status`, `duration_ms`, and `remote_ip`, never logging bearer tokens, cookies, or OAuth secrets. `GET /healthz` returns `{"ok":true}` with no version or internal details.

## Using with `glue job` and the Browser UI

```sh
export GLUE_SERVER=http://127.0.0.1:8800
export GLUE_TOKEN=replace-with-gateway-token

glue job submit --server "$GLUE_SERVER" --repo https://github.com/org/repo --objective "Add unit tests"
glue job attach --server "$GLUE_SERVER" <job-id>
glue job approve --server "$GLUE_SERVER" <job-id> <perm-id>
glue job result --server "$GLUE_SERVER" <job-id>
```

Open `http://127.0.0.1:8800` in a browser to view, steer, and approve jobs through the embedded `webui`.
