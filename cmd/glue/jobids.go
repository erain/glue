// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/erain/glue/jobs"
	"golang.org/x/term"
)

// fullJobID matches the generated ID shape: "job-" and 12 hex digits.
func fullJobID(s string) bool {
	h, ok := strings.CutPrefix(s, "job-")
	if !ok || len(h) != 12 {
		return false
	}
	return strings.Trim(h, "0123456789abcdef") == ""
}

// resolveJobID expands "latest" and unique job ID prefixes (with or without
// "job-") to a full ID. A full ID is returned without listing jobs.
func resolveJobID(ctx context.Context, c jobAPI, arg string) (string, error) {
	arg = strings.TrimSpace(arg)
	if fullJobID(arg) {
		return arg, nil
	}
	list, err := c.List(ctx, jobs.Filter{Limit: 1000})
	if err != nil {
		if arg == "latest" {
			return "", err
		}
		// Resolution is a convenience; the backend still checks the ID.
		return arg, nil
	}
	if arg == "latest" {
		if len(list) == 0 {
			return "", fmt.Errorf("no jobs yet")
		}
		return list[0].ID, nil
	}
	prefix := arg
	if !strings.HasPrefix(prefix, "job-") {
		prefix = "job-" + prefix
	}
	var matches []string
	for _, j := range list {
		if j.ID == arg {
			return j.ID, nil
		}
		if strings.HasPrefix(j.ID, prefix) {
			matches = append(matches, j.ID)
		}
	}
	switch len(matches) {
	case 0:
		// Unknown here; let the backend report it, since IDs may be custom.
		return arg, nil
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("job ID %q is ambiguous: %s", arg, strings.Join(matches, ", "))
	}
}

// printJobTable prints jobs as aligned columns, newest first.
func printJobTable(w io.Writer, list []jobs.Job, now time.Time, width int) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tAGE\tREPO\tOBJECTIVE")
	objWidth := max(width-70, 30)
	for _, j := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", j.ID, j.Status, shortAge(now.Sub(j.CreatedAt)), shortRepo(j.Spec.Repo), truncateRunes(oneLine(j.Spec.Objective), objWidth))
	}
	_ = tw.Flush()
}

// shortAge renders a duration as a compact age such as 45s, 12m, 3h or 2d.
func shortAge(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// shortRepo renders a repo as owner/name for URLs, or the path's last element.
func shortRepo(r jobs.RepoSpec) string {
	if r.URL != "" {
		if u, err := url.Parse(r.URL); err == nil && u.Path != "" {
			return strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
		}
		return r.URL
	}
	p := strings.TrimRight(r.Path, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// terminalWidth returns the width of w when it is a terminal, else $COLUMNS,
// else 120.
func terminalWidth(w io.Writer) int {
	if f, ok := w.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		if cols, _, err := term.GetSize(int(f.Fd())); err == nil && cols >= 40 {
			return cols
		}
	}
	var n int
	if _, err := fmt.Sscan(os.Getenv("COLUMNS"), &n); err == nil && n >= 40 {
		return n
	}
	return 120
}
