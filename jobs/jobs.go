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
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/internal/eventlog"
	"github.com/erain/glue/internal/modelselection"
	"github.com/erain/glue/internal/redact"
	"github.com/erain/glue/providers"
	_ "github.com/erain/glue/providers/codex"
	_ "github.com/erain/glue/providers/deepseek"
	_ "github.com/erain/glue/providers/gemini"
	_ "github.com/erain/glue/providers/nvidia"
	_ "github.com/erain/glue/providers/openrouter"
	"github.com/erain/glue/tools/coding"
)

// RepoSpec selects one source and an optional starting revision.
type RepoSpec struct {
	Path    string `json:"path,omitempty"`
	URL     string `json:"url,omitempty"`
	BaseRef string `json:"base_ref,omitempty"`
	// SnapshotOf records "<branch>@<sha>" when BaseRef is a snapshot of a
	// developer's working tree. Draft PRs then target that branch if origin
	// has it, else the default branch.
	SnapshotOf string `json:"snapshot_of,omitempty"`
}

// Budgets limits execution. Zero token and wall-clock budgets are unlimited.
type Budgets struct {
	MaxIterations int           `json:"max_iterations"`
	TokenBudget   int64         `json:"token_budget"`
	WallClock     time.Duration `json:"wall_clock"`
}

// Spec describes a coding objective and its execution settings.
type Spec struct {
	ID        string     `json:"id,omitempty"`
	Objective string     `json:"objective"`
	Repo      RepoSpec   `json:"repo"`
	Provider  string     `json:"provider"`
	Model     string     `json:"model"`
	Thinking  string     `json:"thinking"`
	Budgets   Budgets    `json:"budgets"`
	Policy    string     `json:"policy"`
	Verify    [][]string `json:"verify"`
	Delivery  string     `json:"delivery"`
	Backend   string     `json:"backend,omitempty"`

	AllowedBinaries     []string      `json:"allowed_binaries,omitempty"`
	ShellEnvPass        []string      `json:"shell_env_pass,omitempty"`
	ShellEnv            []string      `json:"shell_env,omitempty"`
	ShellEnvSecretAllow []string      `json:"shell_env_secret_allow,omitempty"`
	ShellTimeout        time.Duration `json:"shell_timeout,omitempty"`
}

// Status is the durable job lifecycle state.
type Status string

const (
	Created         Status = "created"
	Running         Status = "running"
	WaitingForInput Status = "waiting_for_input"
	Suspended       Status = "suspended"
	Achieved        Status = "achieved"
	Blocked         Status = "blocked"
	BudgetLimited   Status = "budget_limited"
	MaxIterations   Status = "max_iterations"
	Errored         Status = "errored"
	Canceled        Status = "canceled"
	Interrupted     Status = "interrupted"

	WaitingForCredentials Status = "waiting_for_credentials"
)

// Credentials holds runtime secrets pushed over POST /v1/credentials in memory only.
type Credentials struct {
	GitHubToken  string            `json:"github_token,omitempty"`
	ProviderKeys map[string]string `json:"provider_keys,omitempty"`
	// NotifyURLs receive a JSON POST when a job waits for input or finishes.
	// They are secrets: kept in memory and redacted like tokens. A push
	// without them keeps the URLs already set.
	NotifyURLs []string `json:"notify_urls,omitempty"`
}

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
	// StatusReason records why a job is in its current state.
	StatusReason string `json:"status_reason,omitempty"`
	// TaskDeleted reports whether the backing sandbox task was deleted after retention.
	TaskDeleted bool `json:"task_deleted,omitempty"`
}

// Options supplies storage and deferred execution dependencies.
type Options struct {
	StateDir        string
	ProviderFactory func(string) (glue.Provider, error)
	Now             func() time.Time
	Permission      glue.Permission
	Policy          *Policy

	AllowedBinaries, ShellEnvPass, ShellEnv, ShellEnvSecretAllow []string
	ShellTimeout                                                 time.Duration
	// AfterFunc schedules a wall-clock deadline and returns its stop function.
	AfterFunc func(time.Duration, func()) func() bool
	// RequireCredentials requires runtime credentials to be pushed before running jobs.
	RequireCredentials bool
}

