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
	"runtime/debug"
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

// Queued is recorded for a job whose task could not start because no AX
// worker had room, or did not become ready within the start timeout. Start
// it again with Start.
const Queued jobs.Status = "queued"

// ErrNoCapacity means no AX worker had room for the task.
var ErrNoCapacity = errors.New("no AX worker has room for the task")

// ErrStartTimeout means the task did not become ready within Options.StartTimeout.
var ErrStartTimeout = errors.New("the AX task did not become ready in time")

// noCapacityGrace is how long Resume waits before trusting a no-capacity failure.
var noCapacityGrace = 3 * time.Second

// DefaultStartTimeout bounds how long Resume waits for a task to be ready.
const DefaultStartTimeout = 2 * time.Minute

// noCapacity reports whether a task failed or queued only because no worker had room.
func noCapacity(t *v1alpha1.Task) bool {
	st := t.GetStatus().GetState()
	if st != v1alpha1.TaskState_TASK_STATE_FAILED && st != v1alpha1.TaskState_TASK_STATE_QUEUED {
		return false
	}
	if noRoomMessage(t.GetStatus().GetStatusDetail().GetDetail()) {
		return true
	}
	return slices.ContainsFunc(t.GetStatus().GetConditions(), func(c *v1alpha1.Condition) bool {
		return c.GetReason() == v1alpha1.ReasonActorResumeFailed && noRoomMessage(c.GetMessage())
	})
}

// noRoomMessage matches Substrate's "no worker has room" resume failure, which
// AX reports either from ResumeTask or later as a task condition.
func noRoomMessage(msg string) bool {
	return strings.Contains(msg, "ResourceExhausted") && strings.Contains(msg, "no worker has room") || strings.Contains(msg, "no worker has room for the actor")
}

// DefaultEgressHosts is the default TLS passthrough allowlist for AX coding tasks.
var DefaultEgressHosts = []string{"github.com", "api.github.com", "codeload.github.com", "proxy.golang.org", "sum.golang.org", "storage.googleapis.com", "generativelanguage.googleapis.com", "openrouter.ai", "registry.npmjs.org", "pypi.org", "files.pythonhosted.org"}

func WorkspaceName(jobID string) string { return "glue-" + jobID + "-ws" }
func GatewayName(jobID string) string   { return "glue-" + jobID + "-gw" }
func TaskName(jobID string) string      { return "glue-" + jobID }

// PublishedImage is the task image CI publishes; see .github/workflows/task-image.yml.
const PublishedImage = "ghcr.io/erain/glue-task"

// DefaultImage resolves the AX task image; see DefaultImageSource.
func DefaultImage() string {
	img, _ := DefaultImageSource()
	return img
}

// DefaultImageSource resolves the AX task image and says where it came from:
// GLUE_AX_IMAGE or GLUE_TASK_IMAGE, then the ax-dev images.env written by
// scripts/ax-dev/build-task-image.sh, then the published image for this
// build: the release tag for a tagged release, otherwise :main.
func DefaultImageSource() (image, source string) {
	for _, k := range []string{"GLUE_AX_IMAGE", "GLUE_TASK_IMAGE"} {
		if v := envOr(k); v != "" {
			return v, "env " + k
		}
	}
	base := envOr("XDG_CACHE_HOME")
	if home, _ := os.UserHomeDir(); base == "" && home != "" {
		base = filepath.Join(home, ".cache")
	}
	if base != "" {
		path := filepath.Join(base, "glue", "ax-dev", "images.env")
		data, _ := os.ReadFile(path)
		for line := range strings.SplitSeq(string(data), "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export ")), "="); ok && strings.TrimSpace(k) == "GLUE_TASK_IMAGE" {
				return strings.Trim(strings.TrimSpace(v), `"'`), path
			}
		}
	}
	return PublishedImage + ":" + publishedTag(buildVersion()), "published image"
}

// publishedTag maps a module version to a published image tag: a plain
// release version such as v1.14.0 maps to itself; anything else to "main".
func publishedTag(version string) string {
	v, ok := strings.CutPrefix(version, "v")
	if !ok || strings.ContainsAny(v, "-+") {
		return "main"
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return "main"
	}
	for _, p := range parts {
		if p == "" || strings.Trim(p, "0123456789") != "" {
			return "main"
		}
	}
	return version
}

func buildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		return info.Main.Version
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
	// StartTimeout bounds the wait for a resumed task to be ready (default 2m).
	StartTimeout time.Duration
	GetJob       func(ctx context.Context, id string) (jobs.Spec, jobs.Status, error)
	// Progress, when set, is called as each submit stage completes:
	// "gateway", "task", "sandbox ready", "credentials", "started".
	Progress  func(stage string)
	SetStatus func(ctx context.Context, id string, spec jobs.Spec, status jobs.Status) error
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
	// An empty Image means the default, resolved per task by taskImage.
	opts.Image, opts.PollInterval = strings.TrimSpace(opts.Image), cmp.Or(opts.PollInterval, 250*time.Millisecond)
	opts.StartTimeout = cmp.Or(opts.StartTimeout, DefaultStartTimeout)
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

func (b *Backend) progress(stage string) {
	if b.opts.Progress != nil {
		b.opts.Progress(stage)
	}
}

func resolveTaskSecret(ctx context.Context, opts Options, conn *Connection) (string, error) {
	if s := cmp.Or(strings.TrimSpace(opts.Secret), envOr("GLUE_AX_SECRET")); s != "" {
		return s, nil
	}
	var rnd [32]byte
	_, _ = rand.Read(rnd[:])
	gen := hex.EncodeToString(rnd[:])
	if opts.Connect.AXClient != nil || (conn.KubeConfig == "" && conn.KubeContext == "" && !inKubePod(false)) {
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
	return st != "" && st != jobs.Created && st != SubmitUnknown && st != Queued && st != jobs.Running && st != jobs.WaitingForInput && st != jobs.WaitingForCredentials && st != jobs.Suspended
}
func isTaskAwakeState(st v1alpha1.TaskState) bool {
	return st == v1alpha1.TaskState_TASK_STATE_WORKING || st == v1alpha1.TaskState_TASK_STATE_IDLE || st == v1alpha1.TaskState_TASK_STATE_REQUIRES_ACTION
}
func isTaskFailed(t *v1alpha1.Task) bool {
	st := t.GetStatus().GetState()
	return st == v1alpha1.TaskState_TASK_STATE_FAILED || st == v1alpha1.TaskState_TASK_STATE_CANCELLED
}
func isTaskSuspended(t *v1alpha1.Task) bool {
	return t.GetStatus().GetState() == v1alpha1.TaskState_TASK_STATE_SUSPENDED
}
func isWorkspaceReady(t *v1alpha1.Task) bool {
	return isTaskAwakeState(t.GetStatus().GetState()) && slices.ContainsFunc(t.GetStatus().GetConditions(), func(c *v1alpha1.Condition) bool {
		return (c.GetType() == v1alpha1.ConditionReady || c.GetType() == v1alpha1.ConditionWorkspaceReady) && c.GetStatus() == "True"
	})
}
func (b *Backend) getTask(ctx context.Context, id string) (*v1alpha1.Task, error) {
	return b.conn.AX.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: b.conn.Atespace, Name: TaskName(id)})
}
func (b *Backend) listAllTasks(ctx context.Context) ([]*v1alpha1.Task, error) {
	var all []*v1alpha1.Task
	var pageTok string
	for {
		resp, err := b.conn.AX.ListTasks(ctx, &v1alpha1.ListTasksRequest{Atespace: b.conn.Atespace, PageSize: 1000, PageToken: pageTok})
		if err != nil {
			return nil, fmt.Errorf("axbackend: list tasks in %s: %w", b.conn.Atespace, err)
		}
		all = append(all, resp.GetTasks()...)
		if pageTok = resp.GetNextPageToken(); pageTok == "" {
			return all, nil
		}
	}
}
func (b *Backend) ensureGateway(ctx context.Context, id string) error {
	gw := &v1alpha1.Gateway{
		Metadata: &v1alpha1.ObjectMeta{Name: GatewayName(id), Atespace: b.conn.Atespace},
		Spec: &v1alpha1.GatewaySpec{Egress: []*ateapipb.EgressRule{{
			TlsPassthrough: &ateapipb.TlsPassthroughRule{Hostnames: slices.Clone(b.opts.EgressHosts), Ports: &ateapipb.Ports{Numbers: []int32{443}}},
		}}},
	}
	_, err := b.conn.AX.CreateGateway(ctx, &v1alpha1.CreateGatewayRequest{Gateway: gw})
	if status.Code(err) == codes.AlreadyExists {
		_, err = b.conn.AX.UpdateGateway(ctx, &v1alpha1.UpdateGatewayRequest{Gateway: gw})
	}
	if err != nil {
		return fmt.Errorf("axbackend: ensure gateway %s: %w", GatewayName(id), err)
	}
	return nil
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

// Create provisions the AX Gateway and Task (reconciling via GetTask first on SubmitUnknown).
func (b *Backend) Create(ctx context.Context, id string, spec jobs.Spec) (jobs.Job, error) {
	if id = strings.TrimSpace(cmp.Or(id, spec.ID)); id == "" {
		id, _ = jobs.NewID()
	}
	img := b.taskImage(ctx)
	spec.ID, spec.Backend, spec.Objective = "", "", strings.TrimSpace(spec.Objective)
	spec.Repo.URL, spec.Repo.Path, spec.Repo.BaseRef = strings.TrimSpace(spec.Repo.URL), strings.TrimSpace(spec.Repo.Path), cmp.Or(strings.TrimSpace(spec.Repo.BaseRef), "main")
	if img == "" {
		return jobs.Job{}, fmt.Errorf("%w: no AX task image configured; pass --image <ref> or set GLUE_AX_IMAGE (run glue job doctor)", jobs.ErrInvalid)
	}
	if !strings.HasPrefix(id, "job-") || len(id) <= 4 || strings.ContainsAny(id, " \t\r\n/\\") || spec.Objective == "" || spec.Repo.URL == "" || spec.Repo.Path != "" {
		return jobs.Job{}, fmt.Errorf("%w: ax backend requires a job-* id, an objective, and repo.url (without repo.path)", jobs.ErrInvalid)
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
	if err := b.ensureGateway(ctx, id); err != nil {
		return job, err
	}
	b.progress("gateway")
	taskCfg := spec
	taskCfg.ID, taskCfg.Repo = id, jobs.RepoSpec{URL: spec.Repo.URL, BaseRef: spec.Repo.BaseRef, SnapshotOf: spec.Repo.SnapshotOf}
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
	cmd := []string{"glue", "serve", "--coding", "--jobs", "--job-from-ax", "--listen", ":8080", "--state-dir", "/workspace/.glue/state", "--token", daemon.HashToken(b.taskToken(id))}
	taskSpec := &v1alpha1.TaskSpec{
		Profile:  &v1alpha1.Profile{Image: img, Resources: &v1alpha1.ResourcesSpec{Limits: &v1alpha1.ResourceList{Cpu: b.opts.CPU, Memory: b.opts.Memory}}},
		Gateway:  &v1alpha1.GatewayRef{Name: GatewayName(id)},
		Commands: cmd,
		Goal:     &v1alpha1.Goal{Text: spec.Objective},
		Config:   cfgStruct,
	}
	if _, err := b.conn.AX.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Name: TaskName(id), Atespace: b.conn.Atespace}, Spec: taskSpec}}); err != nil && status.Code(err) != codes.AlreadyExists && !(status.Code(err) == codes.FailedPrecondition && strings.Contains(err.Error(), "already exists")) {
		if c := status.Code(err); errors.Is(err, context.DeadlineExceeded) || c == codes.DeadlineExceeded || c == codes.Unavailable || c == codes.Unknown {
			job.Status = SubmitUnknown
			b.recordState(ctx, id, spec, SubmitUnknown, now)
		}
		return job, fmt.Errorf("axbackend: create task %s: %w", TaskName(id), err)
	}
	b.recordState(ctx, id, spec, jobs.Created, now)
	b.progress("task")
	return job, nil
}

