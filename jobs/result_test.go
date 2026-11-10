// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erain/glue"
)

func TestMain(m *testing.M) {
	if os.Getenv("GLUE_GIT_ASKPASS") == "1" {
		os.Exit(Askpass(os.Args[1:], os.Stdout))
	}
	if len(os.Args) > 1 && os.Args[1] == "git-askpass" {
		os.Exit(Askpass(os.Args[2:], os.Stdout))
	}
	os.Exit(m.Run())
}

func repoWithBareOrigin(t *testing.T, remoteURL string) (string, string) {
	t.Helper()
	repo := repoFixture(t)
	bare := filepath.Join(t.TempDir(), "octocat", "hello-world.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "--bare", "-qb", "main", bare).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v %s", err, out)
	}
	if remoteURL == "" {
		remoteURL = bare
	}
	for _, args := range [][]string{
		{"remote", "add", "origin", remoteURL},
		{"remote", "set-url", "--push", "origin", bare},
		{"push", "-q", "origin", "main:main"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return repo, bare
}

func TestVerifyFailureRecorded(t *testing.T) {
	repo, bare := repoWithBareOrigin(t, "https://github.com/octocat/hello-world.git")
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected GitHub API request for non-achieved job: %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	t.Cleanup(api.Close)
	t.Setenv("GLUE_GITHUB_API_URL", api.URL)
	t.Setenv("GLUE_GITHUB_TOKEN", "test-verify-token")

	p := &scriptProvider{turns: [][]glue.ProviderEvent{
		turn(`{"items":[{"title":"edit file"}]}`),
		{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "w1", Name: "write_file", Arguments: []byte(`{"path":"work.txt","content":"partial\n"}`)}}, {Type: glue.ProviderEventDone}},
		turn("wrote partial file"),
		turn(`{"done":false,"items":[{"title":"edit file"}],"summary":"still failing verify"}`),
	}}
	stateDir := t.TempDir()
	m, err := New(Options{StateDir: stateDir, ProviderFactory: func(string) (glue.Provider, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })

	job, err := m.Create(context.Background(), Spec{
		Objective: "edit file",
		Repo:      RepoSpec{Path: repo, BaseRef: "main"},
		Budgets:   Budgets{MaxIterations: 1},
		Delivery:  "draft_pr",
		ShellEnv:  []string{"CUSTOM_VERIFY_ENV=verify-env-ok"},
		Verify: [][]string{
			{"sh", "-c", "i=1; while [ $i -le 249 ]; do echo line-$i; i=$((i+1)); done; echo $CUSTOM_VERIFY_ENV"},
			{"sh", "-c", "echo verify check failed >&2; exit 7"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Result(context.Background(), job.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("Result before start = %v, want ErrConflict", err)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if finished := waitJob(t, m, job.ID); finished.Status != MaxIterations {
		t.Fatalf("status = %s, want %s", finished.Status, MaxIterations)
	}

	res, err := m.Result(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.JobID != job.ID || res.Status != MaxIterations || res.Summary != "still failing verify" || len(res.Verify) != 2 || res.PRURL != "" || res.DeliveryError != "" {
		t.Fatalf("unexpected result: %+v", res)
	}
	lines := strings.Split(res.Verify[0].OutputTail, "\n")
	if res.Verify[0].ExitCode != 0 || len(lines) != 200 || lines[0] != "line-51" || lines[len(lines)-1] != "verify-env-ok" {
		t.Fatalf("verify[0] = %+v (lines=%d)", res.Verify[0], len(lines))
	}
	if res.Verify[1].ExitCode != 7 || res.Verify[1].OutputTail != "verify check failed" {
		t.Fatalf("verify[1] = %+v", res.Verify[1])
	}
	if res.HeadSHA == "" || res.HeadSHA == res.BaseSHA || !strings.Contains(res.Diffstat, "work.txt") {
		t.Fatalf("commit/diffstat missing: %+v", res)
	}
	pushedSHA, err := exec.Command("git", "-C", bare, "rev-parse", "refs/heads/"+res.Branch).CombinedOutput()
	if err != nil || strings.TrimSpace(string(pushedSHA)) != res.HeadSHA {
		t.Fatalf("pushed branch SHA = %q err %v, want %s", pushedSHA, err, res.HeadSHA)
	}
	raw, err := os.ReadFile(filepath.Join(stateDir, "jobs", job.ID, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var disk Result
	if err := json.Unmarshal(raw, &disk); err != nil || disk.Verify[1].ExitCode != 7 || disk.HeadSHA != res.HeadSHA {
		t.Fatalf("disk result mismatch: %+v err %v", disk, err)
	}
}

func TestResultCommitsWorkspaceWithAuthorAndDiffstat(t *testing.T) {
	t.Setenv("GLUE_GIT_AUTHOR_NAME", "Glue Bot")
	t.Setenv("GLUE_GIT_AUTHOR_EMAIL", "bot@example.com")

	repo := repoFixture(t)
	p := &scriptProvider{turns: [][]glue.ProviderEvent{
		turn(`{"items":[{"title":"write file"}]}`),
		{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "write", Name: "write_file", Arguments: []byte(`{"path":"created.txt","content":"hello\n"}`)}}, {Type: glue.ProviderEventDone}},
		turn("wrote file"),
		turn(`{"done":true,"items":[{"title":"write file","done":true,"evidence":"created.txt"}],"summary":"Add created.txt greeting"}`),
	}}
	m, err := New(Options{StateDir: t.TempDir(), ProviderFactory: func(string) (glue.Provider, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })

	job, err := m.Create(context.Background(), Spec{Objective: "write file", Repo: RepoSpec{Path: repo, BaseRef: "main"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	finished := waitJob(t, m, job.ID)
	res, err := m.Result(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.JobID != job.ID || res.Status != Achieved || res.BaseRef != "main" || res.Branch != "glue/"+job.ID || res.BaseSHA == "" || res.BaseSHA == res.HeadSHA || !strings.Contains(res.Diffstat, "created.txt") || res.FinishedAt.IsZero() {
		t.Fatalf("unexpected result: %+v", res)
	}
	out, err := exec.Command("git", "-C", finished.Workspace, "log", "-1", "--format=%an|%ae|%s").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "Glue Bot|bot@example.com|Add created.txt greeting" {
		t.Fatalf("git log = %q err %v", out, err)
	}
}

type recordedPRRequest struct {
	Path, Auth, Title, Head, Base, Body string
	Draft                               bool
}

func runDeliveredJob(t *testing.T, repoURL, parentFullName, token string) (*Manager, string, string, Job, Result, recordedPRRequest) {
	t.Helper()
	repo, bare := repoWithBareOrigin(t, repoURL)
	var gotPR recordedPRRequest
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/"):
			if parentFullName != "" {
				_, _ = w.Write([]byte(`{"default_branch":"main","parent":{"full_name":"` + parentFullName + `","default_branch":"main"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			var body struct {
				Title, Head, Base, Body string
				Draft                   bool
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode PR body: %v", err)
			}
			gotPR = recordedPRRequest{Path: r.URL.Path, Auth: r.Header.Get("Authorization"), Title: body.Title, Head: body.Head, Base: body.Base, Body: body.Body, Draft: body.Draft}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"html_url":"https://github.com` + strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos"), "/pulls") + `/pull/42"}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(api.Close)
	t.Setenv("GLUE_GITHUB_API_URL", api.URL)
	t.Setenv("GLUE_GITHUB_TOKEN", token)

	p := &scriptProvider{turns: [][]glue.ProviderEvent{
		turn(`{"items":[{"title":"deliver feature"}]}`),
		{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "w1", Name: "write_file", Arguments: []byte(`{"path":"feature.txt","content":"delivered\n"}`)}}, {Type: glue.ProviderEventDone}},
		turn("created feature.txt"),
		turn(`{"done":true,"items":[{"title":"deliver feature","done":true,"evidence":"feature.txt created"}],"summary":"Ship feature.txt"}`),
	}}
	stateDir := t.TempDir()
	m, err := New(Options{StateDir: stateDir, ProviderFactory: func(string) (glue.Provider, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	job, err := m.Create(context.Background(), Spec{
		Objective: "Implement feature.txt", Repo: RepoSpec{Path: repo, BaseRef: "main"}, Delivery: "draft_pr",
		ShellEnvSecretAllow: []string{"GLUE_GITHUB_TOKEN"}, Verify: [][]string{{"sh", "-c", "echo verify-suite-passed; env"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	finished := waitJob(t, m, job.ID)
	res, err := m.Result(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	return m, stateDir, bare, finished, res, gotPR
}

func TestDeliverDraftPR(t *testing.T) {
	_, _, bare, job, res, pr := runDeliveredJob(t, "https://github.com/octocat/hello-world.git", "", "test-gh-token")
	if res.Status != Achieved || res.DeliveryError != "" || res.PRURL != "https://github.com/octocat/hello-world/pull/42" {
		t.Fatalf("unexpected result: %+v", res)
	}
	count, err := exec.Command("git", "-C", bare, "rev-list", "--count", res.BaseSHA+"..refs/heads/"+res.Branch).CombinedOutput()
	if err != nil || strings.TrimSpace(string(count)) != "1" {
		t.Fatalf("commit count = %q err %v", count, err)
	}
	if pr.Path != "/repos/octocat/hello-world/pulls" || !pr.Draft || pr.Base != "main" || pr.Head != "glue/"+job.ID {
		t.Fatalf("unexpected PR request: %+v", pr)
	}
	for _, want := range []string{"Implement feature.txt", "deliver feature", "Ship feature.txt", "verify-suite-passed", "Opened by Glue job " + job.ID} {
		if !strings.Contains(pr.Body, want) {
			t.Fatalf("PR body missing %q:\n%s", want, pr.Body)
		}
	}
	_, _, _, _, failedRes, _ := runDeliveredJob(t, "invalid-origin", "", "test-gh-token")
	if failedRes.Status != Achieved || failedRes.DeliveryError == "" || failedRes.PRURL != "" {
		t.Fatalf("delivery failure should preserve Achieved and record error: %+v", failedRes)
	}
}

func TestDeliverToForkParent(t *testing.T) {
	_, _, bare, job, res, pr := runDeliveredJob(t, "git@github.com:fork-owner/hello-world.git", "upstream-org/hello-world", "test-gh-token")
	if res.Status != Achieved || res.DeliveryError != "" || res.PRURL != "https://github.com/upstream-org/hello-world/pull/42" {
		t.Fatalf("unexpected result: %+v", res)
	}
	pushed, err := exec.Command("git", "-C", bare, "rev-parse", "refs/heads/"+res.Branch).CombinedOutput()
	if err != nil || strings.TrimSpace(string(pushed)) != res.HeadSHA {
		t.Fatalf("pushed SHA = %q err %v, want %s", pushed, err, res.HeadSHA)
	}
	if pr.Path != "/repos/upstream-org/hello-world/pulls" || !pr.Draft || pr.Base != "main" || pr.Head != "fork-owner:glue/"+job.ID {
		t.Fatalf("unexpected fork PR request: %+v", pr)
	}
}

func TestTokenNeverPersisted(t *testing.T) {
	const secret = "ghp_secret_never_persisted_token_99887766"
	m, stateDir, _, job, res, pr := runDeliveredJob(t, "https://github.com/octocat/hello-world.git", "", secret)
	if res.Status != Achieved || pr.Auth != "Bearer "+secret {
		t.Fatalf("status=%s auth=%q", res.Status, pr.Auth)
	}
	err := filepath.WalkDir(stateDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("token leaked in state file %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := m.Events(context.Background(), job.ID, 0)
	if err != nil || len(events) == 0 {
		t.Fatalf("events=%d err=%v", len(events), err)
	}
	for _, ev := range events {
		raw, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("token leaked in event %d (%s)", ev.Seq, ev.Type)
		}
	}
}
