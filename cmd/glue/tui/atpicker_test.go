package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erain/glue"
)

func TestDetectAtTriggerOpensOnAtWord(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		// Opens
		{"@", "", true},
		{"@ut", "ut", true},
		{"explain @ut", "ut", true},
		{"\t@a", "a", true},

		// Does NOT open
		{"", "", false},
		{"hello", "", false},
		{"alice@example.com", "", false}, // no preceding whitespace
		{"@@param", "", false},           // escaped literal @
		{"@ut hello", "", false},         // word ended already
		{"@a\tb", "", false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.in, func(t *testing.T) {
			got, ok := detectAtTrigger(c.in)
			if ok != c.wantOK || got != c.want {
				t.Fatalf("detectAtTrigger(%q) = (%q, %v), want (%q, %v)",
					c.in, got, ok, c.want, c.wantOK)
			}
		})
	}
}

func TestApplyAtSelection(t *testing.T) {
	t.Parallel()
	if got := applyAtSelection("explain @ut", "util.go"); got != "explain @util.go " {
		t.Fatalf("got %q", got)
	}
	if got := applyAtSelection("@ut", "util.go"); got != "@util.go " {
		t.Fatalf("got %q", got)
	}
	if got := applyAtSelection("first then @sec", "sub/dir/second.go"); got != "first then @sub/dir/second.go " {
		t.Fatalf("got %q", got)
	}
}

func TestRemoveAtToken(t *testing.T) {
	t.Parallel()
	if got := removeAtToken("explain @ut"); got != "explain " {
		t.Fatalf("got %q", got)
	}
	if got := removeAtToken("@ut"); got != "" {
		t.Fatalf("got %q", got)
	}
}

// makePickerFixture creates a tempdir with a known set of files and
// returns an atPicker over it. Used by ranking tests.
func makePickerFixture(t *testing.T) *atPicker {
	t.Helper()
	dir := t.TempDir()
	mk := func(rel, body string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("util.go", "")
	mk("util_test.go", "")
	mk("cmd/util/helper.go", "")
	mk("vendor/x/util.go", "")
	mk("README.md", "")
	mk(".git/config", "")  // should be skipped
	mk(".env", "SECRET=1") // should be blocklisted
	return newAtPicker(dir)
}

func TestWalkWorkspaceSkipsBlockedAndGit(t *testing.T) {
	t.Parallel()
	p := makePickerFixture(t)
	for _, f := range p.files {
		if strings.HasPrefix(f, ".git/") {
			t.Errorf(".git leaked: %s", f)
		}
		if f == ".env" {
			t.Errorf(".env leaked")
		}
	}
}

func TestRefilterEmptyQueryReturnsAll(t *testing.T) {
	t.Parallel()
	p := makePickerFixture(t)
	if len(p.matches) != len(p.files) {
		t.Fatalf("empty query should match every file: %d / %d", len(p.matches), len(p.files))
	}
}

func TestRefilterRanksBasenameStartFirst(t *testing.T) {
	t.Parallel()
	p := makePickerFixture(t)
	p.refilter("ut")
	if len(p.matches) == 0 {
		t.Fatal("no matches")
	}
	// util.go is the best match — basename starts with "ut" and the
	// path is shortest.
	if got := p.files[p.matches[0]]; got != "util.go" {
		t.Fatalf("top match = %q, want util.go", got)
	}
}

func TestRefilterIsCaseInsensitive(t *testing.T) {
	t.Parallel()
	p := makePickerFixture(t)
	p.refilter("README")
	hits := []string{}
	for _, idx := range p.matches {
		hits = append(hits, p.files[idx])
	}
	if !contains(hits, "README.md") {
		t.Fatalf("case match failed: %v", hits)
	}
}

func contains(haystack []string, needle string) bool {
	for _, x := range haystack {
		if x == needle {
			return true
		}
	}
	return false
}

func TestPickerUpDownClamps(t *testing.T) {
	t.Parallel()
	p := makePickerFixture(t)
	p.refilter("")
	for i := 0; i < 20; i++ {
		p.down()
	}
	if p.cursor != len(p.matches)-1 {
		t.Fatalf("down past end: cursor = %d, want %d", p.cursor, len(p.matches)-1)
	}
	for i := 0; i < 20; i++ {
		p.up()
	}
	if p.cursor != 0 {
		t.Fatalf("up past start: cursor = %d", p.cursor)
	}
}

func TestAlwaysAllowPermission(t *testing.T) {
	t.Parallel()
	p := alwaysAllowPermission{}
	got, err := p.Decide(context.Background(), glue.PermissionRequest{Tool: "write_file"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !got.Allow {
		t.Fatalf("decision = %+v, want allow", got)
	}
	if got.RememberFor != glue.RememberSession {
		t.Fatalf("RememberFor = %v, want RememberSession", got.RememberFor)
	}
}

// TestAtPickerWindowIndicators verifies the "↑/↓ N more" lines that make
// off-window matches discoverable, and that popupRows accounts for them
// so the layout height math never truncates the popup frame.
func TestAtPickerWindowIndicators(t *testing.T) {
	t.Parallel()
	files := make([]string, 12)
	for i := range files {
		files[i] = fmt.Sprintf("file%02d.go", i)
	}
	p := &atPicker{files: files}
	p.refilter("")
	if len(p.matches) != 12 {
		t.Fatalf("matches = %d, want 12", len(p.matches))
	}

	// Cursor at top: window is [0, 8) → only a "↓ 4 more" tail.
	out := renderAtPicker(p, 100)
	if !strings.Contains(out, "↓ 4 more") || strings.Count(out, " more") != 1 {
		t.Fatalf("top window indicators wrong:\n%s", out)
	}
	if got := p.popupRows(); got != atPickerVisibleRows+1 {
		t.Fatalf("popupRows = %d, want %d (window + ↓ line)", got, atPickerVisibleRows+1)
	}

	// Cursor mid-list: both indicators.
	for i := 0; i < 8; i++ {
		p.down()
	}
	// cursor = 8 → window [1, 9): 1 above, 3 below.
	out = renderAtPicker(p, 100)
	if !strings.Contains(out, "↑ 1 more") || !strings.Contains(out, "↓ 3 more") {
		t.Fatalf("mid window indicators wrong:\n%s", out)
	}
	if got := p.popupRows(); got != atPickerVisibleRows+2 {
		t.Fatalf("popupRows = %d, want %d (window + both lines)", got, atPickerVisibleRows+2)
	}

	// Cursor at bottom: window is [4, 12) → only a "↑ 4 more" head.
	for i := 0; i < 8; i++ {
		p.down()
	}
	out = renderAtPicker(p, 100)
	if !strings.Contains(out, "↑ 4 more") || strings.Count(out, " more") != 1 {
		t.Fatalf("bottom window indicators wrong:\n%s", out)
	}

	// Few matches: no indicators, popupRows = match count.
	p.refilter("file00")
	if got := p.popupRows(); got != 1 {
		t.Fatalf("popupRows = %d, want 1", got)
	}
	if out = renderAtPicker(p, 100); strings.Contains(out, " more") {
		t.Fatalf("unexpected indicator with a single match:\n%s", out)
	}
}
