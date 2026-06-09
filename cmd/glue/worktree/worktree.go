// Package worktree creates the dedicated git worktrees isolated goals run
// in: <repo root>/.glue/worktrees/<goal-id> on branch goal/<id-suffix>.
// Shared by the TUI's /goal -w and the headless `glue goal --worktree`.
// Git plumbing lives here in cmd/glue per ADR-0012 — the library stays
// git-free.
package worktree

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Branch derives the branch name for an isolated goal from its record id:
// "goal-ab12cd" → "goal/ab12cd".
func Branch(goalID string) string {
	return "goal/" + strings.TrimPrefix(goalID, "goal-")
}

// Ensure creates (or re-attaches, on resume) the dedicated git worktree for
// a goal and returns its directory.
func Ensure(workDir, goalID string) (string, error) {
	root, err := Git(workDir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("worktree isolation needs a git repository at %s", workDir)
	}
	dir := filepath.Join(root, ".glue", "worktrees", goalID)
	branch := Branch(goalID)

	// Resume case: the worktree directory already exists — verify it is a
	// usable checkout rather than silently working in a stale directory.
	if _, statErr := os.Stat(dir); statErr == nil {
		if _, err := Git(dir, "rev-parse", "--is-inside-work-tree"); err != nil {
			return "", fmt.Errorf("%s exists but is not a git worktree; remove it and retry", dir)
		}
		return dir, nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	// New branch first; if the branch survived a deleted worktree (or a
	// previous run), attach to it instead.
	if _, err := Git(root, "worktree", "add", dir, "-b", branch); err != nil {
		if _, err2 := Git(root, "worktree", "add", dir, branch); err2 != nil {
			return "", fmt.Errorf("git worktree add: %w", err)
		}
	}
	return dir, nil
}

// Git runs git with the given args in dir and returns trimmed stdout.
func Git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
