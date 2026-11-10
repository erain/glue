// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/erain/glue"
	filestore "github.com/erain/glue/stores/file"
)

type recovery struct {
	job  Job
	goal glue.GoalRecord
}

func (m *Manager) recoverJobs() error {
	ctx := context.Background()
	all, err := m.List(ctx)
	if err != nil {
		return err
	}
	var plans []recovery
	for _, job := range all {
		if job.Status != Running && job.Status != WaitingForInput && job.Status != WaitingForCredentials {
			continue
		}
		plan, err := m.recoverJob(ctx, job)
		if err != nil {
			if writeErr := m.finalize(job.ID, glue.GoalResult{}, Errored); writeErr != nil {
				return writeErr
			}
			if writeErr := m.record(job.ID, "job_done", map[string]any{"status": Errored, "error": err.Error()}, "", nil, Errored); writeErr != nil {
				return writeErr
			}
			continue
		}
		if plan != nil {
			plans = append(plans, *plan)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, plan := range plans {
		m.launch(plan.job, &plan.goal, true)
	}
	return nil
}
func (m *Manager) recoverJob(ctx context.Context, job Job) (*recovery, error) {
	store := filestore.New(filepath.Join(m.options.StateDir, "sessions"))
	agent := glue.NewAgent(glue.AgentOptions{Store: store})
	rec, ok, err := agent.LoadGoal(ctx, job.GoalID)
	if err != nil {
		return nil, err
	}
	if !ok || job.Workspace == "" || rec.WorkDir != job.Workspace {
		return nil, fmt.Errorf("jobs: recovery requires saved goal/workspace identity")
	}
	events, err := m.Events(ctx, job.ID, 0)
	if err != nil {
		return nil, err
	}
	first, started, reset := 1, job.CreatedAt, false
	for _, e := range events {
		switch e.Type {
		case "job_started":
			started = e.Time
			reset = true
		case "job_resumed":
			var p struct {
				Reason string `json:"reason"`
				From   int    `json:"from_iteration"`
			}
			if err := decode(e.Payload, &p); err != nil {
				return nil, err
			}
			if p.Reason != "restart" {
				first = p.From
				started = e.Time
				reset = true
			}
		case "plan":
			reset = false
		}
	}
	if reset {
		rec.Usage = glue.Usage{}
		rec.Status = glue.GoalRunning
	}
	pending, err := m.Permissions(ctx, job.ID, "pending")
	if err != nil {
		return nil, err
	}
	m.eventsMu.Lock()
	tx, err := m.store.DB.BeginTx(ctx, nil)
	if err == nil {
		defer tx.Rollback()
		for _, p := range pending {
			if err = m.appendEvent(tx, job.ID, "permission_expired", map[string]any{"permission_id": p.ID}, "", nil, ""); err != nil {
				break
			}
		}
		if err == nil {
			resumedStatus := Running
			if m.missingCredentials(job.Spec, false) {
				resumedStatus = WaitingForCredentials
			}
			err = m.appendEvent(tx, job.ID, "job_resumed", map[string]any{"from_iteration": rec.Iterations + 1, "reason": "restart"}, "", nil, resumedStatus)
		}
		if err == nil {
			err = tx.Commit()
		}
	}
	m.eventsMu.Unlock()
	if err != nil {
		return nil, err
	}
	status := Status("")
	switch rec.Status {
	case glue.GoalAchieved, glue.GoalBlocked, glue.GoalMaxIterations, glue.GoalBudgetLimited, glue.GoalErrored:
		status = Status(rec.Status)
	}
	remaining := job.Spec.Budgets.MaxIterations - (rec.Iterations - first + 1)
	if status == "" && remaining <= 0 {
		status = MaxIterations
	}
	if status == "" && job.Spec.Budgets.TokenBudget > 0 && rec.Usage.TotalTokens >= job.Spec.Budgets.TokenBudget {
		status = BudgetLimited
	}
	if job.Spec.Budgets.WallClock > 0 {
		job.Spec.Budgets.WallClock -= m.options.Now().Sub(started)
		if status == "" && job.Spec.Budgets.WallClock <= 0 {
			status = BudgetLimited
		}
	}
	if status != "" {
		result := glue.GoalResult{Objective: rec.Objective, Status: glue.GoalStatus(status), Iterations: rec.Iterations, Checklist: rec.Checklist, Usage: rec.Usage, Summary: rec.Summary}
		if err := m.finalize(job.ID, result, status); err != nil {
			return nil, err
		}
		return nil, m.record(job.ID, "job_done", map[string]any{"status": status, "result": result}, "", nil, status)
	}
	job.Spec.Budgets.MaxIterations = remaining
	// An abandoned iteration's session transcript must not become the new maker's
	// history. Its events and filesystem edits remain durable; replay starts fresh.
	for _, id := range []string{fmt.Sprintf("%s:iter-%d", job.ID, rec.Iterations+1), fmt.Sprintf("%s:check-%d", job.ID, rec.Iterations+1)} {
		if err := store.Delete(ctx, id); err != nil {
			return nil, err
		}
	}
	if len(rec.Checklist) == 0 {
		if err := store.Delete(ctx, job.ID+":plan"); err != nil {
			return nil, err
		}
	}
	return &recovery{job: job, goal: rec}, nil
}