// IsSuspended reports whether the job's AX task is suspended, without waking it.
func (b *Backend) IsSuspended(ctx context.Context, id string) (bool, error) {
	t, err := b.getTask(ctx, id)
	if err != nil {
		return false, err
	}
	return isTaskSuspended(t), nil
}

func (b *Backend) Suspend(ctx context.Context, id string) error {
	_, err := b.conn.AX.SuspendTask(ctx, &v1alpha1.SuspendTaskRequest{Atespace: b.conn.Atespace, Name: TaskName(id)})
	return err
}

// Resume resumes the AX task, waits for readiness, and pushes credentials.
func (b *Backend) Resume(ctx context.Context, id string) error {
	if _, err := b.conn.AX.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Atespace: b.conn.Atespace, Name: TaskName(id)}); err != nil {
		if noRoomMessage(err.Error()) {
			return ErrNoCapacity
		}
		return fmt.Errorf("axbackend: resume task %s: %w", TaskName(id), err)
	}
	resumed := time.Now()
	deadline := resumed.Add(b.opts.StartTimeout)
	for {
		t, err := b.getTask(ctx, id)
		// A queued task still shows its earlier failure until AX picks up
		// this resume, so a no-capacity failure counts only after a grace period.
		if err == nil && noCapacity(t) {
			if time.Since(resumed) > noCapacityGrace {
				return ErrNoCapacity
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(b.opts.PollInterval):
			}
			continue
		}
		if err != nil || isTaskFailed(t) {
			return cmp.Or(err, fmt.Errorf("axbackend: task %s entered %v state", TaskName(id), t.GetStatus().GetState()))
		}
		if time.Now().After(deadline) {
			return ErrStartTimeout
		}
		if isWorkspaceReady(t) {
			b.progress("sandbox ready")
			if err := b.PushCredentials(ctx, id); err != nil {
				return err
			}
			b.progress("credentials")
			return nil
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
	if err == nil && noCapacity(t) {
		return b.Resume(ctx, id)
	}
	if err != nil || isTaskFailed(t) {
		return cmp.Or(err, fmt.Errorf("axbackend: task %s is in %v state", TaskName(id), t.GetStatus().GetState()))
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
		if errors.Is(err, ErrNoCapacity) || errors.Is(err, ErrStartTimeout) {
			// Leave the task created; a later Start resumes it again.
			_ = b.Suspend(ctx, id)
			b.recordState(ctx, id, spec, Queued, time.Time{})
			return jobs.Job{ID: id, Spec: spec, Status: Queued}, err
		}
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
	b.progress("started")
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

func resolveStateStatus(state v1alpha1.TaskState, st jobs.Status, suspendedWins bool) jobs.Status {
	switch {
	case isTerminal(st):
		return st
	case st == Queued && !isTaskAwakeState(state):
		return Queued
	case state == v1alpha1.TaskState_TASK_STATE_FAILED || state == v1alpha1.TaskState_TASK_STATE_CANCELLED:
		return jobs.Errored
	case state == v1alpha1.TaskState_TASK_STATE_SUSPENDED && (suspendedWins || st == ""):
		return jobs.Suspended
	case isTaskAwakeState(state) && (st == "" || st == SubmitUnknown || st == jobs.Created || st == jobs.Suspended):
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
	if err != nil || json.Unmarshal(raw, &spec) != nil {
		return jobs.Job{}, false
	}
	spec.Objective = cmp.Or(strings.TrimSpace(spec.Objective), strings.TrimSpace(t.GetSpec().GetGoal().GetText()))
	if spec.Objective == "" {
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
	j := jobs.Job{ID: id, Spec: spec, Status: resolveStateStatus(t.GetStatus().GetState(), cSt, true), CreatedAt: createdAt, UpdatedAt: createdAt}
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
	tasks, err := b.listAllTasks(ctx)
	if err != nil {
		return nil, err
	}
	var out []jobs.Job
	for _, t := range tasks {
		if j, ok := b.taskToJob(t); ok {
			out = append(out, j)
		}
	}
	b.reconcileAwake(ctx, tasks, out)
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

// IsTerminal reports whether st is a finished job status.
func IsTerminal(st jobs.Status) bool { return isTerminal(st) }

// reconcileTimeout bounds each status query reconcileAwake sends to an awake task.
const reconcileTimeout = 3 * time.Second

// reconcileAwake asks every awake task for its job status, four at a time.
// Finished jobs get their result cached and their task suspended, so they
// stop holding a worker. Suspended tasks are never contacted.
func (b *Backend) reconcileAwake(ctx context.Context, tasks []*v1alpha1.Task, out []jobs.Job) {
	idx := map[string]int{}
	for i, j := range out {
		idx[TaskName(j.ID)] = i
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	sem := make(chan struct{}, 4)
	for _, t := range tasks {
		i, ok := idx[t.GetMetadata().GetName()]
		if !ok || !isWorkspaceReady(t) {
			continue
		}
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			qctx, cancel := context.WithTimeout(ctx, reconcileTimeout)
			defer cancel()
			d, err := b.jobClient(id).Show(qctx, id)
			if err != nil || d.Status == "" {
				return
			}
			if isTerminal(d.Status) {
				b.settleFinished(qctx, id)
			}
			b.updateCache(id, func(c *cachedJob) { c.Status = d.Status })
			mu.Lock()
			out[i].Status = d.Status
			mu.Unlock()
		}(i, out[i].ID)
	}
	wg.Wait()
}

// settleFinished caches the completion record and detail of a finished job
// whose task is awake, then suspends the task. It does nothing while active.
func (b *Backend) settleFinished(ctx context.Context, id string) bool {
	if t, err := b.getTask(ctx, id); err != nil || !isWorkspaceReady(t) {
		return false
	}
	res, err := b.jobClient(id).Result(ctx, id)
	if err != nil || !isTerminal(res.Status) {
		return false
	}
	d, derr := b.jobClient(id).Show(ctx, id)
	b.updateCache(id, func(c *cachedJob) {
		c.Status, c.Result = res.Status, &res
		if derr == nil && d.ID != "" {
			c.Detail = &d
		}
	})
	return b.Suspend(ctx, id) == nil
}

// Show inspects a Glue job, querying the live task when running and reconstructing without waking when suspended.
func (b *Backend) Show(ctx context.Context, id string) (jobs.Detail, error) {
	t, err := b.getTask(ctx, id)
	if err != nil {
		return jobs.Detail{}, fmt.Errorf("axbackend: get task %s: %w", TaskName(id), err)
	}
	job, _ := b.taskToJob(t)
	if isWorkspaceReady(t) {
		d, ferr := b.fetchDetail(ctx, id)
		if ferr == nil {
			if job.Spec.Repo.URL != "" {
				d.Spec.Repo = job.Spec.Repo
			}
			b.updateCache(id, func(c *cachedJob) {
				c.Spec, c.Status, c.Detail = d.Spec, d.Status, &d
				if !isTerminal(d.Status) {
					c.Result = nil
				}
			})
			if isTerminal(d.Status) {
				b.settleFinished(ctx, id)
			}
			if c := b.loadCache(id); c != nil && c.Result != nil && d.PRURL == "" {
				d.PRURL = c.Result.PRURL
			}
			return d, nil
		}
		// A task suspending right after job_done can answer 503 before GetTask reflects Suspended.
		if c := b.loadCache(id); c == nil || !isTerminal(c.Status) {
			if t2, gerr := b.getTask(ctx, id); gerr != nil || isWorkspaceReady(t2) {
				return jobs.Detail{}, ferr
			}
		}
	}
	c := b.loadCache(id)
	d := jobs.Detail{Job: job}
	if c != nil && c.Detail != nil {
		d = *c.Detail
		d.Job.Status = job.Status
		if job.Spec.Objective != "" {
			d.Spec = job.Spec
		}
	}
	// A job that finished while attached has a cached result but may have
	// no cached detail; take its final iteration and usage from the result.
	if c != nil && c.Result != nil {
		d.PRURL = cmp.Or(d.PRURL, c.Result.PRURL)
		d.Iteration = max(d.Iteration, c.Result.Iterations)
		if c.Result.Usage.TotalTokens > d.Usage.TotalTokens {
			d.Usage = c.Result.Usage
		}
	}
	return d, nil
}

func (b *Backend) Status(ctx context.Context, id string) (jobs.Status, error) {
	_, st := b.cachedState(ctx, id)
	t, err := b.getTask(ctx, id)
	if err != nil {
		return cmp.Or(st, jobs.Created), err
	}
	return resolveStateStatus(t.GetStatus().GetState(), st, false), nil
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
	wasSuspended := isTaskSuspended(t)
	if err = b.withRunning(ctx, id, fn); wasSuspended {
		_ = b.Suspend(ctx, id)
	}
	return err
}

func (b *Backend) ReadResult(ctx context.Context, id string) (jobs.Result, error) {
	if c := b.loadCache(id); c != nil && c.Result != nil && isTerminal(c.Result.Status) {
		return *c.Result, nil
	}
	if t, err := b.getTask(ctx, id); err != nil || !isWorkspaceReady(t) {
		return jobs.Result{}, cmp.Or(err, fmt.Errorf("%w: task is suspended", jobs.ErrConflict))
	}
	return b.Result(ctx, id)
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

// OpenPR opens or reuses a draft pull request for a finished AX job and caches pr_url without waking suspended tasks.
func (b *Backend) OpenPR(ctx context.Context, id, token string) (jobs.Result, error) {
	res, err := b.Result(ctx, id)
	if err != nil || res.PRURL != "" {
		return res, err
	}
	spec, _ := b.cachedState(ctx, id)
	t, terr := b.getTask(ctx, id)
	if terr == nil {
		if j, ok := b.taskToJob(t); ok && spec.Objective == "" {
			spec = j.Spec
		} else if ok && spec.Repo.URL == "" {
			spec.Repo = j.Spec.Repo
		}
	}
	tok := cmp.Or(strings.TrimSpace(token), strings.TrimSpace(b.opts.Credentials.GitHubToken), envOr("GLUE_GITHUB_TOKEN"), envOr("GITHUB_TOKEN"))
	if tok == "" {
		return jobs.Result{}, errors.New("pr needs a GitHub token: set GLUE_GITHUB_TOKEN or GITHUB_TOKEN, or run gh auth login")
	}
	prURL, err := jobs.OpenDraftPR(ctx, jobs.Job{ID: id, Spec: spec}, res, tok)
	if err != nil {
		return jobs.Result{}, err
	}
	res.PRURL, res.DeliveryError = prURL, ""
	b.updateCache(id, func(c *cachedJob) {
		c.Result = &res
		if c.Detail != nil {
			c.Detail.PRURL = prURL
		}
	})
	if terr == nil && isWorkspaceReady(t) {
		_, _ = b.jobClient(id).SetPRURL(ctx, id, prURL)
	}
	return res, nil
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

// CachedPatch returns a locally cached terminal patch without contacting or waking the AX task.
func (b *Backend) CachedPatch(id string) ([]byte, bool) {
	c := b.loadCache(id)
	if c == nil || c.Result == nil {
		return nil, false
	}
	if c.Result.BaseSHA == "" || c.Result.HeadSHA == "" || c.Result.BaseSHA == c.Result.HeadSHA {
		return []byte{}, true
	}
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	data, err := os.ReadFile(b.patchPath(id, strings.TrimSpace(c.Result.HeadSHA)))
	return data, err == nil
}

// LivePatch fetches a job patch only if the AX task is already awake.
func (b *Backend) LivePatch(ctx context.Context, id string) ([]byte, error) {
	if t, err := b.getTask(ctx, id); err != nil || !isWorkspaceReady(t) {
		return nil, cmp.Or(err, fmt.Errorf("%w: task is suspended", jobs.ErrConflict))
	}
	_ = b.EnsureCredentials(ctx, id)
	return b.jobClient(id).Patch(ctx, id)
}

// Patch returns the job's patch, from the local cache when this run's patch
// was fetched before, so repeated calls do not wake the task.
func (b *Backend) Patch(ctx context.Context, id string) ([]byte, error) {
	res, err := b.Result(ctx, id)
	if err != nil {
		return b.LivePatch(ctx, id)
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
			finished := false
			if ev.Type == "job_waiting_for_credentials" {
				_ = b.PushCredentials(ctx, id)
			} else if ev.Type == "job_done" {
				var p struct{ Status jobs.Status }
				if raw, err := json.Marshal(ev.Payload); err == nil && json.Unmarshal(raw, &p) == nil && isTerminal(p.Status) {
					b.updateCache(id, func(c *cachedJob) { c.Status = p.Status })
					finished = true
				}
			}
			err := h(ev)
			if finished {
				b.settleFinished(context.WithoutCancel(ctx), id)
			}
			return err
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
func (b *Backend) Answer(ctx context.Context, id, questionID, text string) error {
	return b.withRunning(ctx, id, func(c *jobclient.Client) error { return c.Answer(ctx, id, questionID, text) })
}
func (b *Backend) Interrupt(ctx context.Context, id, text string) error {
	return b.withRunning(ctx, id, func(c *jobclient.Client) error { return c.Interrupt(ctx, id, text) })
}
func (b *Backend) Say(ctx context.Context, id, text string) error {
	return b.withRunning(ctx, id, func(c *jobclient.Client) error { return c.Say(ctx, id, text) })
}

// Cancel cancels the job and suspends its task once the job has finished.
func (b *Backend) Cancel(ctx context.Context, id string) error {
	err := b.withTempWake(ctx, id, func(c *jobclient.Client) error {
		if err := c.Cancel(ctx, id); err != nil {
			return err
		}
		for range 40 {
			if b.settleFinished(ctx, id) {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(b.opts.PollInterval):
			}
		}
		return nil
	})
	if err == nil {
		b.updateCache(id, func(c *cachedJob) {
			if !isTerminal(c.Status) {
				c.Status = jobs.Canceled
			}
		})
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

// DeleteTask deletes the job's AX task, gateway, and workspace (if any) and its local cache.
func (b *Backend) DeleteTask(ctx context.Context, id string) error {
	return b.deleteResources(ctx, id, false)
}

// Prune deletes the job's AX task, gateway, and workspace (if any) but keeps the
// locally cached completion record, so glue job result still works.
func (b *Backend) Prune(ctx context.Context, id string) error {
	if _, err := b.Result(ctx, id); err != nil {
		return fmt.Errorf("axbackend: cache result before pruning %s: %w", id, err)
	}
	return b.deleteResources(ctx, id, true)
}

func (b *Backend) deleteResources(ctx context.Context, id string, keepCache bool) error {
	_, err1 := b.conn.AX.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Atespace: b.conn.Atespace, Name: TaskName(id)})
	_, err2 := b.conn.AX.DeleteGateway(ctx, &v1alpha1.DeleteGatewayRequest{Atespace: b.conn.Atespace, Name: GatewayName(id)})
	_, err3 := b.conn.AX.DeleteWorkspace(ctx, &v1alpha1.DeleteWorkspaceRequest{Atespace: b.conn.Atespace, Name: WorkspaceName(id)})
	for _, err := range []error{err1, err2, err3} {
		if err != nil && status.Code(err) != codes.NotFound {
			return fmt.Errorf("axbackend: delete task resources for %s: %w", id, err)
		}
	}
	if keepCache {
		return nil
	}
	b.cacheMu.Lock()
	_ = os.Remove(b.cachePath(id))
	b.removePatches(id)
	b.cacheMu.Unlock()
	_ = os.Remove(jobclient.SeqPath(b.jobClient(id).BaseURL, id))
	return nil
}

func (b *Backend) ListTasks(ctx context.Context) ([]string, error) {
	tasks, err := b.listAllTasks(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, t := range tasks {
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
	if err == nil && method == http.MethodPost && resp.StatusCode < 300 && (strings.HasSuffix(subpath, "/continue") || strings.HasSuffix(subpath, "/goal")) {
		b.updateCache(jobID, func(c *cachedJob) { c.Status, c.Result = jobs.Running, nil })
		b.cacheMu.Lock()
		b.removePatches(jobID)
		b.cacheMu.Unlock()
	}
	return resp.StatusCode, data, err
}
