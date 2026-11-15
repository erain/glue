# ADR-0021: Opt-In In-Cluster Gateway with Cloudflare Access
## Status
Accepted, 2026-11-15. Scoped amendment to [ADR-0020](0020-ax-first-direct-execution.md) item 4 ([#636](https://github.com/erain/glue/issues/636)).
## Context
[ADR-0020](0020-ax-first-direct-execution.md) made CLI `glue --remote` execute directly against AX without requiring an in-cluster gateway, removing mandatory `glue-system` manifests. However, restricting `glue-gateway` to out-of-cluster hosts forced users wanting a permanent 24/7 browser portal (`https://glue.<domain>`) to maintain a separate Linux VM, copy a `kubeconfig` onto it, and manage host `systemd` units — even though their Kubernetes cluster already runs 24/7 and can host a tiny control-plane `Deployment` outside gVisor `Actor` workers with zero AX worker slots consumed.
## Decision
1. **CLI remains 100% direct-to-AX.** `glue --remote` and `glue job` require zero cluster gateway and ship zero mandatory `glue-system` manifests.
2. **Opt-in `glue remote <setup|status|down>`.** `glue remote setup` pre-validates `gateway.yaml` (`KnownFields(true)`) and reconciles an opt-in 1-pod `Deployment` in namespace `glue-gateway` (`ate.dev/api-access: "true"`, `1Gi` SQLite PVC, runtime credential `Secret`, and least-privilege RBAC for `ClusterTrustBundle` plus `ax-system` `PodCertificateRequest`, `ax-server` token minting, and `glue-ax-task-hmac`).
3. **Zero-trust edge auth and sidecar tunnel.** In-cluster `glue-gateway` defaults to `auth.mode: cloudflare-access` (`Cf-Access-Jwt-Assertion` JWKS verification; `--dev-token` for local clusters) with an optional `cloudflared` sidecar forwarding to `http://127.0.0.1:8800`, leaving `ax-system` and `ate-system` controllers untouched.
## Consequences
Operators get a bookmarkable 24/7 browser portal on their existing AX cluster consuming `0` AX gVisor worker slots when idle and `0` separate VMs (`glue remote down [--purge-pvc]` tears it down), sharing `ax-system/glue-ax-task-hmac` and `internal/axbackend` with direct `glue --remote`.
