package atmentions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	toolsfs "github.com/erain/glue/tools/fs"
)

func mustWrite(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNoMentionsIsNoOp(t *testing.T) {
	t.Parallel()
	res, err := Expand("just a plain prompt with no at signs", Options{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if res.Prompt != "just a plain prompt with no at signs" {
		t.Fatalf("prompt mutated: %q", res.Prompt)
	}
	if len(res.Included) != 0 || len(res.Skipped) != 0 {
		t.Fatalf("expected zero mentions, got %+v", res)
	}
}

func TestExpandBareMention(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mustWrite(t, dir, "util.go", "package util\n")
	res, err := Expand("explain @util.go", Options{WorkDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Prompt, "--- @util.go ---") {
		t.Fatalf("missing header in:\n%s", res.Prompt)
	}
	if !strings.Contains(res.Prompt, "package util") {
		t.Fatalf("missing content in:\n%s", res.Prompt)
	}
	if len(res.Included) != 1 || res.Included[0] != "util.go" {
		t.Fatalf("included = %v", res.Included)
	}
}

func TestExpandMultipleMentionsDedup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mustWrite(t, dir, "a.go", "AAA")
	mustWrite(t, dir, "b.go", "BBB")
	// Two mentions of a.go should produce one section.
	res, err := Expand("see @a.go, also @a.go and @b.go too", Options{WorkDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(res.Prompt, "--- @a.go ---") != 1 {
		t.Fatalf("a.go appeared more than once:\n%s", res.Prompt)
	}
	if !strings.Contains(res.Prompt, "BBB") {
		t.Fatalf("b.go missing:\n%s", res.Prompt)
	}
}

func TestExpandQuotedPathWithSpaces(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mustWrite(t, dir, "with space.txt", "spaced!")
	res, err := Expand(`open @"with space.txt"`, Options{WorkDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Prompt, "spaced!") {
		t.Fatalf("quoted path not resolved:\n%s", res.Prompt)
	}
}

func TestExpandIgnoresEmailLikeText(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mustWrite(t, dir, "example.com", "would be a problem")
	res, err := Expand("contact alice@example.com please", Options{WorkDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Prompt, "would be a problem") {
		t.Fatalf("email accidentally inlined:\n%s", res.Prompt)
	}
	if len(res.Included) != 0 {
		t.Fatalf("email accidentally treated as mention: %v", res.Included)
	}
}

func TestExpandEscapedDoubleAt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mustWrite(t, dir, "param", "should not appear")
	res, err := Expand("use @@param in prose", Options{WorkDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Prompt, "should not appear") {
		t.Fatalf("@@ escape failed:\n%s", res.Prompt)
	}
}

func TestExpandRefusesBlockedPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mustWrite(t, dir, ".env", "SECRET=1")
	res, err := Expand("leak @.env please", Options{WorkDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Prompt, "SECRET=1") {
		t.Fatalf(".env content leaked:\n%s", res.Prompt)
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("skipped = %v, want 1", res.Skipped)
	}
	if !strings.Contains(res.Skipped[0].Reason, "blocked") {
		t.Fatalf("skip reason = %q", res.Skipped[0].Reason)
	}
}

func TestExpandRefusesPathEscape(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	res, err := Expand("try @../../etc/passwd", Options{WorkDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("skipped = %v, want 1", res.Skipped)
	}
}

func TestExpandRefusesDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Expand("inline @sub", Options{WorkDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Reason, "directory") {
		t.Fatalf("skipped = %+v", res.Skipped)
	}
}

func TestExpandRefusesOversizeFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mustWrite(t, dir, "big.txt", strings.Repeat("x", 4096))
	res, err := Expand("inline @big.txt", Options{WorkDir: dir, MaxBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Reason, "exceeds") {
		t.Fatalf("skipped = %+v", res.Skipped)
	}
}

func TestExpandMissingFileIsSkippedNotErrored(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	res, err := Expand("see @does-not-exist.go", Options{WorkDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Reason, "not found") {
		t.Fatalf("skipped = %+v", res.Skipped)
	}
	if !strings.Contains(res.Prompt, "see @does-not-exist.go") {
		t.Fatalf("original mention should remain in prompt: %q", res.Prompt)
	}
}

func TestBlocklistInjectable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mustWrite(t, dir, "internal.txt", "INTERNAL")
	bl := toolsfs.Default().Merge("internal.txt")
	res, err := Expand("see @internal.txt", Options{WorkDir: dir, Blocklist: bl})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("custom blocklist ignored: %+v", res)
	}
}
