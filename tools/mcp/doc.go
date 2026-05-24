// Package mcp implements the client foundation for consuming Model Context
// Protocol servers from glue hosts.
//
// This package follows ADR-0011. The first implementation supports JSON-RPC
// lifecycle negotiation over stdio only. Mapping MCP tools to glue.Tool values,
// Streamable HTTP, resources, prompts, sampling, elicitation, and OAuth are
// deferred follow-up surfaces.
package mcp
