# ADR-0013: Pre-1.0 Stability Stance and Release Cadence

## Status

Accepted.

## Context

The framework is feature-complete for the `0.x` series and powers two
reference agents (`agents/glue-review`, `agents/peggy`). The public
`Agent` / `Session` / `Tool` surface has been stable in practice across
~30 PRs without breaking changes. We are about to publish an official
homepage and a tagged release for the first time, which forces a
decision: lock the surface and cut `v1.0.0` now, or stay pre-1.0 and
keep the right to break API on minor bumps.

The choice is between two real trade-offs:

- **Lock now (`v1.0.0`).** Strongest external commitment; every
  subsequent break would require `v2.0.0`, `v3.0.0`, etc. With no
  external users yet, an early `v1.0.0` would tie our hands for
  refactors we haven&rsquo;t even discovered we want.
- **Stay pre-1.0 (`v0.x`).** SemVer permits breaking changes on minor
  bumps in `0.x`. Lower commitment, faster iteration. The downside is
  a "we may still break things" caveat on the homepage.

## Decision

Stay on `0.x` for now. Cut **`v0.1.0`** as the first tagged release
and use minor bumps (`v0.2.0`, `v0.3.0`, …) for breaking changes,
patch bumps (`v0.1.1`, `v0.1.2`, …) for non-breaking fixes. Lock
`v1.0.0` only after a deliberate surface-review pass — not on a launch
deadline.

Concretely:

- The public `glue.Agent` / `glue.Session` / `glue.Tool` /
  `glue.Provider` surface, the `loop` package types, and the registered
  tool / provider / store packages are all considered API in the SemVer
  sense.
- Internal helpers (unexported symbols, anything inside an `internal/`
  package — none exist today but the rule reserves the right) are
  exempt.
- Every breaking change must land with a `CHANGELOG.md` entry under
  the next minor version, prefixed `**Breaking:**`, and a migration
  note. We do not break API on patch releases.
- The README, `docs/building-agents.md`, and the homepage carry one
  honest sentence on stability rather than implying production readiness.

## Loopholes and Fixes

- **Loophole: subscription-auth providers leak through the
  stability promise.** The Codex provider depends on a third-party
  auth path OpenAI does not formally document. Fixed by carrying the
  caveat on the provider package, the README, the homepage, and
  `SECURITY.md` separately from the framework-wide stability claim.

- **Loophole: "stable in practice" is not measurable.** Fixed by tying
  the stability commitment to `CHANGELOG.md` discipline: a breaking
  change without a `**Breaking:**` entry under a minor-bump section
  is a release bug, not a design choice.

- **Loophole: `v1.0.0` could be deferred forever.** Not fixed in this
  ADR. The trigger is "we have outside users whose breakage cost
  exceeds our iteration cost" plus an explicit surface-review pass.
  We will revisit when that becomes true; until then, this is the
  honest answer.

- **Loophole: tagging dilutes the contributor protocol&rsquo;s
  one-issue/one-PR rule.** Not really — tags are cut on the merge
  commit of the issue that bumps `CHANGELOG.md`, not as separate
  unowned mutations.

## Consequences

The homepage can ship today with an honest "pre-1.0" line and a real
tagged release behind `go get github.com/erain/glue@v0.1.0`. Future
breaking changes are cheap (one minor bump) but visible (one CHANGELOG
entry per break). Locking to `v1.0.0` remains a future, deliberate
choice rather than a launch-deadline accident.
