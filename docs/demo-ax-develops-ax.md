# Demo & Runbook: Use AX to Develop AX with Glue

End-to-end showcase for Glue remote mode ([tracker #402](https://github.com/erain/glue/issues/402), [tracker #526](https://github.com/erain/glue/issues/526), [ADR-0019](adr/0019-remote-jobs-architecture.md), [ADR-0020](adr/0020-ax-first-direct-execution.md)): dispatch a coding job directly from your laptop to an [AX](https://github.com/google/ax) cluster in one command (`glue --remote "<objective>"`), close your lid while it executes autonomously (`auto` policy by default), and inspect or retrieve verified commits and a draft pull request — or optionally attach a host-mode `glue-gateway` for mobile browser approvals and idle-wait task suspension.

> **New here?** Follow the [remote quickstart](remote-quickstart.md) instead: it needs no locally built image and is checked end to end by `scripts/demo/aha.sh`. This page is the Phase 2 showcase record (AX `56bd0fc`, 2026-11), kept for its measurements and upstream AX feedback; some numbers predate later speedups (for example, submit now takes about 3 s, not 13 s).

## 1. Pre-Flight Checklist

1. **AX contribution rules and repository access:**
   `google/ax` currently restricts pull requests to collaborators and asks new external contributors to get vouched first. Verify your permission level before targeting `google/ax`:
   ```sh
   gh repo view google/ax --json viewerPermission   # {"viewerPermission":"MAINTAIN"} (or WRITE/ADMIN)
   ```
   To run the live showcase without opening noise PRs on `google/ax`, target a fork or sandbox repository (`https://github.com/erain/glue-sandbox`) where your token has push and PR access; the execution path is identical.
2. **GitHub token and model credentials:**
   Confirm `gh auth status` includes `repo` scope (`'gist', 'project', 'read:org', 'repo', 'workflow'`) and `GEMINI_API_KEY` (or `OPENROUTER_API_KEY`) is exported in your shell or `~/.env`. `glue --remote` (and the optional host `glue-gateway`) picks up `GLUE_GITHUB_TOKEN` / `GITHUB_TOKEN` / `gh auth token` and provider keys automatically and pushes them in memory to tasks via `POST /v1/credentials` — never storing secrets in `Task.spec.env`.
3. **Standing AX cluster capacity and task resources:**
   Load the standing cluster environment and verify `ate-system`, `ax-system`, and `ax-workers` (`4` gVisor replicas) are Ready with zero custom Glue cluster deployments:
   ```sh
   source scripts/ax-dev/env.sh
   scripts/ax-dev/status.sh
   ```
   `internal/axbackend` defaults task limits to `cpu: "4", memory: "8Gi"` so AX's unit test suite (`go test ./...`, 16 packages with in-process Substrate fakes) compiles and runs comfortably inside the sandbox.
4. **Per-task TLS egress allowlist (`tls_passthrough:443`):**
   `github.com`, `api.github.com`, `codeload.github.com`, `proxy.golang.org`, `sum.golang.org`, `storage.googleapis.com`, `generativelanguage.googleapis.com`, and `openrouter.ai`.

## 2. End-to-End Runbook

### Path A (Default): Direct Laptop-to-AX Execution (`glue --remote`)

Pick a scoped AX fix (for example, updating the stale `kubectl port-forward -n ate-system svc/atenet-router 8001:80` example in AX's docs to `kubectl ate proxy actor`, keeping `--verify` to the unit tier documented in AX's `AGENTS.md`) and submit directly to AX — no gateway deployment needed:

```sh
source scripts/ax-dev/env.sh

id=$(glue --remote 'Update docs/manifests.md to note that atenet-router requires mTLS via kubectl ate proxy actor rather than plain HTTP port-forward on port 80.' \
  --repo https://github.com/google/ax \
  --verify 'go build ./cmd/ax ./cmd/ax-server ./cmd/ax-task-runner' \
  --verify 'go test ./...' \
  --verify 'go vet ./...' \
  --deliver pr)

glue job attach "$id"          # Ctrl-C detaches anytime without canceling the AX task
glue job show "$id"            # reconstructs status without waking a suspended sandbox
glue job result "$id" --json | jq '{status, pr_url, verify: [.verify[] | {argv, exit_code}]}'
gh pr view <pr-url> --json isDraft,state,title
```

Under the default `auto` policy, `glue serve` runs inside the isolated AX gVisor sandbox, completes the plan/make/check loop, runs every `--verify` command, commits on `glue/<job-id>`, pushes via `GIT_ASKPASS`, opens an upstream draft PR, and caches `result.json` locally under `~/.config/glue/ax-jobs/`. Run the automated Laptop-to-AX suite anytime with `scripts/ax-dev/e2e/submit.sh`.

### Path B (Optional): Mobile Browser Approvals & Idle Suspend (`glue-gateway`)

When you want mobile browser access (`webui`), GitHub OAuth sign-in, or automatic AX task suspension while waiting on interactive permission prompts (`--policy workspace-auto` or `ask-all`), start `glue-gateway` out-of-cluster on a host or VM with pure `type: ax` config (see [`gateway/README.md`](../gateway/README.md), [`docs/runbooks/public-gateway.md`](runbooks/public-gateway.md), and `scripts/ax-dev/e2e/suspend-resume.sh`):

1. Submit with `--server "https://${GLUE_PUBLIC_HOST}" --policy workspace-auto --deliver pr`. Workspace edits and `go` commands auto-approve (`permission_auto`), while `git remote -v` / `curl` / `ssh` pause in `waiting_for_input`; with no live SSE viewer, `glue-gateway` calls `SuspendTask` after `suspend_after` (`2m` default).
2. Open `https://<host>` on a phone browser, inspect the **Checklist**, **Transcript**, and pending **Permission** card (`SUSPENDED – WAITING FOR YOU`), and tap **Allow for session**.
3. `glue-gateway` calls `ResumeTask`, pushes runtime credentials (`POST /v1/credentials`), remaps the approval onto the re-issued permission (`original_permission_id`), caches `result.json` in SQLite on completion, and suspends the task (`reason: "finished"`).

## 3. Recorded Live Showcase Executions (`kind-glue-ax`)

**Direct Laptop-to-AX (`job-66b247c96cee`, Substrate `v1.0.0` GA + AX `56bd0fc55574`):** Executed via `scripts/ax-dev/e2e/submit.sh` (`glue --remote ... --deliver draft_pr --verify "go test ./..." --attach`, default `auto` policy, `glue-task@sha256:d9cc2cb9fc15...`, `gemini-3-flash-preview`, `thinking=medium`) against `https://github.com/erain/glue-sandbox`:

```sh
$ glue job result --ax job-66b247c96cee --json | jq '{job_id, status, branch, head_sha, pr_url, verify}'
{
  "job_id": "job-66b247c96cee",
  "status": "achieved",
  "branch": "glue/job-66b247c96cee",
  "head_sha": "4087857e671285e7204dbbf7cfe43ea31f564342",
  "pr_url": "https://github.com/erain/glue-sandbox/pull/7",
  "verify": [
    {
      "argv": ["go", "test", "./..."],
      "exit_code": 0,
      "duration_ms": 632,
      "output_tail": "ok  \texample.com/sandbox\t(cached)"
    }
  ]
}
```

**Interactive Suspend/Resume Showcase (`job-628de8c215a8`, `--policy workspace-auto`):**

```sh
$ glue job result job-628de8c215a8 --json | jq '{status, pr_url, verify: [.verify[] | {argv, exit_code}]}'
{
  "status": "achieved",
  "pr_url": "https://github.com/erain/glue-sandbox/pull/5",
  "verify": [
    {
      "argv": ["git", "status", "--short"],
      "exit_code": 0
    },
    {
      "argv": ["test", "-s", "README.md"],
      "exit_code": 0
    }
  ]
}
```

### Timings, Event Lifecycle, and Usage

| Metric | Recorded Value | Notes |
|---|---|---|
| Submit → first streamed events (`seq 1..2`) | `20.65 s` | `UpdateWorkspace`, `CreateTask`, `CreateActorEgressPolicy`, first `ResumeTask` shallow clone, `POST /v1/credentials`, `POST /v1/jobs/{id}/start` |
| `waiting_for_input` → AX `Suspended` (`seq 20 → 22`) | `5.12 s` | `GLUE_SUSPEND_AFTER=5s` (`2m` default); `glue job list` and `glue job show` verified not to wake `glue-job-628de8c215a8` |
| Approve → AX `Running` + `permission_decision` (`seq 23 → 35`) | `12.84 s` | `ResumeTask`, credential push, `job_resumed` (`iteration 1`), `job-628de8c215a8:permission:20` remapped to `:31` |
| Resume → verify, push, draft PR, and finish suspend (`seq 35 → 138`) | `44.53 s` | `permission_auto` (`rule: "workspace-edit"` on `README.md`), checker `4/4 ✓`, verify commands, `git push`, `gh pr create --draft`, `SuspendTask` |
| Total active runtime (excluding idle human wait) | `78.02 s` | Contiguous event log `seq 1..138` (`contiguous_seq: true`) |
| Token usage & estimated cost | `40,995` tokens | `1` iteration (`36,419` input / `4,576` output tokens on `gemini-3-flash-preview`, `$0.0073 USD`) |

## 4. Recorded Failure Drills

| Drill | Job ID | Action | Observed Result |
|---|---|---|---|
| **1. Verify failure (`blocked` with evidence)** | `job-4c6f33e03a3d` | Submitted with `--verify 'git show HEAD:upstream_release_tag.txt'` and an unmet precondition (`upstream_release_tag.txt` absent from `HEAD`) | Ended in `status: "blocked"` after `3` iterations (`NoProgressLimit = 3`), opened no PR (`pr_url: ""`), and recorded `verify: [{"argv": ["git", "show", "HEAD:upstream_release_tag.txt"], "exit_code": 128, "duration_ms": 30, "output_tail": "fatal: path 'upstream_release_tag.txt' does not exist in 'HEAD'"}]`. |
| **2. Cancel from phone** | `job-b4f900581d3a` | Paused at `job-b4f900581d3a:permission:25` (`waiting_for_input`), then canceled via `DELETE /v1/jobs/job-b4f900581d3a` | Transitioned immediately to `status: "canceled"` (`status_reason: "finished"`) and suspended AX task `glue-job-b4f900581d3a` (`phase: Suspended`). |
| **3. Lost connection (2m airplane mode)** | `job-4a1520e4754a` | Disconnected SSE client at `seq = 33` for `126 s` while the job resumed and completed on the cluster, then reconnected with `Last-Event-ID: 33` | Replayed `seq 34..596` (`563` events) in order with zero gaps or duplicates (`{"pre_disconnect_seqs":[1,33],"replayed_first_seq":34,"replayed_last_seq":596,"total_events":596,"contiguous_no_gaps_or_dups":true}`). |
| **4. Gateway restart mid-job** | `job-4a1520e4754a` | Restarted `glue-gateway` process while waiting at `job-4a1520e4754a:permission:32` | Restarted gateway reloaded `events.sqlite` from `state_dir`, served `status: "waiting_for_input"` and pending permission `:32` without manual steps, and completed `job-4a1520e4754a` (`status: "achieved"`, `verify[0].exit_code: 0`). |

## 5. Upstream Feedback for AX (Draft for Owner to Post)

Summary of what Glue needed to run interactive, durable coding jobs on AX (`56bd0fc` + Substrate `v1.0.0`) and what AX could provide natively:

1. **Interactive clients, resumable event streams, and permission brokering ([google/ax#430](https://github.com/google/ax/issues/430)):**
   Glue runs `glue serve --jobs` inside the task and mirrors sequenced (`seq 1..N`), `Last-Event-ID`-resumable SSE events into `glue-gateway`'s SQLite cache so mobile browsers and `glue job attach` can disconnect, survive 5-minute `atenet-router` stream timeouts, and replay transcripts while the task is suspended. Native replayable task event logs and interactive approval hooks in AX would let any agent framework use this flow without a custom event-caching gateway.
2. **Structured task outcomes and post-retention durability ([google/ax#346](https://github.com/google/ax/issues/346)):**
   Because AX does not yet surface process exit codes or structured task artifacts after a task is suspended or deleted, Glue writes `/workspace/.glue/state/jobs/<id>/result.json` (`status`, `branch`, `base_sha`, `head_sha`, `pr_url`, `verify`, `usage`) and caches it in `glue-gateway` before retention cleanup. First-class task outcome records in AX would eliminate this duplication.
3. **Declarative idle and completion auto-suspend ([google/ax#420](https://github.com/google/ax/issues/420)):**
   Glue's gateway tracks live SSE viewers, calls `SuspendTask` after `suspend_after` when a job waits for human input (`waiting_for_input`), and calls `SuspendTask` on `job_done` (`suspend_on_finish`). Native `onCompletion: Suspend` and idle-wait auto-suspend policies on `Task` would move this lifecycle management into the AX controller.
4. **Declarative per-task egress and secret references ([google/ax#449](https://github.com/google/ax/issues/449), PR [google/ax#499](https://github.com/google/ax/pull/499), [google/ax#475](https://github.com/google/ax/issues/475)):**
   AX tasks default-deny egress and store `Task.spec.env` in plain text. Glue therefore creates a Substrate `EgressPolicy` (`tls_passthrough:443`) per task before first `ResumeTask`, omits secrets from `Task.spec.env`, pushes credentials into task memory over `atenet-router` (`POST /v1/credentials`) on start and resume, and redacts token patterns before disk writes. Native `Task` egress specs, secret env references (`#499`), and egress header credential injection (`#475`) so sandboxes never hold raw tokens would remove the largest operational workarounds.
5. **Gateway ingress resource without unintended wake-on-read ([google/ax#549](https://github.com/google/ax/issues/549)):**
   Any HTTP request routed through `atenet-router` automatically wakes a `Suspended` actor. Glue's gateway must serve all read-only queries (`list`, `show`, `permissions`, cached `events`, `result`) from its own SQLite database and `ax-server` `GetTask`, dialing `atenet-router:8444` (`mTLS CONNECT`) only on state-changing actions or live stream attaches. A native `Gateway` resource (`#549`) with explicit wake policy would prevent accidental wakeups.
6. **Pre-suspend signal and side-effect-free golden pre-boot ([google/ax#451](https://github.com/google/ax/issues/451), [google/ax#428](https://github.com/google/ax/issues/428)):**
   Because AX runs the task command once during a no-network golden pre-boot (`#428`) and may freeze/restart the container across suspend/resume without a signal (`#451`), Glue made `glue serve --job-from-ax` startup side-effect-free (requiring an explicit `POST /v1/jobs/{id}/start`), persisted every event and state transition continuously to `/workspace`, and re-issued/remapped pending permissions on restart (`job_resumed`). A pre-suspend signal (`#451`) and skipping or flagging golden pre-boot execution (`#428`) would simplify agent checkpointing.
