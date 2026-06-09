package fs

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// This file implements the edit_file repair ladder: a cascade of
// matching strategies, cheapest and strictest first, that absorbs the
// whitespace and punctuation drift models introduce when quoting file
// content back. Every strategy is deterministic — no similarity
// scoring — so a match is always explainable in the tool result.
//
// Ladder order (stop at the first strategy that matches):
//
//  1. exact            — byte-for-byte (the pre-existing behavior).
//  2. whitespace       — per-line, ignoring trailing spaces/tabs.
//  3. indentation      — per-line, ignoring all leading/trailing
//                        whitespace; the replacement is re-indented to
//                        the file's actual indentation.
//  4. punctuation      — per-line like (3), additionally folding smart
//                        quotes, Unicode dashes, and exotic spaces to
//                        their ASCII forms; for a single-line fragment,
//                        folded substring search inside each line.
//  5. block-anchor     — for blocks of >= 3 lines, match on the first
//                        and last line only (trimmed), same line count.
//
// If nothing matches and old_string looks over-escaped (a known model
// failure mode: literal `\n` two-byte sequences instead of newlines),
// the sequences are unescaped and the ladder runs once more.

// editOutcome reports how an edit was applied.
type editOutcome struct {
	updated   int    // number of replacements made
	strategy  string // which ladder rung matched
	firstLine int    // 1-based first line of the first replacement, in the updated content
	lastLine  int    // 1-based last line of the first replacement, in the updated content
	unescaped bool   // true when over-escape repair was needed
}

type matchAmbiguousError struct {
	count    int
	strategy string
}

func (e *matchAmbiguousError) Error() string {
	return fmt.Sprintf("old_string matches %d times (via %s match); add surrounding context to make it unique or set replace_all=true", e.count, e.strategy)
}

type matchNotFoundError struct{ needle string }

func (e *matchNotFoundError) Error() string {
	echo := e.needle
	if len(echo) > 120 {
		echo = echo[:120] + "…"
	}
	return fmt.Sprintf("old_string not found (tried exact and whitespace/indentation/punctuation-tolerant matching). Searched for %q. Re-read the file with read_file and copy its exact text — check indentation, line endings, and quote/dash characters", echo)
}

// applyLadderEdit replaces old with new inside content using the repair
// ladder. content, old, and new must already be LF-normalized.
func applyLadderEdit(content, old, new string, replaceAll bool) (string, editOutcome, error) {
	updated, outcome, err := ladderOnce(content, old, new, replaceAll)
	if err == nil {
		return updated, outcome, nil
	}
	if _, ambiguous := err.(*matchAmbiguousError); ambiguous {
		return "", editOutcome{}, err
	}
	// Over-escape repair: a model that wrote "\\n" in JSON delivers a
	// literal backslash+n. Unescape both sides and try once more.
	uOld, uNew := repairOverEscaping(old), repairOverEscaping(new)
	if uOld != old {
		updated, outcome, uerr := ladderOnce(content, uOld, uNew, replaceAll)
		if uerr == nil {
			outcome.unescaped = true
			return updated, outcome, nil
		}
		if _, ambiguous := uerr.(*matchAmbiguousError); ambiguous {
			return "", editOutcome{}, uerr
		}
	}
	return "", editOutcome{}, err
}

