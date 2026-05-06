package main

import (
	"testing"
)

func TestParseInlineCommentsHappyPath(t *testing.T) {
	t.Parallel()
	md := "## Summary\n" +
		"This branch adds X.\n\n" +
		"## Issues\n" +
		"- [critical] main.go:12 — segfault on nil pointer\n" +
		"- [major] **server/handler.go:42** — leaks file descriptor\n" + // bold path is tolerated
		"- [minor] `pkg/utils.go:7` — unused import\n\n" + // backticked path
		"## Suggestions\n" +
		"- [minor] README.md:5 - explain new flag\n\n" + // hyphen separator
		"## Looks good\n" +
		"- [bonus] file.go:1 — should NOT parse, not a real severity\n"

	got := parseInlineComments(md)
	want := []struct {
		path string
		line int
		sev  string
	}{
		{"main.go", 12, "critical"},
		{"server/handler.go", 42, "major"},
		{"pkg/utils.go", 7, "minor"},
		{"README.md", 5, "minor"},
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d; got=%+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Path != w.path || got[i].Line != w.line || got[i].Severity != w.sev {
			t.Errorf("entry %d = %+v, want path=%s line=%d sev=%s", i, got[i], w.path, w.line, w.sev)
		}
	}
}

func TestParseInlineCommentsSkipsZeroLine(t *testing.T) {
	t.Parallel()
	md := "## Issues\n- [major] some-file.go:0 — cannot pin line\n"
	if got := parseInlineComments(md); len(got) != 0 {
		t.Fatalf("expected zero entries (line=0 is sentinel); got %+v", got)
	}
}

func TestParseInlineCommentsIgnoresOtherSections(t *testing.T) {
	t.Parallel()
	// Identical-shape entries outside Issues/Suggestions must NOT be parsed.
	md := "## Open questions\n- [major] foo.go:1 — should not parse\n"
	if got := parseInlineComments(md); len(got) != 0 {
		t.Fatalf("expected empty (entry outside Issues/Suggestions); got %+v", got)
	}
}

func TestParseInlineCommentsEmptyMarkdown(t *testing.T) {
	t.Parallel()
	if got := parseInlineComments(""); len(got) != 0 {
		t.Fatalf("expected nil/empty for empty input; got %+v", got)
	}
}

func TestParseInlineCommentsFreeFormPasses(t *testing.T) {
	t.Parallel()
	// Free-form bullets (no `[severity] path:line`) inside Issues should
	// be silently dropped — they belong in the bulk body.
	md := "## Issues\n- This is a generic concern with no file reference.\n- [major] real.go:9 — actual entry\n"
	got := parseInlineComments(md)
	if len(got) != 1 || got[0].Path != "real.go" || got[0].Line != 9 {
		t.Fatalf("expected one parsed entry; got %+v", got)
	}
}

func TestParseInlineCommentsCapturesFix(t *testing.T) {
	t.Parallel()
	md := "## Issues\n" +
		"- [major] math.go:6 — Off-by-one bug. Fix: change `i <= n` to `i < n`.\n" +
		"- [minor] util.go:9 — Missing nil check. Fix: guard with `if x == nil { return }`.\n"
	got := parseInlineComments(md)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2; got=%+v", len(got), got)
	}
	if got[0].Body != "Off-by-one bug" || got[0].Fix != "change `i <= n` to `i < n`." {
		t.Errorf("entry 0: body=%q fix=%q", got[0].Body, got[0].Fix)
	}
	if got[1].Body != "Missing nil check" || got[1].Fix != "guard with `if x == nil { return }`." {
		t.Errorf("entry 1: body=%q fix=%q", got[1].Body, got[1].Fix)
	}
}

func TestParseInlineCommentsBackcompatNoFix(t *testing.T) {
	t.Parallel()
	// v1 prompt output: no Fix clause. Body stays whole, Fix is empty.
	md := "## Issues\n- [major] foo.go:3 — Something is wrong here.\n"
	got := parseInlineComments(md)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Body != "Something is wrong here." || got[0].Fix != "" {
		t.Errorf("body=%q fix=%q (want body with period, fix empty)", got[0].Body, got[0].Fix)
	}
}

func TestParseInlineCommentsFixUsesLastMatch(t *testing.T) {
	t.Parallel()
	// "fix" appearing in the description must NOT cause a premature
	// split. The real `Fix:` is the LAST one.
	md := "## Issues\n- [major] x.go:1 — The fix should be obvious. Fix: rewrite the function.\n"
	got := parseInlineComments(md)
	if len(got) != 1 {
		t.Fatalf("len=%d", len(got))
	}
	if got[0].Body != "The fix should be obvious" {
		t.Errorf("body lost the in-description 'fix': got %q", got[0].Body)
	}
	if got[0].Fix != "rewrite the function." {
		t.Errorf("fix wrong: %q", got[0].Fix)
	}
}

func TestSplitBodyAndFixVariations(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in       string
		wantBody string
		wantFix  string
	}{
		// Standard form.
		{"do X. Fix: do Y", "do X", "do Y"},
		// Lowercase variant.
		{"do X. fix: do Y", "do X", "do Y"},
		// All-caps.
		{"do X. FIX: do Y", "do X", "do Y"},
		// No period, just space.
		{"do X Fix: do Y", "do X", "do Y"},
		// No Fix at all — body kept whole, including any trailing period
		// (matches the v1 backwards-compat path).
		{"do X.", "do X.", ""},
		{"do X", "do X", ""},
		// "fix" used in body — last match wins.
		{"the fix is small. Fix: real prompt", "the fix is small", "real prompt"},
	}
	for _, c := range cases {
		body, fix := splitBodyAndFix(c.in)
		if body != c.wantBody || fix != c.wantFix {
			t.Errorf("splitBodyAndFix(%q) = (%q, %q), want (%q, %q)",
				c.in, body, fix, c.wantBody, c.wantFix)
		}
	}
}

func TestSplitSections(t *testing.T) {
	t.Parallel()
	md := "## Summary\nintro\n\n## Issues\nfirst\n\n## Suggestions\nsecond\n"
	got := splitSections(md)
	if got["Summary"] == "" || got["Issues"] == "" || got["Suggestions"] == "" {
		t.Fatalf("missing sections: %+v", got)
	}
	if got["Nonexistent"] != "" {
		t.Fatalf("unknown section returned non-empty: %q", got["Nonexistent"])
	}
}
