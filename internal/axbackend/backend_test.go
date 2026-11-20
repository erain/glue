// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package axbackend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erain/glue/daemon"
	"github.com/erain/glue/internal/eventlog"
	"github.com/erain/glue/jobs"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"
)

type fakeAXControlPlane struct {
	v1alpha1.UnimplementedAXServer
	mu                                     sync.Mutex
	steps                                  []string
	workspaces                             map[string]*v1alpha1.Workspace
	gateways                               map[string]*v1alpha1.Gateway
	tasks                                  map[string]*v1alpha1.Task
	polls                                  map[string]int
	pageSizeLimit                          int
	createTaskCalls, resumeCalls, getCalls int
	failFirstCreateTask                    error
	noRoom, neverReady, noRoomRPC          bool
	taskHTTPHits                           atomic.Int32
}

func delKey[V any](m map[string]V, k string) error {
	if _, ok := m[k]; !ok {
		return status.Error(codes.NotFound, "not found")
	}
	delete(m, k)
	return nil
}
func (f *fakeAXControlPlane) lockStep(name string) func() {
	f.mu.Lock()
	f.steps = append(f.steps, name)
	return f.mu.Unlock
}
func (f *fakeAXControlPlane) CreateGateway(_ context.Context, req *v1alpha1.CreateGatewayRequest) (*v1alpha1.Gateway, error) {
	defer f.lockStep("CreateGateway")()
	gw := req.GetGateway()
	if _, ok := f.gateways[gw.GetMetadata().GetName()]; ok {
		return nil, status.Error(codes.AlreadyExists, "gateway exists")
	}
	f.gateways[gw.GetMetadata().GetName()] = gw
	return gw, nil
}
func (f *fakeAXControlPlane) UpdateGateway(_ context.Context, req *v1alpha1.UpdateGatewayRequest) (*v1alpha1.Gateway, error) {
	defer f.lockStep("UpdateGateway")()
	gw := req.GetGateway()
	f.gateways[gw.GetMetadata().GetName()] = gw
	return gw, nil
}
func (f *fakeAXControlPlane) CreateTask(_ context.Context, req *v1alpha1.CreateTaskRequest) (*v1alpha1.Task, error) {
	defer f.lockStep("CreateTask")()
	f.createTaskCalls++
	t := req.GetTask()
	if _, ok := f.tasks[t.GetMetadata().GetName()]; ok {
		return nil, status.Error(codes.FailedPrecondition, "task already exists and is immutable")
	}
	if gwName := t.GetSpec().GetGateway().GetName(); gwName != "" && f.gateways[gwName] == nil {
		return nil, status.Error(codes.FailedPrecondition, "gateway not found")
	}
	err := f.failFirstCreateTask
	t.Status, f.tasks[t.GetMetadata().GetName()], f.failFirstCreateTask = &v1alpha1.TaskStatus{State: v1alpha1.TaskState_TASK_STATE_SUSPENDED}, t, nil
	return t, err
}
func (f *fakeAXControlPlane) SuspendTask(_ context.Context, req *v1alpha1.SuspendTaskRequest) (*v1alpha1.Task, error) {
	defer f.lockStep("SuspendTask")()
	if t, ok := f.tasks[req.GetName()]; ok {
		t.Status = &v1alpha1.TaskStatus{State: v1alpha1.TaskState_TASK_STATE_SUSPENDED}
		return t, nil
	}
	return nil, status.Error(codes.NotFound, "not found")
}
func (f *fakeAXControlPlane) ResumeTask(_ context.Context, req *v1alpha1.ResumeTaskRequest) (*v1alpha1.Task, error) {
	defer f.lockStep("ResumeTask")()
	f.resumeCalls, f.polls[req.GetName()] = f.resumeCalls+1, 0
	if f.noRoomRPC {
		return nil, status.Error(codes.Unavailable, "resuming task on substrate: resuming actor: rpc error: code = ResourceExhausted desc = no worker has room for the actor")
	}
	if t, ok := f.tasks[req.GetName()]; ok && f.gateways[t.GetSpec().GetGateway().GetName()] != nil {
		t.Status = &v1alpha1.TaskStatus{State: v1alpha1.TaskState_TASK_STATE_QUEUED}
		return t, nil
	}
	return nil, status.Error(codes.FailedPrecondition, "missing task or gateway before ResumeTask")
}
func (f *fakeAXControlPlane) GetTask(_ context.Context, req *v1alpha1.GetTaskRequest) (*v1alpha1.Task, error) {
	defer f.lockStep("GetTask")()
	f.getCalls++
	t, ok := f.tasks[req.GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	if f.polls[req.GetName()]++; f.noRoom && t.GetStatus().GetState() == v1alpha1.TaskState_TASK_STATE_QUEUED {
		t.Status = &v1alpha1.TaskStatus{State: v1alpha1.TaskState_TASK_STATE_QUEUED, Conditions: []*v1alpha1.Condition{{Type: v1alpha1.ConditionReady, Status: "False", Reason: v1alpha1.ReasonActorResumeFailed, Message: "resuming actor default/" + req.GetName() + ": rpc error: code = ResourceExhausted desc = no worker has room for the actor"}}}
	} else if f.neverReady {
		// stays Queued
	} else if t.GetStatus().GetState() == v1alpha1.TaskState_TASK_STATE_QUEUED && f.polls[req.GetName()] >= 2 {
		t.Status = &v1alpha1.TaskStatus{State: v1alpha1.TaskState_TASK_STATE_WORKING, Conditions: []*v1alpha1.Condition{{Type: v1alpha1.ConditionWorkspaceReady, Status: "True"}, {Type: v1alpha1.ConditionReady, Status: "True"}}}
	}
	return t, nil
}
func (f *fakeAXControlPlane) ListTasks(_ context.Context, req *v1alpha1.ListTasksRequest) (*v1alpha1.ListTasksResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := slices.Sorted(maps.Keys(f.tasks))
	start := 0
	if req.GetPageToken() != "" {
		_, _ = fmt.Sscanf(req.GetPageToken(), "%d", &start)
	}
	limit := len(keys)
	if f.pageSizeLimit > 0 && f.pageSizeLimit < limit {
		limit = f.pageSizeLimit
	}
	end := min(len(keys), start+limit)
	var out []*v1alpha1.Task
	for _, k := range keys[start:end] {
		out = append(out, f.tasks[k])
	}
	var next string
	if end < len(keys) {
		next = fmt.Sprintf("%d", end)
	}
	return &v1alpha1.ListTasksResponse{Tasks: out, NextPageToken: next}, nil
}
func (f *fakeAXControlPlane) DeleteTask(_ context.Context, req *v1alpha1.DeleteTaskRequest) (*emptypb.Empty, error) {
	defer f.lockStep("DeleteTask")()
	return &emptypb.Empty{}, delKey(f.tasks, req.GetName())
}
func (f *fakeAXControlPlane) DeleteGateway(_ context.Context, req *v1alpha1.DeleteGatewayRequest) (*emptypb.Empty, error) {
	defer f.lockStep("DeleteGateway")()
	return &emptypb.Empty{}, delKey(f.gateways, req.GetName())
}
func (f *fakeAXControlPlane) DeleteWorkspace(_ context.Context, req *v1alpha1.DeleteWorkspaceRequest) (*emptypb.Empty, error) {
	defer f.lockStep("DeleteWorkspace")()
	return &emptypb.Empty{}, delKey(f.workspaces, req.GetName())
}

func check(t *testing.T, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Fatalf(format, args...)
	}
}
func must[T any](v T, err error) func(*testing.T) T {
	return func(t *testing.T) T { t.Helper(); check(t, err == nil, "unexpected error: %v", err); return v }
}
func mustOK(t *testing.T, err error) { t.Helper(); check(t, err == nil, "unexpected error: %v", err) }

