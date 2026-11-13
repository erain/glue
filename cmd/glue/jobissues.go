// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/erain/glue/jobs"
)

// issuePlaceholder stands in for the objective while the repo is resolved;
// each issue's title replaces it.
const issuePlaceholder = "(objective from GitHub issue)"

// submitIssues submits one job per GitHub issue: the title is the objective,
// the body and latest comments are the context, and delivery defaults to a
// draft PR that closes the issue. Every issue is checked before any job is
// submitted. repoGiven allows issues from a repository other than the job's.
func submitIssues(ctx context.Context, c jobAPI, base jobs.Spec, refs []string, repoGiven bool, token string, start bool, stdout, stderr io.Writer) error {
	if token == "" {
		return errors.New("--issue needs a GitHub token: set GLUE_GITHUB_TOKEN or GITHUB_TOKEN, or run gh auth login")
	}
	owner, repo, err := jobs.GitHubRepoOf(base.Repo.URL)
	if err != nil {
		return fmt.Errorf("--issue needs a GitHub repository: %w", err)
	}
	var issues []jobs.Issue
	for _, ref := range refs {
		o, r, n, err := jobs.ParseIssueRef(ref)
		if err != nil {
			return err
		}
		if o == "" {
			o, r = owner, repo
		} else if !repoGiven && (!strings.EqualFold(o, owner) || !strings.EqualFold(r, repo)) {
			return fmt.Errorf("issue %s/%s#%d is not in this checkout's repository %s/%s; pass --repo <url> to say which repository the job works in (for example --repo https://github.com/%s/%s)", o, r, n, owner, repo, o, r)
		}
		issue, err := jobs.FetchIssue(ctx, o, r, n, token)
		if err != nil {
			return fmt.Errorf("read issue %s/%s#%d: %w", o, r, n, err)
		}
		if issue.State != "open" {
			return fmt.Errorf("issue %s is %s; reopen it or give the objective directly", issue.Ref(), issue.State)
		}
		if issue.Title == "" {
			return fmt.Errorf("issue %s has no title", issue.Ref())
		}
		issues = append(issues, issue)
	}
	if base.Delivery == "" {
		base.Delivery = "draft_pr"
	}
	for i, issue := range issues {
		spec := base
		if i > 0 {
			spec.ID = ""
		}
		spec.Objective, spec.Issue = issue.Title, issue.Ref()
		spec.Context = strings.TrimSpace(issue.Context() + "\n\n" + base.Context)
		job, err := c.Submit(ctx, spec, start)
		if q := queuedError(err, job.ID); q != nil {
			err = q
		}
		if job.ID == "" && err != nil {
			return fmt.Errorf("issue %s: %w", issue.Ref(), err)
		}
		if len(issues) == 1 {
			fmt.Fprintln(stdout, job.ID)
		} else {
			fmt.Fprintf(stdout, "%s\t#%d\n", job.ID, issue.Number)
		}
		fmt.Fprintf(stderr, "%s  %s: %s (delivery %s)\n", job.ID, issue.Ref(), issue.Title, spec.Delivery)
		if err != nil {
			return fmt.Errorf("issue %s: %w", issue.Ref(), err)
		}
	}
	return nil
}
