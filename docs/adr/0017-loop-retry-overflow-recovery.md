# ADR-0017: Loop-Level Retry and Overflow Recovery

## Status

Accepted (2026-06-09).

## Context

Glue deliberately shipped without provider-level retries: transport
concerns were left to provider SDKs, and ADR-0006 quarantined
subscription-auth fragility inside the Codex provider on the same
principle. In practice (dogfooding the coding agent on Gemini and
OpenRouter open-weight models), two failure shapes dominate lost
turns:

1. **Transient provider failures** — 429s, 5xx, dropped SSE streams
   ("stream closed before done event"). The provider SDKs retry some
   HTTP-level cases, but a stream that dies mid-turn surfaces as a
   turn error and kills the whole run, even mid-goal-loop.
2. **Context-window overflow** — the request outgrew the model. This
   is *never* fixed by retrying, but it is fixed by compacting; the
   session has a compactor and the loop does not.

Every reference harness we analyzed (pi, Cline, Codex CLI, Gemini
CLI — see `docs/coding-harness-roadmap.md`) recovers both
automatically. pi's state machine is the closest shape to glue's
architecture.

## Decision

Add a small, classified retry layer at the **loop** level, and
overflow recovery at the **session** level — each where the needed
capability lives:

- `loop.RetryPolicy` on `RunRequest`. The zero value enables retries
  (3 retries, 2s base doubling to a 30s cap); `Disabled: true`
  restores fail-fast. Errors are classified by pattern banks:
  *overflow* → returned immediately as typed `*loop.OverflowError`;
  *fatal* (auth, invalid request, billing, 404) → returned
  immediately; *transient* → retried with backoff, honoring
  server-provided hints (`Retry-After`, Gemini `RetryInfo.retryDelay`)
  when they exceed the computed delay. `EventRetry` is emitted before
  each sleep so UIs can show "reconnecting n/m". Nothing is appended
  to the transcript until an attempt succeeds, so retries cannot
  duplicate history.
- `glue.Session.Prompt` catches `*loop.OverflowError` and, when the
  agent has a `Compactor`, compacts once (ignoring the size
  threshold — the provider just said we are over) and retries once.
  Without a compactor the typed error surfaces unchanged.

## Consequences

- The "no provider-level retry" stance is amended, not reversed:
  providers still own transport, and the loop owns *turn-level*
  recovery with bounded, observable, opt-out behavior.
- Default behavior changes: previously a 429 failed the turn; now it
  retries up to 3 times (~14s worst case) before failing. Library
  consumers that depended on fail-fast set `Retry.Disabled`.
- Pattern-bank classification is heuristic by design (providers do
  not expose typed errors through the streaming interface). Unknown
  errors classify as fatal, so the bank can only under-retry, never
  retry something destructive.
