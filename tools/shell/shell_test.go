package shell

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/erain/glue"
)

type fakeExecutor struct {
	result glue.ExecResult
	err    error

	commands []glue.ExecCommand
	stdin    []string
}

func (f *fakeExecutor) Run(_ context.Context, cmd glue.ExecCommand) (glue.ExecResult, error) {
	f.commands = append(f.commands, cmd)
	if cmd.Stdin != nil {
		data, _ := io.ReadAll(cmd.Stdin)
		f.stdin = append(f.stdin, string(data))
	} else {
		f.stdin = append(f.stdin, "")
	}
	if f.err != nil {
		return glue.ExecResult{}, f.err
	}
	return f.result, nil
}

func TestExecToolSpecPermissionMetadata(t *testing.T) {
	tool, err := Exec(ExecOptions{WorkDir: t.TempDir(), AllowedBinaries: []string{"go"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if tool.Name != ToolName {
		t.Fatalf("tool name = %q, want %q", tool.Name, ToolName)
	}
	if !tool.RequiresPermission {
		t.Fatal("RequiresPermission = false, want true")
	}
	if tool.PermissionAction != "exec" {
		t.Fatalf("PermissionAction = %q, want exec", tool.PermissionAction)
	}
	got := tool.PermissionTarget(glue.ToolCall{Arguments: json.RawMessage(`{"argv":["go","test","./..."]}`)})
	if got != "go test ./..." {
		t.Fatalf("PermissionTarget = %q, want command preview", got)
	}
}

func TestExecBuildsCommand(t *testing.T) {
	work := t.TempDir()
	fake := &fakeExecutor{result: glue.ExecResult{Stdout: []byte("ok\n"), Stderr: []byte("warn\n")}}
	tool, err := Exec(ExecOptions{
		Executor:        fake,
		WorkDir:         work,
		Env:             []string{"A=B"},
		AllowedBinaries: []string{"go"},
		Timeout:         5 * time.Second,
		MaxOutputBytes:  100,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	res := callTool(t, tool, `{"argv":["go","test","./pkg"],"dir":"sub","stdin":"input","max_output_bytes":10}`)
	if res.IsError {
		t.Fatalf("IsError = true, want false; text=%q", res.Content[0].Text)
	}
	if len(fake.commands) != 1 {
		t.Fatalf("commands = %d, want 1", len(fake.commands))
	}
	cmd := fake.commands[0]
	if !reflect.DeepEqual(cmd.Argv, []string{"go", "test", "./pkg"}) {
		t.Fatalf("argv = %#v", cmd.Argv)
	}
	if want := filepath.Join(work, "sub"); cmd.Dir != want {
		t.Fatalf("dir = %q, want %q", cmd.Dir, want)
	}
	if !reflect.DeepEqual(cmd.Env, []string{"A=B"}) {
		t.Fatalf("env = %#v", cmd.Env)
	}
	if cmd.Timeout != 5*time.Second {
		t.Fatalf("timeout = %s, want 5s", cmd.Timeout)
	}
	if cmd.MaxOutputBytes != 10 {
		t.Fatalf("max output = %d, want 10", cmd.MaxOutputBytes)
	}
	if fake.stdin[0] != "input" {
		t.Fatalf("stdin = %q, want input", fake.stdin[0])
	}
	if got := res.Metadata["stdout_bytes"]; got != 3 {
		t.Fatalf("stdout_bytes = %#v, want 3", got)
	}
}

func TestExecMaxOutputCannotRaiseHostCap(t *testing.T) {
	fake := &fakeExecutor{}
	tool, err := Exec(ExecOptions{
		Executor:        fake,
		WorkDir:         t.TempDir(),
		AllowedBinaries: []string{"go"},
		MaxOutputBytes:  10,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	callTool(t, tool, `{"argv":["go"],"max_output_bytes":100}`)
	if got := fake.commands[0].MaxOutputBytes; got != 10 {
		t.Fatalf("max output = %d, want host cap 10", got)
	}
}

func TestExecValidationErrors(t *testing.T) {
	work := t.TempDir()
	cases := []struct {
		name string
		args string
		want string
	}{
		{"missing argv", `{}`, "argv is required"},
		{"empty binary", `{"argv":[""]}`, "argv[0] is required"},
		{"path binary", `{"argv":["./go"]}`, "must be a binary basename"},
		{"disallowed binary", `{"argv":["rm","-rf","."]}`, "not allowed"},
		{"dir escape", `{"argv":["go"],"dir":"../outside"}`, "escapes work directory"},
		{"negative max output", `{"argv":["go"],"max_output_bytes":-1}`, "non-negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeExecutor{}
			tool, err := Exec(ExecOptions{Executor: fake, WorkDir: work, AllowedBinaries: []string{"go"}})
			if err != nil {
				t.Fatalf("Exec: %v", err)
			}
			res := callTool(t, tool, tc.args)
			if !res.IsError {
				t.Fatalf("IsError = false, want true")
			}
			if !strings.Contains(res.Content[0].Text, tc.want) {
				t.Fatalf("content = %q, want %q", res.Content[0].Text, tc.want)
			}
			if len(fake.commands) != 0 {
				t.Fatalf("executor called for invalid request")
			}
		})
	}
}

func TestExecConstructorValidation(t *testing.T) {
	if _, err := Exec(ExecOptions{}); err == nil {
		t.Fatal("empty WorkDir error = nil")
	}
	if _, err := Exec(ExecOptions{WorkDir: t.TempDir(), Timeout: -1}); err == nil {
		t.Fatal("negative Timeout error = nil")
	}
	if _, err := Exec(ExecOptions{WorkDir: t.TempDir(), MaxOutputBytes: -1}); err == nil {
		t.Fatal("negative MaxOutputBytes error = nil")
	}
	if _, err := Exec(ExecOptions{WorkDir: t.TempDir(), AllowedBinaries: []string{"./go"}}); err == nil {
		t.Fatal("path-shaped allowed binary error = nil")
	}
}

func TestExecNonZeroExitAndTimeoutAreErrorResults(t *testing.T) {
	nonzero := callWithFakeResult(t, glue.ExecResult{ExitCode: 2, Stdout: []byte("fail\n")})
	if !nonzero.IsError {
		t.Fatal("nonzero IsError = false, want true")
	}
	if nonzero.Metadata["exit_code"] != 2 {
		t.Fatalf("exit_code metadata = %#v, want 2", nonzero.Metadata["exit_code"])
	}

	timedOut := callWithFakeResult(t, glue.ExecResult{ExitCode: -1, TimedOut: true, Stderr: []byte("killed\n")})
	if !timedOut.IsError {
		t.Fatal("timeout IsError = false, want true")
	}
	if timedOut.Metadata["timed_out"] != true {
		t.Fatalf("timed_out metadata = %#v, want true", timedOut.Metadata["timed_out"])
	}
}

func TestExecExecutorErrorIsToolError(t *testing.T) {
	fake := &fakeExecutor{err: errors.New("spawn failed")}
	tool, err := Exec(ExecOptions{Executor: fake, WorkDir: t.TempDir(), AllowedBinaries: []string{"go"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	res := callTool(t, tool, `{"argv":["go"]}`)
	if !res.IsError {
		t.Fatal("IsError = false, want true")
	}
	if !strings.Contains(res.Content[0].Text, "spawn failed") {
		t.Fatalf("content = %q, want spawn failed", res.Content[0].Text)
	}
}

func TestExecOutputFormatting(t *testing.T) {
	res := callWithFakeResult(t, glue.ExecResult{
		Stdout:    []byte("out"),
		Stderr:    []byte("err"),
		ExitCode:  0,
		Truncated: true,
	})
	text := res.Content[0].Text
	for _, want := range []string{"exit_code: 0", "timed_out: false", "truncated: true", "stdout:\nout", "stderr:\nerr"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output %q missing %q", text, want)
		}
	}
	if res.Metadata["truncated"] != true {
		t.Fatalf("truncated metadata = %#v, want true", res.Metadata["truncated"])
	}
}

func callWithFakeResult(t *testing.T, result glue.ExecResult) glue.ToolResult {
	t.Helper()
	fake := &fakeExecutor{result: result}
	tool, err := Exec(ExecOptions{Executor: fake, WorkDir: t.TempDir(), AllowedBinaries: []string{"go"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	return callTool(t, tool, `{"argv":["go"]}`)
}

func callTool(t *testing.T, tool glue.Tool, args string) glue.ToolResult {
	t.Helper()
	res, err := tool.Execute(context.Background(), glue.ToolCall{Name: tool.Name, Arguments: json.RawMessage(args)})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res
}
