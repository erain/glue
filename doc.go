// Package glue provides the public API for defining and running agents.
//
// It is intentionally thin over the lower-level loop package. It owns
// user-facing concepts such as agents, sessions, skills, roles, and stores.
// The runtime mechanics — provider event consumption, tool execution, and
// transcript management — live in the sibling [glue/loop] package.
//
// This file is the package marker for the bootstrap scaffold. Concrete types
// (Agent, Session, Tool, etc.) are added by later issues.
package glue
