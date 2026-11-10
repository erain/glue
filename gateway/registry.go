// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package gateway

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/erain/glue/internal/eventlog"
	"github.com/erain/glue/jobs"
)

const (
	registrySchema = `CREATE TABLE IF NOT EXISTS jobs (id TEXT PRIMARY KEY, backend TEXT NOT NULL, backend_ref TEXT NOT NULL, owner TEXT NOT NULL, spec_json TEXT NOT NULL, status TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, last_seq INTEGER NOT NULL, result_json TEXT NOT NULL);`
	jobColumns     = "id,backend,backend_ref,owner,spec_json,status,created_at,updated_at,last_seq,result_json"
)

// SubmitUnknown is recorded when an AX CreateTask call fails with an ambiguous error.
const SubmitUnknown jobs.Status = "submit_unknown"

var (
	validStatuses = []jobs.Status{"", jobs.Created, SubmitUnknown, jobs.Running, jobs.WaitingForInput, jobs.Achieved, jobs.Blocked, jobs.BudgetLimited, jobs.MaxIterations, jobs.Errored, jobs.Canceled, jobs.Interrupted}
	eventStatuses = map[string]jobs.Status{
		"job_created": jobs.Created, "job_started": jobs.Running, "job_resumed": jobs.Running,
		"permission_decision": jobs.Running, "permission_expired": jobs.Running,
		"permission_request": jobs.WaitingForInput, "job_waiting": jobs.WaitingForInput,
	}
)

// Record is one job row in the gateway SQLite registry.
type Record struct {
	ID, Backend, BackendRef, Owner string
	Spec                           jobs.Spec
	Status                         jobs.Status
	CreatedAt, UpdatedAt           time.Time
	LastSeq                        int64
	Result                         *jobs.Result
}

func (r Record) Job() jobs.Job {
	spec := r.Spec
	spec.Backend = r.Backend
	return jobs.Job{ID: r.ID, Spec: spec, Status: r.Status, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, LastSeq: r.LastSeq}
}

type Registry struct {
	mu    sync.Mutex
	store *eventlog.Store
}

