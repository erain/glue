// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package ax

import (
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/erain/glue/internal/eventlog"
	"github.com/erain/glue/internal/jobclient"
	"github.com/erain/glue/jobs"
	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

// SubmitUnknown is recorded when CreateTask fails with an ambiguous transport/deadline error.
const SubmitUnknown jobs.Status = "submit_unknown"

var defaultEgressHosts = []string{"github.com", "api.github.com", "codeload.github.com", "proxy.golang.org", "sum.golang.org", "storage.googleapis.com", "generativelanguage.googleapis.com", "openrouter.ai"}

func WorkspaceName(jobID string) string { return "glue-" + jobID + "-ws" }
func TaskName(jobID string) string      { return "glue-" + jobID }

// Options configures an AX Backend instance.
type Options struct {
	Atespace, Image, CPU, Memory, AXServer, Substrate, Router, CertFile, CAFile, TokenFile, Secret string
	EgressHosts                                                                                    []string
	Credentials                                                                                    jobs.Credentials
	PollInterval                                                                                   time.Duration
	AXClient                                                                                       v1alpha1.AXClient
	SubstrateClient                                                                                ateapipb.ControlClient
	RouterTLS                                                                                      *tls.Config
	RouterDialContext                                                                              func(ctx context.Context, network, addr string) (net.Conn, error)
	GetJob                                                                                         func(ctx context.Context, id string) (jobs.Spec, jobs.Status, error)
	SetStatus                                                                                      func(ctx context.Context, id string, spec jobs.Spec, status jobs.Status) error
}

// Backend executes Glue jobs as one-shot AX tasks routed through atenet-router.
type Backend struct {
	opts      Options
	axClient  v1alpha1.AXClient
	subClient ateapipb.ControlClient
	http      *http.Client
}

// NewBackend initializes an AX Backend and dials control-plane clients when not injected.
func NewBackend(opts Options) (*Backend, error) {
	opts.Atespace, opts.CPU, opts.Memory = cmp.Or(strings.TrimSpace(opts.Atespace), "default"), cmp.Or(strings.TrimSpace(opts.CPU), "4"), cmp.Or(strings.TrimSpace(opts.Memory), "8Gi")
	opts.Router, opts.PollInterval = cmp.Or(strings.TrimSpace(opts.Router), DefaultRouterAddr), cmp.Or(opts.PollInterval, 250*time.Millisecond)
	if len(opts.EgressHosts) == 0 {
		opts.EgressHosts = defaultEgressHosts
	}
	for p, def := range map[*string]string{&opts.CertFile: DefaultPodCertBundle, &opts.CAFile: DefaultServiceDNSCA, &opts.TokenFile: DefaultSubstrateTokenFile} {
		if _, err := os.Stat(def); strings.TrimSpace(*p) == "" && err == nil {
			*p = def
		}
	}
	if opts.GetJob == nil {
		opts.GetJob = func(context.Context, string) (jobs.Spec, jobs.Status, error) { return jobs.Spec{}, "", nil }
	}
	if opts.SetStatus == nil {
		opts.SetStatus = func(context.Context, string, jobs.Spec, jobs.Status) error { return nil }
	}
	rt, err := NewRouterTransport(RouterOptions{RouterAddr: opts.Router, Atespace: opts.Atespace, CertFile: opts.CertFile, CAFile: opts.CAFile, TLSConfig: opts.RouterTLS, DialContext: opts.RouterDialContext})
	if err != nil {
		return nil, err
	}
	b := &Backend{opts: opts, axClient: opts.AXClient, subClient: opts.SubstrateClient, http: &http.Client{Transport: rt}}
	if b.axClient == nil {
		b.axClient, _, err = DialAX(opts.AXServer)
	}
	if err == nil && b.subClient == nil {
		b.subClient, _, err = DialSubstrate(SubstrateOptions{Target: opts.Substrate, CAFile: opts.CAFile, TokenFile: opts.TokenFile})
	}
	return b, err
}
func (b *Backend) taskToken(jobID string) string {
	sum := sha256.Sum256([]byte("glue-ax-task-token-v1:" + cmp.Or(b.opts.Secret, "glue-ax-default-secret") + ":" + jobID))
	return hex.EncodeToString(sum[:])
}
func (b *Backend) jobClient(jobID string) *jobclient.Client {
	return &jobclient.Client{BaseURL: fmt.Sprintf("http://%s:%d", TaskName(jobID), DefaultTaskPort), Token: b.taskToken(jobID), HTTP: b.http}
}
func (b *Backend) credentials() jobs.Credentials {
	c := b.opts.Credentials
	c.GitHubToken = cmp.Or(strings.TrimSpace(c.GitHubToken), strings.TrimSpace(os.Getenv("GLUE_GITHUB_TOKEN")))
	pks := make(map[string]string, len(c.ProviderKeys)+4)
	for k, v := range c.ProviderKeys {
		if k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v); k != "" && v != "" {
			pks[k] = v
		}
	}
	for p, env := range map[string]string{"gemini": "GEMINI_API_KEY", "openrouter": "OPENROUTER_API_KEY", "deepseek": "DEEPSEEK_API_KEY", "nvidia": "NVIDIA_API_KEY"} {
		if pks[p] == "" {
			if v := strings.TrimSpace(os.Getenv(env)); v != "" {
				pks[p] = v
			}
		}
	}
	if len(pks) > 0 {
		c.ProviderKeys = pks
	}
	return c
}

