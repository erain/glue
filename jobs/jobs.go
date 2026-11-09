// Package jobs persists coding objectives independently of their execution.
// Create only writes state and does not start execution.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/internal/eventlog"
	"github.com/erain/glue/internal/modelselection"
	"github.com/erain/glue/providers"
	_ "github.com/erain/glue/providers/codex"
	_ "github.com/erain/glue/providers/deepseek"
	_ "github.com/erain/glue/providers/gemini"
	_ "github.com/erain/glue/providers/nvidia"
	_ "github.com/erain/glue/providers/openrouter"
)

// RepoSpec selects one source and an optional starting revision.
type RepoSpec struct {
	Path    string `json:"path,omitempty"`
	URL     string `json:"url,omitempty"`
	BaseRef string `json:"base_ref,omitempty"`
}

// Budgets limits execution. Zero token and wall-clock budgets are unlimited.
type Budgets struct {
	MaxIterations int           `json:"max_iterations"`
	TokenBudget   int64         `json:"token_budget"`
	WallClock     time.Duration `json:"wall_clock"`
}

// Spec describes a coding objective and its execution settings.
type Spec struct {
	Objective string     `json:"objective"`
	Repo      RepoSpec   `json:"repo"`
	Provider  string     `json:"provider"`
	Model     string     `json:"model"`
	Thinking  string     `json:"thinking"`
	Budgets   Budgets    `json:"budgets"`
	Policy    string     `json:"policy"`
	Verify    [][]string `json:"verify"`
	Delivery  string     `json:"delivery"`
}

// Status is the durable job lifecycle state.
type Status string

const (
	Created         Status = "created"
	Running         Status = "running"
	WaitingForInput Status = "waiting_for_input"
	Achieved        Status = "achieved"
	Blocked         Status = "blocked"
	BudgetLimited   Status = "budget_limited"
	MaxIterations   Status = "max_iterations"
	Errored         Status = "errored"
	Canceled        Status = "canceled"
	Interrupted     Status = "interrupted"
)

