// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/daemon"
	"github.com/erain/glue/internal/axbackend"
	"github.com/erain/glue/internal/jobclient"
	"github.com/erain/glue/jobs"
	filestore "github.com/erain/glue/stores/file"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"
	"gopkg.in/yaml.v3"
)

func checkAX(t *testing.T, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Fatalf(format, args...)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	checkAX(t, err == nil, "git %v: %v (%s)", args, err, out)
}

type fakeAXRig struct {
	v1alpha1.UnimplementedAXServer
	t            *testing.T
	repoDir      string
	makeProvider func() glue.Provider
	mu           sync.Mutex
	workspaces   map[string]*v1alpha1.Workspace
	gateways     map[string]*v1alpha1.Gateway
	tasks        map[string]*v1alpha1.Task
	daemons      map[string]*daemon.Server
	managers     map[string]*jobs.Manager
	restarts     map[string]func() *jobs.Manager
	lastOpts     axbackend.Options
	resumes      int
}

func newFakeAXRig(t *testing.T, makeProvider func() glue.Provider) *fakeAXRig {
	t.Helper()
	rig := &fakeAXRig{
		t: t, repoDir: serveJobRepo(t), makeProvider: makeProvider,
		workspaces: map[string]*v1alpha1.Workspace{}, gateways: map[string]*v1alpha1.Gateway{}, tasks: map[string]*v1alpha1.Task{},
		daemons: map[string]*daemon.Server{}, managers: map[string]*jobs.Manager{},
		restarts: map[string]func() *jobs.Manager{},
	}
	lis, gs := bufconn.Listen(1<<20), grpc.NewServer()
	v1alpha1.RegisterAXServer(gs, rig)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(func() { gs.Stop(); _ = lis.Close() })
	dialOpts := []grpc.DialOption{grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials())}
	axCli, axCC, err := axbackend.DialAX("passthrough:///bufnet", dialOpts...)
	checkAX(t, err == nil, "DialAX: %v", err)
	t.Cleanup(func() { _ = axCC.Close() })

	upTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.Host)
		rig.mu.Lock()
		srv := rig.daemons[cmp.Or(host, r.Host)]
		rig.mu.Unlock()
		if srv != nil {
			srv.ServeHTTP(w, r)
		} else {
			http.NotFound(w, r)
		}
	}))
	tlsSrc := httptest.NewTLSServer(nil)
	t.Cleanup(func() { upTS.Close(); tlsSrc.Close() })
	serverTLS := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: tlsSrc.TLS.Certificates}
	dialRouter := func(context.Context, string, string) (net.Conn, error) {
		cConn, sConn := net.Pipe()
		go func() {
			tc := tls.Server(sConn, serverTLS)
			defer tc.Close()
			if br := bufio.NewReader(tc); func() bool { req, err := http.ReadRequest(br); return err == nil && req.Method == http.MethodConnect }() {
				if up, err := net.Dial("tcp", upTS.Listener.Addr().String()); err == nil {
					defer up.Close()
					_, _ = io.WriteString(tc, "HTTP/1.1 200 Connection Established\r\n\r\n")
					done := make(chan struct{})
					go func() { _, _ = io.Copy(up, br); close(done) }()
					_, _ = io.Copy(tc, up)
					<-done
				}
			}
		}()
		return cConn, nil
	}
	cacheDir, origNew := filepath.Join(t.TempDir(), "ax-jobs"), newAXJobAPI
	t.Cleanup(func() { newAXJobAPI = origNew })
	newAXJobAPI = func(ctx context.Context, opts axbackend.Options) (jobAPI, error) {
		rig.mu.Lock()
		rig.lastOpts, opts.Image, opts.Secret, opts.CacheDir, opts.PollInterval = opts, cmp.Or(opts.Image, "localhost:5001/glue-task:test"), "test-hmac-secret", cacheDir, 5*time.Millisecond
		rig.mu.Unlock()
		opts.Connect = axbackend.ConnectOptions{Atespace: opts.Connect.Atespace, AXClient: axCli, RouterDialContext: dialRouter, RouterTLS: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}}
		b, err := axbackend.NewBackend(ctx, opts)
		if err != nil {
			return nil, err
		}
		return &axJobAPI{b}, nil
	}
	return rig
}

func (r *fakeAXRig) lock() func() { r.mu.Lock(); return r.mu.Unlock }
func (r *fakeAXRig) taskOrErr(name string) (*v1alpha1.Task, error) {
	if t, ok := r.tasks[name]; ok {
		return t, nil
	}
	return nil, status.Error(codes.NotFound, "not found")
}
func (r *fakeAXRig) CreateGateway(_ context.Context, req *v1alpha1.CreateGatewayRequest) (*v1alpha1.Gateway, error) {
	defer r.lock()()
	gw := req.GetGateway()
	if _, ok := r.gateways[gw.GetMetadata().GetName()]; ok {
		return nil, status.Error(codes.AlreadyExists, "exists")
	}
	r.gateways[gw.GetMetadata().GetName()] = gw
	return gw, nil
}
func (r *fakeAXRig) UpdateGateway(_ context.Context, req *v1alpha1.UpdateGatewayRequest) (*v1alpha1.Gateway, error) {
	defer r.lock()()
	gw := req.GetGateway()
	r.gateways[gw.GetMetadata().GetName()] = gw
	return gw, nil
}
func (r *fakeAXRig) CreateTask(ctx context.Context, req *v1alpha1.CreateTaskRequest) (*v1alpha1.Task, error) {
	defer r.lock()()
	task := req.GetTask()
	name := task.GetMetadata().GetName()
	if _, ok := r.tasks[name]; ok {
		return nil, status.Error(codes.AlreadyExists, "exists")
	}
	var tok string
	cmds := task.GetSpec().GetCommands()
	if idx := slices.Index(cmds, "--token"); idx >= 0 && idx+1 < len(cmds) {
		tok = strings.TrimSpace(cmds[idx+1])
	}
	rawCfg, _ := json.Marshal(task.GetSpec().GetConfig().AsMap())
	var spec jobs.Spec
	_ = json.Unmarshal(rawCfg, &spec)
	spec.Repo = jobs.RepoSpec{Path: r.repoDir, BaseRef: "main"}
	if spec.Delivery == "draft_pr" || spec.Delivery == "branch" {
		spec.Delivery = "none"
	}
	stateDir, prov := r.t.TempDir(), r.makeProvider()
	mgr := must(jobs.New(jobs.Options{StateDir: stateDir, ProviderFactory: fakeFactory(prov), RequireCredentials: true}))
	if _, err := mgr.Create(ctx, spec); err != nil {
		_ = mgr.Close()
		return nil, err
	}
	srv := must(daemon.New(daemon.Options{Host: glue.NewAgent(glue.AgentOptions{Provider: prov, Model: "fake", Store: filestore.New(r.t.TempDir())}), Token: tok, StateDir: stateDir, Jobs: mgr}))
	r.t.Cleanup(func() { _ = srv.Close(); _ = mgr.Close() })
	task.Status = &v1alpha1.TaskStatus{State: v1alpha1.TaskState_TASK_STATE_SUSPENDED}
	jobID := strings.TrimPrefix(name, "glue-")
	r.tasks[name], r.daemons[name], r.managers[jobID] = task, srv, mgr
	r.restarts[jobID] = func() *jobs.Manager {
		_ = errors.Join(srv.Close(), mgr.Close())
		p2 := r.makeProvider()
		mgr = must(jobs.New(jobs.Options{StateDir: stateDir, ProviderFactory: fakeFactory(p2), RequireCredentials: true}))
		srv = must(daemon.New(daemon.Options{Host: glue.NewAgent(glue.AgentOptions{Provider: p2, Model: "fake", Store: filestore.New(r.t.TempDir())}), Token: tok, StateDir: stateDir, Jobs: mgr}))
		r.t.Cleanup(func() { _ = srv.Close(); _ = mgr.Close() })
		defer r.lock()()
		r.tasks[name].Status, r.daemons[name], r.managers[jobID] = &v1alpha1.TaskStatus{State: v1alpha1.TaskState_TASK_STATE_SUSPENDED}, srv, mgr
		return mgr
	}
	return task, nil
}
func (r *fakeAXRig) ResumeTask(_ context.Context, req *v1alpha1.ResumeTaskRequest) (*v1alpha1.Task, error) {
	defer r.lock()()
	r.resumes++
	if t, ok := r.tasks[req.GetName()]; ok && r.gateways[t.GetSpec().GetGateway().GetName()] != nil {
		t.Status = &v1alpha1.TaskStatus{State: v1alpha1.TaskState_TASK_STATE_WORKING, Conditions: []*v1alpha1.Condition{{Type: v1alpha1.ConditionWorkspaceReady, Status: "True"}, {Type: v1alpha1.ConditionReady, Status: "True"}}}
		return t, nil
	}
	return nil, status.Error(codes.FailedPrecondition, "missing task or gateway")
}
func (r *fakeAXRig) SuspendTask(_ context.Context, req *v1alpha1.SuspendTaskRequest) (*v1alpha1.Task, error) {
	defer r.lock()()
	t, err := r.taskOrErr(req.GetName())
	if err == nil {
		t.Status = &v1alpha1.TaskStatus{State: v1alpha1.TaskState_TASK_STATE_SUSPENDED}
	}
	return t, err
}
func (r *fakeAXRig) GetTask(_ context.Context, req *v1alpha1.GetTaskRequest) (*v1alpha1.Task, error) {
	defer r.lock()()
	return r.taskOrErr(req.GetName())
}
func (r *fakeAXRig) ListTasks(_ context.Context, _ *v1alpha1.ListTasksRequest) (*v1alpha1.ListTasksResponse, error) {
	defer r.lock()()
	return &v1alpha1.ListTasksResponse{Tasks: slices.Collect(maps.Values(r.tasks))}, nil
}
func (r *fakeAXRig) DeleteTask(_ context.Context, req *v1alpha1.DeleteTaskRequest) (*emptypb.Empty, error) {
	defer r.lock()()
	delete(r.tasks, req.GetName())
	return &emptypb.Empty{}, nil
}
func (r *fakeAXRig) DeleteGateway(_ context.Context, req *v1alpha1.DeleteGatewayRequest) (*emptypb.Empty, error) {
	defer r.lock()()
	delete(r.gateways, req.GetName())
	return &emptypb.Empty{}, nil
}
func (r *fakeAXRig) DeleteWorkspace(_ context.Context, req *v1alpha1.DeleteWorkspaceRequest) (*emptypb.Empty, error) {
	defer r.lock()()
	delete(r.workspaces, req.GetName())
	return &emptypb.Empty{}, nil
}

func clearJobTestEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "GLUE_CONFIG_DIR", "GLUE_NOTIFY_URL", "GLUE_DELIVER", "GLUE_PROVIDER", "GLUE_MODEL", "GLUE_THINKING", "GLUE_AX_ATESPACE", "GLUE_SERVER", "GLUE_TOKEN", "GLUE_DAEMON_TOKEN", "AX_SERVER", "AX_SERVER_ADDR", "GLUE_AX_SERVER", "GLUE_AX_KUBECONFIG", "GLUE_AX_CONTEXT", "KUBECONFIG", "KUBE_CONTEXT", "KUBECONTEXT", "GEMINI_API_KEY", "OPENROUTER_API_KEY", "DEEPSEEK_API_KEY", "NVIDIA_API_KEY", "OPENAI_API_KEY", "GLUE_GITHUB_TOKEN", "GITHUB_TOKEN"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	t.Setenv("HOME", t.TempDir())
}

func TestAXRepoResolution(t *testing.T) {
	clearJobTestEnv(t)
	ctx := context.Background()
	for _, tc := range []struct{ in, want string }{
		{"git@github.com:erain/glue.git", "https://github.com/erain/glue.git"},
		{"ssh://git@github.com/erain/glue.git", "https://github.com/erain/glue.git"},
		{"ssh://git@github.com:22/erain/glue.git", "https://github.com/erain/glue.git"},
		{"https://user:ghp_secret123@github.com/erain/glue.git", "https://github.com/erain/glue.git"},
		{"http://tok@example.com:8080/org/repo.git", "http://example.com:8080/org/repo.git"},
	} {
		rs, err := resolveAXRepoSpec(ctx, tc.in, "")
		checkAX(t, err == nil && rs.URL == tc.want && rs.BaseRef == "" && !strings.Contains(rs.URL, "ghp_") && !strings.Contains(rs.URL, "tok@"), "resolveAXRepoSpec(%q) = %+v, %v", tc.in, rs, err)
	}
	_, err := resolveAXRepoSpec(ctx, serveJobRepo(t), "")
	checkAX(t, err != nil && strings.Contains(err.Error(), "direct AX jobs require a git remote URL"), "expected missing origin error, got %v", err)

	repo := serveJobRepo(t)
	runGit(t, repo, "remote", "add", "origin", "git@github.com:erain/glue.git")
	sha, _ := gitOutput(ctx, repo, "rev-parse", "HEAD")
	runGit(t, repo, "checkout", "-qb", "feature/unpushed")
	rs := must(resolveAXRepoSpec(ctx, repo, ""))
	checkAX(t, rs.URL == "https://github.com/erain/glue.git" && rs.BaseRef == "", "unpushed = %+v", rs)
	runGit(t, repo, "update-ref", "refs/remotes/origin/feature/unpushed", sha)
	checkAX(t, must(resolveAXRepoSpec(ctx, repo, "")).BaseRef == "feature/unpushed", "expected feature/unpushed")
	runGit(t, repo, "update-ref", "refs/remotes/origin/trunk", sha)
	runGit(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/trunk")
	checkAX(t, must(resolveAXRepoSpec(ctx, repo, "")).BaseRef == "feature/unpushed", "current branch on origin should win over origin/HEAD")
	runGit(t, repo, "checkout", "-qb", "local-only")
	checkAX(t, must(resolveAXRepoSpec(ctx, repo, "")).BaseRef == "trunk", "expected trunk")
	checkAX(t, must(resolveAXRepoSpec(ctx, repo, "release-1")).BaseRef == "release-1", "expected release-1")
}

func TestJobTargetSelectionAndCredentials(t *testing.T) {
	clearJobTestEnv(t)
	origKube, origGH, origNewAX := resolveKubeTarget, resolveGitHubCLIToken, newAXJobAPI
	t.Cleanup(func() { resolveKubeTarget, resolveGitHubCLIToken, newAXJobAPI = origKube, origGH, origNewAX })
	resolveKubeTarget = func(string, string) (string, string, error) { return "", "", errors.New("no kube context") }
	resolveGitHubCLIToken = func(context.Context) string { return "gho_from_gh_cli" }
	var gotAXOpts axbackend.Options
	newAXJobAPI = func(_ context.Context, opts axbackend.Options) (jobAPI, error) {
		gotAXOpts = opts
		return httpJobAPI{&jobclient.Client{}}, nil
	}
	missingMeta, metaPath := filepath.Join(t.TempDir(), "missing.json"), filepath.Join(t.TempDir(), "daemon.json")
	must(0, daemon.WriteMetadata(metaPath, daemon.Metadata{Version: 1, BaseURL: "http://127.0.0.1:43210", Token: "meta-tok", PID: 1}))
	_, _, err := resolveJobAPI(context.Background(), jobTargetOpts{conn: jobclient.Config{MetadataPath: missingMeta}}, nil)
	checkAX(t, err != nil, "expected error when neither HTTP nor kube target exists")
	resolveKubeTarget = func(string, string) (string, string, error) { return "/tmp/kc", "kind-glue-ax", nil }
	for _, tc := range []struct {
		name, env string
		opts      jobTargetOpts
		want      bool
	}{
		{"kube fallback", "", jobTargetOpts{conn: jobclient.Config{MetadataPath: missingMeta}}, true},
		{"daemon.json default", "", jobTargetOpts{conn: jobclient.Config{MetadataPath: metaPath}}, false},
		{"--remote beats daemon.json", "", jobTargetOpts{conn: jobclient.Config{MetadataPath: metaPath}, remote: true}, true},
		{"--remote + --server uses HTTP", "", jobTargetOpts{conn: jobclient.Config{ServerURL: "http://127.0.0.1:43210", Token: "t"}, remote: true}, false},
		{"--remote + --backend local uses HTTP", "", jobTargetOpts{conn: jobclient.Config{MetadataPath: metaPath}, remote: true, backend: "local"}, false},
		{"GLUE_AX_CONTEXT beats daemon.json", "GLUE_AX_CONTEXT", jobTargetOpts{conn: jobclient.Config{MetadataPath: metaPath}}, true},
		{"explicit --metadata uses HTTP", "GLUE_AX_CONTEXT", jobTargetOpts{conn: jobclient.Config{MetadataPath: metaPath}, metadataExplicit: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv(tc.env, "kind-glue-ax")
			}
			_, direct, err := resolveJobAPI(context.Background(), tc.opts, nil)
			checkAX(t, err == nil && direct == tc.want, "direct=%v err=%v want=%v", direct, err, tc.want)
		})
	}
	checkAX(t, gotAXOpts.Credentials.GitHubToken == "gho_from_gh_cli", "expected gh auth token fallback")
}

