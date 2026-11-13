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
| `budgets` | `{max_iterations, token_budget, wall_clock}` | Iteration limit (default `10`), token cap, and wall-clock timeout. Jobs submitted directly to AX default to `wall_clock: 1h` and `token_budget: 2000000` when unset; `--no-budget` removes those defaults. A job stopped by a budget ends `budget_limited`, still runs the finalize step, and records `budget_exceeded` (`wall_clock` or `token_budget`) in `result.json`. |
| `policy` | `string` | Permission profile (`auto` default, `workspace-auto`, `ask-all`, `yolo`, or custom policy name). |
| `verify` | `[][]string` | Post-goal verification commands (`argv` slices) executed in the job workspace. |
| `delivery` | `string` | `none`, `branch`, or `draft_pr`. Defaults to `none` for local jobs and to `branch` for jobs submitted directly to AX, so work done in a sandbox always reaches `origin`. |
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
4. **Unfinished work:** When a job ends `canceled` or `errored` with changes in its workspace, the manager commits them on `glue/<job-id>` ("Unfinished work from Glue job …") and pushes the branch when `delivery` is `branch` or `draft_pr` (it never opens a PR for them). The result is marked `partial: true`. `interrupted` jobs are left alone, because a restart resumes them.
5. **Completion record:** The manager atomically writes
   `<state>/jobs/<id>/result.json` (`0600`) and serves it at
   `GET /v1/jobs/{id}/result` (`409 conflict` while the job is still active). `GET /v1/jobs/{id}/patch` returns `git diff --binary <base_sha>..<head_sha>` as `text/x-diff` (empty when nothing changed, `413` above 5 MiB with the branch to fetch instead):

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
  "delivery_error": "",
  "partial": false,
  "budget_exceeded": ""
}
```

## CLI (`glue --remote` and `glue job`)

From a git checkout, `glue --remote "<objective>"` and `glue job <subcommand>` execute directly against AX via `internal/axbackend` (`--ax`, `--kubeconfig`, `--context`, `--atespace`, `--image`, `--env`) with zero gateway deployment required ([ADR-0020](adr/0020-ax-first-direct-execution.md)): `glue` normalizes `remote.origin.url` to HTTPS, detects the default base branch, pushes ambient GitHub and provider credentials in memory (`POST /v1/credentials`), authenticates tasks via `ax-system/glue-ax-task-hmac`, and caches terminal `jobs.Result` records under `~/.config/glue/ax-jobs/` (`0600`) so `list`, `show`, and `result` do not wake suspended sandboxes.

When `--repo` is omitted (or is a local checkout) and `--base` is not given, a remote job starts from the current branch if `origin` has it, otherwise from `origin`'s default branch. Before anything is created, Glue compares the checkout with that remote ref (using remote-tracking refs, no network) and refuses to submit when the clone would miss local work: a branch that is not on `origin`, commits on `HEAD` that no `origin` ref contains, or uncommitted or untracked files (paths under `.glue/` are ignored). The error lists what was found and the ways forward: push first, pass `--base <ref>`, or pass `--ignore-local` to start from the remote ref on purpose.

`--snapshot` hands off work in progress instead. Glue builds one commit from the working tree (tracked changes plus untracked files that are not ignored, excluding `.glue/`) on top of `HEAD` using a temporary index, so your index, branch and stash are untouched. It pushes that commit to `glue/snapshot-<job-id>` on `origin` (unpushed commits come along as its parents) and starts the job from it. The job records `repo.snapshot_of: <branch>@<sha>` (shown by `glue job show`), and a draft PR targets that branch when `origin` has it, otherwise the default branch. A snapshot that adds more than 50 MiB of file content is refused unless `--snapshot-max-size` raises the limit. `glue job delete`, run from the checkout, also deletes the snapshot branch.

`glue job pull <id>` brings a finished job's changes into the current checkout. It refuses when the checkout's `origin` is not the job's repo. By default it fetches `glue/<id>` from `origin` into a local branch of the same name (`--checkout` also switches to it; an existing local branch needs `--force`). When the branch was never pushed, it builds the branch from the job patch at `base_sha` in a temporary worktree, so your checkout is untouched. `--apply` instead applies the job patch to the working tree with `git apply --3way` without committing, and refuses when the patch touches files with local changes unless `--force`.

Finished jobs stop holding an AX worker. In direct mode no component runs while you are away, so whichever `glue job` command comes next does the cleanup: `attach` suspends the task after `job_done`, once the result is cached locally; `list` and `show` ask each awake task (never a suspended one) for its status, four at a time with a 3 s timeout, and cache and suspend any that have finished; `cancel` suspends the task once the job reports a terminal status. A job that finishes while no client is connected keeps its worker until the next `glue job` command; the optional gateway or AX-native completion handling ([google/ax#420](https://github.com/google/ax/issues/420)) closes that gap. `glue job prune [--older-than 7d] [--dry-run]` deletes the AX task, workspace, and egress policy of finished jobs last updated before the cutoff, keeping the cached result so `glue job result` still works (against a `--server`, it deletes the job).

`submit` (and `glue --remote`) prints each AX stage as it completes on stderr (`workspace`, `task`, `egress`, `sandbox ready`, `credentials`, `started`, each with the time since submit), then a summary: repo and base ref, model and thinking level, policy and delivery, budgets, verify commands, and the `attach` / `show` / `cancel` commands. Stdout is only the job ID, so `id=$(glue --remote ...)` works; `--quiet` suppresses the stderr output. The output is plain text with no terminal control codes.

`glue job doctor [--json]` checks everything a remote job needs and prints `ok` or `FAIL` for each check, with one fix line per failure: the resolved target (AX context, kubeconfig and atespace, or an HTTP server and where it came from), the AX connection (tunnels and router credentials), `ax-server` (or the server) reachability, the task image, the repo's `origin` and base ref, the GitHub token's source and whether it can push to the repo (via `GET /repos/{owner}/{repo}` on `GLUE_GITHUB_API_URL`), and the selected provider's API key. It never prints secret values and exits non-zero when any check fails. When a job command cannot reach the resolved server, the error names the server, where it came from, and points to `glue job doctor`.

`glue job logs <id> [--tail N] [--json]` prints a job's history up to its current end and returns: it does not follow, does not prompt for permissions, and does not move the attach cursor, so it prints the same history every time. `--json` prints one event envelope per line. On AX, a suspended task woken to read it is suspended again. `glue job attach --from-start` replays from the first event, and `--tail N` starts N events before the end; attaching to a finished job with nothing new prints its summary. A job that was continued has several `job_done` events, and attach stops only at the last one.

### Latency

`scripts/ax-dev/e2e/latency.sh [--model M] [--thinking T]` runs one small job on the standing cluster and reports the submit stages, the time from submit to the job ID and to the job's first event, and the median and p90 model-turn time (from `model_ms` on each `turn_end` event, which the loop records for every model request including retries). It exits non-zero when a budget is missed (`SUBMIT_BUDGET_S=5`, `FIRST_EVENT_BUDGET_S=15`, `TURN_MEDIAN_BUDGET_S=5`). Measured on 2026-11-12 (AX `56bd0fc`, `kind-glue-ax`):

| Model / thinking | Submit → job ID | Median turn | p90 turn | One-line job |
|---|---|---|---|---|
| `gemini-3.8-flash` (default) | 13.2 s | 17.3 s | 30.5 s | 337 s |
| `gemini-3.8-flash`, `low` | 13.2 s | 14.1 s | 22.7 s | 254 s |
| `gemini-3-flash-preview`, `low` | 12.8 s | 1.0 s | 1.1 s | 22 s |

About 10.5 s of the submit time is AX building a golden snapshot for the task's own template (#591), and the default coding model is about 15 times slower per turn than `gemini-3-flash-preview` (#590).

`glue job list` prints an aligned table (ID, status, age, repo, objective) sized to the terminal; `--json` keeps the full records for scripts. `glue job show` adds an `activity` line: the current phase, the number of model turns so far, the last tool called, and how long ago the job last recorded anything. Usage in `show` updates after every model turn, not only at iteration boundaries (`jobs.Detail.Activity`). Every subcommand that takes a job ID also accepts a unique prefix, with or without `job-` (`glue job show 55f8`), or `latest` for the newest job.

`glue job say <id> "<text>"` records an `operator_message` and, while a maker is running, also queues the text on that maker's session (`Session.Steer`). The loop adds it as a user message before the maker's next model request, so the agent acts on it within one step; the job logs `operator_message_delivered` when that happens. The message also stays durable guidance for later iterations and the checker. A message sent while the planner or checker runs reaches the next maker iteration as guidance.

The default task image ships Go, Node.js with npm, Python with pip, venv and uv, a C/C++ compiler, ripgrep and jq, and AX tasks may reach the npm and PyPI registries by default, so JavaScript, TypeScript and Python repos can install dependencies in `--verify` commands or during the job. Other toolchains need a custom image (`--image`).

`--notify <url>` (repeatable; default `GLUE_NOTIFY_URL`) makes the job POST to each URL when it asks for a permission (`event: waiting_for_input`, with the question) and when it finishes (`event: done`, with status, summary and PR URL). The body is JSON with a one-line `text` field that Slack-style incoming webhooks render as is. Hosts starting with `ntfy.` get that line as plain text, with a `Title` header and the PR URL as `Click`, so `--notify https://ntfy.sh/<your-topic>` gives phone notifications with no other setup. The URLs are secrets: they travel with the in-memory runtime credentials (never in the task spec), are redacted from events and results, and their hosts are added to the task's egress allowlist. Each post is tried three times; a failure is logged as `notify_failed` and never fails the job. A credential push without URLs keeps the ones already set, but a task restart forgets them unless the resuming client also passes `--notify` or sets `GLUE_NOTIFY_URL`. `glue serve` hosts can set `GLUE_NOTIFY_URL` in their own environment.

