# Glue CLI, TUI & Harness Guide

`cmd/glue` packages the `github.com/erain/glue` framework into a single static Go binary for local terminal coding, headless automation, and remote execution on [AX](https://github.com/google/ax). For a 5-minute hands-on walkthrough of remote AX jobs, start with [remote-quickstart.md](remote-quickstart.md).

## Command Overview

```sh
go install github.com/erain/glue/cmd/glue@latest
```

| Command | Purpose |
| :--- | :--- |
| `glue` / `glue code [flags]` | Start the interactive coding TUI in the current directory (`--continue`, `--select`, `--model`, `--prompt`). |
| `glue run --prompt "<text>" [flags]` | Run a one-shot or piped prompt (`--coding` enables workspace tools; `--mode json` emits JSONL events). |
| `glue goal "<objective>" [flags]` | Run the autonomous `plan → maker → checker` goal loop locally (`--coding`, `--worktree`, `--resume`, `--list`). |
| `glue --remote "<objective>" [flags]` | Dispatch a remote coding job directly to an AX gVisor sandbox (`-i` interactive, `--snapshot`, `--verify`, `--attach`). |
| `glue job init` | Run `doctor` checks and pair phone `ntfy` notifications via QR code (`~/.config/glue/config.yaml`, `0600`). |
| `glue remote <setup\|status\|down>` | Deploy, inspect, or remove the opt-in cluster `glue-gateway` with Cloudflare Access (`0` idle AX workers). |
| `glue job ui [--open] [--tunnel]` | Open the local browser UI with a `#token` magic link (and optional `cloudflared` HTTPS tunnel + phone QR). |
| `glue job <list\|show\|attach\|logs\|result>` | Inspect status without waking suspended AX tasks, or stream live events (`Ctrl-C` detaches cleanly). |
| `glue job <say\|answer\|approve\|deny>` | Steer a running maker (`--interrupt` cancels the active turn) or unblock a question/permission. |
| `glue job <diff\|pull\|pr\|teleport>` | Inspect unified diff, pull branch/patch (`--apply`/`--checkout`), open a Draft PR, or teleport code + context into the local TUI. |
| `glue job <compare\|pick\|continue\|prune>` | Compare `--attempts N` runs, promote a winner, address PR review feedback (`--from-pr`), or clean up old sandboxes. |
| `glue models [--provider <name>] [--json]` | List providers, curated models, defaults, and verified thinking levels offline. |
| `glue serve` / `glue connect` | Run or attach to a local HTTP+SSE daemon (`--coding`, `--jobs`). |

## Interactive Coding TUI

Running bare `glue` in a terminal opens the Bubble Tea coding TUI ([ADR-0014](adr/0014-coding-agent-tui.md), [ADR-0015](adr/0015-session-tree.md)): streaming Markdown replies, live tool-call cards with inline `[a]llow / [s]ession / [t]his-file / [n]o` prompts, surgical `edit_file` diff previews, and a status bar tracking active goals and remote AX jobs (`⧗ N remote jobs · M waiting`).

### Keybindings & File Mentions

- **`Enter`** sends the message; **`Ctrl+J`** inserts a newline across all terminals.
- **`@` / `@<path>`** fuzzy-searches workspace files inline and embeds file contents (`@"path with space"` for spaces; `.env`, SSH keys, and secrets are blocklisted).
- **`Esc`** cancels the active turn; **`Ctrl+C`** cancels once and quits on a second press.
- **`Ctrl+Y`** copies the last assistant reply's raw Markdown via OSC 52 (works over SSH without external clipboard tools).
- **`PgUp` / `PgDn`** or mouse wheel scrolls the transcript with sticky-bottom auto-scroll.

### Slash Commands

| Command | Action |
| :--- | :--- |
| `/goal [-w] <objective>` | Pursue a goal in the background (`-w` isolates in `.glue/worktrees/<id>` on branch `goal/<id>`). Subcommands: `status`, `pause`, `resume`, `list`, `clear`. |
| `/remote [--snapshot] <objective>` | Hand off the task to an AX sandbox with your current session summary as context (`--notes`). |
| `/jobs` | Open interactive Mission Control (refreshed every 10 s without waking suspended AX tasks). `Enter` opens live transcript: `d` diff, `1`–`9`/`a` answer, `y`/`s`/`n` permission, `s` say, `i` interrupt, `o` draft PR, `p` branch pull, `A` `--apply`, `t` teleport, `c` cancel. |
| `/teleport [<id>]` | Pull a remote job's git changes and redacted `[user, assistant]` handoff briefing into `.glue/sessions` and switch to it. |
| `/compact` | Summarize older messages with the token-aware compactor to free context window while preserving a cumulative file ledger. |
| `/fork [N]` · `/clone` · `/tree` | Branch from message `N` (default: before last user turn), duplicate the session, or switch branches in a visual `├─ / └─` lineage picker. |
| `/resume` · `/session [id]` · `/new` | Pick and replay a stored session, switch by ID, or start a fresh transcript (`/clear`). |
| `/model <id>` · `/tools` · `/usage` | Switch model for the session, inspect active tools, or view token usage; `/copy [N]` copies reply `N` as raw Markdown. |

## Headless Runs, Daemon & Local Goal Loop

```sh
# One-shot coding run or piped diff review:
glue code --provider codex --prompt "Fix the failing test."
git diff main | glue run --prompt "Review this diff for bugs."
glue run --coding --mode json --prompt "Run go test ./..." | jq -r '.type'

# Isolated worktree goal (exit codes: 0 achieved · 2 blocked · 3 max-iterations · 4 budget · 1 error):
glue goal --coding --yolo --worktree "Make golangci-lint pass on ./..."
glue goal --list && glue goal --resume

# Local HTTP+SSE daemon:
glue serve --coding --state-dir .glue/state --listen 127.0.0.1:8787 --token "$GLUE_TOKEN"
```

Coding jobs and `glue serve` default to `--policy auto` (with opt-in `workspace-auto`, `ask-all`, `yolo`, and `--policy-file`; see [policies.md](policies.md)). Goal progress checkpoints to the session store after every iteration ([ADR-0016](adr/0016-goal-loop.md)).

## Models, Providers & Thinking Levels

Defaults depend on activity: `glue`, `glue code`, `--coding`, `glue goal`, and `glue --remote` default to `gemini/gemini-3-flash-preview` (`thinking: low`); non-coding `glue run` and `glue serve` default to `openrouter/openrouter/free`. Override per run with `--provider`, `--model`, and `--thinking`, interactively with `--select`, or persistently in `~/.config/glue/config.yaml`.

```sh
glue models                                  # inspect curated models & thinking levels offline
glue run --select --coding                   # interactive arrow-key picker on stderr
glue goal --provider gemini --model gemini-4-argon --thinking high "Audit concurrency in jobs/"
glue run --provider deepseek --thinking low --coding --prompt "Find the entry point"
```

- **Gemini (`GEMINI_API_KEY`):** Supports `thinking_level` (`low`, `medium`, `high` on Gemini 3/4) with thought-signature round-tripping.
- **Codex (`codex login`):** Bills against a personal ChatGPT subscription via `~/.codex/auth.json` (`$GLUE_CODEX_AUTH` / `$CODEX_HOME`; [ADR-0006](adr/0006-codex-provider.md)).
- **DeepSeek (`DEEPSEEK_API_KEY`):** Defaults to `deepseek-flash` (`off`, `low`, `high`, `max` thinking) and replays streamed reasoning across tool calls.
- **NVIDIA (`NVIDIA_API_KEY`) & OpenRouter (`OPENROUTER_API_KEY`):** OpenAI-compatible hosting for Kimi K2, Qwen, GLM, Llama, and `openrouter/free`.

## Coding Tools & Harness Reliability

`tools/coding.Tools(...)` provides `read_file`, `write_file`, `edit_file`, `list_dir`, `find_files`, `grep`, `shell_exec`, `git_diff_branch`, and `git_log_branch` over `glue.Executor` ([ADR-0012](adr/0012-sdk-coding-agent-peggy-boundary.md)):

- **Self-repairing `edit_file`:** Resolves whitespace, indentation (re-indenting the replacement to match the file), smart quotes/dashes, over-escaping, and block anchors deterministically, then echoes updated lines so the model does not re-read the file.
- **Head + tail `shell_exec` spooling:** Retains both the initial compiler error and final exit summary on long outputs while spooling the full stream to a named temp file.
- **Active-toolset prompts:** `coding.SystemPrompt` builds instructions directly from registered tools (`terse` for frontier models, `explicit` for open-weight models) so prompts never drift from available tools.
- **Loop hardening ([ADR-0017](adr/0017-loop-retry-overflow-recovery.md)):** Every turn runs `loop.HardenHistory` (synthesizing error results for interrupted tool calls and stripping foreign thinking signatures), retries transient 429/5xx drops using `Retry-After` hints, compacts once on `*loop.OverflowError`, nudges narrated stalls (`AutoContinue`), and stops repetitive tool loops via `RunRequest.Guardrails`.

### Standard CLI Flags for Custom Agent Binaries

`cli.RegisterStandardFlags` wires `--provider`, `--model`, `--id`, `--store`, `--work`, and `--max-turns` onto any Go `flag.FlagSet`:

```go
fs := flag.NewFlagSet("my-agent", flag.ContinueOnError)
get := cli.RegisterStandardFlags(fs, nil)
_ = fs.Parse(os.Args[1:])
cfg := get()
```