func TestDirectAXCLIEndToEnd(t *testing.T) {
	clearJobTestEnv(t)
	origKube, origGH := resolveKubeTarget, resolveGitHubCLIToken
	t.Cleanup(func() { resolveKubeTarget, resolveGitHubCLIToken = origKube, origGH })
	resolveKubeTarget = func(string, string) (string, string, error) { return "/tmp/kc", "kind-glue-ax", nil }
	resolveGitHubCLIToken = func(context.Context) string { return "" }
	envPath := filepath.Join(t.TempDir(), "ax.env")
	must(0, os.WriteFile(envPath, []byte("GEMINI_API_KEY=AIzaFromEnvFile123456\nOPENROUTER_API_KEY=sk-or-from-env-file\nGITHUB_TOKEN=ghp_fromEnvFile123456\n"), 0o600))

	var firstProvider *scriptedProvider
	rig := newFakeAXRig(t, func() glue.Provider {
		if firstProvider == nil {
			firstProvider = &scriptedProvider{turns: [][]glue.ProviderEvent{
				textTurn(`{"items":[{"title":"add Makefile"}]}`),
				toolCallTurn("call-1", "write_file", `{"path":"Makefile","content":"test:\n\t@true\n"}`),
				toolCallTurn("call-2", "write_file", `{"path":"extra.txt","content":"skip\n"}`),
				textTurn("wrote Makefile"),
				textTurn(`{"done":false,"items":[{"title":"add Makefile","done":false}],"summary":"waiting for confirmation"}`),
				textTurn("confirmed Makefile"),
				textTurn(`{"done":true,"items":[{"title":"add Makefile","done":true,"evidence":"Makefile present"}],"summary":"Add Makefile test target"}`),
			}}
			return firstProvider
		}
		return &scriptedProvider{turns: [][]glue.ProviderEvent{
			textTurn(`{"items":[{"title":"check"}]}`),
			textTurn("done"),
			textTurn(`{"done":true,"items":[{"title":"check","done":true,"evidence":"ok"}],"summary":"Quick check done"}`),
		}}
	})

	localCheckout := serveJobRepo(t)
	runGit(t, localCheckout, "remote", "add", "origin", "git@github.com:erain/glue-sandbox.git")
	runGit(t, localCheckout, "update-ref", "refs/remotes/origin/main", "HEAD")
	runTop := func(args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := runCLIWithDeps(context.Background(), args, strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), serveDaemon, nil)
		checkAX(t, code == 0, "glue %v failed (%d): %s", args, code, stderr.String())
		return stdout.String()
	}
	wd := must(os.Getwd())
	t.Cleanup(func() { _ = os.Chdir(wd) })
	must(0, os.Chdir(localCheckout))

	jobID := strings.TrimSpace(runTop("--remote", "add a Makefile with a test target", "--policy", "ask-all", "--verify", "git status --porcelain", "--max-iterations", "1", "--env", envPath, "--kubeconfig", "/tmp/kc", "--context", "kind-glue-ax", "--atespace", "default", "--image", "localhost:5001/glue-task:custom"))
	rig.mu.Lock()
	mgr, opts, taskCfgStr := rig.managers[jobID], rig.lastOpts, fmt.Sprint(rig.tasks[axbackend.TaskName(jobID)].GetSpec().GetConfig().AsMap())
	rig.mu.Unlock()
	checkAX(t, opts.Connect.KubeConfig == "/tmp/kc" && opts.Connect.KubeContext == "kind-glue-ax" && opts.Image == "localhost:5001/glue-task:custom" && opts.Credentials.GitHubToken == "ghp_fromEnvFile123456" && opts.Credentials.ProviderKeys["gemini"] == "AIzaFromEnvFile123456" && opts.Credentials.ProviderKeys["openrouter"] == "sk-or-from-env-file", "unexpected AX options: kc=%q kctx=%q img=%q", opts.Connect.KubeConfig, opts.Connect.KubeContext, opts.Image)
	checkAX(t, !strings.Contains(taskCfgStr, "ghp_") && !strings.Contains(taskCfgStr, "AIza") && strings.Contains(taskCfgStr, "https://github.com/erain/glue-sandbox.git"), "unexpected task config: %s", taskCfgStr)
	checkAX(t, strings.Contains(taskCfgStr, "delivery:branch"), "AX jobs must default to branch delivery: %s", taskCfgStr)
	checkAX(t, strings.Contains(taskCfgStr, "wall_clock:3.6e+12") && strings.Contains(taskCfgStr, "token_budget:2e+06"), "default budgets missing from task config: %s", taskCfgStr)
	checkAX(t, strings.Contains(mustJobCLI(t, "show", jobID, "--ax"), "wall_clock: 1h0m0s"), "show must print the effective wall clock")

	perm1 := waitJob(t, mgr, jobID, jobs.WaitingForInput)
	showOut := mustJobCLI(t, "show", jobID, "--ax")
	checkAX(t, strings.Contains(mustJobCLI(t, "list", "--ax"), jobID) && strings.Contains(showOut, "status: waiting_for_input") && strings.Contains(showOut, perm1), "list/show --ax = %q", showOut)
	mustJobCLI(t, "say", jobID, "keep Makefile minimal", "--ax")
	mustJobCLI(t, "approve", jobID, perm1, "--remember", "session", "--ax")
	mustJobCLI(t, "deny", jobID, waitJob(t, mgr, jobID, jobs.WaitingForInput), "--reason", "not needed", "--ax")
	waitJob(t, mgr, jobID, jobs.MaxIterations)
	out1 := mustJobCLI(t, "attach", jobID, "--ax")
	checkAX(t, strings.Contains(out1, "operator: keep Makefile minimal") && strings.Contains(out1, "job max_iterations"), "attachOut1=%q", out1)
	mustJobCLI(t, "continue", jobID, "proceed", "--max-iterations", "1", "--ax")
	out2 := mustJobCLI(t, "attach", jobID, "--ax")
	checkAX(t, strings.Contains(out2, "resumed: iteration 2 (continue)") && strings.Contains(out2, "job achieved — Add Makefile test target") && !strings.Contains(out2, "wrote Makefile") && strings.Contains(fmt.Sprint(firstProvider.requests[len(firstProvider.requests)-2].Messages), "keep Makefile minimal"), "attachOut2=%q", out2)

	var res jobs.Result
	checkAX(t, json.Unmarshal([]byte(mustJobCLI(t, "result", jobID, "--json", "--ax")), &res) == nil && res.Status == jobs.Achieved && len(res.Verify) == 1 && res.Verify[0].ExitCode == 0, "result --ax = %+v", res)
	mustJobCLI(t, "delete", jobID, "--ax")
	checkAX(t, !strings.Contains(mustJobCLI(t, "list", "--ax"), jobID), "expected deleted job removed from list --ax")

	checkAX(t, strings.Contains(runTop("--remote", "Quick check", "--attach"), "job achieved — Quick check done"), "glue --remote --attach failed")
	eqID := strings.TrimSpace(runTop("--remote=Fix flaky retry test", "--repo", "https://user:secret@github.com/erain/glue-sandbox.git", "--start=false"))
	objID := strings.TrimSpace(runTop("--remote", "--objective", "Fix flaky retry test", "--deliver", "pr", "--verify", "go test ./...", "--start=false"))
	posID := strings.TrimSpace(mustJobCLI(t, "submit", "Positional AX objective", "--start=false"))
	rig.mu.Lock()
	eqCfg, objCfg, posCfg := fmt.Sprint(rig.tasks[axbackend.TaskName(eqID)].GetSpec().GetConfig().AsMap()), rig.tasks[axbackend.TaskName(objID)].GetSpec().GetConfig().AsMap(), rig.tasks[axbackend.TaskName(posID)].GetSpec().GetConfig().AsMap()
	rig.mu.Unlock()
	checkAX(t, !strings.Contains(eqCfg, "secret") && objCfg["delivery"] == "draft_pr" && posCfg["objective"] == "Positional AX objective", "eqCfg=%s objCfg=%+v posCfg=%+v", eqCfg, objCfg, posCfg)
	mustJobCLI(t, "cancel", objID, "--ax")
	for _, id := range []string{eqID, objID, posID} {
		mustJobCLI(t, "delete", id, "--ax")
	}
}

func axLiveStrict() bool { return os.Getenv("CI") == "true" || os.Getenv("GLUE_E2E_AX_STRICT") == "1" }