func newFakeBackendEnv(t *testing.T, handler http.HandlerFunc) (*fakeAXControlPlane, *fakeConnectRouter, Options, *Backend) {
	t.Helper()
	cp := &fakeAXControlPlane{workspaces: map[string]*v1alpha1.Workspace{}, gateways: map[string]*v1alpha1.Gateway{}, tasks: map[string]*v1alpha1.Task{}, polls: map[string]int{}}
	lis, srv := bufconn.Listen(1<<20), grpc.NewServer()
	v1alpha1.RegisterAXServer(srv, cp)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { srv.Stop(); _ = lis.Close() })
	dialOpt := []grpc.DialOption{grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials())}
	axCli, axConn, _ := DialAX("passthrough:///bufnet", dialOpt...)
	t.Cleanup(func() { _ = axConn.Close() })
	router := startFakeConnectRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cp.taskHTTPHits.Add(1)
		defer cp.lockStep(r.Method + " " + r.URL.Path)()
		w.Header().Set("Content-Type", "application/json")
		if handler != nil {
			handler(w, r)
			return
		}
		jobID := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/jobs/"), "/")[0]
		_, _ = fmt.Fprintf(w, `{"id":%q,"status":"running","credentials":"present"}`, jobID)
	}))
	opts := Options{Connect: ConnectOptions{Atespace: "default", RouterAddr: router.Addr, RouterTLS: router.ClientTLS, AXClient: axCli}, Image: "localhost:5001/glue-task@sha256:abcd", Secret: "unit-test-hmac-secret", CacheDir: filepath.Join(t.TempDir(), "ax-jobs"), PollInterval: 5 * time.Millisecond}
	b := must(NewBackend(t.Context(), opts))(t)
	t.Cleanup(func() { _ = b.Close() })
	return cp, router, opts, b
}

