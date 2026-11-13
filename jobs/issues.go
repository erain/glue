// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/erain/glue"
)

// MaxIssueContext caps the issue text a job gets as context, in characters.
const MaxIssueContext = 12000

// maxIssueComments is how many of the latest comments an issue context keeps.
const maxIssueComments = 10

// Issue is a GitHub issue a job is started from.
type Issue struct {
	Owner, Repo string
	Number      int
	Title, Body string
	State       string
	Comments    []IssueComment
}

// IssueComment is one comment on an Issue.
type IssueComment struct {
	Author, Body string
}

// Ref is the issue's "owner/repo#N" reference.
func (i Issue) Ref() string { return fmt.Sprintf("%s/%s#%d", i.Owner, i.Repo, i.Number) }

var (
	issueURLRe = regexp.MustCompile(`^https://github\.com/([^/]+)/([^/]+)/issues/(\d+)/?$`)
	issueRefRe = regexp.MustCompile(`^(?:([^/\s#]+)/([^/\s#]+))?#?(\d+)$`)
)

// ParseIssueRef parses "12", "#12", "owner/repo#12" or an issue URL. Owner
// and repo are empty for a bare number.
func ParseIssueRef(s string) (owner, repo string, number int, err error) {
	s = strings.TrimSpace(s)
	m := issueURLRe.FindStringSubmatch(s)
	if m == nil {
		m = issueRefRe.FindStringSubmatch(s)
	}
	if m == nil {
		return "", "", 0, fmt.Errorf("%q is not an issue number, owner/repo#N, or issue URL", s)
	}
	n, err := strconv.Atoi(m[3])
	if err != nil || n <= 0 {
		return "", "", 0, fmt.Errorf("%q is not an issue number", s)
	}
	return m[1], m[2], n, nil
}

// GitHubRepoOf returns the owner and repo of a GitHub repository URL.
func GitHubRepoOf(url string) (owner, repo string, err error) { return parseOriginRepo(url) }

// FetchIssue reads an issue and its latest comments from GitHub. Pull
// requests are refused.
func FetchIssue(ctx context.Context, owner, repo string, number int, token string) (Issue, error) {
	base := fmt.Sprintf("%s/repos/%s/%s/issues/%d", githubAPIBase(), owner, repo, number)
	var raw struct {
		Title, Body, State string
		Comments           int
		PullRequest        *struct{} `json:"pull_request"`
	}
	if err := githubJSON(ctx, http.MethodGet, base, token, nil, &raw); err != nil {
		return Issue{}, err
	}
	issue := Issue{Owner: owner, Repo: repo, Number: number, Title: strings.TrimSpace(raw.Title), Body: strings.TrimSpace(raw.Body), State: raw.State}
	if raw.PullRequest != nil {
		return issue, fmt.Errorf("%s is a pull request, not an issue", issue.Ref())
	}
	// Comments come oldest first, 100 per page; read from the last page back.
	for page := (raw.Comments + 99) / 100; page >= 1 && len(issue.Comments) < maxIssueComments; page-- {
		var list []struct {
			Body string
			User struct{ Login string }
		}
		if err := githubJSON(ctx, http.MethodGet, fmt.Sprintf("%s/comments?per_page=100&page=%d", base, page), token, nil, &list); err != nil {
			return Issue{}, err
		}
		var got []IssueComment
		for _, c := range list {
			got = append(got, IssueComment{Author: c.User.Login, Body: strings.TrimSpace(c.Body)})
		}
		issue.Comments = append(got, issue.Comments...)
	}
	if n := len(issue.Comments); n > maxIssueComments {
		issue.Comments = issue.Comments[n-maxIssueComments:]
	}
	return issue, nil
}

// Context renders the issue body and comments as job context, capped at
// MaxIssueContext characters.
func (i Issue) Context() string {
	var b strings.Builder
	fmt.Fprintf(&b, "GitHub issue %s: %s\n\n%s", i.Ref(), i.Title, cmpOr(i.Body, "(no description)"))
	if len(i.Comments) > 0 {
		b.WriteString("\n\nLatest comments:")
		for _, c := range i.Comments {
			fmt.Fprintf(&b, "\n\n@%s: %s", c.Author, c.Body)
		}
	}
	out := []rune(b.String())
	if len(out) > MaxIssueContext {
		out = append(out[:MaxIssueContext-1], '…')
	}
	return string(out)
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// prBody is the draft PR body for a job whose branch is in prOwner/prRepo. It
// starts with a line that closes the job's issue on merge, if it has one.
func prBody(job Job, goal glue.GoalResult, res Result, prOwner, prRepo string) string {
	body := formatPRBody(job, goal, res)
	owner, repo, number, err := ParseIssueRef(job.Spec.Issue)
	if err != nil {
		return body
	}
	if owner == "" || strings.EqualFold(owner, prOwner) && strings.EqualFold(repo, prRepo) {
		return fmt.Sprintf("Closes #%d\n\n%s", number, body)
	}
	return fmt.Sprintf("Closes %s/%s#%d\n\n%s", owner, repo, number, body)
}
