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
	if !strings.HasPrefix(got, strings.Repeat("a", 50)+"\n") {
		t.Fatalf("expected 50 kept bytes; got %q", got)
	}
	if !strings.Contains(got, "Line 1 is 200 bytes") || !strings.Contains(got, "exceeds the 50-byte cap") {
		t.Fatalf("expected oversized-line message; got %q", got)
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

func TestPageLinesWindowAndContinuation(t *testing.T) {
	content := "l1\nl2\nl3\nl4\nl5\n"

	if got := pageLines(content, 1, 100, 1000); got != "l1\nl2\nl3\nl4\nl5" {
		t.Fatalf("full read = %q", got)
	}

	got := pageLines(content, 2, 2, 1000)
	for _, want := range []string{"l2\nl3", "[Showing lines 2-3 of 5. Use offset=4 to continue.]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("window read %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "l4") {
		t.Fatalf("window leaked next line: %q", got)
	}

	if got := pageLines(content, 99, 10, 1000); !strings.Contains(got, "beyond the end of the file (5 lines total)") {
		t.Fatalf("offset error = %q", got)
	}
}

func TestPageLinesByteCapNeverSplitsLines(t *testing.T) {
	content := "aaaa\nbbbb\ncccc\n"
	got := pageLines(content, 1, 100, 10)
	if !strings.HasPrefix(got, "aaaa\nbbbb\n") {
		t.Fatalf("kept lines wrong: %q", got)
	}
	if strings.Contains(got, "cccc") {
		t.Fatalf("byte cap split or leaked a line: %q", got)
	}
	if !strings.Contains(got, "Use offset=3 to continue") {
		t.Fatalf("missing continuation: %q", got)
	}
}

func TestReadFileToolOffsetParam(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.txt"), []byte("x1\nx2\nx3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := ReadFileTool(ReadFileOptions{WorkDir: dir, Blocklist: Default()})
	res, err := tool.Execute(context.Background(), glue.ToolCall{
		Arguments: json.RawMessage(`{"path":"p.txt","offset":3}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := res.Content[0].Text
	if !strings.HasPrefix(got, "x3") || strings.Contains(got, "x2") {
		t.Fatalf("offset read = %q", got)
	}
	if !strings.Contains(got, "Showing lines 3-3 of 3") {
		t.Fatalf("missing range note: %q", got)
	}
}
