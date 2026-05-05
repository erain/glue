package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/erain/glue"
)

func TestBuildPathspecEmpty(t *testing.T) {
	t.Parallel()
	if got := buildPathspec(nil, nil); got != nil {
		t.Fatalf("expected nil for no filters, got %+v", got)
	}
	if got := buildPathspec([]string{}, []string{}); got != nil {
		t.Fatalf("expected nil for empty slices, got %+v", got)
	}
}

func TestBuildPathspecIncludesOnly(t *testing.T) {
	t.Parallel()
	got := buildPathspec([]string{"*.go", "cmd/**"}, nil)
	want := []string{"*.go", "cmd/**"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestBuildPathspecExcludesOnly(t *testing.T) {
	t.Parallel()
	// Excludes-only must inject a `*` catch-all include so Git's
	// pathspec semantics produce the intuitive "everything except X".
	got := buildPathspec(nil, []string{"vendor/**", "*.gen.go"})
	want := []string{"*", ":(exclude)vendor/**", ":(exclude)*.gen.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestBuildPathspecBothLists(t *testing.T) {
	t.Parallel()
	got := buildPathspec([]string{"src/**"}, []string{"src/testdata/**"})
	want := []string{"src/**", ":(exclude)src/testdata/**"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// TestGitDiffBranchToolHonorsPathspec drives the diff tool against a
// real git repo with two changed files and asserts the pathspec
// actually narrows the output. This catches regressions where the
// pathspec arg list gets fed in wrong order.
func TestGitDiffBranchToolHonorsPathspec(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	gitInit(t, repo)
	writeFile(t, repo, "main.go", "package main\nfunc main(){}\n")
	writeFile(t, repo, "secret/notes.md", "TODO: ship faster\n")
	gitCommit(t, repo, "two files", "main.go", "secret/notes.md")

	ctx := context.Background()

	// 1) No pathspec: both files appear in diff.
	tool := gitDiffBranchTool(repo, nil)
	res, err := tool.Execute(ctx, glue.ToolCall{Name: tool.Name, Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("baseline diff: %v", err)
	}
	body := res.Content[0].Text
	if !strings.Contains(body, "main.go") || !strings.Contains(body, "secret/notes.md") {
		t.Fatalf("baseline diff missing files: %s", body)
	}

	// 2) Include-only: just .go files.
	toolGo := gitDiffBranchTool(repo, buildPathspec([]string{"*.go"}, nil))
	res, _ = toolGo.Execute(ctx, glue.ToolCall{Name: toolGo.Name, Arguments: json.RawMessage(`{}`)})
	body = res.Content[0].Text
	if !strings.Contains(body, "main.go") {
		t.Errorf("include-only: main.go should appear: %s", body)
	}
	if strings.Contains(body, "secret/notes.md") {
		t.Errorf("include-only: secret/notes.md should NOT appear: %s", body)
	}

	// 3) Exclude-only: skip everything under secret/.
	toolEx := gitDiffBranchTool(repo, buildPathspec(nil, []string{"secret/*"}))
	res, _ = toolEx.Execute(ctx, glue.ToolCall{Name: toolEx.Name, Arguments: json.RawMessage(`{}`)})
	body = res.Content[0].Text
	if !strings.Contains(body, "main.go") {
		t.Errorf("exclude-only: main.go should appear: %s", body)
	}
	if strings.Contains(body, "secret/notes.md") {
		t.Errorf("exclude-only: secret/notes.md should NOT appear: %s", body)
	}
}
