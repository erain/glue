# Remote Mode Roadmap

Fire a Glue coding job, disconnect, and pick it up later from any terminal or
a browser. Jobs run on this machine or as tasks on
[AX](https://github.com/google/ax). Status lives in tracker
[#402](https://github.com/erain/glue/issues/402); decisions are recorded in
[ADR-0019](adr/0019-remote-jobs-architecture.md). This document explains the
plan; each step is a self-contained issue with its own verification.

## The experience

1. `glue job submit --server https://<host> --repo <git repo> --objective "..."`
   prints a job ID. The job runs in an AX sandbox, or locally under
   `glue serve`.
2. Close the terminal. The job keeps working: plan, edit, test, check.
3. Later, from a phone: sign in with GitHub, watch the job live, and answer
   its permission request. While it waited, its AX task was suspended;
   answering resumes it.
4. The job finishes with verify evidence, a pushed branch, and a draft PR
   against the repository's upstream. The result outlives the sandbox.
5. `glue job attach <id>` from any terminal replays what you missed and
   continues live.

## Components

| Component | Where | Responsibility |
|---|---|---|
| `jobs` package | core module | Job spec, state machine, workspace, goal loop, event log, delivery, completion record |
| `glue serve` (protocol v2) | core module | Durable runs and jobs, resumable SSE, permissions, credentials endpoint, browser UI |
| `webui` package | core module | Embedded HTML/CSS/JS (no build step) shared by `glue serve` and the gateway |
| `glue job` CLI | core module | Submit, list, attach, steer, approve, result |
| `glue-gateway` | [`gateway/`](../gateway/README.md) module | Auth (`github` OAuth allowlist + server-side sessions + personal API tokens, or `dev-token`), public hardening (HSTS, trusted `X-Forwarded-Proto`, 1 MiB body limit, HTTP timeouts, redacted access logs, `/healthz`), SQLite job registry and event cache, embedded `webui`, `local` backend (`glue serve` over HTTP+SSE) and `ax` backend (`UpdateWorkspace`, `CreateTask` with zero secret env vars, `CreateActorEgressPolicy`, `ResumeTask`, `SuspendTask`, runtime credential push via `POST /v1/credentials`, idle `waiting_for_input` and terminal suspend/resume with permission ID remapping, task retention and `glue job delete`, mTLS `CONNECT` router transport) |
| Task, gateway & tunnel | [`deploy/ax`](../deploy/ax/README.md), [`deploy/cloudflared`](../deploy/cloudflared/README.md) | AX task runner image (`deploy/ax/task-image`), `glue-gateway` image + Kubernetes manifests (`deploy/ax/gateway`), Cloudflare Tunnel manifests + host `systemd` unit (`deploy/cloudflared`), and public runbook ([`docs/runbooks/public-gateway.md`](runbooks/public-gateway.md)) |
| Dev cluster | [`scripts/ax-dev`](../scripts/ax-dev/README.md) | Pinned kind + Agent Substrate + AX for development and e2e tests |

```
 browser / phone ──HTTPS──► Cloudflare Tunnel ──► glue-gateway
 glue job CLI ─────────────────────────────────►   ├─ backend: local ──► glue serve (this machine)
                                                   └─ backend: ax ──► ax-server (Workspace, Task, suspend/resume)
                                                                  ──► Substrate (per-task egress policy)
                                                                  ──► atenet-router (mTLS CONNECT) ──► task :8080
 AX task (gVisor): ax-task-runner ──► glue serve --jobs   /workspace: repo + .glue/state
```

## Job lifecycle

```
created ──start──► running ⇄ waiting_for_input ──► achieved | blocked | max_iterations
   │                  │  (permission asked)          | budget_limited | errored | canceled
   │                  └── process restart ──► running (job_resumed, same seq stream)
   └── nothing happens until start (safe for AX golden pre-boot)
terminal non-success ──continue(text)──► running
```

In AX, the gateway suspends the task when a job waits for input with no live
viewer, and on finish. Any user action resumes it first.

## Milestones

| Milestone | Outcome | Issues |
|---|---|---|
| A. Durable jobs | Jobs run, survive restarts, and deliver PRs using only `glue serve` and the CLI | #403–#412 |
| B. Browser UI | Watch and steer jobs from a browser, covered by an automated browser test | #413–#415 |
| C. AX on kind | Pinned dev cluster, task image, and a hand-run end-to-end job (feasibility gate) | #416–#418 |
| D. Gateway | One front door: AX backend, suspend while waiting, results outlive tasks, runtime credentials | #419–#423 |
| E. Public access | GitHub sign-in and Cloudflare Tunnel | #424–#425 |
| F. Showcase | "Use AX to develop AX" demo and upstream feedback | #426 |

Milestones A–B and C proceed in parallel. Milestone D starts only after the
hand-run job in #418 succeeds and its findings are folded into #420–#423.

## How each step is verified

Every issue lists named tests, exact commands with expected output, and the
evidence to paste into its PR. The common definition of done is in the
tracker: formatted, vetted, built, and tested (with `-race` for concurrent
code); real fixtures (in-process daemon, temp git repos, `httptest` fakes of
GitHub and AX) over mocks; opt-in tests that fail rather than skip in CI;
docs and CHANGELOG in the same PR. Cluster steps ship scripts under
`scripts/ax-dev/` that assert outcomes and exit non-zero on failure.

## AX findings that shape the plan (checked 2026-11-07)

Pinned: AX `241bc4d0c682a6898da5f718a9ec84fe7c9197f9`, Substrate
`v1.0.0-rc.1`, kind v0.33.0, Go 1.27.x for the gateway and task image.

- **Runtime:** gVisor or microVM only; AX's own e2e setup runs kind with
  gVisor on a single host. This machine meets the kernel (6.8, OverlayFS
  `lowerdir+`) and capacity requirements.
- **Egress:** default deny; AX creates no egress policy, so the gateway
  creates one per task (GitHub, Go module proxy, model API hosts).
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
