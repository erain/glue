// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/internal/redact"
	"github.com/erain/glue/tools/coding"
)

const (
	verifyOutputTailLines = 200
	defaultGitHubAPIURL   = "https://api.github.com"
)

// Askpass implements the hidden "glue git-askpass" credential helper for GIT_ASKPASS.
func Askpass(args []string, stdout io.Writer) int {
	if len(args) > 0 && strings.HasPrefix(strings.ToLower(strings.TrimSpace(args[0])), "username") {
		fmt.Fprintln(stdout, "x-access-token")
		return 0
	}
	token := strings.TrimSpace(os.Getenv("GLUE_GITHUB_TOKEN"))
	if token == "" {
		return 1
	}
	fmt.Fprintln(stdout, token)
	return 0
}

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
	// Partial marks a canceled or errored job whose unfinished work was
	// committed (and pushed when delivery allows) so it is not lost.
	Partial bool `json:"partial,omitempty"`
	// Setup records the setup commands that ran before the agent started.
	Setup []VerifyResult `json:"setup,omitempty"`
	// BudgetExceeded names the budget that stopped a budget_limited job: "wall_clock" or "token_budget".
	BudgetExceeded string `json:"budget_exceeded,omitempty"`
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
	if job.Status == Created || job.Status == Running || job.Status == WaitingForInput || job.Status == WaitingForCredentials {
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
	res.Summary = redact.String(res.Summary)
	res.DeliveryError = redact.String(res.DeliveryError)
	for i := range res.Verify {
		res.Verify[i].OutputTail = redact.String(res.Verify[i].OutputTail)
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
	data = redact.Bytes(data)
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
	res.Setup = m.setupResults(id)
	if status == BudgetLimited {
		res.BudgetExceeded = "wall_clock"
		if job.Spec.Budgets.TokenBudget > 0 && goal.Usage.TotalTokens >= job.Spec.Budgets.TokenBudget {
			res.BudgetExceeded = "token_budget"
		}
	}
	if info, err := os.Stat(job.Workspace); job.Workspace != "" && err == nil && info.IsDir() {
		deliverable := status == Achieved || status == Blocked || status == MaxIterations || status == BudgetLimited
		if end, ok := m.lastSetup(context.Background(), id); ok && !end.OK {
			// Nothing was built; do not verify, commit, or push setup's leftovers.
			deliverable = false
		}
		if deliverable {
			if len(job.Spec.Verify) > 0 {
				if resolved, err := coding.ResolveOptions(m.codingOptions(job.ID, job.Spec, job.Workspace, job.BaseSHA)); err != nil {
					res.DeliveryError = sanitizeToken(err.Error())
				} else {
					for _, argv := range job.Spec.Verify {
						res.Verify = append(res.Verify, runVerifyCommand(context.Background(), job.Workspace, resolved.Env, resolved.ShellTimeout, argv))
					}
				}
			}
			if err := commitWorkspaceChanges(context.Background(), job, goal.Summary); err != nil && res.DeliveryError == "" {
				res.DeliveryError = sanitizeToken(err.Error())
			}
		}
		// Canceled and errored jobs keep their unfinished work on the job
		// branch. Interrupted jobs are skipped: a restart resumes them.
		partial := status == Canceled || status == Errored
		if partial {
			msg := "Unfinished work from Glue job " + job.ID + " (" + string(status) + ")"
			if err := commitWorkspaceChanges(context.Background(), job, msg); err != nil && res.DeliveryError == "" {
				res.DeliveryError = sanitizeToken(err.Error())
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
		res.Partial = partial && res.BaseSHA != "" && res.HeadSHA != "" && res.HeadSHA != res.BaseSHA
		if (deliverable || res.Partial) && res.DeliveryError == "" && (job.Spec.Delivery == "branch" || job.Spec.Delivery == "draft_pr") && res.BaseSHA != "" && res.HeadSHA != "" && res.HeadSHA != res.BaseSHA {
			if err := deliverJob(context.Background(), job, goal, &res, m.githubToken()); err != nil {
				res.DeliveryError = sanitizeToken(err.Error())
			}
		}
	}
	res.FinishedAt = m.options.Now().UTC()
	return m.writeResult(res)
}

// MaxPatchBytes caps the size of a job patch served by Manager.Patch.
var MaxPatchBytes int64 = 5 << 20

// ErrPatchTooLarge reports a job patch over MaxPatchBytes.
var ErrPatchTooLarge = errors.New("jobs: patch is too large")

// Patch returns the binary git diff from the job's base to its final commit.
func (m *Manager) Patch(ctx context.Context, id string) ([]byte, error) {
	res, err := m.Result(ctx, id)
	if err != nil {
		return nil, err
	}
	if res.BaseSHA == "" || res.HeadSHA == "" || res.BaseSHA == res.HeadSHA {
		return []byte{}, nil
	}
	job, err := m.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "git", "-C", job.Workspace, "diff", "--binary", res.BaseSHA+".."+res.HeadSHA)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(out, MaxPatchBytes+1))
	if int64(len(data)) > MaxPatchBytes {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("%w: over %d MiB; fetch the branch %s instead", ErrPatchTooLarge, MaxPatchBytes>>20, res.Branch)
	}
	if err := errors.Join(readErr, cmd.Wait()); err != nil {
		return nil, fmt.Errorf("jobs: diff %s: %w", id, err)
	}
	return data, nil
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

func deliverJob(ctx context.Context, job Job, goal glue.GoalResult, res *Result, token string) error {
	if err := pushBranch(ctx, job.Workspace, res.Branch, token); err != nil || job.Spec.Delivery != "draft_pr" || res.Status != Achieved {
		return err
	}
	// TODO(#402): Support non-GitHub forges (such as GitLab and Gitea) for draft PR delivery.
	originURL, err := workspaceGit(ctx, job.Workspace, nil, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	owner, repo, err := parseOriginRepo(originURL)
	if err != nil {
		return err
	}
	res.PRURL, err = openGitHubDraftPR(ctx, owner, repo, job, goal, *res, token)
	return err
}

func gitAuthEnv(token string) ([]string, func(), error) {
	if token = strings.TrimSpace(token); token == "" {
		return nil, func() {}, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, nil, err
	}
	f, err := os.CreateTemp("", "glue-askpass-*")
	if err != nil {
		return nil, nil, err
	}
	askpass := f.Name()
	cleanup := func() { _ = os.Remove(askpass) }
	_, writeErr := fmt.Fprintf(f, "#!/bin/sh\nexec %q git-askpass \"$@\"\n", exe)
	if err := errors.Join(writeErr, f.Close(), os.Chmod(askpass, 0o700)); err != nil {
		cleanup()
		return nil, nil, err
	}
	return []string{
		"GLUE_GITHUB_TOKEN=" + token,
		"GIT_ASKPASS=" + askpass, "SSH_ASKPASS=" + askpass, "GIT_ASKPASS_REQUIRE=force",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=",
	}, cleanup, nil
}

func pushBranch(ctx context.Context, dir, branch, token string) error {
	extra, cleanup, err := gitAuthEnv(token)
	if err != nil {
		return err
	}
	defer cleanup()
	_, err = workspaceGit(ctx, dir, extra, "push", "origin", "refs/heads/"+branch+":refs/heads/"+branch)
	return err
}

// prBase picks the branch a job's draft PR targets: the job's base ref, or
// for a snapshot job the developer's branch when origin has it; otherwise
// the default branch, never the snapshot branch itself.
func prBase(ctx context.Context, job Job, token, defaultBranch string) string {
	base := strings.TrimPrefix(strings.TrimSpace(job.Spec.Repo.BaseRef), "refs/heads/")
	if branch, _, ok := strings.Cut(job.Spec.Repo.SnapshotOf, "@"); ok {
		base = ""
		if extra, cleanup, err := gitAuthEnv(token); err == nil {
			if out, err := workspaceGit(ctx, job.Workspace, extra, "ls-remote", "--heads", "origin", branch); err == nil && strings.TrimSpace(out) != "" {
				base = branch
			}
			cleanup()
		}
	}
	if base == "" {
		base = defaultBranch
	}
	if base == "" {
		base = "main"
	}
	return base
}

func parseOriginRepo(raw string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	var parts []string
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", "", fmt.Errorf("jobs: invalid origin URL: %w", err)
		}
		parts = strings.Split(strings.Trim(u.Path, "/"), "/")
		if u.Scheme != "file" && (u.Host == "" || len(parts) != 2) {
			return "", "", fmt.Errorf("jobs: unsupported origin URL %q", raw)
		}
	} else if !strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, ".") && strings.Contains(raw, ":") {
		host, path, _ := strings.Cut(raw, ":")
		parts = strings.Split(strings.Trim(path, "/"), "/")
		if host == "" || strings.Contains(host, "/") || len(parts) != 2 {
			return "", "", fmt.Errorf("jobs: unsupported origin URL %q", raw)
		}
	} else {
		parts = strings.Split(strings.Trim(filepath.ToSlash(raw), "/"), "/")
	}
	if len(parts) < 2 {
		return "", "", fmt.Errorf("jobs: cannot parse owner/repo from origin %q", raw)
	}
	owner, repo := parts[len(parts)-2], strings.TrimSuffix(parts[len(parts)-1], ".git")
	if owner == "" || repo == "" || owner == "." || owner == ".." || repo == "." || repo == ".." {
		return "", "", fmt.Errorf("jobs: invalid owner/repo in origin %q", raw)
	}
	return owner, repo, nil
}

