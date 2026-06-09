// Package shell provides permission-gated command execution tools for
// agents that explicitly opt into local coding workflows.
package shell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/erain/glue"
	tfs "github.com/erain/glue/tools/fs"
)

const (
	// ToolName is the model-visible name of the shell execution tool.
	ToolName = "shell_exec"

	// DefaultTimeout caps a single shell_exec call when ExecOptions.Timeout
	// is zero.
	DefaultTimeout = 30 * time.Second
)

// ExecOptions configures Exec.
type ExecOptions struct {
	// Executor runs the validated command. Nil uses glue.LocalExecutor{}.
	Executor glue.Executor

	// WorkDir is the workspace root. Required.
	WorkDir string

	// Env is the exact child environment. The model cannot add env vars.
	Env []string

	// AllowedBinaries is a basename allowlist. Empty means deny all.
	AllowedBinaries []string

	// Timeout caps each command. Zero falls back to DefaultTimeout.
	Timeout time.Duration

	// MaxOutputBytes caps stdout and stderr independently. Zero falls
	// back to glue.DefaultExecMaxOutputBytes.
	MaxOutputBytes int

	// SpoolDir, when non-empty, asks the executor to keep each
	// truncated stream's complete output in a temp file under this
	// directory; the tool result names the file so the model can read
	// back what was dropped. Empty disables spooling.
	SpoolDir string
}

type execArgs struct {
	Argv           []string `json:"argv"`
	Dir            string   `json:"dir,omitempty"`
	Stdin          string   `json:"stdin,omitempty"`
	MaxOutputBytes int      `json:"max_output_bytes,omitempty"`
}

