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
    # Backend implementation type. Supported value in #419: "local" ("ax" in #420).
    type: local
    # Base HTTP URL of the upstream glue serve instance (required).
    url: http://127.0.0.1:8787
    # Bearer token for authenticating upstream requests to glue serve (required).
    token: replace-with-local-daemon-token

# Default backend when a job submission omits backend (optional; defaults to backends[0].name).
default_backend: local
```

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