func openGitHubDraftPR(ctx context.Context, owner, repo string, job Job, goal glue.GoalResult, res Result, token string) (string, error) {
	apiBase := strings.TrimRight(strings.TrimSpace(os.Getenv("GLUE_GITHUB_API_URL")), "/")
	if apiBase == "" {
		apiBase = defaultGitHubAPIURL
	}
	token = strings.TrimSpace(token)
	var repoInfo struct {
		DefaultBranch string `json:"default_branch"`
		Parent        *struct {
			FullName      string `json:"full_name"`
			DefaultBranch string `json:"default_branch"`
		} `json:"parent"`
	}
	if err := githubJSON(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/%s", apiBase, owner, repo), token, nil, &repoInfo); err != nil {
		return "", err
	}
	targetRepo, head, defaultBranch := owner+"/"+repo, res.Branch, strings.TrimSpace(repoInfo.DefaultBranch)
	if repoInfo.Parent != nil && strings.TrimSpace(repoInfo.Parent.FullName) != "" {
		targetRepo, head = strings.TrimSpace(repoInfo.Parent.FullName), owner+":"+res.Branch
		if b := strings.TrimSpace(repoInfo.Parent.DefaultBranch); b != "" {
			defaultBranch = b
		}
	}
	base := prBase(ctx, job, token, defaultBranch)
	title, _, _ := strings.Cut(strings.TrimSpace(goal.Summary), "\n")
	if title == "" {
		title, _, _ = strings.Cut(strings.TrimSpace(job.Spec.Objective), "\n")
	}
	if title == "" {
		title = "Glue job " + job.ID
	}
	var prResp struct {
		HTMLURL string `json:"html_url"`
		URL     string `json:"url"`
	}
	reqBody := map[string]any{"title": title, "head": head, "base": base, "body": formatPRBody(job, goal, res), "draft": true}
	if err := githubJSON(ctx, http.MethodPost, fmt.Sprintf("%s/repos/%s/pulls", apiBase, targetRepo), token, reqBody, &prResp); err != nil {
		return "", err
	}
	if u := strings.TrimSpace(prResp.HTMLURL); u != "" {
		return u, nil
	}
	if u := strings.TrimSpace(prResp.URL); u != "" {
		return u, nil
	}
	return "", errors.New("jobs: github pull request response missing url")
}

