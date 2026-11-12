// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package axbackend

import (
	"bytes"
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/erain/glue/daemon"
	"github.com/erain/glue/internal/eventlog"
	"github.com/erain/glue/internal/jobclient"
	"github.com/erain/glue/jobs"
	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

// SubmitUnknown is recorded when CreateTask fails with an ambiguous transport or deadline error.
const SubmitUnknown jobs.Status = "submit_unknown"

// DefaultEgressHosts is the default TLS passthrough allowlist for AX coding tasks.
var DefaultEgressHosts = []string{"github.com", "api.github.com", "codeload.github.com", "proxy.golang.org", "sum.golang.org", "storage.googleapis.com", "generativelanguage.googleapis.com", "openrouter.ai"}

func WorkspaceName(jobID string) string { return "glue-" + jobID + "-ws" }
func TaskName(jobID string) string      { return "glue-" + jobID }

// DefaultImage resolves the AX task image from GLUE_AX_IMAGE, GLUE_TASK_IMAGE, or ax-dev images.env.
func DefaultImage() string {
	if v := envOr("GLUE_AX_IMAGE", "GLUE_TASK_IMAGE"); v != "" {
		return v
	}
	base := envOr("XDG_CACHE_HOME")
	if home, _ := os.UserHomeDir(); base == "" && home != "" {
		base = filepath.Join(home, ".cache")
	}
	if base != "" {
		data, _ := os.ReadFile(filepath.Join(base, "glue", "ax-dev", "images.env"))
		for line := range strings.SplitSeq(string(data), "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export ")), "="); ok && strings.TrimSpace(k) == "GLUE_TASK_IMAGE" {
				return strings.Trim(strings.TrimSpace(v), `"'`)
			}
		}
	}
	return ""
}

// Options configures a direct AX Backend instance.
type Options struct {
	Connect                              ConnectOptions
	Image, CPU, Memory, Secret, CacheDir string
	EgressHosts                          []string
	Credentials                          jobs.Credentials
	PollInterval                         time.Duration
	GetJob                               func(ctx context.Context, id string) (jobs.Spec, jobs.Status, error)
	SetStatus                            func(ctx context.Context, id string, spec jobs.Spec, status jobs.Status) error
}

