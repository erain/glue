// Package peggy is a long-running personal-assistant agent built on
// the glue framework.
//
// Peggy reads two files at startup:
//
//   - SOUL.md — Markdown identity ("# Identity", "# About me",
//     "# People", "# Projects", "# Preferences" by convention; loader
//     does not enforce structure). Contents are embedded in the
//     system prompt so the model sees who Peggy is and who you are
//     on every turn.
//   - settings.json — JSON config (provider, model, store path,
//     compaction knobs, coding tools, MCP servers, channels, and
//     permission tiers).
//
// v0.3 ships the single-prompt CLI, Telegram channel, durable memory,
// opt-in local coding tools, configured MCP tool servers, and a local
// HTTP+SSE daemon so terminal and Telegram clients can share one
// long-running Peggy process.
// Tracker:
// https://github.com/erain/glue/issues/110.
//
// Design rule (per ADR-0005): every product concern lives in this
// package and not in core glue. The framework supplies interfaces
// (Provider, Store, Compactor, Searcher); Peggy fills them in.
package peggy
