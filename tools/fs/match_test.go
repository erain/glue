package fs

import (
	"os"
	"strings"
	"testing"
)

// ladderEdit is a test helper: run an edit through the public tool and
// return the result plus the file's content afterwards.
func ladderEdit(t *testing.T, content, args string) (res string, isError bool, after string) {
	t.Helper()
	dir := t.TempDir()
	path := writeTemp(t, dir, "f.txt", content)
	r := callEdit(t, EditFileOptions{WorkDir: dir}, `{"path":"f.txt",`+args+`}`)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return r.Content[0].Text, r.IsError, string(data)
}

func TestLadderWhitespaceTolerant(t *testing.T) {
	// File has trailing spaces the model did not quote.
	content := "func a() {  \n\treturn 1  \n}\n"
	text, isErr, after := ladderEdit(t, content,
		`"old_string":"func a() {\n\treturn 1\n}","new_string":"func a() {\n\treturn 2\n}"`)
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	if !strings.Contains(after, "return 2") {
		t.Fatalf("edit not applied: %q", after)
	}
	if !strings.Contains(text, "whitespace-tolerant") {
		t.Fatalf("result does not report strategy: %s", text)
	}
}

func TestLadderIndentationTolerantReindents(t *testing.T) {
	// File uses tabs; the model quoted with four spaces. The
	// replacement must land with the file's tabs, not the model's
	// spaces.
	content := "if x {\n\tdoThing()\n}\n"
	_, isErr, after := ladderEdit(t, content,
		`"old_string":"if x {\n    doThing()\n}","new_string":"if x {\n    doOther()\n}"`)
	if isErr {
		t.Fatal("expected success")
	}
	if !strings.Contains(after, "\tdoOther()") {
		t.Fatalf("replacement not re-indented with tabs: %q", after)
	}
}

func TestLadderPunctuationTolerantMultiline(t *testing.T) {
	// File contains an em-dash and curly quotes; model quoted ASCII.
	content := "// note — see “spec”\nvalue := 1\n"
	_, isErr, after := ladderEdit(t, content,
		`"old_string":"// note - see \"spec\"\nvalue := 1","new_string":"// note - see \"spec\"\nvalue := 2"`)
	if isErr {
		t.Fatal("expected punctuation-tolerant match")
	}
	if !strings.Contains(after, "value := 2") {
		t.Fatalf("edit not applied: %q", after)
	}
}

func TestLadderPunctuationTolerantFragment(t *testing.T) {
	// Single-line fragment with a curly quote in the file.
	content := "msg := ‘hello’ + suffix\n"
	_, isErr, after := ladderEdit(t, content,
		`"old_string":"'hello'","new_string":"'goodbye'"`)
	if isErr {
		t.Fatal("expected fragment fold match")
	}
	if !strings.Contains(after, "'goodbye'") {
		t.Fatalf("edit not applied: %q", after)
	}
	if strings.Contains(after, "hello") {
		t.Fatalf("old text still present: %q", after)
	}
}

func TestLadderBlockAnchor(t *testing.T) {
	// Middle line drifted (model misquoted the comment); anchors hold.
	content := "func b() {\n\t// computes the thing carefully\n\treturn compute()\n}\n"
	_, isErr, after := ladderEdit(t, content,
		`"old_string":"func b() {\n\t// computes the thing\n\treturn compute()\n}","new_string":"func b() {\n\treturn computeFast()\n}"`)
	if isErr {
		t.Fatal("expected block-anchor match")
	}
	if !strings.Contains(after, "computeFast()") || strings.Contains(after, "carefully") {
		t.Fatalf("block not replaced: %q", after)
	}
}

func TestLadderOverEscapeRepair(t *testing.T) {
	content := "alpha\nbeta\n"
	// Model over-escaped: JSON \\n delivers literal backslash-n.
	text, isErr, after := ladderEdit(t, content,
		`"old_string":"alpha\\nbeta","new_string":"alpha\\ngamma"`)
	if isErr {
		t.Fatalf("expected unescape repair, got: %s", text)
	}
	if !strings.Contains(after, "gamma") {
		t.Fatalf("edit not applied: %q", after)
	}
	if !strings.Contains(text, "unescaping") {
		t.Fatalf("result does not report unescaping: %s", text)
	}
}

func TestLadderCRLFPreserved(t *testing.T) {
	content := "one\r\ntwo\r\nthree\r\n"
	_, isErr, after := ladderEdit(t, content,
		`"old_string":"two","new_string":"TWO"`)
	if isErr {
		t.Fatal("expected success")
	}
	if after != "one\r\nTWO\r\nthree\r\n" {
		t.Fatalf("CRLF not preserved: %q", after)
	}
}

