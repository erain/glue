package git

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/erain/glue"
	tfs "github.com/erain/glue/tools/fs"
)

// DefaultDiffMaxBytes caps the diff returned by DiffBranchTool when the
// model does not request a smaller cap.
const DefaultDiffMaxBytes = 200 * 1024

// DiffBranchOptions configures DiffBranchTool.
type DiffBranchOptions struct {
	// WorkDir is the git repo to diff in. Required.
	WorkDir string

	// Pathspec is appended after `--`; build with BuildPathspec from
	// deployment-supplied include/exclude globs. Empty means "all
	// changed files".
	Pathspec []string

	// DefaultBase is used when the tool call does not specify `base`.
	// Empty falls back to "main".
	DefaultBase string

	// MaxBytes caps the returned diff. Zero falls back to
	// DefaultDiffMaxBytes.
	MaxBytes int

	// Timeout caps a single git invocation. Zero falls back to
	// DefaultRunTimeout from RunGit.
	Timeout time.Duration
}

type diffBranchArgs struct {
	Base     string `json:"base"`
	MaxBytes int    `json:"max_bytes"`
}

// DiffBranchTool returns a glue.Tool named "git_diff_branch" that runs
// `git diff --no-color <base>...HEAD` in opts.WorkDir, optionally
// pathspec-filtered. Errors surface as error ToolResults so the model
// can recover.
func DiffBranchTool(opts DiffBranchOptions) glue.Tool {
	defaultBase := opts.DefaultBase
	if defaultBase == "" {
		defaultBase = "main"
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultDiffMaxBytes
	}

	return glue.NewTool[diffBranchArgs](
		glue.ToolSpec{
			Name:          "git_diff_branch",
			Description:   "Show the diff of the current branch versus a base ref (default 'main'). Includes file additions, deletions, and modifications. The diff may be pre-filtered by deployment-supplied path globs — only files in scope appear. Use this first to scope the review.",
			PromptSnippet: "Show the branch diff vs a base ref",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "base": { "type": "string", "description": "Base ref to diff against. Default 'main'." },
    "max_bytes": { "type": "integer", "description": "Cap on returned diff size in bytes. Default 204800." }
  }
}`),
		},
		func(ctx context.Context, a diffBranchArgs) (glue.ToolResult, error) {
			base := strings.TrimSpace(a.Base)
			if base == "" {
				base = defaultBase
			}
			limit := a.MaxBytes
			if limit <= 0 {
				limit = maxBytes
			}
			gitArgs := []string{"diff", "--no-color", base + "...HEAD"}
			if len(opts.Pathspec) > 0 {
				gitArgs = append(gitArgs, "--")
				gitArgs = append(gitArgs, opts.Pathspec...)
			}
			out, err := RunGit(ctx, RunOptions{WorkDir: opts.WorkDir, Timeout: opts.Timeout}, gitArgs...)
			if err != nil {
				return glue.ErrorResult(err), nil
			}
			return glue.TextResult(tfs.Truncate(out, limit)), nil
		},
	)
}
