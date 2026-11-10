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
  # Authentication mode. Supported value in #419: "dev-token" ("github" OAuth in #424).
  mode: dev-token
  # Shared bearer / login token. If omitted in YAML, read from GLUE_GATEWAY_TOKEN.
  token: replace-with-gateway-token

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
```

## AX Backend (`gateway/backend/ax`)

For `type: ax` backends, `gateway/backend/ax` executes each Glue job as a one-shot AX `Task` (`glue-<job-id>`) backed by an AX `Workspace` (`glue-<job-id>-ws`):

1. **Ordered provisioning:** `Create` calls `UpdateWorkspace` (`dir: "."`, `depth: 1`, and `.glue/token`) and `CreateTask` (`glue serve --coding --jobs --job-from-ax --listen :8080 --state-dir /workspace/.glue/state --token-file /workspace/.glue/token`, with `spec.ID` in `Task.spec.config` and no task bearer token in `Task.spec.config`). `Start` applies `CreateActorEgressPolicy` (`tls_passthrough:443`) before calling `ResumeTask`, polls `GetTask` until `Running` + `WorkspaceReady=True`, and calls `POST /v1/jobs/{id}/start` through the router.
2. **Ambiguous submit reconciliation:** If `CreateTask` fails with a deadline or transport error, the job is marked `submit_unknown` and reconciled via `GetTask` before retrying so duplicate tasks are never created.
3. **mTLS HTTP `CONNECT` router transport:** `RouterTransport` dials `atenet-router.ate-system.svc.cluster.local:8444` with the gateway pod's `podidentity.podcert.ate.dev/identity` mTLS certificate and sends `CONNECT glue-<job-id>:8080 HTTP/1.1` with header `ate-target-actor: <atespace>/glue-<job-id>`. Status reads (`GET /v1/jobs`, `GET /v1/jobs/{id}`) read only the gateway SQLite registry and AX `GetTask` so inspecting a job never wakes a suspended task.

## SQLite Registry and Event Cache

`OpenRegistry(state_dir)` opens `<state_dir>/events.sqlite` (WAL mode) with two tables:

- `jobs(id, backend, backend_ref, owner, spec_json, status, created_at, updated_at, last_seq, result_json)`: tracks each gateway-allocated job ID, its owning backend and backend reference, job specification, derived lifecycle status, highest mirrored sequence number, and cached completion `jobs.Result`.
- `events(run_id, seq, payload_json, PRIMARY KEY(run_id, seq))`: stores an immutable copy of every `eventlog.Envelope` streamed from the backend.

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
