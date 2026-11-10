package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/internal/eventlog"
)

// ErrInvalid identifies a request validation failure.
var ErrInvalid = errors.New("jobs: invalid request")

func invalid(field, message string) error {
	return fmt.Errorf("%w: %s: %s", ErrInvalid, field, message)
}

// Filter selects jobs in creation-time/ID descending order. Zero Limit is unlimited.
type Filter struct {
	Status Status
	Limit  int
	Before string
}

// Detail adds durable goal progress to a job record.
type Detail struct {
	Job
	Iteration              int                  `json:"iteration"`
	Checklist              []glue.ChecklistItem `json:"checklist"`
	Usage                  glue.Usage           `json:"usage"`
	PendingPermissionCount int                  `json:"pending_permission_count"`
	Credentials            string               `json:"credentials,omitempty"`
}

// PermissionInfo is a request and its latest durable outcome.
type PermissionInfo struct {
	ID        string                 `json:"id"`
	Request   glue.PermissionRequest `json:"request"`
	State     string                 `json:"state"`
	CreatedAt time.Time              `json:"created_at"`
	UpdatedAt time.Time              `json:"updated_at"`
}

// Query applies filters and an exclusive Before job ID cursor.
func (m *Manager) Query(ctx context.Context, f Filter) ([]Job, error) {
	if f.Limit < 0 || f.Limit > 1000 {
		return nil, invalid("limit", "must be between 0 and 1000")
	}
	switch f.Status {
	case "", Created, Running, WaitingForInput, WaitingForCredentials, Suspended, Achieved, Blocked, BudgetLimited, MaxIterations, Errored, Canceled, Interrupted:
	default:
		return nil, invalid("status", "unknown status")
	}
	query := "SELECT " + columns + " FROM jobs WHERE 1=1"
	args := []any{}
	if f.Status != "" {
		query += " AND status=?"
		args = append(args, f.Status)
	}
	if f.Before != "" {
		cursor, err := m.Get(ctx, f.Before)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, invalid("before", "unknown job cursor")
			}
			return nil, err
		}
		query += " AND (created_at,id)<(?,?)"
		args = append(args, cursor.CreatedAt.Format("2006-01-02T15:04:05.000000000Z"), cursor.ID)
	}
	query += " ORDER BY created_at DESC,id DESC"
	if f.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, f.Limit)
	}
	rows, err := m.store.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := []Job{}
	for rows.Next() {
		job, err := scan(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}
func (m *Manager) snapshot(ctx context.Context, id string) (Detail, []PermissionInfo, error) {
	var detail Detail
	tx, err := m.store.DB.BeginTx(ctx, nil)
	if err != nil {
		return detail, nil, err
	}
	defer tx.Rollback()
	job, err := scan(tx.QueryRowContext(ctx, "SELECT "+columns+" FROM jobs WHERE id=?", id))
	if err != nil {
		return detail, nil, err
	}
	events, err := eventlog.ReadTx(tx, id, 0)
	if err != nil {
		return detail, nil, err
	}
	if err := tx.Commit(); err != nil {
		return detail, nil, err
	}
	return SummarizeEvents(job, events)
}

// SummarizeEvents folds ordered job events into a Detail and permission history.
func SummarizeEvents(job Job, events []eventlog.Envelope) (Detail, []PermissionInfo, error) {
	detail := Detail{Job: job, Checklist: []glue.ChecklistItem{}}
	permissions := []PermissionInfo{}
	index := map[string]int{}
	for _, event := range events {
		switch event.Type {
		case "plan", "iteration_start", "maker_done", "verdict", "done", "blocked", "budget_limited", "max_iterations":
			var progress glue.GoalEvent
			if err := decode(event.Payload, &progress); err != nil {
				return detail, nil, err
			}
			if progress.Iteration > detail.Iteration {
				detail.Iteration = progress.Iteration
			}
			if progress.Checklist != nil {
				detail.Checklist = progress.Checklist
			}
			if event.Type != "iteration_start" {
				detail.Usage = progress.Usage
			}
		case "permission_request":
			var p struct {
				ID      string                 `json:"permission_id"`
				Request glue.PermissionRequest `json:"request"`
			}
			if err := decode(event.Payload, &p); err != nil {
				return detail, nil, err
			}
			index[p.ID] = len(permissions)
			permissions = append(permissions, PermissionInfo{ID: p.ID, Request: p.Request, State: "pending", CreatedAt: event.Time, UpdatedAt: event.Time})
		case "permission_decision", "permission_expired":
			var p struct {
				ID         string                  `json:"permission_id"`
				OriginalID string                  `json:"original_permission_id"`
				Decision   glue.PermissionDecision `json:"decision"`
			}
			if err := decode(event.Payload, &p); err != nil {
				return detail, nil, err
			}
			if i, ok := index[p.ID]; ok {
				state := "expired"
				if event.Type == "permission_decision" {
					state = "denied"
					if p.Decision.Allow {
						state = "allowed"
					}
				}
				permissions[i].State = state
				permissions[i].UpdatedAt = event.Time
			}
			if p.OriginalID != "" {
				if i, ok := index[p.OriginalID]; ok && permissions[i].State == "pending" {
					permissions[i].State = "expired"
					permissions[i].UpdatedAt = event.Time
				}
			}
		case "job_done":
			var terminal struct {
				Result *glue.GoalResult `json:"result"`
			}
			if err := decode(event.Payload, &terminal); err != nil {
				return detail, nil, err
			}
			if terminal.Result != nil {
				detail.Iteration = terminal.Result.Iterations
				detail.Usage = terminal.Result.Usage
				if terminal.Result.Checklist != nil {
					detail.Checklist = terminal.Result.Checklist
				}
			}
			for i := range permissions {
				if permissions[i].State == "pending" {
					permissions[i].State = "expired"
					permissions[i].UpdatedAt = event.Time
				}
			}
		}
	}
	for _, p := range permissions {
		if p.State == "pending" {
			detail.PendingPermissionCount++
		}
	}
	return detail, permissions, nil
}
func decode(value, target any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

// Inspect reads job metadata and progress from one consistent snapshot.
func (m *Manager) Inspect(ctx context.Context, id string) (Detail, error) {
	detail, _, err := m.snapshot(ctx, id)
	if err == nil {
		detail.Credentials = "absent"
		if m.HasCredentials() {
			detail.Credentials = "present"
		}
	}
	return detail, err
}

// Permissions reads request history without starting or contacting an executor.
func (m *Manager) Permissions(ctx context.Context, id, state string) ([]PermissionInfo, error) {
	switch state {
	case "", "pending", "allowed", "denied", "expired":
	default:
		return nil, invalid("state", "unknown permission state")
	}
	_, all, err := m.snapshot(ctx, id)
	if err != nil {
		return nil, err
	}
	if state == "" {
		return all, nil
	}
	filtered := []PermissionInfo{}
	for _, p := range all {
		if p.State == state {
			filtered = append(filtered, p)
		}
	}
	return filtered, nil
}