func TestLadderBOMPreserved(t *testing.T) {
	content := "\uFEFFhead\nbody\n"
	_, isErr, after := ladderEdit(t, content,
		`"old_string":"body","new_string":"BODY"`)
	if isErr {
		t.Fatal("expected success")
	}
	if !strings.HasPrefix(after, "\uFEFF") || !strings.Contains(after, "BODY") {
		t.Fatalf("BOM not preserved: %q", after)
	}
}

func TestLadderAmbiguousFlexibleMatch(t *testing.T) {
	content := "  x := 1\n  y := 2\n  x := 1\n"
	text, isErr, _ := ladderEdit(t, content,
		`"old_string":"x := 1","new_string":"x := 9"`)
	if !isErr {
		t.Fatal("expected ambiguity error")
	}
	if !strings.Contains(text, "matches 2 times") || !strings.Contains(text, "replace_all") {
		t.Fatalf("error not instructive: %s", text)
	}
}

func TestLadderReplaceAllFlexible(t *testing.T) {
	content := "a()  \nb()\na()  \n"
	_, isErr, after := ladderEdit(t, content,
		`"old_string":"a()","new_string":"c()","replace_all":true`)
	if isErr {
		t.Fatal("expected success")
	}
	if strings.Count(after, "c()") != 2 {
		t.Fatalf("replace_all did not hit both: %q", after)
	}
}

func TestLadderNotFoundError(t *testing.T) {
	text, isErr, _ := ladderEdit(t, "totally different\n",
		`"old_string":"no such text","new_string":"x"`)
	if !isErr {
		t.Fatal("expected not-found error")
	}
	for _, want := range []string{"not found", "no such text", "read_file"} {
		if !strings.Contains(text, want) {
			t.Fatalf("error missing %q: %s", want, text)
		}
	}
}

func TestLadderRejectsLazyPlaceholder(t *testing.T) {
	text, isErr, after := ladderEdit(t, "func f() {\n\tbody()\n}\n",
		`"old_string":"func f() {\n\tbody()\n}","new_string":"func f() {\n\t// rest of the code unchanged\n}"`)
	if !isErr {
		t.Fatal("expected placeholder rejection")
	}
	if !strings.Contains(text, "placeholder") {
		t.Fatalf("error not instructive: %s", text)
	}
	if !strings.Contains(after, "body()") {
		t.Fatal("file must be untouched")
	}
}

func TestLadderResultEchoesUpdatedLines(t *testing.T) {
	content := "l1\nl2\nl3\nl4\nl5\nl6\nl7\n"
	text, isErr, _ := ladderEdit(t, content,
		`"old_string":"l4","new_string":"L4-new"`)
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	if !strings.Contains(text, "updated lines 4-4") {
		t.Fatalf("missing line range: %s", text)
	}
	// Snippet includes the new line plus two context lines each side.
	for _, want := range []string{"l2", "l3", "L4-new", "l5", "l6"} {
		if !strings.Contains(text, want) {
			t.Fatalf("snippet missing %q: %s", want, text)
		}
	}
	if strings.Contains(strings.TrimSuffix(text, "\n"), "l7") {
		t.Fatalf("snippet should not include l7: %s", text)
	}
}

func TestLadderExactStillWinsOverFlexible(t *testing.T) {
	// An exact match exists alongside a whitespace-drifted candidate;
	// the exact one must be chosen and reported as exact.
	content := "key := 1\nkey := 1  \n"
	text, isErr, after := ladderEdit(t, content,
		`"old_string":"key := 1\n","new_string":"key := 2\n"`)
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	if strings.Contains(text, "tolerant") {
		t.Fatalf("should be an exact match: %s", text)
	}
	if !strings.HasPrefix(after, "key := 2\n") {
		t.Fatalf("wrong occurrence replaced: %q", after)
	}
}

func TestFoldASCIIWithMapOffsets(t *testing.T) {
	line := "a—b" // em-dash is 3 bytes
	folded, starts, ends := foldASCIIWithMap(line)
	if folded != "a-b" {
		t.Fatalf("folded = %q", folded)
	}
	if starts[1] != 1 || ends[1] != 4 {
		t.Fatalf("dash mapping wrong: starts=%v ends=%v", starts, ends)
	}
	if starts[2] != 4 || ends[2] != 5 {
		t.Fatalf("trailing rune mapping wrong: starts=%v ends=%v", starts, ends)
	}
}
