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

func TestReadFileTool_ReadsAndTruncates(t *testing.T) {
	dir := t.TempDir()
	body := strings.Repeat("a", 200)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := ReadFileTool(ReadFileOptions{WorkDir: dir, Blocklist: Default(), MaxBytes: 50})
	res, err := tool.Execute(context.Background(), glue.ToolCall{
		Arguments: json.RawMessage(`{"path":"f.txt"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content[0].Text)
	}
	got := res.Content[0].Text
	if !strings.HasSuffix(got, "[... truncated]") {
		t.Fatalf("expected truncation marker; got %q", got)
	}
	if len(got) != 50 {
		t.Fatalf("expected len=50, got %d", len(got))
	}
}

func TestReadFileTool_BlocksSensitivePath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("S=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := ReadFileTool(ReadFileOptions{WorkDir: dir, Blocklist: Default()})
	res, err := tool.Execute(context.Background(), glue.ToolCall{
		Arguments: json.RawMessage(`{"path":".env"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("expected blocklist rejection, got success: %s", res.Content[0].Text)
	}
	if !strings.Contains(res.Content[0].Text, "blocked by sensitive-file pattern") {
		t.Fatalf("unexpected error message: %q", res.Content[0].Text)
	}
}

func TestReadFileTool_RejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	tool := ReadFileTool(ReadFileOptions{WorkDir: dir})
	res, err := tool.Execute(context.Background(), glue.ToolCall{
		Arguments: json.RawMessage(`{"path":"../escape"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("expected traversal rejection")
	}
}

func TestReadFileTool_MissingPathArg(t *testing.T) {
	tool := ReadFileTool(ReadFileOptions{WorkDir: t.TempDir()})
	res, err := tool.Execute(context.Background(), glue.ToolCall{
		Arguments: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("expected error result on missing path")
	}
}