func requireLiveAX(t *testing.T) {
	t.Helper()
	if os.Getenv("GLUE_E2E_AX") != "1" {
		t.Skip("skipping live AX test; set GLUE_E2E_AX=1 to run")
	}
	if err := checkLiveAXPrecondition("", ""); err != nil {
		check(t, !axClusterRequired(), "live AX cluster is required when CI=true and GLUE_E2E_AX=1: %v", err)
		t.Skipf("live AX cluster unavailable (%v)", err)
	}
}

func TestDefaultImageFallsBackToGHCR(t *testing.T) {
	for in, want := range map[string]string{"v1.14.0": "v1.14.0", "v1.14.0-rc.1": "main", "v1.13.1-0.20261112192641-dfff7ecb5b01": "main", "(devel)": "main", "": "main", "v1.2": "main"} {
		check(t, publishedTag(in) == want, "publishedTag(%q) = %q, want %q", in, publishedTag(in), want)
	}
}

func TestDefaultImageEnvWins(t *testing.T) {
	t.Setenv("GLUE_AX_IMAGE", "example.com/custom:1")
	img, src := DefaultImageSource()
	check(t, img == "example.com/custom:1" && src == "env GLUE_AX_IMAGE", "env image = %q (%s)", img, src)
}

func TestDefaultImageAndRandomInjectedSecret(t *testing.T) {
	xdg := t.TempDir()
	for k, v := range map[string]string{"GLUE_AX_IMAGE": "", "GLUE_TASK_IMAGE": "", "GLUE_AX_SECRET": "", "XDG_CACHE_HOME": xdg} {
		t.Setenv(k, v)
	}
	img, src := DefaultImageSource()
	check(t, img == PublishedImage+":main" && src == "published image", "fallback image = %q (%s), want the published :main image", img, src)
	imgPath := filepath.Join(xdg, "glue", "ax-dev", "images.env")
	mustOK(t, errors.Join(os.MkdirAll(filepath.Dir(imgPath), 0o700), os.WriteFile(imgPath, []byte("export GLUE_TASK_IMAGE=\"localhost:5001/glue-task@sha256:1234\"\n"), 0o600)))
	check(t, DefaultImage() == "localhost:5001/glue-task@sha256:1234", "DefaultImage = %q", DefaultImage())
	fakeGHCR(t, false)
	cp, _, opts, _ := newFakeBackendEnv(t, nil)
	opts.Secret, opts.Image = "", ""
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	b1, b2 := must(NewBackend(t.Context(), opts))(t), must(NewBackend(t.Context(), opts))(t)
	t.Cleanup(func() { _ = b1.Close(); _ = b2.Close() })
	check(t, len(b1.secret) == 64 && b1.secret != b2.secret && b1.taskToken("job-123456") != b2.taskToken("job-123456"), "expected distinct random secrets")
	_, err := b1.Create(t.Context(), "job-noimage001", jobs.Spec{Objective: "test", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}})
	mustOK(t, err)
	cp.mu.Lock()
	taskImg := cp.tasks[TaskName("job-noimage001")].GetSpec().GetProfile().GetImage()
	cp.mu.Unlock()
	check(t, taskImg == PublishedImage+"@sha256:"+strings.Repeat("ab", 32), "task image without configuration = %q, want the published image pinned by digest", taskImg)
}

// fakeGHCR serves the anonymous token and manifest endpoints for the
// published image, answering with a fixed digest, or failing when down is set.
func fakeGHCR(t *testing.T, down bool) *int {
	t.Helper()
	heads := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case down:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		case r.URL.Path == "/token" && r.URL.Query().Get("scope") == "repository:erain/glue-task:pull":
			_, _ = w.Write([]byte(`{"token":"anon"}`))
		case r.Method == http.MethodHead && r.URL.Path == "/v2/erain/glue-task/manifests/main" && r.Header.Get("Authorization") == "Bearer anon" && strings.Contains(r.Header.Get("Accept"), "image.index"):
			*heads++
			w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("ab", 32))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	orig := ghcrBase
	ghcrBase = srv.URL
	pinMu.Lock()
	pinCache = map[string]string{}
	pinMu.Unlock()
	t.Cleanup(func() { ghcrBase = orig })
	return heads
}

func TestPinPublishedImage(t *testing.T) {
	heads := fakeGHCR(t, false)
	for range 2 {
		got, err := PinPublished(t.Context(), PublishedImage+":main")
		check(t, err == nil && got == PublishedImage+"@sha256:"+strings.Repeat("ab", 32), "pinned = %q, %v", got, err)
	}
	check(t, *heads == 1, "manifest HEADs = %d, want 1 (cached)", *heads)
	_, err := PinPublished(t.Context(), "localhost:5001/glue-task:main")
	check(t, err != nil, "a non-ghcr reference was pinned")
}

