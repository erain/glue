package glue

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalExecutorCapturesStdoutStderr(t *testing.T) {
	res, err := LocalExecutor{}.Run(context.Background(), ExecCommand{
		Argv: helperArgv(t, "stdout-stderr"),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := string(res.Stdout), "stdout text"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got, want := string(res.Stderr), "stderr text"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", res.ExitCode)
	}
}

func TestLocalExecutorRejectsEmptyArgv(t *testing.T) {
	_, err := LocalExecutor{}.Run(context.Background(), ExecCommand{})
	if err == nil {
		t.Fatal("Run returned nil error")
	}
}

func TestLocalExecutorReturnsSetupFailureAsError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-executable")
	res, err := LocalExecutor{}.Run(context.Background(), ExecCommand{
		Argv: []string{missing},
	})
	if err == nil {
		t.Fatal("Run returned nil error")
	}
	if res.ExitCode != -1 {
		t.Fatalf("exit code = %d, want -1", res.ExitCode)
	}
}

func TestLocalExecutorNonZeroExitIsResult(t *testing.T) {
	res, err := LocalExecutor{}.Run(context.Background(), ExecCommand{
		Argv: helperArgv(t, "exit", "7"),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 7 {
		t.Fatalf("exit code = %d, want 7", res.ExitCode)
	}
	if got, want := string(res.Stderr), "exiting 7"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func TestLocalExecutorTimeoutIsResult(t *testing.T) {
	start := time.Now()
	res, err := LocalExecutor{}.Run(context.Background(), ExecCommand{
		Argv:    helperArgv(t, "sleep", "2s"),
		Timeout: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.TimedOut {
		t.Fatal("TimedOut = false, want true")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("timeout took %s, want under 1s", elapsed)
	}
}

func TestLocalExecutorCallerCancellationIsError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := LocalExecutor{}.Run(ctx, ExecCommand{
		Argv: helperArgv(t, "sleep", "2s"),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
}

func TestLocalExecutorTruncatesEachStream(t *testing.T) {
	res, err := LocalExecutor{}.Run(context.Background(), ExecCommand{
		Argv:           helperArgv(t, "large-output"),
		MaxOutputBytes: 4,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := string(res.Stdout), "aaaa"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got, want := string(res.Stderr), "bbbb"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	if !res.Truncated {
		t.Fatal("Truncated = false, want true")
	}
}

func TestLocalExecutorStdin(t *testing.T) {
	res, err := LocalExecutor{}.Run(context.Background(), ExecCommand{
		Argv:  helperArgv(t, "stdin"),
		Stdin: strings.NewReader("hello stdin"),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := string(res.Stdout), "hello stdin"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestLocalExecutorDir(t *testing.T) {
	dir := t.TempDir()
	res, err := LocalExecutor{}.Run(context.Background(), ExecCommand{
		Argv: helperArgv(t, "cwd"),
		Dir:  dir,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := string(res.Stdout), dir; got != want {
		t.Fatalf("cwd = %q, want %q", got, want)
	}
}

func TestLocalExecutorEnvIsExact(t *testing.T) {
	t.Setenv("GLUE_EXEC_PARENT_ONLY", "secret")

	res, err := LocalExecutor{}.Run(context.Background(), ExecCommand{
		Argv: helperArgv(t, "env", "GLUE_EXEC_PARENT_ONLY"),
	})
	if err != nil {
		t.Fatalf("Run nil env: %v", err)
	}
	if got := string(res.Stdout); got != "" {
		t.Fatalf("nil Env inherited parent value %q", got)
	}

	res, err = LocalExecutor{}.Run(context.Background(), ExecCommand{
		Argv: helperArgv(t, "env", "GLUE_EXEC_PARENT_ONLY"),
		Env:  []string{"GLUE_EXEC_PARENT_ONLY=visible"},
	})
	if err != nil {
		t.Fatalf("Run explicit env: %v", err)
	}
	if got, want := string(res.Stdout), "visible"; got != want {
		t.Fatalf("env = %q, want %q", got, want)
	}
}

func helperArgv(t *testing.T, mode string, args ...string) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	argv := []string{exe, "-test.run=TestLocalExecutorHelper", "--", mode}
	return append(argv, args...)
}

func TestLocalExecutorHelper(t *testing.T) {
	idx := -1
	for i, arg := range os.Args {
		if arg == "--" {
			idx = i
			break
		}
	}
	if idx == -1 {
		return
	}
	args := os.Args[idx+1:]
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, "missing helper mode")
		os.Exit(2)
	}

	switch args[0] {
	case "stdout-stderr":
		fmt.Fprint(os.Stdout, "stdout text")
		fmt.Fprint(os.Stderr, "stderr text")
	case "exit":
		code := 1
		if len(args) > 1 {
			_, _ = fmt.Sscanf(args[1], "%d", &code)
		}
		fmt.Fprintf(os.Stderr, "exiting %d", code)
		os.Exit(code)
	case "sleep":
		d := 2 * time.Second
		if len(args) > 1 {
			parsed, err := time.ParseDuration(args[1])
			if err == nil {
				d = parsed
			}
		}
		time.Sleep(d)
	case "large-output":
		fmt.Fprint(os.Stdout, "aaaaaaaaaa")
		fmt.Fprint(os.Stderr, "bbbbbbbbb")
	case "stdin":
		_, _ = io.Copy(os.Stdout, os.Stdin)
	case "cwd":
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "getwd: %v", err)
			os.Exit(2)
		}
		fmt.Fprint(os.Stdout, wd)
	case "env":
		if len(args) < 2 {
			fmt.Fprint(os.Stderr, "missing env key")
			os.Exit(2)
		}
		fmt.Fprint(os.Stdout, os.Getenv(args[1]))
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q", args[0])
		os.Exit(2)
	}
	os.Exit(0)
}