func githubJSON(ctx context.Context, method, endpoint, token string, reqBody, respBody any) error {
	var reader io.Reader
	if reqBody != nil {
		raw, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "glue")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("jobs: github %s %s: status %d: %s", method, endpoint, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if respBody == nil {
		return nil
	}
	return json.Unmarshal(body, respBody)
}

func formatPRBody(job Job, goal glue.GoalResult, res Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Objective\n\n%s\n\n## Summary\n\n%s\n\n## Checklist\n\n", strings.TrimSpace(job.Spec.Objective), strings.TrimSpace(goal.Summary))
	for _, item := range goal.Checklist {
		mark := " "
		if item.Done {
			mark = "x"
		}
		fmt.Fprintf(&b, "- [%s] %s", mark, strings.TrimSpace(item.Title))
		if ev := strings.TrimSpace(item.Evidence); ev != "" {
			fmt.Fprintf(&b, " — %s", ev)
		}
		b.WriteByte('\n')
	}
	b.WriteString("\n## Verify\n\n")
	for _, v := range res.Verify {
		fmt.Fprintf(&b, "- `%s` (exit `%d`, `%dms`)\n", strings.Join(v.Argv, " "), v.ExitCode, v.DurationMS)
		if tail := strings.TrimSpace(v.OutputTail); tail != "" {
			fmt.Fprintf(&b, "```\n%s\n```\n", tail)
		}
	}
	fmt.Fprintf(&b, "\n## Usage\n\nIterations: %d · Input tokens: %d · Output tokens: %d · Total tokens: %d\n\nOpened by Glue job %s\n",
		res.Iterations, res.Usage.InputTokens, res.Usage.OutputTokens, res.Usage.TotalTokens, job.ID)
	return b.String()
}

func sanitizeToken(msg string) string {
	return redact.String(msg)
}