// Job is a persisted specification and its execution metadata.
type Job struct {
	ID        string    `json:"id"`
	Spec      Spec      `json:"spec"`
	Status    Status    `json:"status"`
	BaseSHA   string    `json:"base_sha"`
	Workspace string    `json:"workspace"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	LastSeq   int64     `json:"last_seq"`
	GoalID    string    `json:"goal_id"`
}

// Options supplies storage and deferred execution dependencies.
type Options struct {
	StateDir        string
	ProviderFactory func(string) (glue.Provider, error)
	Now             func() time.Time
	Permission      glue.Permission
	// AfterFunc schedules a wall-clock deadline and returns its stop function.
	AfterFunc func(time.Duration, func()) func() bool
}

// Manager owns durable job state. Creating it does not start execution.
type Manager struct {
	mu, eventsMu sync.Mutex
	active       map[string]*execution
	wg           sync.WaitGroup
	closed       bool
	closeOnce    sync.Once
	closeErr     error
	lock         *os.File
	store        *eventlog.Store
	options      Options
}

const schema = `CREATE TABLE IF NOT EXISTS jobs(id TEXT PRIMARY KEY,spec_json TEXT NOT NULL,status TEXT NOT NULL,base_sha TEXT NOT NULL,workspace TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,last_seq INTEGER NOT NULL,goal_id TEXT NOT NULL);`
const columns = "id,spec_json,status,base_sha,workspace,created_at,updated_at,last_seq,goal_id"

// New opens durable state without contacting providers or repositories.
func New(opts Options) (*Manager, error) {
	if strings.TrimSpace(opts.StateDir) == "" {
		return nil, errors.New("jobs: StateDir is required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	lock, err := acquireLock(opts.StateDir)
	if err != nil {
		return nil, err
	}
	store, err := eventlog.Open(opts.StateDir, schema)
	if err != nil {
		releaseLock(lock)
		return nil, err
	}
	return &Manager{lock: lock, store: store, options: opts, active: map[string]*execution{}}, nil
}

func validate(s Spec) (Spec, error) {
	s.Objective = strings.TrimSpace(s.Objective)
	if s.Objective == "" {
		return s, invalid("objective", "required")
	}
	if (s.Repo.Path == "") == (s.Repo.URL == "") {
		return s, invalid("repo", "exactly one path or url is required")
	}
	if s.Budgets.MaxIterations < 0 {
		return s, invalid("budgets.max_iterations", "must not be negative")
	}
	if s.Budgets.TokenBudget < 0 {
		return s, invalid("budgets.token_budget", "must not be negative")
	}
	if s.Budgets.WallClock < 0 {
		return s, invalid("budgets.wall_clock", "must not be negative")
	}
	if s.Budgets.MaxIterations == 0 {
		s.Budgets.MaxIterations = 10
	}
	if s.Delivery == "" {
		s.Delivery = "none"
	}
	switch s.Delivery {
	case "none", "branch", "draft_pr":
	default:
		return s, invalid("delivery", "unknown mode")
	}
	for _, argv := range s.Verify {
		if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
			return s, invalid("verify", "commands require an executable")
		}
	}
	var err error
	s.Provider, s.Model, err = modelselection.Resolve(true, s.Provider, s.Model)
	if err != nil {
		return s, invalid("provider", err.Error())
	}
	if _, err := providers.ThinkingOptions(s.Provider, s.Model, s.Thinking); err != nil {
		return s, invalid("thinking", err.Error())
	}
	return s, nil
}

// Create atomically writes a validated specification and its first event.
func (m *Manager) Create(ctx context.Context, spec Spec) (Job, error) {
	var job Job
	spec, err := validate(spec)
	if err != nil {
		return job, err
	}
	var random [6]byte
	if _, err := rand.Read(random[:]); err != nil {
		return job, err
	}
	now := m.options.Now().UTC()
	job = Job{ID: "job-" + hex.EncodeToString(random[:]), Spec: spec, Status: Created, CreatedAt: now, UpdatedAt: now, LastSeq: 1}
	data, err := json.Marshal(spec)
	if err != nil {
		return Job{}, err
	}
	tx, err := m.store.DB.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	stamp := now.Format("2006-01-02T15:04:05.000000000Z")
	if _, err := tx.ExecContext(ctx, "INSERT INTO jobs("+columns+") VALUES(?,?,?,'','',?,?,1,'')", job.ID, string(data), Created, stamp, stamp); err != nil {
		return Job{}, err
	}
	if err := eventlog.Insert(tx, eventlog.Envelope{Version: 2, ID: job.ID + ":1", RunID: job.ID, Seq: 1, Time: now, Type: "job_created", Payload: spec}); err != nil {
		return Job{}, err
	}
	if err := tx.Commit(); err != nil {
		return Job{}, err
	}
	return job, nil
}
func scan(row interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var data, created, updated string
	if err := row.Scan(&j.ID, &data, &j.Status, &j.BaseSHA, &j.Workspace, &created, &updated, &j.LastSeq, &j.GoalID); err != nil {
		return j, err
	}
	if err := json.Unmarshal([]byte(data), &j.Spec); err != nil {
		return j, err
	}
	var err error
	j.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return j, err
	}
	j.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	return j, err
}

// Get reads a job, returning sql.ErrNoRows for an unknown ID.
func (m *Manager) Get(ctx context.Context, id string) (Job, error) {
	return scan(m.store.DB.QueryRowContext(ctx, "SELECT "+columns+" FROM jobs WHERE id=?", id))
}

// List returns jobs newest first, ordered by creation time and ID.
func (m *Manager) List(ctx context.Context) ([]Job, error) {
	return m.Query(ctx, Filter{})
}

// Events replays strictly later events for an existing job.
func (m *Manager) Events(ctx context.Context, id string, after int64) ([]eventlog.Envelope, error) {
	if after < 0 {
		return nil, fmt.Errorf("jobs: after must not be negative")
	}
	if _, err := m.Get(ctx, id); err != nil {
		return nil, err
	}
	return m.store.Read(id, after)
}
