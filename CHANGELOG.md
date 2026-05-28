# Changelog

The library at `github.com/erain/glue` is **pre-1.0**; the `0.x`
series may break API on minor bumps. See
[`docs/adr/0013-pre-1-0-stability-stance.md`](docs/adr/0013-pre-1-0-stability-stance.md)
for the policy, and pin a tag in your `go.mod` if you need stability.
Breaking changes always land with a `**Breaking:**` entry under a
minor-bump section — never on a patch release.

This file tracks library-level changes. The reference agents version
independently:
[`agents/glue-review`](agents/glue-review/README.md) (release notes in
its [GitHub Releases](https://github.com/erain/glue/releases?q=agents%2Fglue-review)),
and [`agents/peggy/CHANGELOG.md`](agents/peggy/CHANGELOG.md).

## Unreleased

_(no changes yet)_

## 0.1.0 — 2026-05-27

First tagged release. Brings the framework to launch shape and stabilizes
the public surface for `go get github.com/erain/glue@v0.1.0`.

### Added (M7 dual-track surface)

- `tools/coding`, a reusable SDK package that assembles the local
  coding-agent tool bundle (`read_file`, `write_file`, `edit_file`,
  `list_dir`, `find_files`, `grep`, `shell_exec`, `git_diff_branch`,
  `git_log_branch`) over the existing `tools/fs`, `tools/git`,
  `tools/shell`, and `glue.Executor` primitives.
- `cmd/glue` coding-agent mode: `glue run --coding` and
  `glue serve --coding` register the SDK coding bundle, with local
  terminal permission prompts for one-shot runs and daemon-brokered
  permissions for served runs.
- `cmd/glue --provider`: `run` and `serve` select any registered
  provider (`codex`, `gemini`, `nvidia`, `openrouter`) through the
  `providers` registry instead of being hardwired to Gemini, so the
  binary can run as a coding agent on a ChatGPT subscription
  (`glue run --provider codex --coding`). `--model` defaults to the
  selected provider's registry default model.
- `tools/fs.FileEdit` (`edit_file`), a permission-gated surgical
  exact-string replacement tool with a unique-match guard and optional
  `replace_all`.
- Read-only navigation tools `tools/fs.ListDirTool` (`list_dir`),
  `FindTool` (`find_files`), and `GrepTool` (`grep`).
  Workspace-scoped and escape-safe; `grep` skips secret-shaped
  (Blocklist) and oversized files, and all three skip `.git`.

### Public surface present at this tag

For completeness, this first tagged release also stabilizes everything
shipped during the bootstrap and the long-running foundation
(ADR-0005). The full surface — `Agent` / `Session` / `Tool` /
`Provider` types, the `loop` package, the four providers, both stores
(`stores/file`, `stores/sqlite` with FTS5 search), every `tools/*`
package, subagents (`glue.SubagentTool`), skills/roles/AGENTS.md,
structured JSON, opt-in parallel tool execution, the `Compactor`
interface and `SummarizingCompactor`, the `prompts` versioned-prompt
catalog, the `cli` standard-flags helper, and the `cmd/glue`
`run` / `serve` / `connect` daemon protocol — is documented in
[`README.md`](README.md), [`docs/building-agents.md`](docs/building-agents.md),
and [`docs/design.md`](docs/design.md).

### Notes

- The Codex provider authenticates via `codex login` (subscription
  auth path OpenAI does not formally document). Intended for personal
  use; see [`SECURITY.md`](SECURITY.md) for the scope statement.
- The local executor is permission-gated, not sandboxed. Implement
  `glue.Executor` against a container/VM if you need isolation
  ([ADR-0009](docs/adr/0009-executor-permission-hook.md)).

## Initial bootstrap (pre-0.1.0)

The library was under active development as a Go agent harness
inspired by [pi-mono](https://github.com/badlogic/pi-mono) and
[Flue](https://github.com/withastro/flue) before this first tag. The
detailed history lives in the git log; the surface that survived into
`v0.1.0` is listed above under "Public surface present at this tag."
