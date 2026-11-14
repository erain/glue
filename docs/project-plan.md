# Glue Project Plan

Direction reset: 2026-11-07. The active tracker is
[#392](https://github.com/erain/glue/issues/392). GitHub issues own current
status and acceptance criteria; this document owns direction and planning.

## Product direction

Glue has two product surfaces:

- **Agent framework:** the reusable Go API, provider-independent loop,
  tools, sessions, persistence, permissions, and goal execution.
- **Coding agent:** `cmd/glue`, an AX-first, remote-first coding agent for
  one-command remote jobs (`glue --remote`, `glue job`), local interactive TUI /
  headless goals, and optional browser access (`glue-gateway`) with verified PRs.

Peggy is retired; its source was removed in
[#393](https://github.com/erain/glue/issues/393). Keep reusable framework
capabilities; do not move Peggy's product features into core. Preserve Git
history, historical ADRs, and release records. Personal databases, credentials,
and deployments are outside source cleanup.

## Local and remote execution

Use **mode** for execution location; provider/model continues to mean the LLM
backend. Local execution can still call a hosted model API.

| Mode | Intended experience | Status |
|------|---------------------|--------|
| Remote (`AX-first`) | Dispatch a coding job directly from your laptop to AX (`glue --remote`), close the lid, inspect/attach/steer later (`glue job`), and get verified commits and a draft PR — plus optional host `glue-gateway` for browser UI. | Shipped ([#402](https://github.com/erain/glue/issues/402), [#526](https://github.com/erain/glue/issues/526), [ADR-0019](adr/0019-remote-jobs-architecture.md), [ADR-0020](adr/0020-ax-first-direct-execution.md)). |
| Local | Code interactively or run a goal in a workspace on this machine without AX or a cluster. | Shipped (`run`, TUI, `goal`, and `serve --coding`). |

Glue owns agent behavior and the coding-job contract; [AX](https://github.com/google/ax)
is Glue's sole external platform dependency ([ADR-0020](adr/0020-ax-first-direct-execution.md)).
`glue --remote` and `glue job` talk directly to AX via `internal/axbackend` with
zero `glue-system` Kubernetes manifests, default to the autonomous `auto` policy
(with opt-in `workspace-auto`, `ask-all`, and `--policy-file`), verify against a
standing `kind-glue-ax` cluster (Agent Substrate `v1.0.0` GA + AX `56bd0fc55574`),
and keep `glue-gateway` as an optional out-of-cluster host/VM browser portal.

## Historical planning baseline (2026-11-07)

Phase 1 ([remote-mode-roadmap.md](remote-mode-roadmap.md),
[ADR-0019](adr/0019-remote-jobs-architecture.md), [#402](https://github.com/erain/glue/issues/402))
and Phase 2 ([ADR-0020](adr/0020-ax-first-direct-execution.md),
[#526](https://github.com/erain/glue/issues/526)) are shipped, and so is
Phase 3 ([remote-mode-phase3-design.md](remote-mode-phase3-design.md),
2026-11-13), whose result is the [remote quickstart](remote-quickstart.md). The 2026-11-07
planning steps below record how the initial roadmap was produced:

This is the plan for producing that roadmap, not an implementation backlog.
Work through these deliverables in order under #392:

1. **Inventory and baseline.** Record what ships, current tests, reusable
   session/goal/daemon/executor seams, and active work. Preserve #358 / PR #359
   (tracing), #360 / PR #361 (evals), and #390 (NVIDIA smoke repair) as separate
   work. Recheck their status before scheduling; other agents may be coding.
2. **AX feasibility report.** Pin the AX source revision and runtime/image
   versions. Probe a minimal Glue job: workspace layout, runner packaging,
   model credentials, task observation, and durable result retrieval. Compare
   CLI and Go client integration. Capture commands, logs, failures, and cleanup;
   distinguish source inspection from live cluster evidence.
3. **User journeys and job contract.** Specify submit → return a durable ID →
   disconnect → inspect/reattach → retrieve a reviewable result. Also specify
   failure, cancellation, timeout, retry, and interrupted submission. Decide
   whether in-flight steering belongs after the first remote MVP.
4. **Architecture decisions.** Record the choices below in ADRs with evidence,
   alternatives, and remaining blockers. Choose the smallest useful remote
   slice before expanding the CLI, TUI, daemon, or SDK.
5. **Detailed roadmap.** Convert the decisions into dependency-ordered
   milestones and small implementation issues. Every issue needs Goal, Scope,
   Out of scope, Implementation notes, Docs update required, Verification
   commands, and Acceptance criteria. Include a test plan and evidence gate.

The inventory should re-evaluate prior deferred harness/TUI/daemon ideas for
this direction; they are candidates, not inherited commitments. The shipped
[harness analysis](coding-harness-roadmap.md) remains historical evidence.

## Decisions the roadmap must resolve

- **Inputs and workspace:** repository and exact base revision, clean remote
  clone versus explicit dirty-tree upload, instructions/skills, toolchain,
  network policy, output location, and workspace isolation.
- **Execution boundary:** whole-job entry point and image, bounded goals,
  noninteractive permissions, CLI versus SDK transport, AX context/atespace,
  compatibility pin, and prerequisite diagnostics.
- **Durability:** remote job identity, local-to-AX mapping, persistent state,
  reconnection without resubmission, duplicate prevention, and ambiguous
  submission outcomes. Do not automatically replay a possibly accepted job.
- **Completion and artifacts:** separate AX infrastructure state from Glue
  success/blocked/budget/error outcomes. Define a durable completion record,
  patch or branch against the base revision, logs, summary, test commands and
  exit status, and artifact retrieval after process exit or sandbox cleanup.
- **Credentials and trust:** provision only required repository/model access;
  keep secrets out of manifests, logs, and artifacts. Define unattended
  permission policy, budgets, cancellation, retention, and explicit cleanup.
- **Review:** retrieve changes without overwriting a dirty local workspace;
  surface conflicts and failed verification. Automatic merge is outside MVP.

AX currently keeps its runner alive after the child command exits and does
not propagate that exit status into control-plane completion. Therefore
`Running`/`Ready` is not proof that coding work succeeded. The feasibility
report must validate this against the pinned revision and design a completion
channel. Sources checked on 2026-11-07:
[runner contract](https://github.com/google/ax/blob/main/docs/runner.md) and
[core concepts](https://github.com/google/ax/blob/main/docs/concepts.md).

## Readiness and delivery gates

The planning phase is done when the inventory, reproducible AX report, job
contract, ADRs, and ordered issue backlog agree. Each milestone must name its
user-visible result, dependencies, verification, and unresolved decisions.
No implementation should begin from a vague “remote backend” ticket.

The first remote delivery must prove one bounded job end to end: submit,
close the client, reconnect by ID, and retrieve a patch plus verification
against a known base revision. Include a failed job, lost connection, and
cancel/timeout evidence. A successful fake adapter or ready sandbox alone
cannot establish that remote execution is shipped. Local workflows must
continue to pass their normal checks without AX installed.

## Operating model and history

Use one issue and one focused draft PR per logical change. Verify locally,
review the full diff, and let the human decide when to mark ready or merge.
Completed implementation requires a merged PR. Owner-canceled or superseded
work is closed as not planned with an explanation; never count it as delivered.
See [CONTRIBUTING.md](../CONTRIBUTING.md).

[#1](https://github.com/erain/glue/issues/1) preserves the bootstrap roadmap;
[#110](https://github.com/erain/glue/issues/110) preserves Peggy and subsequent
coding-agent milestones. [ADR-0018](adr/0018-framework-coding-and-ax-direction.md),
[ADR-0019](adr/0019-remote-jobs-architecture.md), and
[ADR-0020](adr/0020-ax-first-direct-execution.md) record the product boundary
and AX-first remote architecture; [design.md](design.md) describes the core
framework and job internals.
