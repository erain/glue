package tui

import (
	"context"
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
		{"@@param", "", false},            // escaped literal @
		{"@ut hello", "", false},          // word ended already
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
	mk(".git/config", "")     // should be skipped
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
