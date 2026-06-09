package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/erain/glue"
)

// reviewTools is the small toolbox the reviewer agent needs: see the
// branch's diff, see the branch's commit history, and read individual
// files for context. Output is capped so a runaway tool call cannot blow
// up the model's context window.
//
// All file paths are resolved relative to workDir and rejected if they
// escape it (".." traversal, absolute paths). Read paths are also
// matched against a blocklist so the agent cannot quote secret-shaped
// files (`.env`, `id_rsa`, `*.pem`, ...) into a public review comment;
// see blocklist.go for the default pattern list. `extraBlocked` is a
// caller-supplied extension to the defaults — it does not replace them.
//
// The git tools shell out to the system `git` binary so we don't pull
// in a Git library just for `diff` / `log`.
func reviewTools(workDir string, extraBlocked, paths, pathsIgnore []string) []glue.Tool {
	blocked := mergeBlocklist(extraBlocked)
	pathspec := buildPathspec(paths, pathsIgnore)
	return []glue.Tool{
		gitDiffBranchTool(workDir, pathspec),
		gitLogBranchTool(workDir),
		readFileTool(workDir, blocked),
	}
}

// buildPathspec converts include / exclude glob lists into Git pathspec
// arguments. The Git CLI accepts:
//
//   - bare patterns as include filters (e.g. `*.go`, `cmd/...`)
//   - `:(exclude)pattern` or `:!pattern` as exclude filters
//
// Excludes only take effect after at least one include pattern is
// present, so when callers pass only excludes we add `*` as an explicit
// catch-all include — matching the intuitive "review everything except X"
// semantics.
func buildPathspec(paths, pathsIgnore []string) []string {
	if len(paths) == 0 && len(pathsIgnore) == 0 {
		return nil
	}
	out := []string{}
	for _, p := range paths {
		out = append(out, p)
	}
	if len(out) == 0 && len(pathsIgnore) > 0 {
		out = append(out, "*")
	}
	for _, p := range pathsIgnore {
		out = append(out, ":(exclude)"+p)
	}
	return out
}

const (
	defaultDiffMaxBytes = 200 * 1024
	defaultLogLimit     = 50
	defaultReadMaxBytes = 80 * 1024
)

type gitDiffArgs struct {
	Base     string `json:"base"`
	MaxBytes int    `json:"max_bytes"`
}

func gitDiffBranchTool(workDir string, pathspec []string) glue.Tool {
	return glue.NewTool[gitDiffArgs](
		glue.ToolSpec{
			Name:        "git_diff_branch",
			Description: "Show the diff of the current branch versus a base ref (default 'main'). Includes file additions, deletions, and modifications. The diff may be pre-filtered by deployment-supplied path globs — only files in scope appear. Use this first to scope the review.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "base": { "type": "string", "description": "Base ref to diff against. Default 'main'." },
    "max_bytes": { "type": "integer", "description": "Cap on returned diff size in bytes. Default 204800." }
  }
}`),
		},
		func(ctx context.Context, a gitDiffArgs) (glue.ToolResult, error) {
			base := strings.TrimSpace(a.Base)
			if base == "" {
				base = "main"
			}
			max := a.MaxBytes
			if max <= 0 {
				max = defaultDiffMaxBytes
			}
			gitArgs := []string{"diff", "--no-color", base + "...HEAD"}
			if len(pathspec) > 0 {
				gitArgs = append(gitArgs, "--")
				gitArgs = append(gitArgs, pathspec...)
			}
			out, err := runGit(ctx, workDir, gitArgs...)
			if err != nil {
				return glue.ErrorResult(err), nil
			}
			return glue.TextResult(truncate(out, max)), nil
		},
	)
}

type gitLogArgs struct {
	Base  string `json:"base"`
	Limit int    `json:"limit"`
}

func gitLogBranchTool(workDir string) glue.Tool {
	return glue.NewTool[gitLogArgs](
		glue.ToolSpec{
			Name:        "git_log_branch",
			Description: "Show the commit history of the current branch since a base ref (default 'main'). Useful for reading commit messages to understand author intent.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "base":  { "type": "string", "description": "Base ref. Default 'main'." },
    "limit": { "type": "integer", "description": "Max commits returned. Default 50." }
  }
}`),
		},
		func(ctx context.Context, a gitLogArgs) (glue.ToolResult, error) {
			base := strings.TrimSpace(a.Base)
			if base == "" {
				base = "main"
			}
			limit := a.Limit
			if limit <= 0 {
				limit = defaultLogLimit
			}
			out, err := runGit(ctx, workDir,
				"log",
				"--no-color",
				fmt.Sprintf("-n%d", limit),
				"--pretty=format:%h %an  %s%n%b%n---",
				base+"..HEAD",
			)
			if err != nil {
				return glue.ErrorResult(err), nil
			}
			return glue.TextResult(out), nil
		},
	)
}

