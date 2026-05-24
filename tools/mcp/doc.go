// Package mcp implements the client foundation for consuming Model Context
// Protocol servers from glue hosts.
//
// This package follows ADR-0011. It supports JSON-RPC lifecycle negotiation
// over stdio and Streamable HTTP, discovery of MCP server tools, mapping
// those tools to permission-gated glue.Tool values, read-only resource
// metadata inspection, and permission-gated resource reads. Prompts, sampling,
// elicitation, OAuth, and dynamic discovery are deferred follow-up surfaces.
package mcp
