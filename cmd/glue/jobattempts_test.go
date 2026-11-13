// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/internal/axbackend"
	"github.com/erain/glue/jobs"
)

type attemptsFakeAPI struct {
	jobAPI
	mu      sync.Mutex
	jobs    []jobs.Job
	results map[string]jobs.Result
}

func (f *attemptsFakeAPI) Close() error { return nil }
func (f *attemptsFakeAPI) Submit(_ context.Context, spec jobs.Spec, _ bool) (jobs.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if spec.ID == "" {
		spec.ID = fmt.Sprintf("job-%012d", len(f.jobs)+1)
	}
	j := jobs.Job{ID: spec.ID, Spec: spec, Status: jobs.Running, CreatedAt: time.Unix(1000+int64(len(f.jobs)), 0)}
	f.jobs = append(f.jobs, j)
	return j, nil
}
func (f *attemptsFakeAPI) List(context.Context, jobs.Filter) ([]jobs.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]jobs.Job(nil), f.jobs...)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 { // newest first, like the real backends
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
func (f *attemptsFakeAPI) Result(_ context.Context, id string) (jobs.Result, error) {
	if r, ok := f.results[id]; ok {
		return r, nil
	}
	return jobs.Result{}, jobs.ErrConflict
}

func runAttemptsCLI(t *testing.T, api *attemptsFakeAPI, args ...string) (string, string, error) {
	t.Helper()
	clearJobTestEnv(t)
	orig := newAXJobAPI
	t.Cleanup(func() { newAXJobAPI = orig })
	newAXJobAPI = func(context.Context, axbackend.Options) (jobAPI, error) { return api, nil }
	var stdout, stderr bytes.Buffer
	err := jobCommand(context.Background(), args, strings.NewReader(""), &stdout, &stderr, nil)
	return stdout.String(), stderr.String(), err
}

func TestAttemptsCreatesGroup(t *testing.T) {
	api := &attemptsFakeAPI{}
	out, errOut, err := runAttemptsCLI(t, api, "submit", "--ax", "--quiet", "--repo", "https://github.com/erain/glue-sandbox", "--deliver", "pr", "--attempts", "3", "add Shout")
	if err != nil {
		t.Fatal(err)
	}
	if ids := strings.Fields(out); len(ids) != 3 {
		t.Fatalf("stdout = %q", out)
	}
	group := api.jobs[0].Spec.Group
	if !strings.HasPrefix(group, "grp-") || !strings.Contains(errOut, "glue job compare "+group) {
		t.Fatalf("group %q, stderr %q", group, errOut)
	}
	for _, j := range api.jobs {
		if j.Spec.Group != group || j.Spec.Delivery != "branch" || j.Spec.Objective != "add Shout" {
			t.Fatalf("attempt spec = %+v", j.Spec)
		}
	}
	if api.jobs[0].ID == api.jobs[1].ID {
		t.Fatal("attempts share an ID")
	}
	listOut, _, err := runAttemptsCLI(t, api, "list", "--ax", "--group", group)
	if err != nil || strings.Count(listOut, "job-") != 3 {
		t.Fatalf("list --group: %v\n%s", err, listOut)
	}
	if _, _, err := runAttemptsCLI(t, api, "submit", "--ax", "--attempts", "9", "x"); err == nil {
		t.Fatal("--attempts 9 accepted")
	}
	if _, _, err := runAttemptsCLI(t, api, "submit", "--ax", "--attempts", "2", "--attach", "x"); err == nil {
		t.Fatal("--attempts with --attach accepted")
	}
}

func TestAttemptsSpreadModels(t *testing.T) {
	api := &attemptsFakeAPI{}
	if _, _, err := runAttemptsCLI(t, api, "submit", "--ax", "--quiet", "--repo", "https://github.com/erain/glue-sandbox", "--attempts", "3", "--model", "gemini/gemini-3-flash-preview", "--model", "gemini-3-pro-preview", "x"); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, j := range api.jobs {
		got = append(got, j.Spec.Model)
	}
	if strings.Join(got, ",") != "gemini-3-flash-preview,gemini-3-pro-preview,gemini-3-flash-preview" {
		t.Fatalf("models = %v", got)
	}
}

func groupFixture() *attemptsFakeAPI {
	spec := func(model string) jobs.Spec {
		return jobs.Spec{Objective: "x", Group: "grp-0000000000aa", Model: model, Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}, Delivery: "branch"}
	}
	t0 := time.Unix(1000, 0)
	api := &attemptsFakeAPI{jobs: []jobs.Job{
		{ID: "job-00000000000a", Spec: spec("m-a"), Status: jobs.Achieved, CreatedAt: t0},
		{ID: "job-00000000000b", Spec: spec("m-b"), Status: jobs.Blocked, CreatedAt: t0.Add(time.Second)},
		{ID: "job-00000000000c", Spec: spec(""), Status: jobs.Running, CreatedAt: t0.Add(2 * time.Second)},
		{ID: "job-00000000000d", Spec: jobs.Spec{Objective: "other"}, Status: jobs.Achieved, CreatedAt: t0},
	}}
	api.results = map[string]jobs.Result{
		"job-00000000000a": {Branch: "glue/job-00000000000a", Diffstat: " a.go | 3 ++-\n 2 files changed, 20 insertions(+), 2 deletions(-)\n", Verify: []jobs.VerifyResult{{ExitCode: 0}}, FinishedAt: t0.Add(95 * time.Second), Usage: glueUsage(1234), Summary: "added Shout"},
		"job-00000000000b": {Branch: "glue/job-00000000000b", Diffstat: " 1 file changed, 4 insertions(+)", Verify: []jobs.VerifyResult{{ExitCode: 0}, {ExitCode: 1}}, FinishedAt: t0.Add(61 * time.Second), Usage: glueUsage(900)},
	}
	return api
}

