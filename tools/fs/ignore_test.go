package fs

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFileT(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestIgnoreMatcher_CoreSemantics(t *testing.T) {
	root := t.TempDir()
	writeFileT(t, filepath.Join(root, ".gitignore"), `
# comment, then a blank line

*.log
build/
/anchored.txt
docs/*.tmp
!keep.log
**/generated
sub/**
`)

	m := NewIgnoreMatcher(root)
	cases := []struct {
		rel   string
		isDir bool
		want  bool
	}{
		{"app.log", false, true},              // basename glob at root
		{"nested/deep/app.log", false, true},  // basename glob at depth
		{"keep.log", false, false},            // negation wins (last match)
		{"nested/keep.log", false, false},     // negation at depth
		{"build", true, true},                 // dir-only pattern on a dir
		{"build", false, false},               // dir-only pattern on a file
		{"build/out.bin", false, true},        // inside ignored dir
		{"anchored.txt", false, true},         // leading-slash anchor at root
		{"nested/anchored.txt", false, false}, // anchor does not float
		{"docs/scratch.tmp", false, true},     // slash pattern anchored
		{"other/docs/scratch.tmp", false, false},
		{"generated", true, true},         // **/ matches at scope root
		{"a/b/generated", true, true},     // **/ at depth
		{"sub/anything.txt", false, true}, // trailing /** contents
		{"sub", true, false},              // trailing /** not the dir itself
		{"README.md", false, false},
	}
	for _, tc := range cases {
		if got := m.Ignored(tc.rel, tc.isDir); got != tc.want {
			t.Errorf("Ignored(%q, dir=%v) = %v, want %v", tc.rel, tc.isDir, got, tc.want)
		}
	}
}

func TestIgnoreMatcher_NestedGitignore(t *testing.T) {
	root := t.TempDir()
	writeFileT(t, filepath.Join(root, ".gitignore"), "*.secret\n")
	writeFileT(t, filepath.Join(root, "pkg", ".gitignore"), "vendor/\n!important.secret\n")

	m := NewIgnoreMatcher(root)
	if !m.Ignored("pkg/vendor", true) {
		t.Error("nested .gitignore dir rule not applied")
	}
	if !m.Ignored("pkg/vendor/lib.js", false) {
		t.Error("file under nested-ignored dir should be ignored")
	}
	if m.Ignored("vendor", true) {
		t.Error("nested rule leaked to the root scope")
	}
	if !m.Ignored("top.secret", false) {
		t.Error("root rule lost")
	}
	if m.Ignored("pkg/important.secret", false) {
		t.Error("deeper .gitignore negation should override the root rule")
	}
}

func TestIgnoreMatcher_NilAndMissing(t *testing.T) {
	var nilM *IgnoreMatcher
	if nilM.Ignored("anything", false) {
		t.Error("nil matcher must ignore nothing")
	}
	m := NewIgnoreMatcher(t.TempDir()) // no .gitignore at all
	if m.Ignored("main.go", false) {
		t.Error("matcher with no .gitignore must ignore nothing")
	}
}
