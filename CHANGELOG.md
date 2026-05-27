# Changelog

The library at `github.com/erain/glue` remains **pre-1.0**; the `0.x`
series may break API on minor bumps. The PR-review agent at
`agents/glue-review` ships with its own version line and is **1.0**;
see [`agents/glue-review/CHANGELOG.md`](agents/glue-review/CHANGELOG.md)
for its release notes.

This file tracks library-level changes only.

## Unreleased

- Added `tools/coding`, a reusable SDK package that assembles the local
  coding-agent tool bundle (`read_file`, `write_file`, `shell_exec`,
  `git_diff_branch`, and `git_log_branch`) over the existing filesystem,
  shell, git, and `glue.Executor` primitives.
- Added `cmd/glue` coding-agent mode: `glue run --coding` and
  `glue serve --coding` now register the SDK coding bundle, with local
  terminal permission prompts for one-shot runs and daemon-brokered
  permissions for served runs.
- Added `cmd/glue --provider`: `run` and `serve` now select any
  registered provider (`codex`, `gemini`, `nvidia`, `openrouter`) through
  the `providers` registry instead of being hardwired to Gemini, so the
  binary can run as a coding agent on a ChatGPT subscription
  (`glue run --provider codex --coding`). `--model` now defaults to the
  selected provider's registry default model.

## Initial bootstrap (pre-1.0)

The library has been under active development as a Go agent harness
inspired by [pi-mono](https://github.com/badlogic/pi-mono) and
[Flue](https://github.com/withastro/flue). Notable shipped surface:

- `Agent` / `Session` public API; file-backed session store at
  `stores/file`.
- Reusable provider-agnostic loop in `loop/`.
- Providers: `gemini`, `nvidia`, `openrouter`, with shared
  OpenAI-compatible plumbing in `providers/openaicompat`.
- Skills, roles, structured JSON output, parallel tool execution.

The library will cut `v1.0.0` once the public API is settled. For now
the agent's stability does not require library stability — the agent
absorbs library bumps internally.