// Manager owns durable job state and recovers unfinished executions on startup.
type Manager struct {
	mu, eventsMu sync.Mutex
	active       map[string]*execution
	wg           sync.WaitGroup

	// notifyMu guards notifyClosed; notifyWG tracks notifications in flight
	// so Close can let them finish before closing the store.
	notifyMu     sync.Mutex
	notifyClosed bool
	notifyWG     sync.WaitGroup
	closed       bool
	closeOnce    sync.Once
	closeErr     error
	lock         *os.File
	store        *eventlog.Store
	options      Options

	credsMu     sync.RWMutex
	creds       Credentials
	hasCreds    bool
	credsNotify chan struct{}
}

const schema = `CREATE TABLE IF NOT EXISTS jobs(id TEXT PRIMARY KEY,spec_json TEXT NOT NULL,status TEXT NOT NULL,base_sha TEXT NOT NULL,workspace TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,last_seq INTEGER NOT NULL,goal_id TEXT NOT NULL);`
const columns = "id,spec_json,status,base_sha,workspace,created_at,updated_at,last_seq,goal_id"

// New opens durable state and resumes running/waiting jobs in the background.
// Created and terminal jobs never start automatically.
func New(opts Options) (*Manager, error) {
	if strings.TrimSpace(opts.StateDir) == "" {
		return nil, errors.New("jobs: StateDir is required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Policy != nil {
		if err := opts.Policy.Validate(); err != nil {
			return nil, err
		}
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
	m := &Manager{lock: lock, store: store, options: opts, active: map[string]*execution{}, credsNotify: make(chan struct{})}
	if err := m.recoverJobs(); err != nil {
		return nil, errors.Join(err, m.Close())
	}
	return m, nil
}

// SetCredentials stores runtime credentials in memory, registers them for
// redaction, and wakes any jobs waiting in waiting_for_credentials.
func (m *Manager) SetCredentials(creds Credentials) {
	tok, keys := strings.TrimSpace(creds.GitHubToken), make(map[string]string, len(creds.ProviderKeys))
	redact.Register(tok)
	for k, v := range creds.ProviderKeys {
		if k, v = strings.ToUpper(strings.TrimSpace(k)), strings.TrimSpace(v); k != "" && v != "" {
			keys[k] = v
			if f, ok := providers.Lookup(strings.ToLower(k)); ok && f.EnvKey != "" {
				keys[strings.ToUpper(f.EnvKey)] = v
			}
			redact.Register(v)
		}
	}
	var notify []string
	for _, u := range creds.NotifyURLs {
		if u = strings.TrimSpace(u); ValidNotifyURL(u) == nil {
			notify = append(notify, u)
			redact.Register(u)
		}
	}
	m.credsMu.Lock()
	if len(notify) == 0 {
		notify = m.creds.NotifyURLs
	}
	m.creds, m.hasCreds = Credentials{GitHubToken: tok, ProviderKeys: keys, NotifyURLs: notify}, tok != "" || len(keys) > 0
	close(m.credsNotify)
	m.credsNotify = make(chan struct{})
	m.credsMu.Unlock()
	if list, err := m.Query(context.Background(), Filter{Status: WaitingForCredentials}); err == nil {
		stamp := m.options.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
		for _, job := range list {
			if !m.missingCredentials(job.Spec, false) {
				_, _ = m.store.DB.Exec("UPDATE jobs SET status=?,updated_at=? WHERE id=? AND status=?", Running, stamp, job.ID, WaitingForCredentials)
			}
		}
	}
}

// HasCredentials reports whether runtime credentials have been pushed in memory.
func (m *Manager) HasCredentials() bool {
	m.credsMu.RLock()
	defer m.credsMu.RUnlock()
	return m.hasCreds
}
func (m *Manager) githubToken() string {
	m.credsMu.RLock()
	tok := m.creds.GitHubToken
	m.credsMu.RUnlock()
	if tok != "" {
		return tok
	}
	return strings.TrimSpace(os.Getenv("GLUE_GITHUB_TOKEN"))
}
func (m *Manager) providerKey(name string) string {
	name = strings.TrimSpace(name)
	m.credsMu.RLock()
	val := m.creds.ProviderKeys[strings.ToUpper(name)]
	m.credsMu.RUnlock()
	if val != "" || name == "" {
		return val
	}
	return strings.TrimSpace(os.Getenv(name))
}

func (m *Manager) policyFor(name string, allowedBinaries ...string) (Policy, error) {
	name = strings.TrimSpace(name)
	if m.options.Policy != nil {
		cfgName := strings.TrimSpace(m.options.Policy.Name)
		if cfgName == "" {
			cfgName = "custom"
		}
		if name == "" || name == cfgName {
			p := *m.options.Policy
			p.Name = cfgName
			return p, nil
		}
	}
	if len(allowedBinaries) == 0 {
		allowedBinaries = m.options.AllowedBinaries
	}
	return BuiltInPolicy(name, allowedBinaries...)
}

func (m *Manager) validate(s Spec) (Spec, error) {
	s.ID = strings.TrimSpace(s.ID)
	if s.ID != "" && (!strings.HasPrefix(s.ID, "job-") || len(s.ID) <= 4 || strings.ContainsAny(s.ID, " \t\r\n/\\")) {
		return s, invalid("id", "must start with job-")
	}
	s.Objective = strings.TrimSpace(s.Objective)
	if s.Objective == "" {
		return s, invalid("objective", "required")
	}
	if (s.Repo.Path == "") == (s.Repo.URL == "") {
		return s, invalid("repo", "exactly one path or url is required")
	}
	if s.Repo.Path != "" {
		absolute, err := filepath.Abs(s.Repo.Path)
		if err != nil {
			return s, invalid("repo.path", err.Error())
		}
		s.Repo.Path = absolute
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
	if s.ShellTimeout < 0 {
		return s, invalid("shell_timeout", "must not be negative")
	}
	if err := coding.ValidateShellEnv(s.ShellEnv); err != nil {
		return s, invalid("shell_env", err.Error())
	}
	if slices.ContainsFunc(s.AllowedBinaries, func(b string) bool { return strings.ContainsAny(strings.TrimSpace(b), `/\`) }) {
		return s, invalid("allowed_binaries", "must be binary basenames")
	}
	policy, err := m.policyFor(s.Policy, s.AllowedBinaries...)
	if err != nil {
		return s, invalid("policy", err.Error())
	}
	s.Policy = policy.Name
	s.Provider, s.Model, err = modelselection.Resolve(true, s.Provider, s.Model)
	if err != nil {
		return s, invalid("provider", err.Error())
	}
	if _, err := providers.ThinkingOptions(s.Provider, s.Model, s.Thinking); err != nil {
		return s, invalid("thinking", err.Error())
	}
	return s, nil
}

// NewID allocates a random job identifier ("job-" + 12 hex characters).
func NewID() (string, error) {
	var random [6]byte
	_, err := rand.Read(random[:])
	return "job-" + hex.EncodeToString(random[:]), err
}

// Create atomically writes a validated specification and its first event.
func (m *Manager) Create(ctx context.Context, spec Spec) (Job, error) {
	var job Job
	spec, err := m.validate(spec)
	if err != nil {
		return job, err
	}
	id := spec.ID
	if spec.ID = ""; id == "" {
		if id, err = NewID(); err != nil {
			return job, err
		}
	}
	now := m.options.Now().UTC()
	job = Job{ID: id, Spec: spec, Status: Created, CreatedAt: now, UpdatedAt: now, LastSeq: 1}
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
		if strings.Contains(err.Error(), "UNIQUE") {
			err = fmt.Errorf("%w: job %s already exists", ErrConflict, job.ID)
		}
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
