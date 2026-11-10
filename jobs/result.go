// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/tools/coding"
)

const verifyOutputTailLines = 200

// VerifyResult records one post-goal verification command execution.
type VerifyResult struct {
	Argv       []string `json:"argv"`
	ExitCode   int      `json:"exit_code"`
	DurationMS int64    `json:"duration_ms"`
	OutputTail string   `json:"output_tail"`
}

// Result is the durable completion record persisted to <state>/jobs/<id>/result.json.
type Result struct {
	JobID         string         `json:"job_id"`
	Status        Status         `json:"status"`
	Objective     string         `json:"objective"`
	BaseRef       string         `json:"base_ref"`
	BaseSHA       string         `json:"base_sha"`
	HeadSHA       string         `json:"head_sha"`
	Branch        string         `json:"branch"`
	PRURL         string         `json:"pr_url"`
	Diffstat      string         `json:"diffstat"`
	Verify        []VerifyResult `json:"verify"`
	Iterations    int            `json:"iterations"`
	Usage         glue.Usage     `json:"usage"`
	Summary       string         `json:"summary"`
	FinishedAt    time.Time      `json:"finished_at"`
	DeliveryError string         `json:"delivery_error"`
}

// Result reads the durable completion record for a finished job.
func (m *Manager) Result(ctx context.Context, id string) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	job, err := m.Get(ctx, id)
	if err != nil {
		return Result{}, err
	}
	if job.Status == Created || job.Status == Running || job.Status == WaitingForInput {
		return Result{}, fmt.Errorf("%w: job is not finished", ErrConflict)
	}
	data, err := os.ReadFile(m.resultPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Result{}, fmt.Errorf("%w: result is not available", ErrConflict)
		}
		return Result{}, err
	}
	var res Result
	if err := json.Unmarshal(data, &res); err != nil {
		return Result{}, err
	}
	if res.Verify == nil {
		res.Verify = []VerifyResult{}
	}
	return res, nil
}

func (m *Manager) resultPath(id string) string {
	return filepath.Join(m.options.StateDir, "jobs", id, "result.json")
}

func (m *Manager) writeResult(res Result) error {
	if res.Verify == nil {
		res.Verify = []VerifyResult{}
	}
	path := m.resultPath(res.JobID)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (m *Manager) finalize(id string, goal glue.GoalResult, status Status) error {
	job, err := m.Get(context.Background(), id)
	if err != nil {
		return err
	}
	res := Result{
		JobID: job.ID, Status: status, Objective: job.Spec.Objective,
		BaseRef: job.Spec.Repo.BaseRef, BaseSHA: job.BaseSHA, Branch: "glue/" + job.ID,
		Verify: []VerifyResult{}, Iterations: goal.Iterations, Usage: goal.Usage, Summary: goal.Summary,
	}
	if info, err := os.Stat(job.Workspace); job.Workspace != "" && err == nil && info.IsDir() {
		if status == Achieved || status == Blocked || status == MaxIterations || status == BudgetLimited {
			if len(job.Spec.Verify) > 0 {
				if resolved, err := coding.ResolveOptions(m.codingOptions(job.ID, job.Spec, job.Workspace, job.BaseSHA)); err != nil {
					res.DeliveryError = err.Error()
				} else {
					for _, argv := range job.Spec.Verify {
						res.Verify = append(res.Verify, runVerifyCommand(context.Background(), job.Workspace, resolved.Env, resolved.ShellTimeout, argv))
					}
				}
			}
			if err := commitWorkspaceChanges(context.Background(), job, goal.Summary); err != nil && res.DeliveryError == "" {
				res.DeliveryError = err.Error()
			}
		}
		if head, err := workspaceGit(context.Background(), job.Workspace, nil, "rev-parse", "HEAD"); err == nil {
			res.HeadSHA = head
		}
		if res.BaseSHA != "" && res.HeadSHA != "" {
			if diffstat, err := workspaceGit(context.Background(), job.Workspace, nil, "diff", "--stat", res.BaseSHA+".."+res.HeadSHA); err == nil {
				res.Diffstat = diffstat
			}
		}
	}
	res.FinishedAt = m.options.Now().UTC()
	return m.writeResult(res)
}

func runVerifyCommand(ctx context.Context, dir string, env []string, timeout time.Duration, argv []string) VerifyResult {
	if timeout <= 0 {
		timeout = coding.DefaultShellTimeout
	}
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, resolveVerifyExecutable(argv[0], env), argv[1:]...)
	cmd.Dir, cmd.Env = dir, env
	started := time.Now()
	out, err := cmd.CombinedOutput()
	durationMS := time.Since(started).Milliseconds()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		if exitCode == 0 {
			exitCode = -1
		}
		if len(out) == 0 {
			out = []byte(err.Error())
		}
	}
	return VerifyResult{Argv: slices.Clone(argv), ExitCode: exitCode, DurationMS: durationMS, OutputTail: tailLines(string(out), verifyOutputTailLines)}
}

func resolveVerifyExecutable(bin string, env []string) string {
	if filepath.IsAbs(bin) || strings.ContainsRune(bin, filepath.Separator) {
		return bin
	}
	for _, entry := range env {
		if key, val, ok := strings.Cut(entry, "="); ok && key == "PATH" {
			for _, dir := range filepath.SplitList(val) {
				candidate := filepath.Join(dir, bin)
				if info, err := os.Stat(candidate); dir != "" && err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
					return candidate
				}
			}
		}
	}
	return bin
}

func tailLines(s string, maxLines int) string {
	s = strings.TrimSuffix(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	if s == "" || maxLines <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n")
}

func commitWorkspaceChanges(ctx context.Context, job Job, summary string) error {
	status, err := workspaceGit(ctx, job.Workspace, nil, "status", "--porcelain=v1")
	if err != nil || status == "" {
		return err
	}
	if _, err := workspaceGit(ctx, job.Workspace, nil, "add", "-A"); err != nil {
		return err
	}
	msg := strings.TrimSpace(summary)
	if msg == "" {
		msg = strings.TrimSpace(job.Spec.Objective)
	}
	if msg == "" {
		msg = "Glue job " + job.ID
	}
	name, email := strings.TrimSpace(os.Getenv("GLUE_GIT_AUTHOR_NAME")), strings.TrimSpace(os.Getenv("GLUE_GIT_AUTHOR_EMAIL"))
	if name == "" {
		if v, err := workspaceGit(ctx, job.Workspace, nil, "config", "user.name"); err != nil || v == "" {
			name = "Glue"
		}
	}
	if email == "" {
		if v, err := workspaceGit(ctx, job.Workspace, nil, "config", "user.email"); err != nil || v == "" {
			email = "glue@localhost"
		}
	}
	var extra []string
	if name != "" {
		extra = append(extra, "GIT_AUTHOR_NAME="+name, "GIT_COMMITTER_NAME="+name)
	}
	if email != "" {
		extra = append(extra, "GIT_AUTHOR_EMAIL="+email, "GIT_COMMITTER_EMAIL="+email)
	}
	_, err = workspaceGit(ctx, job.Workspace, extra, "commit", "-qm", msg)
	return err
}

func workspaceGit(ctx context.Context, dir string, extraEnv []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("jobs: git %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
