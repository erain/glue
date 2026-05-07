# ADR 0004: glue-review v2 — Fix-Pack Output For Coding Agents

## Status

Accepted. Implementation tracked in a follow-up issue.

## Context

`agents/glue-review` v1.x produces a human-style PR review: inline
comments on the diff (via the GitHub PR Reviews API), a sticky markdown
fallback when entries don't parse cleanly, and `Fix:` clauses inside
each comment as of v1.1.0. The output is shaped for human reviewers
who skim the comments and decide what to act on.

Two facts shifted the design space:

1. **The actionable extraction step is the only one that survives.**
   In practice, when reviewers act on a comment, they reformulate it
   into a prompt for a coding agent (Claude Code, Codex, Cursor) and
   let the agent apply the fix. The human-readable comment is a
   transient intermediate.
2. **Free models are individually unreliable at code review.** Single-
   model output has obvious failure modes: hallucinated file:line
   citations (the reason the citation validator exists in v1), missed
   real issues, and noisy "issues" that aren't issues. Running multiple
   free models and merging is cheap (free) and dramatically cuts the
   false-positive rate.

The PR-reviewer space (CodeRabbit, Greptile, etc.) is crowded.
"Fix-pack generator for coding agents" is not. The pivot is also a
positioning decision.

## Decision

`glue-review` v2 emits a single artifact: `.glue-review-fixes.json` —
a queue of agent-runnable prompts produced by multiple free models and
merged by a synthesizer pass. Inline review comments, sticky markdown,
and the PR Reviews API integration are removed from the agent.

### Output contract: the fix-pack

A fix-pack is one JSON file. Schema version `"1"` is defined as:

```json
{
  "version": "1",
  "head_sha": "abc1234...",
  "base_ref": "main",
  "models": ["deepseek-r1", "qwen3-coder", "kimi-k2"],
  "synthesizer": "deepseek-r1",
  "generated_at": "2026-05-07T12:34:56Z",
  "fixes": [
    {
      "id": "fix-001",
      "severity": "major",
      "path": "src/handler.go",
      "line": 142,
      "issue": "off-by-one in pagination loop",
      "agent_prompt": "In src/handler.go around line 142, the loop iterates `for i := 0; i <= n; i++` but the function contract is to process exactly n items. Change the bound to `i < n`. Add a unit test that calls the function with n=3 and asserts it processes 3 items, not 4.",
      "models_flagged": ["deepseek-r1", "qwen3-coder"],
      "confidence": 0.66
    }
  ],
  "dropped_singletons": 4,
  "stats": {
    "models_succeeded": 3,
    "models_failed": 0,
    "raw_issues": 11,
    "after_dedup": 7,
    "after_singleton_drop": 3
  }
}
```

Field semantics:

- `version` — schema version. Bumps on backwards-incompatible changes.
- `head_sha` / `base_ref` — pin the diff this pack was generated against.
- `models` — list of model ids that ran. Order is the orchestration
  order, not preference.
- `synthesizer` — model id that performed the merge/dedupe/judge pass.
  May appear twice (e.g. once in `models` if it also reviewed
  primary, once here).
- `fixes[].id` — stable within a pack; `fix-001`, `fix-002`, … in
  emission order. Not stable across packs.
- `fixes[].severity` — `critical` | `major` | `minor` | `info`. The
  synthesizer normalizes input severities to this enum.
- `fixes[].path` / `fixes[].line` — file path relative to repo root,
  1-indexed line on the new side of the diff.
- `fixes[].issue` — one-sentence summary, agent- and human-readable.
- `fixes[].agent_prompt` — self-contained instruction that a coding
  agent can execute without reading the rest of the pack. Includes
  the file, the line, the *what* and *why*, and a concrete change.
  Required to be self-contained because consumers iterate the array
  and feed each prompt independently.
- `fixes[].models_flagged` — which primary models flagged this issue
  (after dedup). Length ≥ 2 in the default policy (singletons dropped);
  exposed so downstream consumers can re-tune.
- `fixes[].confidence` — `len(models_flagged) / len(models)`. A
  derived field; included for ergonomics so consumers don't compute
  it.
- `dropped_singletons` — count of issues flagged by exactly one model
  that were dropped from `fixes`. Surfaces the false-positive
  reduction the policy bought.
- `stats` — orchestration telemetry. Useful for tuning model lineup
  later.

