// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package gateway

import (
	"cmp"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
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
	registrySchema = `CREATE TABLE IF NOT EXISTS jobs (id TEXT PRIMARY KEY, backend TEXT NOT NULL, backend_ref TEXT NOT NULL, owner TEXT NOT NULL, spec_json TEXT NOT NULL, status TEXT NOT NULL, status_reason TEXT NOT NULL DEFAULT '', task_deleted INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, last_seq INTEGER NOT NULL, upstream_seq INTEGER NOT NULL DEFAULT 0, result_json TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sessions (id TEXT PRIMARY KEY, login TEXT NOT NULL, github_id INTEGER NOT NULL, csrf_token TEXT NOT NULL, created_at TEXT NOT NULL, last_seen_at TEXT NOT NULL, expires_at TEXT NOT NULL);`
	jobColumns = "id,backend,backend_ref,owner,spec_json,status,status_reason,task_deleted,created_at,updated_at,last_seq,upstream_seq,result_json"
)

// SubmitUnknown is recorded when an AX CreateTask call fails with an ambiguous error.
const SubmitUnknown jobs.Status = "submit_unknown"

var (
	validStatuses = []jobs.Status{"", jobs.Created, SubmitUnknown, jobs.Running, jobs.WaitingForInput, jobs.WaitingForCredentials, jobs.Suspended, jobs.Achieved, jobs.Blocked, jobs.BudgetLimited, jobs.MaxIterations, jobs.Errored, jobs.Canceled, jobs.Interrupted}
	eventStatuses = map[string]jobs.Status{
		"job_created": jobs.Created, "job_started": jobs.Running, "job_resumed": jobs.Running, "task_resumed": jobs.Running,
		"permission_decision": jobs.Running, "permission_expired": jobs.Running,
		"permission_request": jobs.WaitingForInput, "job_waiting": jobs.WaitingForInput,
		"job_waiting_for_credentials": jobs.WaitingForCredentials,
	}
)

// Record is one job row in the gateway SQLite registry.
type Record struct {
	ID, Backend, BackendRef, Owner string
	Spec                           jobs.Spec
	Status                         jobs.Status
	StatusReason                   string
	TaskDeleted                    bool
	CreatedAt, UpdatedAt           time.Time
	LastSeq, UpstreamSeq           int64
	Result                         *jobs.Result
}

func (r Record) Job() jobs.Job {
	spec := r.Spec
	spec.Backend = r.Backend
	return jobs.Job{ID: r.ID, Spec: spec, Status: r.Status, StatusReason: r.StatusReason, TaskDeleted: r.TaskDeleted, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, LastSeq: r.LastSeq}
}

type Registry struct {
	mu    sync.Mutex
	store *eventlog.Store
	now   func() time.Time
}

