package glue

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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

	// SpoolDir, when non-empty, asks the executor to write each
	// stream's complete output to a temp file under this directory.
	// The file is kept (and named in ExecResult.StdoutSpool /
	// StderrSpool) only when that stream exceeded MaxOutputBytes;
	// otherwise it is removed. Executors may ignore this field.
	SpoolDir string
}

// ExecResult is the captured result of one command execution.
//
// When a stream exceeds the output cap, the executor keeps the head in
// Stdout/Stderr and a rolling tail in StdoutTail/StderrTail — build
// logs need both the first error and the final status. The bytes
// dropped between them are counted in StdoutOmitted/StderrOmitted.
type ExecResult struct {
	Stdout    []byte
	Stderr    []byte
	ExitCode  int
	TimedOut  bool
	Truncated bool

	// StdoutTail / StderrTail hold the kept tail of a truncated
	// stream. Nil when the stream fit within the cap.
	StdoutTail []byte
	StderrTail []byte

	// StdoutOmitted / StderrOmitted count the bytes dropped between
	// head and tail. Zero when the stream fit.
	StdoutOmitted int
	StderrOmitted int

	// StdoutLines / StderrLines are the total lines observed on each
	// stream, including dropped output.
	StdoutLines int
	StderrLines int

	// StdoutSpool / StderrSpool name temp files holding the complete
	// stream. Set only when the stream truncated and
	// ExecCommand.SpoolDir was provided.
	StdoutSpool string
	StderrSpool string
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

	stdout := newLimitedOutput(maxOutput, cmd.SpoolDir, "stdout")
	stderr := newLimitedOutput(maxOutput, cmd.SpoolDir, "stderr")
	c.Stdout = stdout
	c.Stderr = stderr

	err := c.Run()
	result := ExecResult{
		Stdout:        stdout.Head(),
		Stderr:        stderr.Head(),
		StdoutTail:    stdout.Tail(),
		StderrTail:    stderr.Tail(),
		StdoutOmitted: stdout.Omitted(),
		StderrOmitted: stderr.Omitted(),
		StdoutLines:   stdout.Lines(),
		StderrLines:   stderr.Lines(),
		StdoutSpool:   stdout.FinishSpool(),
		StderrSpool:   stderr.FinishSpool(),
		ExitCode:      exitCode(c),
		Truncated:     stdout.Truncated() || stderr.Truncated(),
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

// limitedOutput keeps the head and a rolling tail of a stream within a
// byte budget, counts lines across the whole stream, and optionally
// spools the complete stream to a temp file.
type limitedOutput struct {
	headMax int
	tailMax int
	head    bytes.Buffer
	tail    []byte
	omitted int

	newlines    int
	lastByte    byte
	wroteAny    bool
	spool       *os.File
	spoolBroken bool
}

func newLimitedOutput(limit int, spoolDir, stream string) *limitedOutput {
	tailMax := limit / 2
	w := &limitedOutput{headMax: limit - tailMax, tailMax: tailMax}
	if spoolDir != "" {
		f, err := os.CreateTemp(spoolDir, "glue-exec-*-"+stream+".log")
		if err == nil {
			w.spool = f
		}
	}
	return w
}

func (w *limitedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if n == 0 {
		return 0, nil
	}
	w.wroteAny = true
	w.newlines += bytes.Count(p, []byte{'\n'})
	w.lastByte = p[n-1]
	if w.spool != nil && !w.spoolBroken {
		if _, err := w.spool.Write(p); err != nil {
			w.spoolBroken = true
		}
	}
	if room := w.headMax - w.head.Len(); room > 0 {
		take := room
		if take > len(p) {
			take = len(p)
		}
		_, _ = w.head.Write(p[:take])
		p = p[take:]
	}
	if len(p) == 0 {
		return n, nil
	}
	// Past the head: maintain a rolling tail of at most tailMax bytes.
	if len(p) >= w.tailMax {
		w.omitted += len(w.tail) + len(p) - w.tailMax
		w.tail = append(w.tail[:0], p[len(p)-w.tailMax:]...)
		return n, nil
	}
	w.tail = append(w.tail, p...)
	if over := len(w.tail) - w.tailMax; over > 0 {
		w.omitted += over
		w.tail = append(w.tail[:0:0], w.tail[over:]...)
	}
	return n, nil
}

// Head returns the kept head of the stream. When nothing was dropped,
// the head and tail are merged so callers see one contiguous output.
func (w *limitedOutput) Head() []byte {
	if w.omitted == 0 && len(w.tail) > 0 {
		out := make([]byte, 0, w.head.Len()+len(w.tail))
		out = append(out, w.head.Bytes()...)
		return append(out, w.tail...)
	}
	return append([]byte(nil), w.head.Bytes()...)
}

// Tail returns the kept tail, nil unless bytes were dropped.
func (w *limitedOutput) Tail() []byte {
	if w.omitted == 0 {
		return nil
	}
	return append([]byte(nil), w.tail...)
}

func (w *limitedOutput) Omitted() int { return w.omitted }

// Lines is the total line count observed, including dropped output.
func (w *limitedOutput) Lines() int {
	if !w.wroteAny {
		return 0
	}
	if w.lastByte == '\n' {
		return w.newlines
	}
	return w.newlines + 1
}

func (w *limitedOutput) Truncated() bool { return w.omitted > 0 }

// FinishSpool closes the spool file and returns its path when the
// stream truncated; otherwise the file is removed and "" is returned.
func (w *limitedOutput) FinishSpool() string {
	if w.spool == nil {
		return ""
	}
	path := w.spool.Name()
	_ = w.spool.Close()
	w.spool = nil
	if w.omitted == 0 || w.spoolBroken {
		_ = os.Remove(path)
		return ""
	}
	return path
}