The artifact is the contract. There is no markdown rendering, no PR
comment, no inline review.

### Multi-model orchestration

```
diff →
  fan-out (parallel, single attempt each):
    • DeepSeek-R1 (deepseek/deepseek-r1:free) — reasoning depth
    • Qwen3 Coder (qwen/qwen3-coder:free) — line-level correctness
    • Kimi K2 (moonshotai/kimi-k2:free) — long context for deep reads
  →
  synthesizer pass:
    • DeepSeek-R1 acts as judge
    • input: each model's raw output + the original diff
    • prompt skeleton in §Synthesizer prompt below
    • output: the deduplicated, severity-normalized, singleton-dropped fix list
  →
  fix-pack JSON
```

Failure handling:

- Each primary model gets one attempt. On HTTP 429 / 5xx / context
  cancellation, the model is dropped from this run. The pack records
  `models_succeeded` and `models_failed` so the consumer sees
  degradation.
- If ≥ 2 models succeed, the synthesizer runs and the pack is
  emitted normally (singletons can still be dropped because there are
  ≥ 2 reviewers).
- If exactly 1 model succeeds, the synthesizer still runs (acts as a
  formatter / cleaner) but `dropped_singletons` is set to the full
  raw count and `fixes` is empty. The job exits non-zero so CI sees
  the run as degraded.
- If 0 models succeed, the job exits non-zero with no artifact.

Models run in parallel because the cold-start latency of free routes
(Kimi can take 30s+ for first byte; R1 reasoning is slow) makes serial
infeasible. Each model gets its own context with the same system
prompt and the same tool inventory (diff, log, read_file). Synthesizer
runs after all primaries return.

### Synthesizer policy

