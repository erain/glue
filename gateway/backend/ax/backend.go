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
	cfgStruct, _ := structpb.NewStruct(cfgMap)
	var envVars []*v1alpha1.EnvVar
	for _, k := range []string{"GLUE_GITHUB_TOKEN", "GEMINI_API_KEY", "OPENROUTER_API_KEY"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			envVars = append(envVars, &v1alpha1.EnvVar{Name: k, Value: v})
		}
	}
	if _, err := b.axClient.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: TaskName(id), Atespace: b.opts.Atespace},
		Spec: &v1alpha1.TaskSpec{
			Image: b.opts.Image, Command: []string{"glue", "serve", "--coding", "--jobs", "--job-from-ax", "--listen", ":8080", "--state-dir", "/workspace/.glue/state", "--token-file", "/workspace/.glue/token"},
			Workspace: &v1alpha1.WorkspaceRef{Name: WorkspaceName(id), Path: "/workspace"}, Resources: &v1alpha1.ResourcesSpec{Limits: &v1alpha1.ResourceList{Cpu: b.opts.CPU, Memory: b.opts.Memory}}, Env: envVars, Config: cfgStruct,
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
	if _, err := b.axClient.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Atespace: b.opts.Atespace, Name: TaskName(id)}); err != nil {
		return jobs.Job{}, fmt.Errorf("ax: resume task %s: %w", TaskName(id), err)
	}
	for {
		t, err := b.axClient.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: b.opts.Atespace, Name: TaskName(id)})
		if err != nil || t.GetStatus().GetPhase() == "Failed" || ctx.Err() != nil {
			return jobs.Job{}, cmp.Or(err, ctx.Err(), fmt.Errorf("ax: task %s entered Failed phase", TaskName(id)))
		}
		if t.GetStatus().GetPhase() == "Running" && slices.ContainsFunc(t.GetStatus().GetConditions(), func(c *v1alpha1.Condition) bool {
			return c.GetType() == "WorkspaceReady" && c.GetStatus() == "True"
		}) {
			break
		}
		time.Sleep(b.opts.PollInterval)
	}
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
	return b.jobClient(id).StreamEvents(ctx, id, after, h)
}
func (b *Backend) Result(ctx context.Context, id string) (jobs.Result, error) {
	return b.jobClient(id).Result(ctx, id)
}
func (b *Backend) Cancel(ctx context.Context, id string) error {
	return b.jobClient(id).Cancel(ctx, id)
}
func (b *Backend) Do(ctx context.Context, method, subpath string, body []byte) (int, []byte, error) {
	subpath = strings.TrimLeft(subpath, "/")
	cli := b.jobClient(strings.SplitN(subpath, "/", 2)[0])
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
