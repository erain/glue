package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/erain/glue"
)

// ErrConflict means a requested lifecycle or permission transition is no longer valid.
var ErrConflict = errors.New("jobs: transition conflict")

type pendingDecision struct {
	req  glue.PermissionRequest
	done chan glue.PermissionDecision
}
type jobPermission struct {
	manager *Manager
	id      string
	run     *execution
}

func (p jobPermission) Decide(ctx context.Context, request glue.PermissionRequest) (glue.PermissionDecision, error) {
	r, m := p.run, p.manager
	r.mu.Lock()
	job, err := m.Get(ctx, p.id)
	if err != nil {
		r.mu.Unlock()
		return glue.PermissionDecision{}, err
	}
	if scope, ok := r.allows[request.Tool+"\x00"+request.Action+"\x00"+request.Target]; ok {
		r.mu.Unlock()
		return glue.PermissionDecision{Allow: true, RememberFor: scope}, nil
	}
	if m.options.Permission == nil {
		policy, err := m.policyFor(job.Spec.Policy, job.Spec.AllowedBinaries...)
		if err != nil {
			r.mu.Unlock()
			return glue.PermissionDecision{}, err
		}
		if eval := policy.Evaluate(job.Workspace, request); eval.Decision != DecisionAsk {
			if err := m.record(p.id, "permission_auto", map[string]any{"tool": request.Tool, "target": request.Target, "decision": eval.Decision, "rule": eval.Rule}, "", nil, ""); err != nil {
				r.writeErr = err
				r.cancel()
				r.mu.Unlock()
				return glue.PermissionDecision{}, err
			}
			r.mu.Unlock()
			if eval.Decision == DecisionAllow {
				return glue.PermissionDecision{Allow: true}, nil
			}
			return glue.PermissionDecision{Allow: false, Reason: "permission denied by policy (" + eval.Rule + ")"}, nil
		}
	}
	id := fmt.Sprintf("%s:permission:%d", p.id, job.LastSeq+1)
	pending := &pendingDecision{req: request, done: make(chan glue.PermissionDecision, 1)}
	if r.pending == nil {
		r.pending = map[string]*pendingDecision{}
	}
	r.pending[id] = pending
	err = m.record(p.id, "permission_request", map[string]any{"permission_id": id, "request": request}, "", nil, WaitingForInput)
	if err == nil && len(r.pending) == 1 {
		err = m.record(p.id, "job_waiting", nil, "", nil, WaitingForInput)
	}
	if err != nil {
		delete(r.pending, id)
		r.writeErr = err
		r.cancel()
		r.mu.Unlock()
		return glue.PermissionDecision{}, err
	}
	r.mu.Unlock()
	if handler := m.options.Permission; handler != nil {
		decision, err := handler.Decide(ctx, request)
		if err != nil {
			return p.expire(id, pending, err)
		}
		if err := m.Decide(ctx, p.id, id, decision); err != nil {
			return p.expire(id, pending, err)
		}
	}
	select {
	case decision := <-pending.done:
		return decision, nil
	case <-ctx.Done():
		return p.expire(id, pending, ctx.Err())
	}
}
func (p jobPermission) expire(id string, pending *pendingDecision, cause error) (glue.PermissionDecision, error) {
	r := p.run
	r.mu.Lock()
	if r.pending[id] != pending {
		r.mu.Unlock()
		return <-pending.done, nil
	}
	status := Running
	if len(r.pending) > 1 {
		status = WaitingForInput
	}
	if err := p.manager.record(p.id, "permission_expired", map[string]any{"permission_id": id}, "", nil, status); err != nil {
		r.writeErr = err
		r.cancel()
		cause = err
	}
	delete(r.pending, id)
	r.mu.Unlock()
	return glue.PermissionDecision{}, cause
}

// Decide durably records a pending permission decision before releasing its tool.
func (m *Manager) Decide(ctx context.Context, id, permissionID string, decision glue.PermissionDecision) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	r := m.active[id]
	m.mu.Unlock()
	if r == nil {
		return ErrConflict
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	pending := r.pending[permissionID]
	if pending == nil || r.ctx.Err() != nil {
		return ErrConflict
	}
	status := Running
	if len(r.pending) > 1 {
		status = WaitingForInput
	}
	if err := m.record(id, "permission_decision", map[string]any{"permission_id": permissionID, "decision": decision}, "", nil, status); err != nil {
		return err
	}
	if decision.Allow && decision.RememberFor != glue.RememberNever {
		if r.allows == nil {
			r.allows = map[string]glue.RememberScope{}
		}
		r.allows[pending.req.Tool+"\x00"+pending.req.Action+"\x00"+pending.req.Target] = decision.RememberFor
	}
	delete(r.pending, permissionID)
	pending.done <- decision
	return nil
}
func (m *Manager) budget(run *execution, d time.Duration) {
	if d == 0 {
		return
	}
	after := m.options.AfterFunc
	if after == nil {
		after = func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }
	}
	run.stopTimer = after(d, func() {
		run.mu.Lock()
		defer run.mu.Unlock()
		if !run.finished && run.ctx.Err() == nil {
			run.budgetExpired = true
			run.cancel()
		}
	})
}
