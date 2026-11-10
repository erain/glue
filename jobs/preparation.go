// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/erain/glue"
	"github.com/erain/glue/internal/worktree"
	filestore "github.com/erain/glue/stores/file"
)

func (m *Manager) startIntent(ctx context.Context, job Job) error {
	subdir := "worktrees"
	if job.Spec.Repo.URL != "" {
		subdir = "repos"
	}
	dir, err := filepath.Abs(filepath.Join(m.options.StateDir, subdir, job.ID))
	if err != nil {
		return err
	}
	agent := glue.NewAgent(glue.AgentOptions{Store: filestore.New(filepath.Join(m.options.StateDir, "sessions"))})
	if err := agent.SaveGoal(ctx, glue.GoalRecord{ID: job.ID, Objective: job.Spec.Objective, Status: glue.GoalRunning, WorkDir: dir}); err != nil {
		return err
	}
	m.eventsMu.Lock()
	defer m.eventsMu.Unlock()
	tx, err := m.store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE jobs SET workspace=?,goal_id=? WHERE id=?", dir, job.ID, job.ID); err != nil {
		return err
	}
	initialStatus := Running
	if m.missingCredentials(job.Spec, false) {
		initialStatus = WaitingForCredentials
	}
	if err := m.appendEvent(tx, job.ID, "job_started", nil, "", nil, initialStatus); err != nil {
		return err
	}
	return tx.Commit()
}

func (m *Manager) prepareWorkspace(ctx context.Context, job Job) (worktree.Workspace, error) {
	result := worktree.Workspace{Path: job.Workspace, BaseSHA: job.BaseSHA, Branch: "glue/" + job.ID}
	info, err := os.Lstat(result.Path)
	if os.IsNotExist(err) {
		if job.BaseSHA != "" {
			return result, fmt.Errorf("jobs: prepared workspace is missing")
		}
		result, err = worktree.Prepare(ctx, worktree.Options{Path: job.Spec.Repo.Path, URL: job.Spec.Repo.URL, BaseRef: job.Spec.Repo.BaseRef, Dir: job.Workspace, Branch: result.Branch})
		if err != nil {
			return result, err
		}
	} else {
		if err != nil {
			return result, err
		}
		if !info.IsDir() {
			return result, fmt.Errorf("jobs: workspace must be a directory, not a symlink or file")
		}
		git := func(args ...string) (string, error) {
			cmd := exec.CommandContext(ctx, "git", append([]string{"-C", result.Path}, args...)...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return "", fmt.Errorf("jobs: workspace git %s failed: %w", args[0], err)
			}
			return strings.TrimSpace(string(out)), nil
		}
		root, err := git("rev-parse", "--show-toplevel")
		if err != nil {
			return result, fmt.Errorf("jobs: partial workspace needs repair; contents preserved: %w", err)
		}
		actual, err := filepath.EvalSymlinks(root)
		if err != nil {
			return result, err
		}
		expected, err := filepath.EvalSymlinks(result.Path)
		if err != nil {
			return result, err
		}
		if actual != expected {
			return result, fmt.Errorf("jobs: workspace is not an independent checkout; contents preserved")
		}
		branch, err := git("symbolic-ref", "--short", "HEAD")
		if err != nil {
			return result, err
		}
		if branch != result.Branch {
			return result, fmt.Errorf("jobs: workspace branch mismatch; contents preserved")
		}
		if result.BaseSHA == "" {
			// No model can run until this metadata commits, so a recovered new
			// checkout must still be clean. Never reset or remove partial data.
			status, err := git("status", "--porcelain=v1")
			if err != nil {
				return result, err
			}
			if status != "" {
				return result, fmt.Errorf("jobs: incomplete workspace needs repair; contents preserved")
			}
			result.BaseSHA, err = git("rev-parse", "HEAD")
			if err != nil {
				return result, err
			}
		} else if _, err := git("merge-base", "--is-ancestor", result.BaseSHA, "HEAD"); err != nil {
			return result, err
		}
	}
	if _, err := m.store.DB.ExecContext(ctx, "UPDATE jobs SET base_sha=? WHERE id=?", result.BaseSHA, job.ID); err != nil {
		return result, err
	}
	return result, nil
}
