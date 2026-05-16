// Package auth handles ChatGPT-subscription OAuth tokens for the
// providers/codex transport.
//
// For v0.1 the package does not run an interactive login: it reads the
// auth.json file written by OpenAI's upstream Codex CLI (run "codex
// login" once outside glue) and refreshes stale tokens against
// auth.openai.com/oauth/token. The transport package in providers/codex
// uses the access token returned here as a Bearer credential against
// the Responses endpoint at chatgpt.com/backend-api/codex/responses.
//
// Design: docs/adr/0006-codex-provider.md.
package auth
