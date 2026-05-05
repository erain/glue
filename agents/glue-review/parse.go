package main

import (
	"regexp"
	"strconv"
	"strings"
)

// InlineComment is one line-level review entry that the calling Action
// can submit via the GitHub Pull Request Reviews API.
type InlineComment struct {
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Severity string `json:"severity"`
	Body     string `json:"body"`
}

// inlineEntryRE matches a single Issues / Suggestions list entry the
// system prompt asks for:
//
//	- [critical|major|minor] path/to/file.ext:LINE — description
//
// We accept a few variants because models are inconsistent:
//
//   - Leading `-`, `*`, or `[N]` bullet markers (or none).
//   - Bold severity (`**[major]**`).
//   - Separator may be em-dash `—`, hyphen `-`, en-dash `–`, or colon `:`.
//   - Trailing description may be multi-line; we only capture up to the
//     next entry start or section header.
var inlineEntryRE = regexp.MustCompile(
	`(?m)^[ \t]*(?:[-*]|\d+\.)?[ \t]*` + // optional list marker
		`(?:\*\*)?\[(critical|major|minor)\](?:\*\*)?[ \t]+` + // severity (optionally bold)
		"(?:`|\\*\\*)?" + // optional opening backtick OR bold
		`([^\s:` + "`" + `*]+):(\d+)` + // path:line
		"(?:`|\\*\\*)?" + // optional closing backtick OR bold
		`[ \t]*[—–\-:][ \t]*` + // separator
		`(.+)$`, // description
)

// parseInlineComments scans a review's Markdown output for entries
// inside the Issues and Suggestions sections that match the strict
// `[severity] path:line — body` shape. Entries with `:0` (model
// signaling "no precise line") and entries in any other section are
// skipped — they belong in the bulk review body.
func parseInlineComments(markdown string) []InlineComment {
	if markdown == "" {
		return nil
	}
	sections := splitSections(markdown)
	out := []InlineComment{}
	for _, name := range []string{"Issues", "Suggestions"} {
		body, ok := sections[name]
		if !ok {
			continue
		}
		for _, m := range inlineEntryRE.FindAllStringSubmatch(body, -1) {
			line, err := strconv.Atoi(m[3])
			if err != nil || line <= 0 {
				continue
			}
			out = append(out, InlineComment{
				Severity: m[1],
				Path:     m[2],
				Line:     line,
				Body:     strings.TrimSpace(m[4]),
			})
		}
	}
	return out
}

// splitSections walks the markdown looking for `## Header` lines and
// returns each section's body keyed by header text. Headers outside the
// canonical set (Summary, Issues, Suggestions, Looks good, Open
// questions) are still captured verbatim — we don't gate on them in
// case the model deviates.
func splitSections(markdown string) map[string]string {
	out := map[string]string{}
	lines := strings.Split(markdown, "\n")
	current := ""
	var buf strings.Builder
	flush := func() {
		if current != "" {
			out[current] = buf.String()
		}
		buf.Reset()
	}
	for _, line := range lines {
		if h, ok := matchHeading(line); ok {
			flush()
			current = h
			continue
		}
		if current != "" {
			buf.WriteString(line)
			buf.WriteByte('\n')
		}
	}
	flush()
	return out
}

// matchHeading returns the canonical title (case-preserved, trimmed) if
// the line is an H2 markdown header, else "", false.
func matchHeading(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "## ") {
		return "", false
	}
	return strings.TrimSpace(trimmed[3:]), true
}
