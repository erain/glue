# ADR-0016: Goal loop — loop engineering and `/goal`

## Status

Accepted.

## Context

The popular 2026 shift in agentic coding is **loop engineering**: you stop
hand-prompting an agent turn-by-turn and instead design a loop that prompts it
for you, decides what's next, and runs until a goal is *verifiably* met. The
concrete embodiment is the **`/goal`** command — shipped built-in in OpenAI's
Codex CLI and cloned for Pi (`pi-goal`). You give one persistent objective and
the agent loops autonomously: plan → act → verify → repeat.

Across the reference implementations and the people who popularized the idea
(Addy Osmani; Boris Cherny, who built Claude Code; Peter Steinberger), the same
robust shape recurs:

- **Audit-gated completion.** Decompose the objective into a checklist of
  *verifiable deliverables*. Do not accept proxy signals ("tests look like they
  pass", "I wrote the code") as completion — confirm against real evidence.
- **Maker ≠ checker.** "The model that wrote the code is too nice grading its
  own homework." A separate verifier (different instructions, often a different
  model) audits.
- **Hard guardrails.** Max iterations, no-progress detection, and a token/$
  budget ceiling. Naive loops without these "generate confident mistakes at
  scale."
- **Memory on disk, not in context.** The strongest variant (the "Ralph loop")
  re-seeds context each iteration from durable external state, sidestepping
  context rot — Codex itself notes its continuation prompt can be lost after
  mid-turn compaction.

glue already has every supporting primitive (subagents, sessions with
structured output, the turn loop's `MaxTurns` budget, per-message usage,
worktree isolation, a scheduler in Peggy). The only missing piece is the
goal-directed **outer loop** itself.

## Decision

Add a **library primitive** — `Agent.PursueGoal(ctx, GoalSpec) (GoalResult,
error)` in the root `glue` package — that wraps the existing single-turn loop
in a goal-directed outer loop. Per ADR-0012, the primitive lives in the library
so every consumer (the `cmd/glue` TUI, `glue run --goal`, the daemon) is thin.

Each iteration:

1. **Plan** (first iteration only): a planner prompt decomposes the objective
   into a checklist of verifiable `ChecklistItem`s via `Session.PromptJSON`.
2. **Act (maker)**: a *fresh* session is prompted with a continuation prompt
   embedding the current checklist. It reads the real code/tests to reconstruct
   context, works the open items, and validates locally — but does **not**
   decide completion.
3. **Verify (checker)**: a separate session — its own model and a verifier
   system prompt — audits each checklist item against concrete evidence and
   returns a structured verdict (`Session.PromptJSON`). It is told not to trust
   the maker and not to accept proxy signals.
4. **Decide**: update the checklist from the verdict; stop on `achieved`,
   `blocked` (no progress), `budget_limited`, or `max_iterations`.

### Key choices (the open questions from the design)

- **Fresh-context Ralph loop, not accumulate-and-compact.** Each maker
  iteration runs in a fresh session seeded from the durable checklist. Memory
  lives in the checklist, not a growing transcript. More deterministic,
  cheaper, and it dodges the compaction-loss failure Codex hit. glue's
  compaction remains available for the inner per-iteration turn loop.
- **Configurable checker model, defaulting to the maker model.** The
  maker≠checker *judgment* separation (distinct session, verifier system
  prompt, structured verdict) is what matters; a different model is optional.
- **Session-scoped / in-memory for v1.** Durable `glue/goal:*` metadata and
  resume-across-restart are deferred to Phase 3, so Phase 1 is a clean,
  fully-testable primitive.
- **Guardrails are first-class** `GoalSpec` fields: `MaxIterations`,
  `NoProgressLimit`, `TokenBudget`. No-progress = the set of not-done item
  titles is unchanged for `NoProgressLimit` consecutive iterations.

### Rejected alternatives

- **Run the loop inside the user's one session (Codex-style accumulation).**
  Simpler to surface in a TUI, but couples memory to the transcript and inherits
  context rot / compaction-loss. Rejected in favor of fresh-context + durable
  checklist.
- **Checker self-grades in the maker session.** Defeats the maker≠checker
  principle; the writer rationalizes its own output. Rejected.
- **Put the loop in `cmd/glue` only.** Would strand `glue run`/daemon and
  violate ADR-0012's "library owns the primitive". Rejected.

## Consequences

- A reusable, headless goal loop that `glue run --goal` and a future TUI
  `/goal` both consume. Composed with Peggy's scheduler + worktrees + skills +
  MCP, it completes the "loop engineering" picture on glue.
- Phase 1 creates a fresh session per maker/checker iteration; long goals leave
  several namespaced sessions in the store. Acceptable for v1; Phase 3
  persistence can consolidate.
- The loop is only as safe as its verification. Callers should run goals on a
  branch/worktree and review the result — the loop produces work to ship, it
  does not ship it. This is the deliberate guard against the "unverified
  autonomy / cognitive surrender" failure modes.

## Rollout

- **Phase 1 (this ADR, #320):** `Agent.PursueGoal` primitive + maker/checker +
  guardrails, headless and tested.
- **Phase 2:** TUI `/goal`, `/goal pause|resume|clear|status`, status bar,
  live checklist panel (reuses the new `/` autocomplete).
- **Phase 3:** durable `glue/goal:*` state + resume; `goal/<slug>` branch
  isolation; daemon / scheduled goals.
