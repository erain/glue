# Glue on AX (`deploy/ax`)

Container image and configuration for running Glue durable coding jobs as [AX](https://github.com/google/ax) `Task` resources on [Agent Substrate](https://github.com/agent-substrate/substrate).

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
- `--token-file /workspace/.glue/token` generates a 256-bit bearer token once, stores it on `/workspace` (`0600`), and reuses it across suspend/resume.

## Gateway Image and Manifests (`deploy/ax/gateway`)

- **`deploy/ax/gateway/Dockerfile`:** Multi-stage build compiling `gateway/cmd/glue-gateway` with Go `1.27.3` into a minimal `debian:bookworm-slim` runtime image (`ca-certificates` + `/usr/local/bin/glue-gateway`).
- **`deploy/ax/gateway/manifests.yaml`:** Deploys `glue-gateway` in namespace `glue-system` (labeled `ate.dev/api-access: "true"` so Substrate's `ate-api-server` NetworkPolicy permits gRPC ingress) under ServiceAccount `glue-gateway` (allowlisted in `scripts/ax-dev/up.sh` for Substrate control API and `atenet-router` ingress), with:
  - Projected `ate-token` (`/var/run/secrets/ateapi/token`, `audience: api.ate-system.svc`), `servicedns-ca` (`/run/servicedns-ca/trust-bundle.pem`), and `podidentity` (`/run/podidentity.podcert.ate.dev/credential-bundle.pem`) volumes.
  - PersistentVolumeClaim `glue-gateway-state` mounted at `/var/lib/glue-gateway` for `events.sqlite`.
  - ConfigMap `glue-gateway-config` (`default_backend: ax`, `type: ax`, `atespace: default`, `image: ${GLUE_TASK_IMAGE}`) and Secret `glue-gateway-secrets` (`GLUE_GATEWAY_TOKEN`, `GLUE_GITHUB_TOKEN`, `GEMINI_API_KEY`, `OPENROUTER_API_KEY`).
- **Deploy & E2E verification on `kind`:**
  ```sh
  scripts/ax-dev/deploy-gateway.sh
  scripts/ax-dev/e2e/submit.sh
  ```
