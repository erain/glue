package fs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erain/glue"
)

func callEdit(t *testing.T, opts EditFileOptions, args string) glue.ToolResult {
	t.Helper()
	tool, err := FileEdit(opts)
	if err != nil {
		t.Fatalf("FileEdit: %v", err)
	}
	res, err := tool.Execute(context.Background(), glue.ToolCall{Name: tool.Name, Arguments: json.RawMessage(args)})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res
}

func writeTemp(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFileEditToolSpecPermissionMetadata(t *testing.T) {
	tool, err := FileEdit(EditFileOptions{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("FileEdit: %v", err)
	}
	if tool.Name != "edit_file" {
		t.Fatalf("tool name = %q, want edit_file", tool.Name)
	}
	if !tool.RequiresPermission {
		t.Fatal("RequiresPermission = false, want true")
	}
	if tool.PermissionAction != "edit_file" {
		t.Fatalf("PermissionAction = %q, want edit_file", tool.PermissionAction)
	}
	got := tool.PermissionTarget(glue.ToolCall{Arguments: json.RawMessage(`{"path":"a.go","old_string":"x","new_string":"y","replace_all":true}`)})
	if got != "a.go (replace_all)" {
		t.Fatalf("PermissionTarget = %q", got)
	}
}

func TestFileEditUniqueReplacement(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "a.txt", "alpha beta gamma")
	res := callEdit(t, EditFileOptions{WorkDir: dir}, `{"path":"a.txt","old_string":"beta","new_string":"BETA"}`)
	if res.IsError {
		t.Fatalf("IsError = true: %q", res.Content[0].Text)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "alpha BETA gamma" {
		t.Fatalf("content = %q", got)
	}
	if res.Metadata["replacements"] != 1 {
		t.Fatalf("replacements = %v, want 1", res.Metadata["replacements"])
	}
}

func TestFileEditAmbiguousWithoutReplaceAll(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "a.txt", "x x x")
	res := callEdit(t, EditFileOptions{WorkDir: dir}, `{"path":"a.txt","old_string":"x","new_string":"y"}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "matches 3 times") {
		t.Fatalf("result = %+v", res)
	}
	// File must be untouched.
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "x x x" {
		t.Fatalf("file mutated on ambiguous edit: %q", got)
	}
}

func TestFileEditReplaceAll(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "a.txt", "x x x")
	res := callEdit(t, EditFileOptions{WorkDir: dir}, `{"path":"a.txt","old_string":"x","new_string":"y","replace_all":true}`)
	if res.IsError {
		t.Fatalf("IsError = true: %q", res.Content[0].Text)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "y y y" {
		t.Fatalf("content = %q", got)
	}
	if res.Metadata["replacements"] != 3 {
		t.Fatalf("replacements = %v, want 3", res.Metadata["replacements"])
	}
}

func TestFileEditNoMatch(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "a.txt", "hello")
	res := callEdit(t, EditFileOptions{WorkDir: dir}, `{"path":"a.txt","old_string":"absent","new_string":"y"}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "not found") {
		t.Fatalf("result = %+v", res)
	}
}

func TestFileEditRejectsEmptyAndNoop(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "a.txt", "hello")
	if res := callEdit(t, EditFileOptions{WorkDir: dir}, `{"path":"a.txt","old_string":"","new_string":"y"}`); !res.IsError || !strings.Contains(res.Content[0].Text, "non-empty") {
		t.Fatalf("empty old_string result = %+v", res)
	}
	if res := callEdit(t, EditFileOptions{WorkDir: dir}, `{"path":"a.txt","old_string":"hello","new_string":"hello"}`); !res.IsError || !strings.Contains(res.Content[0].Text, "identical") {
		t.Fatalf("noop result = %+v", res)
	}
}

func TestFileEditMissingFile(t *testing.T) {
	dir := t.TempDir()
	res := callEdit(t, EditFileOptions{WorkDir: dir}, `{"path":"nope.txt","old_string":"a","new_string":"b"}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "does not exist") {
		t.Fatalf("result = %+v", res)
	}
}

func TestFileEditRefusesDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := callEdit(t, EditFileOptions{WorkDir: dir}, `{"path":"sub","old_string":"a","new_string":"b"}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "directory") {
		t.Fatalf("result = %+v", res)
	}
}

func TestFileEditRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "real.txt", "data")
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(filepath.Join(dir, "real.txt"), link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	res := callEdit(t, EditFileOptions{WorkDir: dir}, `{"path":"link.txt","old_string":"data","new_string":"x"}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "symlink") {
		t.Fatalf("result = %+v", res)
	}
}

func TestFileEditRefusesBlockedPath(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, ".env", "SECRET=1")
	res := callEdit(t, EditFileOptions{WorkDir: dir, Blocklist: Default()}, `{"path":".env","old_string":"1","new_string":"2"}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "blocked") {
		t.Fatalf("result = %+v", res)
	}
}

func TestFileEditRefusesPathEscape(t *testing.T) {
	dir := t.TempDir()
	res := callEdit(t, EditFileOptions{WorkDir: dir}, `{"path":"../escape.txt","old_string":"a","new_string":"b"}`)
	if !res.IsError {
		t.Fatalf("result = %+v, want path-escape error", res)
	}
}

func TestFileEditOversizeFile(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "big.txt", "aaaa")
	res := callEdit(t, EditFileOptions{WorkDir: dir, MaxBytes: 2}, `{"path":"big.txt","old_string":"a","new_string":"b","replace_all":true}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "exceeds max") {
		t.Fatalf("result = %+v", res)
	}
}

func TestFileEditPreservesMode(t *testing.T) {
	dir := t.TempDir()
	path := writeTemp(t, dir, "exec.sh", "echo old")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	res := callEdit(t, EditFileOptions{WorkDir: dir}, `{"path":"exec.sh","old_string":"old","new_string":"new"}`)
	if res.IsError {
		t.Fatalf("IsError = true: %q", res.Content[0].Text)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want 0755", info.Mode().Perm())
	}
}
