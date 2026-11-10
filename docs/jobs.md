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
3. **Branch push and draft PR delivery:** When `delivery` is `branch` or
   `draft_pr` and commits exist on `glue/<job-id>`, the manager pushes the
   branch to `origin`. When `GLUE_GITHUB_TOKEN` is set in the `glue serve`
   process environment (never passed to the coding shell tool environment), Git
   authenticates via a `GIT_ASKPASS` helper backed by the hidden
   `glue git-askpass` subcommand so the token never appears in `argv`, remote
   URLs, events, logs, or `result.json`. When `delivery` is `draft_pr` and the
   job status is `achieved`, the manager parses `owner/repo` from `origin`
   (`https://` and `git@` forms), queries `GET /repos/{owner}/{repo}` on
   `GLUE_GITHUB_API_URL` (default `https://api.github.com`), and opens a draft
   pull request against `parent.full_name` with `head: "<owner>:<branch>"` when
   `origin` is a fork (or against `origin` with `head: "<branch>"` otherwise)
   and `base` set to the job's `BaseRef` (or target default branch). The PR
   body includes the objective, summary, checklist, verification results, token
   usage, and `"Opened by Glue job <id>"`. Push or API failures record
   `delivery_error` while preserving the job's terminal status.
4. **Completion record:** The manager atomically writes
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

## CLI (`glue job`)

`glue job` speaks `/v1/jobs` against `--server`/`GLUE_SERVER` using `--token`/`GLUE_TOKEN`, `~/.config/glue/servers.json` (`0600`, written by `glue job login --server <url>`), or local daemon metadata: `login`, `submit` (`--select`, `--backend`, `--policy`, `--verify`, `--deliver`, `--attach`), `start`, `list`, `show`, `attach`, `result [--json]`, `approve`, `deny`, `say`, `continue`, and `cancel`.

`glue job attach <id>` (and `glue job submit ... --attach`) replays events strictly after the last seen sequence cached in `~/.config/glue/jobs/<server-hash>/<id>.seq` (`0600`), renders the transcript (`plan`, `iteration_start`, `text_delta`, `tool_start`, `tool_end`, `verdict`, `operator_message`, `run_interrupted`, `job_resumed`, `job_done`), prompts inline for pending permissions, detaches on Ctrl-C without cancelling the server job, and reconnects with `Last-Event-ID` backoff when the stream drops before `job_done`.

## Browser UI (`webui`) and Session Auth

`glue serve` mounts `webui.Handler()` at `/` and `/static/*` with `Content-Security-Policy: default-src 'self'`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, and `X-Frame-Options: DENY`. Browsers authenticate via `POST /v1/session/login` (`{"token": "<daemon-token>"}`), which sets an `HttpOnly; SameSite=Strict` cookie (`glue_session`, `Secure` over TLS). Authenticated routes accept either `Authorization: Bearer <token>` or the session cookie; state-changing cookie-authenticated requests require `X-Glue-CSRF` matching `csrf_token` from `GET /v1/session` (else `403 Forbidden`).

Opening `http://<listen-addr>/` in a browser renders three dependency-free ES-module views (`webui/static/index.html`, `api.js`, `app.js`, `app.css`):

- **Login view:** prompts for the daemon token, calls `POST /v1/session/login`, and fetches `csrf_token` from `GET /v1/session`. All state-changing browser requests (`POST` and `DELETE`) flow through `api.js` with `X-Glue-CSRF`.
- **Jobs list view (`#/`):** shows each job's status badge, pending permission count (`N pending`), truncated objective, repository, model, iteration (`iter X/Y`), and updated timestamp, with status filtering and 10-second auto-refresh.
- **Job detail view (`#/jobs/<id>`):** renders the job header (objective, status, iteration/max, model, repository, token usage, and a confirmation-gated **Cancel job** control calling `DELETE /v1/jobs/{id}`), a top pending-permissions banner, checklist with `[x]`/`[ ]` completion marks, and live SSE transcript over `EventSource(/v1/jobs/{id}/events)` (merged `text_delta` assistant blocks, collapsible tool calls with 4 KiB output truncation and "Show more", goal/lifecycle dividers, and interactive permission cards hydrated from both SSE and `GET /v1/jobs/{id}/permissions?state=pending` with **Allow once**, **Allow for session**, and **Deny** with optional reason) with `Last-Event-ID` reconnection and client-side `seq` deduplication. Active jobs show a **Send message** form (`POST /v1/jobs/{id}/messages`); terminal non-success jobs show **Continue with instructions** (`POST /v1/jobs/{id}/continue`, re-attaching the live event stream). Terminal jobs render a **Result** panel from `GET /v1/jobs/{id}/result` with final status, clickable PR link (`pr_url`), branch, `base_sha → head_sha`, diffstat, verification commands with exit code badges and expandable output tails, iterations, token usage, and estimated cost when pricing is configured. Responsive layout collapses to a single column below `700px`.
