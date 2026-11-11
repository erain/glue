# Runbook: Manual End-to-End Glue Job Inside AX (`scripts/ax-dev/manual-job.sh`)

> **Normal workflow:** Use `source scripts/ax-dev/env.sh && glue --remote "<objective>" --attach` (or `scripts/ax-dev/e2e/submit.sh`) to run coding jobs directly on AX via `internal/axbackend` ([ADR-0020](../adr/0020-ax-first-direct-execution.md)). This runbook documents the low-level `ax` / `kubectl ate` plumbing steps exercised by `scripts/ax-dev/manual-job.sh` from the Milestone C feasibility gate (`#418`).

## Prerequisites

1. Provision the local cluster and build the digest-pinned `glue-task` image:
   ```sh
   scripts/ax-dev/up.sh
   scripts/ax-dev/build-task-image.sh
   ```
2. Export `GEMINI_API_KEY` (or `OPENROUTER_API_KEY`) and ensure `GLUE_GITHUB_TOKEN` is set or `gh auth token` is signed in.

## End-to-End Flow (`scripts/ax-dev/manual-job.sh`)

`scripts/ax-dev/manual-job.sh` automates the 6 manual steps in order and is safe to rerun:

1. **Workspace (`<name>-ws`):** Creates `Workspace` with `spec.git[0] = {name: "repo", repo: <url>, branch: <base>, dir: ".", depth: 1}` so AX checks out the target repository directly at `/workspace` (rather than `/workspace/<repo-name>`).
2. **Task (`<name>`):** Creates `Task` in `Suspended` phase with `GLUE_TASK_IMAGE` (`@sha256:...` from `${STATE}/images.env`), `resources.limits: {cpu: "4", memory: "8Gi"}`, command `glue serve --coding --jobs --job-from-ax --listen :8080 --state-dir /workspace/.glue/state --token-file /workspace/.glue/token`, `spec.config` (the `jobs.Spec` payload), and dev-only `spec.env` (`GLUE_GITHUB_TOKEN`, `GEMINI_API_KEY`).
3. **EgressPolicy (before resume):** Creates `EgressPolicy` (`tls_passthrough` port `443`) for `github.com`, `api.github.com`, `codeload.github.com`, `proxy.golang.org`, `sum.golang.org`, `storage.googleapis.com`, `generativelanguage.googleapis.com`, and `openrouter.ai`.
4. **First resume (`ax resume task <name>`):** Schedules the actor onto `ax-workers`, runs maiden-run `SetupWorkspace` (`git init` + `git fetch --depth=1 origin <base>` + `git checkout -f FETCH_HEAD` using `/usr/local/bin/glue-git-askpass`), and starts `glue serve`. Waits for `phase: Running` and `WorkspaceReady: True`.
5. **Proxy & token login:** Starts `kubectl ate proxy actor <name> 18080:8080 -a default`, reads `/workspace/.glue/token` via `ax ssh`, and runs `glue job login`.
6. **Golden boot check, start, and attach:** Verifies `GET /v1/jobs` shows `status: "created"` before `POST /v1/jobs/<id>/start`, starts the job, streams events via `glue job attach`, and prints `glue job result --json`.

## Verified Scenarios (`https://github.com/erain/glue-sandbox`)

### 1. Golden Boot (`status: "created"` Before `start`)

```sh
./scripts/ax-dev/manual-job.sh \
  --recreate --name glue-manual \
  --repo https://github.com/erain/glue-sandbox \
  --objective "Create go.mod (module example.com/sandbox, go 1.24), greet.go (package sandbox; func Greet(name string) string returning \"Hello, \" + name + \"!\"), and greet_test.go (package sandbox; func TestGreet(t *testing.T)), then run go test ./..." \
  --no-start
```

Expected output:
```text
  [ok] Task glue-manual created in Suspended phase (52 ms)
  [ok] Task glue-manual is Running and WorkspaceReady=True (3150 ms)
  [ok] Connected to http://127.0.0.1:18080: job_id=job-593cb09ed1df status=created
  [ok] Golden boot verified: job job-593cb09ed1df is in status "created" before explicit start.
```

### 2 & 3. Detach/Reattach Cursor Continuity & `draft_pr` Completion

Starting `job-593cb09ed1df`, detaching (`kill` of `glue job attach` and `kubectl ate proxy` at saved cursor `seq=5` in `~/.config/glue/jobs/<hash>/job-593cb09ed1df.seq`), restarting `kubectl ate proxy`, and re-running `glue job attach` resumes via `Last-Event-ID: 5` with zero sequence gaps (`1..167`) and opens an upstream draft PR:

