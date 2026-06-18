# ADR-0019: Evals-as-Gates Harness

## Status

Accepted (2026-06-18).

## Context

The v1.13.0 harness work (edit repair ladder, retry/overflow recovery,
compaction, guardrails, capability registry) shipped real behavior
changes to the agent loop with no automated way to prove any of them
helped — or to catch a regression in the next change. The only feedback
loop was dogfooding by hand. A prompt tweak, a model swap, or a tool
description edit could quietly make the coding agent worse and nothing
would fail.

Research into Vercel's **Eve** framework (June 2026) flagged its
**evals** as the second clean borrow (sibling to tracing, ADR-0018):
scored test suites that run an agent against cases and gate deploys in
CI. glue had nothing equivalent.

## Decision

Add an `evals/` package — a Go-native eval harness — plus a `glue eval`
command that runs a declarative suite and gates on the aggregate pass
rate.

Core model:

- A **`Case`** is a prompt plus per-case prompt options and a list of
  **`Scorer`s**. A **`Suite`** is named cases with a shared pass
  threshold. A **`Runner`** runs each case in its own session against a
  `*glue.Agent` (optionally concurrently) and returns a **`Report`**.
- A **`Scorer`** grades a `Result` on a 0..1 scale. A case passes when it
  ran without error and every scorer meets the threshold
  (`DefaultPassThreshold` = 0.5, so 1/0 deterministic scorers and graded
  judge scores share one cutoff).
- **`Report.Gate(minPassRate)`** returns an error when the pass rate is
  too low; `glue eval` turns that into a non-zero exit so CI fails.

Built-in scorers cover the two shapes that matter:

- **Deterministic**: `Contains`, `NotContains`, `Regex`, `Equals`, and
  `Command` (runs a shell command in a directory, passing on exit 0).
  `Command` is the coding-agent gate — let the agent edit files, then run
  the build/tests and score the outcome.
- **`Judge`** (LLM-as-judge): grades a response against a rubric via an
  independent grading agent, for open-ended quality deterministic checks
  can't capture.

Key choices:

- **Go-native, not a TS/DSL runtime.** glue agents are compiled Go
  packages (ADR-0012); suites are too, so cases and scorers live next to
  the agent and run under `go test`. A declarative **`SuiteSpec`**
  (JSON) covers the built-in scorers for data-file suites and powers
  `glue eval`; it rejects unknown fields and scorer types so a typo is a
  loud error, never a silently-skipped check.
- **Library independent of the binary.** The `evals` package depends only
  on `glue`; the `glue eval` command is a thin front end that builds the
  agent-under-test (optionally with coding tools, auto-approving
  permission) and a separate, tool-less **judge agent** so an
  LLM-as-judge never grades its own work.
- **Errors fail loudly.** A scorer that errors (bad regex, judge call
  failure) fails the case as a harness problem rather than scoring zero
  silently; a low score is expressed as a `Value` with a `Reason`, not an
  error.

## Consequences

- A prompt/model/tool change can be regression-tested:
  `glue eval suite.json --min-pass-rate 0.9` exits non-zero in CI when
  the agent degrades. Suites can mix deterministic gates (build passes,
  no banned phrases) with judged quality.
- The harness runs real agent turns, so a suite costs provider tokens and
  wall-clock; it is opt-in (a command / a test), never on the default
  path.
- `Command` scorers and `--coding` eval runs execute tools and shell
  commands with auto-approval — they are meant for a sandbox, feature
  branch, or worktree, like `glue goal --yolo`. This is documented, not
  enforced.
- Out of scope for now: remote/deployed eval runs, dataset management,
  and per-case sandbox isolation. The `Runner` takes any `*glue.Agent`,
  so a host can supply a worktree-rooted one.
