package fs

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeJoin(t *testing.T) {
	base := t.TempDir()
	cases := []struct {
		name    string
		rel     string
		wantErr string
	}{
		{"empty", "", "path is required"},
		{"absolute", "/etc/passwd", "absolute paths are not allowed"},
		{"traversal", "../outside", "escapes work directory"},
		{"deep traversal", "a/b/../../../outside", "escapes work directory"},
		{"trailing dotdot", "a/..", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SafeJoin(base, tc.rel)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if !strings.HasPrefix(got, base) {
					t.Fatalf("resolved %q not under base %q", got, base)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got success: %q", tc.wantErr, got)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestSafeJoin_AcceptsValidNested(t *testing.T) {
	base := t.TempDir()
	got, err := SafeJoin(base, "subdir/file.go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(base, "subdir", "file.go")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"under cap", "hello", 10, "hello"},
		{"exact cap", "hello", 5, "hello"},
		{"over cap with marker fits", strings.Repeat("a", 100), 50, strings.Repeat("a", 50-len("\n\n[... truncated]")) + "\n\n[... truncated]"},
		{"over cap, marker too big", "abcdefghij", 5, "abcde"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Truncate(tc.in, tc.max)
			if got != tc.want {
				t.Fatalf("Truncate(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
		})
	}
}
