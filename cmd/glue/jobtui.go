// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/erain/glue/cmd/glue/tui"
	"github.com/erain/glue/internal/axbackend"
	"github.com/erain/glue/internal/jobclient"
	"github.com/erain/glue/jobs"
)

// submitRemoteFromTUI runs the CLI submit path for /remote, so the TUI gets
// the same local-state checks, --snapshot and .glue/remote.yaml handling.
func submitRemoteFromTUI(ctx context.Context, workDir string, req tui.RemoteRequest, progress func(string)) (string, error) {
	args := []string{"submit", "--remote", "--repo", cmp.Or(workDir, ".")}
	if req.Context != "" {
		args = append(args, "--notes", req.Context)
	}
	if req.Snapshot {
		args = append(args, "--snapshot")
	}
	var stdout bytes.Buffer
	lines := &lineWriter{emit: progress}
	err := jobCommand(ctx, append(args, "--objective", req.Objective), strings.NewReader(""), &stdout, lines, nil)
	lines.flush()
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(stdout.String())
	if id == "" {
		return "", errors.New("submit printed no job ID")
	}
	return id, nil
}

// lineWriter calls emit for each complete line written to it.
type lineWriter struct {
	emit func(string)
	buf  []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		if w.emit != nil {
			w.emit(string(w.buf[:i]))
		}
		w.buf = w.buf[i+1:]
	}
}

func (w *lineWriter) flush() {
	if len(w.buf) > 0 && w.emit != nil {
		w.emit(string(w.buf))
	}
	w.buf = nil
}

// tuiJobs implements tui.JobsBridge with the glue job commands, so the TUI
// reaches the same backend (AX or a daemon) as the CLI.
type tuiJobs struct{ workDir string }

func (t tuiJobs) run(ctx context.Context, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	if err := jobCommand(ctx, args, strings.NewReader(""), &stdout, &stderr, nil); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%w: %s", err, msg)
		}
		return "", err
	}
	return stdout.String() + stderr.String(), nil
}

// List reads job records only; it never wakes a suspended task.
func (t tuiJobs) List(ctx context.Context) ([]tui.JobRow, error) {
	var stdout bytes.Buffer
	if err := jobCommand(ctx, []string{"list", "--json", "--limit", "20"}, strings.NewReader(""), &stdout, &bytes.Buffer{}, nil); err != nil {
		return nil, err
	}
	var out struct{ Jobs []jobs.Job }
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return nil, err
	}
	rows := make([]tui.JobRow, 0, len(out.Jobs))
	for _, j := range out.Jobs {
		rows = append(rows, tui.JobRow{ID: j.ID, Status: string(j.Status), Objective: j.Spec.Objective, Updated: j.UpdatedAt})
	}
	return rows, nil
}

func (t tuiJobs) Transcript(ctx context.Context, id string) (string, error) {
	return t.run(ctx, "logs", id)
}

func (t tuiJobs) Detail(ctx context.Context, id string) (out tui.JobDetail, err error) {
	c, _, err := resolveJobAPI(ctx, jobTargetOpts{conn: jobclient.Config{MetadataPath: defaultMetadataPath()}}, nil)
	if err != nil {
		return out, err
	}
	defer c.Close()
	d, err := c.Show(ctx, id)
	if err != nil {
		return out, err
	}
	out = tui.JobDetail{ID: d.ID, Status: string(d.Status), Objective: d.Spec.Objective, PRURL: d.PRURL}
	if d.Question != nil {
		out.Question = &tui.JobQuestion{ID: d.Question.ID, Question: d.Question.Question, Options: d.Question.Options}
	}
	if d.PendingPermissionCount > 0 || d.Status == jobs.WaitingForInput {
		perms, _ := c.Permissions(ctx, d.ID, "pending")
		for _, p := range perms {
			out.Permissions = append(out.Permissions, tui.JobPermission{ID: p.ID, Tool: p.Request.Tool, Action: p.Request.Action, Target: p.Request.Target})
		}
	}
	if axbackend.IsTerminal(d.Status) {
		if res, rerr := c.Result(ctx, d.ID); rerr == nil {
			out.Summary, out.PRURL = res.Summary, cmp.Or(out.PRURL, res.PRURL)
		}
	}
	return out, nil
}

func (t tuiJobs) exec(ctx context.Context, a ...string) error         { _, err := t.run(ctx, a...); return err }
func (t tuiJobs) Diff(ctx context.Context, id string) (string, error) { return t.run(ctx, "diff", id) }
func (t tuiJobs) Cancel(ctx context.Context, id string) error         { return t.exec(ctx, "cancel", id) }
func (t tuiJobs) OpenPR(ctx context.Context, id string) (string, error) {
	out, err := t.run(ctx, "pr", id)
	return strings.TrimSpace(strings.TrimPrefix(out, "draft PR:")), err
}
func (t tuiJobs) Answer(ctx context.Context, id, q, s string) error {
	return t.exec(ctx, "answer", "--question", q, "--", id, s)
}
func (t tuiJobs) DecidePermission(ctx context.Context, id, p string, ok bool, r string) error {
	if !ok {
		return t.exec(ctx, "deny", "--", id, p)
	}
	return t.exec(ctx, "approve", "--remember", r, "--", id, p)
}
func (t tuiJobs) Say(ctx context.Context, id, s string, intr bool) error {
	if intr {
		s = cmp.Or(strings.TrimSpace(s), "Interrupt current step.")
	}
	return t.exec(ctx, "say", fmt.Sprintf("--interrupt=%t", intr), "--", id, s)
}
func (t tuiJobs) Pull(ctx context.Context, id string, apply bool) (string, error) {
	return t.run(ctx, "pull", fmt.Sprintf("--apply=%t", apply), id, "--repo", cmp.Or(t.workDir, "."))
}
