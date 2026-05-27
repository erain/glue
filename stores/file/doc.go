// Package file provides a local JSON-backed session store for Glue.
//
// It writes one file per session below a configured directory using
// atomic temp-file-plus-rename writes, and implements the glue.Store
// interface. It is the simple default with no extra dependencies; reach
// for stores/sqlite when you need cross-session search.
package file
