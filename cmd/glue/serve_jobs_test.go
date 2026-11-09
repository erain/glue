// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
		job := serveJobRequest(t, ts.URL, "/v1/jobs", jobs.Spec{Objective: "add README", Repo: jobs.RepoSpec{Path: repo}, Policy: "yolo"})
		if job.Status != jobs.Created {
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
	if code != 0 || finished.Status != jobs.Achieved {
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
	if code := runCLIWithServe(context.Background(), append(baseArgs(), "--jobs", "--policy-file", policyPath), io.Discard, &errs, fakeFactory(&scriptedProvider{}), serve); code != 0 || created.Spec.Policy != "custom-deny" {
		t.Fatalf("code %d policy %q stderr %s", code, created.Spec.Policy, errs.String())
	}
}