func checkLiveDirectAX(kubeCfg, kubeCtx string) error {
	if _, err := exec.LookPath("kubectl"); err != nil {
		return fmt.Errorf("kubectl binary not found (%w); run 'scripts/ax-dev/up.sh && source scripts/ax-dev/env.sh'", err)
	}
	cfg, kctx, err := axbackend.ResolveKubeTarget(kubeCfg, kubeCtx)
	if err != nil {
		return fmt.Errorf("%w; run 'scripts/ax-dev/up.sh && source scripts/ax-dev/env.sh'", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", cfg, "--context", kctx, "get", "ns", axbackend.DefaultAXNamespace, "-o", "name").CombinedOutput(); err != nil {
		return fmt.Errorf("AX namespace %q unreachable in %q (%v: %s); run 'scripts/ax-dev/up.sh && source scripts/ax-dev/env.sh'", axbackend.DefaultAXNamespace, kctx, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func TestLiveDirectAXJobPreconditionStrict(t *testing.T) {
	t.Setenv("CI", "true")
	err := checkLiveDirectAX(filepath.Join(t.TempDir(), "missing"), "kind-missing")
	checkAX(t, axLiveStrict() && err != nil && strings.Contains(err.Error(), "scripts/ax-dev/up.sh"), "expected actionable strict error, got %v", err)
}

func TestLiveDirectAXJob(t *testing.T) {
	if os.Getenv("GLUE_E2E_AX") != "1" {
		t.Skip("skipping live direct AX CLI test; set GLUE_E2E_AX=1 to run")
	}
	if err := checkLiveDirectAX("", ""); err != nil {
		if axLiveStrict() {
			t.Fatalf("live AX cluster required when CI=true or GLUE_E2E_AX_STRICT=1: %v", err)
		}
		t.Skipf("live AX cluster unavailable (%v)", err)
	}
	for _, k := range []string{"GLUE_SERVER", "GLUE_TOKEN", "GLUE_DAEMON_TOKEN", "AX_SERVER"} {
		t.Setenv(k, "")
	}
	if !envSet("GEMINI_API_KEY", "OPENROUTER_API_KEY") {
		if home, err := os.UserHomeDir(); err == nil {
			_ = loadEnvFiles([]string{filepath.Join(home, ".env")})
		}
	}
	t.Setenv("GLUE_CONFIG_DIR", t.TempDir())
	t.Cleanup(func() { _ = axbackend.StopTunnels("") })

	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	runLive := func(args ...string) (string, time.Duration) {
		t.Helper()
		t0 := time.Now()
		var stdout, stderr bytes.Buffer
		code := runCLIWithDeps(ctx, args, strings.NewReader(""), &stdout, &stderr, defaultProviderFactory, serveDaemon, nil)
		checkAX(t, code == 0, "glue %v failed (%d): %s", args, code, stderr.String())
		return stdout.String(), time.Since(t0)
	}
	beforeOut, _ := runLive("job", "list")
	t.Cleanup(func() {
		cCtx, cCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cCancel()
		var out bytes.Buffer
		_ = runCLIWithDeps(cCtx, []string{"job", "list", "--ax"}, strings.NewReader(""), &out, io.Discard, defaultProviderFactory, serveDaemon, nil)
		for line := range strings.SplitSeq(out.String(), "\n") {
			if id, _, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok && id != "" && !strings.Contains(beforeOut, id+"\t") {
				_ = runCLIWithDeps(cCtx, []string{"job", "delete", id, "--ax"}, strings.NewReader(""), io.Discard, io.Discard, defaultProviderFactory, serveDaemon, nil)
			}
		}
	})

	out, createDur := runLive("job", "submit", "--repo", "https://github.com/erain/glue-sandbox.git", "--objective", "Verify direct AX CLI control flow", "--deliver", "none", "--start=false")
	ctrlID := strings.TrimSpace(out)
	listOut, _ := runLive("job", "list")
	showOut, _ := runLive("job", "show", ctrlID)
	checkAX(t, strings.Contains(listOut, ctrlID) && strings.Contains(showOut, "Verify direct AX CLI control flow"), "list/show mismatch")
	runLive("job", "cancel", ctrlID)
	runLive("job", "delete", ctrlID)
	t.Logf("live control flow (%s): create=%s", ctrlID, createDur)
	if !envSet("GEMINI_API_KEY", "OPENROUTER_API_KEY") {
		return
	}
	remoteArgs := []string{"--remote", "Create hello_ax.txt containing the exact line direct-ax-ok.", "--repo", "https://github.com/erain/glue-sandbox.git", "--deliver", "none", "--verify", "grep -qx direct-ax-ok hello_ax.txt", "--max-iterations", "2", "--provider", "openrouter"}
	if envSet("GEMINI_API_KEY") {
		remoteArgs = append(remoteArgs[:len(remoteArgs)-2], "--provider", "gemini", "--model", "gemini-3-flash-preview", "--thinking", "medium")
	}
	subOut, subDur := runLive(remoteArgs...)
	jobID := strings.TrimSpace(subOut)
	checkAX(t, strings.HasPrefix(jobID, "job-"), "unexpected job ID %q", jobID)
	listOut2, listDur := runLive("job", "list")
	attachOut, attachDur := runLive("job", "attach", jobID)
	checkAX(t, strings.Contains(listOut2, jobID) && strings.Contains(attachOut, "job achieved"), "attach output:\n%s", attachOut)
	resJSON, resDur := runLive("job", "result", jobID, "--json")
	var res, res2 jobs.Result
	checkAX(t, json.Unmarshal([]byte(resJSON), &res) == nil && res.Status == jobs.Achieved && res.HeadSHA != "" && res.HeadSHA != res.BaseSHA && len(res.Verify) == 1 && res.Verify[0].ExitCode == 0, "unexpected result: %+v", res)

	t.Setenv("GLUE_CONFIG_DIR", t.TempDir())
	res2JSON, res2Dur := runLive("job", "result", jobID, "--json")
	checkAX(t, json.Unmarshal([]byte(res2JSON), &res2) == nil && res2.Status == jobs.Achieved && res2.HeadSHA == res.HeadSHA, "second-laptop result mismatch: %+v", res2)
	_, delDur := runLive("job", "delete", jobID)
	afterDel, _ := runLive("job", "list")
	checkAX(t, !strings.Contains(afterDel, jobID), "expected %s deleted from AX task list", jobID)
	t.Logf("live coding job (%s): submit=%s list=%s attach=%s result=%s laptop2_result=%s delete=%s head=%s", jobID, subDur, listDur, attachDur, resDur, res2Dur, delDur, res.HeadSHA)
}

// pushedCheckout returns a checkout on main whose HEAD is already on origin.
func pushedCheckout(t *testing.T) string {
	t.Helper()
	repo := serveJobRepo(t)
	runGit(t, repo, "remote", "add", "origin", "https://github.com/erain/glue-sandbox.git")
	runGit(t, repo, "update-ref", "refs/remotes/origin/main", "HEAD")
	runGit(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	return repo
}

func TestRemoteBaseDefaultsToCurrentBranch(t *testing.T) {
	clearJobTestEnv(t)
	ctx := context.Background()
	repo := pushedCheckout(t)
	runGit(t, repo, "checkout", "-qb", "feature/retry")
	runGit(t, repo, "commit", "--allow-empty", "-qm", "feature work")
	runGit(t, repo, "update-ref", "refs/remotes/origin/feature/retry", "HEAD")
	rs, err := resolveRemoteCheckout(ctx, repo, "", false)
	checkAX(t, err == nil && rs.BaseRef == "feature/retry" && rs.URL == "https://github.com/erain/glue-sandbox.git", "got %+v, %v", rs, err)
}

func TestRemoteRefusesDirtyTree(t *testing.T) {
	clearJobTestEnv(t)
	ctx := context.Background()
	repo := pushedCheckout(t)
	must(0, os.WriteFile(filepath.Join(repo, "notes.txt"), []byte("wip\n"), 0o600))
	must(0, os.MkdirAll(filepath.Join(repo, ".glue", "sessions"), 0o700))
	must(0, os.WriteFile(filepath.Join(repo, ".glue", "sessions", "s.json"), []byte("{}"), 0o600))
	_, err := resolveRemoteCheckout(ctx, repo, "", false)
	var lse *localStateError
	checkAX(t, errors.As(err, &lse) && lse.dirtyFiles == 1 && lse.unpushed == 0 && !lse.branchMissingOnRem, "got %v", err)
	checkAX(t, strings.Contains(err.Error(), "1 file(s) with uncommitted or untracked changes") && strings.Contains(err.Error(), "--ignore-local"), "message: %v", err)
	rs, err := resolveRemoteCheckout(ctx, repo, "main", false)
	checkAX(t, err == nil && rs.BaseRef == "main", "explicit --base should proceed: %+v, %v", rs, err)
}

func TestRemoteRefusesUnpushedCommits(t *testing.T) {
	clearJobTestEnv(t)
	ctx := context.Background()
	repo := pushedCheckout(t)
	runGit(t, repo, "checkout", "-qb", "my-feature")
	runGit(t, repo, "commit", "--allow-empty", "-qm", "local only")
	_, err := resolveRemoteCheckout(ctx, repo, "", false)
	var lse *localStateError
	checkAX(t, errors.As(err, &lse) && lse.unpushed == 1 && lse.branchMissingOnRem && lse.base == "main", "got %v", err)
	checkAX(t, strings.Contains(err.Error(), `branch "my-feature" does not exist on origin`) && strings.Contains(err.Error(), "1 commit(s) on HEAD are not on origin"), "message: %v", err)
}

func TestRemoteIgnoreLocalProceeds(t *testing.T) {
	clearJobTestEnv(t)
	ctx := context.Background()
	repo := pushedCheckout(t)
	runGit(t, repo, "checkout", "-qb", "my-feature")
	runGit(t, repo, "commit", "--allow-empty", "-qm", "local only")
	must(0, os.WriteFile(filepath.Join(repo, "notes.txt"), []byte("wip\n"), 0o600))
	rs, err := resolveRemoteCheckout(ctx, repo, "", true)
	checkAX(t, err == nil && rs.BaseRef == "main", "got %+v, %v", rs, err)
	rs, err = resolveSubmitRepo(ctx, true, true, false, "", repo, "", "objective")
	checkAX(t, err != nil && rs.URL == "", "submit without --ignore-local should refuse, got %+v, %v", rs, err)
	rs, err = resolveSubmitRepo(ctx, true, true, true, "", repo, "", "objective")
	checkAX(t, err == nil && rs.BaseRef == "main", "submit with --ignore-local: %+v, %v", rs, err)
}

func TestAXDefaultBudgets(t *testing.T) {
	got := remoteBudgets(jobs.Budgets{MaxIterations: 3}, false)
	checkAX(t, got == jobs.Budgets{MaxIterations: 3, WallClock: time.Hour, TokenBudget: 2_000_000}, "defaults = %+v", got)
	got = remoteBudgets(jobs.Budgets{WallClock: 5 * time.Minute, TokenBudget: 10}, false)
	checkAX(t, got == jobs.Budgets{WallClock: 5 * time.Minute, TokenBudget: 10}, "explicit budgets must win: %+v", got)
}

func TestNoBudgetFlag(t *testing.T) {
	got := remoteBudgets(jobs.Budgets{MaxIterations: 2}, true)
	checkAX(t, got == jobs.Budgets{MaxIterations: 2}, "--no-budget = %+v", got)
	checkAX(t, budgetText("0s", true) == "unlimited" && budgetText("1h0m0s", false) == "1h0m0s", "budgetText")
}

type progressFakeAPI struct {
	jobAPI
	opts axbackend.Options
}

func (f *progressFakeAPI) Close() error { return nil }
func (f *progressFakeAPI) Submit(_ context.Context, spec jobs.Spec, _ bool) (jobs.Job, error) {
	for _, s := range []string{"workspace", "task", "egress", "sandbox ready", "credentials", "started"} {
		if f.opts.Progress != nil {
			f.opts.Progress(s)
		}
	}
	return jobs.Job{ID: "job-0123456789ab", Spec: spec, Status: jobs.Running}, nil
}

func runFakeSubmit(t *testing.T, extra ...string) (string, string) {
	t.Helper()
	clearJobTestEnv(t)
	orig := newAXJobAPI
	t.Cleanup(func() { newAXJobAPI = orig })
	newAXJobAPI = func(_ context.Context, opts axbackend.Options) (jobAPI, error) {
		return &progressFakeAPI{opts: opts}, nil
	}
	var stdout, stderr bytes.Buffer
	args := append([]string{"submit", "--ax", "--repo", "https://github.com/erain/glue-sandbox", "--base", "main", "--verify", "go test ./...", "--deliver", "pr", "Add Shout"}, extra...)
	if err := jobCommand(context.Background(), args, strings.NewReader(""), &stdout, &stderr, nil); err != nil {
		t.Fatalf("submit: %v", err)
	}
	return stdout.String(), stderr.String()
}

func TestSubmitProgress(t *testing.T) {
	_, stderr := runFakeSubmit(t)
	for _, want := range []string{"  workspace", "  sandbox ready", "  started", "job job-0123456789ab started on AX", "repo      https://github.com/erain/glue-sandbox @ main", "model     gemini/", "policy    auto · delivery draft_pr", "verify    go test ./...", "glue job attach job-0123456789ab", "glue job cancel job-0123456789ab"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "\x1b[") {
		t.Fatalf("stderr contains terminal control codes: %q", stderr)
	}
}

func TestSubmitStdoutIsOnlyJobID(t *testing.T) {
	stdout, _ := runFakeSubmit(t)
	if stdout != "job-0123456789ab\n" {
		t.Fatalf("stdout = %q, want only the job ID", stdout)
	}
}

func TestSubmitQuiet(t *testing.T) {
	stdout, stderr := runFakeSubmit(t, "--quiet")
	if stdout != "job-0123456789ab\n" || stderr != "" {
		t.Fatalf("--quiet: stdout=%q stderr=%q", stdout, stderr)
	}
}

type notifyFakeAPI struct{ jobAPI }

func (notifyFakeAPI) Close() error { return nil }
func (notifyFakeAPI) Submit(_ context.Context, spec jobs.Spec, _ bool) (jobs.Job, error) {
	return jobs.Job{ID: "job-notify000001", Spec: spec}, nil
}

func TestNotifyHostInEgress(t *testing.T) {
	clearJobTestEnv(t)
	orig := newAXJobAPI
	t.Cleanup(func() { newAXJobAPI = orig })
	var got axbackend.Options
	newAXJobAPI = func(_ context.Context, opts axbackend.Options) (jobAPI, error) {
		got = opts
		return notifyFakeAPI{}, nil
	}
	var out bytes.Buffer
	err := jobCommand(context.Background(), []string{"submit", "--ax", "--repo", "https://github.com/erain/glue-sandbox", "--notify", "https://ntfy.sh/glue-secret-topic", "Add Shout"}, strings.NewReader(""), &out, &out, nil)
	checkAX(t, err == nil, "submit: %v", err)
	checkAX(t, slices.Contains(got.EgressHosts, "ntfy.sh") && slices.Contains(got.EgressHosts, "github.com"), "egress = %v", got.EgressHosts)
	checkAX(t, len(got.Credentials.NotifyURLs) == 1 && got.Credentials.NotifyURLs[0] == "https://ntfy.sh/glue-secret-topic", "notify creds = %v", got.Credentials.NotifyURLs)

	t.Setenv("GLUE_NOTIFY_URL", "https://hooks.example.com/abc")
	t.Setenv("GLUE_PUBLIC_URL", "https://glue.example.com/")
	err = jobCommand(context.Background(), []string{"submit", "--ax", "--repo", "https://github.com/erain/glue-sandbox", "Add Shout"}, strings.NewReader(""), &out, &out, nil)
	checkAX(t, err == nil && slices.Contains(got.EgressHosts, "hooks.example.com") && got.Credentials.PublicURL == "https://glue.example.com", "env default: %v %v %q", err, got.EgressHosts, got.Credentials.PublicURL)

	err = jobCommand(context.Background(), []string{"submit", "--ax", "--repo", "https://github.com/erain/glue-sandbox", "--notify", "ntfy.sh/x", "Add Shout"}, strings.NewReader(""), &out, &out, nil)
	checkAX(t, err != nil && strings.Contains(err.Error(), "invalid notify URL"), "bad URL: %v", err)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestUserConfigNotifySurvivesRestart(t *testing.T) {
	clearJobTestEnv(t)
	for k, v := range map[string]string{"GLUE_GITHUB_TOKEN": "ghp_fakeNotifyTestToken", "GEMINI_API_KEY": "AIzaRestartNotifyKey123456", "XDG_CONFIG_HOME": t.TempDir()} {
		t.Setenv(k, v)
	}
	doneCh := make(chan struct{}, 2)
	sink := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if b, _ := io.ReadAll(r.Body); strings.Contains(string(b), `"status":"achieved"`) {
			doneCh <- struct{}{}
		}
	}))
	t.Cleanup(sink.Close)
	must(saveUserConfig(&userConfig{Notify: []string{sink.URL + "/glue-0123456789abcdef0123456789abcdef"}}))
	calls := 0
	rig := newFakeAXRig(t, func() glue.Provider {
		calls++
		return &scriptedProvider{turns: map[bool][][]glue.ProviderEvent{true: {textTurn(`{"items":[{"title":"n"}]}`), textTurn("wip"), textTurn(`{"done":false,"items":[{"title":"n","done":false}],"summary":"wip"}`)}, false: {textTurn("ok"), textTurn(`{"done":true,"items":[{"title":"n","done":true,"evidence":"ok"}],"summary":"ok"}`)}}[calls == 1]}
	})
	jobID := strings.TrimSpace(mustJobCLI(t, "submit", "--ax", "--repo", "https://github.com/erain/glue-sandbox.git", "--max-iterations", "1", "Add note"))
	waitJob(t, func() *jobs.Manager { defer rig.lock()(); return rig.managers[jobID] }(), jobID, jobs.MaxIterations)
	checkAX(t, must(rig.restarts[jobID]().Inspect(context.Background(), jobID)).Credentials == "absent", "expected absent credentials")
	mustJobCLI(t, "continue", jobID, "finish", "--max-iterations", "1", "--ax")
	attachOut := mustJobCLI(t, "attach", jobID, "--ax")
	checkAX(t, strings.Contains(attachOut, "job achieved") && !strings.Contains(attachOut, "0123456789abcdef0123456789abcdef"), "attachOut=%q", attachOut)
	select {
	case <-doneCh:
	case <-time.After(3 * time.Second):
		t.Fatal("expected achieved notification after task restart")
	}
}

func TestJobInitGeneratesTopicAndPings(t *testing.T) {
	clearJobTestEnv(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	must(saveUserConfig(&userConfig{PublicURL: "https://glue.example.com"}))
	var pingURL, pingBody string
	doer := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		pingURL, pingBody = r.URL.String(), string(must(io.ReadAll(r.Body)))
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})
	var out1, out2, out3 bytes.Buffer
	checkAX(t, jobCommand(context.Background(), []string{"init", "--non-interactive"}, strings.NewReader(""), &out1, &out1, doer) == nil && strings.HasPrefix(pingURL, "https://ntfy.sh/glue-") && len(strings.TrimPrefix(pingURL, "https://ntfy.sh/glue-")) == 32 && pingBody == "Glue phone notifications connected" && strings.Contains(out1.String(), pingURL) && strings.Contains(out1.String(), "█"), "init 1: %s", out1.String())
	checkAX(t, jobCommand(context.Background(), []string{"init", "--non-interactive"}, strings.NewReader(""), &out2, &out2, doer) == nil && !strings.Contains(out2.String(), pingURL) && strings.Contains(out2.String(), redactNotify(pingURL)), "out2=%s", out2.String())
	cfg, _, err := loadUserConfig()
	over := "https://ntfy.sh/glue-override0123456789abcdef0123"
	checkAX(t, err == nil && cfg.PublicURL == "https://glue.example.com" && slices.Equal(cfg.Notify, []string{pingURL}) && jobCommand(context.Background(), []string{"init", "--notify", over, "--non-interactive"}, strings.NewReader(""), &out3, &out3, doer) == nil && !strings.Contains(out3.String(), over), "cfg=%+v err=%v", cfg, err)
}

func TestE2EAXUserConfigNotifyWithoutFlag(t *testing.T) {
	if os.Getenv("GLUE_E2E_AX") != "1" {
		t.Skip("set GLUE_E2E_AX=1 to run")
	}
	for _, k := range []string{"GLUE_SERVER", "GLUE_TOKEN", "GLUE_DAEMON_TOKEN", "AX_SERVER", "GLUE_NOTIFY_URL", "GLUE_CONFIG_DIR"} {
		t.Setenv(k, "")
	}
	_ = loadEnvFiles([]string{filepath.Join(os.Getenv("HOME"), ".env")})
	if err := checkLiveDirectAX("", ""); err != nil || !envSet("GEMINI_API_KEY", "OPENROUTER_API_KEY") {
		checkAX(t, !axLiveStrict(), "live AX cluster and provider key required (cluster=%v)", err)
		t.Skipf("live AX preconditions unavailable (cluster=%v)", err)
	}
	ghTok, _ := githubToken(t.Context())
	t.Setenv("GLUE_GITHUB_TOKEN", ghTok)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Cleanup(func() { _ = axbackend.StopTunnels("") })
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	run := func(args ...string) string {
		var out, errBuf bytes.Buffer
		checkAX(t, runCLIWithDeps(ctx, args, strings.NewReader(""), &out, &errBuf, defaultProviderFactory, serveDaemon, nil) == 0, "glue %v: %s", args, errBuf.String())
		return out.String()
	}
	run("job", "init", "--non-interactive", "--repo", "https://github.com/erain/glue-sandbox.git")
	cfg := must(func() (*userConfig, error) { c, _, err := loadUserConfig(); return c, err }())
	cfg.Deliver, cfg.Provider, cfg.Model = "none", map[bool]string{true: "gemini", false: "openrouter"}[envSet("GEMINI_API_KEY")], map[bool]string{true: "gemini-3-flash-preview"}[envSet("GEMINI_API_KEY")]
	must(saveUserConfig(cfg))
	jobID := strings.TrimSpace(run("--remote", "Create notify_cfg.txt containing exact line ok.", "--repo", "https://github.com/erain/glue-sandbox.git", "--verify", "grep -qx ok notify_cfg.txt", "--max-iterations", "2"))
	t.Cleanup(func() {
		_ = runCLIWithDeps(context.Background(), []string{"job", "delete", jobID, "--ax"}, strings.NewReader(""), io.Discard, io.Discard, defaultProviderFactory, serveDaemon, nil)
	})
	att := run("job", "attach", jobID)
	checkAX(t, strings.Contains(att, "job achieved"), "attach %s:\n%s", jobID, att)
	for range 15 {
		if resp, err := http.Get(cfg.Notify[0] + "/json?poll=1&since=10m"); err == nil && strings.Contains(func() string { defer resp.Body.Close(); b, _ := io.ReadAll(resp.Body); return string(b) }(), jobID) {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("did not receive ntfy done message for %s", jobID)
}

func TestE2EAXNtfyTwoWayQuestionReply(t *testing.T) {
	if os.Getenv("GLUE_E2E_AX") != "1" {
		t.Skip("set GLUE_E2E_AX=1 to run")
	}
	for _, k := range []string{"GLUE_SERVER", "GLUE_TOKEN", "GLUE_DAEMON_TOKEN", "AX_SERVER", "GLUE_NOTIFY_URL", "GLUE_PUBLIC_URL", "GLUE_CONFIG_DIR"} {
		t.Setenv(k, "")
	}
	_ = loadEnvFiles([]string{filepath.Join(os.Getenv("HOME"), ".env")})
	if err := checkLiveDirectAX("", ""); err != nil || !envSet("GEMINI_API_KEY", "OPENROUTER_API_KEY") {
		checkAX(t, !axLiveStrict(), "live AX cluster and provider key required (cluster=%v)", err)
		t.Skipf("live AX preconditions unavailable (cluster=%v)", err)
	}
	ghTok, _ := githubToken(t.Context())
	t.Setenv("GLUE_GITHUB_TOKEN", ghTok)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Cleanup(func() { _ = axbackend.StopTunnels("") })
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	t.Cleanup(cancel)
	run := func(args ...string) string {
		var out, errBuf bytes.Buffer
		checkAX(t, runCLIWithDeps(ctx, args, strings.NewReader(""), &out, &errBuf, defaultProviderFactory, serveDaemon, nil) == 0, "glue %v: %s", args, errBuf.String())
		return out.String()
	}
	var rnd [16]byte
	_, _ = rand.Read(rnd[:])
	topicURL, hasGem := fmt.Sprintf("https://ntfy.sh/glue-e2e-%x", rnd), envSet("GEMINI_API_KEY")
	must(saveUserConfig(&userConfig{PublicURL: "https://glue.example.com", Notify: []string{topicURL}, Deliver: "none", Provider: map[bool]string{true: "gemini", false: "openrouter"}[hasGem], Model: map[bool]string{true: "gemini-3-flash-preview"}[hasGem]}))
	sresp := must(http.DefaultClient.Do(must(http.NewRequestWithContext(ctx, http.MethodGet, topicURL+"/json", nil))))
	t.Cleanup(func() { _ = sresp.Body.Close() })
	sc := bufio.NewScanner(sresp.Body)
	checkAX(t, sc.Scan() && strings.Contains(sc.Text(), `"open"`), "ntfy open: %v", sc.Err())
	prompt := "Call ask_user once right now with question 'Which label should go in choice.txt?' and options ['Alpha', 'Beta'], then create choice.txt containing only the chosen word followed by a newline."
	jobID := strings.TrimSpace(run("--remote", prompt, "--repo", "https://github.com/erain/glue-sandbox.git", "--image", cmp.Or(os.Getenv("GLUE_AX_E2E_IMAGE"), axbackend.DefaultImage()), "--verify", "grep -qx Beta choice.txt", "--max-iterations", "3"))
	t.Cleanup(func() {
		_ = runCLIWithDeps(context.Background(), []string{"job", "delete", jobID, "--ax"}, strings.NewReader(""), io.Discard, io.Discard, defaultProviderFactory, serveDaemon, nil)
	})
	wantClick, tapped, confirmed := "https://glue.example.com/#/jobs/"+jobID, false, false
	for sc.Scan() {
		var ev struct {
			Event, Title, Message, Click string
			Actions                      []struct{ Action, Label, URL, Method, Body string }
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Event != "message" || !strings.Contains(ev.Message, jobID) {
			continue
		}
		checkAX(t, ev.Click == wantClick, "click=%q, want %q", ev.Click, wantClick)
		for _, a := range ev.Actions {
			if !tapped && a.Action == "http" && a.Label == "Beta" && a.Method == "POST" && strings.HasPrefix(a.URL, "https://ntfy.sh/glue-reply-") {
				presp := must(http.Post(a.URL, "application/json", strings.NewReader(a.Body)))
				checkAX(t, presp.Body.Close() == nil && presp.StatusCode < 300, "reply POST status=%d", presp.StatusCode)
				tapped = true
			}
		}
		confirmed = confirmed || strings.Contains(ev.Message, "✓ Glue job "+jobID+": answered")
		if ev.Title == "Glue job achieved" {
			logs := run("job", "logs", jobID, "--json", "--ax")
			checkAX(t, tapped && confirmed && strings.Contains(logs, `"source":"ntfy"`) && !strings.Contains(logs, "glue-reply-"), "tapped=%v confirmed=%v", tapped, confirmed)
			return
		}
	}
	t.Fatalf("ntfy stream ended before %s achieved (tapped=%v confirmed=%v err=%v)", jobID, tapped, confirmed, sc.Err())
}

func okGW(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"ok":true}`) }

func stubRemoteEnv(t *testing.T, fn func([]string, []byte) ([]byte, error), h http.HandlerFunc) func(...string) (string, error) {
	t.Helper()
	clearJobTestEnv(t)
	t.Setenv("GLUE_CONFIG_DIR", filepath.Join(t.TempDir(), "glue"))
	ts := httptest.NewServer(h)
	origExec, origFwd, origKube, origGH := remoteExec, remoteForward, resolveKubeTarget, resolveGitHubCLIToken
	t.Cleanup(func() {
		ts.Close()
		remoteExec, remoteForward, resolveKubeTarget, resolveGitHubCLIToken = origExec, origFwd, origKube, origGH
	})
	resolveKubeTarget = func(string, string) (string, string, error) { return "/tmp/kc", "kind-test", nil }
	resolveGitHubCLIToken = func(context.Context) string { return "" }
	remoteForward = func(context.Context, string, string, string, string, int, string) (string, error) {
		return strings.TrimPrefix(ts.URL, "http://"), nil
	}
	remoteExec = func(_ context.Context, _ string, in []byte, name string, args ...string) ([]byte, error) {
		return fn(append([]string{name}, args...), in)
	}
	return func(args ...string) (string, error) {
		var b bytes.Buffer
		err := remoteCommand(t.Context(), args, &b, &b, ts.Client())
		return b.String(), err
	}
}

func TestRemoteSetupManifestsAndConfigValidate(t *testing.T) {
	calls := 0
	run := stubRemoteEnv(t, func([]string, []byte) ([]byte, error) { calls++; return nil, nil }, okGW)
	_, badYAML := validateGatewayYAML([]byte("listen: 0.0.0.0:8800\nunknown_field: 1\n"), true)
	checkAX(t, badYAML != nil && strings.Contains(badYAML.Error(), "unknown_field"), "KnownFields: %v", badYAML)
	for _, bad := range [][]string{{"setup"}, {"setup", "--cf-team", "t.cloudflareaccess.com", "--cf-aud", "a"}, {"setup", "--public-url", "https://g.example", "--cf-team", "t.cloudflareaccess.com", "--cf-aud", "a", "--dev-token"}, {"setup", "--public-url", "https://g.example", "--cf-team", "http://evil.example", "--cf-aud", "a"}, {"setup", "--public-url", "https://g.example", "--cf-team", "t.cloudflareaccess.com", "--cf-aud", "a", "--allow-email", "@"}, {"setup", "--public-url", "https://g.example", "--dev-token"}} {
		_, err := run(bad...)
		checkAX(t, err != nil && calls == 0, "expected pre-apply failure for %v (calls=%d)", bad, calls)
	}
	dry := must(run("setup", "--dry-run", "--public-url", "https://glue.example.com/", "--cf-team", "myteam.cloudflareaccess.com", "--cf-aud", "aud-1", "--allow-email", "alice@example.com", "--cf-certs-url", "http://172.18.0.1:9000/certs", "--tunnel-token", "tun-xyz"))
	var kinds []string
	var parsedGW remoteGatewayYAML
	for dec, doc := yaml.NewDecoder(strings.NewReader(dry)), map[string]any{}; dec.Decode(&doc) == nil; doc = map[string]any{} {
		kinds = append(kinds, doc["kind"].(string))
		if doc["kind"] == "ConfigMap" {
			parsedGW = must(validateGatewayYAML([]byte(doc["data"].(map[string]any)["gateway.yaml"].(string)), false))
		}
	}
	_, _ = must(run("setup", "--public-url", "https://glue.example.com", "--cf-team", "myteam.cloudflareaccess.com", "--cf-aud", "aud-1")), must(run("setup", "--dev-token"))
	ucfg, _, uerr := loadUserConfig()
	checkAX(t, uerr == nil && ucfg.PublicURL == "https://glue.example.com" && slices.Equal(kinds, []string{"Namespace", "ServiceAccount", "ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding", "PersistentVolumeClaim", "ConfigMap", "Secret", "Deployment", "Service"}) && parsedGW.Auth.TeamDomain == "https://myteam.cloudflareaccess.com" && parsedGW.Auth.CertsURL == "http://172.18.0.1:9000/certs" && !strings.Contains(dry, "serviceaccounts/token") && strings.Contains(dry, "cloudflared"), "kinds=%v gw=%+v ucfg=%+v", kinds, parsedGW, ucfg)
}

func TestRemoteSetupRedactsSecrets(t *testing.T) {
	applied, failApply := "", false
	run := stubRemoteEnv(t, func(args []string, in []byte) ([]byte, error) {
		if slices.Contains(args, "apply") {
			applied = string(in)
			return []byte("apply leaked dev-sec-555 and tun-sec-666"), map[bool]error{true: os.ErrPermission}[failApply]
		}
		return nil, nil
	}, okGW)
	for k, v := range map[string]string{"GEMINI_API_KEY": "gem-sec-999", "OPENROUTER_API_KEY": "or-sec-888", "GLUE_GITHUB_TOKEN": "ghp-sec-777", "GLUE_NOTIFY_URL": "https://ntfy.sh/glue-0123456789abcdef0123456789abcdef"} {
		t.Setenv(k, v)
	}
	dry, live := must(run("setup", "--dry-run", "--tunnel-token=tun-sec-666", "--dev-token=dev-sec-555")), must(run("setup", "--tunnel-token=tun-sec-666", "--dev-token", "dev-sec-555"))
	failApply = true
	_, err := run("setup", "--tunnel-token=tun-sec-666", "--dev-token=dev-sec-555")
	checkAX(t, strings.Contains(dry, "[REDACTED]") && err != nil && strings.Contains(err.Error(), "[REDACTED]"), "expected redacted output/error, got %v", err)
	for _, s := range []string{"gem-sec-999", "or-sec-888", "ghp-sec-777", "glue-0123456789abcdef0123456789abcdef", "tun-sec-666", "dev-sec-555"} {
		checkAX(t, strings.Contains(applied, s) && !strings.Contains(dry+live+err.Error(), s), "secret %q leaked or missing", s)
	}
}

func TestRemoteStatusAndDown(t *testing.T) {
	mode, seenAuth, deleted := "cloudflare-access", "", []string(nil)
	run := stubRemoteEnv(t, func(args []string, _ []byte) ([]byte, error) {
		cmd := strings.Join(args, " ")
		if strings.Contains(cmd, "delete") {
			deleted = append(deleted, cmd)
		} else if strings.Contains(cmd, "get configmap") {
			return []byte("public_url: https://glue.example.com\nauth:\n  mode: " + mode + "\n"), nil
		}
		return []byte(map[bool]string{true: "1/1", false: base64.StdEncoding.EncodeToString([]byte("tok-secret-1"))}[strings.Contains(cmd, "get deployment")]), nil
	}, func(w http.ResponseWriter, r *http.Request) {
		seenAuth = map[bool]string{true: r.Header.Get("Authorization"), false: seenAuth}[r.URL.Path != "/healthz"]
		w.WriteHeader(map[bool]int{true: http.StatusUnauthorized, false: http.StatusOK}[r.URL.Path != "/healthz" && mode == "cloudflare-access" && seenAuth == ""])
		_, _ = io.WriteString(w, `{"ok":true,"default_backend":"ax"}`)
	})
	st1 := must(run("status"))
	mode = "dev-token"
	st2, _ := must(run("status")), must(run("down"))
	d1 := strings.Join(deleted, "\n")
	deleted = nil
	_ = must(run("down", "--purge-pvc"))
	d2 := strings.Join(deleted, "\n")
	checkAX(t, strings.Contains(st1, "ready: 1/1") && strings.Contains(st1, "access-protected") && seenAuth == "Bearer tok-secret-1" && !strings.Contains(st2, "tok-secret-1") && strings.Contains(st2, "backends: ok (ax)") && !strings.Contains(d1, "pvc/") && !strings.Contains(d1, "namespace/glue-gateway") && strings.Contains(d2, "pvc/glue-gateway-state") && strings.Contains(d2, "namespace/glue-gateway"), "st1=%q st2=%q d1=%q d2=%q", st1, st2, d1, d2)
}

func TestE2EClusterGatewaySetupAndCloudflareAccess(t *testing.T) {
	if os.Getenv("GLUE_E2E_AX") != "1" {
		t.Skip("set GLUE_E2E_AX=1 to run")
	}
	_, derr := exec.LookPath("docker")
	if err := errors.Join(derr, checkLiveDirectAX("", "")); err != nil {
		checkAX(t, !axLiveStrict(), "live AX cluster and docker required when CI=true or GLUE_E2E_AX_STRICT=1 (%v); run scripts/ax-dev/up.sh", err)
		t.Skipf("live AX cluster or docker unavailable: %v", err)
	}
	t.Setenv("GLUE_CONFIG_DIR", filepath.Join(t.TempDir(), "glue"))
	kc, kctx, kerr := axbackend.ResolveKubeTarget("", "")
	var bridgeIP string
	for f := range strings.FieldsSeq(string(must(exec.CommandContext(t.Context(), "docker", "network", "inspect", "kind", "-f", "{{range .IPAM.Config}}{{.Gateway}} {{end}}").Output()))) {
		bridgeIP = cmp.Or(bridgeIP, map[bool]string{true: f}[net.ParseIP(f).To4() != nil])
	}
	checkAX(t, kerr == nil && bridgeIP != "", "kube=%v bridge=%q", kerr, bridgeIP)
	rsaKey := must(rsa.GenerateKey(rand.Reader, 2048))
	jwks := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kid": "k1", "kty": "RSA", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(rsaKey.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(rsaKey.E)).Bytes())}}})
	}))
	jwks.Listener = must(net.Listen("tcp4", "0.0.0.0:0"))
	jwks.Start()
	t.Cleanup(jwks.Close)
	mintCF := func(email string) string {
		b64 := func(v any) string { raw, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(raw) }
		u := b64(map[string]string{"alg": "RS256", "kid": "k1"}) + "." + b64(map[string]any{"iss": "https://e2e.cloudflareaccess.com", "aud": "aud-e2e-636", "email": email, "exp": time.Now().Add(time.Hour).Unix()})
		s := sha256.Sum256([]byte(u))
		return u + "." + base64.RawURLEncoding.EncodeToString(must(rsa.SignPKCS1v15(rand.Reader, rsaKey, crypto.SHA256, s[:])))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	t.Cleanup(func() {
		_ = errors.Join(remoteCommand(context.Background(), []string{"down", "--purge-pvc"}, io.Discard, io.Discard, http.DefaultClient), axbackend.StopTunnels(""))
	})
	var setupOut, statusOut bytes.Buffer
	certsURL := "http://" + net.JoinHostPort(bridgeIP, strings.Split(jwks.Listener.Addr().String(), ":")[1]) + "/certs"
	checkAX(t, remoteCommand(ctx, []string{"setup", "--build-local", "--public-url", "https://glue.e2e.example", "--cf-team", "e2e.cloudflareaccess.com", "--cf-aud", "aud-e2e-636", "--allow-email", "alice@example.com", "--cf-certs-url", certsURL}, &setupOut, &setupOut, http.DefaultClient) == nil, "remote setup failed: %s", setupOut.String())
	axConn := must(axbackend.Connect(ctx, axbackend.ConnectOptions{KubeConfig: kc, KubeContext: kctx, Atespace: "default"}))
	defer axConn.Close()
	before := must(axConn.AX.ListTasks(ctx, &v1alpha1.ListTasksRequest{Atespace: "default"}))
	checkAX(t, !slices.ContainsFunc(before.GetTasks(), func(tk *v1alpha1.Task) bool {
		return tk.GetStatus().GetState() == v1alpha1.TaskState_TASK_STATE_WORKING
	}), "idle gateway consumed running AX task: %+v", before.GetTasks())
	baseURL := "http://" + must(axbackend.EnsureTunnel(ctx, kc, kctx, remoteGatewayNS, "glue-gateway", 8800, "/healthz"))
	doReq := func(method, path, jwt, cookie, csrf, body string) (int, map[string]any, []*http.Cookie) {
		req, _ := http.NewRequestWithContext(ctx, method, baseURL+path, strings.NewReader(body))
		req.Header = map[bool]http.Header{true: {"Cf-Access-Jwt-Assertion": {jwt}, "X-Glue-CSRF": {csrf}, "Cookie": {cookie}}, false: {}}[jwt != ""]
		resp := must(http.DefaultClient.Do(req))
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m, resp.Cookies()
	}
	st401, _, _ := doReq(http.MethodGet, "/v1/jobs", "", "", "", "")
	st403, _, _ := doReq(http.MethodGet, "/v1/me", mintCF("mallory@other.example"), "", "", "")
	jwt := mintCF("alice@example.com")
	stMe, meBody, cks := doReq(http.MethodGet, "/v1/me", jwt, "", "", "")
	checkAX(t, stMe == http.StatusOK && len(cks) > 0, "me=%d body=%v", stMe, meBody)
	sess, csrf := cks[0].Name+"="+cks[0].Value, meBody["csrf_token"].(string)
	stList, _, _ := doReq(http.MethodGet, "/v1/jobs", jwt, sess, "", "")
	stCreate, createdBody, _ := doReq(http.MethodPost, "/v1/jobs", jwt, sess, csrf, `{"objective":"E2E gateway job","repo":{"url":"https://github.com/erain/glue-sandbox","base_ref":"main"}}`)
	jobID, _ := createdBody["job"].(map[string]any)["id"].(string)
	after := must(axConn.AX.ListTasks(ctx, &v1alpha1.ListTasksRequest{Atespace: "default"}))
	stDel, _, _ := doReq(http.MethodDelete, "/v1/jobs/"+jobID+"?purge=true", jwt, sess, csrf, "")
	checkAX(t, st401 == http.StatusUnauthorized && st403 == http.StatusForbidden && meBody["login"] == "alice@example.com" && stList == http.StatusOK && stCreate == http.StatusCreated && strings.HasPrefix(jobID, "job-") && slices.ContainsFunc(after.GetTasks(), func(tk *v1alpha1.Task) bool { return tk.GetMetadata().GetName() == "glue-"+jobID }) && stDel == http.StatusOK, "http: 401=%d 403=%d list=%d create=%d(%q) del=%d", st401, st403, stList, stCreate, jobID, stDel)
	checkAX(t, remoteCommand(ctx, []string{"status"}, &statusOut, &statusOut, http.DefaultClient) == nil && strings.Contains(statusOut.String(), "ready: 1/1") && strings.Contains(statusOut.String(), "healthz: ok") && remoteCommand(ctx, []string{"down", "--purge-pvc"}, io.Discard, io.Discard, http.DefaultClient) == nil && exec.CommandContext(ctx, "kubectl", "--kubeconfig", kc, "--context", kctx, "get", "ns", remoteGatewayNS).Run() != nil, "status=%s", statusOut.String())
}

func startJobUI(t *testing.T, want string, args ...string) (string, func()) {
	ctx, cancel := context.WithCancel(t.Context())
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- jobCommand(ctx, append([]string{"ui", "--ax"}, args...), strings.NewReader(""), pw, io.Discard, http.DefaultClient)
	}()
	var out strings.Builder
	for buf := make([]byte, 4096); !strings.Contains(out.String(), want); {
		n, err := pr.Read(buf)
		out.Write(buf[:n])
		checkAX(t, err == nil, "read banner: %v", err)
	}
	go io.Copy(io.Discard, pr)
	return out.String(), func() { cancel(); checkAX(t, <-done == nil, "ui shutdown"); _ = pw.Close() }
}

func TestJobUIServesAXJobsWithoutWakeup(t *testing.T) {
	clearJobTestEnv(t)
	t.Setenv("GLUE_GITHUB_TOKEN", "ghp_fake")
	origOpen, opened := openBrowserURL, false
	t.Cleanup(func() { openBrowserURL = origOpen })
	openBrowserURL = func(string) error { opened = true; return nil }
	rig := newFakeAXRig(t, func() glue.Provider {
		return &scriptedProvider{turns: [][]glue.ProviderEvent{textTurn("Interactive ready."), textTurn("Woken and replied.")}}
	})
	activeID := strings.TrimSpace(mustJobCLI(t, "submit", "Active workspace", "--repo", "https://github.com/erain/glue-sandbox.git", "--interactive", "--ax"))
	suspID := strings.TrimSpace(mustJobCLI(t, "submit", "Suspended workspace", "--repo", "https://github.com/erain/glue-sandbox.git", "--interactive", "--ax"))
	resumes := func() int { defer rig.lock()(); return rig.resumes }
	for _, id := range []string{activeID, suspID} {
		waitJob(t, func() *jobs.Manager { defer rig.lock()(); return rig.managers[id] }(), id, jobs.WaitingForInput)
	}
	must(rig.SuspendTask(t.Context(), &v1alpha1.SuspendTaskRequest{Name: axbackend.TaskName(suspID)}))
	banner, stopUI := startJobUI(t, "\n")
	baseURL, tok, ok := strings.Cut(strings.TrimSpace(strings.Split(banner, "\n")[0]), "/#token=")
	qResp, loginResp := must(http.Get(baseURL+"/v1/jobs?token="+tok)), must(http.Post(baseURL+"/v1/session/login", "application/json", strings.NewReader(`{"token":"`+tok+`"}`)))
	var sess map[string]string
	_, _, _ = json.NewDecoder(loginResp.Body).Decode(&sess), qResp.Body.Close(), loginResp.Body.Close()
	cks, before := loginResp.Cookies(), resumes()
	checkAX(t, ok && len(tok) == 64 && !opened && qResp.StatusCode == http.StatusUnauthorized && loginResp.StatusCode == http.StatusOK && len(cks) == 1 && cks[0].HttpOnly && cks[0].SameSite == http.SameSiteStrictMode && sess["csrf_token"] != "", "login=%d q=%d", loginResp.StatusCode, qResp.StatusCode)
	doUI := func(method, path, body string) (int, string) {
		req, _ := http.NewRequestWithContext(t.Context(), method, baseURL+path, strings.NewReader(body))
		req.AddCookie(cks[0])
		req.Header.Set("X-Glue-CSRF", sess["csrf_token"])
		resp := must(http.DefaultClient.Do(req))
		defer resp.Body.Close()
		return resp.StatusCode, string(must(io.ReadAll(resp.Body)))
	}
	var bodies strings.Builder
	for p, want := range map[string]int{"/v1/jobs": 200, "/v1/jobs/" + suspID: 200, "/v1/jobs/" + suspID + "/permissions?state=pending": 200, "/v1/jobs/" + suspID + "/result": 409, "/v1/jobs/" + suspID + "/patch": 409, "/v1/jobs/" + suspID + "/events?cached=1": 200, "/v1/models": 200} {
		st, b := doUI(http.MethodGet, p, "")
		checkAX(t, st == want, "GET %s = %d, want %d", p, st, want)
		bodies.WriteString(b)
	}
	checkAX(t, strings.Contains(bodies.String(), activeID) && strings.Contains(bodies.String(), `"status":"suspended"`) && resumes() == before, "read-only woke task: %d->%d", before, resumes())
	stMsg, _ := doUI(http.MethodPost, "/v1/jobs/"+suspID+"/messages", `{"text":"wake up"}`)
	stopUI()
	checkAX(t, stMsg == http.StatusAccepted && resumes() == before+1, "steer=%d resumes=%d->%d", stMsg, before, resumes())
}

func TestJobUITunnelQRAndCleanup(t *testing.T) {
	clearJobTestEnv(t)
	_, binDir, pidFile := newFakeAXRig(t, func() glue.Provider { return &scriptedProvider{} }), t.TempDir(), filepath.Join(t.TempDir(), "child.pid")
	for k, v := range map[string]string{"GLUE_GITHUB_TOKEN": "ghp_fake", "PATH": t.TempDir()} {
		t.Setenv(k, v)
	}
	errMissing := jobCommand(t.Context(), []string{"ui", "--ax", "--tunnel"}, strings.NewReader(""), io.Discard, io.Discard, http.DefaultClient)
	must(0, os.WriteFile(filepath.Join(binDir, "cloudflared"), []byte("#!/bin/sh\nsleep 60 &\necho $! > "+pidFile+"\necho 'INF https://www.cloudflare.com/website-terms/'\necho 'INF | https://example.trycloudflare.com |'\nwait\n"), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/bin:/usr/bin")
	banner, stopUI := startJobUI(t, "█", "--tunnel")
	pid := must(strconv.Atoi(strings.TrimSpace(string(must(os.ReadFile(pidFile))))))
	stopUI()
	for i := 0; i < 50 && syscall.Kill(pid, 0) == nil; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	checkAX(t, errMissing != nil && strings.Contains(errMissing.Error(), "cloudflared not found") && !strings.Contains(strings.TrimSpace(errMissing.Error()), "\n") && strings.HasPrefix(banner, "https://example.trycloudflare.com/#token=") && syscall.Kill(pid, 0) != nil, "missing=%v banner=%s pid=%d", errMissing, banner, pid)
}