func OpenRegistry(stateDir string) (*Registry, error) {
	if stateDir = strings.TrimSpace(stateDir); stateDir == "" {
		return nil, errors.New("gateway: stateDir is required")
	}
	store, err := eventlog.Open(stateDir, registrySchema)
	if err == nil {
		store.DB.SetMaxOpenConns(1)
		_, _ = store.DB.Exec("ALTER TABLE jobs ADD COLUMN task_deleted INTEGER NOT NULL DEFAULT 0")
	}
	return &Registry{store: store, now: time.Now}, err
}
func (r *Registry) Close() error { return r.store.DB.Close() }
func (r *Registry) fmtTime(t time.Time) string {
	now := time.Now
	if r != nil && r.now != nil {
		now = r.now
	}
	return cmp.Or(t, now()).UTC().Format(time.RFC3339Nano)
}
func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
func (r *Registry) CreateJob(ctx context.Context, rec Record) (Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec.Status = cmp.Or(rec.Status, jobs.Created)
	rec.CreatedAt = cmp.Or(rec.CreatedAt, r.now()).UTC()
	rec.UpdatedAt = cmp.Or(rec.UpdatedAt, rec.CreatedAt).UTC()
	_, err := r.store.DB.ExecContext(ctx, "INSERT INTO jobs("+jobColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,'')",
		rec.ID, rec.Backend, rec.BackendRef, rec.Owner, mustJSON(rec.Spec), rec.Status, rec.StatusReason, rec.TaskDeleted,
		r.fmtTime(rec.CreatedAt), r.fmtTime(rec.UpdatedAt), rec.LastSeq, rec.UpstreamSeq)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return Record{}, fmt.Errorf("%w: job %s already exists", jobs.ErrConflict, rec.ID)
	}
	return rec, err
}
func (r *Registry) UpsertStatus(ctx context.Context, id, backend string, spec jobs.Spec, st jobs.Status) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.fmtTime(time.Time{})
	_, err := r.store.DB.ExecContext(ctx, "INSERT INTO jobs("+jobColumns+") VALUES(?,?,?,'dev-token',?,?,'',0,?,?,0,0,'') ON CONFLICT(id) DO UPDATE SET status=excluded.status,updated_at=excluded.updated_at", id, backend, id, mustJSON(spec), st, now, now)
	return err
}
func scanRecord(row interface{ Scan(...any) error }) (Record, error) {
	var rec Record
	var specJSON, created, updated, resultJSON string
	err := row.Scan(&rec.ID, &rec.Backend, &rec.BackendRef, &rec.Owner, &specJSON, &rec.Status, &rec.StatusReason, &rec.TaskDeleted, &created, &updated, &rec.LastSeq, &rec.UpstreamSeq, &resultJSON)
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
		mustJSON(result), result.Status, result.Status, r.fmtTime(time.Time{}), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
func (r *Registry) MarkTaskDeleted(ctx context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.store.DB.ExecContext(ctx, "UPDATE jobs SET task_deleted=1 WHERE id=?", id)
	return err
}
func (r *Registry) PurgeJob(ctx context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err1 := r.store.DB.ExecContext(ctx, "DELETE FROM events WHERE run_id=?", id)
	res, err2 := r.store.DB.ExecContext(ctx, "DELETE FROM jobs WHERE id=?", id)
	if err := errors.Join(err1, err2); err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
func (r *Registry) ActiveTaskJobs(ctx context.Context) ([]Record, error) {
	rows, err := r.store.DB.QueryContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE task_deleted=0 ORDER BY updated_at ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		if rec, err := scanRecord(rows); err == nil {
			out = append(out, rec)
		}
	}
	return out, rows.Err()
}
func (r *Registry) AppendEvent(ctx context.Context, jobID string, ev eventlog.Envelope) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var lastSeq, upSeq int64
	if err := r.store.DB.QueryRowContext(ctx, "SELECT last_seq,upstream_seq FROM jobs WHERE id=?", jobID).Scan(&lastSeq, &upSeq); err != nil || (ev.Seq > 0 && ev.Seq <= upSeq) {
		return false, err
	}
	upSeq, ev.Seq = max(upSeq, ev.Seq), max(lastSeq+1, ev.Seq)
	ev.Version, ev.RunID, ev.ID = 2, jobID, fmt.Sprintf("%s:%d", jobID, ev.Seq)
	ins, err := r.store.DB.ExecContext(ctx, "INSERT OR IGNORE INTO events(run_id,seq,payload_json) VALUES(?,?,?)", jobID, ev.Seq, mustJSON(ev))
	if err != nil {
		return false, err
	}
	if affected, _ := ins.RowsAffected(); affected == 0 {
		return false, nil
	}
	st, reason := statusFromEvent(ev), ""
	if m, ok := ev.Payload.(map[string]any); ok && ev.Type == "task_suspended" {
		if reason, _ = m["reason"].(string); reason == "waiting_for_input" {
			st = jobs.Suspended
		}
	}
	_, err = r.store.DB.ExecContext(ctx,
		"UPDATE jobs SET status=CASE WHEN ?='' THEN status ELSE ? END,status_reason=CASE WHEN ? != '' THEN ? WHEN ? != '' THEN '' ELSE status_reason END,last_seq=?,upstream_seq=?,updated_at=? WHERE id=?",
		st, st, reason, reason, st, ev.Seq, upSeq, r.fmtTime(ev.Time), jobID)
	return err == nil, err
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

// Session is a server-side browser session row in the gateway SQLite registry.
type Session struct {
	ID, Login, CSRFToken             string
	GitHubID                         int64
	CreatedAt, LastSeenAt, ExpiresAt time.Time
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
func (r *Registry) CreateSession(ctx context.Context, login string, githubID int64, absTTL time.Duration) (Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now().UTC()
	s := Session{ID: randHex(32), Login: login, GitHubID: githubID, CSRFToken: randHex(32), CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(cmp.Or(absTTL, DefaultSessionAbsoluteTTL))}
	_, err := r.store.DB.ExecContext(ctx, "INSERT INTO sessions(id,login,github_id,csrf_token,created_at,last_seen_at,expires_at) VALUES(?,?,?,?,?,?,?)",
		s.ID, s.Login, s.GitHubID, s.CSRFToken, r.fmtTime(s.CreatedAt), r.fmtTime(s.LastSeenAt), r.fmtTime(s.ExpiresAt))
	return s, err
}
func (r *Registry) AuthenticateSession(ctx context.Context, id string, idleTTL time.Duration) (Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var s Session
	var created, lastSeen, expires string
	if err := r.store.DB.QueryRowContext(ctx, "SELECT id,login,github_id,csrf_token,created_at,last_seen_at,expires_at FROM sessions WHERE id=?", id).Scan(&s.ID, &s.Login, &s.GitHubID, &s.CSRFToken, &created, &lastSeen, &expires); err != nil {
		return Session{}, err
	}
	s.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	s.LastSeenAt, _ = time.Parse(time.RFC3339Nano, lastSeen)
	s.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expires)
	now := r.now().UTC()
	if !now.Before(s.ExpiresAt) || !now.Before(s.LastSeenAt.Add(cmp.Or(idleTTL, DefaultSessionIdleTTL))) {
		_, _ = r.store.DB.ExecContext(ctx, "DELETE FROM sessions WHERE id=?", id)
		return Session{}, sql.ErrNoRows
	}
	s.LastSeenAt = now
	_, err := r.store.DB.ExecContext(ctx, "UPDATE sessions SET last_seen_at=? WHERE id=?", r.fmtTime(now), id)
	return s, err
}
func (r *Registry) DeleteSession(ctx context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.store.DB.ExecContext(ctx, "DELETE FROM sessions WHERE id=?", id)
	return err
}
