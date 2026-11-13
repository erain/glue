// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
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
