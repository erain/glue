// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// MaxPRFeedback caps the continue instructions built from a PR, in characters.
const MaxPRFeedback = 8000

// glueCommentMarker marks PR comments Glue posts, so feedback skips them.
const glueCommentMarker = "<!-- glue -->"

// PRFeedbackItem is one piece of feedback on a job's pull request.
type PRFeedbackItem struct {
	Kind   string // "review", "comment" or "check"
	Author string
	Where  string // file:line for a review thread, the check name for a check
	Text   string
	At     time.Time
}

// PRFeedback is what a pull request asks the job to address.
type PRFeedback struct {
	URL   string
	Items []PRFeedbackItem
}

var prURLRe = regexp.MustCompile(`^https://github\.com/([^/]+)/([^/]+)/pull/(\d+)`)

// FetchPRFeedback reads unresolved review threads, comments newer than since
// (except Glue's own), and failed check runs on headSHA.
func FetchPRFeedback(ctx context.Context, prURL, headSHA string, since time.Time, token string) (PRFeedback, error) {
	m := prURLRe.FindStringSubmatch(strings.TrimSpace(prURL))
	if m == nil {
		return PRFeedback{}, fmt.Errorf("%q is not a GitHub pull request URL", prURL)
	}
	owner, repo, number := m[1], m[2], m[3]
	n, _ := strconv.Atoi(number)
	api := githubAPIBase()
	fb := PRFeedback{URL: prURL}

	var gql struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						Nodes []struct {
							IsResolved bool
							Path       string
							Line       int
							Comments   struct {
								Nodes []struct {
									Body      string
									CreatedAt time.Time
									Author    struct{ Login string }
								}
							}
						}
					}
				}
			}
		}
	}
	query := `query($owner:String!,$repo:String!,$n:Int!){repository(owner:$owner,name:$repo){pullRequest(number:$n){reviewThreads(first:100){nodes{isResolved path line comments(first:20){nodes{body createdAt author{login}}}}}}}}`
	if err := githubJSON(ctx, http.MethodPost, api+"/graphql", token, map[string]any{"query": query, "variables": map[string]any{"owner": owner, "repo": repo, "n": n}}, &gql); err != nil {
		return PRFeedback{}, fmt.Errorf("read review threads: %w", err)
	}
	for _, t := range gql.Data.Repository.PullRequest.ReviewThreads.Nodes {
		if t.IsResolved || len(t.Comments.Nodes) == 0 {
			continue
		}
		var parts []string
		for _, c := range t.Comments.Nodes {
			parts = append(parts, "@"+c.Author.Login+": "+strings.TrimSpace(c.Body))
		}
		last := t.Comments.Nodes[len(t.Comments.Nodes)-1]
		where := t.Path
		if t.Line > 0 {
			where += ":" + strconv.Itoa(t.Line)
		}
		fb.Items = append(fb.Items, PRFeedbackItem{Kind: "review", Author: t.Comments.Nodes[0].Author.Login, Where: where, Text: strings.Join(parts, "\n"), At: last.CreatedAt})
	}

	var comments []struct {
		Body      string
		CreatedAt time.Time `json:"created_at"`
		User      struct{ Login string }
	}
	url := fmt.Sprintf("%s/repos/%s/%s/issues/%s/comments?per_page=100", api, owner, repo, number)
	if !since.IsZero() {
		url += "&since=" + since.UTC().Format(time.RFC3339)
	}
	if err := githubJSON(ctx, http.MethodGet, url, token, nil, &comments); err != nil {
		return PRFeedback{}, fmt.Errorf("read PR comments: %w", err)
	}
	for _, c := range comments {
		if strings.Contains(c.Body, glueCommentMarker) || c.CreatedAt.Before(since) {
			continue
		}
		fb.Items = append(fb.Items, PRFeedbackItem{Kind: "comment", Author: c.User.Login, Text: strings.TrimSpace(c.Body), At: c.CreatedAt})
	}

	if headSHA != "" {
		var runs struct {
			CheckRuns []struct {
				ID          int64
				Name        string
				Conclusion  string
				CompletedAt time.Time `json:"completed_at"`
				Output      struct{ Title, Summary string }
			} `json:"check_runs"`
		}
		if err := githubJSON(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/%s/commits/%s/check-runs?per_page=100", api, owner, repo, headSHA), token, nil, &runs); err != nil {
			return PRFeedback{}, fmt.Errorf("read check runs: %w", err)
		}
		for _, r := range runs.CheckRuns {
			if r.Conclusion != "failure" && r.Conclusion != "timed_out" {
				continue
			}
			text := strings.TrimSpace(strings.Join(slices.DeleteFunc([]string{r.Output.Title, r.Output.Summary}, func(s string) bool { return strings.TrimSpace(s) == "" }), "\n"))
			if tail := actionsLogTail(ctx, api, owner, repo, r.ID, token); tail != "" {
				text = strings.TrimSpace(text + "\nLog tail:\n" + tail)
			}
			fb.Items = append(fb.Items, PRFeedbackItem{Kind: "check", Where: r.Name, Text: cmpOr(text, r.Conclusion), At: r.CompletedAt})
		}
	}
	slices.SortStableFunc(fb.Items, func(a, b PRFeedbackItem) int { return b.At.Compare(a.At) })
	return fb, nil
}

// actionsLogTail returns the last lines of a GitHub Actions job log, or "" when
// the check run is not an Actions job or the log cannot be read.
func actionsLogTail(ctx context.Context, api, owner, repo string, jobID int64, token string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/%s/actions/jobs/%d/logs", api, owner, repo, jobID), nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	if len(lines) > 30 {
		lines = lines[len(lines)-30:]
	}
	return strings.TrimSpace(string(bytes.Join(lines, []byte("\n"))))
}

// Instructions renders the feedback as continue instructions, newest first,
// capped at MaxPRFeedback characters. It is empty when there is nothing to do.
func (f PRFeedback) Instructions() string {
	if len(f.Items) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Address this feedback on the pull request %s, then make sure the build and tests pass.", f.URL)
	for _, it := range f.Items {
		switch it.Kind {
		case "review":
			fmt.Fprintf(&b, "\n\nReview thread on %s:\n%s", it.Where, it.Text)
		case "comment":
			fmt.Fprintf(&b, "\n\nComment from @%s:\n%s", it.Author, it.Text)
		case "check":
			fmt.Fprintf(&b, "\n\nFailed check %q:\n%s", it.Where, it.Text)
		}
	}
	out := []rune(b.String())
	if len(out) > MaxPRFeedback {
		out = append(out[:MaxPRFeedback-1], '…')
	}
	return string(out)
}