The synthesizer drops issues flagged by only one primary model.
Rationale: false positives are the dominant failure mode of free
models on code review, and consensus-of-2 is the cheapest way to cut
them. The schema retains `models_flagged` and `dropped_singletons` so
a downstream consumer can re-tune (e.g. "I want recall, give me
singletons too" → re-run with `--keep-singletons`, future flag).

The synthesizer is also responsible for:

- **Dedup**: clustering issues that point at the same path:line
  (±2 lines) with semantically similar prose. Uses model judgment;
  no fuzzy-match heuristic.
- **Severity normalization**: input severities vary
  (`high|critical|important|warn|...`); output is the enum
  `critical|major|minor|info`.
- **Prompt strengthening**: each surviving issue's `agent_prompt` is
  rewritten to be self-contained — the consumer iterates the array
  and pipes each prompt independently into a coding agent without
  reading siblings.
- **Citation rejection**: if a clustered issue's path:line cannot be
  matched against the diff (the v1 citation-validator's job), the
  synthesizer is instructed to drop it. We rely on the model to
  enforce this rather than a Go validator; the validator can be
  added back as a post-pass if accuracy regresses.

### Synthesizer prompt skeleton

```
You are merging N code reviews of the same Git diff into a deduplicated,
prioritized fix-pack for downstream coding agents.

INPUTS:
- The original diff: <diff>
- N reviewer outputs, each as a list of (severity, path, line, issue, fix-prompt) entries.

OUTPUT (JSON only, no prose):
{
  "fixes": [
    {
      "severity": "critical|major|minor|info",
      "path": "...",
      "line": 0,
      "issue": "one-sentence summary",
      "agent_prompt": "self-contained instruction including path, line, what, why, and the concrete change",
      "models_flagged": ["model-id", "model-id"]
    }
  ],
  "dropped_singletons": 0
}

RULES:
1. Cluster entries that point at the same file:line (±2 lines) with semantically similar prose. Treat them as one issue; merge their reviewer ids into models_flagged.
2. After clustering, DROP any issue flagged by fewer than 2 distinct reviewers. Increment dropped_singletons for each dropped issue.
3. Normalize severity to the four-value enum.
4. Rewrite agent_prompt to be self-contained: it must name the file, the line, the *what* (the bug), the *why* (the contract / invariant being violated), and the *concrete change* (what edit applies the fix). A coding agent must be able to execute it without seeing other entries.
5. If an entry's path:line is not present in the diff (line not part of the new side), drop it silently.
6. Output JSON only. No commentary.
```

### What gets removed from v1

- `parse.go` + `parse_test.go` — inline-comment parsing.
- `validate.go` + `validate_test.go` + `pathspec_test.go` — diff-line
  citation validation. Replaced by the synthesizer's rule 5.
- `failover.go` test + the failover loop in `main.go` — superseded by
  per-model fan-out with drop-on-failure.
- `prompts/v1.md` + `prompts/v2.md` — the markdown-shaped review
  prompts. Replaced by a fix-pack-shaped primary prompt and the
  synthesizer prompt above.
- Sticky-markdown-comment step and PR Reviews API call in `action.yml`.

### What stays

- `tools.go` — diff, log, read_file. Still needed.
- `blocklist.go` + `blocklist_test.go` — sensitive-file refusal still
  matters.
- `fixture_test.go` — adapted to assert fix-pack shape (and that the
  drop-singletons policy actually fires on each fixture).
- `prompts/` directory — repopulated with v3 primary + v3 synthesizer
  prompts.

### GitHub Action shape

- Inputs: `models` (CSV, default
  `deepseek-r1,qwen3-coder,kimi-k2`), `synthesizer` (default
  `deepseek-r1`), `openrouter-api-key`, `output-path` (default
  `.glue-review-fixes.json`), `paths` / `paths-ignore` /
  `extra-blocked-paths` (kept from v1).
- Step 1: run the agent. Writes `output-path`.
- Step 2: `actions/upload-artifact` with the JSON.
- Step 3: write a one-line summary to `$GITHUB_STEP_SUMMARY`:
  `"glue-review: 3 fixes (5 dropped as singletons; models: 3/3 succeeded). Download: glue-review-fixes.json"`.
- No PR comment. No inline review. The PR-author breadcrumb is the
  Action job summary, which is visible without opening the PR
  comments tab.

`@v1` floating tag stays pinned to the existing v1.1.0 release.
`@v2` is the new floating tag. The v1 source remains in git history;
we do not remove it from the v1 tagged releases. New users adopt
`@v2`; existing v1 users are unaffected until they explicitly bump.

### Migration plan

- `agents/glue-review/CHANGELOG.md` gains a `v2.0.0` entry that
  enumerates the breaking changes and points at this ADR.
- README's `agents/glue-review` blurb is rewritten for v2.
- A short `MIGRATION.md` in the agent directory shows the v1→v2
  workflow diff (one yaml block before/after).

## Consequences

Positive:

- Sharper product story: "fix-pack generator for coding agents,"
  not "yet another PR review bot."
- False-positive rate drops because singletons get dropped.
- Removes ~470 lines of inline-comment parsing/validation that exist
  only because v1 needed to post inline comments. Code base shrinks.
- Schema-first: downstream tooling (auto-fix runners, dashboards,
  follow-up agents) can integrate against a stable JSON contract.

Negative:

- Hard break for v1 consumers. Mitigated by `@v1` pinning and a
  documented migration.
- Latency increases. Three parallel free models (Kimi cold-start
  ~30s) plus a synthesizer pass means review wall-clock can run
  60–90s. Acceptable given the quality-over-speed call.
- Loses the "human reviewer reads inline comments and acts on them
  manually" workflow. Users who want that stay on `@v1`.
- We give up the v1 citation-validator (Go-side) and rely on the
  synthesizer to enforce path:line presence. If accuracy regresses
  we re-add a Go post-pass.

## Out of scope for v2.0.0

- Auto-applying fixes. The fix-pack is the output; running the
  prompts is a different agent's job. A separate `glue-fix` agent
  may follow.
- Markdown rendering of the pack. Downstream tooling can render if
  needed.
- Schema validation in the `glue` core. The fix-pack schema is
  agent-local for now.
- Tunable singleton policy. Fixed at "drop singletons" for v2.0.0;
  a `--keep-singletons` flag is a v2.1 candidate.
- Cost/quota tracking. OpenRouter free routes are unmetered enough
  not to need it; revisit if we move off free tier.

## Implementation notes

The implementation is filed as a single big-bang issue rather than
split into 4–5 mini-issues. Rationale:

- The pieces are interlocked: removing inline-comment parsing
  presumes the synthesizer is producing the new format; the
  synthesizer presumes the new prompts; the prompts presume the
  fix-pack schema. Splitting them produces multiple "half-shipped"
  states on `main`.
- The user has explicitly authorized the rewrite as a single big
  change.
- v1 stays accessible via `@v1` regardless of when v2 lands, so the
  blast radius of the big-bang PR is contained.
