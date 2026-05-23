package glue

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"
)

// DefaultExecMaxOutputBytes is the per-stream output cap used by
// LocalExecutor when ExecCommand.MaxOutputBytes is zero.
const DefaultExecMaxOutputBytes = 64 * 1024

// Executor abstracts command execution for shell-capable tools. The
// default implementation, LocalExecutor, runs commands on the local
// machine; sandboxed or remote executors can implement the same
// interface without changing tool code.
type Executor interface {
	Run(ctx context.Context, cmd ExecCommand) (ExecResult, error)
}

// ExecCommand describes one argv-style command execution request.
type ExecCommand struct {
	// Argv is the executable plus arguments. It must be non-empty and
	// is never interpreted through a shell.
	Argv []string

	// Dir, when non-empty, is the child process working directory.
	Dir string

	// Env is the exact child environment. Nil means inherit no
	// environment from the agent process.
	Env []string

	// Stdin, when non-nil, is connected to the child process stdin.
	Stdin io.Reader

	// Timeout limits this command independently from the caller's
	// context. Zero means no executor-level timeout.
	Timeout time.Duration

	// MaxOutputBytes caps stdout and stderr independently. Zero uses
	// DefaultExecMaxOutputBytes.
	MaxOutputBytes int
}

// ExecResult is the captured result of one command execution.
type ExecResult struct {
	Stdout    []byte
	Stderr    []byte
	ExitCode  int
	TimedOut  bool
	Truncated bool
}

// LocalExecutor runs commands on the local machine with os/exec. It is
// intentionally not a sandbox.
type LocalExecutor struct{}

var _ Executor = LocalExecutor{}

// Run implements Executor.
func (LocalExecutor) Run(ctx context.Context, cmd ExecCommand) (ExecResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(cmd.Argv) == 0 || cmd.Argv[0] == "" {
		return ExecResult{ExitCode: -1}, errors.New("glue: exec argv is required")
	}
	if cmd.Timeout < 0 {
		return ExecResult{ExitCode: -1}, errors.New("glue: exec timeout must be non-negative")
	}
	if cmd.MaxOutputBytes < 0 {
		return ExecResult{ExitCode: -1}, errors.New("glue: exec max output bytes must be non-negative")
	}

	maxOutput := cmd.MaxOutputBytes
	if maxOutput == 0 {
		maxOutput = DefaultExecMaxOutputBytes
	}

	runCtx := ctx
	cancel := func() {}
	if cmd.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, cmd.Timeout)
	}
	defer cancel()

	c := exec.CommandContext(runCtx, cmd.Argv[0], cmd.Argv[1:]...)
	if cmd.Dir != "" {
		c.Dir = cmd.Dir
	}
	if cmd.Env == nil {
		c.Env = []string{}
	} else {
		c.Env = append([]string(nil), cmd.Env...)
	}
	if cmd.Stdin != nil {
		c.Stdin = cmd.Stdin
	}

	stdout := &limitedOutput{limit: maxOutput}
	stderr := &limitedOutput{limit: maxOutput}
	c.Stdout = stdout
	c.Stderr = stderr

	err := c.Run()
	result := ExecResult{
		Stdout:    stdout.Bytes(),
		Stderr:    stderr.Bytes(),
		ExitCode:  exitCode(c),
		Truncated: stdout.Truncated() || stderr.Truncated(),
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, ctxErr
	}
	if cmd.Timeout > 0 && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		result.TimedOut = true
		if result.ExitCode == 0 {
			result.ExitCode = -1
		}
		return result, nil
	}
	if err == nil {
		return result, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	return result, fmt.Errorf("glue: exec %q: %w", cmd.Argv[0], err)
}

func exitCode(c *exec.Cmd) int {
	if c.ProcessState == nil {
		return -1
	}
	return c.ProcessState.ExitCode()
}

type limitedOutput struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (w *limitedOutput) Write(p []byte) (int, error) {
	originalLen := len(p)
	if originalLen == 0 {
		return 0, nil
	}
	remaining := w.limit - w.buf.Len()
	if remaining <= 0 {
		w.truncated = true
		return originalLen, nil
	}
	if len(p) > remaining {
		w.truncated = true
		p = p[:remaining]
	}
	_, _ = w.buf.Write(p)
	return originalLen, nil
}

func (w *limitedOutput) Bytes() []byte {
	return append([]byte(nil), w.buf.Bytes()...)
}

func (w *limitedOutput) Truncated() bool {
	return w.truncated
}
