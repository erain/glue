// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erain/glue"
)

func TestIssuePRBodyCloses(t *testing.T) {
	// Inside a task the spec has no repo URL; the PR's repository decides.
	for _, tc := range []struct{ issue, want string }{
		{"erain/glue-sandbox#12", "Closes #12\n\n## Objective"},
		{"Erain/Glue-Sandbox#5", "Closes #5\n\n"},
		{"erain/other#3", "Closes erain/other#3\n\n"},
		{"", "## Objective"},
	} {
		body := prBody(Job{Spec: Spec{Objective: "x", Issue: tc.issue, Repo: RepoSpec{Path: "/workspace"}}}, glue.GoalResult{}, Result{}, "erain", "glue-sandbox")
		if !strings.HasPrefix(body, tc.want) {
			t.Errorf("issue %q: body starts %q", tc.issue, body[:min(len(body), 40)])
		}
	}
}

func TestParseIssueRef(t *testing.T) {
	for in, want := range map[string]string{
		"12": "/#12", "#12": "/#12", "erain/glue#7": "erain/glue#7",
		"https://github.com/erain/glue/issues/402": "erain/glue#402",
	} {
		o, r, n, err := ParseIssueRef(in)
		if got := o + "/" + r + "#" + itoa(n); err != nil || strings.TrimPrefix(got, "/") != strings.TrimPrefix(want, "/") {
			t.Errorf("%q = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "0", "abc", "https://github.com/erain/glue/pull/3"} {
		if _, _, _, err := ParseIssueRef(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	long := Issue{Owner: "o", Repo: "r", Number: 1, Title: "t", Body: strings.Repeat("x", MaxIssueContext)}
	if n := len([]rune(long.Context())); n != MaxIssueContext {
		t.Fatalf("context length = %d", n)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func TestPostJobsResolvesGitHubIssue(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/erain/glue-sandbox/issues/42":
			_, _ = fmt.Fprintf(w, `{"title":"Fix mobile layout","body":"Repro steps","state":"open","comments":1}`)
		case "/repos/erain/glue-sandbox/issues/42/comments":
			_, _ = w.Write([]byte(`[{"body":"latest note","user":{"login":"alice"}}]`))
		case "/repos/erain/glue-sandbox/issues/7":
			_, _ = w.Write([]byte(`{"title":"PR","state":"open","pull_request":{}}`))
		case "/repos/erain/glue-sandbox/issues/8":
			_, _ = w.Write([]byte(`{"title":"Old","state":"closed"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(gh.Close)
	t.Setenv("GLUE_GITHUB_API_URL", gh.URL)
	s, err := ResolveIssue(context.Background(), Spec{Issue: "erain/glue-sandbox#42"}, "tok")
	if err != nil || s.Repo.URL != "https://github.com/erain/glue-sandbox" || s.Objective != "Fix mobile layout" || s.Issue != "erain/glue-sandbox#42" || s.Delivery != "draft_pr" || !strings.Contains(s.Context, "@alice: latest note") {
		t.Fatalf("resolved = %+v, err = %v", s, err)
	}
	for _, bad := range []Spec{{Issue: "#42"}, {Mode: ModeInteractive, Issue: "erain/glue-sandbox#42"}, {Issue: "erain/glue-sandbox#7"}, {Issue: "erain/glue-sandbox#8"}, {Issue: "https://github.com/erain/glue-sandbox/pull/7"}} {
		if _, err := ResolveIssue(context.Background(), bad, "tok"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("spec %+v: got %v, want ErrInvalid", bad, err)
		}
	}
}
