# ADR-0020: AX-First Direct Execution and Default Autonomous Policy

## Status

Accepted, 2026-11-11. Refines [ADR-0019](0019-remote-jobs-architecture.md) and
supersedes its in-cluster gateway requirement (`glue-system`) and
interactive-first job policy default. Tracked in
[#526](https://github.com/erain/glue/issues/526) under
[#392](https://github.com/erain/glue/issues/392).

## Context

[ADR-0019](0019-remote-jobs-architecture.md) placed remote jobs on
[AX](https://github.com/google/ax) behind a mandatory `glue-system` Kubernetes
gateway deployment (`deploy/ax/gateway/`) and defaulted jobs to
`workspace-auto`. Requiring cluster manifests and explicit Substrate/router
endpoints before running a CLI job added operational friction even though AX
(`56bd0fc55574`) and Agent Substrate (`v1.0.0`) allow authorized clients to
tunnel to `ax-server` and mint short-lived `PodCertificateRequest` router
credentials directly. Meanwhile, `workspace-auto` paused unattended jobs on
routine commands (`git remote`, `curl`, `gh`) despite gVisor sandboxing,
workspace confinement, `GIT_ASKPASS`, and secret redaction.

## Decision

1. **Laptop-to-AX direct execution.** `glue --remote "<objective>"` and
   `glue job` execute directly against AX via `internal/axbackend` with zero
   `glue-gateway` deployment required. `glue` auto-detects `origin` (normalizing
   SSH to HTTPS) and the base branch, pushes ambient GitHub and provider
   credentials in memory (`POST /v1/credentials`), and reconstructs job state
   from AX `Task.Spec.Config` + `ax-system/glue-ax-task-hmac` + `~/.config/glue/ax-jobs/`
   (`0600`) so multi-laptop `list`, `show`, `attach`, and `result` work without
   waking suspended sandboxes on read-only inspection.
2. **AX as sole external platform dependency.** Glue ships zero `glue-system`
   Kubernetes namespaces, Deployments, ServiceAccounts, or PVCs
   (`deploy/ax/gateway/` and `deploy/cloudflared/manifests.yaml` are removed),
   and root `package glue` keeps zero AX imports. Internal Substrate transport
   details (`RouterTransport` mTLS `CONNECT`, `PodCertificateRequest` rotation,
   `CreateActorEgressPolicy`, and `~/.config/glue/tunnels/` port-forwards) stay
   encapsulated inside `internal/axbackend` and never appear in user flags or YAML.
3. **Autonomous execution (`auto`) by default.** Coding jobs
   (`jobs.BuiltInPolicy("")`) and `glue serve` default to `auto` (`DecisionAllow`,
   unrestricted workspace binary basenames `"*"`) without `yolo` warnings while
   preserving `tools/fs.SafeJoin` confinement, secret file blocklists, and
   `internal/redact`. `workspace-auto`, `ask-all`, and `--policy-file` remain
   opt-in human-approval profiles.
4. **Optional out-of-cluster `glue-gateway`.** `gateway/` (`glue-gateway`) is
   kept strictly as an optional host/VM HTTP+SSE portal (mobile `webui`, GitHub
   OAuth, personal API tokens, Cloudflare Tunnel via
   `deploy/cloudflared/glue-gateway.service`, and `suspend_after` idle suspend)
   configured with pure `type: ax` fields (`atespace`, `image`, `resources`,
   `egress_hosts`, optional `ax_server` / `kube_config` / `kube_context`).

## Consequences

- Any laptop with AX access runs, inspects, attaches, steers, and retrieves
  remote coding jobs in one command (`glue --remote "<objective>"`); local mode
  continues working offline without AX.
- Root `go.mod` moves to Go 1.27 (`internal/axbackend` only), direct CLI runs
  and optional host `glue-gateway` instances interoperate via
  `ax-system/glue-ax-task-hmac`, and standing `kind-glue-ax` (`scripts/ax-dev/env.sh`)
  verifies both paths without touching `~/.kube/config`.