// ladderOnce runs the matching ladder a single time.
func ladderOnce(content, old, new string, replaceAll bool) (string, editOutcome, error) {
	type stage struct {
		name string
		find func() ([]span, func(span) string)
	}
	stages := []stage{
		{"exact", func() ([]span, func(span) string) {
			return exactSpans(content, old), func(span) string { return new }
		}},
		{"whitespace-tolerant", func() ([]span, func(span) string) {
			return lineWindowSpans(content, old, lineEqRStrip), func(span) string { return new }
		}},
		{"indentation-tolerant", func() ([]span, func(span) string) {
			spans := lineWindowSpans(content, old, lineEqTrim)
			return spans, reindentRenderer(content, old, new)
		}},
		{"punctuation-tolerant", func() ([]span, func(span) string) {
			if !strings.Contains(old, "\n") {
				return foldedFragmentSpans(content, old), func(span) string { return new }
			}
			spans := lineWindowSpans(content, old, lineEqFold)
			return spans, reindentRenderer(content, old, new)
		}},
		{"block-anchor", func() ([]span, func(span) string) {
			spans := blockAnchorSpans(content, old)
			return spans, reindentRenderer(content, old, new)
		}},
	}

	for _, st := range stages {
		spans, render := st.find()
		if len(spans) == 0 {
			continue
		}
		if len(spans) > 1 && !replaceAll {
			return "", editOutcome{}, &matchAmbiguousError{count: len(spans), strategy: st.name}
		}
		updated, firstStart := replaceSpans(content, spans, render)
		firstLine, lastLine := replacedLineRange(updated, firstStart, len(render(spans[0])))
		return updated, editOutcome{
			updated:   len(spans),
			strategy:  st.name,
			firstLine: firstLine,
			lastLine:  lastLine,
		}, nil
	}
	return "", editOutcome{}, &matchNotFoundError{needle: old}
}

// span is a half-open byte range [start, end) into content.
type span struct{ start, end int }

// exactSpans finds non-overlapping exact occurrences, left to right.
func exactSpans(content, old string) []span {
	if old == "" {
		return nil
	}
	var spans []span
	for from := 0; ; {
		i := strings.Index(content[from:], old)
		if i < 0 {
			break
		}
		start := from + i
		spans = append(spans, span{start, start + len(old)})
		from = start + len(old)
	}
	return spans
}

// lineInfo locates one line inside content: content[start:end] is the
// line's text without its trailing newline.
type lineInfo struct{ start, end int }

func splitLineInfos(content string) []lineInfo {
	var lines []lineInfo
	start := 0
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' {
			lines = append(lines, lineInfo{start, i})
			start = i + 1
		}
	}
	lines = append(lines, lineInfo{start, len(content)})
	return lines
}

// oldNeedleLines splits old_string into comparable lines. A trailing
// newline is recorded separately so the matched span can absorb it.
func oldNeedleLines(old string) (lines []string, trailingNL bool) {
	lines = strings.Split(old, "\n")
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		return lines[:len(lines)-1], true
	}
	return lines, false
}

func lineEqRStrip(a, b string) bool {
	return strings.TrimRight(a, " \t") == strings.TrimRight(b, " \t")
}

func lineEqTrim(a, b string) bool {
	return strings.TrimSpace(a) == strings.TrimSpace(b)
}

func lineEqFold(a, b string) bool {
	return foldASCII(strings.TrimSpace(a)) == foldASCII(strings.TrimSpace(b))
}

