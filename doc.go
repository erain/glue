// Package glue provides the public API for defining and running agents.
//
// It is intentionally thin over the lower-level loop package. Users
// construct an [Agent] with [NewAgent], open a named [Session] with
// [Agent.Session], and drive turns with [Session.Prompt]. Provider
// implementations live in subpackages (initially providers/gemini), and
// session persistence is provided by stores subpackages (initially
// stores/file).
//
// Normalized message and event types are re-exported here so that callers
// only need to import "github.com/erain/glue".
package glue
