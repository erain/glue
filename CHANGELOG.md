# Changelog

The library at `github.com/erain/glue` versions on a `1.x` line but
has **not** locked its API: minor bumps may still break API, exactly
as the pre-1.0 stance in
[`docs/adr/0013-pre-1-0-stability-stance.md`](docs/adr/0013-pre-1-0-stability-stance.md)
describes (its addendum records how the `1.x` line started). Pin a
tag in your `go.mod` if you need stability. Breaking changes always
land with a `**Breaking:**` entry under a minor-bump section — never
on a patch release.

This file tracks library-level changes. The maintained reference agent
[`agents/glue-review`](agents/glue-review/README.md) versions independently
(release notes in its [GitHub Releases](https://github.com/erain/glue/releases?q=agents%2Fglue-review)).
Peggy is retired; its [historical release notes](https://github.com/erain/glue/blob/6c502a4d69b680d05fd373d3809bcc6c47e1b2ed/agents/peggy/CHANGELOG.md) remain in Git history.

## Unreleased

- Repos can describe their remote jobs in `.glue/remote.yaml` (image, setup,
  verify, delivery, policy, extra egress hosts, CPU/memory, budgets); flags win
  over the file. New `--egress-host`, `--cpu` and `--memory` flags.

- **The default coding model is now `gemini-3-flash-preview` with thinking
  `low`** (was `gemini-3.8-flash`). Measured on remote jobs, it answers in about
  1 s per model turn against 14–17 s, so a one-line job takes 22 s instead of
  over 5 minutes. An unset `--thinking` on that model means `low`; pass
  `--model gemini-3.8-flash` to keep the previous model.

- Every `turn_end` loop event now records `model_ms`, the model request's
  duration. New `scripts/ax-dev/e2e/latency.sh` measures a remote job's submit
  stages and model-turn latency against budgets.

- New `glue job logs <id> [--tail N] [--json]` re-reads a job's history
  without following it or moving the attach cursor; `glue job attach` gains
  `--from-start` and `--tail N`, prints the summary for a finished job with
  nothing new, and no longer stops at the first `job_done` of a continued job.

- `glue job list` prints an aligned table, `glue job show` shows live activity
  (phase, model turns, last tool, time since the last event) with usage
  updated every model turn, and job ID arguments accept unique prefixes and
  `latest`. `show` for a finished, suspended AX job now reports its final
  iterations and tokens instead of zeros.

- `--notify <url>` (or `GLUE_NOTIFY_URL`) makes remote jobs POST a short
  notification when they wait for input or finish: JSON with a `text` line for
  Slack-style webhooks, plain text with a title for ntfy topics. URLs are kept
  in memory with the runtime credentials and redacted.

- The AX task image now includes Node.js 26 LTS with npm, uv, gcc/g++,
  ripgrep and jq, and AX tasks may reach `registry.npmjs.org`, `pypi.org` and
  `files.pythonhosted.org` by default, so JavaScript and Python repos work as
  remote jobs. New `scripts/ax-dev/e2e/image-smoke.sh` checks the image.

- `glue job say` now reaches a running maker at its next model turn instead of
  the next iteration, and the job logs `operator_message_delivered`. The loop
  gains `RunRequest.Steer` / `EventSteer`, and sessions gain `Session.Steer`
  for library users.

- `glue job attach` prints a readable transcript: elapsed-time prefixes,
  phase headers, one line per tool call with its key argument, outcome and
  duration, the first 10 lines of failed output, and a closing summary with
  status, duration, tokens, branch, PR and verify results. `--verbose` shows
  full tool output.

- New `glue job doctor [--json]` checks the remote target, AX connection,
  task image, repo, GitHub push access and provider key, and prints a fix for
  each failure. Job commands that cannot reach their server now say which
  server, where it came from, and point to `glue job doctor`; a missing AX
  task image gets its own error naming `--image` / `GLUE_AX_IMAGE`.

- `glue --remote` and `glue job submit` now report each AX submit stage with
  its elapsed time and end with a summary of the job's repo, model, policy,
  delivery, budgets and follow-up commands, all on stderr; stdout is still
  only the job ID. `--quiet` turns this off. `axbackend.Options.Progress`
  exposes the stages.

- Finished AX jobs no longer hold a worker in direct mode: `glue job attach`
  suspends the task after `job_done`, `glue job list`/`show` reconcile awake
  tasks whose job has finished, and `glue job cancel` suspends the task. New
  `glue job prune [--older-than 7d] [--dry-run]` deletes old finished jobs'
  AX resources while keeping their cached results.

- The daemon token file seeded into AX tasks now holds only `sha256:<hex>` of
  the per-job token, so an agent that reads `/workspace/.glue/token` can no
  longer use it to call the daemon (for example to approve its own permission
  requests). The daemon accepts a token of the form `sha256:<hex>`
  (`daemon.HashToken`) and verifies bearers against the digest. With a hashed
  token, browser session cookies and CSRF tokens use a random per-process key,
  so they cannot be derived from the hash; browser sessions then last until
  the daemon restarts.

- Jobs submitted directly to AX now default to a 1 hour wall clock and a
  2,000,000 token budget (`--no-budget` opts out); `glue job show` prints the
  effective budgets and `result.json` records which budget stopped a
  `budget_limited` job (`budget_exceeded`).

- Secret redaction no longer corrupts ordinary words that contain a token
  prefix (for example `ax-task-runner` was shown as `ax-ta[REDACTED]`):
  token shapes must start at a word boundary and be at least 20 characters
  after the prefix. GitHub `ghs_`, `ghu_` and `ghr_` tokens are now matched too.

- New `glue job pull <id> [--checkout|--apply] [--force]` brings a finished
  job's changes into the local checkout, as the `glue/<id>` branch (built from
  the job patch when the branch was never pushed) or as uncommitted changes.

- Remote work is no longer stranded in the sandbox: jobs submitted directly
  to AX default to `--deliver branch`, canceled or errored jobs commit and push
  their unfinished work (`partial: true` in the result), and the new
  `GET /v1/jobs/{id}/patch` endpoint and `glue job diff <id> [--stat]` show a
  job's changes. Direct AX mode caches the patch locally, so later `diff`
  calls do not wake the task. `glue job result` prints `delivery_error`.

- Goal checkers are limited to 12 tool turns and then return a verdict from
  the evidence they gathered, and a maker that runs out of turns hands over to
  the checker instead of failing. Jobs no longer end `errored` because the
  checker kept re-checking finished work.

- The goal planner is limited to 6 tool turns. When it runs out, or a loop
  guardrail stops it for repeating a call, Glue asks once more in a fresh
  session without tools, passing what the planner observed, instead of
  silently falling back to a one-item plan. `loop.ErrMaxTurns` is now an
  exported sentinel (the error message is unchanged).

- `glue --remote --snapshot` sends the working tree, including uncommitted and
  untracked changes and unpushed commits, to a remote job without touching the
  local index, branch or stash. The snapshot is pushed to
  `glue/snapshot-<job-id>`, recorded as `repo.snapshot_of`, and removed by
  `glue job delete`; draft PRs target the developer's branch.

- Remote jobs submitted from a local checkout now start from the current
  branch when `origin` has it, and refuse to submit when the remote clone
  would miss local work (unpushed branch or commits, uncommitted or untracked
  files). Pass `--base <ref>` or `--ignore-local` to proceed anyway.

- Add the remote mode Phase 3 design
  ([docs/remote-mode-phase3-design.md](docs/remote-mode-phase3-design.md)): a
  hands-on review of `glue --remote` on the standing AX cluster and a
  dependency-ordered work queue (milestones K–P) linked from the remote
  roadmap and project plan.

- Record [ADR-0020](docs/adr/0020-ax-first-direct-execution.md) (direct
  Laptop-to-AX execution via `internal/axbackend`, AX as sole external platform
  dependency with zero `glue-system` manifests, `auto` default job policy, and
  optional out-of-cluster `glue-gateway`) and align `README.md`,
  `docs/project-plan.md`, `docs/remote-mode-roadmap.md`,
  `docs/demo-ax-develops-ax.md`, `docs/jobs.md`, and
  `docs/runbooks/public-gateway.md` around the AX-first workflow.

- **Breaking:** Remove `glue-system` Kubernetes deployment manifests
  (`deploy/ax/gateway/`, `deploy/cloudflared/manifests.yaml`,
  `scripts/ax-dev/deploy-gateway.sh`, `make ax-dev-gateway`) and duplicate
  package `gateway/backend/ax`, refactoring the optional host/VM `glue-gateway`
  onto `internal/axbackend` with pure `type: ax` configuration (`atespace`,
  `image` defaulting via `axbackend.DefaultImage()`, `resources`, `egress_hosts`,
  optional `ax_server` / `kube_config` / `kube_context`; legacy `substrate`,
  `router`, `cert_file`, `ca_file`, and `token_file` YAML keys are removed) and
  updating `scripts/ax-dev/e2e/submit.sh` and `suspend-resume.sh` for direct CLI
  and out-of-cluster gateway verification.

- Enable direct Laptop-to-AX job execution in `cmd/glue` via `glue --remote "<objective>"`
  and `glue job <subcommand>` (`--ax`, `--kubeconfig`, `--context`, `--atespace`,
  `--image`, `--env`), with automatic git `origin` SSH-to-HTTPS URL normalization,
  default remote base branch detection, and in-memory credential pickup from env
  vars or `gh auth token` while preserving `--server` and local `glue serve` workflows.

- Add direct AX coding-job lifecycle and stateless task discovery in
  `internal/axbackend` (`Backend`, `NewBackend`, `DefaultImage`, `SubmitUnknown`)
  supporting ordered `Workspace` + `Task` provisioning, egress setup before
  `ResumeTask`, in-memory runtime credential push, per-cluster random HMAC task
  tokens (`glue-ax-task-hmac`), `ListJobs` / `Show` reconstruction from AX tasks
  without waking suspended sandboxes, and cached terminal `Result` retrieval
  with transparent resume and re-suspend.

- Add client-side AX auto-tunneling and short-lived router TLS / control-token
  caching in `internal/axbackend` (`Connect`, `ResolveKubeTarget`, `TunnelDir`,
  `StopTunnels`) so host tools connect directly to AX (`ax-server`,
  `atenet-router`, and internal control API) without modifying `~/.kube/config`
  or deploying an in-cluster gateway pod.

- Upgrade root `go.mod` to Go 1.27.1 (`github.com/google/ax` `@56bd0fc55574`
  and `go.ate.dev/substrate v1.0.0`) while keeping root `package glue` free of
  AX imports, and add `internal/axbackend` (`RouterTransport`, `DialAX`, and
  internal control dialing) for mTLS HTTP `CONNECT` task routing with dynamic
  TLS certificate refresh.

- Default Glue coding jobs (`jobs.BuiltInPolicy("")`) and `glue serve --policy`
  to the new `auto` permission profile (`Default: allow`, no `yolo` startup
  warning), support `"*"` in `AllowedBinaries` (`tools/shell`, `tools/coding`,
  `jobs`) so `auto` jobs can run arbitrary workspace binary basenames (`rg`,
  `gofmt`, `jq`, `sed`, `curl`, `gh`, `bash -c`) while preserving workspace
  path/symlink confinement, secret file blocklists, and secret redaction, and
  keep `workspace-auto`, `ask-all`, and `--policy-file` as opt-in human-approval
  profiles.

- Upgrade `scripts/ax-dev` (`kind-glue-ax`) to Agent Substrate `v1.0.0` GA
  (`b34fbee74290`) published release images and AX `56bd0fc55574`, add
  `scripts/ax-dev/env.sh` (`make ax-dev-env`) to export `KUBECONFIG`,
  `KUBE_CONTEXT`, `GLUE_AX_CONTEXT`, and `GLUE_TASK_IMAGE` from
  `~/.cache/glue/ax-dev` without modifying `~/.kube/config`, add `status.sh
  --quiet` (`make ax-dev-ready`), and ensure `smoke.sh` and `scripts/ax-dev/e2e/*`
  clean up per-test `Task`/`Workspace`/`EgressPolicy` resources while keeping
  `kind-glue-ax` running as a warm standing cluster.

- Add the end-to-end showcase runbook, recorded live AX execution (`job-628de8c215a8`
  opening draft PR `erain/glue-sandbox#5`), four recorded failure drills (verify
  failure `blocked`, phone cancel, 2-minute disconnect replay via `Last-Event-ID`,
  and mid-job `glue-gateway` restart recovery), and upstream AX feedback in
  `docs/demo-ax-develops-ax.md` with a "See it in action" link in `README.md`.

- Harden `glue-gateway` for public exposure behind Cloudflare Tunnel with HSTS
  (`Strict-Transport-Security`), configurable `trust_forwarded_proto` for
  `Secure` cookies and redirect URLs, `max_body_bytes` (default 1 MiB, returning
  `413 Request Entity Too Large`), `gateway.NewHTTPServer` timeouts
  (`ReadHeaderTimeout`, `ReadTimeout`, `IdleTimeout`), structured access logs
  that omit query strings, tokens, and cookies, an HTML `403 Access Denied` page
  for non-allowlisted browser OAuth sign-ins, `deploy/cloudflared/` Kubernetes
  and `systemd` manifests, and `docs/runbooks/public-gateway.md`.

- Add personal API tokens (`POST /v1/tokens`, `GET /v1/tokens`,
  `DELETE /v1/tokens/{id}`, stored as SHA-256 hashes in `api_tokens`), scope
  gateway jobs to their `owner` login (`TestJobsScopedToOwner`), and add GitHub
  sign-in and the `#/tokens` personal API token management view to `webui`.

- Add `auth.mode: github` OAuth sign-in (`/auth/github/login` and
  `/auth/github/callback`) with `auth.allowed_logins`, server-side SQLite
  sessions (`HttpOnly; Secure; SameSite=Lax`, 7-day absolute TTL and 24-hour
  idle TTL, `POST /v1/session/logout`), double-submit `X-Glue-CSRF` enforcement,
  auth endpoint rate limiting, `/healthz` and strict unauthenticated route
  allowlisting (`(*gateway.Server).Routes`), and refuse `dev-token` mode on
  `https://` `public_url` unless `--insecure-dev` is set.

- Push runtime credentials (`github_token` and `provider_keys`) from
  `glue-gateway` to AX tasks over `atenet-router` (`POST /v1/credentials`)
  before `POST /v1/jobs/{id}/start`, after each `ResumeTask`, and whenever
  `GET /v1/jobs/{id}` reports `credentials: "absent"`, and omit credential
  environment variables from AX `Task` specs (`gateway.Config.Credentials`,
  `(*ax.Backend).PushCredentials`, `(*ax.Backend).EnsureCredentials`).

- Add `POST /v1/credentials` on `glue serve` to hold `github_token` and
  `provider_keys` in memory only, wait in `waiting_for_credentials` (and report
  `credentials: "absent" | "present"` in `GET /v1/jobs/{id}`) until required
  credentials arrive, wire in-memory credentials into model providers, `draft_pr`
  delivery, and `--shell-env-secret-allow`, and redact known credential values
  and common token shapes (`ghp_…`, `github_pat_…`, `gho_…`, `AIza…`, `sk-…`)
  from events, tool outputs, sessions, and results (`jobs.Credentials`,
  `jobs.WaitingForCredentials`, `jobclient.Client.PushCredentials`).

- Persist job completion results in the `glue-gateway` SQLite registry before
  any AX task deletion, automatically delete terminal AX Tasks, Workspaces, and
  EgressPolicies after `retain_tasks` (default `168h` or `GLUE_RETAIN_TASKS`)
  while keeping the registry record (`task_deleted: true`), events, and result,
  add `glue job delete <id>` (`DELETE /v1/jobs/{id}?purge=true`) to delete the
  task and purge all gateway data for a job, and reconcile orphan `glue-*` AX
  tasks on gateway startup and via `GET /v1/diagnostics` without deleting them.

- Suspend AX tasks while jobs wait for human input (`suspend_after` default
  `2m`, or `GLUE_SUSPEND_AFTER`, when no live SSE viewer is attached) and when
  jobs finish (`suspend_on_finish` default `true`), serve read-only inspection
  views (`GET /v1/jobs`, `GET /v1/jobs/{id}`, `GET /v1/jobs/{id}/permissions`,
  `GET /v1/jobs/{id}/result`, and cached `/events`) from the gateway SQLite
  registry without waking suspended tasks, resume tasks on demand (`ResumeTask`
  + `WorkspaceReady`) with `Last-Event-ID` reconnect and re-issued permission ID
  remapping (`original_permission_id` + `permission_id`), surface the
  `"Suspended – waiting for you"` badge and resume notice in `webui`, and add
  `scripts/ax-dev/e2e/suspend-resume.sh`.

- Add `deploy/ax/gateway/Dockerfile` and `deploy/ax/gateway/manifests.yaml` for
  running `glue-gateway` in `glue-system` with projected Substrate pod
  certificates and `ate-token` volumes, plus `scripts/ax-dev/deploy-gateway.sh`
  and `scripts/ax-dev/e2e/submit.sh` for end-to-end AX job submission and draft
  PR delivery verification on kind.

- Add `gateway/backend/ax.Backend` submit/reconcile pipeline (`UpdateWorkspace`
  with `dir: "."`, `depth: 1` -> `CreateTask` -> `CreateActorEgressPolicy`
  before resume -> `ResumeTask` + poll `GetTask` until `Running` +
  `WorkspaceReady` -> `POST /v1/jobs/{id}/start` through `RouterTransport`),
  `submit_unknown` reconciliation via `GetTask`, and control-plane status reads
  that never touch task HTTP.

- Add `type: ax` gateway backend configuration (`atespace`, `image`,
  `resources`, `egress_hosts`, `ax_server`, `substrate`, `router`),
  `gateway/backend/ax.RouterTransport` (`http.RoundTripper` tunneling over HTTP
  `CONNECT` with `ate-target-actor: <atespace>/<task>` and mTLS pod identity),
  and control-plane gRPC client dialers (`DialAX`, `DialSubstrate`).

- Add the `glue-gateway` CLI entrypoint (`gateway/cmd/glue-gateway`), gateway
  configuration/architecture reference (`gateway/README.md`), `gateway.Backend`,
  `gateway.LocalBackend`, and `gateway.Server` with resilient upstream SSE event
  proxying, SQLite caching, and embedded `webui`.

- Add the nested `gateway` module (`Config` and SQLite `Registry`) plus
  caller-supplied job IDs (`jobs.Spec.ID`, `jobs.NewID`, `jobs.SummarizeEvents`).

- Add `scripts/ax-dev/manual-job.sh` (`make ax-dev-manual-job`) and
  `docs/runbooks/ax-manual-job.md` to run and verify a Glue coding job end to
  end inside an AX `Task`, configure system `core.askPass`
  (`/usr/local/bin/glue-git-askpass`) in `deploy/ax/task-image/Dockerfile` for
  private repository fetches during AX `SetupWorkspace`, and resolve
  `refs/remotes/origin/<ref>` in `internal/worktree.Prepare` for AX shallow
  detached-`HEAD` checkouts.

- Add the Glue AX task image (`deploy/ax/task-image/Dockerfile`,
  `scripts/ax-dev/build-task-image.sh`, `deploy/ax/README.md`) layering Go
  1.27.3 (SHA-256 verified), `git`, `make`, `ca-certificates`, `bash`, and
  `glue` on the digest-pinned `ax-task-runner` image, plus `glue serve
  --job-from-ax` and `--token-file` (`jobs.JobSpecFromAXTaskYAML`,
  `jobs.LoadAXTaskYAML`, `(*jobs.Manager).EnsureJobFromAX`) for idempotent,
  zero-outbound-call job creation on AX golden pre-boot and resume.

- Add reproducible local kind + Agent Substrate (`v1.0.0-rc.1`) + AX (`241bc4d`)
  development cluster scripts in `scripts/ax-dev/` (`versions.env`, `prereqs.sh`,
  `up.sh`, `status.sh`, `smoke.sh`, `down.sh`, `README.md`) and `Makefile`
  targets (`ax-dev-up`, `ax-dev-status`, `ax-dev-smoke`, `ax-dev-down`).

- Add a headless Chrome end-to-end browser test (`TestE2EJobBrowserWorkflow` in
  `webui/e2e_test.go`, gated by `GLUE_E2E_BROWSER=1` with strict `CI=true`
  precondition enforcement and failure screenshot/console artifact capture) plus
  a `browser` job in `.github/workflows/ci.yml` covering login, jobs list,
  transcript plan/tool rendering, permission approval, mid-job page reload
  sequence deduplication, operator steering messages, and completion result
  verification.

- Add browser job steering and result views in `webui/static` (`api.js`, `app.js`,
  `app.css`): CSRF-protected `POST`/`DELETE` helper, interactive permission cards
  (**Allow once**, **Allow for session**, **Deny** with reason) hydrated from SSE
  and `GET /v1/jobs/{id}/permissions?state=pending`, pending permission banner and
  list badge, **Send message** (`POST /v1/jobs/{id}/messages`) and **Continue with
  instructions** (`POST /v1/jobs/{id}/continue`), confirmation-gated job
  cancellation (`DELETE /v1/jobs/{id}`), and a terminal **Result** panel
  (`GET /v1/jobs/{id}/result`) showing status, PR link, branch, commits, diffstat,
  verification command exit badges/output tails, usage, and estimated cost.

- Add browser UI views in `webui/static` (`index.html`, `app.js`, `app.css`) for
  daemon token login, status-filtered jobs list with 10-second auto-refresh, and
  live SSE job detail transcripts (checklist, merged `text_delta` messages,
  collapsible tool calls with 4 KiB output truncation, goal dividers, highlighted
  permission cards, `Last-Event-ID` reconnect, and `seq` deduplication).

- Add `webui` (`webui.Handler()`) serving `/` and `/static/*` with strict
  browser security headers (`Content-Security-Policy: default-src 'self'`,
  `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`,
  `X-Frame-Options: DENY`), plus `POST /v1/session/login` (`HttpOnly; SameSite=Strict`
  cookie) and `GET /v1/session` (`X-Glue-CSRF` double-submit protection on
  state-changing cookie requests) in `daemon`.

- Add `glue job attach <id>` and `glue job submit --attach` with per-server
  sequence cursor replay (`~/.config/glue/jobs/<server-hash>/<id>.seq`), inline
  permission prompts, Ctrl-C detach without cancelling the job, and automatic
  `Last-Event-ID` reconnect backoff.

- Add `internal/jobclient` and the `glue job` control and inspection CLI
  (`login`, `submit` with `--select`, `start`, `list`, `show`, `result`,
  `approve`, `deny`, `say`, `continue`, `cancel`), storing server tokens in
  `~/.config/glue/servers.json` (`0600`).

- Deliver finished job branches (`delivery: "branch" | "draft_pr"`) to `origin`
  via a hidden `glue git-askpass` helper backed by `GLUE_GITHUB_TOKEN` (never
  exposed to the coding shell or persisted to disk/events), and open fork-aware
  draft pull requests over the GitHub REST API (`GLUE_GITHUB_API_URL`) for
  achieved `draft_pr` jobs while recording `pr_url` and `delivery_error` in
  `result.json`.

- Run post-goal `verify` commands in the job environment, commit workspace edits
  on `glue/<job-id>` with `GLUE_GIT_AUTHOR_NAME`/`GLUE_GIT_AUTHOR_EMAIL` (or
  repository config fallback), persist `<state>/jobs/<id>/result.json`, and
  serve completion records at `GET /v1/jobs/{id}/result`.

- Give coding and job shells a safe default toolchain environment
  (`--shell-env-pass`, `--shell-env`, `--shell-env-secret-allow`), filter
  secret-shaped variables by default, create `<state>/home/<job-id>` when
  `HOME` is unset, spill truncated outputs under `<state>/tmp`, and raise
  the default coding/job `shell_exec` timeout to `10m` (`--shell-timeout`)
  with per-call timeout overrides capped at the maximum.

- Enforce permission policies across jobs, daemon runs (`daemon.NewPermissionPolicy`),
  and `glue serve` (`--policy` / `--policy-file`), default jobs to `workspace-auto`,
  and emit `permission_auto` audit events on automatic `allow`/`deny` decisions.

- Add `jobs.Policy`, built-in permission profiles (`ask-all`, `workspace-auto`,
  `yolo`), strict JSON policy file validation, and workspace-confined rule
  evaluation.

- Resume active jobs after a process restart, expire abandoned permissions, and
  retain iteration, token, and wall-clock limits across recovery.

- Persist job preparation intent before repository work, require goal checkpoints,
  and recover validated checkouts without recreating branches or losing edits.

- Let restartable goal hosts require checkpoints before execution, save initial
  goal records, and preserve iteration and usage counters on recovery.

- Continue blocked, iteration-limited, budget-limited, or interrupted jobs with
  new guidance, preserving their checklist, branch, and isolated workspace.

- Queue durable operator messages over the job API and show them to the next
  maker and checker, with delivery events and guidance preserved across restarts.

- Let goal hosts deliver operator guidance to the next maker iteration and
  its independent checker through an optional safe-point callback.

- Prevent duplicate job execution by locking each manager state directory until
  its workers and storage close; a killed process releases ownership automatically.

- Host durable jobs with `glue serve --jobs` (on by default with --coding),
  including lazy provider startup, isolated execution and lifecycle cleanup.

- Expose authenticated job lifecycle, permission, inspection, and resumable
  event endpoints through the daemon when a job manager is configured.

- Expose authenticated model choices and activity defaults, and share
  resumable SSE handling between run and job streams.

- Inspect durable job progress and permission history, filter job lists,
  and cancel unstarted jobs without preparing a workspace.

- Track coding jobs waiting on permission, persist decisions before tools
  resume, and enforce wall-clock budgets with durable terminal outcomes.

- Start isolated coding jobs, retain goal/session/tool events, and persist
  terminal outcomes with cancellation and shutdown interruption.

- Persist validated coding job specs and creation events without starting
  execution; inspect them after manager restart.

- Share durable event storage between daemon and job hosts while preserving
  daemon registry/permission transactions and resumable replay.

- Share isolated workspace preparation between CLI goals and job hosts,
  including requested base revisions and URL clones with recorded base SHAs.

- Observe goal planner, maker, and checker sessions before prompts through
  `GoalSpec.OnSession`, including phase and iteration for event transcripts.

- Retain terminal daemon runs for 720 hours by default (`--retain-runs`),
  delete associated history atomically, and record shutdown interruptions.

- Persist daemon permission requests and decisions, expose their history,
  and expire interrupted-run requests after restart. Late decisions return 409.

- Inspect persisted daemon runs through list/get APIs; restarted daemons
  report interrupted prompts with a terminal replay event.

- **Breaking:** daemon protocol v2 persists events, resumes streams by sequence,
  and keeps runs alive after disconnect. `glue serve --state-dir` controls storage.

- CLI chat defaults to OpenRouter free; coding and goals default to Gemini 3.8 Flash.

- **Remote mode is planned (`docs/remote-mode-roadmap.md`, ADR-0019).**
  Fire a coding job, disconnect, and pick it up from any terminal or a
  browser, with jobs running locally or as AX tasks. The plan is a tracker
  (#402) with one verifiable issue per step; nothing is implemented yet.
- **Gemini ids with a `models/` prefix get Gemini 3/4 handling
  (`providers/gemini`, `providers`).** Ids copied from ListModels, such as
  `models/gemini-3.1-pro-preview`, again receive thought-signature replay,
  includeThoughts, and `--thinking` validation. `gemini-3-pro-preview`,
  which the API no longer serves, is removed from the model catalog.
- **Live tests run on OpenRouter's `openrouter/free`.** The glue-review
  smoke and fixture replay and the local-agent example now need only
  `OPENROUTER_API_KEY`, and CI's always-on live job is `live (openrouter)`.
  The `live (nvidia)` job is removed; the NVIDIA adapter smoke is opt-in
  via `NVIDIA_LIVE_MODEL`. Retired defaults are replaced: glue-review's
  Gemini model is `gemini-3-flash-preview`, and the local-agent example
  uses the Gemini provider default. (#395, #390)
- **Breaking:** Removed the retired `agents/peggy` personal-assistant product,
  including its CLI, Telegram adapter, dashboard, and scheduling. Shared Glue
  framework, daemon, stores, providers, and coding tools remain supported.
  Current direction: framework and coding agent, with local execution today
  and AX-backed remote execution in planning.

- **TUI: `/copy [N]` + Ctrl+Y copy an assistant reply's raw markdown to
  the system clipboard (`cmd/glue/tui`).** Selecting a reply out of the
  terminal dragged in glamour styling (ANSI, box-drawing, wrapped lines),
  so the paste was mangled. The transcript already keeps each reply's raw
  markdown separate from its rendered form; `/copy` now sends that clean
  text to the clipboard via an OSC 52 sequence — works locally and over
  SSH with no external binary. Defaults to the last reply; `/copy 2`
  targets reply #2; **Ctrl+Y** copies the last reply with no typing. (#362)
- **Stale-file guard for `edit_file` (`tools/fs.ReadLedger`).** The
  roadmap's #1 cause of edit-match failures: the file changed on disk
  (user edit, formatter, git operation) after the model read it, and
  the edit then mismatched or clobbered the newer content. The coding
  bundle now shares a read-ledger: `read_file` records each file's
  mtime, `edit_file` refuses to edit a file whose mtime moved since —
  with a "re-read the file" instruction — and both mutators refresh
  the ledger after their own writes so harness edits never
  false-alarm. Files never read are not checked; a nil ledger keeps
  the old behavior everywhere else. (#368)

- **TUI usage display actually works, and can show dollars
  (`tui.Pricing`).** The status bar's token segment and `/usage` read
  a field no code ever populated — they were dead since they shipped.
  Turns now report their summed assistant-message usage; the status
  bar shows the last turn's `N in / N out`, `/usage` adds session
  totals, and both append a running cost estimate when the
  `--usage-*-price` flags are set. Session totals reset with
  `/clear`, `/resume`, and session switches. (#370)

- **AGENTS.md is now a chain from repo root to workdir
  (`LoadContext`).** Previously only `<workDir>/AGENTS.md` loaded;
  in a monorepo, running from a subdirectory lost the repo-wide
  instructions (or vice versa). Every AGENTS.md from the repository
  root (nearest ancestor with `.git`) down to the workdir is
  concatenated root-first, so nearer files read as refinements.
  Outside a repository the old single-file behavior is unchanged.
  (#369)

- **Actionable model-unavailable errors
  (`providers.ModelUnavailableHint`, `Factory.ModelsListURL`).** A 404
  / model-not-found failure previously surfaced as a raw provider
  error with no recovery path (live case: NVIDIA's catalog lists
  `moonshotai/kimi-k2.6` but some accounts 404 on it). One-shot runs
  and the TUI now append which model was requested, that the account
  may lack access, and how to pick another — including the provider's
  model-catalog URL declared in the registry (gemini, nvidia,
  openrouter). The TUI hint points at `/model <id>`. (#367)

- **`glue run --continue` reopens the most recently updated session.**
  Resume previously existed only inside the TUI (`/resume` picker);
  from the CLI you had to remember the `--id` you used. `--continue`
  asks the store for the newest session, prints which one it picked
  (id, message count, last-updated), and works for both the TUI and
  one-shot `--prompt` runs. Mutually exclusive with `--id`. (#366)

- **`find_files`, `grep`, and the TUI `@`-picker respect `.gitignore`
  (`tools/fs.IgnoreMatcher`).** The recursive walkers previously
  skipped only `.git`, so real projects burned tokens walking
  `node_modules` and build output. A dependency-free matcher covers
  the gitignore core — negation with last-match-wins, `/` anchoring,
  trailing-`/` dir-only patterns, `*`/`?`/`[...]`/`**`, nested
  `.gitignore` files — and ignored directories prune the whole
  subtree (git semantics: no re-inclusion below an excluded dir).
  Coding-bundle tools set `NavOptions.RespectGitignore`; per-call
  `include_ignored: true` reaches ignored paths when needed. (#365)

- **Refreshable environment context block for coding runs
  (`AgentOptions.EnvContext`, `tools/coding.EnvContext`).** Coding
  sessions now carry a tagged `<environment_context>` user message —
  cwd, OS, date, git branch/dirty-state/head — injected before the
  first prompt and re-injected only when it changes, so the transcript
  prefix (and provider prompt caching) stays intact. The model stops
  spending its opening turns on `pwd`/`git status`. Wired into
  `glue run --coding` and `glue goal --coding`; the TUI's `/resume`
  replay filters the blocks out of the visible transcript. (#364)

- **TUI: `/` picker shows every command; `@` picker gains scroll
  indicators (`cmd/glue/tui`).** The slash-command popup previously
  reused the file picker's 8-row scroll window with no indicator, so a
  bare `/` silently hid 5 of the 13 commands. It now lists the whole
  (bounded) command set, and the `@`-file picker — which keeps its
  window because workspaces are unbounded — shows `↑/↓ N more` lines
  when matches are clipped. (#356)

## 1.13.0 — 2026-06-09

- **Per-model capability registry + tool-owned prompt assembly
  (`providers`, `loop`, `tools/*`, `cmd/glue`).** The providers
  registry now carries declarative `Capabilities` per provider
  (context window, parallel-tool safety, prompt variant, auto-continue
  proneness; `providers.CapabilitiesFor(name)`), replacing
  if-provider-name switches — the `glue` binary's Gemini auto-continue
  gating now reads the registry. Tools own their prompt text: the new
  `ToolSpec.PromptSnippet` / `PromptGuidelines` fields (set across
  `tools/fs`, `tools/shell`, `tools/git`) feed
  `coding.SystemPrompt(tools, variant)`, which assembles a coding
  system prompt from the active toolset — one line per tool plus
  deduplicated guidelines, in a terse variant for frontier models
  (gemini, codex) and an explicit variant for open-weight models. The
  `glue` binary previously ran `--coding` with **no** system prompt;
  it now gets the assembled one, and the prompt can never drift from
  the registered toolset (snapshot-tested)
  ([docs/coding-harness-roadmap.md](docs/coding-harness-roadmap.md)
  P1.5). Closes #345.

- **Loop guardrails (`loop`).** Two graduated detectors now watch every
  tool round, on by default (`RunRequest.Guardrails`, zero value =
  defaults; `Disabled` opts out): repeating the **same tool call with
  identical arguments** draws a corrective user message at 3
  consecutive occurrences and ends the run with the typed
  `ErrRepeatedToolCalls` at 5; **consecutive all-error tool rounds**
  draw a corrective message at 3 and end the run with
  `ErrTooManyMistakes` at 6. Streaks reset on any change of arguments
  or any successful result; injected messages are marked
  `glue/guardrail` in metadata and `EventGuardrail` reports
  kind/count/action. These are the failure shapes that waste tokens
  fastest on open-weight models — and unattended `glue goal` runs
  ([docs/coding-harness-roadmap.md](docs/coding-harness-roadmap.md)
  P2.8). Closes #344.

- **Next-speaker stall recovery (`loop`, `glue`, `cmd/glue`).** Gemini's
  classic stall — narrating "I will now update the file." and stopping
  without calling a tool — is now recovered: with the new opt-in
  `RunRequest.AutoContinue` / `AgentOptions.AutoContinue`, an assistant
  turn whose closing sentence announces a future action (and asks no
  question) gets a synthetic "Please continue." user message (marked
  `glue/auto-continue`, at most twice per run, `EventAutoContinue`
  emitted). The `glue` binary enables it automatically for the gemini
  provider when tools are registered. The surrogate-sanitization item
  from the roadmap was verified unnecessary back in #313 (Go's
  `encoding/json` already sanitizes invalid UTF-8)
  ([docs/coding-harness-roadmap.md](docs/coding-harness-roadmap.md)
  P2.7). Closes #343.

- **Compaction upgrade (`glue.SummarizingCompactor`).** The default
  summarization prompt now requests a structured state snapshot (Goal /
  Constraints / Progress / Key Decisions / Next Steps / Critical
  Context, exact paths and error messages preserved verbatim) with a
  prompt-injection firewall, and instructs the summarizer to integrate
  any previous snapshot instead of re-summarizing it. The snapshot is
  framed as a handoff from another assistant instance (reduces re-doing
  finished work). New: a cumulative **file ledger** — read/modified
  paths extracted from coding-tool calls, merged across compactions and
  carried in marker metadata; **safe split points** that never orphan a
  tool-call/result pair; opt-in `KeepRecentTokens` to select the
  verbatim tail by token budget instead of message count; and an
  **inflation guard** that refuses a "summary" bigger than the region
  it replaces
  ([docs/coding-harness-roadmap.md](docs/coding-harness-roadmap.md)
  P1.6). Closes #342.

- **Retry/overflow recovery (`loop`, `glue`)
  ([ADR-0017](docs/adr/0017-loop-retry-overflow-recovery.md)).**
  Transient provider failures (429/5xx, rate limits, dropped or
  never-finished streams) are now retried at the loop level with
  exponential backoff (3 retries, 2s base, 30s cap), honoring
  server-provided `Retry-After` / Gemini `RetryInfo.retryDelay` hints;
  the new `EventRetry` event reports each attempt. Auth/billing/
  invalid-request errors still fail fast, and context-window overflow
  surfaces as the new typed `*loop.OverflowError` — which
  `Session.Prompt` catches, compacts once (when a `Compactor` is
  configured), and retries once. Retries never duplicate history:
  nothing is appended until an attempt succeeds. Opt out with the new
  `RunRequest.Retry: loop.RetryPolicy{Disabled: true}`
  ([docs/coding-harness-roadmap.md](docs/coding-harness-roadmap.md)
  P1.4). Closes #341.

- **History hardening (`loop`).** Every `loop.Run` now repairs the
  transcript before anything reaches the provider, via the new exported
  `loop.HardenHistory`: assistant tool calls whose result is missing
  (Esc-cancel, crash, resumed/forked session) get a synthesized
  `IsError` result marked `glue/synthetic`; orphaned tool results and
  empty assistant turns are dropped; tool-call IDs are normalized to
  `[A-Za-z0-9_-]{1,64}` consistently across call and result; and turns
  produced by a different model lose their thinking blocks and
  provider-specific signatures. Both the Gemini and OpenAI-compatible
  APIs reject these malformed transcripts with opaque 400s
  ([docs/coding-harness-roadmap.md](docs/coding-harness-roadmap.md)
  P0.3). Closes #340.

- **Structured output truncation (`glue`, `tools/shell`, `tools/fs`).**
  `shell_exec` output is now captured head+tail instead of head-only —
  a long build log keeps both the first error and the final status,
  with an inline `[... N bytes (~M lines) omitted ...]` marker, a total
  line count, and (in the coding bundle) the complete stream spooled to
  a temp file named in the result so the model can read back what was
  dropped. Timeouts now prepend "command timed out after Ns; partial
  output below" to the kept output instead of replacing it.
  `read_file` gains `offset` / `max_lines` paging with dual line+byte
  caps; truncated reads end with `[Showing lines X-Y of Z. Use
  offset=N to continue.]`, oversized single lines and >20MB files get
  explicit shell-tool escape hatches. `ExecResult` gains additive
  `StdoutTail`/`StderrTail`, `*Omitted`, `*Lines`, and `*Spool` fields;
  `ExecCommand.SpoolDir` opts executors into spooling
  ([docs/coding-harness-roadmap.md](docs/coding-harness-roadmap.md)
  P0.2). Closes #339.

- **`edit_file` repair ladder (`tools/fs`).** `old_string` no longer has
  to match byte-for-byte: a deterministic cascade absorbs the drift
  models introduce — trailing-whitespace differences, indentation drift
  (the replacement is re-indented to the file's real indentation, with
  per-level tab/space mapping), smart-quote/Unicode-dash/exotic-space
  folding, block-anchor matching for ≥3-line blocks with a misquoted
  middle, and over-escape repair (literal `\n` sequences from
  double-escaped JSON). CRLF line endings and a UTF-8 BOM are preserved
  on write. Non-exact matches are reported in the result; success now
  echoes the updated lines (so the model needn't re-read the file);
  failures echo what was searched for with concrete advice; and
  "rest of code unchanged"-style placeholder replacements are rejected.
  Source-verified port of the consensus technique across pi, Cline,
  Codex CLI, and Gemini CLI
  ([docs/coding-harness-roadmap.md](docs/coding-harness-roadmap.md) P0.1).
  Closes #338.

## 1.12.0 — 2026-06-09

- **Headless `glue goal` subcommand — scheduled/CI goal runs (goal loop
  Phase 3c).** `glue goal "<objective>"` runs a full plan→make→verify loop
  without a TUI, streaming checklist-level progress to stdout and exiting
  with a status-mapped code (0 achieved · 2 blocked · 3 max-iterations ·
  4 budget-limited · 1 errored) so cron, CI, or a peggy schedule can branch
  on the outcome. Shares `glue run`'s infrastructure flags plus
  `--max-iterations`, `--budget`, and `--worktree` (same `.glue/worktrees/`
  + `goal/<id>` isolation as the TUI's `/goal -w`); `--yolo` for unattended
  runs. `glue goal --list` prints stored records and `glue goal --resume
  [id]` continues from the verified checklist — the store is shared with the
  TUI, so a goal started in `glue run` can be resumed headlessly and vice
  versa. The worktree helper moved to a shared `cmd/glue/worktree` package.
  Closes #328.
- **Goal worktree isolation — `/goal -w` (goal loop Phase 3b).**
  `/goal -w <objective>` (or `--worktree`) runs the goal's maker and checker
  in a dedicated git worktree at `.glue/worktrees/<goal-id>` on branch
  `goal/<id>`, leaving your checkout untouched; the finished goal names the
  branch to review and merge (never auto-merged). The run's coding tool set
  is rebuilt rooted at the worktree via the new `tui.Config.BuildTools`
  factory (wired from `glue run --coding` with the same `--allow-binary` /
  `--coding-allow-overwrite` / `--tools` / `--no-tools` flags), and the new
  `GoalSpec.WorkDir` / `GoalRecord.WorkDir` (`glue/goal:workdir`) field makes
  resume — in-process or across restarts — re-attach the same worktree and
  branch. Without `--coding` or outside a git repository, `-w` refuses with a
  clear message before any goal starts. Closes #326.
- **Durable goal state + resume across restarts (goal loop Phase 3a).**
  When the agent has a `Store`, `Agent.PursueGoal` checkpoints a `GoalRecord`
  (objective, status, verified checklist, iterations, usage, summary) as
  namespaced `glue/goal:*` metadata on the `SessionPrefix` session — after
  planning, after every checker verdict, and at the terminal state. A context
  cancellation persists as the new `GoalPaused` status (resumable); the new
  `GoalRunning` status marks in-flight records. `Agent.LoadGoal` and
  `Agent.ListGoals` (most recent first, via the optional `SessionLister`
  capability) read records back, and `GoalRecord.Resumable()` says whether a
  goal can continue from its checklist. `GoalSpec.StartIteration` lets a
  resumed run continue iteration numbering (`iter-4`, `check-4`, …) so
  maker/checker sessions stay fresh instead of appending to the previous
  run's transcripts. In the TUI, `/goal resume` with no in-process goal now
  continues the most recent unfinished record from the store — a goal
  survives quitting `glue run` — and the new `/goal list` shows recent goals
  with status, checklist fraction, and age. Checkpointing is best-effort and
  storeless agents are unaffected. Closes #324.

## 1.11.0 — 2026-06-09

- **TUI `/goal`: drive the goal loop from inside `glue run` (Phase 2).**
  `/goal <objective>` runs `Agent.PursueGoal` in the background on its own
  session ids while the chat stays usable: a live goal card in the transcript
  updates in place per iteration (verified `[x]/[ ]` checklist with evidence,
  iteration counter, token usage, last checker verdict), and the status bar
  gains a `◎ goal · iter 2/10 · 1/4 ✓ · 12.3k tok` segment. Subcommands:
  `/goal status`, `/goal pause` (cancels cleanly, keeps the verified
  checklist), `/goal resume` (continues from that checklist without
  re-planning), `/goal clear`. Goal tool calls flow through the same in-card
  permission prompt as chat turns — pending permission requests are now a FIFO
  queue, so a concurrent goal + chat request can no longer drop one (`--yolo`
  makes goals fully autonomous). Closes #322.
- **`GoalSpec.Checklist`: seed `Agent.PursueGoal` with an existing plan.**
  When non-empty, the planning step is skipped and the loop continues from the
  given items (done flags and evidence respected) — the primitive behind
  `/goal resume`, and the hook Phase 3's durable resume will reuse.
- **`GoalSpec.Permission` now applies to the planner and checker sessions too**
  (previously maker-only). Without this, a checker under an agent with no
  default permission policy — e.g. the interactive TUI — was denied every
  side-effecting tool and could not run builds or tests to verify evidence.

## 1.10.0 — 2026-06-09

- **Goal loop: `Agent.PursueGoal` — "loop engineering" / `/goal` (Phase 1).**
  A new library primitive that turns one persistent objective into an
  autonomous loop: a planner decomposes the goal into a verifiable checklist,
  then each iteration runs a **maker** (a fresh session seeded from the
  checklist — a Ralph-style loop, so memory lives in durable state, not a
  growing transcript) followed by a separate **checker** session that audits
  against real evidence (`Session.PromptJSON`) and decides completion — the
  writer never grades its own homework. Bounded by `GoalSpec` guardrails
  (`MaxIterations`, `NoProgressLimit`, `TokenBudget`) and observable via
  `GoalSpec.Emit`. Headless primitive only; the TUI `/goal` command and durable
  resume are follow-ups. Designed in
  [ADR-0016](docs/adr/0016-goal-loop.md). Closes #320.

## 1.9.0 — 2026-06-09

- **TUI: inline autocomplete for `/` slash commands (`cmd/glue/tui`).**
  Typing `/` now opens a filtering command popup (mirroring the `@`-file
  picker): `↑/↓` navigate, `Tab` completes, `Esc` closes (keeping what you
  typed), and `Enter` runs a fully-typed or no-argument command while
  completing an argument-taking one (e.g. `/model `). The command list is now
  a single source of truth shared by the picker and `/help`, so they can't
  drift. Closes #318.

## 1.8.0 — 2026-06-08

- **Fix Gemini 3.x tool calls: round-trip `thoughtSignature` (`providers/gemini`).**
  Gemini 3.x (incl. the default `gemini-3.1-pro-preview`) returns an opaque
  `thoughtSignature` on the parts it produces and **requires** it echoed back
  on replay; without it the second turn of any tool-using conversation failed
  with `400 … Function call is missing a thought_signature in functionCall
  parts`. The provider now captures the signature from function-call and
  thought parts (stored base64 on `ContentPart.Signature`) and replays it
  verbatim, sends prior thinking back as real `thought` parts, and enables
  `includeThoughts` on Gemini 3.x so reasoning streams through. Verified live
  with a two-turn tool loop. (Also points the gated live smoke test at the
  provider default; it hardcoded the now-removed `gemini-2.5-flash`.)

- **Gemini: synthetic `thoughtSignature` fallback for unsigned replays
  (`providers/gemini`).** When an active-loop model turn reaches Gemini 3.x
  without a real signature on its first function call — compacted history, a
  transcript written before signature round-tripping landed, or a turn that
  genuinely arrived unsigned — the provider now injects the sentinel
  `skip_thought_signature_validator` (the same value Google's gemini-cli uses)
  so the request still validates. Scoped to the active loop (everything after
  the most recent genuine user turn) and to Gemini 3.x ids; real signatures are
  never overwritten. Verified live.

- **Gemini: `GOOGLE_GENAI_API_VERSION` env knob (`providers/gemini`).** Pins
  the API version the client targets (e.g. `v1alpha`/`v1beta`) so users can
  reach version-gated preview features without a code change, matching
  gemini-cli. Unset leaves the SDK default in place.

## 1.7.0 — 2026-06-08

- **Fix Gemini default id: `gemini-3.1-pro-preview`, not `gemini-3.1-pro`.**
  v1.4.0 set the default to `gemini-3.1-pro`, which 404s — the public
  id on `generativelanguage.googleapis.com` v1beta carries the
  `-preview` suffix. Verified against `ListModels`. Override paths
  remain unchanged: `--model`, `GLUE_MODEL`, or
  `gemini.Options.DefaultModel`.

## 1.6.0 — 2026-06-08

- **TUI: cap transcript at 100 cols, center on wide terminals
  (`cmd/glue/tui`).** The viewport used to stretch to the full terminal
  width, so on a 200-col terminal the assistant text wrapped at the
  right edge and read as a wall. Now `bodyMaxWidth = 100` (locked to
  the input box width) caps the conversation column and `View()`
  centers it inside `m.width` via `lipgloss.PlaceHorizontal`. Glamour
  markdown renderer width inherits the cap so wrapping aligns. Header
  and status bar stay full-width — they read better edge-to-edge.
  Closes #307.
- **`glue version` / `glue --version` / `glue -v`.** Prints the module
  version, git revision, build time, and Go toolchain from the linker-
  embedded build info (same data as `go version -m $(which glue)`),
  reachable as a first-class subcommand so users can self-diagnose a
  stale binary without a side command.

## 1.5.0 — 2026-06-08

- **Catppuccin TUI theme (`cmd/glue/tui`).** Replaced the slate +
  Tailwind-primary palette with Catppuccin Mocha (dark) and Latte
  (light), picked at construction time by lipgloss's terminal-
  background heuristic. The accent role moves from #6d28d9 to mauve
  (#cba6f7 Mocha / #8839ef Latte) — still distinctly purple, but a
  pastel hue family where success/error/warn (green/red/peach) sit at
  similar lightness/saturation so the chrome no longer shouts. Glamour
  markdown output now ships under matching JSON style configs
  (`glamour-mocha.json`, `glamour-latte.json`) with chroma syntax
  highlighting via `catppuccin-mocha` / `catppuccin-latte`. All
  variable names in `cmd/glue/tui/styles.go` are unchanged; downstream
  callers (if any) keep compiling. Closes #305.

## 1.4.0 — 2026-06-08

- **Default Gemini model is now `gemini-3.1-pro`** (was `gemini-2.5-flash`).
  `gemini-2.5-flash` was removed from the v1beta `generateContent`
  API and began returning 404 for users on the current key surface.
  `gemini-3.1-pro` is the unmetered daily-driver target for this
  project. Override at any time with `--model <id>` or `GLUE_MODEL`.
  Library callers can still pass any model id through
  `loop.ProviderRequest.Model` or `gemini.Options.DefaultModel`.

## 1.3.0 — 2026-06-08

- Added `cmd/glue` interactive TUI. `glue run` with no `--prompt` and a
  terminal on stdin+stdout now opens a bubbletea-based interactive
  coding agent: scrollable transcript, multi-line input, streaming
  text, tool-call cards with inline permission prompts, and a small
  `edit_file` diff preview. Slash commands: `/help`, `/exit`,
  `/clear`, `/usage`, `/tools`, `/model <id>`, `/session [id]`. Ctrl+C
  cancels the current turn on first press, quits on second. Designed
  in [ADR-0014](docs/adr/0014-coding-agent-tui.md). Scripted and
  one-shot paths (`glue run --prompt ...`, `echo ... | glue run`) are
  preserved exactly. The new charmbracelet dependencies live under
  `cmd/glue/tui/`; the library import graph is unchanged.
- **Session tree (`glue` + `cmd/glue/tui`).** New `Agent.ForkSession`
  and `Agent.CloneSession` write child sessions whose metadata records
  their place in the lineage; `SessionParent` reads it back. New typed
  `ErrSessionNotFound`. Metadata keys are namespaced under
  `glue/tree:`. The TUI gains three slash commands: `/fork [N]`
  (defaults to "branch from just before my last user message"),
  `/clone`, and `/tree` (modal lineage view with `↑/↓` navigate, Enter
  switch, Esc cancel; current node marked `◉`, others `●`, non-root
  nodes tagged `forked@N`). Designed in
  [ADR-0015](docs/adr/0015-session-tree.md). Additive only — no
  existing API or on-disk format changes.
- **Daily-driver workflow polish (`cmd/glue` + TUI):**
  - **`--yolo` flag** on `glue run`: auto-allows every side-effecting
    tool call (`write_file` / `edit_file` / `shell_exec` / MCP) without
    surfacing a permission prompt. Implies `--coding-allow-overwrite`.
    Stderr banner at startup and a yellow `⚠ --yolo enabled` row in the
    TUI welcome card + a `yolo` chip in the status bar so it's never
    invisible. Use on a feature branch.
  - **`@` autocomplete file picker** in the TUI input. Type `@` after
    whitespace to open an inline rounded-purple popup of workspace
    files (walked once, capped at 5000, `.git` / `node_modules` /
    secret-shaped paths skipped). Type more chars to fuzzy-filter
    (case-insensitive, basename-start ranked first); `↑/↓` navigate;
    `Tab` / `Enter` insert the path (the textarea ends up with the full
    `@<path> `); `Esc` removes the `@<query>` and closes.
  - **`@file` argument expansion.** In `glue run --prompt`, in piped
    stdin, and in the interactive TUI's submit handler, `@<path>`
    inlines the file contents under a `--- @path ---` header so the
    model sees the actual code. Supports `@"path with space"` and
    `@@literal` escape. Workspace-rooted and refuses path-escape,
    secret-shaped (`.env`), symlinks, directories, and oversized
    files. Lives in the new `cmd/glue/atmentions` package.
  - **`--tools <list>` and `--no-tools` flags on `glue run`.** Filter
    the registered tool set: `--tools read_file,grep` restricts the
    model to that allowlist; `--no-tools` strips them all for a
    text-only run. Unknown names error with the list of available
    tools (no silent typos).
  - **`--mode json` output mode for one-shot runs.** Emits stable
    JSON-Lines events on stdout (`start`, `text`, `tool_start`,
    `tool_end`, `done`, `error`) for scripting and IDE integration.
    The interactive TUI ignores `--mode`; piping or `--prompt` enables
    it.
  - **`/compact` slash command in the TUI.** Triggers a token-aware
    `SummarizingCompactor` over the current session's transcript,
    persists the compacted state to the store, and reports
    `before → after` message counts as a system message.
  - **`/resume` session picker in the TUI.** Opens a modal list of the
    ten most-recent stored sessions (↑/↓ navigate, Enter select, Esc
    cancel) and replays the chosen session's transcript into the TUI.
    Needs a `Store` that implements `SessionLister`; the file store
    qualifies.
- Added `glue.Agent.ListSessions` (mirrors `Agent.SearchSessions`):
  returns `[]SessionSummary` from a `SessionLister`-capable store, or
  the new `ErrSessionListingNotSupported` sentinel.
- TUI input layer polish (`cmd/glue/tui`):
  - Dropped the textarea's internal `│ ` prompt — the box border was the
    only vertical line you should see.
  - Default to **1-row input that grows to 6** as you type (was a
    3-row minimum that felt heavy for short prompts).
  - **Ctrl+J inserts a newline; Enter submits** (Ctrl+J is ASCII LF and
    works on every terminal — Shift+Enter does not). The old Alt+Enter
    binding is gone.
  - **Accent (purple) rounded border** on the input box so "type here"
    is unambiguous.
  - **Italic muted placeholder** ("Ask anything · / for commands") and
    **accent-colored cursor**.
  - **Input box capped at 100 columns and centered** on wider terminals
    so it doesn't feel disconnected from the conversation above.
  - Bracketed paste was already on by default — pasting multi-line text
    no longer fires multiple submits.
  - Welcome card + status bar updated with the new key hints.
- TUI polish (`cmd/glue/tui` v1.1):
  - **Markdown rendering** for assistant text via `charmbracelet/glamour`,
    applied after each turn completes (streaming stays plain to avoid
    partial-markdown flicker).
  - **Sticky scroll**: streaming deltas no longer yank you to the bottom
    if you scrolled up to read older context; the status bar shows
    `↓ more below` when there's content past the viewport.
  - **Permission prompts moved inside the relevant tool card** instead
    of a separate floating box, so the action keys appear next to the
    diff/args they're about.
  - **Welcome card** with example prompts replaces the bare-system
    startup state; rebuilt after `/clear`.
  - **`/help` and `/tools` render as rounded, titled blocks** instead of
    cramped one-line system messages.
  - **Per-tool spinner** for in-flight tool calls; **"thinking…"
    spinner in the status bar** between turn start and first stream
    chunk; spinner only animates during a turn (no idle ticking).
  - **Esc cancels the current turn** in addition to Ctrl+C.
  - **`/clear` now clears the transcript** and starts a fresh session id
    + welcome card (was: only changed the session id). `/new` is an alias.
  - **Mouse wheel scrolls the transcript** via the existing mouse-cell
    motion handler.
  - **Visual rule between turns** so user → assistant → user cadence is
    scannable on long sessions.

## 0.1.0 — 2026-05-27

First tagged release. Brings the framework to launch shape and stabilizes
the public surface for `go get github.com/erain/glue@v0.1.0`.

### Added (M7 dual-track surface)

- `tools/coding`, a reusable SDK package that assembles the local
  coding-agent tool bundle (`read_file`, `write_file`, `edit_file`,
  `list_dir`, `find_files`, `grep`, `shell_exec`, `git_diff_branch`,
  `git_log_branch`) over the existing `tools/fs`, `tools/git`,
  `tools/shell`, and `glue.Executor` primitives.
- `cmd/glue` coding-agent mode: `glue run --coding` and
  `glue serve --coding` register the SDK coding bundle, with local
  terminal permission prompts for one-shot runs and daemon-brokered
  permissions for served runs.
- `cmd/glue --provider`: `run` and `serve` select any registered
  provider (`codex`, `gemini`, `nvidia`, `openrouter`) through the
  `providers` registry instead of being hardwired to Gemini, so the
  binary can run as a coding agent on a ChatGPT subscription
  (`glue run --provider codex --coding`). `--model` defaults to the
  selected provider's registry default model.
- `tools/fs.FileEdit` (`edit_file`), a permission-gated surgical
  exact-string replacement tool with a unique-match guard and optional
  `replace_all`.
- Read-only navigation tools `tools/fs.ListDirTool` (`list_dir`),
  `FindTool` (`find_files`), and `GrepTool` (`grep`).
  Workspace-scoped and escape-safe; `grep` skips secret-shaped
  (Blocklist) and oversized files, and all three skip `.git`.

### Public surface present at this tag

For completeness, this first tagged release also stabilizes everything
shipped during the bootstrap and the long-running foundation
(ADR-0005). The full surface — `Agent` / `Session` / `Tool` /
`Provider` types, the `loop` package, the four providers, both stores
(`stores/file`, `stores/sqlite` with FTS5 search), every `tools/*`
package, subagents (`glue.SubagentTool`), skills/roles/AGENTS.md,
structured JSON, opt-in parallel tool execution, the `Compactor`
interface and `SummarizingCompactor`, the `prompts` versioned-prompt
catalog, the `cli` standard-flags helper, and the `cmd/glue`
`run` / `serve` / `connect` daemon protocol — is documented in
[`README.md`](README.md), [`docs/building-agents.md`](docs/building-agents.md),
and [`docs/design.md`](docs/design.md).

### Notes

- The Codex provider authenticates via `codex login` (subscription
  auth path OpenAI does not formally document). Intended for personal
  use; see [`SECURITY.md`](SECURITY.md) for the scope statement.
- The local executor is permission-gated, not sandboxed. Implement
  `glue.Executor` against a container/VM if you need isolation
  ([ADR-0009](docs/adr/0009-executor-permission-hook.md)).

## Initial bootstrap (pre-0.1.0)

The library was under active development as a Go agent harness
inspired by [pi-mono](https://github.com/badlogic/pi-mono) and
[Flue](https://github.com/withastro/flue) before this first tag. The
detailed history lives in the git log; the surface that survived into
`v0.1.0` is listed above under "Public surface present at this tag."