// PushCredentials sends runtime credentials to the task over atenet-router.
func (b *Backend) PushCredentials(ctx context.Context, id string) error {
	cli, creds := b.jobClient(id), b.credentials()
	for attempt := 0; ; attempt++ {
		err := cli.PushCredentials(ctx, creds)
		if err == nil {
			return nil
		}
		if attempt >= 20 || ctx.Err() != nil {
			return fmt.Errorf("ax: push credentials for %s through router: %w", id, cmp.Or(ctx.Err(), err))
		}
		time.Sleep(b.opts.PollInterval)
	}
}

// EnsureCredentials pushes credentials when GET /v1/jobs/<id> reports credentials: "absent".
func (b *Backend) EnsureCredentials(ctx context.Context, id string) error {
	if d, err := b.jobClient(id).Show(ctx, id); err != nil {
		return err
	} else if d.Credentials == "absent" || d.Status == jobs.WaitingForCredentials {
		return b.PushCredentials(ctx, id)
	}
	return nil
}

// Create provisions the AX Workspace and Task (reconciling via GetTask first if submit_unknown).
func (b *Backend) Create(ctx context.Context, id string, spec jobs.Spec) (jobs.Job, error) {
	id = strings.TrimSpace(cmp.Or(id, spec.ID))
	spec.ID, spec.Backend, spec.Objective = id, "", strings.TrimSpace(spec.Objective)
	spec.Repo.URL, spec.Repo.Path, spec.Repo.BaseRef = strings.TrimSpace(spec.Repo.URL), strings.TrimSpace(spec.Repo.Path), cmp.Or(strings.TrimSpace(spec.Repo.BaseRef), "main")
	if id == "" || spec.Objective == "" || spec.Repo.URL == "" || spec.Repo.Path != "" {
		return jobs.Job{}, fmt.Errorf("%w: ax backend requires id, objective, and repo.url (without repo.path)", jobs.ErrInvalid)
	}
	job := jobs.Job{ID: id, Spec: spec, Status: jobs.Created, CreatedAt: time.Now().UTC()}
	if _, prevSt, _ := b.opts.GetJob(ctx, id); prevSt == SubmitUnknown {
		if _, err := b.axClient.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: b.opts.Atespace, Name: TaskName(id)}); err == nil {
			_ = b.opts.SetStatus(ctx, id, spec, jobs.Created)
			return job, nil
		} else if status.Code(err) != codes.NotFound {
			return job, fmt.Errorf("ax: reconcile task %s: %w", TaskName(id), err)
		}
	}
	if _, err := b.axClient.UpdateWorkspace(ctx, &v1alpha1.UpdateWorkspaceRequest{Workspace: &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: WorkspaceName(id), Atespace: b.opts.Atespace},
		Spec:     &v1alpha1.WorkspaceSpec{Git: []*v1alpha1.GitRepo{{Name: "repo", Repo: spec.Repo.URL, Branch: spec.Repo.BaseRef, Dir: ".", Depth: 1}}, Files: []*v1alpha1.File{{Path: ".glue/token", Content: b.taskToken(id) + "\n"}}},
	}}); err != nil {
		return job, fmt.Errorf("ax: update workspace %s: %w", WorkspaceName(id), err)
	}
	taskCfgSpec := spec
	taskCfgSpec.Repo = jobs.RepoSpec{Path: "/workspace", BaseRef: spec.Repo.BaseRef}
	rawCfg, _ := json.Marshal(taskCfgSpec)
	var cfgMap map[string]any
	_ = json.Unmarshal(rawCfg, &cfgMap)
	if taskCfgSpec.Budgets == (jobs.Budgets{}) {
		delete(cfgMap, "budgets")
	} else if bm, ok := cfgMap["budgets"].(map[string]any); ok && taskCfgSpec.Budgets.TokenBudget == 0 {
		delete(bm, "token_budget")
	}
	cfgStruct, _ := structpb.NewStruct(cfgMap)
	if _, err := b.axClient.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: TaskName(id), Atespace: b.opts.Atespace},
		Spec: &v1alpha1.TaskSpec{
			Image: b.opts.Image, Command: []string{"glue", "serve", "--coding", "--jobs", "--job-from-ax", "--listen", ":8080", "--state-dir", "/workspace/.glue/state"},
			Workspace: &v1alpha1.WorkspaceRef{Name: WorkspaceName(id), Path: "/workspace"}, Resources: &v1alpha1.ResourcesSpec{Limits: &v1alpha1.ResourceList{Cpu: b.opts.CPU, Memory: b.opts.Memory}}, Config: cfgStruct,
		},
	}}); err != nil && status.Code(err) != codes.AlreadyExists {
		if c := status.Code(err); errors.Is(err, context.DeadlineExceeded) || c == codes.DeadlineExceeded || c == codes.Unavailable || c == codes.Unknown {
			job.Status = SubmitUnknown
			_ = b.opts.SetStatus(ctx, id, spec, SubmitUnknown)
		}
		return job, fmt.Errorf("ax: create task %s: %w", TaskName(id), err)
	}
	_ = b.opts.SetStatus(ctx, id, spec, jobs.Created)
	return job, nil
}

