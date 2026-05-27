package coding

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/erain/glue"
)

type fakeExecutor struct {
	commands []glue.ExecCommand
	stdin    []string
}

func (f *fakeExecutor) Run(_ context.Context, cmd glue.ExecCommand) (glue.ExecResult, error) {
	f.commands = append(f.commands, cmd)
	if cmd.Stdin != nil {
		data, _ := io.ReadAll(cmd.Stdin)
		f.stdin = append(f.stdin, string(data))
	}
	return glue.ExecResult{Stdout: []byte("ok\n")}, nil
}

func TestToolsDisabledDoesNotValidate(t *testing.T) {
	tools, resolved, err := Tools(Options{WorkDir: "/does/not/exist"})
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools) != 0 {
		t.Fatalf("tools = %d, want 0", len(tools))
	}
	if resolved.WorkDir != "/does/not/exist" {
		t.Fatalf("workdir = %q", resolved.WorkDir)
	}
}

func TestToolsResolveDefaultsAndRegisterBundle(t *testing.T) {
	work := t.TempDir()
	tools, resolved, err := Tools(Options{
		Enabled:         true,
		WorkDir:         work,
		AllowedBinaries: []string{" go ", "", "go", "git"},
	})
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if !filepath.IsAbs(resolved.WorkDir) {
		t.Fatalf("workdir = %q, want absolute", resolved.WorkDir)
	}
	if !reflect.DeepEqual(resolved.AllowedBinaries, []string{"go", "git"}) {
		t.Fatalf("allowed binaries = %#v", resolved.AllowedBinaries)
	}
	if resolved.Blocklist == nil {
		t.Fatal("blocklist = nil, want default")
	}

	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	want := []string{"edit_file", "find_files", "git_diff_branch", "git_log_branch", "grep", "list_dir", "read_file", "shell_exec", "write_file"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("tool names = %#v, want %#v", names, want)
	}
}

func TestToolsUsesInjectedExecutor(t *testing.T) {
	work := t.TempDir()
	fake := &fakeExecutor{}
	tools, _, err := Tools(Options{
		Enabled:         true,
		WorkDir:         work,
		AllowedBinaries: []string{"go"},
		Executor:        fake,
		Env:             []string{"A=B"},
	})
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	shell := findTool(t, tools, "shell_exec")
	res, err := shell.Execute(context.Background(), glue.ToolCall{
		Name:      shell.Name,
		Arguments: json.RawMessage(`{"argv":["go","version"],"stdin":"hello"}`),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("shell_exec error: %s", res.Content[0].Text)
	}
	if len(fake.commands) != 1 {
		t.Fatalf("commands = %d, want 1", len(fake.commands))
	}
	if !reflect.DeepEqual(fake.commands[0].Argv, []string{"go", "version"}) {
		t.Fatalf("argv = %#v", fake.commands[0].Argv)
	}
	if !reflect.DeepEqual(fake.commands[0].Env, []string{"A=B"}) {
		t.Fatalf("env = %#v", fake.commands[0].Env)
	}
	if fake.stdin[0] != "hello" {
		t.Fatalf("stdin = %q, want hello", fake.stdin[0])
	}
}

func TestResolveOptionsRejectsInvalidWorkspace(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ResolveOptions(Options{Enabled: true, WorkDir: file})
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("ResolveOptions error = %v, want not a directory", err)
	}
}

func TestExpandPathExpandsHomeOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := ExpandPath("~/workspace")
	if err != nil {
		t.Fatalf("ExpandPath: %v", err)
	}
	if got != filepath.Join(home, "workspace") {
		t.Fatalf("~ expanded to %q, want home workspace", got)
	}
	got, err = ExpandPath("$HOME/workspace/${HOME}")
	if err != nil {
		t.Fatalf("ExpandPath HOME: %v", err)
	}
	want := home + "/workspace/" + home
	if got != want {
		t.Fatalf("HOME expanded to %q, want %q", got, want)
	}
}

func findTool(t *testing.T, tools []glue.Tool, name string) glue.Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("missing tool %q", name)
	return glue.Tool{}
}
