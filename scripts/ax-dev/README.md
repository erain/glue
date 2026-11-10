# Local AX Cluster on kind (`scripts/ax-dev`)

Reproducible single-host [kind](https://kind.sigs.k8s.io/) cluster running pinned [Agent Substrate](https://github.com/agent-substrate/substrate) (`v1.0.0-rc.1`) and [AX](https://github.com/google/ax) (`241bc4d`) for Glue remote-mode development.

## Commands

| Command | Make target | Description |
|---|---|---|
| `scripts/ax-dev/prereqs.sh` | `make ax-dev-prereqs` | Verify Docker, `kubectl`, Go 1.27 toolchain, kernel >= 6.8, cgroup v2, disk >= 40 GB, `/dev/kvm`, and `inotify` limits |
| `scripts/ax-dev/up.sh` | `make ax-dev-up` | Provision `glue-ax`, install Substrate + AX, build/push `ax-server` and `ax-task-runner` to `localhost:5001`, write `${STATE}/images.env` (idempotent) |
| `scripts/ax-dev/status.sh` | `make ax-dev-status` | Verify `ate-system`, `ax-system`, and `ax-workers` (`GLUE_AX_WORKERS`, default 4) are Ready |
| `scripts/ax-dev/smoke.sh` | `make ax-dev-smoke` | Run the 6-assertion platform smoke test (apply/resume/ssh, default-deny egress, `tls_passthrough` egress, `kubectl ate proxy` ingress on `:8080`, suspend/resume `/workspace` durability, delete) |
| `scripts/ax-dev/down.sh` | `make ax-dev-down` | Tear down `glue-ax`, `kind-registry`, and any leftover port-forwards/tunnels |

Cached sources (`substrate`, `ax`), host binaries (`kind`, `kubectl-ate`, `ax`), `kubeconfig`, and `images.env` live under `STATE=${XDG_CACHE_HOME:-~/.cache}/glue/ax-dev`. Source `scripts/ax-dev/versions.env` to add `${STATE}/bin` and `KUBECONFIG` to your shell.

## Cluster Configuration & Operational Notes

- **Ingress allowlist (`--ingress-allowed-spiffe-ids`):** permits `spiffe://cluster.local/ns/ax-system/sa/ax-server` (`ax ssh`), `spiffe://cluster.local/ns/ate-system/sa/atenet-client` (`kubectl ate proxy`), and `spiffe://cluster.local/ns/glue-system/sa/glue-gateway` (`glue-gateway`).
- **Credential provider:** `--credential-provider='{"name":"k8s.io"}'` deploys Substrate's bundled Kubernetes Secrets egress credential provider.
- **Worker pool:** `ax-workers` runs `GLUE_AX_WORKERS` (default `4`) gVisor replicas with node `fs.inotify.max_user_instances=8192`.
- **Default-deny egress:** tasks have no outbound network access (including DNS) until a per-actor `EgressPolicy` is created via `kubectl ate create egress-policy <task> -a <atespace> -f -`.
- **Router mTLS only:** `atenet-router` only serves mTLS on ports `443` (HTTP) and `8444` (`CONNECT` tunnel); use `kubectl ate proxy actor <task> <local>:<remote> -a <atespace>`.
- **Tasks start `Suspended` & require digest-pinned images:** `ax apply` creates tasks in `Suspended` phase (`ax resume task <name>` schedules onto `ax-workers`), and images must use `@sha256:...` digests from `${STATE}/images.env`.