```json
{
  "job_id": "job-593cb09ed1df",
  "status": "achieved",
  "branch": "glue/job-593cb09ed1df",
  "base_sha": "bf0fbc54075f896925427d2c7d8acfff5821e94f",
  "head_sha": "4ea9d0c3c4635a0c86dc8380ff2530346cc717ef",
  "pr_url": "https://github.com/erain/glue-sandbox/pull/2",
  "delivery_error": "",
  "verify": [
    {
      "argv": ["go", "test", "./..."],
      "exit_code": 0,
      "duration_ms": 607,
      "output_tail": "ok  \texample.com/sandbox\t(cached)"
    }
  ]
}
```

### 4. Suspend, Router Auto-Resume, and Permission Approval (`ask-all`)

```sh
./scripts/ax-dev/manual-job.sh \
  --recreate --name glue-suspend --repo https://github.com/erain/glue-sandbox \
  --policy ask-all --deliver none --no-verify --port 18082 \
  --objective "Create note.txt containing \"ax-suspend-resume-ok\n\"." --no-attach
```

When `glue job show` reports `status: waiting_for_input`:
1. `ax suspend task glue-suspend` transitions the task to `Suspended` (`WORKER-IP: <none>`) in `132 ms`.
2. Any request through `kubectl ate proxy` (`glue job show`) causes `atenet-router` to auto-resume the actor on `ax-workers` (`822 ms`).
3. On restart, `glue serve` recovers the job from `/workspace/.glue/state`, expires the pre-suspend permission, emits `job_resumed` (`resumed: iteration 1 (restart)`), and re-issues the permission request (`job-d7e8ddfe5955:permission:17`).
4. `glue job approve --remember session job-d7e8ddfe5955 job-d7e8ddfe5955:permission:17` unblocks the run and completes with `status: "achieved"` (`head_sha: "9a005a7159ced68570073d07d60eb8ac29fc845d"`).

### 5. Failure Case (`max_iterations` + Non-Zero `verify` Exit Code)

```sh
./scripts/ax-dev/manual-job.sh \
  --recreate --name glue-fail --repo https://github.com/erain/glue-sandbox \
  --policy yolo --deliver none --max-iterations 1 \
  --verify "git show HEAD:upstream_release_tag.txt" --port 18083 \
  --objective "Verify that upstream_release_tag.txt was already committed in the repository before this job started; do not create or modify any files if it is missing."
```

Expected `glue job result --json`:
```json
{
  "job_id": "job-c59aa7be3542",
  "status": "max_iterations",
  "branch": "glue/job-c59aa7be3542",
  "base_sha": "bf0fbc54075f896925427d2c7d8acfff5821e94f",
  "head_sha": "bf0fbc54075f896925427d2c7d8acfff5821e94f",
  "verify": [
    {
      "argv": ["git", "show", "HEAD:upstream_release_tag.txt"],
      "exit_code": 128,
      "duration_ms": 29,
      "output_tail": "fatal: path 'upstream_release_tag.txt' does not exist in 'HEAD'"
    }
  ]
}
```

## Key Findings for Milestone D (`#420`–`#423`)

- **`Workspace.spec.git[0].dir: "."` is required:** AX clones into `/workspace/<repo-name>` unless `dir: "."` is set.
- **Shallow fetch detached `HEAD` (`FETCH_HEAD`):** AX initializes `/workspace` via `git init` + `git fetch --depth=1 origin <branch>` + `git checkout -f FETCH_HEAD`, leaving only `refs/remotes/origin/<branch>` (no `refs/heads/*`). Fixed in `internal/worktree/job.go`.
- **Private repo fetch during `SetupWorkspace`:** `ax-task-runner` runs `SetupWorkspace` after exporting `spec.env` (`GLUE_GITHUB_TOKEN`). Installing `/usr/local/bin/glue-git-askpass` (`core.askPass`) in `deploy/ax/task-image/Dockerfile` allows private repository clones on first resume without embedding tokens in URLs or `.git/config`.
- **Timings:** Task creation (`Suspended`): `~52 ms`; first `ax resume` (including shallow git clone + `WorkspaceReady`): `~3.15 s`; `ax suspend`: `~132 ms`; router auto-resume on proxy request: `~822 ms`.
