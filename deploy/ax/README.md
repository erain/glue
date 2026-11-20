# Glue on AX (`deploy/ax`)

Container image for running Glue durable coding jobs directly as [AX](https://github.com/google/ax) `Task` resources — with zero custom Kubernetes namespaces, ServiceAccounts, PVCs, or Deployments.

## Task Image (`deploy/ax/task-image/Dockerfile`)

- **Base image:** digest-pinned `ax-task-runner` (`ARG BASE_IMAGE` from `${STATE}/images.env`), keeping `ENTRYPOINT ["/usr/local/bin/ax-task-runner"]` untouched as PID 1.
- **Toolchain:** Go `1.27.3` and Node.js `26.13.0` LTS (with `npm`) from their official tarballs with pinned SHA-256 verification, the base image's Python `3.12` with `pip` and `venv` plus `uv 0.13.6`, `gcc`/`g++` with libc headers, `git`, `make`, `ripgrep`, `jq`, `curl`, `ca-certificates`, `bash`, and `/usr/local/bin/glue` built from this repo in a multi-stage build. The image is about 1.4 GiB (up from about 0.7 GiB with Go only); AX workers cache it after the first pull. `scripts/ax-dev/e2e/image-smoke.sh [image]` prints each tool's version and the image size.
- **Default egress:** besides GitHub, the Go module proxy and the model providers, tasks may reach `registry.npmjs.org`, `pypi.org`, and `files.pythonhosted.org` (TLS passthrough on port 443).
- **Optional module cache pre-warm:** pass `--build-arg PREWARM_GOMOD_REPO=<url>` (or set `PREWARM_GOMOD_REPO` / `PREWARM_GOMOD_REF` when running `scripts/ax-dev/build-task-image.sh`) to pre-populate `GOMODCACHE=/go/pkg/mod`.
- **Published image:** `.github/workflows/task-image.yml` rebuilds `ax-task-runner` from the AX revision pinned in `scripts/ax-dev/versions.env` (published as `ghcr.io/erain/glue-ax-task-runner:ax-<commit>`), builds this image on top for `linux/amd64` and `linux/arm64`, and publishes `ghcr.io/erain/glue-task:main` on every push to `main`, `:vX.Y.Z` for release tags, and `:sha-<commit>` for both. Pull requests that touch the image only build it for `linux/amd64` and run the smoke script. When no image is configured, `glue` uses the published image matching its own version (`:vX.Y.Z` for a release build, else `:main`); `glue job doctor` shows which image and why.
- **Build & push:** `scripts/ax-dev/build-task-image.sh` (or `make ax-dev-task-image`) pushes `localhost:5001/glue-task:dev`, prints its `@sha256:...` digest, and records `GLUE_TASK_IMAGE` in `${STATE}/images.env`.

## Task Command (`--job-from-ax` and `--token`)

```sh
glue serve --coding --jobs --job-from-ax --listen :8080 --state-dir /workspace/.glue/state --token sha256:<hex>
```

- `--job-from-ax` reads `$AX_METADATA_URL/metadata/v1alpha1/ax/task` (or `AX_TASK_YAML`), leniently decodes `spec.config` (and falls back to `spec.goal.text`), validates the 31 KiB task YAML and 16 KiB `spec.config` limits, and creates the job in `created` status if none exists in `--state-dir`. Startup is idempotent across restarts, makes no outbound network calls, and writes terminal summaries to `$AX_OUTPUT_FILE` when set.
- `--token sha256:<hex>` passes only the one-way SHA-256 verifier of the per-job HMAC bearer token (`daemon.HashToken`), avoiding per-job `Workspace` pre-warm overhead while ensuring `glue serve` verifies bearers without ever holding the raw token.
- **Unprivileged runner execution & egress:** AX `v0.4.0-rc.1` mounts `ax-runner-volume` and executes task commands as unprivileged `UID 1000:1000` (`HOME=/workspace`, `/go` writable in `deploy/ax/task-image/Dockerfile`), with outbound TLS passthrough governed by a native `v1alpha1.Gateway` (`glue-<jobID>-gw`) bound at `CreateTask` time.
- **Runtime credentials & redaction:** AX `Task` specs contain zero credential environment variables. Instead, `glue --remote` / `glue job` (and the optional host-mode `glue-gateway`, both backed by `internal/axbackend`) push `github_token` and `provider_keys` in memory (`POST /v1/credentials`) before `POST /v1/jobs/{id}/start`, after each `ResumeTask`, and whenever `GET /v1/jobs/{id}` reports `credentials: "absent"`. `glue serve` holds credentials in memory only and redacts known credential values and common token shapes (`ghp_…`, `github_pat_…`, `gho_…`, `AIza…`, `sk-…`) before persisting events, tool outputs, session files, or results under `/workspace/.glue`.

## End-to-End Verification on `kind-glue-ax`

```sh
source scripts/ax-dev/env.sh
scripts/ax-dev/e2e/submit.sh
scripts/ax-dev/e2e/suspend-resume.sh
```
