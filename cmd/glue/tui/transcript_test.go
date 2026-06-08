package tui

import (
	"strings"
	"testing"
)

// stripANSI removes ANSI escape sequences so we can assert against the
// plain rendered text without coupling to color codes.
func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		if r == '\x1b' {
			inEsc = true
			continue
		}
		if inEsc {
			if r == 'm' || r == 'K' || r == 'H' || r == 'A' || r == 'B' || r == 'C' || r == 'D' || r == 'J' {
				inEsc = false
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func TestRenderUserAndAssistant(t *testing.T) {
	t.Parallel()
	u := transcriptItem{Kind: itemUser, Text: "hello"}
	if got := stripANSI(u.render(80)); !strings.Contains(got, "user >") || !strings.Contains(got, "hello") {
		t.Fatalf("user render = %q", got)
	}
	a := transcriptItem{Kind: itemAssistant, Text: "world"}
	got := stripANSI(a.render(80))
	if !strings.Contains(got, "assistant") || !strings.Contains(got, "world") {
		t.Fatalf("assistant render = %q", got)
	}
}

func TestRenderToolPhases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		item  transcriptItem
		wants []string
	}{
		{
			name: "pending shows awaiting permission",
			item: transcriptItem{Kind: itemTool, ToolName: "edit_file",
				ToolArgs: `{"path":"x.go","old_string":"a","new_string":"b"}`, ToolPhase: tsPending},
			wants: []string{"edit_file", "awaiting permission", "path=x.go", "- a", "+ b"},
		},
		{
			name: "running shows running marker",
			item: transcriptItem{Kind: itemTool, ToolName: "shell_exec",
				ToolArgs: `{"argv":["go","test"]}`, ToolPhase: tsRunning},
			wants: []string{"shell_exec", "running", "argv="},
		},
		{
			name: "done shows result body",
			item: transcriptItem{Kind: itemTool, ToolName: "read_file",
				ToolArgs: `{"path":"r.txt"}`, ToolResult: "alpha\nbeta\n", ToolPhase: tsDone},
			wants: []string{"read_file", "done", "alpha", "beta"},
		},
		{
			name: "failed shows failed marker",
			item: transcriptItem{Kind: itemTool, ToolName: "shell_exec",
				ToolArgs: `{"argv":["false"]}`, ToolResult: "exit 1", ToolErr: true, ToolPhase: tsDone},
			wants: []string{"shell_exec", "failed", "exit 1"},
		},
		{
			name: "denied shows denied marker",
			item: transcriptItem{Kind: itemTool, ToolName: "write_file",
				ToolArgs: `{"path":"x"}`, ToolPhase: tsDenied},
			wants: []string{"write_file", "denied"},
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := stripANSI(c.item.render(120))
			for _, w := range c.wants {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n%s", w, got)
				}
			}
		})
	}
}

func TestFlattenArgsKeyOrderAndQuoting(t *testing.T) {
	t.Parallel()
	// `path` is in the priority list and should come first; other keys follow
	// in stable (sorted) order.
	got := flattenArgs(`{"path":"a.go","max_bytes":1024}`)
	if !strings.HasPrefix(got, "path=a.go") {
		t.Fatalf("priority key not first: %q", got)
	}
	if !strings.Contains(got, "max_bytes=1024") {
		t.Fatalf("missing trailing key: %q", got)
	}

	// Multi-word strings get quoted; multi-line strings flatten newlines.
	got = flattenArgs(`{"pattern":"hello world\nnext"}`)
	if !strings.Contains(got, `pattern="hello world⏎next"`) {
		t.Fatalf("quoting wrong: %q", got)
	}

	// Invalid JSON falls back to the raw string, newlines flattened.
	got = flattenArgs("not-json\nstill")
	if !strings.Contains(got, "not-json") {
		t.Fatalf("fallback wrong: %q", got)
	}
}

func TestEditDiffPreview(t *testing.T) {
	t.Parallel()
	preview := editDiffPreview(`{"path":"util.go","old_string":"func A() int { return 0 }","new_string":"func A() int { return 42 }"}`)
	plain := stripANSI(preview)
	for _, w := range []string{"util.go", "- func A() int { return 0 }", "+ func A() int { return 42 }"} {
		if !strings.Contains(plain, w) {
			t.Errorf("missing %q in:\n%s", w, plain)
		}
	}

	// Bad JSON → empty preview (don't crash).
	if got := editDiffPreview("nope"); got != "" {
		t.Fatalf("expected empty preview on bad JSON, got %q", got)
	}

	// Long content truncates at ~6 lines + ellipsis.
	long := strings.Repeat("line\n", 12)
	preview = editDiffPreview(`{"path":"big.go","old_string":"x","new_string":"` + strings.ReplaceAll(long, "\n", `\n`) + `"}`)
	if !strings.Contains(stripANSI(preview), "…") {
		t.Errorf("expected truncation marker, got:\n%s", stripANSI(preview))
	}
}

func TestTruncateRuneSafe(t *testing.T) {
	t.Parallel()
	if got := truncate("hello world", 5); got != "hell…" {
		t.Fatalf("ascii truncate = %q", got)
	}
	// Multi-byte runes: should count by rune, not byte.
	if got := truncate("héllo", 4); got != "hél…" {
		t.Fatalf("rune truncate = %q", got)
	}
	if got := truncate("abc", 10); got != "abc" {
		t.Fatalf("no-truncate = %q", got)
	}
}
