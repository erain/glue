package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scratchRepo initializes a git repository with one commit so worktrees can
// branch off it.
func scratchRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
		{"commit", "--allow-empty", "-q", "-m", "init"},
	} {
		if _, err := Git(dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	return dir
}

func TestEnsureCreatesAndReuses(t *testing.T) {
	t.Parallel()
	repo := scratchRepo(t)

	dir, err := Ensure(repo, "goal-abc123")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	want := filepath.Join(repo, ".glue", "worktrees", "goal-abc123")
	if dir != want {
		t.Fatalf("dir = %q, want %q", dir, want)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("worktree dir missing: %v", err)
	}
	branch, err := Git(dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || branch != "goal/abc123" {
		t.Fatalf("branch = %q err=%v, want goal/abc123", branch, err)
	}

	// Second call (resume) reuses the existing worktree.
	again, err := Ensure(repo, "goal-abc123")
	if err != nil || again != dir {
		t.Fatalf("reuse: dir=%q err=%v", again, err)
	}

	// Deleted worktree but surviving branch: re-attach instead of failing
	// on the duplicate branch name.
	if _, err := Git(repo, "worktree", "remove", "--force", dir); err != nil {
		t.Fatalf("worktree remove: %v", err)
	}
	reattached, err := Ensure(repo, "goal-abc123")
	if err != nil || reattached != dir {
		t.Fatalf("re-attach: dir=%q err=%v", reattached, err)
	}
}

func TestEnsureOutsideRepo(t *testing.T) {
	t.Parallel()
	if _, err := Ensure(t.TempDir(), "goal-x"); err == nil || !strings.Contains(err.Error(), "git repository") {
		t.Fatalf("err = %v, want needs-a-git-repository", err)
	}
}
