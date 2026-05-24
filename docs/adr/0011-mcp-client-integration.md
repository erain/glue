# ADR 0011: MCP Client Integration For Peggy

## Status

Accepted. Filed under tracker
[#110](https://github.com/erain/glue/issues/110) / issue
[#177](https://github.com/erain/glue/issues/177) as the first M4 design
issue for Peggy v0.4 ("ecosystem").

Implementation issues that follow this ADR:

- `tools/mcp`: JSON-RPC client, lifecycle, and stdio transport.
- `tools/mcp`: expose MCP server tools as `glue.Tool` values.
- `agents/peggy`: settings and registration for configured MCP servers.
- `tools/mcp`: Streamable HTTP transport with explicit auth config.
- Peggy documentation and smoke coverage for MCP-backed tools.

Spec baseline: official Model Context Protocol specification version
2025-11-25, checked 2026-05-24:

- <https://modelcontextprotocol.io/specification/2025-11-25/basic>
- <https://modelcontextprotocol.io/specification/2025-11-25/basic/lifecycle>
- <https://modelcontextprotocol.io/specification/2025-11-25/basic/transports>
- <https://modelcontextprotocol.io/specification/2025-11-25/server/tools>
- <https://modelcontextprotocol.io/specification/2025-11-25/basic/authorization>

## Context

Peggy v0.3 can run as one local daemon shared by terminal and Telegram
clients. She can remember, search sessions, read and write a trusted
workspace, run allowlisted commands, and gate side effects through
channel-owned permission UX.

M4 starts the ecosystem layer. Hermes-Agent's tools-as-RPC direction and
the broader MCP ecosystem point to the same product need: Peggy should be
able to use external tool servers without each integration becoming a
bespoke Go package. MCP is the obvious protocol to consume first because
it standardizes server-discovered tools, JSON Schemas, process lifecycle,
and remote transports.

ADR-0005's purity rule still controls the design:

> Every product concern enters glue only as an interface the host fills
> in, never as a default behavior with UI, channel, or policy baked in.

MCP therefore cannot become a default behavior in core `glue`. It is an
extension package that produces ordinary `glue.Tool` values. Peggy
chooses which servers to configure, how to display or classify
permission, and which channels may call those tools.

The relevant MCP spec facts for this ADR:

- MCP uses JSON-RPC 2.0 messages and lifecycle negotiation.
- Standard transports are stdio and Streamable HTTP. Stdio is the
  local-first path; Streamable HTTP replaces the older HTTP+SSE
  transport.
- Stdio clients launch the MCP server as a subprocess. JSON-RPC messages
  travel over stdin/stdout and are newline delimited; stderr is logging,
  not protocol.
- Streamable HTTP uses one MCP endpoint that accepts POST and may return
  JSON or an SSE stream. HTTP clients must be prepared for both.
- Servers expose tools with `tools/list`; clients call them with
  `tools/call`.
- Tool definitions include an `inputSchema`, optional `outputSchema`,
  description/title metadata, and optional annotations.
- MCP tool failures can be protocol-level JSON-RPC errors or successful
  tool results with `isError: true`.
- HTTP authorization exists in the spec, but static local config is
  enough for glue's first MCP client pass.

## Decision

### 1. Package boundary

Add MCP support as `tools/mcp`, not core `glue`.

`tools/mcp` imports `github.com/erain/glue` and exposes a small host API:

```go
type ServerConfig struct {
    Name      string
    Transport string // "stdio" or "http"

    Command string   // stdio
    Args    []string // stdio
    Env     []string // explicit KEY=VALUE entries
    WorkDir string

    URL     string            // http
    Headers map[string]string // http, already resolved

    ToolPrefix string
    Timeout    time.Duration
}

type Manager struct { ... }

func NewManager(ctx context.Context, configs []ServerConfig, opts Options) (*Manager, error)
func (m *Manager) Tools() []glue.Tool
func (m *Manager) Close() error
```

Exact names may change during implementation, but the shape is fixed:
the package manages configured MCP sessions and hands the host a stable
slice of `glue.Tool` values. It does not construct agents, read Peggy's
settings file, render permission UI, or decide product policy.

Core `glue` APIs do not change for v1. `Tool`, `ToolSpec`, and
`Permission` are already sufficient.

### 2. Protocol support

`tools/mcp` implements enough MCP client behavior for tools:

1. Start transport.
2. Send `initialize` with the latest supported protocol version and
   minimal client capabilities.
3. Require a compatible server protocol version.
4. Send `notifications/initialized`.
5. Call `tools/list`.
6. Convert tools to `glue.Tool`.
7. On tool execution, call `tools/call`.
8. On shutdown, close the transport according to the transport's rules.

Client capabilities in v1 are intentionally minimal:

- no `roots`;
- no `sampling`;
- no `elicitation`;
- no client-side prompts/resources exposed back to servers.

If an MCP server requires unsupported client features to make a tool
safe or useful, the integration should fail closed with a clear setup
error.

### 3. Transport sequence

Implement stdio first.

Stdio is the best fit for Peggy's local-first shape:

- it does not require hosted auth;
- it composes with existing trusted-workspace thinking;
- it can be tested deterministically with fake subprocesses;
- it is the transport clients are expected to support whenever possible.

Stdio process rules:

- `Command` is an executable path or basename, never a shell string.
- `Args` is argv-style.
- `Env` is explicit. The package must not inherit the full parent
  environment by default.
- `WorkDir` is explicit or empty for the parent process working
  directory.
- stdout is protocol only; stderr may be captured/truncated for
  diagnostics.
- shutdown closes stdin, waits briefly, then sends SIGTERM and
  eventually SIGKILL if needed.
- context cancellation cancels in-flight JSON-RPC requests and begins
  process cleanup.

Streamable HTTP lands after stdio with the same client/lifecycle code
behind a transport interface.

HTTP rules:

- Use the single configured MCP endpoint URL.
- Send JSON-RPC messages by POST.
- Accept both `application/json` and `text/event-stream` responses.
- Include the negotiated `MCP-Protocol-Version` header after
  initialization.
- Support static headers and token-from-env configuration.
- Do not implement dynamic OAuth registration in v1.
- Do not implement the deprecated HTTP+SSE transport unless a later
  compatibility issue proves it is necessary.

### 4. Tool name mapping

MCP tools become `glue.Tool` values.

Tool names are namespaced by server to avoid collisions and to satisfy
provider tool-name restrictions:

```text
mcp_<server>_<tool>
```

Both `<server>` and `<tool>` are sanitized to ASCII letters, digits, and
underscores. The original MCP server/tool names are retained in local
metadata for permission prompts, logs, and the eventual tool call.

`ToolSpec.Description` is built from MCP title/description plus a short
server prefix. `ToolSpec.Parameters` preserves the MCP `inputSchema`
raw JSON when it is valid. If an MCP tool has no parameters and no
schema, glue supplies:

```json
{"type":"object","additionalProperties":false}
```

The client should reject tools whose schemas are malformed or whose
names collapse to empty values after sanitization. Rejections are setup
warnings by default; a strict mode can turn them into setup errors.

### 5. Tool call and result mapping

Executing a glue MCP tool sends MCP `tools/call` with the original tool
name and the model-provided JSON arguments.

Result mapping:

- MCP text content becomes glue text content.
- MCP structured content is preserved in `ToolResult.Metadata` and also
  rendered as compact JSON text when there is no text content.
- Non-text content types are rendered as compact JSON text in v1 and
  preserved in metadata. Rich image/resource mapping can follow after
  the core client works.
- MCP `isError: true` maps to `ToolResult.IsError = true` with the
  model-visible text preserved.
- MCP protocol errors become `ToolResult.IsError = true` when the model
  may be able to recover, or a Go error when the MCP session/transport
  is broken.

Output schemas are recorded but not enforced in the first
implementation. Validation can be added once the basic tool path is
stable.

### 6. Permission and safety policy

MCP tools are treated as side-effecting by default.

Reason: MCP servers are external code/services. A tool named "search" may
still exfiltrate data, mutate remote state, or spend money. Peggy should
not trust server annotations as policy.

Default mapping:

```go
ToolSpec{
    RequiresPermission: true,
    PermissionAction:   "mcp_call",
    PermissionTarget:   "<server>.<tool>",
}
```

Configuration may mark specific servers or tools as read-only later, but
that is an explicit host override. Peggy's existing permission tiers
still apply by channel:

- `read_only` denies MCP calls before execution;
- `prompt` asks the owning client;
- `trusted` allows the call, subject to MCP server config and timeouts.

Permission prompts should show:

- server name;
- original MCP tool name;
- sanitized glue tool name;
- compact/truncated arguments;
- transport kind.

Secrets must not be logged. Header values, bearer tokens, provider keys,
and configured env values are redacted in diagnostics.

### 7. Peggy settings

Peggy owns product configuration. Proposed shape:

```json
{
  "mcp": {
    "servers": {
      "filesystem": {
        "enabled": true,
        "transport": "stdio",
        "command": "mcp-server-filesystem",
        "args": ["/path/to/workspace"],
        "env": ["LOG_LEVEL=warn"],
        "work_dir": "/path/to/workspace",
        "timeout_seconds": 30
      },
      "linear": {
        "enabled": false,
        "transport": "http",
        "url": "https://example.invalid/mcp",
        "headers_env": {
          "Authorization": "LINEAR_MCP_AUTH_HEADER"
        },
        "timeout_seconds": 30
      }
    }
  }
}
```

Rules:

- `enabled` defaults to false for examples and true only when a user
  explicitly configures a real server.
- `headers_env` maps header names to env var names. This avoids writing
  secrets to `settings.json`.
- For stdio, `command` and `args` are argv fields. There is no shell
  expansion.
- Each server name must be stable because it becomes part of tool names
  and permission targets.
- `timeout_seconds` applies to initialization, `tools/list`, and
  individual `tools/call` requests unless a later setting splits them.

Peggy registers MCP tools only when the server initializes and lists
tools successfully. A failed server should produce a clear setup error
when explicitly enabled. Future "best effort" mode can be additive.

### 8. Deferred MCP features

The following are intentionally out of v1:

- MCP resources as first-class Peggy memory/search inputs.
- MCP prompts as Peggy roles or prompt templates.
- MCP sampling requests back into Peggy's provider.
- MCP elicitation and task-augmented flows.
- Dynamic OAuth client registration.
- Persistent token storage.
- Tool-list change notifications and hot reload.
- Rich image/resource content mapping into provider-specific
  multimodal parts.
- MCP server marketplace or discovery UI.

Deferring these keeps the first implementation small and testable while
leaving the session and transport internals extensible.

## Test Strategy

No live MCP services in unit tests.

Required deterministic coverage:

- JSON-RPC request/response ID matching and protocol-error handling.
- Lifecycle negotiation success and incompatible protocol failure.
- Stdio fake server that initializes, lists tools, handles `tools/call`,
  emits stderr, and exits cleanly.
- Tool name sanitization and collision handling.
- Schema pass-through for `inputSchema`.
- MCP `isError: true` to glue `ToolResult.IsError`.
- Context cancellation and process cleanup.
- Peggy settings decoding and secret redaction.
- Permission metadata on generated tools.

HTTP transport tests use `httptest` with both JSON and SSE-style
responses once Streamable HTTP lands.

## Consequences

- Peggy gains ecosystem tools without making core `glue` an MCP-aware
  framework.
- Existing permission tiers and daemon permission UX cover MCP calls
  immediately because MCP tools are just `glue.Tool` values.
- The first implementation is conservative: local stdio, explicit
  settings, permission required by default, and no automatic server
  discovery.
- HTTP MCP remains a clear follow-up rather than a blocker for useful
  local integrations.

## Non-Goals

- No MCP server implementation in glue.
- No default MCP servers bundled with Peggy.
- No hosted MCP marketplace.
- No dynamic plugin loading.
- No public daemon exposure or hosted auth model.
- No replacement of Peggy's local coding tools in v1.
