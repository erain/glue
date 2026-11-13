// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/erain/glue/jobs"
)

// maxAttempts caps --attempts.
const maxAttempts = 8

func newGroupID() (string, error) {
	id, err := jobs.NewID()
	return "grp-" + strings.TrimPrefix(id, "job-"), err
}

// submitAttempts submits n jobs with one spec and a shared group. Repeated
// --model values are spread across the attempts in order. Attempts deliver to
// branches; glue job pick opens the PR for the one you keep.
func submitAttempts(ctx context.Context, c jobAPI, base jobs.Spec, n int, models []string, start bool, stdout, stderr io.Writer) error {
	group, err := newGroupID()
	if err != nil {
		return err
	}
	base.Group = group
	if base.Delivery == "" || base.Delivery == "draft_pr" {
		base.Delivery = "branch"
	}
	var queued error
	for i := range n {
		spec := base
		if i > 0 {
			spec.ID = ""
		}
		if len(models) > 0 {
			spec.Model = normalizeModel(strings.TrimSpace(models[i%len(models)]))
		}
		job, err := c.Submit(ctx, spec, start)
		if q := queuedError(err, job.ID); q != nil {
			fmt.Fprintln(stdout, job.ID)
			queued = q
			continue
		}
		if err != nil {
			return fmt.Errorf("attempt %d of %d: %w", i+1, n, err)
		}
		fmt.Fprintln(stdout, job.ID)
	}
	fmt.Fprintf(stderr, "group %s: %d attempts delivering to branches\n  compare: glue job compare %s\n  keep one: glue job pick <id>\n", group, n, group)
	return queued
}

// groupJobs returns the attempts of a group, oldest first. ref is a group ID
// or the ID of one attempt.
func groupJobs(ctx context.Context, c jobAPI, ref string) (string, []jobs.Job, error) {
	list, err := c.List(ctx, jobs.Filter{Limit: 10000})
	if err != nil {
		return "", nil, err
	}
	group := ref
	if !strings.HasPrefix(ref, "grp-") {
		i := slices.IndexFunc(list, func(j jobs.Job) bool { return j.ID == ref })
		if i < 0 || list[i].Spec.Group == "" {
			return "", nil, fmt.Errorf("job %s is not part of an attempt group", ref)
		}
		group = list[i].Spec.Group
	}
	out := filterGroup(list, group)
	if len(out) == 0 {
		return "", nil, fmt.Errorf("no jobs in group %s", group)
	}
	slices.SortStableFunc(out, func(a, b jobs.Job) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return group, out, nil
}

func filterGroup(list []jobs.Job, group string) []jobs.Job {
	return slices.DeleteFunc(slices.Clone(list), func(j jobs.Job) bool { return j.Spec.Group != group })
}

// jobCompare prints one row per attempt in a group.
func jobCompare(ctx context.Context, c jobAPI, ref string, now time.Time, stdout io.Writer) error {
	group, list, err := groupJobs(ctx, c, ref)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "group %s · %d attempts\n", group, len(list))
	w := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "JOB\tMODEL\tSTATUS\tVERIFY\tCHANGED\tTIME\tTOKENS")
	for _, j := range list {
		verify, changed, took, tokens := "-", "-", now.Sub(j.CreatedAt).Round(time.Second).String()+"…", "-"
		if res, err := c.Result(ctx, j.ID); err == nil {
			verify, changed = verifySummary(res.Verify), diffstatSummary(res.Diffstat)
			if !res.FinishedAt.IsZero() {
				took = res.FinishedAt.Sub(j.CreatedAt).Round(time.Second).String()
			}
			tokens = strconv.FormatInt(res.Usage.TotalTokens, 10)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", j.ID, cmpOrDash(j.Spec.Model), j.Status, verify, changed, took, tokens)
	}
	return w.Flush()
}

func cmpOrDash(s string) string {
	if s == "" {
		return "default"
	}
	return s
}

func verifySummary(vs []jobs.VerifyResult) string {
	if len(vs) == 0 {
		return "-"
	}
	failed := 0
	for _, v := range vs {
		if v.ExitCode != 0 {
			failed++
		}
	}
	if failed == 0 {
		return "pass"
	}
	return fmt.Sprintf("fail %d/%d", failed, len(vs))
}

var diffstatTotals = regexp.MustCompile(`(\d+) files? changed(?:, (\d+) insertions?\(\+\))?(?:, (\d+) deletions?\(-\))?`)

// diffstatSummary turns git's diffstat summary line into "3 files +20 -2".
func diffstatSummary(stat string) string {
	m := diffstatTotals.FindStringSubmatch(stat)
	if m == nil {
		return "none"
	}
	files := m[1] + " files"
	if m[1] == "1" {
		files = "1 file"
	}
	return fmt.Sprintf("%s +%s -%s", files, cmpZero(m[2]), cmpZero(m[3]))
}

func cmpZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

// jobPick opens the draft PR for one attempt and deletes the branches the
// other finished attempts pushed. Unfinished attempts are left alone.
func jobPick(ctx context.Context, c jobAPI, id, token string, stdout, stderr io.Writer) error {
	if token == "" {
		return errors.New("pick needs a GitHub token: set GLUE_GITHUB_TOKEN or GITHUB_TOKEN, or run gh auth login")
	}
	_, list, err := groupJobs(ctx, c, id)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(list, func(j jobs.Job) bool { return j.ID == id })
	if i < 0 {
		return fmt.Errorf("pick takes a job ID, not a group ID; see glue job compare %s", id)
	}
	picked := list[i]
	res, err := c.Result(ctx, picked.ID)
	if err != nil {
		return fmt.Errorf("attempt %s has no result yet: %w", picked.ID, err)
	}
	url, err := jobs.OpenDraftPR(ctx, picked, res, token)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "draft PR: %s\n", url)
	for _, o := range list {
		if o.ID == picked.ID {
			continue
		}
		r, err := c.Result(ctx, o.ID)
		switch {
		case err != nil:
			fmt.Fprintf(stderr, "left %s alone: not finished (cancel it with glue job cancel %s)\n", o.ID, o.ID)
		case r.PRURL != "":
			fmt.Fprintf(stderr, "left %s alone: it has its own PR %s\n", o.ID, r.PRURL)
		case r.Branch != "":
			if err := jobs.DeleteBranch(ctx, o.Spec.Repo.URL, r.Branch, token); err != nil {
				return fmt.Errorf("delete branch %s of %s: %w", r.Branch, o.ID, err)
			}
			fmt.Fprintf(stdout, "deleted branch %s (%s)\n", r.Branch, o.ID)
		}
	}
	return nil
}
