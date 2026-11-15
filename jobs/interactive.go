// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/erain/glue"
	filestore "github.com/erain/glue/stores/file"
)

// GoalPromoteRequest overrides job settings when escalating an interactive job to goal mode.
type GoalPromoteRequest struct {
	Objective string     `json:"objective,omitempty"`
	Verify    [][]string `json:"verify,omitempty"`
	Delivery  string     `json:"delivery,omitempty"`
	Budgets   Budgets    `json:"budgets,omitempty"`
}

func (m *Manager) parkInteractive(id string, run *execution) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	run.mu.Lock()
	defer run.mu.Unlock()
	if !m.closed && run.writeErr == nil && m.active[id] == run {
		delete(m.active, id)
		run.writeErr = m.record(id, "job_waiting", map[string]any{"reason": ReasonAwaitingTurn}, "chat", nil, WaitingForInput)
		return run.writeErr == nil
	}
	return false
}

func (m *Manager) runInteractiveSession(ctx context.Context, job Job, wsPath, baseSHA string, agent *glue.Agent, run *execution, emit func(string, any, string, *int)) (glue.GoalResult, error) {
	sess, err := agent.Session(ctx, job.ID+":chat")
	if err != nil {
		return glue.GoalResult{}, err
	}
	defer sess.Subscribe(func(e glue.Event) {
		if e.Type == glue.EventSteer {
			emit("operator_message_delivered", map[string]any{"text": steerMessageText(e)}, "chat", nil)
		} else {
			emit(string(e.Type), e, "chat", nil)
		}
	})()
	setMaker := func(s *glue.Session) { run.mu.Lock(); run.maker = s; run.mu.Unlock() }
	setMaker(sess)
	defer setMaker(nil)
	prompt := run.turnPrompt
	if len(sess.Messages()) == 0 && job.Spec.Context != "" {
		prompt += "\n\nContext from the developer's session:\n" + job.Spec.Context
	}
	if _, err = sess.Prompt(ctx, prompt); err == nil && ctx.Err() == nil && (job.Spec.Delivery == "branch" || job.Spec.Delivery == "draft_pr") && baseSHA != "" {
		if head, herr := workspaceGit(ctx, wsPath, nil, "rev-parse", "HEAD"); herr == nil && head != "" && head != baseSHA {
			_ = pushBranch(ctx, wsPath, "glue/"+job.ID, m.githubToken())
		}
	}
	return glue.GoalResult{}, err
}

// PromoteToGoal escalates an idle interactive job into autonomous goal pursuit on the same workspace.
func (m *Manager) PromoteToGoal(ctx context.Context, id string, req GoalPromoteRequest) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || ctx.Err() != nil {
		return Job{}, cmp.Or(ctx.Err(), errors.New("jobs: manager is closed"))
	}
	job, err := m.Get(ctx, id)
	if err != nil || !job.Spec.isInteractive() || m.active[id] != nil || (job.Status != WaitingForInput && job.Status != Created && job.Status != Interrupted && job.Status != Blocked) {
		return Job{}, cmp.Or(err, fmt.Errorf("%w: promote requires an idle interactive job", ErrConflict))
	}
	spec := job.Spec
	spec.Mode, spec.Context = ModeGoal, summarizeChatContext(ctx, filepath.Join(m.options.StateDir, "sessions"), id+":chat", spec.Context)
	spec.Objective, spec.Delivery = cmp.Or(strings.TrimSpace(req.Objective), spec.Objective), cmp.Or(strings.TrimSpace(req.Delivery), spec.Delivery)
	if req.Verify != nil {
		spec.Verify = req.Verify
	}
	spec.Budgets = Budgets{cmp.Or(max(0, req.Budgets.MaxIterations), spec.Budgets.MaxIterations), cmp.Or(max(0, req.Budgets.TokenBudget), spec.Budgets.TokenBudget), cmp.Or(max(0, req.Budgets.WallClock), spec.Budgets.WallClock)}
	if job.Spec, err = m.validate(spec); err == nil {
		if err = m.commitIntent(ctx, job, "mode_promoted", map[string]any{"spec": job.Spec}); err == nil {
			if job, err = m.Get(ctx, id); err == nil {
				m.launch(job, nil, false)
			}
		}
	}
	return job, err
}

func summarizeChatContext(ctx context.Context, dir, sessionID, existing string) string {
	state, ok, _ := filestore.New(dir).Load(ctx, sessionID)
	var lines []string
	for _, m := range state.Messages {
		if ok && !glue.IsEnvContextMessage(m) && (m.Role == glue.MessageRoleUser || m.Role == glue.MessageRoleAssistant) {
			var text string
			for _, p := range m.Content {
				if p.Type == glue.ContentTypeText {
					text += p.Text
				}
			}
			if text = strings.TrimSpace(text); text != "" {
				lines = append(lines, string(m.Role)+": "+text)
			}
		}
	}
	if len(lines) > 0 {
		existing = strings.TrimSpace(strings.TrimSpace(existing) + "\n\nInteractive session transcript:\n" + strings.Join(lines, "\n\n"))
	}
	for len(existing) > MaxContextBytes {
		_, n := utf8.DecodeRuneInString(existing)
		existing = existing[max(1, n):]
	}
	return existing
}
