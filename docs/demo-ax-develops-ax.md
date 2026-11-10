# Demo & Runbook: Use AX to Develop AX with Glue (`#426`)

End-to-end showcase for Glue remote mode ([tracker #402](https://github.com/erain/glue/issues/402), [ADR-0019](adr/0019-remote-jobs-architecture.md)): submit a coding job from a laptop against an [AX](https://github.com/google/ax) cluster, close the laptop, watch and approve a permission request from a phone browser while the AX task suspends on idle wait, and receive a verified upstream draft pull request.

## 1. Pre-Flight Checklist

1. **AX contribution rules and repository access:**
   `google/ax` currently restricts pull requests to collaborators and asks new external contributors to get vouched first. Verify your permission level before targeting `google/ax`:
   ```sh
   gh repo view google/ax --json viewerPermission   # {"viewerPermission":"MAINTAIN"} (or WRITE/ADMIN)
   ```
   To run the live showcase without opening noise PRs on `google/ax`, target a fork or sandbox repository (`https://github.com/erain/glue-sandbox`) where your token has push and PR access; the execution path is identical.
2. **GitHub token and model credentials:**
   Confirm `gh auth status` includes `repo` scope (`'gist', 'project', 'read:org', 'repo', 'workflow'`) and `GLUE_GITHUB_TOKEN` + `GEMINI_API_KEY` are configured in `glue-gateway-secrets` (pushed in memory to tasks via `POST /v1/credentials`, never stored in `Task.spec.env`).
3. **Cluster capacity and task resources:**
   Verify `scripts/ax-dev/status.sh` reports `ate-system`, `ax-system`, `glue-system`, and `ax-workers` (`4` gVisor replicas) Ready, and `ConfigMap/glue-gateway-config` sets `resources: {cpu: "4", memory: "8Gi"}` so AX's unit test suite (`go test ./...`, 16 packages with in-process Substrate fakes) compiles and runs comfortably inside the sandbox.
4. **Per-task TLS egress allowlist (`tls_passthrough:443`):**
   `github.com`, `api.github.com`, `codeload.github.com`, `proxy.golang.org`, `sum.golang.org`, `storage.googleapis.com`, `generativelanguage.googleapis.com`, and `openrouter.ai`.

## 2. End-to-End Runbook

### Step 1: Pick a Scoped AX Change and Submit from the Laptop

Pick a small, self-contained AX doc or unit-tested fix (for example, updating the stale `kubectl port-forward -n ate-system svc/atenet-router 8001:80` example in AX's docs to `kubectl ate proxy actor`, keeping `--verify` to the unit tier documented in AX's `AGENTS.md` since AX's `test/e2e` tier requires Docker/kind):

```sh
glue job login --server "https://${GLUE_PUBLIC_HOST}" --token "${GLUE_PAT}"

glue job submit \
  --server "https://${GLUE_PUBLIC_HOST}" \
  --backend ax \
  --repo https://github.com/google/ax \
  --objective 'First run shell_exec with argv ["git", "remote", "-v"] to inspect the repository remote, then update docs/manifests.md to note that atenet-router requires mTLS via kubectl ate proxy actor rather than plain HTTP port-forward on port 80.' \
  --verify 'go build ./cmd/ax ./cmd/ax-server ./cmd/ax-task-runner' \
  --verify 'go test ./...' \
  --verify 'go vet ./...' \
  --deliver draft_pr \
  --policy workspace-auto
```

Close the terminal. Under `--policy workspace-auto`, workspace file edits and `go` commands are auto-approved (`permission_auto`), whereas `git remote -v` (or `git push` / `curl` / `ssh`) triggers `DecisionAsk` (`waiting_for_input`). With no live SSE viewer attached, `glue-gateway` calls `SuspendTask` after `suspend_after` (`2m` default; `5s` in dev) and records `task_suspended` (`reason: "waiting_for_input"`).

### Step 2: Watch and Approve from a Phone Browser

1. Open `https://<host>` on a phone browser, sign in with GitHub OAuth (or gateway token in dev), and tap the job row (`SUSPENDED – WAITING FOR YOU`).
2. Inspect the **Checklist**, **Transcript**, and pending **Permission** card (`shell_exec (exec)` — `Target: git remote -v`).
3. Tap **Allow for session**. `glue-gateway` calls `ResumeTask`, pushes runtime credentials (`POST /v1/credentials`), waits for `job_resumed` (`reason: "restart"`), remaps the approval onto the re-issued permission (`original_permission_id`), and continues the goal loop.

### Step 3: Verify Completion and Upstream Draft PR

When the checker marks all checklist items complete, `glue serve` runs every `--verify` command, commits on `glue/<job-id>`, pushes to `origin` via `GIT_ASKPASS`, and opens a draft PR upstream. `glue-gateway` caches `result.json` in SQLite and suspends the AX task (`reason: "finished"`).

```sh
glue job result <id> --json | jq '{status, pr_url, verify: [.verify[] | {argv, exit_code}]}'
gh pr view <pr-url> --json isDraft,state,title
```

## 3. Recorded Live Showcase Execution (`job-628de8c215a8`)

Executed on `kind-glue-ax` (`glue-task@sha256:3674fd231190...`, `glue-gateway@sha256:ac622c3f9ecc...`, `gemini-3-flash-preview`, `thinking=medium`, `--policy workspace-auto`, `--deliver draft_pr`) against `https://github.com/erain/glue-sandbox`:

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

$ gh pr view https://github.com/erain/glue-sandbox/pull/5 --json isDraft,state,title
{
  "isDraft": true,
  "state": "OPEN",
  "title": "All tasks are complete: the git remote was inspected, and README.md was updated with the required AX Showcase section and description."
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
| **2. Cancel from phone** | `job-b4f900581d3a` | Paused at `job-b4f900581d3a:permission:25` (`waiting_for_input`), then cancelled via `DELETE /v1/jobs/job-b4f900581d3a` | Transitioned immediately to `status: "canceled"` (`status_reason: "finished"`) and suspended AX task `glue-job-b4f900581d3a` (`phase: Suspended`). |
| **3. Lost connection (2m airplane mode)** | `job-4a1520e4754a` | Disconnected SSE client at `seq = 33` for `126 s` while the job resumed and completed on the cluster, then reconnected with `Last-Event-ID: 33` | Replayed `seq 34..596` (`563` events) in order with zero gaps or duplicates (`{"pre_disconnect_seqs":[1,33],"replayed_first_seq":34,"replayed_last_seq":596,"total_events":596,"contiguous_no_gaps_or_dups":true}`). |
| **4. Gateway restart mid-job** | `job-4a1520e4754a` | Ran `kubectl -n glue-system rollout restart deployment/glue-gateway` (`glue-gateway-7f44f49895-7vz86` → `glue-gateway-5c95c45b49-g97m6`) while waiting at `job-4a1520e4754a:permission:32` | New gateway pod reloaded `events.sqlite` from PVC `glue-gateway-state`, served `status: "waiting_for_input"` and pending permission `:32` without manual steps, and completed `job-4a1520e4754a` (`status: "achieved"`, `verify[0].exit_code: 0`). |

## 5. Upstream Feedback for AX (Draft for Owner to Post)

Summary of what Glue needed to run interactive, durable coding jobs on AX (`241bc4d` + Substrate `v1.0.0-rc.1`) and what AX could provide natively:

1. **Interactive clients, resumable event streams, and permission brokering ([google/ax#430](https://github.com/google/ax/issues/430)):**
   Glue runs `glue serve --jobs` inside the task and mirrors sequenced (`seq 1..N`), `Last-Event-ID`-resumable SSE events into `glue-gateway`'s SQLite cache so mobile browsers and `glue job attach` can disconnect, survive 5-minute `atenet-router` stream timeouts, and replay transcripts while the task is suspended. Native replayable task event logs and interactive approval hooks in AX would let any agent framework use this flow without a custom event-caching gateway.
2. **Structured task outcomes and post-retention durability ([google/ax#346](https://github.com/google/ax/issues/346), PR [google/ax#556](https://github.com/google/ax/pull/556)):**
   Because AX does not yet surface process exit codes or structured task artifacts after a task is suspended or deleted, Glue writes `/workspace/.glue/state/jobs/<id>/result.json` (`status`, `branch`, `base_sha`, `head_sha`, `pr_url`, `verify`, `usage`) and caches it in `glue-gateway` before retention cleanup. First-class task outcome records in AX would eliminate this duplication.
3. **Declarative idle and completion auto-suspend ([google/ax#420](https://github.com/google/ax/issues/420)):**
   Glue's gateway tracks live SSE viewers, calls `SuspendTask` after `suspend_after` when a job waits for human input (`waiting_for_input`), and calls `SuspendTask` on `job_done` (`suspend_on_finish`). Native `onCompletion: Suspend` and idle-wait auto-suspend policies on `Task` would move this lifecycle management into the AX controller.
4. **Declarative per-task egress and secret references ([google/ax#449](https://github.com/google/ax/issues/449), PR [google/ax#499](https://github.com/google/ax/pull/499), [google/ax#475](https://github.com/google/ax/issues/475)):**
   AX tasks default-deny egress and store `Task.spec.env` in plain text. Glue therefore creates a Substrate `EgressPolicy` (`tls_passthrough:443`) per task before first `ResumeTask`, omits secrets from `Task.spec.env`, pushes credentials into task memory over `atenet-router` (`POST /v1/credentials`) on start and resume, and redacts token patterns before disk writes. Native `Task` egress specs, secret env references (`#499`), and egress header credential injection (`#475`) so sandboxes never hold raw tokens would remove the largest operational workarounds.
5. **Gateway ingress resource without unintended wake-on-read ([google/ax#549](https://github.com/google/ax/issues/549)):**
   Any HTTP request routed through `atenet-router` automatically wakes a `Suspended` actor. Glue's gateway must serve all read-only queries (`list`, `show`, `permissions`, cached `events`, `result`) from its own SQLite database and `ax-server` `GetTask`, dialing `atenet-router:8444` (`mTLS CONNECT`) only on state-changing actions or live stream attaches. A native `Gateway` resource (`#549`) with explicit wake policy would prevent accidental wakeups.
6. **Pre-suspend signal and side-effect-free golden pre-boot ([google/ax#451](https://github.com/google/ax/issues/451), [google/ax#428](https://github.com/google/ax/issues/428)):**
   Because AX runs the task command once during a no-network golden pre-boot (`#428`) and may freeze/restart the container across suspend/resume without a signal (`#451`), Glue made `glue serve --job-from-ax` startup side-effect-free (requiring an explicit `POST /v1/jobs/{id}/start`), persisted every event and state transition continuously to `/workspace`, and re-issued/remapped pending permissions on restart (`job_resumed`). A pre-suspend signal (`#451`) and skipping or flagging golden pre-boot execution (`#428`) would simplify agent checkpointing.
