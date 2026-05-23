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

func TestFileWriteToolSpecPermissionMetadata(t *testing.T) {
	tool, err := FileWrite(FileWriteOptions{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("FileWrite: %v", err)
	}
	if tool.Name != "write_file" {
		t.Fatalf("tool name = %q, want write_file", tool.Name)
	}
	if !tool.RequiresPermission {
		t.Fatal("RequiresPermission = false, want true")
	}
	if tool.PermissionAction != "write_file" {
		t.Fatalf("PermissionAction = %q, want write_file", tool.PermissionAction)
	}
	got := tool.PermissionTarget(glue.ToolCall{Arguments: json.RawMessage(`{"path":"a.go","overwrite":true}`)})
	if got != "a.go (overwrite)" {
		t.Fatalf("PermissionTarget = %q", got)
	}
}

func TestFileWriteCreatesFile(t *testing.T) {
	dir := t.TempDir()
	res := callWrite(t, FileWriteOptions{WorkDir: dir}, `{"path":"a.txt","content":"hello"}`)
	if res.IsError {
		t.Fatalf("IsError = true: %q", res.Content[0].Text)
	}
	got, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("content = %q, want hello", got)
	}
	if res.Metadata["path"] != "a.txt" || res.Metadata["bytes"] != 5 || res.Metadata["overwritten"] != false {
		t.Fatalf("metadata = %#v", res.Metadata)
	}
}

func TestFileWriteCreatesNestedParents(t *testing.T) {
	dir := t.TempDir()
	res := callWrite(t, FileWriteOptions{WorkDir: dir}, `{"path":"nested/deep/a.txt","content":"hello"}`)
	if res.IsError {
		t.Fatalf("IsError = true: %q", res.Content[0].Text)
	}
	if _, err := os.Stat(filepath.Join(dir, "nested", "deep", "a.txt")); err != nil {
		t.Fatalf("nested file not created: %v", err)
	}
}

func TestFileWriteOverwriteRequiresHostAndArg(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := callWrite(t, FileWriteOptions{WorkDir: dir}, `{"path":"a.txt","content":"new","overwrite":true}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "exists") {
		t.Fatalf("host-disallowed overwrite result = %+v", res)
	}
	res = callWrite(t, FileWriteOptions{WorkDir: dir, AllowOverwrite: true}, `{"path":"a.txt","content":"new"}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "exists") {
		t.Fatalf("arg-disallowed overwrite result = %+v", res)
	}
	res = callWrite(t, FileWriteOptions{WorkDir: dir, AllowOverwrite: true}, `{"path":"a.txt","content":"new","overwrite":true}`)
	if res.IsError {
		t.Fatalf("allowed overwrite failed: %q", res.Content[0].Text)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new" {
		t.Fatalf("content = %q, want new", got)
	}
	if res.Metadata["overwritten"] != true {
		t.Fatalf("overwritten metadata = %#v, want true", res.Metadata["overwritten"])
	}
}

func TestFileWriteValidationErrors(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		args string
		want string
	}{
		{"missing path", `{"content":"x"}`, "path is required"},
		{"absolute", `{"path":"/tmp/x","content":"x"}`, "absolute paths"},
		{"traversal", `{"path":"../x","content":"x"}`, "escapes work directory"},
		{"too large", `{"path":"a.txt","content":"abcd"}`, "exceeds max"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := callWrite(t, FileWriteOptions{WorkDir: dir, MaxBytes: 3}, tc.args)
			if !res.IsError {
				t.Fatalf("IsError = false, want true")
			}
			if !strings.Contains(res.Content[0].Text, tc.want) {
				t.Fatalf("content = %q, want %q", res.Content[0].Text, tc.want)
			}
		})
	}
}

func TestFileWriteBlocklist(t *testing.T) {
	res := callWrite(t, FileWriteOptions{WorkDir: t.TempDir(), Blocklist: Default()}, `{"path":".env","content":"SECRET=x"}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "blocked") {
		t.Fatalf("blocklist result = %+v", res)
	}
}

func TestFileWriteRejectsTargetSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlink not available: %v", err)
	}

	res := callWrite(t, FileWriteOptions{WorkDir: dir, AllowOverwrite: true}, `{"path":"link.txt","content":"new","overwrite":true}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "symlink") {
		t.Fatalf("symlink result = %+v", res)
	}
	got, _ := os.ReadFile(outside)
	if string(got) != "outside" {
		t.Fatalf("outside content changed to %q", got)
	}
}

func TestFileWriteRejectsParentSymlinkEscapeBeforeMkdir(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlink not available: %v", err)
	}

	res := callWrite(t, FileWriteOptions{WorkDir: dir}, `{"path":"link/newdir/a.txt","content":"x"}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "escapes work directory") {
		t.Fatalf("parent symlink result = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(outside, "newdir")); !os.IsNotExist(err) {
		t.Fatalf("created directory through symlink, err=%v", err)
	}
}

func TestFileWriteNoTempFilesRemainOnSuccess(t *testing.T) {
	dir := t.TempDir()
	res := callWrite(t, FileWriteOptions{WorkDir: dir}, `{"path":"a.txt","content":"x"}`)
	if res.IsError {
		t.Fatalf("IsError = true: %q", res.Content[0].Text)
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".glue-write-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temp files remain: %v", matches)
	}
}

func TestFileWriteConstructorValidation(t *testing.T) {
	if _, err := FileWrite(FileWriteOptions{}); err == nil {
		t.Fatal("empty WorkDir error = nil")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := FileWrite(FileWriteOptions{WorkDir: file}); err == nil {
		t.Fatal("file WorkDir error = nil")
	}
	if _, err := FileWrite(FileWriteOptions{WorkDir: t.TempDir(), MaxBytes: -1}); err == nil {
		t.Fatal("negative MaxBytes error = nil")
	}
}

func callWrite(t *testing.T, opts FileWriteOptions, args string) glue.ToolResult {
	t.Helper()
	tool, err := FileWrite(opts)
	if err != nil {
		t.Fatalf("FileWrite: %v", err)
	}
	res, err := tool.Execute(context.Background(), glue.ToolCall{Name: tool.Name, Arguments: json.RawMessage(args)})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res
}
