// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/erain/glue/jobs"
)

// fakeIssues serves GitHub issues for erain/glue-sandbox and records the
// bodies of pull requests opened against it.
func fakeIssues(t *testing.T, closed ...int) *[]string {
	t.Helper()
	var mu sync.Mutex
	prBodies := &[]string{}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/erain/glue-sandbox" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
			return
		case r.URL.Path == "/repos/erain/glue-sandbox/pulls" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
			return
		case r.URL.Path == "/repos/erain/glue-sandbox/pulls":
			var body struct{ Title, Body string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			*prBodies = append(*prBodies, body.Title+"|"+body.Body)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"html_url":"https://github.com/erain/glue-sandbox/pull/1"}`))
			return
		}
		var n int
		if _, err := fmt.Sscanf(r.URL.Path, "/repos/erain/glue-sandbox/issues/%d", &n); err != nil {
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/comments") {
			var out []string
			for i := 1; i <= 12; i++ {
				out = append(out, fmt.Sprintf(`{"body":"comment %d on %d","user":{"login":"dev"}}`, i, n))
			}
			_, _ = fmt.Fprintf(w, "[%s]", strings.Join(out, ","))
			return
		}
		state := "open"
		for _, c := range closed {
			if c == n {
				state = "closed"
			}
		}
		_, _ = fmt.Fprintf(w, `{"title":"Fix issue %d","body":"Steps for %d.","state":%q,"comments":12}`, n, n, state)
	}))
	t.Cleanup(gh.Close)
	t.Setenv("GLUE_GITHUB_API_URL", gh.URL)
	t.Setenv("GLUE_GITHUB_TOKEN", "tok")
	return prBodies
}

func TestSubmitFromIssue(t *testing.T) {
	api := &attemptsFakeAPI{}
	dir := pushedCheckout(t)
	fakeIssues(t)
	out, _, err := runAttemptsCLIKeepEnv(t, api, "submit", "--ax", "--quiet", "--repo", dir, "--issue", "12")
	if err != nil {
		t.Fatal(err)
	}
	if len(api.jobs) != 1 || strings.TrimSpace(out) != api.jobs[0].ID {
		t.Fatalf("stdout %q jobs %+v", out, api.jobs)
	}
	s := api.jobs[0].Spec
	if s.Objective != "Fix issue 12" || s.Issue != "erain/glue-sandbox#12" || s.Delivery != "draft_pr" {
		t.Fatalf("spec = %+v", s)
	}
	if !strings.Contains(s.Context, "Steps for 12.") || !strings.Contains(s.Context, "@dev: comment 12 on 12") || strings.Contains(s.Context, "comment 2 on 12") {
		t.Fatalf("context = %q", s.Context)
	}
	if _, _, err := runAttemptsCLIKeepEnv(t, api, "submit", "--ax", "--repo", dir, "--issue", "12", "and an objective"); err == nil {
		t.Fatal("objective plus --issue accepted")
	}
	if _, _, err := runAttemptsCLIKeepEnv(t, api, "submit", "--ax", "--repo", dir, "--issue", "erain/other#4"); err == nil || !strings.Contains(err.Error(), "not in this checkout's repository") {
		t.Fatalf("issue from another repo: %v", err)
	}
}

func TestSubmitFromIssuesBatch(t *testing.T) {
	api := &attemptsFakeAPI{}
	dir := pushedCheckout(t)
	prBodies := fakeIssues(t)
	out, _, err := runAttemptsCLIKeepEnv(t, api, "submit", "--ax", "--quiet", "--repo", dir, "--issue", "12", "--issue", "https://github.com/erain/glue-sandbox/issues/13", "--deliver", "branch")
	if err != nil {
		t.Fatal(err)
	}
	if len(api.jobs) != 2 || out != api.jobs[0].ID+"\t#12\n"+api.jobs[1].ID+"\t#13\n" {
		t.Fatalf("stdout %q", out)
	}
	if api.jobs[1].Spec.Issue != "erain/glue-sandbox#13" || api.jobs[1].Spec.Delivery != "branch" || api.jobs[0].ID == api.jobs[1].ID {
		t.Fatalf("second spec = %+v", api.jobs[1].Spec)
	}
	for _, j := range api.jobs {
		j.Spec.Repo.URL = "https://github.com/erain/glue-sandbox"
		if _, err := jobs.OpenDraftPR(context.Background(), j, jobs.Result{Branch: "glue/" + j.ID}, "tok"); err != nil {
			t.Fatal(err)
		}
	}
	if len(*prBodies) != 2 || !strings.HasPrefix((*prBodies)[0], "Fix issue 12|Closes #12\n") || !strings.HasPrefix((*prBodies)[1], "Fix issue 13|Closes #13\n") {
		t.Fatalf("PR bodies = %q", *prBodies)
	}
}

func TestSubmitFromClosedIssue(t *testing.T) {
	api := &attemptsFakeAPI{}
	dir := pushedCheckout(t)
	fakeIssues(t, 13)
	_, _, err := runAttemptsCLIKeepEnv(t, api, "submit", "--ax", "--repo", dir, "--issue", "12", "--issue", "13")
	if err == nil || !strings.Contains(err.Error(), "erain/glue-sandbox#13 is closed") {
		t.Fatalf("closed issue: %v", err)
	}
	if len(api.jobs) != 0 {
		t.Fatalf("submitted %d jobs before finding the closed issue", len(api.jobs))
	}
}
