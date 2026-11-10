# Jobs

Glue jobs run autonomous coding objectives in isolated workspaces managed by
`jobs.Manager` and hosted over HTTP by `glue serve --jobs` (enabled by default
with `--coding`).

## Specification (`jobs.Spec`)

| Field | Type | Description |
|---|---|---|
| `objective` | `string` | Required coding goal description. |
| `repo` | `{path, url, base_ref}` | Exactly one of `path` (local git repo) or `url` (clone source), plus optional `base_ref`. |
| `provider`, `model`, `thinking` | `string` | Coding provider/model selection (defaults to `gemini` / `gemini-3.8-flash`). |
| `budgets` | `{max_iterations, token_budget, wall_clock}` | Iteration limit (default `10`), token cap, and wall-clock timeout. |
| `policy` | `string` | Permission profile (`workspace-auto` default, `ask-all`, `yolo`, or custom policy name). |
| `verify` | `[][]string` | Post-goal verification commands (`argv` slices) executed in the job workspace. |
| `delivery` | `string` | `none` (default), `branch`, or `draft_pr`. |
| `allowed_binaries`, `shell_env_pass`, `shell_env`, `shell_env_secret_allow`, `shell_timeout` | shell config | Per-job overrides for the coding shell and verification environment. |

## Lifecycle Statuses (`jobs.Status`)

- **Non-terminal:** `created`, `running`, `waiting_for_input`
- **Terminal:** `achieved`, `blocked`, `budget_limited`, `max_iterations`, `errored`, `canceled`, `interrupted`

Creating a job (`POST /v1/jobs`) persists the spec and `job_created` event
without starting execution unless `?start=true` or `POST /v1/jobs/{id}/start`
is called.

## Finalize Step and Completion Record

When a job finishes, the manager runs a finalize step before recording the
terminal `job_done` event:

1. **Verification:** For `achieved`, `blocked`, `max_iterations`, and
   `budget_limited` jobs, each `verify` command (`argv`) runs in the job
   workspace with the resolved job shell environment and timeout. The exit
   code, duration in milliseconds, and last 200 lines of combined output are
   recorded in `verify`.
2. **Workspace commit:** If the workspace has uncommitted changes, the manager
   stages all files (`git add -A`) and commits them on `glue/<job-id>` using
    the goal summary (or objective fallback) as the commit message and
   `GLUE_GIT_AUTHOR_NAME` / `GLUE_GIT_AUTHOR_EMAIL` when set (falling back to
   repository git config).
3. **Completion record:** The manager atomically writes
   `<state>/jobs/<id>/result.json` (`0600`) and serves it at
   `GET /v1/jobs/{id}/result` (`409 conflict` while the job is still active):

```json
{
  "job_id": "job-...",
  "status": "achieved",
  "objective": "write file",
  "base_ref": "main",
  "base_sha": "...",
  "head_sha": "...",
  "branch": "glue/job-...",
  "pr_url": "",
  "diffstat": " created.txt | 1 +\n 1 file changed, 1 insertion(+)",
  "verify": [
    {"argv": ["go", "test", "./..."], "exit_code": 0, "duration_ms": 142, "output_tail": "ok ..."}
  ],
  "iterations": 1,
  "usage": {"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
  "summary": "Add created.txt greeting",
  "finished_at": "2026-11-10T00:00:00Z",
  "delivery_error": ""
}
```