// lineWindowSpans slides a window of len(oldLines) lines over content
// and reports windows where every line satisfies eq.
func lineWindowSpans(content, old string, eq func(a, b string) bool) []span {
	oldLines, trailingNL := oldNeedleLines(old)
	if len(oldLines) == 0 {
		return nil
	}
	lines := splitLineInfos(content)
	if len(oldLines) > len(lines) {
		return nil
	}
	var spans []span
	for i := 0; i+len(oldLines) <= len(lines); i++ {
		match := true
		for j, ol := range oldLines {
			li := lines[i+j]
			if !eq(content[li.start:li.end], ol) {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		end := lines[i+len(oldLines)-1].end
		if trailingNL && end < len(content) && content[end] == '\n' {
			end++
		}
		spans = append(spans, span{lines[i].start, end})
		i += len(oldLines) - 1 // non-overlapping
	}
	return spans
}

// blockAnchorSpans matches blocks of >= 3 lines on their first and last
// trimmed lines only, requiring the same line count. This survives
// drift in the middle of a block the model quoted imperfectly.
func blockAnchorSpans(content, old string) []span {
	oldLines, trailingNL := oldNeedleLines(old)
	if len(oldLines) < 3 {
		return nil
	}
	first := strings.TrimSpace(oldLines[0])
	last := strings.TrimSpace(oldLines[len(oldLines)-1])
	if first == "" || last == "" {
		return nil
	}
	lines := splitLineInfos(content)
	if len(oldLines) > len(lines) {
		return nil
	}
	var spans []span
	for i := 0; i+len(oldLines) <= len(lines); i++ {
		fi := lines[i]
		li := lines[i+len(oldLines)-1]
		if strings.TrimSpace(content[fi.start:fi.end]) != first ||
			strings.TrimSpace(content[li.start:li.end]) != last {
			continue
		}
		end := li.end
		if trailingNL && end < len(content) && content[end] == '\n' {
			end++
		}
		spans = append(spans, span{fi.start, end})
		i += len(oldLines) - 1
	}
	return spans
}

// foldedFragmentSpans finds a single-line fragment inside content lines
// after folding punctuation, mapping folded offsets back to the
// original bytes.
func foldedFragmentSpans(content, old string) []span {
	needle := foldASCII(old)
	if needle == "" {
		return nil
	}
	var spans []span
	for _, li := range splitLineInfos(content) {
		line := content[li.start:li.end]
		folded, starts, ends := foldASCIIWithMap(line)
		for from := 0; ; {
			i := strings.Index(folded[from:], needle)
			if i < 0 {
				break
			}
			fs, fe := from+i, from+i+len(needle)
			spans = append(spans, span{li.start + starts[fs], li.start + ends[fe-1]})
			from = fe
		}
	}
	return spans
}

// foldASCII folds smart quotes, Unicode dashes, and exotic spaces to
// plain ASCII so model-introduced punctuation drift still matches.
func foldASCII(s string) string {
	folded, _, _ := foldASCIIWithMap(s)
	return folded
}

// foldASCIIWithMap folds s and records, for every folded byte, the
// start and end byte offsets of the source rune in s.
func foldASCIIWithMap(s string) (string, []int, []int) {
	var b strings.Builder
	b.Grow(len(s))
	starts := make([]int, 0, len(s))
	ends := make([]int, 0, len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		repl := foldRune(r)
		for k := 0; k < len(repl); k++ {
			starts = append(starts, i)
			ends = append(ends, i+size)
		}
		b.WriteString(repl)
		i += size
	}
	return b.String(), starts, ends
}

func foldRune(r rune) string {
	switch r {
	case '‘', '’', '‚', '‛':
		return "'"
	case '“', '”', '„', '‟':
		return `"`
	case '‐', '‑', '‒', '–', '—', '―', '−':
		return "-"
	case ' ', ' ', ' ', ' ', ' ', ' ', ' ',
		' ', ' ', ' ', ' ', ' ', ' ', ' ', '　':
		return " "
	default:
		return string(r)
	}
}

// reindentRenderer re-applies the file's actual indentation when a
// match succeeded only after ignoring leading whitespace. It pairs each
// old_string line with the file line it matched, derives an indent
// substitution map (e.g. "four spaces" → "one tab" per nesting level),
// and applies the longest-prefix substitution to every replacement
// line — preserving relative indentation even for tab/space swaps that
// only appear on inner lines.
func reindentRenderer(content, old, new string) func(span) string {
	oldLines, _ := oldNeedleLines(old)
	return func(sp span) string {
		matched := strings.Split(content[sp.start:sp.end], "\n")
		type indentPair struct{ from, to string }
		var pairs []indentPair
		seen := map[string]bool{}
		for j := 0; j < len(oldLines) && j < len(matched); j++ {
			if strings.TrimSpace(oldLines[j]) == "" {
				continue
			}
			from := leadingWhitespace(oldLines[j])
			to := leadingWhitespace(matched[j])
			if seen[from] {
				continue
			}
			seen[from] = true
			pairs = append(pairs, indentPair{from, to})
		}
		// Longest old indent first, so deeper nesting wins prefix checks.
		for i := 0; i < len(pairs); i++ {
			for j := i + 1; j < len(pairs); j++ {
				if len(pairs[j].from) > len(pairs[i].from) {
					pairs[i], pairs[j] = pairs[j], pairs[i]
				}
			}
		}
		changed := false
		for _, p := range pairs {
			if p.from != p.to {
				changed = true
				break
			}
		}
		if !changed {
			return new
		}
		lines := strings.Split(new, "\n")
		for i, line := range lines {
			if line == "" {
				continue
			}
			ws := leadingWhitespace(line)
			for _, p := range pairs {
				if p.from == "" {
					if ws == "" {
						lines[i] = p.to + line
						break
					}
					continue
				}
				if strings.HasPrefix(ws, p.from) {
					lines[i] = p.to + line[len(p.from):]
					break
				}
			}
		}
		return strings.Join(lines, "\n")
	}
}

func leadingWhitespace(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' && s[i] != '\t' {
			return s[:i]
		}
	}
	return s
}

