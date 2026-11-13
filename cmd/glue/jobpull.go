// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// pullOptions selects how glue job pull brings a job's changes home.
type pullOptions struct {
	checkout, apply, force bool
}

// pullJob brings a finished job's changes into the git checkout in dir:
// by default as a local branch glue/<id>, with checkout switching to it, or
// with apply as uncommitted changes in the current working tree.
func pullJob(ctx context.Context, c jobAPI, id, dir string, o pullOptions, stdout io.Writer) error {
	if o.checkout && o.apply {
		return errors.New("--checkout and --apply cannot be combined")
	}
	d, err := c.Show(ctx, id)
	if err != nil {
		return err
	}
	if want := strings.TrimSpace(d.Spec.Repo.URL); want != "" {
		origin, err := gitOutput(ctx, dir, "config", "--get", "remote.origin.url")
		if err != nil || origin == "" {
			return fmt.Errorf("this checkout has no origin remote; run glue job pull from a clone of %s", want)
		}
		got, err1 := normalizeAXRepoURL(ctx, origin)
		norm, err2 := normalizeAXRepoURL(ctx, want)
		if err1 != nil || err2 != nil || !sameRepoURL(got, norm) {
			return fmt.Errorf("job %s ran on %s, but this checkout's origin is %s; run glue job pull from a clone of that repo", id, want, origin)
		}
	}
	res, err := c.Result(ctx, id)
	if err != nil {
		return err
	}
	if res.HeadSHA == "" || res.HeadSHA == res.BaseSHA {
		_, err := fmt.Fprintf(stdout, "job %s made no changes\n", id)
		return err
	}
	branch := res.Branch
	if o.apply {
		patch, err := c.Patch(ctx, id)
		if err != nil {
			return err
		}
		if err := applyPatch(ctx, dir, patch, o.force); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "applied job %s to the working tree (not committed):\n%s\n", id, strings.TrimRight(res.Diffstat, "\n"))
		_, err = fmt.Fprintln(stdout, "next: review with git diff, then commit")
		return err
	}
	if cur, _ := gitOutput(ctx, dir, "branch", "--show-current"); cur == branch {
		return fmt.Errorf("%s is the checked-out branch; switch to another branch before pulling into it", branch)
	}
	if _, err := gitOutput(ctx, dir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch); err == nil && !o.force {
		return fmt.Errorf("local branch %s already exists; delete it or pass --force to overwrite it", branch)
	}
	source := "origin"
	if _, err := gitOutput(ctx, dir, "fetch", "--quiet", "origin", "+refs/heads/"+branch+":refs/heads/"+branch); err != nil {
		patch, perr := c.Patch(ctx, id)
		if perr != nil {
			return fmt.Errorf("branch %s is not on origin and the patch is unavailable: %w", branch, perr)
		}
		if err := branchFromPatch(ctx, dir, branch, res.BaseSHA, patch, "Changes from Glue job "+id); err != nil {
			return fmt.Errorf("branch %s is not on origin; building it from the job patch failed: %w", branch, err)
		}
		source = "the job patch"
	}
	fmt.Fprintf(stdout, "created branch %s from %s:\n%s\n", branch, source, strings.TrimRight(res.Diffstat, "\n"))
	if o.checkout {
		if _, err := gitOutput(ctx, dir, "switch", "--quiet", branch); err != nil {
			return fmt.Errorf("switch to %s: %w (commit or stash local changes first)", branch, err)
		}
		_, err := fmt.Fprintf(stdout, "switched to %s\n", branch)
		return err
	}
	_, err = fmt.Fprintf(stdout, "next: git switch %s\n", branch)
	return err
}

// sameRepoURL compares normalized HTTPS repo URLs, ignoring a .git suffix and case.
func sameRepoURL(a, b string) bool {
	norm := func(s string) string { return strings.ToLower(strings.TrimSuffix(strings.TrimRight(s, "/"), ".git")) }
	return norm(a) == norm(b)
}

// applyPatch applies patch to the working tree in dir with a three-way
// merge. Without force it refuses when the patch touches locally changed files.
func applyPatch(ctx context.Context, dir string, patch []byte, force bool) error {
	// A patch that applies cleanly to the working tree as it is cannot lose
	// local work: this is the usual case after --snapshot, where the job
	// started from your uncommitted files.
	if _, err := gitWithStdin(ctx, dir, patch, "apply", "--check", "-"); err == nil {
		if _, err := gitWithStdin(ctx, dir, patch, "apply", "-"); err != nil {
			return fmt.Errorf("apply job patch: %w", err)
		}
		return nil
	}
	if !force {
		touched, err := gitWithStdin(ctx, dir, patch, "apply", "--numstat", "-")
		if err != nil {
			return err
		}
		status, err := gitOutput(ctx, dir, "status", "--porcelain", "--untracked-files=all")
		if err != nil {
			return err
		}
		var local []string
		for line := range strings.SplitSeq(status, "\n") {
			if len(line) > 3 {
				local = append(local, strings.Trim(line[3:], `"`))
			}
		}
		var clash []string
		for line := range strings.SplitSeq(strings.TrimSpace(touched), "\n") {
			if f := strings.Split(line, "\t"); len(f) == 3 && slices.Contains(local, f[2]) {
				clash = append(clash, f[2])
			}
		}
		if len(clash) > 0 {
			return fmt.Errorf("the job changed files you have local changes in: %s\ncommit or stash them, or pass --force", strings.Join(clash, ", "))
		}
	}
	if _, err := gitWithStdin(ctx, dir, patch, "apply", "--3way", "-"); err != nil {
		return fmt.Errorf("apply job patch: %w", err)
	}
	return nil
}

// branchFromPatch creates branch at base with patch committed on top,
// using a temporary worktree so the user's checkout is untouched.
func branchFromPatch(ctx context.Context, dir, branch, base string, patch []byte, msg string) error {
	tmp, err := os.MkdirTemp("", "glue-pull-*")
	if err != nil {
		return err
	}
	wt := filepath.Join(tmp, "wt")
	defer func() {
		_, _ = gitOutput(context.WithoutCancel(ctx), dir, "worktree", "remove", "--force", wt)
		_ = os.RemoveAll(tmp)
	}()
	if _, err := gitOutput(ctx, dir, "worktree", "add", "--quiet", "--force", "-B", branch, wt, base); err != nil {
		return fmt.Errorf("base commit %s is not in this checkout (git fetch origin first): %w", base, err)
	}
	if _, err := gitWithStdin(ctx, wt, patch, "apply", "--index", "-"); err != nil {
		return err
	}
	_, err = gitOutput(ctx, wt, "-c", "user.name=Glue", "-c", "user.email=glue@localhost", "commit", "--quiet", "-m", msg)
	return err
}

func gitWithStdin(ctx context.Context, dir string, stdin []byte, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}
