// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erain/glue"
)

// fakePRGitHub serves one PR (erain/glue-sandbox#7) with review threads,
// comments and check runs, and records writes.
func fakePRGitHub(t *testing.T, openPR bool) (*[]string, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	writes := &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == "/graphql":
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[
				{"isResolved":false,"path":"pool.go","line":42,"comments":{"nodes":[{"body":"Guard the counter with the mutex.","createdAt":"2026-11-12T10:00:00Z","author":{"login":"reviewer"}}]}},
				{"isResolved":true,"path":"old.go","line":1,"comments":{"nodes":[{"body":"RESOLVED THREAD","createdAt":"2026-11-12T09:00:00Z","author":{"login":"reviewer"}}]}}]}}}}}`))
		case p == "/repos/erain/glue-sandbox/issues/7/comments" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[
				{"body":"OLD COMMENT","created_at":"2026-11-11T08:00:00Z","user":{"login":"dev"}},
				{"body":"<!-- glue -->\nGlue job continued","created_at":"2026-11-12T11:00:00Z","user":{"login":"dev"}},
				{"body":"Also rename Pool.Get to Pool.Acquire.","created_at":"2026-11-12T12:00:00Z","user":{"login":"dev"}}]`))
		case p == "/repos/erain/glue-sandbox/commits/abc123/check-runs":
			_, _ = w.Write([]byte(`{"check_runs":[
				{"id":99,"name":"test","conclusion":"failure","completed_at":"2026-11-12T10:30:00Z","output":{"title":"1 test failed","summary":""}},
				{"id":98,"name":"lint","conclusion":"success","completed_at":"2026-11-12T10:30:00Z","output":{"title":"LINT OK"}}]}`))
		case p == "/repos/erain/glue-sandbox/actions/jobs/99/logs":
			_, _ = w.Write([]byte("setup\n--- FAIL: TestPool (0.01s)\n    pool_test.go:12: race detected\nFAIL\n"))
		case p == "/repos/erain/glue-sandbox" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
		case p == "/repos/erain/glue-sandbox/pulls" && r.Method == http.MethodGet:
			if openPR {
				_, _ = w.Write([]byte(`[{"number":7,"html_url":"https://github.com/erain/glue-sandbox/pull/7"}]`))
			} else {
				_, _ = w.Write([]byte(`[]`))
			}
		case r.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			*writes = append(*writes, p+" "+fmtAny(body["body"]))
			mu.Unlock()
			_, _ = w.Write([]byte(`{"html_url":"https://github.com/erain/glue-sandbox/pull/8"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GLUE_GITHUB_API_URL", srv.URL)
	return writes, &mu
}

func fmtAny(v any) string { s, _ := v.(string); return s }

func TestContinueFromPRBuildsInstructions(t *testing.T) {
	fakePRGitHub(t, true)
	since := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	fb, err := FetchPRFeedback(context.Background(), "https://github.com/erain/glue-sandbox/pull/7", "abc123", since, "tok")
	if err != nil {
		t.Fatal(err)
	}
	got := fb.Instructions()
	for _, want := range []string{"pull request https://github.com/erain/glue-sandbox/pull/7", "Review thread on pool.go:42:\n@reviewer: Guard the counter with the mutex.", "Comment from @dev:\nAlso rename Pool.Get", `Failed check "test":` + "\n1 test failed\nLog tail:", "pool_test.go:12: race detected"} {
		if !strings.Contains(got, want) {
			t.Fatalf("instructions lack %q:\n%s", want, got)
		}
	}
	for _, not := range []string{"RESOLVED THREAD", "OLD COMMENT", "Glue job continued", "LINT OK"} {
		if strings.Contains(got, not) {
			t.Fatalf("instructions contain %q:\n%s", not, got)
		}
	}
	// Newest first: the 12:00 comment, the 10:30 check, the 10:00 review.
	if c, k, r := strings.Index(got, "Comment from"), strings.Index(got, "Failed check"), strings.Index(got, "Review thread"); !(c < k && k < r) {
		t.Fatalf("order comment=%d check=%d review=%d", c, k, r)
	}
	big := PRFeedback{URL: "u", Items: []PRFeedbackItem{{Kind: "comment", Author: "a", Text: strings.Repeat("x", 2*MaxPRFeedback)}}}
	if n := len([]rune(big.Instructions())); n != MaxPRFeedback {
		t.Fatalf("capped length = %d", n)
	}
	if (PRFeedback{}).Instructions() != "" {
		t.Fatal("no feedback should give no instructions")
	}
}

func TestDeliveryReusesExistingPR(t *testing.T) {
	writes, mu := fakePRGitHub(t, true)
	job := Job{ID: "job-0123456789ab", Spec: Spec{Objective: "x"}}
	goal := glue.GoalResult{Summary: "guarded the counter", Checklist: []glue.ChecklistItem{{Title: "guard the counter", Done: true}, {Title: "later", Done: false}}}
	url, err := openGitHubDraftPR(context.Background(), "erain", "glue-sandbox", job, goal, Result{Branch: "glue/job-0123456789ab", HeadSHA: "abcdef0123456789"}, "tok")
	if err != nil || url != "https://github.com/erain/glue-sandbox/pull/7" {
		t.Fatalf("url %q err %v", url, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*writes) != 1 || !strings.HasPrefix((*writes)[0], "/repos/erain/glue-sandbox/issues/7/comments <!-- glue -->") {
		t.Fatalf("writes = %q", *writes)
	}
	c := (*writes)[0]
	if !strings.Contains(c, "pushed `abcdef012345`") || !strings.Contains(c, "Addressed:\n- guard the counter") || strings.Contains(c, "later") {
		t.Fatalf("comment = %q", c)
	}
}

func TestDeliveryOpensPRWhenNoneOpen(t *testing.T) {
	writes, mu := fakePRGitHub(t, false)
	url, err := openGitHubDraftPR(context.Background(), "erain", "glue-sandbox", Job{ID: "job-0123456789ab", Spec: Spec{Objective: "x"}}, glue.GoalResult{Summary: "s"}, Result{Branch: "glue/job-0123456789ab"}, "tok")
	mu.Lock()
	defer mu.Unlock()
	if err != nil || url != "https://github.com/erain/glue-sandbox/pull/8" || len(*writes) != 1 || !strings.HasPrefix((*writes)[0], "/repos/erain/glue-sandbox/pulls ") {
		t.Fatalf("url %q err %v writes %q", url, err, *writes)
	}
}