Against a local `glue serve` daemon or optional host `glue-gateway`, `glue job` speaks `/v1/jobs` over HTTP using `--server`/`GLUE_SERVER` and `--token`/`GLUE_TOKEN`, `~/.config/glue/servers.json` (`0600`, written by `glue job login --server <url>`), or local daemon metadata. Subcommands: `login`, `doctor`, `submit` (`--select`, `--backend`, `--policy`, `--verify`, `--deliver`, `--attach`), `start`, `list`, `show`, `attach`, `logs [--tail N] [--json]`, `result [--json]`, `diff [--stat]`, `pull [--checkout|--apply] [--force]`, `approve`, `deny`, `say`, `continue`, `cancel`, `delete`, and `prune`.

`glue job attach <id>` (and `glue --remote ... --attach` / `glue job submit ... --attach`) replays events strictly after the last seen sequence cached in `~/.config/glue/jobs/<server-hash>/<id>.seq` (`0600`), prompts inline for pending permissions, detaches on Ctrl-C without canceling the server job, and reconnects with `Last-Event-ID` backoff when the stream drops before `job_done`. The transcript is plain text: every line starts with the time since the first event shown (`[mm:ss]`). It shows phase headers (`planning`, `plan`, `iteration N`, `checking`, `verdict`), one line per tool call with its key argument (path, command line, or pattern), outcome (`✓`/`✗`) and duration, the first 10 output lines of a failed call, streamed maker text, operator messages, guardrail, interruption, and suspend/resume events. At `job_done` it prints a summary block: status, iterations, duration, tokens, branch, PR URL, each verify command with its exit code, any delivery error, and the next commands. `--verbose` shows the full output of every tool call.

