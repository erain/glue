package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Options specifies an explicitly created workspace. Exactly one source is required.
type Options struct{ Path, URL, Dir, Branch, BaseRef string }

// Workspace records the checkout and immutable starting revision.
type Workspace struct{ Path, Branch, BaseSHA string }

// Prepare creates a new local worktree or clones a repository, then checks out
// the requested branch from BaseRef. It never reuses an existing destination.
func Prepare(ctx context.Context, opts Options) (Workspace, error) {
	var result Workspace
	if (opts.Path == "") == (opts.URL == "") || opts.Dir == "" || opts.Branch == "" {
		return result, errors.New("workspace requires exactly one source, a destination, and a branch")
	}
	dir, err := filepath.Abs(opts.Dir)
	if err != nil {
		return result, err
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		return result, errors.New("workspace destination must not exist")
	}
	git := func(at string, args ...string) (string, error) {
		commandArgs := args
		if at != "" {
			commandArgs = append([]string{"-C", at}, args...)
		}
		cmd := exec.CommandContext(ctx, "git", commandArgs...)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %w", args[0], err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	source := opts.Path

	if _, err := git(source, "check-ref-format", "--branch", opts.Branch); err != nil {
		return result, err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return result, err
	}
	if opts.URL != "" {
		if _, err := git(filepath.Dir(dir), "clone", "--no-checkout", "--", opts.URL, dir); err != nil {
			return result, err
		}
		source = dir
	}
	ref := opts.BaseRef
	if ref == "" {
		ref, err = git(source, "symbolic-ref", "refs/remotes/origin/HEAD")
		if err != nil {
			for _, prefix := range []string{"refs/heads", "refs/remotes/origin"} {
				if branches, e := git(source, "for-each-ref", "--format=%(refname)", prefix); e == nil && branches != "" && !strings.Contains(branches, "\n") {
					ref = branches
					break
				}
			}
			for _, candidate := range []string{"refs/heads/main", "refs/heads/master", "refs/remotes/origin/main", "refs/remotes/origin/master"} {
				if ref != "" {
					break
				}
				if _, e := git(source, "rev-parse", "--verify", candidate+"^{commit}"); e == nil {
					ref = candidate
					break
				}
			}
			if ref == "" {
				return result, errors.New("repository default branch is unknown; specify BaseRef")
			}
		}
	}
	sha, err := git(source, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		sha, err = git(source, "rev-parse", "--verify", "--end-of-options", "refs/remotes/origin/"+ref+"^{commit}")
	}
	if err != nil {
		return result, err
	}
	if opts.URL != "" {
		_, err = git(source, "checkout", "-b", opts.Branch, sha)
	} else {
		_, err = git(source, "worktree", "add", "-b", opts.Branch, "--", dir, sha)
	}
	if err != nil {
		return result, err
	}
	return Workspace{Path: dir, Branch: opts.Branch, BaseSHA: sha}, nil
}
