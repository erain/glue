package jobs

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/erain/glue"
	"github.com/erain/glue/internal/eventlog"
	"github.com/erain/glue/internal/worktree"
	"github.com/erain/glue/providers"
	filestore "github.com/erain/glue/stores/file"
	"github.com/erain/glue/tools/coding"
)

type execution struct {
	mu       sync.Mutex
	cancel   context.CancelFunc
	writeErr error
}

// Start detaches execution from the caller after durably recording its start.
func (m *Manager) Start(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("jobs: manager is closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.record(id, "job_started", nil, "", nil, Running); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	run := &execution{cancel: cancel}
	m.active[id] = run
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()
		m.execute(runCtx, id, run)
		m.mu.Lock()
		delete(m.active, id)
		m.mu.Unlock()
	}()
	return nil
}

// Cancel stops one active execution. It does not cancel other jobs.
func (m *Manager) Cancel(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.Get(ctx, id); err != nil {
		return err
	}
	if run := m.active[id]; run != nil {
		run.cancel()
	}
	return nil
}

// Close stops active executions and waits for their final writes before closing storage.
func (m *Manager) Close() error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		for _, run := range m.active {
			run.cancel()
		}
		m.mu.Unlock()
		m.wg.Wait()
		m.closeErr = errors.Join(m.closeErr, m.store.DB.Close())
	})
	return m.closeErr
}

func (m *Manager) record(id, typ string, payload any, phase string, iteration *int, status Status) error {
	m.eventsMu.Lock()
	defer m.eventsMu.Unlock()
	tx, err := m.store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var seq int64
	var current Status
	if err := tx.QueryRow("SELECT last_seq,status FROM jobs WHERE id=?", id).Scan(&seq, &current); err != nil {
		return err
	}
	if typ == "job_started" && current != Created {
		return errors.New("jobs: job has already started")
	}
	if status == "" {
		status = current
	}
	seq++
	now := m.options.Now().UTC()
	event := eventlog.Envelope{Version: 2, ID: fmt.Sprintf("%s:%d", id, seq), RunID: id, Seq: seq, Time: now, Type: typ, Payload: payload, Phase: phase, Iteration: iteration}
	if err := eventlog.Insert(tx, event); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE jobs SET status=?,last_seq=?,updated_at=? WHERE id=?", status, seq, now.Format("2006-01-02T15:04:05.000000000Z"), id); err != nil {
		return err
	}
	return tx.Commit()
}
func (m *Manager) execute(ctx context.Context, id string, run *execution) {
	result, err := m.pursue(ctx, id, run)
	status := Status(result.Status)
	if err != nil || status == "" {
		status = Errored
	}
	if ctx.Err() != nil {
		status = Canceled
		m.mu.Lock()
		if m.closed {
			status = Interrupted
		}
		m.mu.Unlock()
	}
	payload := map[string]any{"status": status, "result": result}
	if err != nil {
		payload["error"] = err.Error()
	}
	run.mu.Lock()
	writeErr := run.writeErr
	run.mu.Unlock()
	if writeErr != nil {
		payload["error"] = writeErr.Error()
		status = Errored
		payload["status"] = status
	}
	if writeErr := m.record(id, "job_done", payload, "", nil, status); writeErr != nil {
		run.mu.Lock()
		run.writeErr = writeErr
		run.mu.Unlock()
		m.mu.Lock()
		m.closeErr = errors.Join(m.closeErr, writeErr)
		m.mu.Unlock()
	}
}
func (m *Manager) pursue(ctx context.Context, id string, run *execution) (glue.GoalResult, error) {
	var empty glue.GoalResult
	job, err := m.Get(ctx, id)
	if err != nil {
		return empty, err
	}
	subdir := "worktrees"
	if job.Spec.Repo.URL != "" {
		subdir = "repos"
	}
	workspace, err := worktree.Prepare(ctx, worktree.Options{Path: job.Spec.Repo.Path, URL: job.Spec.Repo.URL, BaseRef: job.Spec.Repo.BaseRef, Dir: filepath.Join(m.options.StateDir, subdir, id), Branch: "glue/" + id})
	if err != nil {
		return empty, err
	}
	if _, err := m.store.DB.ExecContext(ctx, "UPDATE jobs SET workspace=?,base_sha=?,goal_id=? WHERE id=?", workspace.Path, workspace.BaseSHA, id, id); err != nil {
		return empty, err
	}
	factory := m.options.ProviderFactory
	if factory == nil {
		factory = func(name string) (glue.Provider, error) { p, _, _, err := providers.New(name); return p, err }
	}
	provider, err := factory(job.Spec.Provider)
	if err != nil {
		return empty, err
	}
	if provider == nil {
		return empty, errors.New("jobs: provider factory returned nil")
	}
	tools, _, err := coding.Tools(coding.Options{Enabled: true, WorkDir: workspace.Path, AllowOverwrite: true, GitDefaultBase: workspace.BaseSHA})
	if err != nil {
		return empty, err
	}
	options, err := providers.ThinkingOptions(job.Spec.Provider, job.Spec.Model, job.Spec.Thinking)
	if err != nil {
		return empty, err
	}
	caps := providers.CapabilitiesFor(job.Spec.Provider)
	agent := glue.NewAgent(glue.AgentOptions{Provider: provider, Model: job.Spec.Model, Options: options, Tools: tools, WorkDir: workspace.Path, Store: filestore.New(filepath.Join(m.options.StateDir, "sessions")), Permission: m.options.Permission, SystemPrompt: coding.SystemPrompt(tools, caps.PromptVariant), AutoContinue: caps.AutoContinue})
	emit := func(typ string, payload any, phase string, iteration *int) {
		run.mu.Lock()
		defer run.mu.Unlock()
		if run.writeErr != nil {
			return
		}
		if err := m.record(id, typ, payload, phase, iteration, ""); err != nil {
			run.writeErr = err
			run.cancel()
		}
	}
	var unsubscribe []func()
	defer func() {
		for _, stop := range unsubscribe {
			stop()
		}
	}()
	return agent.PursueGoal(ctx, glue.GoalSpec{Objective: job.Spec.Objective, SessionPrefix: id, WorkDir: workspace.Path, MaxIterations: job.Spec.Budgets.MaxIterations, TokenBudget: job.Spec.Budgets.TokenBudget,
		Emit: func(e glue.GoalEvent) { emit(string(e.Type), e, "", &e.Iteration) },
		OnSession: func(phase string, iteration int, s *glue.Session) {
			unsubscribe = append(unsubscribe, s.Subscribe(func(e glue.Event) { emit(string(e.Type), e, phase, &iteration) }))
		},
	})
}
