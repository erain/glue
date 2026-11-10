// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package ax

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erain/glue/jobs"
	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type fakeControlPlane struct {
	v1alpha1.UnimplementedAXServer
	ateapipb.UnimplementedControlServer
	mu                     sync.Mutex
	steps                  []string
	lastWS                 *v1alpha1.Workspace
	tasks                  map[string]*v1alpha1.Task
	hasEgress              bool
	polls, createTaskCalls int
	failFirstCreateTask    error
	taskHTTPHits           atomic.Int32
}

func (f *fakeControlPlane) UpdateWorkspace(_ context.Context, req *v1alpha1.UpdateWorkspaceRequest) (*v1alpha1.Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps, f.lastWS = append(f.steps, "UpdateWorkspace"), req.GetWorkspace()
	return f.lastWS, nil
}
func (f *fakeControlPlane) CreateTask(_ context.Context, req *v1alpha1.CreateTaskRequest) (*v1alpha1.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps, f.createTaskCalls = append(f.steps, "CreateTask"), f.createTaskCalls+1
	t := req.GetTask()
	if _, exists := f.tasks[t.GetMetadata().GetName()]; exists {
		return nil, status.Error(codes.AlreadyExists, "task already exists")
	}
	t.Status, f.tasks[t.GetMetadata().GetName()] = &v1alpha1.TaskStatus{Phase: "Suspended"}, t
	if err := f.failFirstCreateTask; err != nil {
		f.failFirstCreateTask = nil
		return nil, err
	}
	return t, nil
}
func (f *fakeControlPlane) CreateActorEgressPolicy(_ context.Context, req *ateapipb.CreateActorEgressPolicyRequest) (*ateapipb.EgressPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = append(f.steps, "CreateActorEgressPolicy")
	if f.hasEgress {
		return nil, status.Error(codes.AlreadyExists, "egress policy already exists")
	}
	f.hasEgress = true
	return req.GetEgressPolicy(), nil
}
func (f *fakeControlPlane) SuspendTask(_ context.Context, req *v1alpha1.SuspendTaskRequest) (*v1alpha1.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = append(f.steps, "SuspendTask")
	f.tasks[req.GetName()].Status = &v1alpha1.TaskStatus{Phase: "Suspended"}
	return f.tasks[req.GetName()], nil
}
func (f *fakeControlPlane) ResumeTask(_ context.Context, req *v1alpha1.ResumeTaskRequest) (*v1alpha1.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps, f.polls = append(f.steps, "ResumeTask"), 0
	t, ok := f.tasks[req.GetName()]
	if !ok || !f.hasEgress {
		return nil, status.Error(codes.FailedPrecondition, "missing task or egress policy before ResumeTask")
	}
	t.Status = &v1alpha1.TaskStatus{Phase: "Pending"}
	return t, nil
}
func (f *fakeControlPlane) GetTask(_ context.Context, req *v1alpha1.GetTaskRequest) (*v1alpha1.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = append(f.steps, "GetTask")
	t, ok := f.tasks[req.GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "task not found")
	}
	if f.polls++; f.polls >= 2 {
		t.Status = &v1alpha1.TaskStatus{Phase: "Running", Conditions: []*v1alpha1.Condition{{Type: "WorkspaceReady", Status: "True"}}}
	}
	return t, nil
}
func (f *fakeControlPlane) DeleteTask(_ context.Context, req *v1alpha1.DeleteTaskRequest) (*v1alpha1.DeleteTaskResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = append(f.steps, "DeleteTask")
	if _, ok := f.tasks[req.GetName()]; !ok {
		return nil, status.Error(codes.NotFound, "task not found")
	}
	delete(f.tasks, req.GetName())
	return &v1alpha1.DeleteTaskResponse{}, nil
}
func (f *fakeControlPlane) DeleteWorkspace(_ context.Context, req *v1alpha1.DeleteWorkspaceRequest) (*v1alpha1.DeleteWorkspaceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = append(f.steps, "DeleteWorkspace")
	if f.lastWS == nil || f.lastWS.GetMetadata().GetName() != req.GetName() {
		return nil, status.Error(codes.NotFound, "workspace not found")
	}
	f.lastWS = nil
	return &v1alpha1.DeleteWorkspaceResponse{}, nil
}
func (f *fakeControlPlane) DeleteActorEgressPolicy(_ context.Context, _ *ateapipb.DeleteActorEgressPolicyRequest) (*ateapipb.EgressPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = append(f.steps, "DeleteActorEgressPolicy")
	if !f.hasEgress {
		return nil, status.Error(codes.NotFound, "egress policy not found")
	}
	f.hasEgress = false
	return &ateapipb.EgressPolicy{}, nil
}
func (f *fakeControlPlane) ListTasks(_ context.Context, _ *v1alpha1.ListTasksRequest) (*v1alpha1.ListTasksResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*v1alpha1.Task
	for _, t := range f.tasks {
		out = append(out, t)
	}
	return &v1alpha1.ListTasksResponse{Tasks: out}, nil
}
func newFakeAXEnv(t *testing.T) (*fakeControlPlane, *fakeConnectRouter, Options, *Backend) {
	t.Helper()
	cp := &fakeControlPlane{tasks: map[string]*v1alpha1.Task{}}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	v1alpha1.RegisterAXServer(srv, cp)
	ateapipb.RegisterControlServer(srv, cp)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { srv.Stop(); _ = lis.Close() })
	dialOpt := []grpc.DialOption{grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials())}
	axCli, axConn, _ := DialAX("passthrough:///bufnet", dialOpt...)
	subCli, subConn, _ := DialSubstrate(SubstrateOptions{Target: "passthrough:///bufnet", Plaintext: true}, dialOpt...)
	t.Cleanup(func() { _ = axConn.Close(); _ = subConn.Close() })
	router := startFakeConnectRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cp.taskHTTPHits.Add(1)
		cp.mu.Lock()
		cp.steps = append(cp.steps, r.Method+" "+r.URL.Path)
		cp.mu.Unlock()
		jobID := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/jobs/"), "/")[0]
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"job":{"id":%q,"status":"running"}}`, jobID)
	}))
	opts := Options{
		Atespace: "default", Image: "localhost:5001/glue-task@sha256:abcd", CPU: "4", Memory: "8Gi", Router: router.Addr,
		CertFile: router.BundleFile, CAFile: router.CAFile, Secret: "test-secret", PollInterval: 5 * time.Millisecond, AXClient: axCli, SubstrateClient: subCli,
	}
	b, _ := NewBackend(opts)
	return cp, router, opts, b
}
func TestAXSubmitSequence(t *testing.T) {
	cp, router, _, b := newFakeAXEnv(t)
	id, spec := "job-111122223333", jobs.Spec{Objective: "Update project docs", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox", BaseRef: "main"}}
	if job, err := b.Submit(t.Context(), id, spec); err != nil || job.ID != id || job.Status != jobs.Running {
		t.Fatalf("Submit: job=%+v err=%v", job, err)
	}
	want := []string{"UpdateWorkspace", "CreateTask", "CreateActorEgressPolicy", "ResumeTask", "GetTask", "GetTask", "POST /v1/credentials", "POST /v1/jobs/" + id + "/start"}
	if !reflect.DeepEqual(cp.steps, want) {
		t.Fatalf("steps = %v, want %v", cp.steps, want)
	}
	if git := cp.lastWS.GetSpec().GetGit(); len(git) != 1 || git[0].GetDir() != "." || git[0].GetDepth() != 1 || git[0].GetRepo() != spec.Repo.URL {
		t.Fatalf("unexpected workspace git spec: %+v", git)
	}
	if cfg := cp.tasks[TaskName(id)].GetSpec().GetConfig().AsMap(); cfg["id"] != id || strings.Contains(fmt.Sprint(cfg), b.taskToken(id)) {
		t.Fatalf("unexpected task config: %+v", cfg)
	}
	if caps := router.Captures(); len(caps) != 2 || caps[0].TargetActor != "default/"+TaskName(id) || caps[1].TargetActor != "default/"+TaskName(id) {
		t.Fatalf("unexpected router captures: %+v", caps)
	}
}
func TestCredentialsPushedAtRuntimeAndOmittedFromTaskEnv(t *testing.T) {
	t.Setenv("GLUE_GITHUB_TOKEN", "ghp_envtoken1234567890")
	t.Setenv("GEMINI_API_KEY", "AIzaEnvGeminiKey1234567890")
	cp, _, opts, _ := newFakeAXEnv(t)
	var pushed []jobs.Credentials
	var credState atomic.Value
	credState.Store("absent")
	router := startFakeConnectRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cp.mu.Lock()
		cp.steps = append(cp.steps, r.Method+" "+r.URL.Path)
		cp.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/v1/credentials" {
			data, _ := io.ReadAll(r.Body)
			var c jobs.Credentials
			_ = json.Unmarshal(data, &c)
			cp.mu.Lock()
			pushed = append(pushed, c)
			cp.mu.Unlock()
			credState.Store("present")
			_, _ = w.Write([]byte(`{"ok":true,"credentials":"present"}`))
			return
		}
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/jobs/") {
			jobID := strings.TrimPrefix(r.URL.Path, "/v1/jobs/")
			_, _ = fmt.Fprintf(w, `{"id":%q,"status":"running","credentials":%q}`, jobID, credState.Load().(string))
			return
		}
		jobID := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/jobs/"), "/")[0]
		_, _ = fmt.Fprintf(w, `{"job":{"id":%q,"status":"running"}}`, jobID)
	}))
	opts.Router, opts.CertFile, opts.CAFile = router.Addr, router.BundleFile, router.CAFile
	b, err := NewBackend(opts)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	id, spec := "job-credpush0001", jobs.Spec{Objective: "Verify runtime credentials", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}}
	if _, err := b.Submit(t.Context(), id, spec); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if env := cp.tasks[TaskName(id)].GetSpec().GetEnv(); len(env) != 0 {
		t.Fatalf("expected zero Env entries in AX TaskSpec, got %+v", env)
	}
	if s := strings.ToLower(fmt.Sprint(cp.tasks[TaskName(id)])); strings.Contains(s, "token") || strings.Contains(s, "api_key") || strings.Contains(s, "aiza") || strings.Contains(s, "ghp_") {
		t.Fatalf("Task spec leaked token/credential string: %s", s)
	}
	if len(pushed) != 1 || pushed[0].GitHubToken != "ghp_envtoken1234567890" || pushed[0].ProviderKeys["gemini"] != "AIzaEnvGeminiKey1234567890" {
		t.Fatalf("unexpected initial pushed credentials: %+v", pushed)
	}
	if err := b.Suspend(t.Context(), id); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	credState.Store("absent")
	if err := b.Resume(t.Context(), id); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(pushed) != 2 || credState.Load().(string) != "present" {
		t.Fatalf("expected credentials pushed after Resume, got len=%d state=%v", len(pushed), credState.Load())
	}
	credState.Store("absent")
	if _, _, err := b.Do(t.Context(), http.MethodPost, id+"/messages", []byte(`{"text":"hi"}`)); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if len(pushed) != 3 || credState.Load().(string) != "present" {
		t.Fatalf("expected credentials pushed when GET /v1/jobs/%s reports absent, got len=%d state=%v", id, len(pushed), credState.Load())
	}
}
func TestAXSubmitIdempotentAfterRestart(t *testing.T) {
	cp, _, opts, b1 := newFakeAXEnv(t)
	b2, _ := NewBackend(opts)
	id, spec := "job-restart00001", jobs.Spec{Objective: "Idempotent submit", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}}
	if _, err := b1.Submit(t.Context(), id, spec); err != nil {
		t.Fatalf("first Submit: %v", err)
	}
	if job, err := b2.Submit(t.Context(), id, spec); err != nil || job.Status != jobs.Running || len(cp.tasks) != 1 {
		t.Fatalf("second Submit after restart: job=%+v err=%v tasks=%d", job, err, len(cp.tasks))
	}
}
func TestAmbiguousSubmitReconcilesWithoutDuplicate(t *testing.T) {
	cp, _, opts, _ := newFakeAXEnv(t)
	cp.failFirstCreateTask = status.Error(codes.DeadlineExceeded, "context deadline exceeded")
	var persistedStatus jobs.Status
	var persistedSpec jobs.Spec
	opts.GetJob = func(context.Context, string) (jobs.Spec, jobs.Status, error) {
		return persistedSpec, persistedStatus, nil
	}
	opts.SetStatus = func(_ context.Context, _ string, s jobs.Spec, st jobs.Status) error {
		persistedSpec, persistedStatus = s, st
		return nil
	}
	b, _ := NewBackend(opts)
	id, spec := "job-ambiguous001", jobs.Spec{Objective: "Handle ambiguous submit", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}}
	if _, err := b.Submit(t.Context(), id, spec); err == nil || persistedStatus != SubmitUnknown {
		t.Fatalf("expected deadline error and status=%q, got err=%v status=%q", SubmitUnknown, err, persistedStatus)
	}
	if job, err := b.Submit(t.Context(), id, spec); err != nil || job.Status != jobs.Running || persistedStatus != jobs.Running || cp.createTaskCalls != 1 {
		t.Fatalf("reconciled Submit: job=%+v status=%q err=%v createTaskCalls=%d", job, persistedStatus, err, cp.createTaskCalls)
	}
}
func TestStatusReadsDoNotTouchTaskHTTP(t *testing.T) {
	cp, _, _, b := newFakeAXEnv(t)
	id, spec := "job-statusnohttp", jobs.Spec{Objective: "Status check", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}}
	if _, err := b.Submit(t.Context(), id, spec); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	cp.taskHTTPHits.Store(0)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st, _ := b.Status(r.Context(), id)
		_, _ = fmt.Fprintf(w, `{"id":%q,"status":%q}`, id, st)
	})
	for _, path := range []string{"/v1/jobs", "/v1/jobs/" + id} {
		rec := httptest.NewRecorder()
		if h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil)); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"running"`) {
			t.Fatalf("GET %s: code=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
	if hits := cp.taskHTTPHits.Load(); hits != 0 {
		t.Fatalf("taskHTTPHits = %d, want 0", hits)
	}
}
func TestAXSuspendAndResume(t *testing.T) {
	cp, _, _, b := newFakeAXEnv(t)
	id, spec := "job-suspendres01", jobs.Spec{Objective: "Suspend and resume", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}}
	if _, err := b.Submit(t.Context(), id, spec); err != nil || b.Suspend(t.Context(), id) != nil || cp.tasks[TaskName(id)].GetStatus().GetPhase() != "Suspended" || b.Resume(t.Context(), id) != nil || cp.tasks[TaskName(id)].GetStatus().GetPhase() != "Running" {
		t.Fatalf("err=%v task=%+v", err, cp.tasks[TaskName(id)])
	}
}
func TestAXDeleteAndListTasks(t *testing.T) {
	cp, _, _, b := newFakeAXEnv(t)
	id, spec := "job-deletelist01", jobs.Spec{Objective: "Delete and list", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}}
	if _, err := b.Submit(t.Context(), id, spec); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if tasks, err := b.ListTasks(t.Context()); err != nil || !reflect.DeepEqual(tasks, []string{TaskName(id)}) {
		t.Fatalf("ListTasks = %v, err=%v", tasks, err)
	}
	cp.steps = nil
	if err := b.DeleteTask(t.Context(), id); err != nil || b.DeleteTask(t.Context(), id) != nil {
		t.Fatalf("DeleteTask: %v", err)
	}
	want := []string{"DeleteTask", "DeleteWorkspace", "DeleteActorEgressPolicy", "DeleteTask", "DeleteWorkspace", "DeleteActorEgressPolicy"}
	if !reflect.DeepEqual(cp.steps, want) {
		t.Fatalf("steps = %v, want %v", cp.steps, want)
	}
	if tasks, err := b.ListTasks(t.Context()); err != nil || len(tasks) != 0 {
		t.Fatalf("ListTasks after delete = %v, err=%v", tasks, err)
	}
}
