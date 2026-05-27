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

func callTool(t *testing.T, tool glue.Tool, err error, args string) glue.ToolResult {
	t.Helper()
	if err != nil {
		t.Fatalf("build tool: %v", err)
	}
	res, err := tool.Execute(context.Background(), glue.ToolCall{Name: tool.Name, Arguments: json.RawMessage(args)})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res
}

func navTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustWrite(t, dir, "main.go", "package main\nfunc main() { hello() }\n")
	mustWrite(t, dir, "util.go", "package main\nfunc hello() {}\n")
	mustWrite(t, dir, "README.md", "# title\nhello world\n")
	mustWrite(t, dir, ".env", "SECRET=top\n")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, dir, filepath.Join("sub", "deep.go"), "package sub\n// hello again\n")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, dir, filepath.Join(".git", "config"), "hello in git\n")
	return dir
}

func mustWrite(t *testing.T, dir, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestListDirHidesDotfilesAndBlocked(t *testing.T) {
	dir := navTree(t)
	tool, err := ListDirTool(NavOptions{WorkDir: dir, Blocklist: Default()})
	res := callTool(t, tool, err, `{"path":"."}`)
	if res.IsError {
		t.Fatalf("IsError: %q", res.Content[0].Text)
	}
	text := res.Content[0].Text
	if strings.Contains(text, ".env") || strings.Contains(text, ".git") {
		t.Fatalf("dotfiles leaked: %q", text)
	}
	if !strings.Contains(text, "main.go") || !strings.Contains(text, "sub/") {
		t.Fatalf("missing expected entries: %q", text)
	}

	// With all=true, .env is still hidden by the blocklist.
	res = callTool(t, tool, err, `{"path":".","all":true}`)
	if strings.Contains(res.Content[0].Text, ".env") {
		t.Fatalf("blocked .env leaked with all=true: %q", res.Content[0].Text)
	}
}

func TestFindFilesGlob(t *testing.T) {
	dir := navTree(t)
	tool, err := FindTool(NavOptions{WorkDir: dir, Blocklist: Default()})
	res := callTool(t, tool, err, `{"pattern":"*.go"}`)
	if res.IsError {
		t.Fatalf("IsError: %q", res.Content[0].Text)
	}
	text := res.Content[0].Text
	for _, want := range []string{"main.go", "util.go", "sub/deep.go"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q", want, text)
		}
	}
	if strings.Contains(text, "README.md") {
		t.Fatalf("non-matching file returned: %q", text)
	}
}

func TestFindFilesSkipsGit(t *testing.T) {
	dir := navTree(t)
	tool, err := FindTool(NavOptions{WorkDir: dir})
	res := callTool(t, tool, err, `{"pattern":"config"}`)
	if strings.Contains(res.Content[0].Text, ".git") {
		t.Fatalf(".git not skipped: %q", res.Content[0].Text)
	}
}

func TestGrepFindsMatches(t *testing.T) {
	dir := navTree(t)
	tool, err := GrepTool(NavOptions{WorkDir: dir, Blocklist: Default()})
	res := callTool(t, tool, err, `{"pattern":"hello"}`)
	if res.IsError {
		t.Fatalf("IsError: %q", res.Content[0].Text)
	}
	text := res.Content[0].Text
	if !strings.Contains(text, "main.go:") || !strings.Contains(text, "util.go:") {
		t.Fatalf("missing matches: %q", text)
	}
	// .git contents and the secret file must not appear.
	if strings.Contains(text, ".git") || strings.Contains(text, ".env") || strings.Contains(text, "SECRET") {
		t.Fatalf("grep leaked skipped path: %q", text)
	}
}

func TestGrepGlobFilter(t *testing.T) {
	dir := navTree(t)
	tool, err := GrepTool(NavOptions{WorkDir: dir})
	res := callTool(t, tool, err, `{"pattern":"hello","glob":"*.md"}`)
	text := res.Content[0].Text
	if !strings.Contains(text, "README.md:") {
		t.Fatalf("expected README match: %q", text)
	}
	if strings.Contains(text, "main.go") {
		t.Fatalf("glob filter ignored: %q", text)
	}
}

func TestGrepInvalidRegex(t *testing.T) {
	dir := navTree(t)
	tool, err := GrepTool(NavOptions{WorkDir: dir})
	res := callTool(t, tool, err, `{"pattern":"("}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "regular expression") {
		t.Fatalf("result = %+v", res)
	}
}

func TestGrepSkipsOversizeFile(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "big.txt", strings.Repeat("hello\n", 100))
	tool, err := GrepTool(NavOptions{WorkDir: dir, GrepMaxFileBytes: 10})
	res := callTool(t, tool, err, `{"pattern":"hello"}`)
	if res.Content[0].Text != "(no matches)" {
		t.Fatalf("oversize file not skipped: %q", res.Content[0].Text)
	}
}

func TestGrepResultCap(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "many.txt", strings.Repeat("hit\n", 50))
	tool, err := GrepTool(NavOptions{WorkDir: dir, MaxResults: 5})
	res := callTool(t, tool, err, `{"pattern":"hit"}`)
	if !strings.Contains(res.Content[0].Text, "truncated at 5") {
		t.Fatalf("expected truncation marker: %q", res.Content[0].Text)
	}
}

func TestNavRejectsEscape(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		tool func() (glue.Tool, error)
		args string
	}{
		{"list_dir", func() (glue.Tool, error) { return ListDirTool(NavOptions{WorkDir: dir}) }, `{"path":"../"}`},
		{"find_files", func() (glue.Tool, error) { return FindTool(NavOptions{WorkDir: dir}) }, `{"pattern":"*","path":"../"}`},
		{"grep", func() (glue.Tool, error) { return GrepTool(NavOptions{WorkDir: dir}) }, `{"pattern":"x","path":"../"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool, err := tc.tool()
			res := callTool(t, tool, err, tc.args)
			if !res.IsError || !strings.Contains(res.Content[0].Text, "escape") {
				t.Fatalf("result = %+v, want escape error", res)
			}
		})
	}
}

func TestNavRequiresPattern(t *testing.T) {
	dir := t.TempDir()
	find, err := FindTool(NavOptions{WorkDir: dir})
	if res := callTool(t, find, err, `{"pattern":""}`); !res.IsError || !strings.Contains(res.Content[0].Text, "pattern is required") {
		t.Fatalf("find result = %+v", res)
	}
	grep, err := GrepTool(NavOptions{WorkDir: dir})
	if res := callTool(t, grep, err, `{"pattern":""}`); !res.IsError || !strings.Contains(res.Content[0].Text, "pattern is required") {
		t.Fatalf("grep result = %+v", res)
	}
}

func TestNavToolsAreReadOnly(t *testing.T) {
	dir := t.TempDir()
	for _, build := range []func() (glue.Tool, error){
		func() (glue.Tool, error) { return ListDirTool(NavOptions{WorkDir: dir}) },
		func() (glue.Tool, error) { return FindTool(NavOptions{WorkDir: dir}) },
		func() (glue.Tool, error) { return GrepTool(NavOptions{WorkDir: dir}) },
	} {
		tool, err := build()
		if err != nil {
			t.Fatal(err)
		}
		if tool.RequiresPermission {
			t.Fatalf("%s requires permission, want read-only", tool.Name)
		}
	}
}
