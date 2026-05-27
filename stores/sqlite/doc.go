// Package sqlite provides a SQLite-backed glue.Store with FTS5 over
// message text. Implementation uses modernc.org/sqlite — pure Go, no
// CGo, cross-compiles freely.
//
// One DB file per Store instance; multiple sessions share the file.
// The file is opened in WAL mode for concurrent reads.
//
// stores/file remains the simple option. Reach for stores/sqlite when
// you need cross-session search (it implements the optional Searcher
// capability behind Agent.SearchSessions / Session.Search) or when many
// sessions live in one process.
//
// Design: docs/adr/0007-memory-layer.md §2.
package sqlite