// Start applies the egress policy, resumes the AX task, waits for WorkspaceReady, and starts the job via the router.
func (b *Backend) Start(ctx context.Context, id string) (jobs.Job, error) {
	spec, st, _ := b.opts.GetJob(ctx, id)
	if st == SubmitUnknown {
		if _, err := b.Create(ctx, id, spec); err != nil {
			return jobs.Job{}, err
		}
	}
	if _, err := b.subClient.CreateActorEgressPolicy(ctx, &ateapipb.CreateActorEgressPolicyRequest{
		Actor:        &ateapipb.ObjectRef{Atespace: b.opts.Atespace, Name: TaskName(id)},
		EgressPolicy: &ateapipb.EgressPolicy{Metadata: &ateapipb.ResourceMetadata{Atespace: b.opts.Atespace, Name: "default"}, Rules: []*ateapipb.EgressRule{{TlsPassthrough: &ateapipb.TlsPassthroughRule{Hostnames: slices.Clone(b.opts.EgressHosts), Ports: &ateapipb.Ports{Numbers: []int32{443}}}}}},
	}); err != nil && status.Code(err) != codes.AlreadyExists {
		return jobs.Job{}, fmt.Errorf("ax: create egress policy for %s: %w", TaskName(id), err)
	}
	if err := b.Resume(ctx, id); err != nil {
		return jobs.Job{}, err
	}
	return b.startRouter(ctx, id, spec)
}
func (b *Backend) Suspend(ctx context.Context, id string) error {
	_, err := b.axClient.SuspendTask(ctx, &v1alpha1.SuspendTaskRequest{Atespace: b.opts.Atespace, Name: TaskName(id)})
	return err
}
func (b *Backend) Resume(ctx context.Context, id string) error {
	if _, err := b.axClient.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Atespace: b.opts.Atespace, Name: TaskName(id)}); err != nil {
		return fmt.Errorf("ax: resume task %s: %w", TaskName(id), err)
	}
	for {
		t, err := b.axClient.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: b.opts.Atespace, Name: TaskName(id)})
		if err != nil || t.GetStatus().GetPhase() == "Failed" || ctx.Err() != nil {
			return cmp.Or(err, ctx.Err(), fmt.Errorf("ax: task %s entered Failed phase", TaskName(id)))
		}
		if t.GetStatus().GetPhase() == "Running" && slices.ContainsFunc(t.GetStatus().GetConditions(), func(c *v1alpha1.Condition) bool {
			return c.GetType() == "WorkspaceReady" && c.GetStatus() == "True"
		}) {
			return b.PushCredentials(ctx, id)
		}
		time.Sleep(b.opts.PollInterval)
	}
}
func (b *Backend) startRouter(ctx context.Context, id string, spec jobs.Spec) (jobs.Job, error) {
	cli := b.jobClient(id)
	var started jobs.Job
	for attempt := 0; ; attempt++ {
		var err error
		started, err = cli.Start(ctx, id)
		var se *jobclient.StatusError
		if err == nil || (errors.As(err, &se) && se.Code == http.StatusConflict) {
			break
		}
		if attempt >= 20 || ctx.Err() != nil {
			return jobs.Job{}, fmt.Errorf("ax: start job %s through router: %w", id, cmp.Or(ctx.Err(), err))
		}
		time.Sleep(b.opts.PollInterval)
	}
	started.Spec, started.ID, started.Status = spec, cmp.Or(started.ID, id), cmp.Or(started.Status, jobs.Running)
	_ = b.opts.SetStatus(ctx, id, spec, started.Status)
	return started, nil
}
func (b *Backend) Submit(ctx context.Context, id string, spec jobs.Spec) (jobs.Job, error) {
	if _, err := b.Create(ctx, id, spec); err != nil {
		return jobs.Job{}, err
	}
	return b.Start(ctx, id)
}
func (b *Backend) Status(ctx context.Context, id string) (jobs.Status, error) {
	_, st, _ := b.opts.GetJob(ctx, id)
	t, err := b.axClient.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: b.opts.Atespace, Name: TaskName(id)})
	if err == nil && t.GetStatus().GetPhase() == "Failed" {
		return jobs.Errored, nil
	} else if err == nil && t.GetStatus().GetPhase() == "Running" && (st == "" || st == jobs.Created) {
		return jobs.Running, nil
	}
	return cmp.Or(st, jobs.Created), err
}
func (b *Backend) Stream(ctx context.Context, id string, after int64, h func(eventlog.Envelope) error) error {
	_ = b.EnsureCredentials(ctx, id)
	return b.jobClient(id).StreamEvents(ctx, id, after, func(ev eventlog.Envelope) error {
		if ev.Type == "job_waiting_for_credentials" {
			_ = b.PushCredentials(ctx, id)
		}
		return h(ev)
	})
}
func (b *Backend) Result(ctx context.Context, id string) (jobs.Result, error) {
	return b.jobClient(id).Result(ctx, id)
}
func (b *Backend) Cancel(ctx context.Context, id string) error {
	return b.jobClient(id).Cancel(ctx, id)
}