## Browser UI (`webui`) and Session Auth

`glue serve` mounts `webui.Handler()` at `/` and `/static/*` with `Content-Security-Policy: default-src 'self'`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, and `X-Frame-Options: DENY`. Browsers authenticate via `POST /v1/session/login` (`{"token": "<daemon-token>"}`), which sets an `HttpOnly; SameSite=Strict` cookie (`glue_session`, `Secure` over TLS). Authenticated routes accept either `Authorization: Bearer <token>` or the session cookie; state-changing cookie-authenticated requests require `X-Glue-CSRF` matching `csrf_token` from `GET /v1/session` (else `403 Forbidden`).

Opening `http://<listen-addr>/` in a browser renders three dependency-free ES-module views (`webui/static/index.html`, `api.js`, `app.js`, `app.css`):

- **Login view:** prompts for the daemon token, calls `POST /v1/session/login`, and fetches `csrf_token` from `GET /v1/session`. All state-changing browser requests (`POST` and `DELETE`) flow through `api.js` with `X-Glue-CSRF`.
- **Jobs list view (`#/`):** shows each job's status badge, pending permission count (`N pending`), truncated objective, repository, model, iteration (`iter X/Y`), and updated timestamp, with status filtering and 10-second auto-refresh.
- **Job detail view (`#/jobs/<id>`):** renders the job header (objective, status, iteration/max, model, repository, token usage, and a confirmation-gated **Cancel job** control calling `DELETE /v1/jobs/{id}`), a top pending-permissions banner, checklist with `[x]`/`[ ]` completion marks, and live SSE transcript over `EventSource(/v1/jobs/{id}/events)` (merged `text_delta` assistant blocks, collapsible tool calls with 4 KiB output truncation and "Show more", goal/lifecycle dividers, and interactive permission cards hydrated from both SSE and `GET /v1/jobs/{id}/permissions?state=pending` with **Allow once**, **Allow for session**, and **Deny** with optional reason) with `Last-Event-ID` reconnection and client-side `seq` deduplication. Active jobs show a **Send message** form (`POST /v1/jobs/{id}/messages`); terminal non-success jobs show **Continue with instructions** (`POST /v1/jobs/{id}/continue`, re-attaching the live event stream). Terminal jobs render a **Result** panel from `GET /v1/jobs/{id}/result` with final status, clickable PR link (`pr_url`), branch, `base_sha → head_sha`, diffstat, verification commands with exit code badges and expandable output tails, iterations, token usage, and estimated cost when pricing is configured. Responsive layout collapses to a single column below `700px`.
