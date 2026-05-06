# Changelog

This file tracks releases of the `glue-review` agent and its GitHub
Action. It is independent from the parent `github.com/erain/glue`
library, which versions separately.

## v1.1.0 — 2026-05-06

Backwards-compatible feature release. Existing v1.0.0 deployments
continue to work without changes.

### Added

- **Inline AI-fix prompts.** Each `[severity] path:line` entry now
  includes a `Fix: <agent prompt>` clause. The Action renders it as a
  `<details><summary>💡 AI prompt to fix</summary>` collapsible inside
  every inline review comment, so reviewers can copy the prompt
  directly into Claude / Cursor / their coding agent of choice.
  Inspired by the [fluent-bit project's review style](https://github.com/fluent/fluent-bit/pull/11778#pullrequestreview-4236889315).
- **Prompt v2** — the default prompt version is now `v2`, which
  requires the `Fix:` clause. The legacy `v1` prompt remains in
  source; pin via `--prompt-version v1` (or the matching Action input)
  to opt back in if a deployment relies on the v1 output shape.
- `InlineComment.fix` JSON field. Empty when the model produced a v1-
  style entry without a Fix clause; populated for v2+ entries.

### Unchanged

- Action input/output surface — additive only.
- Sticky comment / PR Review marker shape — the Fix collapsible is
  optional content within the existing inline-comment shape.
- v1 prompt output continues to flow through the parser and
  validator unchanged.

## v1.0.0 — 2026-05-05

First stable release. The Action's input/output surface is now
guaranteed to be backwards-compatible across the `v1.x` series.

### Added

- **Composite GitHub Action** at `agents/glue-review/action.yml`. Drop
  it in any repo with one workflow line:

  ```yaml
  - uses: erain/glue/agents/glue-review@v1
    with:
      nvidia-api-key: ${{ secrets.NVIDIA_API_KEY }}
  ```

- **Provider failover**. Pass `provider: nvidia,openrouter,gemini` to
  try each in order; the first whose API key is set in env and whose
  call succeeds wins. Soft-fails to a sticky comment when all fail.

- **Inline review comments** posted via the GitHub Pull Request
  Reviews API. Lines that don't parse cleanly fall through to a
  bulk-markdown sticky comment. Re-runs dismiss prior bot reviews so
  the diff stays uncluttered.

- **`/glue-review` comment trigger** for fork PRs, gated on
  `OWNER`/`MEMBER`/`COLLABORATOR` association so random commenters
  cannot trigger free LLM spend.

- **Path filters**. `paths` / `paths-ignore` Action inputs (and
  `--paths` / `--paths-ignore` CLI flags) restrict the diff via Git
  pathspecs at the source — out-of-scope files never reach the model.

- **Custom prompts**. `prompt` and `prompt-version` inputs let
  deployments retarget the agent ("only review SQL migrations") or
  pin a specific system-prompt revision.

- **Sensitive-file blocklist**. The `read_file` tool refuses to open
  paths matching a built-in pattern list (`.env`, `id_rsa`, `*.pem`,
  `credentials.json`, etc.). Extended via `extra-blocked-paths`.

- **Citation validation**. Inline-comment entries whose `path:line`
  is not reachable on the new side of the diff are dropped before
  the review is posted. Defends against fabricated citations — a
  real LLM failure mode we observed in pre-1.0 dev.

- **Prompt versioning**. System prompts live as `prompts/<version>.md`
  files embedded via `//go:embed`. The sticky-comment marker carries
  the version so a future prompt-shape change starts fresh comments
  instead of editing existing ones into a different format.

- **Fixture replay tests**. Three synthetic-repo scenarios
  (`panic-stub`, `subtle-bug`, `cosmetic-only`) replay through a real
  free model on every CI push to catch prompt regressions.

### Surface guarantees

What `v1.0.0` promises:

- Action inputs and outputs documented in `action.yml` will not
  break in the `v1.x` series.
- The CLI flag set on `glue-review` will not lose flags in `v1.x`.
- The sticky-comment marker shape stays stable so re-runs continue to
  edit the right comment.
- The inline-comment JSON schema stays stable.

What `v1.0.0` does NOT promise:

- Byte-stable model output. LLMs drift; we depend on them.
- Library API stability. `github.com/erain/glue` remains `0.x`.
- Free-tier provider availability. NVIDIA, OpenRouter, and their
  upstream model hosts can rate-limit or break independently;
  failover is best-effort.

### Pinning

For maximum stability:

```yaml
- uses: erain/glue/agents/glue-review@v1.0.0
```

For minor-bump auto-updates (recommended):

```yaml
- uses: erain/glue/agents/glue-review@v1
```

The `v1` floating tag advances on every backwards-compatible release.
