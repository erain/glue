// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/daemon"
	"github.com/erain/glue/internal/axbackend"
	"github.com/erain/glue/internal/jobclient"
	"github.com/erain/glue/jobs"
	filestore "github.com/erain/glue/stores/file"
	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
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
	ateapipb.UnimplementedControlServer
	t            *testing.T
	repoDir      string
	makeProvider func() glue.Provider
	mu           sync.Mutex
	workspaces   map[string]*v1alpha1.Workspace
	tasks        map[string]*v1alpha1.Task
	egress       map[string]bool
	daemons      map[string]*daemon.Server
	managers     map[string]*jobs.Manager
	lastOpts     axbackend.Options
}

func newFakeAXRig(t *testing.T, makeProvider func() glue.Provider) *fakeAXRig {
	t.Helper()
	rig := &fakeAXRig{
		t: t, repoDir: serveJobRepo(t), makeProvider: makeProvider,
		workspaces: map[string]*v1alpha1.Workspace{}, tasks: map[string]*v1alpha1.Task{},
		egress: map[string]bool{}, daemons: map[string]*daemon.Server{}, managers: map[string]*jobs.Manager{},
	}
	lis, gs := bufconn.Listen(1<<20), grpc.NewServer()
	v1alpha1.RegisterAXServer(gs, rig)
	ateapipb.RegisterControlServer(gs, rig)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(func() { gs.Stop(); _ = lis.Close() })
	dialOpts := []grpc.DialOption{grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials())}
	axCli, axCC, err := axbackend.DialAX("passthrough:///bufnet", dialOpts...)
	checkAX(t, err == nil, "DialAX: %v", err)
	ctrlCC := must(grpc.NewClient("passthrough:///bufnet", dialOpts...))
	t.Cleanup(func() { _ = axCC.Close(); _ = ctrlCC.Close() })
	ctrlCli := ateapipb.NewControlClient(ctrlCC)

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
		opts.Connect = axbackend.ConnectOptions{Atespace: opts.Connect.Atespace, AXClient: axCli, ControlClient: ctrlCli, RouterDialContext: dialRouter, RouterTLS: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}}
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
func (r *fakeAXRig) UpdateWorkspace(_ context.Context, req *v1alpha1.UpdateWorkspaceRequest) (*v1alpha1.Workspace, error) {
	defer r.lock()()
	r.workspaces[req.GetWorkspace().GetMetadata().GetName()] = req.GetWorkspace()
	return req.GetWorkspace(), nil
}
func (r *fakeAXRig) CreateTask(ctx context.Context, req *v1alpha1.CreateTaskRequest) (*v1alpha1.Task, error) {
	defer r.lock()()
	task := req.GetTask()
	name := task.GetMetadata().GetName()
	if _, ok := r.tasks[name]; ok {
		return nil, status.Error(codes.AlreadyExists, "exists")
	}
	var tok string
	for _, f := range r.workspaces[task.GetSpec().GetWorkspace().GetName()].GetSpec().GetFiles() {
		if f.GetPath() == ".glue/token" {
			tok = strings.TrimSpace(f.GetContent())
		}
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
	task.Status = &v1alpha1.TaskStatus{Phase: "Suspended"}
	r.tasks[name], r.daemons[name], r.managers[strings.TrimPrefix(name, "glue-")] = task, srv, mgr
	return task, nil
}
func (r *fakeAXRig) CreateActorEgressPolicy(_ context.Context, req *ateapipb.CreateActorEgressPolicyRequest) (*ateapipb.EgressPolicy, error) {
	defer r.lock()()
	r.egress[req.GetActor().GetName()] = true
	return req.GetEgressPolicy(), nil
}
func (r *fakeAXRig) ResumeTask(_ context.Context, req *v1alpha1.ResumeTaskRequest) (*v1alpha1.Task, error) {
	defer r.lock()()
	if t, ok := r.tasks[req.GetName()]; ok && r.egress[req.GetName()] {
		t.Status = &v1alpha1.TaskStatus{Phase: "Running", Conditions: []*v1alpha1.Condition{{Type: "WorkspaceReady", Status: "True"}}}
		return t, nil
	}
	return nil, status.Error(codes.FailedPrecondition, "missing task or egress")
}
func (r *fakeAXRig) SuspendTask(_ context.Context, req *v1alpha1.SuspendTaskRequest) (*v1alpha1.Task, error) {
	defer r.lock()()
	t, err := r.taskOrErr(req.GetName())
	if err == nil {
		t.Status = &v1alpha1.TaskStatus{Phase: "Suspended"}
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
func (r *fakeAXRig) DeleteTask(_ context.Context, req *v1alpha1.DeleteTaskRequest) (*v1alpha1.DeleteTaskResponse, error) {
	defer r.lock()()
	delete(r.tasks, req.GetName())
	return &v1alpha1.DeleteTaskResponse{}, nil
}
func (r *fakeAXRig) DeleteWorkspace(_ context.Context, req *v1alpha1.DeleteWorkspaceRequest) (*v1alpha1.DeleteWorkspaceResponse, error) {
	defer r.lock()()
	delete(r.workspaces, req.GetName())
	return &v1alpha1.DeleteWorkspaceResponse{}, nil
}
func (r *fakeAXRig) DeleteActorEgressPolicy(_ context.Context, req *ateapipb.DeleteActorEgressPolicyRequest) (*ateapipb.EgressPolicy, error) {
	defer r.lock()()
	delete(r.egress, req.GetActor().GetName())
	return &ateapipb.EgressPolicy{}, nil
}

func clearJobTestEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "GLUE_CONFIG_DIR", "GLUE_SERVER", "GLUE_TOKEN", "GLUE_DAEMON_TOKEN", "AX_SERVER", "AX_SERVER_ADDR", "GLUE_AX_SERVER", "GLUE_AX_KUBECONFIG", "GLUE_AX_CONTEXT", "KUBECONFIG", "KUBE_CONTEXT", "KUBECONTEXT", "GEMINI_API_KEY", "OPENROUTER_API_KEY", "DEEPSEEK_API_KEY", "NVIDIA_API_KEY", "OPENAI_API_KEY", "GLUE_GITHUB_TOKEN", "GITHUB_TOKEN"} {
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
