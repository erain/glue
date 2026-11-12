# Glue on AX (`deploy/ax`)

Container image for running Glue durable coding jobs directly as [AX](https://github.com/google/ax) `Task` resources — with zero custom Kubernetes namespaces, ServiceAccounts, PVCs, or Deployments.

## Task Image (`deploy/ax/task-image/Dockerfile`)

- **Base image:** digest-pinned `ax-task-runner` (`ARG BASE_IMAGE` from `${STATE}/images.env`), keeping `ENTRYPOINT ["/usr/local/bin/ax-task-runner"]` untouched as PID 1.
- **Toolchain:** Go `1.27.3` from the official tarball with pinned SHA-256 verification (`65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40` on `linux/amd64`), `git`, `make`, `ca-certificates`, `bash`, and `/usr/local/bin/glue` built from this repo in a multi-stage build.
- **Optional module cache pre-warm:** pass `--build-arg PREWARM_GOMOD_REPO=<url>` (or set `PREWARM_GOMOD_REPO` / `PREWARM_GOMOD_REF` when running `scripts/ax-dev/build-task-image.sh`) to pre-populate `GOMODCACHE=/go/pkg/mod`.
- **Build & push:** `scripts/ax-dev/build-task-image.sh` (or `make ax-dev-task-image`) pushes `localhost:5001/glue-task:dev`, prints its `@sha256:...` digest, and records `GLUE_TASK_IMAGE` in `${STATE}/images.env`.

## Task Command (`--job-from-ax` and `--token-file`)

```sh
glue serve --coding --jobs --job-from-ax --listen :8080 --state-dir /workspace/.glue/state --token-file /workspace/.glue/token
```

- `--job-from-ax` reads `$AX_METADATA_URL/metadata/v1alpha1/ax/task` (or `AX_TASK_YAML`), leniently decodes it, validates the 31 KiB task YAML and 16 KiB `spec.config` limits, and creates the job in `created` status if none exists in `--state-dir`. Startup is idempotent across restarts, makes no outbound network calls, and starts no work during AX's golden pre-boot.
- `--token-file /workspace/.glue/token` reads the per-job credential seeded into `.glue/token` on `/workspace` (`0600`) and reuses it across suspend/resume. `internal/axbackend` seeds only `sha256:<hex>` of the per-job HMAC bearer token (`daemon.HashToken`), so `glue serve` verifies bearers without ever holding the token and the agent, which runs in the same sandbox as root, cannot use the file to call the daemon (for example to approve its own permission requests).
- **Agent isolation inside the task:** AX task containers cannot switch UID (`setresuid` returns `EPERM`, measured on `kind-glue-ax` with AX `56bd0fc`), so the agent's shell runs as the same user as `glue serve`. Nothing on disk under `/workspace/.glue` lets it act as a client: the token file holds only a hash and runtime credentials stay in memory. It can still read Glue's own state (sessions, events). The file tools stay confined to the job worktree.
- **Runtime credentials & redaction:** AX `Task` specs contain zero credential environment variables. Instead, `glue --remote` / `glue job` (and the optional host-mode `glue-gateway`, both backed by `internal/axbackend`) push `github_token` and `provider_keys` in memory (`POST /v1/credentials`) before `POST /v1/jobs/{id}/start`, after each `ResumeTask`, and whenever `GET /v1/jobs/{id}` reports `credentials: "absent"`. `glue serve` holds credentials in memory only and redacts known credential values and common token shapes (`ghp_…`, `github_pat_…`, `gho_…`, `AIza…`, `sk-…`) before persisting events, tool outputs, session files, or results under `/workspace/.glue`.

## End-to-End Verification on `kind-glue-ax`

```sh
source scripts/ax-dev/env.sh
scripts/ax-dev/e2e/submit.sh
scripts/ax-dev/e2e/suspend-resume.sh
```
