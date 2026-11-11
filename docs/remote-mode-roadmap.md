# Remote Mode Roadmap

Start a Glue coding job from your laptop in one command, close the lid, and pick
it up later from any terminal or a browser. Jobs execute directly as isolated
tasks on [AX](https://github.com/google/ax) — with AX as Glue's sole external
platform dependency — or locally under `glue serve`. Phase 1 MVP
([#402](https://github.com/erain/glue/issues/402),
[ADR-0019](adr/0019-remote-jobs-architecture.md)) and Phase 2 AX-first
refinement ([#526](https://github.com/erain/glue/issues/526),
[ADR-0020](adr/0020-ax-first-direct-execution.md)) track the architecture and
milestones below.

## The experience

1. `glue --remote "Fix failing race test" --verify "go test -race ./..." --deliver pr`
   from any git checkout detects `origin` and the default base branch, pushes
   GitHub and model credentials in memory, and starts an AX task immediately —
   no gateway deployment required.
2. Close the terminal. Under the default `auto` policy, the job plans, edits,
   runs verification commands, commits on `glue/<job-id>`, pushes the branch,
   and opens an upstream draft PR inside its isolated gVisor sandbox.
3. `glue job list`, `glue job show <id>`, `glue job attach <id>`, and
   `glue job result <id>` from any authorized laptop inspect status without
   waking suspended tasks, replay missed SSE events via `Last-Event-ID`, steer
   in-flight runs (`say` / `continue`), and read cached terminal results.
4. Optional human approval & mobile browser portal: pass `--policy workspace-auto`
   (or `ask-all`) for interactive approval on sensitive commands, and optionally
   run `glue-gateway` on a host or VM behind Cloudflare Tunnel (`GitHub OAuth`)
   to watch, approve, and auto-suspend idle tasks from a phone browser.

## Components

| Component | Where | Responsibility |
|---|---|---|
| `jobs` package | core module | Job spec, state machine, workspace, goal loop (`auto` default policy), event log, delivery, completion record |
| `glue serve` (protocol v2) | core module | Durable runs and jobs, resumable SSE, permissions, in-memory `/v1/credentials`, embedded browser UI |
| `internal/axbackend` | core module (internal) | Direct AX transport and lifecycle (`Connect` auto-tunnel + `PodCertificateRequest` mTLS cache, `RouterTransport`, `Workspace`/`Task`/egress provisioning, `ax-system/glue-ax-task-hmac` task auth, stateless `ListJobs`/`Show`, `~/.config/glue/ax-jobs/` terminal result cache) |
| `glue --remote` & `glue job` | core module | Direct Laptop-to-AX and `--server` HTTP CLI: submit, list, show, attach, steer, approve, result, cancel, delete |
| `webui` package | core module | Embedded HTML/CSS/JS (no build step) shared by `glue serve` and `glue-gateway` |
| `glue-gateway` (optional) | [`gateway/`](../gateway/README.md) module | Out-of-cluster host/VM HTTP portal: GitHub OAuth allowlist + personal API tokens, public hardening, SQLite job/event cache, idle/finish `SuspendTask` + on-demand `ResumeTask`, delegating pure `type: ax` backends to `internal/axbackend` |
| Task image & host tunnel | [`deploy/ax`](../deploy/ax/README.md), [`deploy/cloudflared`](../deploy/cloudflared/README.md) | Digest-pinned AX task runner image (`deploy/ax/task-image`), host `systemd` unit (`deploy/cloudflared/glue-gateway.service`), and public runbook ([`docs/runbooks/public-gateway.md`](runbooks/public-gateway.md)); zero `glue-system` K8s manifests |
| Standing dev cluster | [`scripts/ax-dev`](../scripts/ax-dev/README.md) | Warm `kind-glue-ax` cluster (`source scripts/ax-dev/env.sh`) running Agent Substrate `v1.0.0` GA + AX `56bd0fc55574` |

```
 laptop CLI: glue --remote / glue job ──────────────────► internal/axbackend ──► AX (ax-server + router mTLS CONNECT)
                                                                ▲
 phone / browser (optional) ──HTTPS──► Cloudflare Tunnel ──► host glue-gateway ─┴─► local glue serve (optional)

 AX task (gVisor): ax-task-runner ──► glue serve --coding --jobs --job-from-ax (:8080, /workspace/.glue)
```

## Job lifecycle

```
created ──start──► running ⇄ waiting_for_input ──► achieved | blocked | max_iterations
   │                  │  (opt-in ask policy)        | budget_limited | errored | canceled
   │                  └── process restart ──► running (job_resumed, same seq stream)
   └── nothing happens until start (safe for AX golden pre-boot)
terminal non-success ──continue(text)──► running
```

Jobs run autonomously to completion under the default `auto` policy. When an
opt-in human-approval policy (`workspace-auto` or `ask-all`) is used with the
optional `glue-gateway`, the gateway suspends idle tasks waiting for input and
on finish, resuming them automatically on user action.

## Milestones

| Milestone | Outcome | Issues |
|---|---|---|
| A. Durable jobs | Jobs run, survive restarts, and deliver PRs using only `glue serve` and the CLI | #403–#412 |
| B. Browser UI | Watch and steer jobs from a browser, covered by an automated browser test | #413–#415 |
| C. AX on kind | Pinned dev cluster, task image, and a hand-run end-to-end job (feasibility gate) | #416–#418 |
| D. Gateway | AX backend, suspend while waiting, results outlive tasks, runtime credentials | #419–#423 |
| E. Public access | GitHub sign-in and Cloudflare Tunnel | #424–#425 |
| F. Showcase | "Use AX to develop AX" demo and upstream feedback | #426 |
| G. Standing AX cluster | Warm `kind-glue-ax` upgraded to Agent Substrate `v1.0.0` GA + AX `56bd0fc` (`env.sh`) | #520 |
| H. Autonomous default | Default coding jobs and `glue serve` to `auto` policy (`DecisionAllow`, `"*"` binaries) | #521 |
| I. AX-first direct CLI | `internal/axbackend`, `glue --remote`, pure `type: ax` host gateway, remove `glue-system` manifests | #522–#524, #529–#530 |
| J. AX-first docs & E2E | ADR-0020, product-first docs/runbooks, and live Laptop-to-AX showcase verification | #525 |

Milestones A–F shipped in tracker [#402](https://github.com/erain/glue/issues/402);
Milestones G–J refine remote execution in tracker [#526](https://github.com/erain/glue/issues/526).

## How each step is verified

Every issue lists named tests, exact commands with expected output, and the
evidence to paste into its PR. The common definition of done is in the
tracker: formatted, vetted, built, and tested (with `-race` for concurrent
code); real fixtures (in-process daemon, temp git repos, `httptest` fakes of
GitHub and AX) over mocks; opt-in tests that fail rather than skip in CI;
docs and CHANGELOG in the same PR. Cluster steps ship scripts under
`scripts/ax-dev/` that assert outcomes and exit non-zero on failure.

## AX findings that shape the design

Current standing cluster pins (`scripts/ax-dev/versions.env`): AX
`56bd0fc55574`, Agent Substrate `v1.0.0` (`b34fbee74290`), kind `v0.33.0`, Go
`1.27.x` (initial feasibility probe checked 2026-11-07 on AX `241bc4d` /
Substrate `v1.0.0-rc.1`).

- **Runtime:** gVisor or microVM only; AX's own e2e setup runs kind with
  gVisor on a single host. This machine meets the kernel (6.8, OverlayFS
  `lowerdir+`) and capacity requirements.
- **Egress:** default deny; AX creates no egress policy, so `internal/axbackend`
  creates one per task (GitHub, Go module proxy, model API hosts) before first
  `ResumeTask`.
- **Secrets:** no secret references; env is stored and shown in plain text
  ([google/ax#348](https://github.com/google/ax/issues/348), [google/ax#449](https://github.com/google/ax/issues/449)).
  Glue therefore creates AX `Task` resources with zero credential environment
  variables and pushes `github_token` and `provider_keys` over `atenet-router`
  to `POST /v1/credentials` in memory before `start`, after each `ResumeTask`,
  and whenever `GET /v1/jobs/{id}` reports `credentials: "absent"`; `glue serve`
  redacts known credential values and common token shapes (`ghp_…`,
  `github_pat_…`, `gho_…`, `AIza…`, `sk-…`) before writing events, tool outputs,
  session files, or results to `/workspace/.glue`.
- **Lifecycle:** new tasks start suspended; suspend keeps only `/workspace`
  and may not signal the process; resume restarts the command; a golden
  pre-boot runs the command without network; nothing auto-suspends; the exit
  status is not surfaced ([google/ax#346](https://github.com/google/ax/issues/346)).
- **Networking:** the router accepts only allowlisted mTLS identities on
  ports 443 and 8444, supports arbitrary-port ingress via `CONNECT`, wakes
  suspended tasks on any request, and cuts idle streams after five minutes.
  AX's documented `port-forward ... 8001:80` example is stale.
- **Developing AX in a task:** AX's CI gate is build, `go test ./...` (fast,
  in-process fakes), vet, gofmt, and a `go mod tidy` diff; its e2e tier needs
  Docker and kind and is out of reach inside a sandbox. AX currently limits
  pull requests to collaborators.

Related upstream work to align with: [google/ax#430](https://github.com/google/ax/issues/430) (interactive clients),
[google/ax#420](https://github.com/google/ax/issues/420) (`onCompletion`), [google/ax#549](https://github.com/google/ax/issues/549) (Gateway), [google/ax#499](https://github.com/google/ax/issues/499)
(egress and secret env), [google/ax#451](https://github.com/google/ax/issues/451) (suspend signal), [google/ax#428](https://github.com/google/ax/issues/428)
(golden boot).
