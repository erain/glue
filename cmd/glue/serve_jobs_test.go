// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/jobs"
)

func serveJobRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-qb", "main"}, {"config", "user.email", "test@example.com"}, {"config", "user.name", "test"}, {"commit", "--allow-empty", "-qm", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v %s", err, out)
		}
	}
	return repo
}
func serveJobRequest(t *testing.T, base, path string, body any) jobs.Job {
	t.Helper()
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer fixture-token")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 && response.StatusCode != 201 {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("status %d %s", response.StatusCode, data)
	}
	var result struct {
		Job jobs.Job `json:"job"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result.Job
}
func TestServeJobEndToEnd(t *testing.T) {
	repo := serveJobRepo(t)
	state := t.TempDir()
	p := &scriptedProvider{turns: [][]glue.ProviderEvent{textTurn(`{"items":[{"title":"README"}]}`), toolCallTurn("write", "write_file", `{"path":"README.md","content":"A scratch repository.\n"}`), textTurn("wrote README"), textTurn(`{"done":true,"items":[{"title":"README","done":true}],"summary":"done"}`)}}
	var finished jobs.Detail
	serve := func(ctx context.Context, _ serveConfig, handler http.Handler, _ io.Writer) error {
		ts := httptest.NewServer(handler)
		defer ts.Close()
		job := serveJobRequest(t, ts.URL, "/v1/jobs", jobs.Spec{Objective: "add README", Repo: jobs.RepoSpec{Path: repo}})
		if job.Status != jobs.Created || job.Spec.Policy != "auto" {
			t.Fatal(job)
		}
		serveJobRequest(t, ts.URL, "/v1/jobs/"+job.ID+"/start", nil)
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/jobs/"+job.ID+"/events", nil)
		req.Header.Set("Authorization", "Bearer fixture-token")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || !bytes.Contains(data, []byte("event: job_done")) {
			t.Fatalf("events %s err %v", data, err)
		}
		req, _ = http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/jobs/"+job.ID, nil)
		req.Header.Set("Authorization", "Bearer fixture-token")
		response, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if err := json.NewDecoder(response.Body).Decode(&finished); err != nil {
			t.Fatal(err)
		}
		return nil
	}
	var out, errs bytes.Buffer
	code := runCLIWithServe(context.Background(), []string{"serve", "--coding", "--jobs", "--state-dir", state, "--store", t.TempDir(), "--work", repo, "--metadata", "", "--token", "fixture-token"}, &out, &errs, fakeFactory(p), serve)
	if code != 0 || finished.Status != jobs.Achieved || strings.Contains(errs.String(), "WARNING:") {
		t.Fatalf("code %d job %+v errors %s", code, finished, errs.String())
	}
	if data, err := os.ReadFile(filepath.Join(finished.Workspace, "README.md")); err != nil || string(data) != "A scratch repository.\n" {
		t.Fatalf("README %q err %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(repo, "README.md")); !os.IsNotExist(err) {
		t.Fatalf("source checkout changed: %v", err)
	}
}
func TestServeJobsDefaultsAndOverride(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		want  int
	}{{"off", nil, 404}, {"coding", []string{"--coding"}, 200}, {"explicit on", []string{"--jobs"}, 200}, {"explicit off", []string{"--coding", "--jobs=false"}, 404}} {
		t.Run(tc.name, func(t *testing.T) {
			state := t.TempDir()
			p := &scriptedProvider{}
			serve := func(_ context.Context, _ serveConfig, h http.Handler, _ io.Writer) error {
				req := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
				req.Header.Set("Authorization", "Bearer fixture-token")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				if w.Code != tc.want {
					t.Fatalf("status %d want %d: %s", w.Code, tc.want, w.Body.String())
				}
				if len(p.requests) != 0 {
					t.Fatal("startup executed provider")
				}
				for _, dir := range []string{"worktrees", "repos"} {
					if _, err := os.Stat(filepath.Join(state, dir)); !os.IsNotExist(err) {
						t.Fatal("startup prepared workspace")
					}
				}
				return nil
			}
			args := append([]string{"serve", "--state-dir", state, "--store", t.TempDir(), "--work", t.TempDir(), "--metadata", "", "--token", "fixture-token"}, tc.flags...)
			var errs bytes.Buffer
			if code := runCLIWithServe(context.Background(), args, io.Discard, &errs, fakeFactory(p), serve); code != 0 {
				t.Fatalf("code %d: %s", code, errs.String())
			}
		})
	}
}

type serveBlockingProvider struct{ started chan struct{} }

func (p serveBlockingProvider) Stream(ctx context.Context, _ glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	ch := make(chan glue.ProviderEvent)
	close(p.started)
	go func() { <-ctx.Done(); close(ch) }()
	return ch, nil
}
func TestServeClosesJobManager(t *testing.T) {
	repo := serveJobRepo(t)
	state := t.TempDir()
	p := serveBlockingProvider{make(chan struct{})}
	id := ""
	serve := func(_ context.Context, _ serveConfig, h http.Handler, _ io.Writer) error {
		ts := httptest.NewServer(h)
		defer ts.Close()
		id = serveJobRequest(t, ts.URL, "/v1/jobs?start=true", jobs.Spec{Objective: "wait", Repo: jobs.RepoSpec{Path: repo}}).ID
		select {
		case <-p.started:
		case <-time.After(10 * time.Second):
			t.Fatal("provider did not start")
		}
		return errors.New("fixture stop")
	}
	var errs bytes.Buffer
	code := runCLIWithServe(context.Background(), []string{"serve", "--jobs", "--state-dir", state, "--store", t.TempDir(), "--metadata", "", "--token", "fixture-token"}, io.Discard, &errs, fakeFactory(p), serve)
	if code != 1 || !strings.Contains(errs.String(), "fixture stop") {
		t.Fatalf("code %d: %s", code, errs.String())
	}
	m, err := jobs.New(jobs.Options{StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	job, err := m.Get(context.Background(), id)
	if err != nil || job.Status != jobs.Interrupted {
		t.Fatalf("job %+v err %v", job, err)
	}
}

func TestServeJobsStartupWithoutCredentials(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	for _, coding := range []bool{false, true} {
		t.Run(fmt.Sprint(coding), func(t *testing.T) {
			args := []string{"serve", "--jobs", "--state-dir", t.TempDir(), "--store", t.TempDir(), "--work", t.TempDir(), "--metadata", "", "--token", "fixture-token"}
			if coding {
				args = append(args, "--coding")
			}
			serve := func(_ context.Context, _ serveConfig, h http.Handler, _ io.Writer) error {
				req := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
				req.Header.Set("Authorization", "Bearer fixture-token")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				if w.Code != 200 {
					t.Fatal(w.Body.String())
				}
				return nil
			}
			var errs bytes.Buffer
			if code := runCLIWithServe(context.Background(), args, io.Discard, &errs, defaultProviderFactory, serve); code != 0 {
				t.Fatalf("code %d: %s", code, errs.String())
			}
		})
	}
}

func TestServePolicyFlags(t *testing.T) {
	policyPath := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policyPath, []byte(`{"name":"custom-deny","default":"deny"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	baseArgs := func() []string {
		return []string{"serve", "--state-dir", t.TempDir(), "--store", t.TempDir(), "--work", t.TempDir(), "--metadata", "", "--token", "fixture-token"}
	}
	noopServe := func(context.Context, serveConfig, http.Handler, io.Writer) error { return nil }
	for name, extra := range map[string][]string{
		"both flags":     {"--policy", "ask-all", "--policy-file", policyPath},
		"unknown policy": {"--policy", "invalid"},
		"missing file":   {"--policy-file", filepath.Join(t.TempDir(), "missing.json")},
	} {
		t.Run(name, func(t *testing.T) {
			var errs bytes.Buffer
			if code := runCLIWithServe(context.Background(), append(baseArgs(), extra...), io.Discard, &errs, fakeFactory(&scriptedProvider{}), noopServe); code == 0 {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
	var errs bytes.Buffer
	if code := runCLIWithServe(context.Background(), append(baseArgs(), "--policy", "yolo"), io.Discard, &errs, fakeFactory(&scriptedProvider{}), noopServe); code != 0 || !strings.Contains(errs.String(), "WARNING:") {
		t.Fatalf("code %d stderr %q", code, errs.String())
	}
	repo := serveJobRepo(t)
	var created jobs.Job
	serve := func(_ context.Context, _ serveConfig, h http.Handler, _ io.Writer) error {
		ts := httptest.NewServer(h)
		defer ts.Close()
		created = serveJobRequest(t, ts.URL, "/v1/jobs", jobs.Spec{Objective: "check policy", Repo: jobs.RepoSpec{Path: repo}})
		return nil
	}
	errs.Reset()
	if code := runCLIWithServe(context.Background(), append(baseArgs(), "--jobs", "--policy-file", policyPath), io.Discard, &errs, fakeFactory(&scriptedProvider{}), serve); code != 0 || created.Spec.Policy != "custom-deny" || strings.Contains(errs.String(), "WARNING:") {
		t.Fatalf("code %d policy %q stderr %s", code, created.Spec.Policy, errs.String())
	}
	for _, tc := range []struct {
		name       string
		flags      []string
		wantPolicy string
		turns      [][]glue.ProviderEvent
		wantAsk    bool
	}{
		{
			name:       "default auto runs shell pipelines and unlisted binaries",
			flags:      nil,
			wantPolicy: "auto",
			turns: [][]glue.ProviderEvent{
				textTurn(`{"items":[{"title":"auto shell"}]}`),
				toolCallTurn("sh1", "shell_exec", `{"argv":["env"]}`),
				toolCallTurn("sh2", "shell_exec", `{"argv":["sh","-c","printf auto-pipe-ok"]}`),
				textTurn("ran commands"),
				textTurn(`{"done":true,"items":[{"title":"auto shell","done":true}],"summary":"ok"}`),
			},
		},
		{
			name:       "workspace-auto prompts on git push",
			flags:      []string{"--policy", "workspace-auto"},
			wantPolicy: "workspace-auto",
			turns: [][]glue.ProviderEvent{
				textTurn(`{"items":[{"title":"push"}]}`),
				toolCallTurn("w1", "write_file", `{"path":"note.txt","content":"hi\n"}`),
				toolCallTurn("push", "shell_exec", `{"argv":["git","push","origin","main"]}`),
				textTurn("stopped"),
				textTurn(`{"done":true,"items":[{"title":"push","done":true}],"summary":"ok"}`),
			},
			wantAsk: true,
		},
		{
			name:       "ask-all prompts on workspace write",
			flags:      []string{"--policy", "ask-all"},
			wantPolicy: "ask-all",
			turns: [][]glue.ProviderEvent{
				textTurn(`{"items":[{"title":"write"}]}`),
				toolCallTurn("w1", "write_file", `{"path":"note.txt","content":"hi\n"}`),
				textTurn("stopped"),
				textTurn(`{"done":true,"items":[{"title":"write","done":true}],"summary":"ok"}`),
			},
			wantAsk: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := serveJobRepo(t)
			p := &scriptedProvider{turns: tc.turns}
			var stderr bytes.Buffer
			runServe := func(ctx context.Context, _ serveConfig, h http.Handler, _ io.Writer) error {
				ts := httptest.NewServer(h)
				defer ts.Close()
				job := serveJobRequest(t, ts.URL, "/v1/jobs?start=true", jobs.Spec{Objective: tc.name, Repo: jobs.RepoSpec{Path: r}})
				if job.Spec.Policy != tc.wantPolicy {
					t.Fatalf("policy = %q, want %q", job.Spec.Policy, tc.wantPolicy)
				}
				if tc.wantAsk {
					deadline := time.Now().Add(10 * time.Second)
					for {
						req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/jobs/"+job.ID+"/permissions?state=pending", nil)
						req.Header.Set("Authorization", "Bearer fixture-token")
						resp, err := http.DefaultClient.Do(req)
						if err != nil {
							t.Fatal(err)
						}
						var perms struct {
							Permissions []struct {
								ID string `json:"id"`
							} `json:"permissions"`
						}
						_ = json.NewDecoder(resp.Body).Decode(&perms)
						resp.Body.Close()
						if len(perms.Permissions) == 1 {
							serveJobRequest(t, ts.URL, "/v1/jobs/"+job.ID+"/permissions/"+perms.Permissions[0].ID+"/decision", map[string]any{"allow": false, "reason": "denied in test"})
							break
						}
						if time.Now().After(deadline) {
							t.Fatalf("timed out waiting for permission prompt on %s", tc.name)
						}
						time.Sleep(5 * time.Millisecond)
					}
				}
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/jobs/"+job.ID+"/events", nil)
				req.Header.Set("Authorization", "Bearer fixture-token")
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				events, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if !bytes.Contains(events, []byte("event: job_done")) {
					t.Fatalf("missing job_done:\n%s", events)
				}
				if !tc.wantAsk && (bytes.Contains(events, []byte("event: permission_request")) || !bytes.Contains(events, []byte("auto-pipe-ok"))) {
					t.Fatalf("unexpected events under auto:\n%s", events)
				}
				return nil
			}
			args := append([]string{"serve", "--coding", "--jobs", "--state-dir", t.TempDir(), "--store", t.TempDir(), "--work", r, "--metadata", "", "--token", "fixture-token"}, tc.flags...)
			if code := runCLIWithServe(context.Background(), args, io.Discard, &stderr, fakeFactory(p), runServe); code != 0 || strings.Contains(stderr.String(), "WARNING:") {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
		})
	}
}

func TestServeJobShellEnvAndHomeDefaults(t *testing.T) {
	for k, v := range map[string]string{"HOME": "", "GEMINI_API_KEY": "parent-gemini-secret", "ALLOWED_API_KEY": "allowed-secret-val"} {
		t.Setenv(k, v)
	}
	repo, state := serveJobRepo(t), t.TempDir()
	p := &scriptedProvider{turns: [][]glue.ProviderEvent{textTurn(`{"items":[{"title":"env"}]}`), toolCallTurn("sh1", "shell_exec", `{"argv":["env"]}`), textTurn("ran env"), textTurn(`{"done":true,"items":[{"title":"env","done":true}],"summary":"ok"}`)}}
	var eventsBody string
	serve := func(ctx context.Context, _ serveConfig, handler http.Handler, _ io.Writer) error {
		ts := httptest.NewServer(handler)
		defer ts.Close()
		job := serveJobRequest(t, ts.URL, "/v1/jobs?start=true", jobs.Spec{Objective: "check shell env", Repo: jobs.RepoSpec{Path: repo}, Policy: "yolo", AllowedBinaries: []string{"env"}, ShellEnv: []string{"JOB_SPECIFIC=yes"}})
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/jobs/"+job.ID+"/events", nil)
		req.Header.Set("Authorization", "Bearer fixture-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		eventsBody = string(data)
		return nil
	}
	var out, errs bytes.Buffer
	args := []string{"serve", "--coding", "--jobs", "--state-dir", state, "--store", t.TempDir(), "--work", repo, "--metadata", "", "--token", "fixture-token", "--shell-env", "DAEMON_VAR=1", "--shell-env-secret-allow", "ALLOWED_API_KEY", "--shell-timeout", "2m"}
	if code := runCLIWithServe(context.Background(), args, &out, &errs, fakeFactory(p), serve); code != 0 || !strings.Contains(eventsBody, "event: job_done") || strings.Contains(eventsBody, "parent-gemini-secret") {
		t.Fatalf("code %d events %s stderr %s", code, eventsBody, errs.String())
	}
	for _, want := range []string{"HOME=" + filepath.Join(state, "home") + "/job-", "DAEMON_VAR=1", "JOB_SPECIFIC=yes", "ALLOWED_API_KEY=allowed-secret-val"} {
		if !strings.Contains(eventsBody, want) {
			t.Fatalf("events missing %q:\n%s", want, eventsBody)
		}
	}
}

func TestJobFromAXIsIdempotent(t *testing.T) {
	state, store, work := t.TempDir(), t.TempDir(), t.TempDir()
	tokenPath := filepath.Join(t.TempDir(), ".glue", "token")
	t.Setenv("AX_METADATA_URL", "")
	t.Setenv("GLUE_DAEMON_TOKEN", "")
	t.Setenv("AX_TASK_YAML", "apiVersion: ax.io/v1alpha1\nkind: Task\nspec:\n  config:\n    objective: \"boot idempotently\"\n    repo:\n      path: "+work+"\n")
	var tokens, ids []string
	for boot := 1; boot <= 2; boot++ {
		var errs bytes.Buffer
		serve := func(_ context.Context, cfg serveConfig, h http.Handler, _ io.Writer) error {
			tokens = append(tokens, cfg.Token)
			req := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			var body struct {
				Jobs []jobs.Job `json:"jobs"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body.Jobs) != 1 || body.Jobs[0].Status != jobs.Created {
				t.Fatalf("boot %d: code=%d body=%s", boot, w.Code, w.Body.String())
			}
			ids = append(ids, body.Jobs[0].ID)
			return nil
		}
		args := []string{"serve", "--coding", "--jobs", "--job-from-ax", "--state-dir", state, "--store", store, "--work", work, "--metadata", "", "--token-file", tokenPath}
		if code := runCLIWithServe(context.Background(), args, io.Discard, &errs, defaultProviderFactory, serve); code != 0 {
			t.Fatalf("boot %d code %d: %s", boot, code, errs.String())
		}
	}
	if ids[0] != ids[1] || tokens[0] == "" || tokens[0] != tokens[1] {
		t.Fatalf("ids=%v tokens=%v", ids, tokens)
	}
	if info, err := os.Stat(tokenPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token file stat err=%v mode=%v", err, info)
	}
}

func TestServeStartupMakesNoOutboundCalls(t *testing.T) {
	origTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = origTransport })
	failDial := func(_ context.Context, network, addr string) (net.Conn, error) {
		t.Errorf("unexpected outbound network dial to %s %s", network, addr)
		return nil, errors.New("outbound network disabled")
	}
	http.DefaultTransport = &http.Transport{DialContext: failDial, DialTLSContext: failDial}
	t.Setenv("AX_METADATA_URL", "")
	t.Setenv("AX_TASK_YAML", "apiVersion: ax.io/v1alpha1\nkind: Task\nspec:\n  config:\n    objective: \"no outbound calls\"\n    repo:\n      path: /workspace\n")
	p := &scriptedProvider{}
	serve := func(_ context.Context, _ serveConfig, h http.Handler, _ io.Writer) error {
		req := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
		req.Header.Set("Authorization", "Bearer t")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"created"`) || len(p.requests) != 0 {
			t.Fatalf("code=%d body=%s requests=%d", w.Code, w.Body.String(), len(p.requests))
		}
		return nil
	}
	var errs bytes.Buffer
	args := []string{"serve", "--coding", "--jobs", "--job-from-ax", "--state-dir", t.TempDir(), "--store", t.TempDir(), "--work", t.TempDir(), "--metadata", "", "--token", "t"}
	if code := runCLIWithServe(context.Background(), args, io.Discard, &errs, fakeFactory(p), serve); code != 0 {
		t.Fatalf("code %d: %s", code, errs.String())
	}
}
