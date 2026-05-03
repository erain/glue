# ADR 0002: Explicit, Caller-Owned Context Compaction

## Status

Accepted.

## Context

Long-running Glue sessions accumulate transcript messages indefinitely.
Eventually the transcript grows past what a provider's context window can
accept, or simply becomes wasteful to send on every turn. We need a way to
trim or summarize older context.

Two design axes:

1. **Trigger.** Token-based ("compact when prompt would exceed N tokens")
   vs. message-count-based ("compact when transcript has more than N
   messages"). Token-based is more accurate but requires per-provider
   token counting that we explicitly listed as out of scope for the first
   compaction implementation.
2. **Replacement strategy.** Drop older messages, summarize them via the
   model, or replace them with a small structured note. Calling the model
   to summarize gives the highest fidelity but is heavy and asynchronous;
   simple drop-with-note is fast and predictable.

## Decision

Compaction is an explicit, caller-owned policy. Glue defines:

- `Compactor` interface: `Compact(ctx, []Message) ([]Message, error)`. It
  receives a clone of the current transcript and returns the new
  transcript. Implementations must not mutate the input.
- `AgentOptions.Compactor` and `AgentOptions.CompactionThreshold`. The
  agent runs the compactor before every prompt whenever the in-memory
  transcript has more than `CompactionThreshold` messages.
- A built-in `KeepRecentMessages(n)` policy that keeps the last `n`
  messages and replaces everything older with a single assistant-role
  marker message recording how many turns were dropped.

When a compactor returns a new (shorter) transcript, the session's
in-memory state is replaced before `loop.Run` is called, and the next
save persists the compacted state.

## Why message-count for the first cut

- Provider-neutral: no token model required.
- Predictable for tests and operators.
- Easy to reason about in combination with the file store: the on-disk
  state is exactly what gets sent to the provider.
- Token-aware compaction can be added later as a separate `Compactor`
  implementation without changing the agent or session API.

## Consequences

- Callers opt in. The default behavior is unchanged: transcripts grow
  forever in memory until the caller decides what to do.
- Compaction summaries are visible in the transcript. The marker message
  carries `Metadata["compaction"]` so observers can filter or render it
  specially.
- The first marker is intentionally lossy: a fancier summarizer can be
  written as a different `Compactor` and slotted in by changing
  `AgentOptions.Compactor`.
- Persistent stores see compacted state on the next `Session.Prompt`
  save. Resuming a session in a new process sees the compacted form.
- Compaction happens inside the same `runMu`-locked region as the prompt,
  so concurrent `Session.Prompt` calls cannot interleave with compaction.
