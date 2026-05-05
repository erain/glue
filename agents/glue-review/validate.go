package main

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

// diffLineMap records, per file path, the set of line numbers on the
// NEW (post-change) side that are part of the diff — i.e., either
// added (`+`) or context (the lines a model could reasonably reference
// when explaining a change). Pure removals are intentionally excluded
// from the new side: there is no new-side line number to point at.
//
// The map is built once from the unified diff string and consulted by
// validateInlineComments to drop fabricated `path:line` citations.
type diffLineMap map[string]map[int]bool

// parseDiffLineMap walks a `git diff --no-color` output and records
// every (file, new-side line) pair the diff makes available for inline
// commenting. The parser is intentionally tolerant: malformed hunks
// are skipped rather than failing the run, since a diff parse error
// post-hoc should never red the review.
//
// File detection uses the `+++ b/<path>` line that follows each
// `--- a/<path>` pair. Hunks are identified by `@@ -a,b +c,d @@`.
// Within a hunk:
//   - `+` lines advance the new-side counter and are recorded.
//   - ` ` (context) lines advance the new-side counter and are recorded.
//   - `-` lines do NOT advance the new-side counter and are not recorded.
func parseDiffLineMap(diff string) diffLineMap {
	out := diffLineMap{}
	scanner := bufio.NewScanner(strings.NewReader(diff))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var currentPath string
	var newLine int

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "+++ "):
			// Format: `+++ b/<path>` (or `+++ /dev/null` for deletions).
			rest := strings.TrimPrefix(line, "+++ ")
			rest = strings.TrimSpace(rest)
			if rest == "/dev/null" {
				currentPath = ""
				continue
			}
			currentPath = strings.TrimPrefix(rest, "b/")
		case strings.HasPrefix(line, "@@ "):
			// Format: `@@ -a,b +c,d @@ optional context`. We only need c.
			start, ok := parseHunkNewStart(line)
			if !ok {
				continue
			}
			newLine = start
		case currentPath != "" && strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			recordLine(out, currentPath, newLine)
			newLine++
		case currentPath != "" && strings.HasPrefix(line, " "):
			recordLine(out, currentPath, newLine)
			newLine++
		case currentPath != "" && strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			// Removal — does NOT consume a new-side line.
			continue
		}
	}
	return out
}

func recordLine(m diffLineMap, path string, line int) {
	if line <= 0 {
		return
	}
	set, ok := m[path]
	if !ok {
		set = map[int]bool{}
		m[path] = set
	}
	set[line] = true
}

// parseHunkNewStart pulls `c` out of `@@ -a,b +c,d @@ ...`. Returns
// (start, ok). For single-line hunks (`+c` instead of `+c,d`), `,d`
// is absent — handle both.
func parseHunkNewStart(header string) (int, bool) {
	plus := strings.Index(header, "+")
	if plus < 0 {
		return 0, false
	}
	rest := header[plus+1:]
	end := strings.IndexAny(rest, " ,")
	if end < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// validateInlineComments splits inline-comment entries into those
// whose `path:line` is reachable on the new side of the diff and
// those that are not. Callers should write the kept set into the JSON
// payload and log the dropped set to stderr so workflow logs surface
// what the model fabricated.
func validateInlineComments(comments []InlineComment, lineMap diffLineMap) (kept, dropped []InlineComment) {
	if len(comments) == 0 {
		return nil, nil
	}
	for _, c := range comments {
		set, ok := lineMap[c.Path]
		if !ok {
			dropped = append(dropped, c)
			continue
		}
		if !set[c.Line] {
			dropped = append(dropped, c)
			continue
		}
		kept = append(kept, c)
	}
	return kept, dropped
}

// describeDropped returns a one-line-per-entry log of dropped inline
// comments, suitable for writing to stderr after a validation pass.
func describeDropped(dropped []InlineComment, lineMap diffLineMap) string {
	if len(dropped) == 0 {
		return ""
	}
	var b strings.Builder
	for _, c := range dropped {
		set, fileInDiff := lineMap[c.Path]
		reason := "path not in diff"
		if fileInDiff {
			reason = fmt.Sprintf("line %d not on new side of diff (file has %d added/context lines)", c.Line, len(set))
		}
		fmt.Fprintf(&b, "[validate] dropped [%s] %s:%d — %s (%s)\n",
			c.Severity, c.Path, c.Line, reason, snippet(c.Body, 80))
	}
	return b.String()
}

func snippet(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
