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
// escape it (".." traversal, absolute paths). The git tools shell out
// to the system `git` binary so we don't pull in a Git library just for
// `diff` / `log`.
func reviewTools(workDir string) []glue.Tool {
	return []glue.Tool{
		gitDiffBranchTool(workDir),
		gitLogBranchTool(workDir),
		readFileTool(workDir),
	}
}

const (
	defaultDiffMaxBytes = 200 * 1024
	defaultLogLimit     = 50
	defaultReadMaxBytes = 80 * 1024
)

func gitDiffBranchTool(workDir string) glue.Tool {
	return glue.Tool{
		ToolSpec: glue.ToolSpec{
			Name:        "git_diff_branch",
			Description: "Show the diff of the current branch versus a base ref (default 'main'). Includes file additions, deletions, and modifications. Use this first to scope the review.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "base": { "type": "string", "description": "Base ref to diff against. Default 'main'." },
    "max_bytes": { "type": "integer", "description": "Cap on returned diff size in bytes. Default 204800." }
  }
}`),
		},
		Execute: func(ctx context.Context, call glue.ToolCall) (glue.ToolResult, error) {
			var args struct {
				Base     string `json:"base"`
				MaxBytes int    `json:"max_bytes"`
			}
			if err := json.Unmarshal(call.Arguments, &args); err != nil {
				return glue.ToolResult{}, err
			}
			base := strings.TrimSpace(args.Base)
			if base == "" {
				base = "main"
			}
			max := args.MaxBytes
			if max <= 0 {
				max = defaultDiffMaxBytes
			}
			out, err := runGit(ctx, workDir, "diff", "--no-color", base+"...HEAD")
			if err != nil {
				return errorResult(err), nil
			}
			return textResult(truncate(out, max)), nil
		},
	}
}

func gitLogBranchTool(workDir string) glue.Tool {
	return glue.Tool{
		ToolSpec: glue.ToolSpec{
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
		Execute: func(ctx context.Context, call glue.ToolCall) (glue.ToolResult, error) {
			var args struct {
				Base  string `json:"base"`
				Limit int    `json:"limit"`
			}
			if err := json.Unmarshal(call.Arguments, &args); err != nil {
				return glue.ToolResult{}, err
			}
			base := strings.TrimSpace(args.Base)
			if base == "" {
				base = "main"
			}
			limit := args.Limit
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
				return errorResult(err), nil
			}
			return textResult(out), nil
		},
	}
}

func readFileTool(workDir string) glue.Tool {
	return glue.Tool{
		ToolSpec: glue.ToolSpec{
			Name:        "read_file",
			Description: "Read a UTF-8 text file from the working directory. Returns the file content, truncated if larger than max_bytes. Use this to inspect files mentioned in the diff when surrounding context is needed.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path":      { "type": "string", "description": "Path relative to the working directory. '..' traversal is rejected." },
    "max_bytes": { "type": "integer", "description": "Cap on returned bytes. Default 81920." }
  },
  "required": ["path"]
}`),
		},
		Execute: func(_ context.Context, call glue.ToolCall) (glue.ToolResult, error) {
			var args struct {
				Path     string `json:"path"`
				MaxBytes int    `json:"max_bytes"`
			}
			if err := json.Unmarshal(call.Arguments, &args); err != nil {
				return glue.ToolResult{}, err
			}
			resolved, err := safeJoin(workDir, args.Path)
			if err != nil {
				return errorResult(err), nil
			}
			max := args.MaxBytes
			if max <= 0 {
				max = defaultReadMaxBytes
			}
			f, err := os.Open(resolved)
			if err != nil {
				return errorResult(err), nil
			}
			defer f.Close()
			data, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
			if err != nil {
				return errorResult(err), nil
			}
			return textResult(truncate(string(data), max)), nil
		},
	}
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

func textResult(text string) glue.ToolResult {
	return glue.ToolResult{
		Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: text}},
	}
}

func errorResult(err error) glue.ToolResult {
	return glue.ToolResult{
		Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: err.Error()}},
		IsError: true,
	}
}