func OpenRegistry(stateDir string) (*Registry, error) {
	if stateDir = strings.TrimSpace(stateDir); stateDir == "" {
		return nil, errors.New("gateway: stateDir is required")
	}
	store, err := eventlog.Open(stateDir, registrySchema)
	if err == nil {
		store.DB.SetMaxOpenConns(1)
	}
	return &Registry{store: store}, err
}
func (r *Registry) Close() error { return r.store.DB.Close() }
func fmtTime(t time.Time) string { return cmp.Or(t, time.Now()).UTC().Format(time.RFC3339Nano) }
func mustJSON(v any) string      { b, _ := json.Marshal(v); return string(b) }
func (r *Registry) CreateJob(ctx context.Context, rec Record) (Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec.Status = cmp.Or(rec.Status, jobs.Created)
	rec.CreatedAt = cmp.Or(rec.CreatedAt, time.Now()).UTC()
	rec.UpdatedAt = cmp.Or(rec.UpdatedAt, rec.CreatedAt).UTC()
	_, err := r.store.DB.ExecContext(ctx, "INSERT INTO jobs("+jobColumns+") VALUES(?,?,?,?,?,?,?,?,?,'')",
		rec.ID, rec.Backend, rec.BackendRef, rec.Owner, mustJSON(rec.Spec), rec.Status,
		fmtTime(rec.CreatedAt), fmtTime(rec.UpdatedAt), rec.LastSeq)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return Record{}, fmt.Errorf("%w: job %s already exists", jobs.ErrConflict, rec.ID)
	}
	return rec, err
}
func (r *Registry) UpsertStatus(ctx context.Context, id, backend string, spec jobs.Spec, st jobs.Status) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := fmtTime(time.Time{})
	_, err := r.store.DB.ExecContext(ctx, "INSERT INTO jobs("+jobColumns+") VALUES(?,?,?,'dev-token',?,?,?,?,0,'') ON CONFLICT(id) DO UPDATE SET status=excluded.status,updated_at=excluded.updated_at", id, backend, id, mustJSON(spec), st, now, now)
	return err
}
func scanRecord(row interface{ Scan(...any) error }) (Record, error) {
	var rec Record
	var specJSON, created, updated, resultJSON string
	err := row.Scan(&rec.ID, &rec.Backend, &rec.BackendRef, &rec.Owner, &specJSON, &rec.Status, &created, &updated, &rec.LastSeq, &resultJSON)
	_ = json.Unmarshal([]byte(specJSON), &rec.Spec)
	rec.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	rec.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	if err == nil && strings.TrimSpace(resultJSON) != "" {
		rec.Result = &jobs.Result{Verify: []jobs.VerifyResult{}}
		err = json.Unmarshal([]byte(resultJSON), rec.Result)
	}
	return rec, err
}
func (r *Registry) GetJob(ctx context.Context, id string) (Record, error) {
	return scanRecord(r.store.DB.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE id=?", id))
}
func (r *Registry) QueryJobs(ctx context.Context, f jobs.Filter) ([]jobs.Job, error) {
	if f.Limit < 0 || f.Limit > 1000 || !slices.Contains(validStatuses, f.Status) {
		return nil, fmt.Errorf("%w: invalid filter", jobs.ErrInvalid)
	}
	if _, err := r.GetJob(ctx, f.Before); f.Before != "" && err != nil {
		return nil, fmt.Errorf("%w: before: %w", jobs.ErrInvalid, err)
	}
	rows, err := r.store.DB.QueryContext(ctx,
		"SELECT "+jobColumns+" FROM jobs WHERE (?='' OR status=?) AND (?='' OR (created_at,id)<((SELECT created_at FROM jobs WHERE id=?),?)) ORDER BY created_at DESC,id DESC LIMIT ?",
		f.Status, f.Status, f.Before, f.Before, f.Before, cmp.Or(f.Limit, 1000))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []jobs.Job{}
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec.Job())
	}
	return out, rows.Err()
}
func (r *Registry) SaveResult(ctx context.Context, id string, result jobs.Result) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if result.Verify == nil {
		result.Verify = []jobs.VerifyResult{}
	}
	res, err := r.store.DB.ExecContext(ctx,
		"UPDATE jobs SET result_json=?,status=CASE WHEN ? != '' THEN ? ELSE status END,updated_at=? WHERE id=?",
		mustJSON(result), result.Status, result.Status, fmtTime(time.Time{}), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
func (r *Registry) AppendEvent(ctx context.Context, jobID string, ev eventlog.Envelope) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ev.RunID = jobID
	ins, err := r.store.DB.ExecContext(ctx, "INSERT OR IGNORE INTO events(run_id,seq,payload_json) VALUES(?,?,?)", jobID, ev.Seq, mustJSON(ev))
	if err != nil {
		return false, err
	}
	if affected, _ := ins.RowsAffected(); affected == 0 {
		return false, nil
	}
	st := statusFromEvent(ev)
	res, err := r.store.DB.ExecContext(ctx,
		"UPDATE jobs SET status=CASE WHEN ?='' THEN status ELSE ? END,last_seq=MAX(last_seq,?),updated_at=? WHERE id=?",
		st, st, ev.Seq, fmtTime(ev.Time), jobID)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, sql.ErrNoRows
	}
	return true, nil
}
func statusFromEvent(ev eventlog.Envelope) jobs.Status {
	if ev.Type == "job_done" {
		var p struct{ Status jobs.Status }
		_ = json.Unmarshal([]byte(mustJSON(ev.Payload)), &p)
		return cmp.Or(p.Status, jobs.Achieved)
	}
	return eventStatuses[ev.Type]
}
func (r *Registry) Events(ctx context.Context, jobID string, after int64) ([]eventlog.Envelope, error) {
	if _, err := r.GetJob(ctx, jobID); after < 0 || err != nil {
		return nil, cmp.Or(err, fmt.Errorf("%w: after must not be negative", jobs.ErrInvalid))
	}
	return r.store.Read(jobID, after)
}
func (r *Registry) summarize(ctx context.Context, id string) (jobs.Detail, []jobs.PermissionInfo, error) {
	rec, err1 := r.GetJob(ctx, id)
	evs, err2 := r.store.Read(id, 0)
	if err := errors.Join(err1, err2); err != nil {
		return jobs.Detail{}, nil, err
	}
	return jobs.SummarizeEvents(rec.Job(), evs)
}
func (r *Registry) Inspect(ctx context.Context, id string) (jobs.Detail, error) {
	d, _, err := r.summarize(ctx, id)
	return d, err
}
func (r *Registry) Permissions(ctx context.Context, id, state string) ([]jobs.PermissionInfo, error) {
	if !slices.Contains([]string{"", "pending", "allowed", "denied", "expired"}, state) {
		return nil, fmt.Errorf("%w: state: unknown permission state", jobs.ErrInvalid)
	}
	_, all, err := r.summarize(ctx, id)
	if err != nil || state == "" {
		return all, err
	}
	return slices.DeleteFunc(all, func(p jobs.PermissionInfo) bool { return p.State != state }), nil
}
