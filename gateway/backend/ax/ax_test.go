// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package ax

import (
	"context"
	"fmt"
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
	want := []string{"UpdateWorkspace", "CreateTask", "CreateActorEgressPolicy", "ResumeTask", "GetTask", "GetTask", "POST /v1/jobs/" + id + "/start"}
	if !reflect.DeepEqual(cp.steps, want) {
		t.Fatalf("steps = %v, want %v", cp.steps, want)
	}
	if git := cp.lastWS.GetSpec().GetGit(); len(git) != 1 || git[0].GetDir() != "." || git[0].GetDepth() != 1 || git[0].GetRepo() != spec.Repo.URL {
		t.Fatalf("unexpected workspace git spec: %+v", git)
	}
	if cfg := cp.tasks[TaskName(id)].GetSpec().GetConfig().AsMap(); cfg["id"] != id || strings.Contains(fmt.Sprint(cfg), b.taskToken(id)) {
		t.Fatalf("unexpected task config: %+v", cfg)
	}
	if caps := router.Captures(); len(caps) != 1 || caps[0].TargetActor != "default/"+TaskName(id) {
		t.Fatalf("unexpected router captures: %+v", caps)
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