func TestCompareTable(t *testing.T) {
	api := groupFixture()
	var out bytes.Buffer
	if err := jobCompare(context.Background(), api, "job-00000000000b", time.Unix(1100, 0), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 5 || !strings.Contains(lines[0], "grp-0000000000aa · 3 attempts") {
		t.Fatalf("compare:\n%s", out.String())
	}
	for i, want := range [][]string{
		{"job-00000000000a", "m-a", "achieved", "pass", "2 files +20 -2", "1m35s", "1234"},
		{"job-00000000000b", "m-b", "blocked", "fail 1/2", "1 file +4 -0", "1m0s", "900"},
		{"job-00000000000c", "default", "running", "-", "-", "1m38s…", "-"},
	} {
		for _, w := range want {
			if !strings.Contains(lines[i+2], w) {
				t.Fatalf("row %d lacks %q:\n%s", i, w, out.String())
			}
		}
	}
	if strings.Contains(out.String(), "job-00000000000d") {
		t.Fatal("a job outside the group was listed")
	}
}

func TestPickDeliversAndCleans(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/erain/glue-sandbox/pulls":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/erain/glue-sandbox":
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/erain/glue-sandbox/pulls":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["head"] != "glue/job-00000000000a" || body["draft"] != true {
				http.Error(w, "bad PR request", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"html_url":"https://github.com/erain/glue-sandbox/pull/7"}`))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(gh.Close)
	t.Setenv("GLUE_GITHUB_API_URL", gh.URL)
	api := groupFixture()
	var out, errOut bytes.Buffer
	if err := jobPick(context.Background(), api, "job-00000000000a", "tok", &out, &errOut); err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "draft PR: https://github.com/erain/glue-sandbox/pull/7") || !strings.Contains(out.String(), "deleted branch glue/job-00000000000b") {
		t.Fatalf("stdout:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "left job-00000000000c alone: not finished") {
		t.Fatalf("stderr:\n%s", errOut.String())
	}
	var deletes []string
	for _, c := range calls {
		if strings.HasPrefix(c, "DELETE") {
			deletes = append(deletes, c)
		}
	}
	if len(deletes) != 1 || deletes[0] != "DELETE /repos/erain/glue-sandbox/git/refs/heads/glue/job-00000000000b" {
		t.Fatalf("deletes = %v", deletes)
	}
	// Picking again returns the PR the first pick opened, if GitHub lists it.
	api.results["job-00000000000a"] = jobs.Result{Branch: "glue/job-00000000000a", PRURL: "https://github.com/erain/glue-sandbox/pull/7"}
	out.Reset()
	if err := jobPick(context.Background(), api, "job-00000000000a", "tok", &out, &errOut); err != nil || !strings.Contains(out.String(), "pull/7") {
		t.Fatalf("second pick: %v %s", err, out.String())
	}
	if err := jobPick(context.Background(), api, "job-00000000000a", "", &out, &errOut); err == nil {
		t.Fatal("pick without a token should fail")
	}
}

func glueUsage(total int64) glue.Usage { return glue.Usage{TotalTokens: total} }

func TestLiveAttempts(t *testing.T) {
	if os.Getenv("GLUE_E2E_AX") != "1" {
		t.Skip("skipping live attempts test; set GLUE_E2E_AX=1 to run")
	}
	if err := checkLiveDirectAX("", ""); err != nil {
		if axLiveStrict() {
			t.Fatalf("live AX cluster required when CI=true or GLUE_E2E_AX_STRICT=1: %v", err)
		}
		t.Skipf("live AX cluster unavailable (%v)", err)
	}
	if !envSet("GEMINI_API_KEY", "OPENROUTER_API_KEY") {
		t.Skip("no model key")
	}
	token, _ := githubToken(t.Context())
	if token == "" {
		t.Skip("no GitHub token")
	}
	for _, k := range []string{"GLUE_SERVER", "GLUE_TOKEN", "GLUE_DAEMON_TOKEN", "AX_SERVER"} {
		t.Setenv(k, "")
	}
	t.Setenv("GLUE_CONFIG_DIR", t.TempDir())
	t.Cleanup(func() { _ = axbackend.StopTunnels("") })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	run := func(args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := runCLIWithDeps(ctx, args, strings.NewReader(""), &stdout, &stderr, defaultProviderFactory, serveDaemon, nil)
		checkAX(t, code == 0, "glue %v failed (%d): %s", args, code, stderr.String())
		return stdout.String()
	}
	ids := strings.Fields(run("--remote", "Create attempt.txt containing the exact line attempt-ok.", "--repo", "https://github.com/erain/glue-sandbox", "--attempts", "2", "--quiet", "--max-iterations", "2", "--verify", "grep -qx attempt-ok attempt.txt"))
	checkAX(t, len(ids) == 2, "attempt IDs = %v", ids)
	var branches []string
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), time.Minute)
		defer ccancel()
		for _, b := range branches {
			_ = jobs.DeleteBranch(cctx, "https://github.com/erain/glue-sandbox", b, token)
		}
		for _, id := range ids {
			_ = runCLIWithDeps(cctx, []string{"job", "delete", id}, strings.NewReader(""), io.Discard, io.Discard, defaultProviderFactory, serveDaemon, nil)
		}
	})
	for _, id := range ids {
		run("job", "attach", id)
		var res jobs.Result
		checkAX(t, json.Unmarshal([]byte(run("job", "result", id, "--json")), &res) == nil && res.Branch != "", "result of %s: %+v", id, res)
		branches = append(branches, res.Branch)
	}
	cmpOut := run("job", "compare", ids[0])
	checkAX(t, strings.Contains(cmpOut, "2 attempts") && strings.Contains(cmpOut, ids[0]) && strings.Contains(cmpOut, ids[1]), "compare:\n%s", cmpOut)
	pickOut := run("job", "pick", ids[0])
	checkAX(t, strings.Contains(pickOut, "draft PR: https://github.com/erain/glue-sandbox/pull/") && strings.Contains(pickOut, "deleted branch "+branches[1]), "pick:\n%s", pickOut)
	prURL := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(pickOut, "\n", 2)[0], "draft PR: "))
	t.Cleanup(func() { closeLivePR(t, prURL, token) })
	out, err := exec.CommandContext(ctx, "git", "ls-remote", "--heads", "https://github.com/erain/glue-sandbox", branches[0], branches[1]).Output()
	checkAX(t, err == nil && strings.Contains(string(out), branches[0]) && !strings.Contains(string(out), branches[1]), "remote heads after pick: %v\n%s", err, out)
	t.Logf("compare:\n%s\npick:\n%s", cmpOut, pickOut)
}

// closeLivePR closes a pull request a live test opened.
func closeLivePR(t *testing.T, prURL, token string) {
	path := strings.TrimPrefix(prURL, "https://github.com/")
	owner, rest, _ := strings.Cut(path, "/")
	repo, num, _ := strings.Cut(strings.Replace(rest, "/pull/", "/", 1), "/")
	req, err := http.NewRequest(http.MethodPatch, fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls/%s", owner, repo, num), strings.NewReader(`{"state":"closed"}`))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		t.Logf("closed %s: %s", prURL, resp.Status)
	}
}
