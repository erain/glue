// Package fs provides filesystem helpers and tool factories that agents
// can register without reinventing path safety, output truncation, or
// the sensitive-file blocklist.
//
// The package is intentionally outside the core glue package so the
// harness stays free of POSIX coupling per ADR 0003. Importing fs gives
// you ready-to-register tools (e.g. ReadFileTool) plus the primitives
// they are built on (SafeJoin, Truncate, Blocklist).
package fs

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// SafeJoin resolves rel against base and rejects any path that escapes
// base via "..", absolute prefixes, or symlinks. Returns the cleaned
// absolute path on success.
//
// Use this from any tool that accepts a model-supplied path: a model
// will eventually try `../../../etc/passwd`, and SafeJoin's job is to
// turn that into an error before os.Open ever runs.
func SafeJoin(base, rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", errors.New("path is required")
	}
	if filepath.IsAbs(rel) {
		return "", errors.New("absolute paths are not allowed")
	}
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	candidate := filepath.Clean(filepath.Join(absBase, rel))
	rel2, err := filepath.Rel(absBase, candidate)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(rel2, "..") || rel2 == ".." {
		return "", fmt.Errorf("path %q escapes work directory", rel)
	}
	return candidate, nil
}

// Truncate caps s at max bytes, appending a visible truncation marker
// when the cap is hit. Returns s unchanged when len(s) <= max.
//
// Designed for tool-output capping: a runaway diff or log read should
// shrink to a bounded size with a marker the model can recognize,
// rather than silently dropping the tail.
func Truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const note = "\n\n[... truncated]"
	if max <= len(note) {
		return s[:max]
	}
	return s[:max-len(note)] + note
}
