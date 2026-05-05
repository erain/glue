package main

import (
	"strings"
	"testing"
)

const sampleDiff = `diff --git a/main.go b/main.go
index e69de29..0123456 100644
--- a/main.go
+++ b/main.go
@@ -0,0 +1,5 @@
+package main
+
+func main() {
+	panic("todo")
+}
diff --git a/util.go b/util.go
index 1111111..2222222 100644
--- a/util.go
+++ b/util.go
@@ -10,6 +10,8 @@ func Hello() string {
 	return "hi"
 }

+func Bye() string {
+	return "bye"
+}
 func unchanged() {}
 // some old line
-// removed line
diff --git a/dropped.go b/dropped.go
deleted file mode 100644
index 3333333..0000000
--- a/dropped.go
+++ /dev/null
@@ -1,3 +0,0 @@
-package main
-
-func gone() {}
`

func TestParseDiffLineMapAddedFile(t *testing.T) {
	t.Parallel()
	m := parseDiffLineMap(sampleDiff)

	mainLines, ok := m["main.go"]
	if !ok {
		t.Fatalf("main.go missing from line map: %+v", m)
	}
	for line := 1; line <= 5; line++ {
		if !mainLines[line] {
			t.Errorf("main.go line %d should be in map", line)
		}
	}
	if mainLines[6] {
		t.Errorf("main.go line 6 should NOT be in map (file is 5 lines)")
	}
}

func TestParseDiffLineMapModifiedFile(t *testing.T) {
	t.Parallel()
	m := parseDiffLineMap(sampleDiff)

	utilLines, ok := m["util.go"]
	if !ok {
		t.Fatalf("util.go missing: %+v", m)
	}
	// Hunk starts at +10. The body contains 7 new-side advancing lines:
	// 2 context, 3 added (the new func), 2 context. Removals don't count.
	expectInMap := []int{10, 11, 12, 13, 14, 15, 16}
	for _, line := range expectInMap {
		if !utilLines[line] {
			t.Errorf("util.go: expected line %d in map (have %+v)", line, utilLines)
		}
	}
}

func TestParseDiffLineMapSkipsDeletedFile(t *testing.T) {
	t.Parallel()
	m := parseDiffLineMap(sampleDiff)
	if _, ok := m["dropped.go"]; ok {
		t.Errorf("dropped.go should not appear in new-side line map (deleted): %+v", m["dropped.go"])
	}
}

func TestValidateInlineCommentsKeepsValid(t *testing.T) {
	t.Parallel()
	m := parseDiffLineMap(sampleDiff)
	comments := []InlineComment{
		{Path: "main.go", Line: 4, Severity: "major", Body: "panic stub"},
		{Path: "util.go", Line: 13, Severity: "minor", Body: "new helper"},
	}
	kept, dropped := validateInlineComments(comments, m)
	if len(kept) != 2 || len(dropped) != 0 {
		t.Fatalf("kept=%d dropped=%d (want 2/0)", len(kept), len(dropped))
	}
}

func TestValidateInlineCommentsDropsFabricated(t *testing.T) {
	t.Parallel()
	m := parseDiffLineMap(sampleDiff)
	comments := []InlineComment{
		{Path: "main.go", Line: 4, Severity: "major", Body: "real entry"},
		// Fabrications — wrong file or wrong line:
		{Path: "main.go", Line: 99, Severity: "critical", Body: "phantom line"},
		{Path: "imaginary.go", Line: 1, Severity: "major", Body: "phantom file"},
		{Path: "dropped.go", Line: 1, Severity: "major", Body: "deleted file"},
	}
	kept, dropped := validateInlineComments(comments, m)
	if len(kept) != 1 {
		t.Fatalf("expected 1 kept, got %d: %+v", len(kept), kept)
	}
	if kept[0].Body != "real entry" {
		t.Fatalf("wrong entry kept: %+v", kept[0])
	}
	if len(dropped) != 3 {
		t.Fatalf("expected 3 dropped, got %d: %+v", len(dropped), dropped)
	}
}

func TestDescribeDroppedNamesReason(t *testing.T) {
	t.Parallel()
	m := parseDiffLineMap(sampleDiff)
	dropped := []InlineComment{
		{Path: "imaginary.go", Line: 1, Severity: "major", Body: "phantom"},
		{Path: "main.go", Line: 99, Severity: "critical", Body: "wrong line"},
	}
	out := describeDropped(dropped, m)
	if !strings.Contains(out, "imaginary.go") || !strings.Contains(out, "path not in diff") {
		t.Errorf("missing 'path not in diff' reason: %s", out)
	}
	if !strings.Contains(out, "main.go") || !strings.Contains(out, "line 99 not on new side") {
		t.Errorf("missing 'line not on new side' reason: %s", out)
	}
}

func TestParseDiffLineMapHandlesEmpty(t *testing.T) {
	t.Parallel()
	if got := parseDiffLineMap(""); len(got) != 0 {
		t.Fatalf("empty diff should yield empty map, got %+v", got)
	}
}

func TestParseHunkNewStartShortForm(t *testing.T) {
	t.Parallel()
	cases := map[string]int{
		"@@ -1,5 +10,3 @@":   10,
		"@@ -1 +1 @@":        1,           // single-line hunks
		"@@ -1,3 +20 @@":     20,          // single-line on new side only
		"@@ -1,5 +10,3 @@ func Foo()": 10, // trailing context
	}
	for in, want := range cases {
		got, ok := parseHunkNewStart(in)
		if !ok || got != want {
			t.Errorf("parseHunkNewStart(%q) = (%d,%v), want (%d,true)", in, got, ok, want)
		}
	}
	// Malformed:
	if _, ok := parseHunkNewStart("garbage"); ok {
		t.Error("garbage header should not parse")
	}
}
