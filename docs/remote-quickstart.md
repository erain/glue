# Remote jobs in five minutes

Hand a coding task to a sandbox, close your laptop, get a phone notification when it is done, and bring the result into your checkout. Everything below is copy-paste; expected output is shown under each command.

## What you need

- `glue` on your `PATH`: `go install github.com/erain/glue/cmd/glue@latest`
- An AX cluster in your current Kubernetes context. For a local one, run `scripts/ax-dev/up.sh` from a clone of this repository, then `source scripts/ax-dev/env.sh`.
- A model key: `export GEMINI_API_KEY=...` (or `OPENROUTER_API_KEY`).
- A GitHub token that can push to your repo: `gh auth login` is enough (or `GITHUB_TOKEN`).
- A git checkout of that repo whose `origin` is on GitHub.

You do not need to build an image: jobs use the published `ghcr.io/erain/glue-task` image (Go, Node.js, Python, a C/C++ compiler, ripgrep, jq), pinned to its current digest.

## 1. Check that everything is in place

From your checkout:

```sh
glue job doctor
```
```
ok    target         AX context kind-glue-ax, kubeconfig ~/.cache/glue/ax-dev/kubeconfig, atespace default
ok    ax connection  tunnels up, router credentials minted
ok    ax-server      reachable, 1 job(s)
ok    task image     ghcr.io/erain/glue-task@sha256:309f…58cc (ghcr.io/erain/glue-task:main, published image)
ok    repo           https://github.com/erain/glue-sandbox @ main
ok    github token   from gh auth token, can push to erain/glue-sandbox
ok    provider key   gemini from env GEMINI_API_KEY
```

Every line that is not `ok` says how to fix it.

## 2. Start a job, with the work you have not pushed yet

`--snapshot` sends your working tree as it is, uncommitted files included. `--notify` posts to your phone: pick a hard-to-guess [ntfy.sh](https://ntfy.sh) topic and subscribe to it in the ntfy app.

```sh
glue --remote "Add a function Sub(a, b int) int next to Add in add.go, with a table-driven test in add_test.go." \
  --snapshot --verify "go test ./..." --notify https://ntfy.sh/<your-topic>
```
```
snapshot: 2 changed file(s), 0 KiB on top of main@bf0fbc54 -> origin glue/snapshot-job-f8a57d276fb8
job-f8a57d276fb8
  repo      https://github.com/erain/glue-sandbox @ glue/snapshot-job-f8a57d276fb8
  model     gemini/gemini-3-flash-preview (thinking: low)
  policy    auto · delivery branch
  …
  verify    go test ./...
  watch     glue job attach job-f8a57d276fb8
```

The job ID comes back in about 3 seconds. (The first job after a new image is published waits about 10 seconds more while the cluster pulls it.) Without `--snapshot`, glue refuses to start from a checkout with unpushed work, so the job never silently works on different code.

Want two tries, or two models, at once? Add `--attempts 2` (and `--model a --model b`), then `glue job compare <group>` and `glue job pick <id>`.

## 3. Watch it, or don't

```sh
glue job attach latest      # live transcript; Ctrl-C detaches, the job keeps going
```
```
[00:03] planning
[00:21] plan (3 items):
          [ ] Implement Sub(a, b int) int in add.go
          [ ] Create table-driven tests for Add and Sub in add_test.go
          [ ] Verify test suite passes by running go test
…
[02:50] job achieved — Sub is implemented and a table-driven test covers Add and Sub.
  status       achieved
  verify       ✓ go test ./... (exit 0, 0.5s)
  branch       glue/job-f8a57d276fb8
```

Your phone gets "job-f8a57d276fb8 achieved …" when it finishes. If the agent needs a decision only you can make, it asks: you get a notification, and you answer with `glue job answer latest "<text>"` or from the web UI. To redirect it mid-run: `glue job say --interrupt latest "<text>"`.

## 4. Bring the result home

```sh
glue job diff latest --stat
glue job pull latest --apply
go test ./...
```
```
applied job job-f8a57d276fb8 to the working tree (not committed):
 add.go      |  3 +++
 add_test.go | 35 +++++++++++++++++++++++++++++++++++
 2 files changed, 38 insertions(+)
next: review with git diff, then commit
ok  	example.com/sandbox	0.002s
```

`--apply` puts the changes in your working tree on top of the uncommitted work you sent. `glue job pull latest --checkout` switches to the job's branch instead. A job started with `--deliver pr` opens a draft PR. For review comments on that PR, run `glue job continue latest --from-pr`.

## 5. Clean up

```sh
glue job list
glue job delete latest      # deletes the sandbox (and a --snapshot job's snapshot branch)
glue job prune              # deletes finished jobs older than 7 days
```

## Next

- Put repo-wide settings (setup and verify commands, image, resources) in `.glue/remote.yaml`: see [jobs.md](jobs.md#per-repo-settings-glueremoteyaml).
- Start from a GitHub issue: `glue --remote --issue 123`.
- From the interactive TUI: `/remote <task>` hands the task off with your session as context, and `/jobs` shows your jobs.

## Proving it on every release

`scripts/demo/aha.sh` runs this story on the standing cluster against `erain/glue-sandbox`. It sends uncommitted work with `--snapshot`, runs two attempts, receives the notifications, compares, pulls, and runs the tests, and it fails on any miss, including the latency budgets (submit ≤ 5 s per job, both attempts done ≤ 240 s). A recorded run, 2026-11-13:

```
submitted job-f8a57d276fb8 job-cf7e5e72a8c4 in 6.0s (3.0s per job, budget 5s)
both finished 182.6s after submit (budget 240s)
JOB               MODEL    STATUS    VERIFY  CHANGED         TIME   TOKENS
job-f8a57d276fb8  default  achieved  pass    2 files +38 -0  2m50s  42090
job-cf7e5e72a8c4  default  achieved  pass    2 files +28 -0  2m58s  68290
received 2 notification(s)
ok  	example.com/sandbox	0.002s
AHA OK: 2 attempts, best job-f8a57d276fb8, 183.8s from submit to tested code in your checkout
```
