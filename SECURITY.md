# Security Policy

## Supported Versions

Glue is **pre-1.0**. Only the latest `0.x` minor release receives
security fixes; older minor versions are not patched. Pin to a tag in
your `go.mod` and read `CHANGELOG.md` before you upgrade — see
[ADR-0013](docs/adr/0013-pre-1-0-stability-stance.md) for the full
stability stance.

| Version | Status |
|---------|--------|
| Latest `v0.x.y` | ✅ Fixes land on `main`; new patch release as needed. |
| Older `v0.x.y` | ❌ No backports. Upgrade. |

## Reporting a Vulnerability

Please **do not file public GitHub issues** for security reports.

- Preferred: open a [private vulnerability report](https://github.com/erain/glue/security/advisories/new)
  via GitHub Security Advisories.
- Fallback: email <koheiipro@gmail.com> with subject `[glue security]`.

You should hear back within 5 business days. We aim to acknowledge,
triage, fix, and release within 30 days for serious issues; if the fix
needs longer we will say so explicitly.

When reporting, include the affected version (`go list -m
github.com/erain/glue` output is ideal), a minimal reproduction, and
your assessment of impact.

## Scope

In scope:

- The public `glue.*` API (Agent, Session, Tool, Provider) and the
  loop package — anything an agent author imports directly.
- The shipped tool packages (`tools/fs`, `tools/shell`, `tools/git`,
  `tools/coding`, `tools/mcp`): path-escape, blocklist evasion,
  permission-gate bypass, symlink races, command-injection through
  `shell_exec`, etc.
- The `cmd/glue` CLI and daemon (HTTP+SSE protocol, token handling,
  permission broker).
- The shipped stores (`stores/file`, `stores/sqlite`) — accidental
  cross-session leakage, SQL injection via FTS5 query construction,
  etc.
- The reference agents under `agents/` to the extent the issue lives
  in agent-side code we ship.

Out of scope (already known, not security bugs):

- **Codex provider auth path.** `providers/codex` uses the upstream
  Codex CLI&rsquo;s `~/.codex/auth.json` to authenticate against a
  ChatGPT-subscription endpoint that OpenAI does not formally document.
  This is a personal-use convenience, not a supported integration. Do
  not ship Codex-backed agents to users.
- **Local execution is not a sandbox.** `tools/shell` and the
  `tools/coding` bundle run commands in the host process via
  `glue.LocalExecutor`. They are permission-gated, not isolated. Hosts
  that need isolation must implement their own `glue.Executor` against
  a container or VM (see ADR-0009).
- **Models can be coerced.** Bugs in *what the model decides to do*
  with the tools we give it (prompt injection, jailbreaks, tool misuse)
  are model-behavior issues, not glue bugs, unless they exploit a flaw
  in the tool layer above.
- **Downstream API key handling.** Glue reads provider keys from env
  vars (`GEMINI_API_KEY`, etc.) and forwards them only to the
  configured provider. Misconfigured shells or leaked `.env` files are
  your operational concern, not glue's.

## Coordinated Disclosure

If you find a vulnerability that affects another OSS project as well
(an upstream Go module, a provider SDK, etc.), please coordinate that
disclosure as you see fit. We will hold our fix until the embargo
window you propose lifts.

Thank you for reporting responsibly.
