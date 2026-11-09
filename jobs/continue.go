// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/erain/glue"
	filestore "github.com/erain/glue/stores/file"
)

// Continue resumes a terminal unfinished job from its saved checklist and worktree.
// Zero maxIterations reuses the job's limit; positive values set a new run limit.
// As with glue goal --resume, token and wall-clock budgets restart for this run.
func (m *Manager) Continue(ctx context.Context, id, text string, maxIterations int) error {
	if maxIterations < 0 {
		return invalid("max_iterations", "must not be negative")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("jobs: manager is closed")
	}
	job, err := m.Get(ctx, id)
	if err != nil {
		return err
	}
	switch job.Status {
	case Blocked, MaxIterations, BudgetLimited, Interrupted:
	default:
		return fmt.Errorf("%w: job is not resumable", ErrConflict)
	}
	if job.GoalID == "" || job.Workspace == "" || job.BaseSHA == "" {
		return fmt.Errorf("%w: saved goal/workspace required", ErrConflict)
	}
	agent := glue.NewAgent(glue.AgentOptions{Store: filestore.New(filepath.Join(m.options.StateDir, "sessions"))})
	rec, ok, err := agent.LoadGoal(ctx, job.GoalID)
	if err != nil {
		return err
	}
	if !ok || len(rec.Checklist) == 0 || job.Workspace == "" || rec.WorkDir != job.Workspace || job.BaseSHA == "" {
		return fmt.Errorf("%w: saved goal/workspace required", ErrConflict)
	}
	info, err := os.Stat(job.Workspace)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("jobs: workspace is not a directory")
	}
	if maxIterations > 0 {
		job.Spec.Budgets.MaxIterations = maxIterations
	}
	data, err := json.Marshal(job.Spec)
	if err != nil {
		return err
	}
	m.eventsMu.Lock()
	defer m.eventsMu.Unlock()
	tx, err := m.store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE jobs SET spec_json=? WHERE id=?", string(data), id); err != nil {
		return err
	}
	if err := m.appendEvent(tx, id, "job_resumed", map[string]any{"from_iteration": rec.Iterations + 1, "reason": "continue"}, "", nil, Running); err != nil {
		return err
	}
	if text = strings.TrimSpace(text); text != "" {
		if err := m.appendEvent(tx, id, "operator_message", map[string]any{"text": text}, "", nil, ""); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	m.launch(job, &rec, false)
	return nil
}