func TestUnpinnedImageWhenRegistryDown(t *testing.T) {
	fakeGHCR(t, true)
	xdg := t.TempDir()
	for k, v := range map[string]string{"GLUE_AX_IMAGE": "", "GLUE_TASK_IMAGE": "", "XDG_CACHE_HOME": xdg} {
		t.Setenv(k, v)
	}
	cp, _, opts, _ := newFakeBackendEnv(t, nil)
	var stages []string
	opts.Image, opts.Progress = "", func(s string) { stages = append(stages, s) }
	b := must(NewBackend(t.Context(), opts))(t)
	t.Cleanup(func() { _ = b.Close() })
	_, err := b.Create(t.Context(), "job-noimage002", jobs.Spec{Objective: "test", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}})
	mustOK(t, err)
	cp.mu.Lock()
	taskImg := cp.tasks[TaskName("job-noimage002")].GetSpec().GetProfile().GetImage()
	cp.mu.Unlock()
	check(t, taskImg == PublishedImage+":main", "fallback task image = %q", taskImg)
	check(t, len(stages) > 0 && strings.Contains(strings.Join(stages, "\n"), "could not resolve"), "no warning: %v", stages)
}

func TestAXSubmitReconcileCredentialsAndCleanup(t *testing.T) {
	t.Setenv("GLUE_GITHUB_TOKEN", "ghp_envtoken1234567890")
	t.Setenv("GEMINI_API_KEY", "AIzaEnvGeminiKey1234567890")
	var pushed []jobs.Credentials
	var credMu sync.Mutex
	cp, router, opts, b := newFakeBackendEnv(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/credentials":
			var c jobs.Credentials
			_ = json.NewDecoder(r.Body).Decode(&c)
			credMu.Lock()
			pushed = append(pushed, c)
			credMu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/jobs/job-111122223333/events":
			w.Header().Set("Content-Type", "text/event-stream")
			ev, _ := json.Marshal(eventlog.Envelope{Version: 2, ID: "job-111122223333:1", RunID: "job-111122223333", Seq: 1, Type: "job_done", Payload: map[string]any{"status": "achieved"}})
			_, _ = fmt.Fprintf(w, "id: 1\ndata: %s\n\n", ev)
		default:
			jobID := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/jobs/"), "/")[0]
			_, _ = fmt.Fprintf(w, `{"id":%q,"status":"running","credentials":"present"}`, jobID)
		}
	})
	id, spec := "job-111122223333", jobs.Spec{Objective: "Update docs", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox", BaseRef: "main"}}
	job := must(b.Submit(t.Context(), id, spec))(t)
	check(t, job.ID == id && job.Status == jobs.Running, "Submit: job=%+v", job)
	want := []string{"CreateGateway", "CreateTask", "ResumeTask", "GetTask", "GetTask", "POST /v1/credentials", "POST /v1/jobs/" + id + "/start"}
	check(t, reflect.DeepEqual(cp.steps, want), "steps = %v, want %v", cp.steps, want)
	task := cp.tasks[TaskName(id)]
	cfgStr := strings.ToLower(fmt.Sprint(task.GetSpec().GetConfig().AsMap()))
	cmdStr := strings.Join(task.GetSpec().GetCommands(), " ")
	check(t, len(task.GetSpec().GetEnv()) == 0 && task.GetSpec().GetGoal().GetText() == "Update docs" && strings.Contains(cmdStr, "--token "+daemon.HashToken(b.taskToken(id))) && !strings.Contains(cmdStr, b.taskToken(id)) && !strings.Contains(cfgStr, strings.ToLower(b.taskToken(id))) && !strings.Contains(cfgStr, "ghp_") && !strings.Contains(cfgStr, "aiza"), "unexpected task spec or leaked secret: %+v", task.GetSpec())
	check(t, len(pushed) == 1 && pushed[0].GitHubToken == "ghp_envtoken1234567890" && pushed[0].ProviderKeys["gemini"] == "AIzaEnvGeminiKey1234567890", "pushed = %+v", pushed)
	router.mu.Lock()
	caps := slices.Clone(router.connects)
	router.mu.Unlock()
	check(t, len(caps) == 2 && caps[0].TargetActor == "default/"+TaskName(id), "router captures = %+v", caps)
	var seen int
	mustOK(t, b.Stream(t.Context(), id, 0, func(ev eventlog.Envelope) error { seen++; return nil }))
	check(t, seen == 1, "seen = %d", seen)

	cp.failFirstCreateTask = status.Error(codes.DeadlineExceeded, "context deadline exceeded")
	_, err := b.Submit(t.Context(), "job-ambiguous001", spec)
	check(t, err != nil && b.loadCache("job-ambiguous001").Status == SubmitUnknown, "expected SubmitUnknown, got err=%v", err)
	beforeCreates := cp.createTaskCalls
	reconciled := must(b.Start(t.Context(), "job-ambiguous001"))(t)
	check(t, reconciled.Status == jobs.Running && cp.createTaskCalls == beforeCreates, "reconciled=%+v creates=%d", reconciled, cp.createTaskCalls)
	b2 := must(NewBackend(t.Context(), opts))(t)
	defer b2.Close()
	mustOK(t, errors.Join(b2.DeleteTask(t.Context(), id), b2.DeleteTask(t.Context(), id)))
	_, statErr := os.Stat(b.cachePath(id))
	check(t, errors.Is(statErr, os.ErrNotExist), "expected cache file removed, got %v", statErr)
}

