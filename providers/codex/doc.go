// Package codex is a glue.Provider that routes through the Codex
// Responses endpoint at chatgpt.com/backend-api/codex/responses,
// authenticated with a ChatGPT subscription via OAuth tokens read from
// the upstream Codex CLI's auth.json (run "codex login" once outside
// glue).
//
// Design: docs/adr/0006-codex-provider.md. Token handling lives in
// providers/codex/auth; this package owns request construction, the
// SSE event stream, and the 401-refresh-retry loop.
package codex
