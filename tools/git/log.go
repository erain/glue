package git

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/erain/glue"
)

// DefaultLogLimit caps the commit count returned by LogBranchTool when
// the model does not request a smaller limit.
const DefaultLogLimit = 50

// LogBranchOptions configures LogBranchTool.
type LogBranchOptions struct {
	// WorkDir is the git repo to read commits from. Required.
	WorkDir string

	// DefaultBase is used when the tool call does not specify `base`.
	// Empty falls back to "main".
	DefaultBase string

	// DefaultLimit is used when the tool call does not specify `limit`.
	// Zero falls back to DefaultLogLimit.
	DefaultLimit int

	// Timeout caps a single git invocation. Zero falls back to
	// DefaultRunTimeout from RunGit.
	Timeout time.Duration
}

type logBranchArgs struct {
	Base  string `json:"base"`
	Limit int    `json:"limit"`
}

// LogBranchTool returns a glue.Tool named "git_log_branch" that runs
// `git log --no-color -n<limit> <base>..HEAD` with a stable
// pretty-format. Useful for reading commit messages to understand
// author intent.
func LogBranchTool(opts LogBranchOptions) glue.Tool {
	defaultBase := opts.DefaultBase
	if defaultBase == "" {
		defaultBase = "main"
	}
	defaultLimit := opts.DefaultLimit
	if defaultLimit <= 0 {
		defaultLimit = DefaultLogLimit
	}

	return glue.NewTool[logBranchArgs](
		glue.ToolSpec{
			Name:          "git_log_branch",
			Description:   "Show the commit history of the current branch since a base ref (default 'main'). Useful for reading commit messages to understand author intent.",
			PromptSnippet: "Show branch commit history vs a base ref",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "base":  { "type": "string", "description": "Base ref. Default 'main'." },
    "limit": { "type": "integer", "description": "Max commits returned. Default 50." }
  }
}`),
		},
		func(ctx context.Context, a logBranchArgs) (glue.ToolResult, error) {
			base := strings.TrimSpace(a.Base)
			if base == "" {
				base = defaultBase
			}
			limit := a.Limit
			if limit <= 0 {
				limit = defaultLimit
			}
			out, err := RunGit(ctx, RunOptions{WorkDir: opts.WorkDir, Timeout: opts.Timeout},
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
