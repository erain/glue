package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/erain/glue"
	"github.com/erain/glue/internal/eventlog"
	"github.com/erain/glue/providers"
	filestore "github.com/erain/glue/stores/file"
	"github.com/erain/glue/tools/coding"
)

type execution struct {
	budgets                 Budgets
	initialUsage            glue.Usage
	resume                  *glue.GoalRecord
	ctx                     context.Context
	pending                 map[string]*pendingDecision
	stopTimer               func() bool
	finished, budgetExpired bool
	mu                      sync.Mutex
	cancel                  context.CancelFunc
	writeErr                error
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
	job, err := m.Get(ctx, id)
	if err != nil {
		return err
	}
	if job.Status != Created {
		return fmt.Errorf("%w: job has already started", ErrConflict)
	}
	if err := m.startIntent(ctx, job); err != nil {
		return err
	}
	m.launch(job, nil, false)
	return nil
}

// launch requires m.mu; the durable transition must already be committed.
func (m *Manager) launch(job Job, resume *glue.GoalRecord, recovering bool) {
	id := job.ID
	runCtx, cancel := context.WithCancel(context.Background())
	run := &execution{ctx: runCtx, cancel: cancel, resume: resume, budgets: job.Spec.Budgets}
	if recovering && resume != nil {
		run.initialUsage = resume.Usage
	}
	m.budget(run, job.Spec.Budgets.WallClock)
	m.active[id] = run
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()
		m.execute(runCtx, id, run)
		m.mu.Lock()
		if m.active[id] == run {
			delete(m.active, id)
		}
		m.mu.Unlock()
	}()
}

// Cancel stops one active execution. It does not cancel other jobs.
func (m *Manager) Cancel(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	job, err := m.Get(ctx, id)
	if err != nil {
		return err
	}
	if job.Status == Created {
		if err := m.finalize(id, glue.GoalResult{}, Canceled); err != nil {
			return err
		}
		return m.record(id, "job_done", map[string]any{"status": Canceled}, "", nil, Canceled)
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
		m.closeErr = errors.Join(m.closeErr, releaseLock(m.lock))
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
	if err := m.appendEvent(tx, id, typ, payload, phase, iteration, status); err != nil {
		return err
	}
	return tx.Commit()
}

func (m *Manager) appendEvent(tx *sql.Tx, id, typ string, payload any, phase string, iteration *int, status Status) error {
	var seq int64
	var current Status
	if err := tx.QueryRow("SELECT last_seq,status FROM jobs WHERE id=?", id).Scan(&seq, &current); err != nil {
		return err
	}
	if typ == "operator_message" && current != Created && current != Running && current != WaitingForInput {
		return fmt.Errorf("%w: guidance requires a created or active job", ErrConflict)
	}
	if typ == "job_started" && current != Created {
		return fmt.Errorf("%w: job has already started", ErrConflict)
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
	return nil
}
func (m *Manager) execute(ctx context.Context, id string, run *execution) {
	result, err := m.pursue(ctx, id, run)
	run.mu.Lock()
	run.finished = true
	budgetExpired := run.budgetExpired
	stop := run.stopTimer
	run.mu.Unlock()
	if stop != nil {
		stop()
	}
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
	if budgetExpired {
		status = BudgetLimited
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
	if finErr := m.finalize(id, result, status); finErr != nil {
		run.mu.Lock()
		run.writeErr = errors.Join(run.writeErr, finErr)
		run.mu.Unlock()
		m.mu.Lock()
		m.closeErr = errors.Join(m.closeErr, finErr)
		m.mu.Unlock()
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
	workspace, err := m.prepareWorkspace(ctx, job)
	if err != nil {
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
	tools, _, err := coding.Tools(m.codingOptions(id, job.Spec, workspace.Path, workspace.BaseSHA))
	if err != nil {
		return empty, err
	}
	options, err := providers.ThinkingOptions(job.Spec.Provider, job.Spec.Model, job.Spec.Thinking)
	if err != nil {
		return empty, err
	}
	caps := providers.CapabilitiesFor(job.Spec.Provider)
	agent := glue.NewAgent(glue.AgentOptions{Provider: provider, Model: job.Spec.Model, Options: options, Tools: tools, WorkDir: workspace.Path, Store: filestore.New(filepath.Join(m.options.StateDir, "sessions")), Permission: jobPermission{manager: m, id: id, run: run}, SystemPrompt: coding.SystemPrompt(tools, caps.PromptVariant), AutoContinue: caps.AutoContinue})
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
	spec := glue.GoalSpec{RequirePersistence: true, Objective: job.Spec.Objective, SessionPrefix: id, WorkDir: workspace.Path, MaxIterations: run.budgets.MaxIterations, TokenBudget: run.budgets.TokenBudget, InitialUsage: run.initialUsage,
		Guidance: func(ctx context.Context, iteration int) (string, error) { return m.guidance(ctx, id, iteration) },
		Emit:     func(e glue.GoalEvent) { emit(string(e.Type), e, "", &e.Iteration) },
		OnSession: func(phase string, iteration int, s *glue.Session) {
			unsubscribe = append(unsubscribe, s.Subscribe(func(e glue.Event) { emit(string(e.Type), e, phase, &iteration) }))
		},
	}
	if run.resume != nil {
		spec.Checklist = run.resume.Checklist
		spec.StartIteration = run.resume.Iterations + 1
	}
	return agent.PursueGoal(ctx, spec)
}

func (m *Manager) codingOptions(id string, spec Spec, workspacePath, baseSHA string) coding.Options {
	bins, envPass, timeout := slices.Clone(spec.AllowedBinaries), spec.ShellEnvPass, spec.ShellTimeout
	if len(bins) == 0 {
		bins = DefaultAllowedBinaries
		if len(m.options.AllowedBinaries) > 0 {
			bins = m.options.AllowedBinaries
		}
	}
	if envPass == nil {
		envPass = m.options.ShellEnvPass
	}
	if timeout <= 0 {
		timeout = m.options.ShellTimeout
	}
	secretAllow := slices.DeleteFunc(append(append([]string(nil), m.options.ShellEnvSecretAllow...), spec.ShellEnvSecretAllow...), func(s string) bool {
		return strings.EqualFold(strings.TrimSpace(s), "GLUE_GITHUB_TOKEN")
	})
	return coding.Options{
		Enabled: true, WorkDir: workspacePath, AllowedBinaries: bins, AllowOverwrite: true,
		EnvPass: envPass, Env: append(append([]string(nil), m.options.ShellEnv...), spec.ShellEnv...),
		SecretAllow: secretAllow,
		DefaultHome: filepath.Join(m.options.StateDir, "home", id), SpoolDir: filepath.Join(m.options.StateDir, "tmp"),
		ShellTimeout: timeout, GitDefaultBase: baseSHA,
	}
}
