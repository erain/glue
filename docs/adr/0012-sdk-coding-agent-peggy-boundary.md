# ADR-0012: Glue SDK, Glue Coding Agent, and Peggy Product Boundary

## Status

Accepted.

## Context

Glue now has two consumers with different jobs:

- `cmd/glue` should prove and expose the framework as a local agent
  harness and coding-agent binary.
- `agents/peggy` should be an always-on personal assistant product built
  on top of Glue, not the owner of reusable coding-agent execution logic.

Keeping coding tools inside Peggy creates a product/framework inversion:
other agents cannot reuse the tool bundle without importing Peggy, and
Glue's own binary cannot dogfood the coding-agent path directly.

## Decision

Glue owns reusable coding-agent primitives and the default coding-agent
binary surface:

- `tools/coding` assembles the standard local coding tool bundle over
  `tools/fs`, `tools/shell`, `tools/git`, and `glue.Executor`.
- `cmd/glue run --coding` runs the SDK-backed coding agent in one-shot
  terminal mode with local permission prompts for side effects.
- `cmd/glue serve --coding` exposes the same coding-agent tool surface
  through the daemon protocol, where `glue connect` brokers permissions.
- Peggy consumes `tools/coding` as a product integration detail and may
  inject a different `glue.Executor`, but it does not own the reusable
  coding-agent tool assembly.

## Loopholes and Fixes

- **Loophole: SDK package exists but no binary exercises it.**
  Fixed by wiring `cmd/glue run --coding` and `cmd/glue serve --coding`
  to `tools/coding`.

- **Loophole: side-effect tools could run locally without an operator
  decision.**
  Fixed by giving `glue run --coding` a local terminal permission
  implementation. Daemon runs already use the daemon permission broker.

- **Loophole: Peggy could drift back into owning coding execution.**
  Fixed by making Peggy's `CodingTools` a thin adapter over
  `tools/coding` and documenting the boundary here.

- **Loophole: local execution is not a sandbox.**
  Not fixed in this ADR. The accepted boundary is permission-gated local
  execution behind `glue.Executor`; a VM/container executor is the next
  additive backend.

- **Loophole: the coding tool bundle is not yet Pi-class.**
  Not fixed in this ADR. `edit`, `grep`, `find`, `ls`, richer patch
  previews, and session tree UX remain follow-up coding-agent features.

- **Loophole: `cmd/glue` provider UX is still Gemini-shaped.**
  Not fixed in this ADR. The binary now proves the coding-agent surface,
  but model/provider selection still needs to move from the historical
  Gemini runner toward the providers registry used by downstream agents.

## Consequences

Glue can now be evaluated as both a framework and a coding-agent binary
without importing Peggy. Peggy remains free to focus on always-on
assistant concerns: identity, memory, channels, reminders, background
runs, notifications, and product UX.
