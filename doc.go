// Package glue provides the public API for defining and running agents.
//
// It is intentionally thin over the lower-level loop package. Users
// construct an [Agent] with [NewAgent], open a named [Session] with
// [Agent.Session], and drive turns with [Session.Prompt]. Tools are
// defined with [NewTool], skills and roles are discovered from a
// WorkDir, and structured output is produced with [Session.PromptJSON].
//
// Provider implementations live in subpackages (providers/gemini,
// providers/codex, providers/nvidia, providers/openrouter, with the
// shared OpenAI-compatible core in providers/openaicompat and a
// driver-style registry in providers). Session persistence is provided
// by stores subpackages (stores/file for the simple default,
// stores/sqlite for cross-session FTS5 search). Reusable tools live
// under tools (tools/fs, tools/git, tools/shell, tools/coding,
// tools/mcp).
//
// Normalized message and event types are re-exported here so that callers
// only need to import "github.com/erain/glue". For a guided, end-to-end
// walkthrough of building an agent, see docs/building-agents.md.
package glue