// DeleteTask deletes the AX Task, Workspace, and per-actor EgressPolicy, ignoring NotFound errors.
func (b *Backend) DeleteTask(ctx context.Context, id string) error {
	if _, err := b.axClient.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Atespace: b.opts.Atespace, Name: TaskName(id)}); err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("ax: delete task %s: %w", TaskName(id), err)
	}
	if _, err := b.axClient.DeleteWorkspace(ctx, &v1alpha1.DeleteWorkspaceRequest{Atespace: b.opts.Atespace, Name: WorkspaceName(id)}); err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("ax: delete workspace %s: %w", WorkspaceName(id), err)
	}
	if _, err := b.subClient.DeleteActorEgressPolicy(ctx, &ateapipb.DeleteActorEgressPolicyRequest{
		Actor: &ateapipb.ObjectRef{Atespace: b.opts.Atespace, Name: TaskName(id)},
		Name:  "default",
	}); err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("ax: delete egress policy for %s: %w", TaskName(id), err)
	}
	return nil
}

// ListTasks returns the names of all AX tasks in the configured atespace.
func (b *Backend) ListTasks(ctx context.Context) ([]string, error) {
	resp, err := b.axClient.ListTasks(ctx, &v1alpha1.ListTasksRequest{Atespace: b.opts.Atespace})
	if err != nil {
		return nil, fmt.Errorf("ax: list tasks in %s: %w", b.opts.Atespace, err)
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
	jobID := strings.SplitN(subpath, "/", 2)[0]
	_ = b.EnsureCredentials(ctx, jobID)
	cli := b.jobClient(jobID)
	req, _ := http.NewRequestWithContext(ctx, method, cli.BaseURL+"/v1/jobs/"+subpath, strings.NewReader(string(body)))
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
