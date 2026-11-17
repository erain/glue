# glue

[![CI](https://github.com/erain/glue/actions/workflows/ci.yml/badge.svg)](https://github.com/erain/glue/actions/workflows/ci.yml)

**Glue is an AX-first, remote-first coding agent and composable Go agent framework.**

- **Coding agent (`cmd/glue`):** Code interactively in a terminal TUI, or dispatch uncommitted work directly into isolated [AX](https://github.com/google/ax) gVisor sandboxes in ~3 seconds — steer from phone lock-screen buttons at `0 CPU`, open a Zero-Trust Cloudflare Access browser workspace, and teleport tested code + context back into your checkout. → <https://glue-coding.vercel.app>
- **Go agent framework (`github.com/erain/glue`):** A small, code-first `Agent` / `Session` / `Tool` SDK across five model providers with context compaction, MCP client support, and pure-Go SQLite FTS5 search. → <https://glue-framework-site.vercel.app>

## Install

```sh
# Coding agent binary:
go install github.com/erain/glue/cmd/glue@latest

# Go SDK:
go get github.com/erain/glue
```

| Package | Purpose |
| :--- | :--- |
| `github.com/erain/glue` | Core public SDK: `Agent`, `Session`, `Tool`, `Store`, `PursueGoal`, failover, subagents. |
| `.../loop` | Provider-agnostic streaming & tool-execution loop with history hardening and retries. |
| `.../providers/{gemini,codex,deepseek,nvidia,openrouter}` | Model adapters + capability registry (`providers.New`, shared `openaicompat` core). |
| `.../stores/{file,sqlite}` | Transcript persistence (`sqlite` adds pure-Go BM25 FTS5 cross-session search). |
| `.../tools/{fs,git,shell,coding,mcp}` | Workspace tools, self-repairing `edit_file`, head+tail `shell_exec`, and MCP client. |
| `.../prompts` · `.../cli` | Versioned `embed.FS` prompt catalog and standard CLI flags for custom agent binaries. |

## The `glue` CLI

Start with the **5-minute walkthrough → [docs/remote-quickstart.md](docs/remote-quickstart.md)**, or see the full command, TUI, and flag reference in **[docs/cli.md](docs/cli.md)**.

### Remote on AX (close the laptop, ship verified PRs)

```sh
glue job init                         # doctor preflight + QR phone pairing (~/.config/glue/config.yaml)
glue --remote "Add bounded jitter & race tests" \
  --snapshot --verify "go test -race ./..."
glue job attach latest                # live resumable transcript; Ctrl-C detaches cleanly
glue job teleport latest              # pulls patch + session briefing into your local coding TUI
```

- **Zero mandatory gateway ([ADR-0020](docs/adr/0020-ax-first-direct-execution.md)):** `glue --remote` connects directly to AX over ephemeral mTLS and pushes model/GitHub tokens into memory (`POST /v1/credentials`). `--snapshot` packages uncommitted edits safely instead of silently cloning stale code.
- **Two-way lock-screen control:** When a job calls `ask_user` or requests permission, AX suspends the sandbox to `0 CPU` and pushes native HTTP `POST` action buttons to `ntfy` on your phone.
- **Zero-Trust browser workspace ([ADR-0021](docs/adr/0021-opt-in-cluster-gateway-cloudflare-access.md)):** Run `glue remote setup` once to deploy a 1-pod `glue-gateway` + `cloudflared` behind **Cloudflare Access** (`0` idle AX workers, no OAuth app, no second login) — chat in `#/new`, escalate via **Run Autonomously**, review the unified diff, and click **Open Draft PR** (or run `glue job ui --open --tunnel` locally).
- **Parallel attempts, issues & PR rounds:** `--attempts N` + `glue job compare`/`pick`, `--issue <n>`, `glue job pr`, `glue job continue --from-pr`, `glue job say --interrupt`, and repo-wide `.glue/remote.yaml` ([docs/jobs.md](docs/jobs.md)).

### Local TUI, autonomous goals & headless runs

```sh
glue                                  # interactive coding TUI (/remote, /jobs, /teleport, /goal, @file)
glue goal --coding --worktree "Make golangci-lint pass on ./..."
git diff main | glue run --prompt "Review this diff for bugs."
```

## Quickstart

Build a tool-calling agent in Go (full step-by-step tutorial in **[docs/building-agents.md](docs/building-agents.md)**; runnable ~100-line demo in [`examples/local-agent`](examples/local-agent)):

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/erain/glue"
	"github.com/erain/glue/providers/gemini"
)

type cityArgs struct {
	City string `json:"city"`
}

func main() {
	ctx := context.Background()
	weather := glue.NewTool[cityArgs](
		glue.ToolSpec{
			Name:        "weather",
			Description: "Look up current weather for a city.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
		},
		func(_ context.Context, a cityArgs) (glue.ToolResult, error) {
			return glue.TextResult("18C and sunny in " + a.City), nil
		},
	)

	agent := glue.NewAgent(glue.AgentOptions{
		Provider: gemini.New(gemini.Options{}),
		Model:    "gemini-3-flash-preview",
		Tools:    []glue.Tool{weather},
	})
	session, err := agent.Session(ctx, "demo")
	if err != nil {
		log.Fatal(err)
	}
	res, err := session.Prompt(ctx, "What's the weather in Toronto?")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.Text)
}
```

## Concepts

| Type | What it is |
| :--- | :--- |
| **`Provider`** | Model backend streaming assistant deltas, thinking, and tool calls. |
| **`Agent`** | Configured unit (`glue.NewAgent`): provider, model, tools, store, `WorkDir`, compactor, roles. |
| **`Session`** | Named conversation transcript driven with `Prompt`, `PromptJSON`, `Skill`, or `Subscribe`. |
| **`Tool`** | Typed function (`glue.NewTool[Args]`), delegated `SubagentTool`, or MCP server tool (`tools/mcp`). |
| **`Store`** | Optional session persistence (`stores/file` JSON or `stores/sqlite` with BM25 FTS5 search). |
| **`loop`** | Provider-agnostic engine with transcript repair, classified retries, overflow compaction, and guardrails. |

## Providers

Run `glue models` to inspect curated defaults and verified thinking levels offline, or see [docs/provider-guide.md](docs/provider-guide.md) to implement a custom provider.

| Provider | Import | Auth | Notes |
| :--- | :--- | :--- | :--- |
| **Gemini** | `providers/gemini` | `GEMINI_API_KEY` | Coding default (`gemini-3-flash-preview` at `low`); Gemini 3/4 thinking levels. |
| **Codex** | `providers/codex` | `codex login` | Uses your ChatGPT subscription (`~/.codex/auth.json`, personal use). |
| **DeepSeek** | `providers/deepseek` | `DEEPSEEK_API_KEY` | `deepseek-flash` default (`off`/`low`/`high`/`max` thinking + reasoning replay). |
| **NVIDIA** | `providers/nvidia` | `NVIDIA_API_KEY` | OpenAI-compatible open-weight endpoints (`org/name`). |
| **OpenRouter** | `providers/openrouter` | `OPENROUTER_API_KEY` | Open-weight aggregator; `openrouter/free` auto-picks a free model. |

## Coding tools

`tools/coding.Tools(...)` bundles `read_file`, `write_file`, `edit_file` (with deterministic whitespace/indentation/quote match repair), `list_dir`, `find_files`, `grep`, `shell_exec` (head + tail output retention with full stream spooling), and git helpers behind `glue.Executor` and permission hooks. Details in [docs/cli.md](docs/cli.md#coding-tools--harness-reliability).

## Reference agents

- **[`agents/glue-review`](agents/glue-review)** — Zero-cost pre-push branch reviewer (CLI and GitHub Action) that posts one sticky PR comment with a copy-pasteable fix block (<https://glue-review.vercel.app>).

## Documentation

Full index in **[docs/README.md](docs/README.md)**:

- **Coding Agent:** [Remote Quickstart](docs/remote-quickstart.md) · [CLI, TUI & Local Goals](docs/cli.md) · [Remote Jobs & `.glue/remote.yaml`](docs/jobs.md) · [Permission Policies](docs/policies.md) · [Cloudflare Access Gateway Runbook](docs/runbooks/public-gateway.md)
- **Go SDK & Architecture:** [Building Agents](docs/building-agents.md) · [Adding a Provider](docs/provider-guide.md) · [Architecture & Internals](docs/design.md) · [ADRs](docs/adr)
- **Project & Governance:** [Project Plan](docs/project-plan.md) · [Contributing](CONTRIBUTING.md) · [Changelog](CHANGELOG.md) · [Security](SECURITY.md)

## Run the tests

```sh
go build ./...
go vet ./...
go test ./...
```

Pre-1.0 stability policy ([ADR-0013](docs/adr/0013-pre-1-0-stability-stance.md)): minor versions may evolve library APIs until an explicit surface lock, with every breaking change documented in [`CHANGELOG.md`](CHANGELOG.md).