// replaceSpans applies the rendered replacement to every span and
// returns the updated content plus the byte offset (in the updated
// content) where the first replacement begins.
func replaceSpans(content string, spans []span, render func(span) string) (string, int) {
	var b strings.Builder
	prev := 0
	firstStart := 0
	for i, sp := range spans {
		b.WriteString(content[prev:sp.start])
		if i == 0 {
			firstStart = b.Len()
		}
		b.WriteString(render(sp))
		prev = sp.end
	}
	b.WriteString(content[prev:])
	return b.String(), firstStart
}

// replacedLineRange computes the 1-based line range covered by the
// replacement that starts at byte offset start with length n, in the
// updated content.
func replacedLineRange(updated string, start, n int) (int, int) {
	firstLine := 1 + strings.Count(updated[:start], "\n")
	end := start + n
	if end > len(updated) {
		end = len(updated)
	}
	lastLine := firstLine + strings.Count(updated[start:end], "\n")
	return firstLine, lastLine
}

// repairOverEscaping collapses literal backslash escape sequences that
// over-eager models emit instead of the control characters themselves.
// It only rewrites sequences that decode to whitespace or quotes; real
// code containing intentional `\n` escapes inside string literals is
// matched by the exact stage long before this runs.
var overEscapeReplacer = strings.NewReplacer(
	`\n`, "\n",
	`\t`, "\t",
	`\r`, "\r",
	`\"`, `"`,
	`\'`, "'",
)

func repairOverEscaping(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	return overEscapeReplacer.Replace(s)
}

// lazyPlaceholderRe flags "rest of the code unchanged"-style
// placeholders a lazy model writes instead of the real replacement.
var lazyPlaceholderRe = regexp.MustCompile(`(?i)(rest of (the )?(code|file|function|method|class)|remains? unchanged|existing (code|implementation)|unchanged (code|lines)|\.\.\. ?(existing|rest|unchanged|omitted)|(code|implementation) omitted)`)

func containsLazyPlaceholder(s string) bool {
	return lazyPlaceholderRe.MatchString(s)
}

// editSnippet renders the updated region with up to context lines on
// each side, for echoing back to the model so it does not re-read the
// file to verify its edit.
func editSnippet(updated string, firstLine, lastLine, context, maxLines, maxBytes int) string {
	lines := strings.Split(updated, "\n")
	lo := firstLine - 1 - context
	if lo < 0 {
		lo = 0
	}
	hi := lastLine + context
	if hi > len(lines) {
		hi = len(lines)
	}
	if hi-lo > maxLines {
		hi = lo + maxLines
	}
	snippet := strings.Join(lines[lo:hi], "\n")
	if len(snippet) > maxBytes {
		snippet = snippet[:maxBytes] + "…"
	}
	return snippet
}