type cachedJob struct {
	ID        string       `json:"id"`
	Spec      jobs.Spec    `json:"spec"`
	Status    jobs.Status  `json:"status"`
	Detail    *jobs.Detail `json:"detail,omitempty"`
	Result    *jobs.Result `json:"result,omitempty"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
}

// Backend executes Glue jobs directly on AX tasks with stateless task discovery and local result caching.
type Backend struct {
	opts    Options
	conn    *Connection
	secret  string
	cacheMu sync.Mutex
}

// NewBackend connects to AX and initializes task authentication and local job caching.
func NewBackend(ctx context.Context, opts Options) (*Backend, error) {
	opts.Connect.Atespace = cmp.Or(strings.TrimSpace(opts.Connect.Atespace), "default")
	opts.CPU, opts.Memory = cmp.Or(strings.TrimSpace(opts.CPU), "4"), cmp.Or(strings.TrimSpace(opts.Memory), "8Gi")
	opts.Image, opts.PollInterval = cmp.Or(strings.TrimSpace(opts.Image), DefaultImage()), cmp.Or(opts.PollInterval, 250*time.Millisecond)
	opts.CacheDir = cmp.Or(strings.TrimSpace(opts.CacheDir), filepath.Join(jobclient.ConfigDir(), "ax-jobs"))
	if len(opts.EgressHosts) == 0 {
		opts.EgressHosts = slices.Clone(DefaultEgressHosts)
	}
	conn, err := Connect(ctx, opts.Connect)
	if err != nil {
		return nil, err
	}
	sec, err := resolveTaskSecret(ctx, opts, conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &Backend{opts: opts, conn: conn, secret: sec}, nil
}

func (b *Backend) Close() error { return b.conn.Close() }

func resolveTaskSecret(ctx context.Context, opts Options, conn *Connection) (string, error) {
	if s := cmp.Or(strings.TrimSpace(opts.Secret), envOr("GLUE_AX_SECRET")); s != "" {
		return s, nil
	}
	var rnd [32]byte
	_, _ = rand.Read(rnd[:])
	gen := hex.EncodeToString(rnd[:])
	if opts.Connect.AXClient != nil || (conn.KubeConfig == "" && conn.KubeContext == "") {
		return gen, nil
	}
	axNS := cmp.Or(strings.TrimSpace(opts.Connect.AXNamespace), envOr("AX_SYSTEM_NAMESPACE"), DefaultAXNamespace)
	tunnelMu.Lock()
	defer tunnelMu.Unlock()
	path := filepath.Join(TunnelDir(), cacheKey(conn.KubeConfig, conn.KubeContext, axNS)+"-hmac.json")
	var entry struct{ Secret string }
	if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &entry) == nil && entry.Secret != "" {
		return entry.Secret, nil
	}
	readCluster := func() string {
		out, err := runKubectl(ctx, conn.KubeConfig, conn.KubeContext, nil, "-n", axNS, "get", "secret", "glue-ax-task-hmac", "-o", "jsonpath={.data.secret}")
		if raw, decErr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out))); err == nil && decErr == nil {
			return strings.TrimSpace(string(raw))
		}
		return ""
	}
	if entry.Secret = readCluster(); entry.Secret == "" {
		manifest, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "glue-ax-task-hmac", "namespace": axNS}, "stringData": map[string]string{"secret": gen}})
		if _, err := runKubectl(ctx, conn.KubeConfig, conn.KubeContext, manifest, "create", "-f", "-"); err == nil {
			entry.Secret = gen
		} else if entry.Secret = readCluster(); entry.Secret == "" {
			return "", fmt.Errorf("axbackend: ensure glue-ax-task-hmac secret in %s: %w", axNS, err)
		}
	}
	return entry.Secret, writePrivateJSON(path, entry)
}

func isTerminal(st jobs.Status) bool {
	return st != "" && st != jobs.Created && st != SubmitUnknown && st != jobs.Running && st != jobs.WaitingForInput && st != jobs.WaitingForCredentials && st != jobs.Suspended
}
func isWorkspaceReady(t *v1alpha1.Task) bool {
	return t.GetStatus().GetPhase() == "Running" && slices.ContainsFunc(t.GetStatus().GetConditions(), func(c *v1alpha1.Condition) bool {
		return c.GetType() == "WorkspaceReady" && c.GetStatus() == "True"
	})
}
func (b *Backend) getTask(ctx context.Context, id string) (*v1alpha1.Task, error) {
	return b.conn.AX.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: b.conn.Atespace, Name: TaskName(id)})
}
func (b *Backend) taskToken(jobID string) string {
	mac := hmac.New(sha256.New, []byte(b.secret))
	_, _ = mac.Write([]byte("glue-ax-task-token-v1:" + jobID))
	return hex.EncodeToString(mac.Sum(nil))
}
func (b *Backend) jobClient(jobID string) *jobclient.Client {
	return &jobclient.Client{BaseURL: fmt.Sprintf("http://%s:%d", TaskName(jobID), DefaultTaskPort), Token: b.taskToken(jobID), HTTP: b.conn.RouterHTTP}
}
func (b *Backend) retry(ctx context.Context, fn func() error) error {
	for attempt := 0; ; attempt++ {
		err := fn()
		if se := (*jobclient.StatusError)(nil); err == nil || attempt >= 40 || (errors.As(err, &se) && se.Code >= 400 && se.Code < 500) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(b.opts.PollInterval):
		}
	}
}
func (b *Backend) cachePath(id string) string { return filepath.Join(b.opts.CacheDir, id+".json") }
func (b *Backend) loadCache(id string) *cachedJob {
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	var c cachedJob
	if data, err := os.ReadFile(b.cachePath(id)); err == nil && json.Unmarshal(data, &c) == nil && c.ID != "" {
		return &c
	}
	return nil
}
func (b *Backend) updateCache(id string, fn func(*cachedJob)) {
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	var c cachedJob
	if data, err := os.ReadFile(b.cachePath(id)); err == nil {
		_ = json.Unmarshal(data, &c)
	}
	c.ID, c.UpdatedAt = id, time.Now().UTC()
	fn(&c)
	_ = errors.Join(os.MkdirAll(b.opts.CacheDir, 0o700), os.Chmod(b.opts.CacheDir, 0o700), writePrivateJSON(b.cachePath(id), c))
}
func (b *Backend) cachedState(ctx context.Context, id string) (spec jobs.Spec, st jobs.Status) {
	if b.opts.GetJob != nil {
		spec, st, _ = b.opts.GetJob(ctx, id)
	}
	if c := b.loadCache(id); c != nil {
		st = cmp.Or(st, c.Status)
		if spec.Objective == "" {
			spec = c.Spec
		}
	}
	return spec, st
}
func (b *Backend) recordState(ctx context.Context, id string, spec jobs.Spec, st jobs.Status, createdAt time.Time) {
	if b.opts.SetStatus != nil {
		_ = b.opts.SetStatus(ctx, id, spec, st)
	}
	b.updateCache(id, func(c *cachedJob) {
		c.Spec, c.Status = spec, st
		if c.CreatedAt.IsZero() && !createdAt.IsZero() {
			c.CreatedAt = createdAt
		}
	})
}
func (b *Backend) credentials() jobs.Credentials {
	c, pks := b.opts.Credentials, map[string]string{}
	for k, v := range c.ProviderKeys {
		if k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v); k != "" && v != "" {
			pks[k] = v
		}
	}
	for p, env := range map[string]string{"gemini": "GEMINI_API_KEY", "openrouter": "OPENROUTER_API_KEY", "deepseek": "DEEPSEEK_API_KEY", "nvidia": "NVIDIA_API_KEY", "codex": "OPENAI_API_KEY"} {
		if v := envOr(env); pks[p] == "" && v != "" {
			pks[p] = v
		}
	}
	c.GitHubToken, c.ProviderKeys = cmp.Or(strings.TrimSpace(c.GitHubToken), envOr("GLUE_GITHUB_TOKEN")), nil
	if len(pks) > 0 {
		c.ProviderKeys = pks
	}
	return c
}
func (b *Backend) PushCredentials(ctx context.Context, id string) error {
	cli, creds := b.jobClient(id), b.credentials()
	if err := b.retry(ctx, func() error { return cli.PushCredentials(ctx, creds) }); err != nil {
		return fmt.Errorf("axbackend: push credentials for %s: %w", id, err)
	}
	return nil
}
func (b *Backend) fetchDetail(ctx context.Context, id string) (d jobs.Detail, err error) {
	if err = b.retry(ctx, func() error { d, err = b.jobClient(id).Show(ctx, id); return err }); err == nil && (d.Credentials == "absent" || d.Status == jobs.WaitingForCredentials) {
		d.Credentials, err = "present", b.PushCredentials(ctx, id)
	}
	return d, err
}
func (b *Backend) EnsureCredentials(ctx context.Context, id string) error {
	_, err := b.fetchDetail(ctx, id)
	return err
}

// Create provisions the AX Workspace and Task (reconciling via GetTask first on SubmitUnknown).
func (b *Backend) Create(ctx context.Context, id string, spec jobs.Spec) (jobs.Job, error) {
	if id = strings.TrimSpace(cmp.Or(id, spec.ID)); id == "" {
		id, _ = jobs.NewID()
	}
	img := cmp.Or(strings.TrimSpace(b.opts.Image), DefaultImage())
	spec.ID, spec.Backend, spec.Objective = "", "", strings.TrimSpace(spec.Objective)
	spec.Repo.URL, spec.Repo.Path, spec.Repo.BaseRef = strings.TrimSpace(spec.Repo.URL), strings.TrimSpace(spec.Repo.Path), cmp.Or(strings.TrimSpace(spec.Repo.BaseRef), "main")
	if !strings.HasPrefix(id, "job-") || len(id) <= 4 || strings.ContainsAny(id, " \t\r\n/\\") || img == "" || spec.Objective == "" || spec.Repo.URL == "" || spec.Repo.Path != "" {
		return jobs.Job{}, fmt.Errorf("%w: ax backend requires job-* id, image, objective, and repo.url (without repo.path)", jobs.ErrInvalid)
	}
	now := time.Now().UTC()
	job := jobs.Job{ID: id, Spec: spec, Status: jobs.Created, CreatedAt: now, UpdatedAt: now}
	if _, prevSt := b.cachedState(ctx, id); prevSt == SubmitUnknown {
		if _, err := b.getTask(ctx, id); err == nil {
			b.recordState(ctx, id, spec, jobs.Created, now)
			return job, nil
		} else if status.Code(err) != codes.NotFound {
			return job, fmt.Errorf("axbackend: reconcile task %s: %w", TaskName(id), err)
		}
	}
	if _, err := b.conn.AX.UpdateWorkspace(ctx, &v1alpha1.UpdateWorkspaceRequest{Workspace: &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: WorkspaceName(id), Atespace: b.conn.Atespace}, Spec: &v1alpha1.WorkspaceSpec{Git: []*v1alpha1.GitRepo{{Name: "repo", Repo: spec.Repo.URL, Branch: spec.Repo.BaseRef, Dir: ".", Depth: 1}}, Files: []*v1alpha1.File{{Path: ".glue/token", Content: daemon.HashToken(b.taskToken(id)) + "\n"}}}}}); err != nil {
		return job, fmt.Errorf("axbackend: update workspace %s: %w", WorkspaceName(id), err)
	}
	taskCfg := spec
	taskCfg.ID, taskCfg.Repo = id, jobs.RepoSpec{Path: "/workspace", BaseRef: spec.Repo.BaseRef, SnapshotOf: spec.Repo.SnapshotOf}
	rawCfg, _ := json.Marshal(taskCfg)
	var cfgMap map[string]any
	_ = json.Unmarshal(rawCfg, &cfgMap)
	if taskCfg.Budgets == (jobs.Budgets{}) {
		delete(cfgMap, "budgets")
	} else if bm, ok := cfgMap["budgets"].(map[string]any); ok && taskCfg.Budgets.TokenBudget == 0 {
		delete(bm, "token_budget")
	}
	cfgMap["repo_url"], cfgMap["created_at"] = spec.Repo.URL, now.Format(time.RFC3339Nano)
	cfgStruct, _ := structpb.NewStruct(cfgMap)
	cmd := []string{"glue", "serve", "--coding", "--jobs", "--job-from-ax", "--listen", ":8080", "--state-dir", "/workspace/.glue/state", "--token-file", "/workspace/.glue/token"}
	if _, err := b.conn.AX.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Name: TaskName(id), Atespace: b.conn.Atespace}, Spec: &v1alpha1.TaskSpec{Image: img, Command: cmd, Workspace: &v1alpha1.WorkspaceRef{Name: WorkspaceName(id), Path: "/workspace"}, Resources: &v1alpha1.ResourcesSpec{Limits: &v1alpha1.ResourceList{Cpu: b.opts.CPU, Memory: b.opts.Memory}}, Config: cfgStruct}}}); err != nil && status.Code(err) != codes.AlreadyExists {
		if c := status.Code(err); errors.Is(err, context.DeadlineExceeded) || c == codes.DeadlineExceeded || c == codes.Unavailable || c == codes.Unknown {
			job.Status = SubmitUnknown
			b.recordState(ctx, id, spec, SubmitUnknown, now)
		}
		return job, fmt.Errorf("axbackend: create task %s: %w", TaskName(id), err)
	}
	b.recordState(ctx, id, spec, jobs.Created, now)
	return job, nil
}

func (b *Backend) Suspend(ctx context.Context, id string) error {
	_, err := b.conn.AX.SuspendTask(ctx, &v1alpha1.SuspendTaskRequest{Atespace: b.conn.Atespace, Name: TaskName(id)})
	return err
}

// Resume ensures the task egress policy exists, resumes the AX task, waits for WorkspaceReady, and pushes credentials.
func (b *Backend) Resume(ctx context.Context, id string) error {
	if _, err := b.conn.Control.CreateActorEgressPolicy(ctx, &ateapipb.CreateActorEgressPolicyRequest{
		Actor:        &ateapipb.ObjectRef{Atespace: b.conn.Atespace, Name: TaskName(id)},
		EgressPolicy: &ateapipb.EgressPolicy{Metadata: &ateapipb.ResourceMetadata{Atespace: b.conn.Atespace, Name: "default"}, Rules: []*ateapipb.EgressRule{{TlsPassthrough: &ateapipb.TlsPassthroughRule{Hostnames: slices.Clone(b.opts.EgressHosts), Ports: &ateapipb.Ports{Numbers: []int32{443}}}}}},
	}); err != nil && status.Code(err) != codes.AlreadyExists {
		return fmt.Errorf("axbackend: create egress policy for %s: %w", TaskName(id), err)
	}
	if _, err := b.conn.AX.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Atespace: b.conn.Atespace, Name: TaskName(id)}); err != nil {
		return fmt.Errorf("axbackend: resume task %s: %w", TaskName(id), err)
	}
	for {
		t, err := b.getTask(ctx, id)
		if err != nil || t.GetStatus().GetPhase() == "Failed" {
			return cmp.Or(err, fmt.Errorf("axbackend: task %s entered Failed phase", TaskName(id)))
		}
		if isWorkspaceReady(t) {
			return b.PushCredentials(ctx, id)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(b.opts.PollInterval):
		}
	}
}

func (b *Backend) EnsureRunning(ctx context.Context, id string) error {
	t, err := b.getTask(ctx, id)
	if err != nil || t.GetStatus().GetPhase() == "Failed" {
		return cmp.Or(err, fmt.Errorf("axbackend: task %s is in Failed phase", TaskName(id)))
	}
	if isWorkspaceReady(t) {
		return b.EnsureCredentials(ctx, id)
	}
	return b.Resume(ctx, id)
}

func (b *Backend) Start(ctx context.Context, id string) (jobs.Job, error) {
	spec, st := b.cachedState(ctx, id)
	if st == SubmitUnknown {
		if _, err := b.Create(ctx, id, spec); err != nil {
			return jobs.Job{}, err
		}
	}
	if err := b.Resume(ctx, id); err != nil {
		return jobs.Job{}, err
	}
	var started jobs.Job
	if err := b.retry(ctx, func() (err error) {
		started, err = b.jobClient(id).Start(ctx, id)
		if se := (*jobclient.StatusError)(nil); errors.As(err, &se) && se.Code == http.StatusConflict {
			return nil
		}
		return err
	}); err != nil {
		return jobs.Job{}, fmt.Errorf("axbackend: start job %s: %w", id, err)
	}
	if spec.Objective != "" {
		started.Spec = spec
	}
	started.ID, started.Status = cmp.Or(started.ID, id), cmp.Or(started.Status, jobs.Running)
	b.recordState(ctx, id, started.Spec, started.Status, time.Time{})
	return started, nil
}

func (b *Backend) Submit(ctx context.Context, id string, spec jobs.Spec) (jobs.Job, error) {
	created, err := b.Create(ctx, id, spec)
	if err != nil {
		return jobs.Job{}, err
	}
	return b.Start(ctx, created.ID)
}

func resolvePhaseStatus(phase string, st jobs.Status, suspendedWins bool) jobs.Status {
	switch {
	case isTerminal(st):
		return st
	case phase == "Failed":
		return jobs.Errored
	case phase == "Suspended" && (suspendedWins || st == ""):
		return jobs.Suspended
	case phase == "Running" && (st == "" || st == SubmitUnknown || st == jobs.Created || st == jobs.Suspended):
		return jobs.Running
	default:
		return cmp.Or(st, jobs.Created)
	}
}

func (b *Backend) taskToJob(t *v1alpha1.Task) (jobs.Job, bool) {
	name := strings.TrimSpace(t.GetMetadata().GetName())
	if !strings.HasPrefix(name, "glue-") || t.GetSpec().GetConfig() == nil {
		return jobs.Job{}, false
	}
	id, cfgMap := strings.TrimPrefix(name, "glue-"), t.GetSpec().GetConfig().AsMap()
	raw, err := json.Marshal(cfgMap)
	var spec jobs.Spec
	if err != nil || json.Unmarshal(raw, &spec) != nil || strings.TrimSpace(spec.Objective) == "" {
		return jobs.Job{}, false
	}
	spec.ID = ""
	if u, _ := cfgMap["repo_url"].(string); strings.TrimSpace(u) != "" {
		spec.Repo = jobs.RepoSpec{URL: strings.TrimSpace(u), BaseRef: spec.Repo.BaseRef, SnapshotOf: spec.Repo.SnapshotOf}
	}
	createdAt, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(cfgMap["created_at"]))
	if createdAt.IsZero() && t.GetMetadata().GetCreateTime().IsValid() {
		createdAt = t.GetMetadata().GetCreateTime().AsTime().UTC()
	}
	var cSt jobs.Status
	c := b.loadCache(id)
	if c != nil {
		cSt = c.Status
	}
	j := jobs.Job{ID: id, Spec: spec, Status: resolvePhaseStatus(t.GetStatus().GetPhase(), cSt, true), CreatedAt: createdAt, UpdatedAt: createdAt}
	if c != nil && !c.UpdatedAt.IsZero() {
		j.UpdatedAt = c.UpdatedAt
	}
	if c != nil && c.Detail != nil {
		j.BaseSHA, j.LastSeq, j.GoalID = c.Detail.BaseSHA, c.Detail.LastSeq, c.Detail.GoalID
	}
	return j, true
}

// ListJobs reconstructs Glue jobs directly from AX tasks and local terminal cache without waking suspended tasks.
func (b *Backend) ListJobs(ctx context.Context, f jobs.Filter) ([]jobs.Job, error) {
	resp, err := b.conn.AX.ListTasks(ctx, &v1alpha1.ListTasksRequest{Atespace: b.conn.Atespace})
	if err != nil {
		return nil, fmt.Errorf("axbackend: list tasks in %s: %w", b.conn.Atespace, err)
	}
	var out []jobs.Job
	for _, t := range resp.GetTasks() {
		if j, ok := b.taskToJob(t); ok {
			out = append(out, j)
		}
	}
	slices.SortFunc(out, func(a, b jobs.Job) int { return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), strings.Compare(b.ID, a.ID)) })
	if idx := slices.IndexFunc(out, func(j jobs.Job) bool { return f.Before != "" && j.ID == f.Before }); idx >= 0 {
		out = out[idx+1:]
	}
	if f.Status != "" {
		out = slices.DeleteFunc(out, func(j jobs.Job) bool { return j.Status != f.Status })
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// Show inspects a Glue job, querying the live task when running and reconstructing without waking when suspended.
func (b *Backend) Show(ctx context.Context, id string) (jobs.Detail, error) {
	t, err := b.getTask(ctx, id)
	if err != nil {
		return jobs.Detail{}, fmt.Errorf("axbackend: get task %s: %w", TaskName(id), err)
	}
	job, _ := b.taskToJob(t)
	if isWorkspaceReady(t) {
		d, err := b.fetchDetail(ctx, id)
		if err != nil {
			return jobs.Detail{}, err
		}
		if job.Spec.Repo.URL != "" {
			d.Spec.Repo = job.Spec.Repo
		}
		b.updateCache(id, func(c *cachedJob) {
			c.Spec, c.Status, c.Detail = d.Spec, d.Status, &d
			if !isTerminal(d.Status) {
				c.Result = nil
			}
		})
		return d, nil
	}
	if c := b.loadCache(id); c != nil && c.Detail != nil {
		d := *c.Detail
		d.Job.Status = job.Status
		if job.Spec.Objective != "" {
			d.Spec = job.Spec
		}
		return d, nil
	}
	return jobs.Detail{Job: job}, nil
}

func (b *Backend) Status(ctx context.Context, id string) (jobs.Status, error) {
	_, st := b.cachedState(ctx, id)
	t, err := b.getTask(ctx, id)
	if err != nil {
		return cmp.Or(st, jobs.Created), err
	}
	return resolvePhaseStatus(t.GetStatus().GetPhase(), st, false), nil
}

func (b *Backend) withRunning(ctx context.Context, id string, fn func(*jobclient.Client) error) error {
	if err := b.EnsureRunning(ctx, id); err != nil {
		return err
	}
	return fn(b.jobClient(id))
}
func (b *Backend) withTempWake(ctx context.Context, id string, fn func(*jobclient.Client) error) error {
	t, err := b.getTask(ctx, id)
	if err != nil {
		return err
	}
	wasSuspended := t.GetStatus().GetPhase() == "Suspended"
	if err = b.withRunning(ctx, id, fn); wasSuspended {
		_ = b.Suspend(ctx, id)
	}
	return err
}

// Result returns a cached terminal completion record or fetches it from the AX task (resuming and re-suspending if needed).
func (b *Backend) Result(ctx context.Context, id string) (res jobs.Result, err error) {
	if c := b.loadCache(id); c != nil && c.Result != nil && isTerminal(c.Result.Status) {
		return *c.Result, nil
	}
	err = b.withTempWake(ctx, id, func(c *jobclient.Client) (e error) { res, e = c.Result(ctx, id); return e })
	if err == nil && isTerminal(res.Status) {
		b.updateCache(id, func(c *cachedJob) { c.Status, c.Result = res.Status, &res })
	}
	return res, err
}

// patchPath names the cached patch for one finished run of a job. It is
// keyed by the result's head commit, so a continued job never reads the
// patch of an earlier run.
func (b *Backend) patchPath(id, headSHA string) string {
	return filepath.Join(b.opts.CacheDir, id+"-"+headSHA+".patch")
}

// removePatches deletes every cached patch of a job. The caller holds cacheMu.
func (b *Backend) removePatches(id string) {
	old, _ := filepath.Glob(filepath.Join(b.opts.CacheDir, id+"-*.patch"))
	for _, p := range old {
		_ = os.Remove(p)
	}
}

// Patch returns the job's patch, from the local cache when this run's patch
// was fetched before, so repeated calls do not wake the task.
func (b *Backend) Patch(ctx context.Context, id string) ([]byte, error) {
	res, err := b.Result(ctx, id)
	if err != nil {
		return nil, err
	}
	head := strings.TrimSpace(res.HeadSHA)
	if head != "" {
		b.cacheMu.Lock()
		data, err := os.ReadFile(b.patchPath(id, head))
		b.cacheMu.Unlock()
		if err == nil {
			return data, nil
		}
	}
	var data []byte
	err = b.withTempWake(ctx, id, func(c *jobclient.Client) (e error) { data, e = c.Patch(ctx, id); return e })
	if err != nil || head == "" {
		return data, err
	}
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	b.removePatches(id)
	if mkErr := os.MkdirAll(b.opts.CacheDir, 0o700); mkErr == nil {
		_ = os.WriteFile(b.patchPath(id, head), data, 0o600)
	}
	return data, nil
}

func (b *Backend) Stream(ctx context.Context, id string, after int64, h func(eventlog.Envelope) error) error {
	return b.withRunning(ctx, id, func(cli *jobclient.Client) error {
		return cli.StreamEvents(ctx, id, after, func(ev eventlog.Envelope) error {
			if ev.Type == "job_waiting_for_credentials" {
				_ = b.PushCredentials(ctx, id)
			} else if ev.Type == "job_done" {
				var p struct{ Status jobs.Status }
				if raw, err := json.Marshal(ev.Payload); err == nil && json.Unmarshal(raw, &p) == nil && isTerminal(p.Status) {
					b.updateCache(id, func(c *cachedJob) { c.Status = p.Status })
				}
			}
			return h(ev)
		})
	})
}

func (b *Backend) Permissions(ctx context.Context, id, state string) ([]jobs.PermissionInfo, error) {
	if t, err := b.getTask(ctx, id); err != nil || !isWorkspaceReady(t) {
		return nil, err
	}
	_ = b.EnsureCredentials(ctx, id)
	return b.jobClient(id).Permissions(ctx, id, state)
}
func (b *Backend) Decide(ctx context.Context, id, permID string, allow bool, rememberFor, reason string) error {
	return b.withRunning(ctx, id, func(c *jobclient.Client) error { return c.Decide(ctx, id, permID, allow, rememberFor, reason) })
}
func (b *Backend) Say(ctx context.Context, id, text string) error {
	return b.withRunning(ctx, id, func(c *jobclient.Client) error { return c.Say(ctx, id, text) })
}
func (b *Backend) Cancel(ctx context.Context, id string) error {
	err := b.withTempWake(ctx, id, func(c *jobclient.Client) error { return c.Cancel(ctx, id) })
	if err == nil {
		b.updateCache(id, func(c *cachedJob) { c.Status = jobs.Canceled })
	}
	return err
}
func (b *Backend) Continue(ctx context.Context, id, text string, maxIterations int) error {
	err := b.withRunning(ctx, id, func(c *jobclient.Client) error { return c.Continue(ctx, id, text, maxIterations) })
	if err == nil {
		b.updateCache(id, func(cj *cachedJob) { cj.Status, cj.Result = jobs.Running, nil })
		b.cacheMu.Lock()
		b.removePatches(id)
		b.cacheMu.Unlock()
	}
	return err
}
func (b *Backend) LoadSeq(id string) int64            { return b.jobClient(id).LoadSeq(id) }
func (b *Backend) SaveSeq(id string, seq int64) error { return b.jobClient(id).SaveSeq(id, seq) }

func (b *Backend) DeleteTask(ctx context.Context, id string) error {
	_, err1 := b.conn.AX.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Atespace: b.conn.Atespace, Name: TaskName(id)})
	_, err2 := b.conn.AX.DeleteWorkspace(ctx, &v1alpha1.DeleteWorkspaceRequest{Atespace: b.conn.Atespace, Name: WorkspaceName(id)})
	_, err3 := b.conn.Control.DeleteActorEgressPolicy(ctx, &ateapipb.DeleteActorEgressPolicyRequest{Actor: &ateapipb.ObjectRef{Atespace: b.conn.Atespace, Name: TaskName(id)}, Name: "default"})
	for _, err := range []error{err1, err2, err3} {
		if err != nil && status.Code(err) != codes.NotFound {
			return fmt.Errorf("axbackend: delete task resources for %s: %w", id, err)
		}
	}
	b.cacheMu.Lock()
	_ = os.Remove(b.cachePath(id))
	b.removePatches(id)
	b.cacheMu.Unlock()
	_ = os.Remove(jobclient.SeqPath(b.jobClient(id).BaseURL, id))
	return nil
}

func (b *Backend) ListTasks(ctx context.Context) ([]string, error) {
	resp, err := b.conn.AX.ListTasks(ctx, &v1alpha1.ListTasksRequest{Atespace: b.conn.Atespace})
	if err != nil {
		return nil, fmt.Errorf("axbackend: list tasks in %s: %w", b.conn.Atespace, err)
	}
	var names []string
	for _, t := range resp.GetTasks() {
		if name := strings.TrimSpace(t.GetMetadata().GetName()); name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

func (b *Backend) Do(ctx context.Context, method, subpath string, body []byte) (int, []byte, error) {
	subpath = strings.TrimLeft(subpath, "/")
	jobID, _, _ := strings.Cut(subpath, "/")
	_ = b.EnsureCredentials(ctx, jobID)
	cli := b.jobClient(jobID)
	req, err := http.NewRequestWithContext(ctx, method, cli.BaseURL+"/v1/jobs/"+subpath, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cli.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cli.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	return resp.StatusCode, data, err
}
