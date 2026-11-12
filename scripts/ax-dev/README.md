# Standing Local AX Cluster on kind (`scripts/ax-dev`)

Reproducible standing [kind](https://kind.sigs.k8s.io/) cluster (`kind-glue-ax`) running pinned [Agent Substrate](https://github.com/agent-substrate/substrate) (`v1.0.0` GA, `b34fbee74290`) and [AX](https://github.com/google/ax) (`56bd0fc55574`) for Glue remote-mode development and instant end-to-end verification.

## Commands

| Command | Make target | Description |
|---|---|---|
| `scripts/ax-dev/prereqs.sh` | `make ax-dev-prereqs` | Verify Docker, `kubectl`, Go 1.27 toolchain, kernel >= 6.8, cgroup v2, disk >= 40 GB, `/dev/kvm`, and `inotify` limits |
| `source scripts/ax-dev/env.sh` | `make ax-dev-env` | Export `KUBECONFIG`, `KUBE_CONTEXT`, `GLUE_AX_CONTEXT`, `GLUE_TASK_IMAGE`, and `PATH` from `${STATE}` without touching `~/.kube/config` |
| `scripts/ax-dev/up.sh` | `make ax-dev-up` | Provision `glue-ax`, install Substrate `v1.0.0` release images + AX (`56bd0fc`), build/push `ax-server` and `ax-task-runner` to `localhost:5001`, write `${STATE}/images.env` (idempotent) |
| `scripts/ax-dev/build-task-image.sh` | `make ax-dev-task-image` | Build `deploy/ax/task-image/Dockerfile` from `ax-task-runner` digest, push `localhost:5001/glue-task:dev`, record `GLUE_TASK_IMAGE` in `${STATE}/images.env` |
| `scripts/ax-dev/status.sh` | `make ax-dev-status` / `make ax-dev-ready` | Verify `ate-system`, `ax-system`, and `ax-workers` (`GLUE_AX_WORKERS`, default 4) are Ready (`--quiet` for fast single-line probe) |
| `scripts/ax-dev/smoke.sh` | `make ax-dev-smoke` | Run the 6-assertion platform smoke test and clean up only its test `Task` + `EgressPolicy`, leaving `glue-ax` warm |
| `scripts/ax-dev/manual-job.sh` | `make ax-dev-manual-job ARGS="..."` | Run an end-to-end Glue coding job inside an AX `Task` (`Workspace`, `Task`, `EgressPolicy`, `ax resume`, `kubectl ate proxy`, golden boot check, `glue job start/attach/result`; see [`docs/runbooks/ax-manual-job.md`](../../docs/runbooks/ax-manual-job.md)) |
| `scripts/ax-dev/e2e/submit.sh` | `make ax-dev-e2e-submit ARGS="..."` | Submit a coding job directly to AX via `glue --remote`, verify draft PR delivery + `go test ./...`, and close the test PR + delete the AX job (`KEEP_E2E_JOB=true` to retain) |
| `scripts/ax-dev/e2e/suspend-resume.sh` | `make ax-dev-e2e-suspend ARGS="..."` | Run an out-of-cluster host `glue-gateway` process (`type: ax`) to verify AX task idle suspend on `waiting_for_input`, read-only inspection without wakeup, on-demand resume, suspend on finish, and cleanup |
| `scripts/ax-dev/e2e/image-smoke.sh [image]` | — | Run the task image and print the version of every bundled tool (Go, Node.js, npm, Python, venv, uv, gcc, make, git, ripgrep, jq, curl, glue) and its size; fails if one is missing |
| `scripts/ax-dev/down.sh` | `make ax-dev-down` | Explicitly tear down `glue-ax`, `kind-registry`, and any leftover port-forwards/tunnels |

## Standing Cluster Isolation

Cached sources (`substrate`, `ax`), host binaries (`kind`, `kubectl-ate`, `ax`), `kubeconfig`, and `images.env` live under `STATE=${XDG_CACHE_HOME:-~/.cache}/glue/ax-dev`:

```sh
source scripts/ax-dev/env.sh
# or: eval "$(make -s ax-dev-env)"
```

- **Host kubeconfig untouched:** `up.sh` and `env.sh` always isolate `KUBECONFIG` to `${STATE}/kubeconfig` (`GLUE_AX_KUBECONFIG`) and set `KUBE_CONTEXT=kind-glue-ax` (`GLUE_AX_CONTEXT=kind-glue-ax`), never modifying `~/.kube/config` or interfering with other kind clusters on the host.
- **Warm standing lifecycle:** `smoke.sh`, `e2e/submit.sh`, and `e2e/suspend-resume.sh` delete only their per-run `Task`, `Workspace`, and `EgressPolicy` resources on exit and leave `kind-glue-ax` running for subsequent tests. Use `make ax-dev-down` only when you explicitly want to remove the cluster.

## Cluster Configuration & Operational Notes

- **Substrate `v1.0.0` release images:** `up.sh` installs published images from `ghcr.io/agent-substrate/substrate:<tag>` via `ate-setup --image-repo` / `--image-tag` without building Substrate from source.
- **Ingress allowlist (`--ingress-allowed-spiffe-ids`):** permits `spiffe://cluster.local/ns/ax-system/sa/ax-server` (`ax ssh` and `internal/axbackend` short-lived client certs) and `spiffe://cluster.local/ns/ate-system/sa/atenet-client` (`kubectl ate proxy`).
- **Credential provider:** `--credential-provider='{"name":"k8s.io"}'` deploys Substrate's bundled Kubernetes Secrets egress credential provider.
- **Worker pool:** `ax-workers` runs `GLUE_AX_WORKERS` (default `4`) gVisor replicas with node `fs.inotify.max_user_instances=8192`.
- **Default-deny egress:** tasks have no outbound network access (including DNS) until a per-actor `EgressPolicy` is created via `kubectl ate create egress-policy <task> -a <atespace> -f -`.
- **Router mTLS only:** `atenet-router` only serves mTLS on ports `443` (HTTP) and `8444` (`CONNECT` tunnel); use `kubectl ate proxy actor <task> <local>:<remote> -a <atespace>`.
- **Tasks start `Suspended` & require digest-pinned images:** `ax apply` creates tasks in `Suspended` phase (`ax resume task <name>` schedules onto `ax-workers`), and images must use `@sha256:...` digests from `${STATE}/images.env`.