// Exec returns a glue.Tool named "shell_exec" that runs argv-style
// commands through a glue.Executor after validating workspace and binary
// bounds. The tool is permission-gated via ToolSpec metadata.
func Exec(opts ExecOptions) (glue.Tool, error) {
	workDir := strings.TrimSpace(opts.WorkDir)
	if workDir == "" {
		return glue.Tool{}, errors.New("shell: WorkDir is required")
	}
	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return glue.Tool{}, fmt.Errorf("shell: resolve WorkDir: %w", err)
	}
	timeout := opts.Timeout
	if timeout < 0 {
		return glue.Tool{}, errors.New("shell: Timeout must be non-negative")
	}
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	maxOutput := opts.MaxOutputBytes
	if maxOutput < 0 {
		return glue.Tool{}, errors.New("shell: MaxOutputBytes must be non-negative")
	}
	if maxOutput == 0 {
		maxOutput = glue.DefaultExecMaxOutputBytes
	}
	allowed, err := allowedBinaries(opts.AllowedBinaries)
	if err != nil {
		return glue.Tool{}, err
	}
	executor := opts.Executor
	if executor == nil {
		executor = glue.LocalExecutor{}
	}
	env := append([]string(nil), opts.Env...)

	return glue.NewTool[execArgs](
		glue.ToolSpec{
			Name:          ToolName,
			Description:   "Run a bounded argv-style command in the configured workspace. Requires permission. Commands are not run through a shell; argv[0] must be an allowed binary basename.",
			PromptSnippet: "Run an allowlisted command (argv-style, no shell)",
			PromptGuidelines: []string{
				"Use shell_exec for builds and tests; long output is kept head+tail with the full stream spooled to a named temp file.",
			},
			RequiresPermission: true,
			PermissionAction:   "exec",
			PermissionTarget:   permissionTarget,
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "argv": { "type": "array", "items": { "type": "string" }, "description": "Executable basename plus arguments, e.g. [\"go\", \"test\", \"./...\"]. argv[0] must be allowlisted." },
    "dir": { "type": "string", "description": "Optional working directory relative to the configured workspace root." },
    "stdin": { "type": "string", "description": "Optional stdin text." },
    "max_output_bytes": { "type": "integer", "description": "Optional per-stream output cap. May only lower the host-configured cap." }
  },
  "required": ["argv"]
}`),
		},
		func(ctx context.Context, args execArgs) (glue.ToolResult, error) {
			cmd, err := buildCommand(args, absWorkDir, env, timeout, maxOutput, allowed)
			if err != nil {
				return glue.ErrorResult(err), nil
			}
			cmd.SpoolDir = opts.SpoolDir
			result, err := executor.Run(ctx, cmd)
			if err != nil {
				return glue.ErrorResult(err), nil
			}
			return formatResult(cmd, result), nil
		},
	), nil
}

func buildCommand(args execArgs, workDir string, env []string, timeout time.Duration, maxOutput int, allowed map[string]struct{}) (glue.ExecCommand, error) {
	if len(args.Argv) == 0 {
		return glue.ExecCommand{}, errors.New("shell: argv is required")
	}
	argv := append([]string(nil), args.Argv...)
	bin := strings.TrimSpace(argv[0])
	if bin == "" {
		return glue.ExecCommand{}, errors.New("shell: argv[0] is required")
	}
	if hasPathSeparator(bin) {
		return glue.ExecCommand{}, fmt.Errorf("shell: argv[0] %q must be a binary basename, not a path", argv[0])
	}
	if _, ok := allowed[bin]; !ok {
		return glue.ExecCommand{}, fmt.Errorf("shell: binary %q is not allowed", bin)
	}
	argv[0] = bin

	dir := workDir
	if strings.TrimSpace(args.Dir) != "" {
		resolved, err := tfs.SafeJoin(workDir, args.Dir)
		if err != nil {
			return glue.ExecCommand{}, err
		}
		dir = resolved
	}

	effectiveMax := maxOutput
	if args.MaxOutputBytes < 0 {
		return glue.ExecCommand{}, errors.New("shell: max_output_bytes must be non-negative")
	}
	if args.MaxOutputBytes > 0 && args.MaxOutputBytes < effectiveMax {
		effectiveMax = args.MaxOutputBytes
	}

	var stdin io.Reader
	if args.Stdin != "" {
		stdin = strings.NewReader(args.Stdin)
	}
	return glue.ExecCommand{
		Argv:           argv,
		Dir:            dir,
		Env:            append([]string(nil), env...),
		Stdin:          stdin,
		Timeout:        timeout,
		MaxOutputBytes: effectiveMax,
	}, nil
}

func allowedBinaries(in []string) (map[string]struct{}, error) {
	out := make(map[string]struct{}, len(in))
	for _, raw := range in {
		bin := strings.TrimSpace(raw)
		if bin == "" {
			continue
		}
		if hasPathSeparator(bin) {
			return nil, fmt.Errorf("shell: allowed binary %q must be a basename", raw)
		}
		out[bin] = struct{}{}
	}
	return out, nil
}

func hasPathSeparator(s string) bool {
	return strings.ContainsAny(s, `/\`)
}

func permissionTarget(call glue.ToolCall) string {
	var args execArgs
	if err := json.Unmarshal(call.Arguments, &args); err != nil || len(args.Argv) == 0 {
		return ToolName
	}
	return strings.Join(args.Argv, " ")
}

func formatResult(cmd glue.ExecCommand, result glue.ExecResult) glue.ToolResult {
	text := renderResult(cmd, result)
	return glue.ToolResult{
		Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: text}},
		IsError: result.ExitCode != 0 || result.TimedOut,
		Metadata: map[string]any{
			"argv":         append([]string(nil), cmd.Argv...),
			"dir":          cmd.Dir,
			"exit_code":    result.ExitCode,
			"timed_out":    result.TimedOut,
			"truncated":    result.Truncated,
			"stdout_bytes": len(result.Stdout) + len(result.StdoutTail) + result.StdoutOmitted,
			"stderr_bytes": len(result.Stderr) + len(result.StderrTail) + result.StderrOmitted,
		},
	}
}

func renderResult(cmd glue.ExecCommand, result glue.ExecResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "exit_code: %d\n", result.ExitCode)
	fmt.Fprintf(&b, "timed_out: %t\n", result.TimedOut)
	fmt.Fprintf(&b, "truncated: %t\n", result.Truncated)
	if result.TimedOut {
		fmt.Fprintf(&b, "command timed out after %s; partial output below\n", cmd.Timeout)
	}
	b.WriteByte('\n')
	writeStream(&b, "stdout", result.Stdout, result.StdoutTail, result.StdoutOmitted, result.StdoutLines, result.StdoutSpool)
	b.WriteByte('\n')
	writeStream(&b, "stderr", result.Stderr, result.StderrTail, result.StderrOmitted, result.StderrLines, result.StderrSpool)
	return b.String()
}

// writeStream renders one captured stream. A truncated stream shows the
// head and tail with a marker counting what was dropped in between —
// build logs need the first error and the final status, not just one
// end — plus the spool file holding the complete output, when kept.
func writeStream(b *strings.Builder, name string, head, tail []byte, omitted, lines int, spool string) {
	if len(head) == 0 && len(tail) == 0 {
		fmt.Fprintf(b, "%s: (empty)\n", name)
		return
	}
	if omitted == 0 {
		fmt.Fprintf(b, "%s:\n%s\n", name, string(head))
		return
	}
	fmt.Fprintf(b, "%s (%d lines total):\n%s\n", name, lines, string(head))
	keptLines := strings.Count(string(head), "\n") + strings.Count(string(tail), "\n")
	omittedLines := lines - keptLines
	if omittedLines < 0 {
		omittedLines = 0
	}
	if spool != "" {
		fmt.Fprintf(b, "[... %d bytes (~%d lines) omitted; full output: %s]\n", omitted, omittedLines, spool)
	} else {
		fmt.Fprintf(b, "[... %d bytes (~%d lines) omitted]\n", omitted, omittedLines)
	}
	fmt.Fprintf(b, "%s\n", string(tail))
}
