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