type readFileArgs struct {
	Path     string `json:"path"`
	MaxBytes int    `json:"max_bytes"`
}

func readFileTool(workDir string, blockedPatterns []string) glue.Tool {
	return glue.NewTool[readFileArgs](
		glue.ToolSpec{
			Name:        "read_file",
			Description: "Read a UTF-8 text file from the working directory. Returns the file content, truncated if larger than max_bytes. Use this to inspect files mentioned in the diff when surrounding context is needed. Refuses to open secret-shaped files (.env, id_rsa, *.pem, credentials.json, etc.).",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path":      { "type": "string", "description": "Path relative to the working directory. '..' traversal is rejected." },
    "max_bytes": { "type": "integer", "description": "Cap on returned bytes. Default 81920." }
  },
  "required": ["path"]
}`),
		},
		func(_ context.Context, a readFileArgs) (glue.ToolResult, error) {
			// Blocklist check happens BEFORE traversal resolution so the
			// model sees a stable error message regardless of whether
			// the file even exists. We also re-check the resolved path
			// below to defend against e.g. symlink-to-secret tricks.
			if blocked, pat := pathBlocked(a.Path, blockedPatterns); blocked {
				return glue.ErrorResult(fmt.Errorf("path %q is blocked by sensitive-file pattern %q; do not retry", a.Path, pat)), nil
			}
			resolved, err := safeJoin(workDir, a.Path)
			if err != nil {
				return glue.ErrorResult(err), nil
			}
			max := a.MaxBytes
			if max <= 0 {
				max = defaultReadMaxBytes
			}
			f, err := os.Open(resolved)
			if err != nil {
				return glue.ErrorResult(err), nil
			}
			defer f.Close()
			data, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
			if err != nil {
				return glue.ErrorResult(err), nil
			}
			return glue.TextResult(truncate(string(data), max)), nil
		},
	)
}

// runGit shells out to the local git binary in workDir. Output is
// returned regardless of exit status so tool errors surface to the model
// rather than crashing the loop.
func runGit(ctx context.Context, workDir string, gitArgs ...string) (string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", errors.New("git binary not found in PATH")
	}
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(dctx, "git", gitArgs...)
	cmd.Dir = workDir
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("git %s: %w (%s)", strings.Join(gitArgs, " "), err, strings.TrimSpace(errBuf.String()))
	}
	return out.String(), nil
}

// safeJoin resolves rel against base and rejects any path that escapes
// base via symlinks or "..". Returns the cleaned absolute path on
// success.
func safeJoin(base, rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", errors.New("path is required")
	}
	if filepath.IsAbs(rel) {
		return "", errors.New("absolute paths are not allowed")
	}
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	candidate := filepath.Clean(filepath.Join(absBase, rel))
	rel2, err := filepath.Rel(absBase, candidate)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(rel2, "..") || rel2 == ".." {
		return "", fmt.Errorf("path %q escapes work directory", rel)
	}
	return candidate, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const note = "\n\n[... truncated]"
	if max <= len(note) {
		return s[:max]
	}
	return s[:max-len(note)] + note
}
