# Remote Mode Phase 3: From Working Demo to Developer Aha Moment

Status: shipped, 2026-11-13 (PRs #567–#614; the exit gate
`scripts/demo/aha.sh` passes). Proposed 2026-11-11. Tracked in
[#402](https://github.com/erain/glue/issues/402) (design issue
[#537](https://github.com/erain/glue/issues/537)). Builds on Phase 1
([ADR-0019](adr/0019-remote-jobs-architecture.md)) and Phase 2
([ADR-0020](adr/0020-ax-first-direct-execution.md),
[#526](https://github.com/erain/glue/issues/526)).

Phases 1 and 2 proved the mechanism: a durable job runs in an AX sandbox,
survives disconnects and suspends, and can end in a draft pull request. This
document reviews the result as a developer who uses Claude Code, Codex,
Antigravity and OpenCode every day would, and plans the next round of work.
The goal of Phase 3 is not more mechanism. It is that the first remote job a
developer runs makes them want to run a second one.

## 1. Why a developer would run coding work remotely

| Pull | What the developer wants | What Glue has today |
|---|---|---|
| Parallel work | Run five tasks at once without five worktrees, port clashes or a hot laptop | One job per submit; no grouping, no comparison |
| Close the lid | Long tasks keep going; pick them up from another machine or a phone | Works: durable jobs, replayable streams, multi-laptop discovery |
| Safe autonomy | No permission prompts, because the sandbox is the boundary | Works: gVisor, egress allowlist, `auto` policy. The agent can still read Glue's own token (finding 9) |
| Results I can trust | A branch or PR with test evidence, not a chat log | Works with `--verify` and `--deliver pr`; by default the work stays in the sandbox |
| Stay in my flow | Hand off from where I am (my branch, my uncommitted work, my session) and get the result back into my checkout | Missing in both directions |
| My infrastructure | My cluster, my model keys, any provider; code and secrets never go to a vendor's cloud | Works, and it is the main difference from hosted cloud agents |

The last row is why Glue remote mode deserves to exist. Hosted background
agents already do the first four rows well. A self-hosted, model-neutral,
open-source remote agent only wins if the everyday experience is as smooth as
theirs. Today it is not, and the gaps are in the plain parts of the journey,
not the hard ones.

## 2. Hands-on findings

All findings come from one session on the standing `kind-glue-ax` cluster on
2026-11-11, using a binary built from `main` at `739567f`, acting as a new
user. The test: clone `erain/glue-sandbox`, create a local branch with one
unpushed commit and one uncommitted edit, then run

```sh
glue --remote "Add a function Shout(name string) string to the sandbox package that returns the greeting from Greet in upper case, with a table-driven test." \
  --verify "go test ./..." --attach
```

`Greet` existed only in local state. That was not planned, and it turned out to
be the most useful part of the test.

| # | Finding | Evidence | Fixed by |
|---|---|---|---|
| 1 | Setup errors do not say what is wrong. | Without `source scripts/ax-dev/env.sh`, `glue job list` prints `dial tcp 127.0.0.1:8787: connect: connection refused` (a stale local daemon file wins target resolution). | [#538](https://github.com/erain/glue/issues/538) |
| 2 | Submit is silent and slow. | Nothing for 13.0 s, then a bare job ID; first event at 32.5 s. No repo, base, model or budget is shown. | [#539](https://github.com/erain/glue/issues/539), [#550](https://github.com/erain/glue/issues/550) |
| 3 | Local work is dropped without a word. | The job cloned `origin/main`; the current branch, the unpushed commit and the dirty file were ignored silently. | [#540](https://github.com/erain/glue/issues/540), [#541](https://github.com/erain/glue/issues/541) |
| 4 | Planning is slow and loops. | 12 planner tool calls, five of them the identical `list_dir {"path":"."}`; 203 s and 42,914 tokens to plan a one-file repo. | [#543](https://github.com/erain/glue/issues/543) |
| 5 | Each step is slow. | Trivial tool calls arrive 7 to 25 s apart. | [#550](https://github.com/erain/glue/issues/550) |
| 6 | A job with a false premise does not stop. | The agent searched git history, `/tmp`, `/proc` and Glue's state for a function that was never there. No default time or token limit; canceled by hand after 17 minutes. | [#542](https://github.com/erain/glue/issues/542), [#552](https://github.com/erain/glue/issues/552) |
| 7 | Steering arrives too late. | `glue job say` was logged as `operator: ...` and the agent continued unrelated exploration for four more minutes. Messages apply at the next iteration, not the next step. | [#551](https://github.com/erain/glue/issues/551) |
| 8 | The transcript is hard to read. | `[tool done] shell_exec: exit_code: 0 timed_out: false truncated: false stdout: ...`; no times, no phases, a one-line ending. | [#547](https://github.com/erain/glue/issues/547) |
| 9 | The agent can read Glue's token and state. | `ls -la /workspace/.glue` and `cat /workspace/.glue/state/sessions/<job>.json` succeed; everything runs as root. Under an ask policy the agent could approve its own requests. | [#544](https://github.com/erain/glue/issues/544) |
| 10 | Redaction corrupts normal text. | `ax-task-runner` became `ax-ta[REDACTED]`; the model then ran the corrupted path. | [#545](https://github.com/erain/glue/issues/545) |
| 11 | `show` and `list` do not show progress. | `tokens: 42914`, `iteration: 1` for 14 minutes. `list` is three tab-separated columns. Full job IDs everywhere. | [#549](https://github.com/erain/glue/issues/549) |
| 12 | History cannot be re-read. | A second `attach` on the same machine prints nothing old; both share one cursor. | [#548](https://github.com/erain/glue/issues/548) |
| 13 | Finished work is stranded. | Default delivery is `none`. The result names a branch that exists only in the sandbox. No command prints the diff or brings it home. | [#554](https://github.com/erain/glue/issues/554), [#555](https://github.com/erain/glue/issues/555) |
| 14 | Finished tasks keep their worker. | After `glue job cancel`, `ax get tasks` shows the task `Running`. In direct mode nothing suspends a finished task; the pool has four workers. | [#546](https://github.com/erain/glue/issues/546), [#563](https://github.com/erain/glue/issues/563) |
| 15 | Nothing tells the developer a job is done. | The only signal is running `glue job list` again. | [#553](https://github.com/erain/glue/issues/553) |
| 16 | Only Go repos, and only with a locally built image. | The image has Go, git and make. Egress hosts, CPU and memory are fixed in code. `DefaultImage()` is empty for a new user. | [#559](https://github.com/erain/glue/issues/559)–[#562](https://github.com/erain/glue/issues/562) |
| 17 | The TUI and remote mode do not know each other. | No `/remote`, no job list in the TUI. | [#557](https://github.com/erain/glue/issues/557), [#558](https://github.com/erain/glue/issues/558) |

What worked well and must not regress: a second terminal could list, inspect
and attach to the job at once; `say` and `cancel` returned in about 0.15 s;
read-only commands did not wake anything; credentials never appeared in the
task spec or the transcript.

## 3. The target experience

```text
~/src/myapp (feature/retry *) $ glue --remote "Make the retry tests pass under -race" --snapshot --attempts 2
  snapshot   3 files, 1 unpushed commit -> glue/snapshot-…        0.9s
  sandbox    ready (ghcr.io/erain/glue-task:v1.14)                3.1s
  started    2 attempts · gemini-3.8-flash · budget 60m / 2M tokens
  group grp-41c2   watch: glue job attach latest   stop: glue job cancel grp-41c2

(lid closed)

phone: "glue: 2/2 attempts finished · go test -race ./... passed on both · glue job compare grp-41c2"

$ glue job compare grp-41c2
$ glue job pull 7f3a --checkout        # the change is in my checkout; I keep working
```

In words:

1. **It starts from where I am.** My branch, my unpushed commits and, when I
   ask, my uncommitted work.
2. **It answers within seconds** and always shows what it decided.
3. **It stops itself** when it is stuck, and asks when the question is mine.
4. **It tells me** when it is done or needs me.
5. **The work comes home** with one command, or as a draft PR.
6. **It scales out**: several attempts or several issues at once.
7. **It runs on my cluster with my keys**, and the agent cannot reach what it
   should not.

## 4. Design principles

- **Local state is the default input.** A remote job that silently uses
  different code than the developer sees is a bug, not a limitation.
- **Never strand work.** Every job that changed files leaves a branch, also
  when it is canceled or fails.
- **Every wait shows progress; every stop gives a reason.**
- **The sandbox boundary includes Glue itself.** The agent is untrusted with
  respect to the daemon token, job state and approval path.
- **Bounded by default.** Time, tokens and repeated actions have limits unless
  the developer removes them.
- **Direct mode stays complete.** Nothing in Phase 3 requires the optional
  gateway. The gateway remains the always-on option (browser, unattended
  suspend).
- **No new platform.** AX stays the only external dependency (ADR-0020). Phase 3
  adds no Kubernetes manifests.

## 5. Design by milestone

### K. Trust the first run

`glue job doctor` ([#538](https://github.com/erain/glue/issues/538)) checks the
target, image, credentials and repo access and prints one fix per failure.
Submit reports stages and a summary on stderr while stdout stays the bare job
ID ([#539](https://github.com/erain/glue/issues/539)).

Local state ([#540](https://github.com/erain/glue/issues/540),
[#541](https://github.com/erain/glue/issues/541)): the base ref is the current
branch when `origin` has it. Dirty trees and unpushed commits stop the submit
with the choices spelled out. `--snapshot` builds one commit from the working
tree with a temporary index (the user's index, branch and stash are untouched),
pushes it to `glue/snapshot-<job-id>`, and uses that as the base. It has to be
a branch because AX prepares the workspace with
`git fetch --depth=1 origin <branch>`.

Bounds ([#542](https://github.com/erain/glue/issues/542),
[#543](https://github.com/erain/glue/issues/543)): AX jobs default to 60
minutes and 2M tokens. The planner gets at most six tool turns. The loop
detects the same call with the same result three times and stops at six.

Boundary ([#544](https://github.com/erain/glue/issues/544),
[#545](https://github.com/erain/glue/issues/545)): shell, verify and git
commands run as an unprivileged `glue-agent` user; the state directory and
token stay root-only; file tools reject the state directory. If gVisor on AX
does not allow the UID switch, the fallback is a memory-only token and state
outside any path the shell can reach. Redaction patterns get word boundaries
and realistic lengths.

Lifecycle ([#546](https://github.com/erain/glue/issues/546)): direct mode has
no always-on component, so cleanup is done by whichever client comes next:
`attach` suspends after `job_done`, `list` and `show` reconcile awake tasks
whose job has finished, `cancel` suspends, and `glue job prune` deletes old
jobs. A finished task that no client visits keeps its worker until then; the
gateway or upstream `onCompletion` support (google/ax#420) closes that gap.

### L. Watch without squinting

The attach renderer ([#547](https://github.com/erain/glue/issues/547)) prints
phases, one line per tool with duration and outcome, a live status line on a
terminal, and a closing summary with the next commands. It is tested against
golden files from a recorded event stream. `glue job logs`, `--from-start` and
`--tail` ([#548](https://github.com/erain/glue/issues/548)) make history
re-readable. `show` gains live activity and per-turn usage; `list` becomes a
table; IDs accept prefixes and `latest`
([#549](https://github.com/erain/glue/issues/549)).

Latency ([#550](https://github.com/erain/glue/issues/550)) gets a script and
budgets: job ID within 5 s on a warm cluster, first event within 15 s, median
trivial model turn within 5 s. The issue fixes the two largest contributors
and files the rest with numbers.

### M. Steer and be asked

Operator messages go into a loop-level steering queue and are delivered before
the next model request; `--interrupt` also cancels the in-flight call
([#551](https://github.com/erain/glue/issues/551)). An `ask_user` tool moves
the job to `waiting_for_input` with a persisted question, answered from the
CLI, attach or the web UI, with a timeout after which the job proceeds on a
recorded assumption ([#552](https://github.com/erain/glue/issues/552)).
`--notify <url>` posts from inside the sandbox when the job waits or ends; the
URL travels with runtime credentials and its host joins the egress allowlist
([#553](https://github.com/erain/glue/issues/553)). A plain webhook covers
ntfy and Slack-style endpoints without any service-specific code.

### N. Bring the work home

AX jobs default to `delivery: branch`, and finalize also pushes partial work
for canceled and failed jobs. A patch endpoint and `glue job diff` show the
change without waking the task again
([#554](https://github.com/erain/glue/issues/554)). `glue job pull` fetches the
branch or applies the patch with a three-way merge
([#555](https://github.com/erain/glue/issues/555)). `glue job continue
--from-pr` turns review threads and failed checks into continue instructions
and reuses the open PR ([#556](https://github.com/erain/glue/issues/556)).

From the TUI, `/remote <objective>` submits with a `context` field filled from
the session summary ([#557](https://github.com/erain/glue/issues/557)), and
`/jobs` lists, shows and pulls jobs
([#558](https://github.com/erain/glue/issues/558)). The handoff carries a
summary, not the transcript: it is small, provider-neutral and good enough for
a fresh planner.

### O. Works on my repo

`.glue/remote.yaml` holds image, setup, verify, delivery, egress hosts,
resources, budgets and policy, with flags taking precedence
([#559](https://github.com/erain/glue/issues/559)). Setup commands run once
before planning and a failure ends the job as `blocked` without calling the
model ([#560](https://github.com/erain/glue/issues/560)). The task image gains
Node.js, Python and common build tools
([#561](https://github.com/erain/glue/issues/561)) and is published to GHCR as
the default ([#562](https://github.com/erain/glue/issues/562)), so a new user
builds nothing.

### P. Scale out and prove it

A full worker pool yields `queued` and a retry command instead of an unbounded
wait ([#563](https://github.com/erain/glue/issues/563)). `--attempts N` runs a
group of jobs, optionally across models; `compare` puts them side by side and
`pick` delivers one and cleans up the rest
([#564](https://github.com/erain/glue/issues/564)). `--issue` turns GitHub
issues into jobs whose PRs close them
([#565](https://github.com/erain/glue/issues/565)). The exit gate is a
five-minute quickstart and `scripts/demo/aha.sh`, which runs the story in
section 3 end to end and fails on any missed budget
([#566](https://github.com/erain/glue/issues/566)).

## 6. Work queue

Each task is one issue and one draft PR under 500 changed lines, with named
tests. Pick the first unchecked task in the earliest wave whose dependencies
are merged. Tasks inside a wave are independent unless a dependency is listed.

**Wave 1: the first job is trustworthy** (P0)

| Task | Issue | Depends on |
|---|---|---|
| K3 Current branch; refuse to drop local work | [#540](https://github.com/erain/glue/issues/540) | |
| K5 Default time and token budgets | [#542](https://github.com/erain/glue/issues/542) | |
| K6 Planner cap and repeated-call guard | [#543](https://github.com/erain/glue/issues/543) | |
| K9 Suspend finished tasks; `prune` | [#546](https://github.com/erain/glue/issues/546) | |
| K2 Submit progress and summary | [#539](https://github.com/erain/glue/issues/539) | |
| K1 `glue job doctor`; actionable errors | [#538](https://github.com/erain/glue/issues/538) | |
| K7 Unprivileged agent shell; hide state and token | [#544](https://github.com/erain/glue/issues/544) | |
| K8 Redaction false positives | [#545](https://github.com/erain/glue/issues/545) | |

**Wave 2: the loop closes** (P0)

| Task | Issue | Depends on |
|---|---|---|
| K4 `--snapshot` | [#541](https://github.com/erain/glue/issues/541) | #540 |
| N1 Branch by default; `glue job diff` | [#554](https://github.com/erain/glue/issues/554) | |
| N2 `glue job pull` | [#555](https://github.com/erain/glue/issues/555) | #554 |
| L1 Readable transcript | [#547](https://github.com/erain/glue/issues/547) | |
| M1 Steering at the next turn; `--interrupt` | [#551](https://github.com/erain/glue/issues/551) | |

**Wave 3: comfortable to live with** (P1)

| Task | Issue | Depends on |
|---|---|---|
| L2 `logs`, `--from-start`, `--tail` | [#548](https://github.com/erain/glue/issues/548) | #547 |
| L3 Live `show`, table `list`, ID prefixes | [#549](https://github.com/erain/glue/issues/549) | |
| L4 Latency script, budgets, top two fixes | [#550](https://github.com/erain/glue/issues/550) | #539, #543 |
| M2 `ask_user` | [#552](https://github.com/erain/glue/issues/552) | #551 |
| M3 `--notify` | [#553](https://github.com/erain/glue/issues/553) | |
| N3 Continue from PR feedback | [#556](https://github.com/erain/glue/issues/556) | #554 |
| O1 `.glue/remote.yaml`; egress and resource flags | [#559](https://github.com/erain/glue/issues/559) | #539 |
| O2 Setup phase | [#560](https://github.com/erain/glue/issues/560) | #559 |
| O3 Node.js and Python in the task image | [#561](https://github.com/erain/glue/issues/561) | |
| O4 Publish the image; default to it | [#562](https://github.com/erain/glue/issues/562) | #561, #538 |
| P1 `queued` when the pool is full | [#563](https://github.com/erain/glue/issues/563) | #546, #539 |

**Wave 4: the aha** (P1 and P2)

| Task | Issue | Depends on |
|---|---|---|
| N4 TUI `/remote` handoff | [#557](https://github.com/erain/glue/issues/557) | #540, #541, #539 |
| P2 `--attempts`, `compare`, `pick` | [#564](https://github.com/erain/glue/issues/564) | #554, #563, #549 |
| P3 `--issue` | [#565](https://github.com/erain/glue/issues/565) | #557, #563 |
| N5 TUI `/jobs` panel | [#558](https://github.com/erain/glue/issues/558) | #557, #555, #547 |
| P4 Quickstart and scripted demo (exit gate) | [#566](https://github.com/erain/glue/issues/566) | #538, #541, #546, #547, #550, #553, #555, #562, #564 |

After Wave 2 a developer can already hand off dirty work, watch a readable
transcript, correct the job, and pull the result. That is the smallest version
of the aha and a good point to re-run the hands-on test from section 2.

### Re-run after Wave 2 (2026-11-12)

The hands-on test from section 2 was repeated on a local build that merges every Wave 1 and Wave 2 branch (draft PRs #568–#583, pushed as `integration/phase3-wave1-2`). It started from the same situation: branch `my-feature` with an unpushed commit that adds `Greet`, an uncommitted `README.md` edit, and an objective that depends on `Greet`. Model `gemini-3-flash-preview`, thinking `low`.

| Step | Before | After |
|---|---|---|
| Setup check | none | `glue job doctor`: 7/7 checks ok |
| Submit from the dirty branch | silently cloned `origin/main` | refused, listing the branch, the unpushed commit and the dirty file |
| `--snapshot` submit | n/a | `snapshot: 1 changed file(s) on top of my-feature@191b5f6`, stages printed, sandbox ready at 14.7 s |
| Run | 17 min searching for `Greet`, canceled by hand | `achieved` in 1m14s, 59,829 tokens, `go test ./...` verified |
| Transcript | raw tool records | phases, one line per tool with timing, summary block |
| Finished task | stayed `Running` | `Suspended` |
| Result | branch only inside the sandbox | `glue job diff --stat`, then `glue job pull --checkout`; local `go test ./...` passes on the pulled branch |

Still open from this run (tracked): `glue job show` reports `iteration: 0, tokens: 0` for a job that finished while attached (#549); the first `go test` in a fresh sandbox took 14 s while it filled the module and build caches (#550, #560); the checker still uses its full 12-turn allowance (#580 bounds it).

## 7. Definition of done

The definition of done in [#402](https://github.com/erain/glue/issues/402)
applies to every task. Two additions for Phase 3:

1. Each PR repeats the "Observed today" reproduction from its issue and shows
   the new behavior next to it.
2. Tasks that change what a job does on AX include a live test gated by
   `GLUE_E2E_AX=1` that fails, not skips, under `CI=true` when the variable is
   set.

## 8. Not in this phase

- Submitting jobs from the browser, and a notification inbox in the web UI.
- Triggering jobs from GitHub events or comments, and scheduled jobs.
- Moving a full session transcript between local and remote.
- Dependency caches shared between jobs, and devcontainer support.
- A Glue-side scheduler or autoscaling for the worker pool.
- Multi-repo jobs.

## 9. Risks

| Risk | Handling |
|---|---|
| gVisor on AX may not allow the UID switch for the agent shell | #544 probes first and has a stated fallback |
| AX's behavior with a full worker pool is unknown | #563 starts with a recorded probe |
| A public image needs a public `ax-task-runner` base | #562 builds the runner from the pinned AX revision if upstream publishes none |
| Snapshots push the developer's unfinished work to `origin` | Explicit `--snapshot` only; size limit; branch removed on `glue job delete`; documented |
| Per-step latency may be dominated by the model, not Glue | #550 measures before fixing and keeps the default model out of scope |
| Direct mode cannot suspend a finished task with no client connected | Documented in #546; the gateway or google/ax#420 covers it |