func TestStatelessDiscoveryAndResultResumeSuspend(t *testing.T) {
	cp, _, opts, b1 := newFakeBackendEnv(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/credentials":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case strings.HasSuffix(r.URL.Path, "/result"):
			jobID := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/jobs/"), "/")[0]
			if jobID == "job-stateless001" {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"job is not finished"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(jobs.Result{JobID: jobID, Status: jobs.Achieved, Branch: "glue/" + jobID, Summary: "all tests green"})
		default:
			_, _ = w.Write([]byte(`{"status":"achieved","credentials":"present"}`))
		}
	})
	j1 := must(b1.Create(t.Context(), "job-stateless001", jobs.Spec{Objective: "First job", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}}))(t)
	time.Sleep(2 * time.Millisecond)
	j2 := must(b1.Create(t.Context(), "job-stateless002", jobs.Spec{Objective: "Second job", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}}))(t)
	opts.CacheDir = filepath.Join(t.TempDir(), "laptop2-ax-jobs")
	b2 := must(NewBackend(t.Context(), opts))(t)
	defer b2.Close()

	cp.mu.Lock()
	cp.pageSizeLimit = 1
	cp.mu.Unlock()
	cp.taskHTTPHits.Store(0)
	list := must(b2.ListJobs(t.Context(), jobs.Filter{}))(t)
	check(t, len(list) == 2 && list[0].ID == j2.ID && list[1].ID == j1.ID && list[0].Status == jobs.Suspended && list[0].Spec.Repo.URL == "https://github.com/erain/glue-sandbox", "ListJobs = %+v", list)
	d := must(b2.Show(t.Context(), j2.ID))(t)
	check(t, d.Status == jobs.Suspended && d.Spec.Objective == "Second job", "Show = %+v", d)
	check(t, len(must(b2.Permissions(t.Context(), j2.ID, "pending"))(t)) == 0, "expected empty permissions while suspended")
	check(t, cp.resumeCalls == 0 && cp.taskHTTPHits.Load() == 0, "expected 0 resume/HTTP hits, got %d/%d", cp.resumeCalls, cp.taskHTTPHits.Load())
	_, resErr := b2.Result(t.Context(), j1.ID)
	check(t, resErr != nil && cp.tasks[TaskName(j1.ID)].GetStatus().GetState() == v1alpha1.TaskState_TASK_STATE_SUSPENDED, "expected non-terminal Result to re-suspend, err=%v", resErr)

	res := must(b2.Result(t.Context(), j2.ID))(t)
	check(t, res.Status == jobs.Achieved && res.Summary == "all tests green" && cp.tasks[TaskName(j2.ID)].GetStatus().GetState() == v1alpha1.TaskState_TASK_STATE_SUSPENDED, "Result = %+v", res)
	check(t, must(os.Stat(b2.cachePath(j2.ID)))(t).Mode().Perm() == 0o600, "expected 0600 cache file")
	cp.getCalls, _ = 0, cp.taskHTTPHits.Swap(0)
	res2 := must(b2.Result(t.Context(), j2.ID))(t)
	check(t, res2.Summary == "all tests green" && cp.getCalls == 0 && cp.taskHTTPHits.Load() == 0, "cached Result made RPC/HTTP calls: %d/%d", cp.getCalls, cp.taskHTTPHits.Load())
	achieved := must(b2.ListJobs(t.Context(), jobs.Filter{Status: jobs.Achieved}))(t)
	check(t, len(achieved) == 1 && achieved[0].ID == j2.ID, "ListJobs(Achieved) = %+v", achieved)
}

func measure[T any](fn func() T) (T, time.Duration) {
	t0 := time.Now()
	return fn(), time.Since(t0)
}

