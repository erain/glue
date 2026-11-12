// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/erain/glue/jobs"
)

// defaultSnapshotMaxBytes caps the new file content a snapshot may push.
const defaultSnapshotMaxBytes = 50 << 20

// snapshotBranch is the origin branch that carries a job's snapshot.
func snapshotBranch(jobID string) string { return "glue/snapshot-" + jobID }

// snapshotRef is a snapshot commit built from a working tree.
type snapshotRef struct {
	sha, branch, of string
	files           int
	bytes           int64
}

// buildSnapshot commits the working tree in dir (tracked changes and
// untracked files that are not ignored, except .glue/) on top of HEAD. It
// uses a temporary index, so the user's index, branch, and stash are untouched.
func buildSnapshot(ctx context.Context, dir string, maxBytes int64) (snapshotRef, error) {
	var ref snapshotRef
	head, err := gitOutput(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return ref, fmt.Errorf("snapshot needs a commit to build on: %w", err)
	}
	ref.branch, _ = gitOutput(ctx, dir, "branch", "--show-current")
	ref.of = ref.branch + "@" + head
	idx, err := os.CreateTemp("", "glue-snapshot-index-*")
	if err != nil {
		return ref, err
	}
	idx.Close()
	defer os.Remove(idx.Name())
	env := append(os.Environ(), "GIT_INDEX_FILE="+idx.Name())
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		cmd.Env = env
		out, err := cmd.Output()
		if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
			err = fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(ee.Stderr)))
		}
		return strings.TrimSpace(string(out)), err
	}
	if _, err := run("read-tree", "HEAD"); err != nil {
		return ref, err
	}
	if _, err := run("add", "-A", "--", ".", ":(exclude).glue"); err != nil {
		return ref, err
	}
	tree, err := run("write-tree")
	if err != nil {
		return ref, err
	}
	changed, err := gitOutput(ctx, dir, "diff-tree", "-r", "--no-commit-id", "--diff-filter=AM", head, tree)
	if err != nil {
		return ref, err
	}
	for line := range strings.SplitSeq(changed, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		ref.files++
		if size, err := gitOutput(ctx, dir, "cat-file", "-s", f[3]); err == nil {
			n, _ := strconv.ParseInt(size, 10, 64)
			ref.bytes += n
		}
	}
	if maxBytes > 0 && ref.bytes > maxBytes {
		return ref, fmt.Errorf("snapshot adds %d MiB of file content, over the %d MiB limit; commit or ignore large files, or raise --snapshot-max-size", ref.bytes>>20, maxBytes>>20)
	}
	if ref.files == 0 {
		ref.sha = head
		return ref, nil
	}
	ref.sha, err = gitOutput(ctx, dir, "-c", "user.name=Glue", "-c", "user.email=glue@localhost", "commit-tree", tree, "-p", head, "-m", "Glue snapshot of "+ref.of)
	return ref, err
}

// pushSnapshot pushes the snapshot commit to origin as glue/snapshot-<id>.
func pushSnapshot(ctx context.Context, dir string, ref snapshotRef, jobID string) error {
	if _, err := gitOutput(ctx, dir, "push", "--quiet", "origin", ref.sha+":refs/heads/"+snapshotBranch(jobID)); err != nil {
		return fmt.Errorf("push snapshot to origin %s: %w", snapshotBranch(jobID), err)
	}
	return nil
}

// resolveSnapshotRepo snapshots the checkout in dir, pushes it, and returns
// a repo spec that starts the job from it.
func resolveSnapshotRepo(ctx context.Context, dir, jobID string, maxBytes int64) (jobs.RepoSpec, snapshotRef, error) {
	rs, err := resolveAXRepoSpec(ctx, dir, "")
	if err != nil {
		return jobs.RepoSpec{}, snapshotRef{}, err
	}
	ref, err := buildSnapshot(ctx, dir, maxBytes)
	if err != nil {
		return jobs.RepoSpec{}, ref, err
	}
	if err := pushSnapshot(ctx, dir, ref, jobID); err != nil {
		return jobs.RepoSpec{}, ref, err
	}
	of := ref.of
	if ref.branch == "" {
		of = cmp.Or(rs.BaseRef, "main") + ref.of
	}
	return jobs.RepoSpec{URL: rs.URL, BaseRef: snapshotBranch(jobID), SnapshotOf: of}, ref, nil
}

// deleteSnapshotBranch removes a job's snapshot branch from origin, if any.
func deleteSnapshotBranch(ctx context.Context, dir, jobID string) error {
	_, err := gitOutput(ctx, dir, "push", "--quiet", "origin", "--delete", "refs/heads/"+snapshotBranch(jobID))
	return err
}
