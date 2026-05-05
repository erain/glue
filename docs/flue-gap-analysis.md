# Glue vs Flue Feature Gap Analysis

Date: 2026-05-05
Reference: https://github.com/withastro/flue#readme

## Executive summary

Glue already implements a strong core harness loop in Go (agent/session API, provider abstraction, roles, skills, JSON output, event streaming, and file-backed session store support in examples/CLI). Compared to Flue, the biggest gaps are around **runtime breadth** (multi-target deployment), **sandbox architecture** (virtual + remote sandbox connectors), **task/subagent orchestration**, **MCP tooling**, and **packaging/CLI developer experience**.

## Method

- Reviewed Glue README, CLI, core loop/session APIs, and design/plan docs.
- Compared those capabilities to the top-level feature claims and examples in the Flue README.

## Capability matrix (feature-wide)

Legend:
- ✅ Implemented in Glue
- 🟡 Partial / early
- ❌ Missing

### 1) Core agent runtime

- Agent + session abstraction: ✅
- Provider-agnostic loop: ✅
- Deterministic sequential tool execution: ✅
- Prompt-level overrides (model/system/max turns): ✅

Gap vs Flue: small. Glue is already strong in this area.

### 2) Sessioning & memory

- In-memory transcript continuity: ✅
- File-backed persistence: 🟡 (supported via file store + examples/CLI, but not parity with Flue's platform-native persistence story)
- Multi-session/thread ergonomics across targets: 🟡

Gap vs Flue: moderate.

### 3) Skills, roles, project context

- AGENTS.md discovery: ✅
- `.agents/skills` discovery + `Session.Skill`: ✅
- Role hierarchy (agent/session/call): ✅

Gap vs Flue: low-to-moderate (Flue additionally emphasizes richer skill-pack workflows and role overlays across task delegation).

### 4) Structured outputs

- JSON-mode prompting + schema forwarding (provider-specific): ✅
- Type-safe result ergonomics comparable to Flue examples: 🟡 (Go typing differs from TS/valibot DX)

Gap vs Flue: low.

### 5) Tooling model (commands/shell/filesystem)

- Tool-call loop support: ✅
- First-class local/virtual sandbox model: ❌
- Privileged command adapters with explicit secret boundaries: ❌

Gap vs Flue: high.

### 6) Task/subagent orchestration

- Detached child tasks (`session.task`) sharing filesystem but isolated memory: ❌
- Agent-driven parallel delegation primitives: ❌

Gap vs Flue: high.

### 7) Runtime targets & deployment

- Local Go library + local CLI: ✅
- Build-once / deploy-anywhere targets (Node, Cloudflare, CI entry patterns): ❌
- HTTP trigger conventions + target-specific build pipeline: ❌

Gap vs Flue: high.

### 8) MCP integration

- Remote MCP server connection as tool adapter: ❌
- Transport controls (streamable HTTP/SSE): ❌

Gap vs Flue: high.

### 9) Ecosystem/connector workflow

- Connector catalog + install flow (`flue add ...` style): ❌
- Runtime adapter conventions for third-party sandboxes: ❌

Gap vs Flue: high.

### 10) CLI maturity

- Basic run command with prompt/session/store/env flags: ✅
- Full dev/build/run lifecycle akin to `flue dev`, `flue run`, `flue build`: ❌

Gap vs Flue: high.

## Gap estimate by breadth

Approximate parity by feature surface (coarse estimate):

- **Glue parity vs Flue (feature count weighted): ~45–55%**

Rationale:
- Core harness primitives are mostly present.
- Most of Flue's differentiators (multi-runtime deployment, sandbox/connectors, MCP, task orchestration) are not yet present.

## Priority roadmap to close the gap

1. **P0 → P1.5: sandbox/tool boundary model**
   - Introduce explicit sandbox interfaces (local, virtual, remote).
   - Add command adapters with scoped env/secrets.

2. **Task orchestration primitives**
   - Add `Session.Task(...)` with isolated transcript + optional shared workspace.
   - Provide parent-child event tracing.

3. **MCP adapter layer**
   - Client abstraction for remote MCP servers.
   - Normalize tools into Glue tool schema.

4. **CLI lifecycle expansion**
   - Add `glue dev`, `glue build`, and HTTP-triggerable agent entrypoints.
   - Standardize config layout similar to `.flue/agents` (Go-native equivalent).

5. **Connector/adapter ecosystem**
   - Define adapter contract and starter templates.
   - Publish 1–2 reference adapters (e.g., local shell + one remote sandbox).

## Suggested near-term success criteria

- Run the same agent in:
  1) local CLI,
  2) HTTP server mode,
  3) CI one-shot mode,
  with identical session semantics.
- Execute a delegated task from a parent session and merge summarized output.
- Connect at least one remote MCP server and complete a tool call end-to-end.