func TestLiveAXJobLifecycle(t *testing.T) {
	requireLiveAX(t)
	for k, v := range map[string]string{"GLUE_CONFIG_DIR": t.TempDir(), "AX_SERVER": "", "GLUE_AX_SECRET": "", "GEMINI_API_KEY": "test-live-gemini-key"} {
		t.Setenv(k, v)
	}
	t.Cleanup(func() { _ = StopTunnels("") })
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	b1 := must(NewBackend(ctx, Options{}))(t)
	t.Cleanup(func() { _ = b1.Close() })
	for _, p := range must(filepath.Glob(filepath.Join(TunnelDir(), "*-hmac.json")))(t) {
		_ = os.Remove(p)
	}
	b2 := must(NewBackend(ctx, Options{CacheDir: filepath.Join(t.TempDir(), "laptop2")}))(t)
	t.Cleanup(func() { _ = b2.Close() })

	created, createDur := measure(func() jobs.Job {
		return must(b1.Create(ctx, "", jobs.Spec{Objective: "Verify direct AX lifecycle golden boot", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox.git", BaseRef: "main"}}))(t)
	})
	t.Cleanup(func() {
		cCtx, cCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cCancel()
		_ = b1.DeleteTask(cCtx, created.ID)
	})
	check(t, b1.taskToken(created.ID) == b2.taskToken(created.ID), "expected identical cluster HMAC task tokens across Backend instances")

	list, listDur := measure(func() []jobs.Job { return must(b2.ListJobs(ctx, jobs.Filter{}))(t) })
	check(t, slices.ContainsFunc(list, func(j jobs.Job) bool {
		return j.ID == created.ID && j.Status == jobs.Suspended && j.Spec.Repo.URL == "https://github.com/erain/glue-sandbox.git"
	}), "ListJobs missing %s: %+v", created.ID, list)
	check(t, must(b2.Show(ctx, created.ID))(t).Status == jobs.Suspended && must(b2.getTask(ctx, created.ID))(t).GetStatus().GetState() == v1alpha1.TaskState_TASK_STATE_SUSPENDED, "expected task to remain Suspended")

	_, resumeDur := measure(func() int { mustOK(t, b2.Resume(ctx, created.ID)); return 0 })
	d, showDur := measure(func() jobs.Detail { return must(b2.Show(ctx, created.ID))(t) })
	check(t, d.Status == jobs.Created && d.Credentials == "present" && d.Spec.Repo.URL == "https://github.com/erain/glue-sandbox.git", "Show after Resume: %+v", d)
	mustOK(t, b2.Suspend(ctx, created.ID))
	_, delDur := measure(func() int { mustOK(t, b2.DeleteTask(ctx, created.ID)); return 0 })
	_, getErr := b2.getTask(ctx, created.ID)
	check(t, status.Code(getErr) == codes.NotFound, "expected NotFound after DeleteTask, got %v", getErr)
	t.Logf("live AX job lifecycle timings (%s): create=%s list=%s resume+boot=%s show=%s delete=%s", created.ID, createDur, listDur, resumeDur, showDur, delDur)
}

func TestJobDiffUsesCache(t *testing.T) {
	var head atomic.Value
	head.Store("aaa111")
	cp, _, _, b := newFakeBackendEnv(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/credentials":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case strings.HasSuffix(r.URL.Path, "/patch"):
			w.Header().Set("Content-Type", "text/x-diff")
			_, _ = fmt.Fprintf(w, "diff --git a/x b/x\n+hello from %s\n", head.Load())
		case strings.HasSuffix(r.URL.Path, "/result"):
			_, _ = fmt.Fprintf(w, `{"status":"achieved","base_sha":"base","head_sha":%q}`, head.Load())
		case strings.HasSuffix(r.URL.Path, "/continue"):
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{}`))
		default:
			_, _ = w.Write([]byte(`{"status":"achieved","credentials":"present"}`))
		}
	})
	id := "job-diffcache001"
	must(b.Create(t.Context(), id, jobs.Spec{Objective: "Diff", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}}))(t)
	p1 := must(b.Patch(t.Context(), id))(t)
	check(t, strings.Contains(string(p1), "+hello from aaa111") && cp.tasks[TaskName(id)].GetStatus().GetState() == v1alpha1.TaskState_TASK_STATE_SUSPENDED, "first patch = %q state %v", p1, cp.tasks[TaskName(id)].GetStatus().GetState())
	hits, resumes := cp.taskHTTPHits.Load(), cp.resumeCalls
	p2 := must(b.Patch(t.Context(), id))(t)
	check(t, string(p2) == string(p1) && cp.taskHTTPHits.Load() == hits && cp.resumeCalls == resumes, "cached patch woke the task: hits %d->%d resumes %d->%d", hits, cp.taskHTTPHits.Load(), resumes, cp.resumeCalls)

	// Continuing the job and finishing on a new commit must not serve the old patch.
	mustOK(t, b.Continue(t.Context(), id, "also add a test", 0))
	head.Store("bbb222")
	p3 := must(b.Patch(t.Context(), id))(t)
	check(t, strings.Contains(string(p3), "+hello from bbb222"), "patch after continue = %q, want the new run's patch", p3)
	_, oldErr := os.Stat(b.patchPath(id, "aaa111"))
	check(t, errors.Is(oldErr, os.ErrNotExist), "old run's patch still cached: %v", oldErr)

	mustOK(t, b.DeleteTask(t.Context(), id))
	_, err := os.Stat(b.patchPath(id, "bbb222"))
	check(t, errors.Is(err, os.ErrNotExist), "patch cache not removed on delete: %v", err)
}

func TestTaskCommandCarriesOnlyTokenHashAndGateway(t *testing.T) {
	cp, _, _, b := newFakeBackendEnv(t, nil)
	id := "job-tokenhash001"
	must(b.Create(t.Context(), id, jobs.Spec{Objective: "Hash", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}}))(t)
	cp.mu.Lock()
	gw := cp.gateways[GatewayName(id)]
	task := cp.tasks[TaskName(id)]
	wsCount := len(cp.workspaces)
	cp.mu.Unlock()
	check(t, wsCount == 0 && task.GetSpec().GetWorkspace() == nil, "expected no per-job Workspace prewarm resource, got %d", wsCount)
	check(t, gw != nil && task.GetSpec().GetGateway().GetName() == GatewayName(id) && len(gw.GetSpec().GetEgress()) == 1, "gateway = %+v", gw)
	cmds := task.GetSpec().GetCommands()
	idx := slices.Index(cmds, "--token")
	check(t, idx >= 0 && idx+1 < len(cmds) && cmds[idx+1] == daemon.HashToken(b.taskToken(id)) && !strings.Contains(strings.Join(cmds, " "), b.taskToken(id)), "task commands must hold only the hash, got %v", cmds)
}

func TestSubmitProgress(t *testing.T) {
	_, _, opts, _ := newFakeBackendEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/credentials" {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		jobID := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/jobs/"), "/")[0]
		_, _ = fmt.Fprintf(w, `{"id":%q,"status":"running","credentials":"present"}`, jobID)
	})
	var stages []string
	opts.Progress = func(stage string) { stages = append(stages, stage) }
	b := must(NewBackend(t.Context(), opts))(t)
	defer b.Close()
	must(b.Submit(t.Context(), "job-progress0001", jobs.Spec{Objective: "Show stages", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}}))(t)
	want := []string{"gateway", "task", "sandbox ready", "credentials", "started"}
	check(t, reflect.DeepEqual(stages, want), "stages = %v, want %v", stages, want)
}

func TestDefaultEgressHosts(t *testing.T) {
	for _, h := range []string{"github.com", "proxy.golang.org", "registry.npmjs.org", "pypi.org", "files.pythonhosted.org", "generativelanguage.googleapis.com", "openrouter.ai"} {
		check(t, slices.Contains(DefaultEgressHosts, h), "DefaultEgressHosts lacks %s", h)
	}
}

func credsHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/credentials" {
		_, _ = w.Write([]byte(`{"ok":true}`))
		return
	}
	jobID := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/jobs/"), "/")[0]
	_, _ = fmt.Fprintf(w, `{"id":%q,"status":"running","credentials":"present"}`, jobID)
}

func TestSubmitReportsQueuedWhenNoWorker(t *testing.T) {
	orig := noCapacityGrace
	noCapacityGrace = 0
	t.Cleanup(func() { noCapacityGrace = orig })
	cp, _, _, b := newFakeBackendEnv(t, credsHandler)
	cp.mu.Lock()
	cp.noRoom = true
	cp.mu.Unlock()
	job, err := b.Submit(t.Context(), "job-queued000001", jobs.Spec{Objective: "wait for room", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}})
	check(t, errors.Is(err, ErrNoCapacity) && job.Status == Queued, "submit with a full pool = %+v, %v", job, err)
	list := must(b.ListJobs(t.Context(), jobs.Filter{}))(t)
	check(t, len(list) == 1 && list[0].Status == Queued, "list = %+v", list)
	d := must(b.Show(t.Context(), "job-queued000001"))(t)
	check(t, d.Status == Queued, "show status = %q", d.Status)
}

func TestQueuedJobStartsLater(t *testing.T) {
	orig := noCapacityGrace
	noCapacityGrace = 0
	t.Cleanup(func() { noCapacityGrace = orig })
	cp, _, _, b := newFakeBackendEnv(t, credsHandler)
	cp.mu.Lock()
	cp.noRoom = true
	cp.mu.Unlock()
	_, err := b.Submit(t.Context(), "job-queued000002", jobs.Spec{Objective: "x", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}})
	check(t, errors.Is(err, ErrNoCapacity), "first submit: %v", err)
	cp.mu.Lock()
	cp.noRoom = false
	cp.mu.Unlock()
	job := must(b.Start(t.Context(), "job-queued000002"))(t)
	check(t, job.Status == jobs.Running, "started later = %+v", job)
}

func TestStartTimeoutFlag(t *testing.T) {
	cp, _, opts, _ := newFakeBackendEnv(t, credsHandler)
	cp.mu.Lock()
	cp.neverReady = true
	cp.mu.Unlock()
	opts.StartTimeout = 50 * time.Millisecond
	b := must(NewBackend(t.Context(), opts))(t)
	defer b.Close()
	start := time.Now()
	job, err := b.Submit(t.Context(), "job-timeout00001", jobs.Spec{Objective: "x", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}})
	check(t, errors.Is(err, ErrStartTimeout) && job.Status == Queued && time.Since(start) < 5*time.Second, "start timeout: %+v %v after %s", job, err, time.Since(start))
}

func TestResumeRPCNoRoomIsQueued(t *testing.T) {
	cp, _, _, b := newFakeBackendEnv(t, credsHandler)
	cp.mu.Lock()
	cp.noRoomRPC = true
	cp.mu.Unlock()
	job, err := b.Submit(t.Context(), "job-queued000003", jobs.Spec{Objective: "x", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}})
	check(t, errors.Is(err, ErrNoCapacity) && job.Status == Queued && job.ID == "job-queued000003", "ResumeTask no-room = %+v, %v", job, err)
	list := must(b.ListJobs(t.Context(), jobs.Filter{}))(t)
	check(t, len(list) == 1 && list[0].Status == Queued, "list = %+v", list)
}

func TestTaskClonesRepoItself(t *testing.T) {
	cp, _, _, b := newFakeBackendEnv(t, credsHandler)
	_, err := b.Create(t.Context(), "job-clone0000001", jobs.Spec{Objective: "x", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox", BaseRef: "dev", SnapshotOf: "main"}})
	mustOK(t, err)
	cp.mu.Lock()
	task := cp.tasks[TaskName("job-clone0000001")]
	wsCount := len(cp.workspaces)
	cp.mu.Unlock()
	check(t, wsCount == 0 && task.GetSpec().GetWorkspace() == nil, "expected no workspace git setup: %+v", task.GetSpec().GetWorkspace())
	repo, _ := task.GetSpec().GetConfig().AsMap()["repo"].(map[string]any)
	check(t, repo["url"] == "https://github.com/erain/glue-sandbox" && repo["base_ref"] == "dev" && repo["snapshot_of"] == "main" && repo["path"] == nil, "task config repo = %v", repo)
}

func TestJobPRDoesNotWakeSuspendedAXTask(t *testing.T) {
	var prReq map[string]any
	var posts atomic.Int32
	routes := map[string]string{"GET /repos/erain/glue-sandbox": `{"default_branch":"main"}`, "GET /repos/erain/glue-sandbox/branches/feat/snap": `{"name":"feat/snap"}`, "GET /repos/erain/glue-sandbox/pulls": `[]`}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := routes[r.Method+" "+r.URL.Path]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		posts.Add(1)
		_ = json.NewDecoder(r.Body).Decode(&prReq)
		_, _ = w.Write([]byte(`{"html_url":"https://github.com/erain/glue-sandbox/pull/9"}`))
	}))
	t.Cleanup(gh.Close)
	t.Setenv("GLUE_GITHUB_API_URL", gh.URL)
	cp, _, _, b := newFakeBackendEnv(t, credsHandler)
	id := "job-pr0000000001"
	_, err := b.Create(t.Context(), id, jobs.Spec{Objective: "Fix bug", Issue: "erain/glue-sandbox#12", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox", BaseRef: "glue-snapshot/" + id, SnapshotOf: "feat/snap@deadbeef"}})
	mustOK(t, err)
	b.updateCache(id, func(c *cachedJob) {
		c.Status, c.Result = jobs.Achieved, &jobs.Result{JobID: id, Status: jobs.Achieved, Branch: "glue/" + id, BaseSHA: "111", HeadSHA: "222", Summary: "Fixed bug"}
	})
	cp.mu.Lock()
	resumesBefore, httpBefore := cp.resumeCalls, cp.taskHTTPHits.Load()
	cp.mu.Unlock()
	res1, res2 := must(b.OpenPR(t.Context(), id, "gh-tok"))(t), must(b.OpenPR(t.Context(), id, "gh-tok"))(t)
	cp.mu.Lock()
	resumesAfter := cp.resumeCalls
	cp.mu.Unlock()
	check(t, res1.PRURL == "https://github.com/erain/glue-sandbox/pull/9" && res2.PRURL == res1.PRURL && posts.Load() == 1, "res1=%+v res2=%+v posts=%d", res1, res2, posts.Load())
	check(t, prReq["draft"] == true && prReq["base"] == "feat/snap" && strings.Contains(fmt.Sprint(prReq["body"]), "Closes #12"), "prReq=%+v", prReq)
	check(t, resumesAfter == resumesBefore && cp.taskHTTPHits.Load() == httpBefore, "woke suspended task: resumes %d->%d http %d->%d", resumesBefore, resumesAfter, httpBefore, cp.taskHTTPHits.Load())
	check(t, must(b.Result(t.Context(), id))(t).PRURL == res1.PRURL && must(b.Show(t.Context(), id))(t).PRURL == res1.PRURL, "cached result/show missing pr_url")
}
