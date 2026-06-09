package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/erain/glue"
)

func TestPathBlockedDefaults(t *testing.T) {
	t.Parallel()
	defaults := defaultBlockedPatterns()
	cases := []struct {
		path string
		want bool
	}{
		// Should block.
		{".env", true},
		{".env.local", true},
		{".env.production", true},
		{".envrc", true},
		{"id_rsa", true},
		{"id_rsa.pub", true},
		{"id_ed25519", true},
		{"server.pem", true},
		{"private.key", true},
		{"client.p12", true},
		{"credentials.json", true},
		{"service-account.json", true},
		{"service-account-prod.json", true},
		{"client-secret-foo.json", true},
		{".npmrc", true},
		{".netrc", true},
		{".pgpass", true},
		{"config/api_secret.txt", true},
		{"deploy/secrets.yaml", true},     // basename matches `secrets`
		{"deploy/secrets/foo.yaml", true}, // path component matches `secrets`
		{"infra/.aws/credentials", true},  // .aws component
		{"server.kubeconfig", true},
		{"FOO_SECRET.env", true}, // case-insensitive basename
		{".ENV", true},           // case-insensitive

		// Should NOT block (legitimate code).
		{"main.go", false},
		{"docs/README.md", false},
		{"src/handler.ts", false},
		{"environment.go", false},   // contains "env" but not the pattern
		{"keychain.go", false},      // contains "key" but not `*.key`
		{"private/notes.md", false}, // word "private" alone is not blocked
	}
	for _, c := range cases {
		got, pat := pathBlocked(c.path, defaults)
		if got != c.want {
			t.Errorf("pathBlocked(%q): got %v (pattern=%q), want %v", c.path, got, pat, c.want)
		}
	}
}

func TestPathBlockedRespectsExtras(t *testing.T) {
	t.Parallel()
	patterns := mergeBlocklist([]string{"*.token", "infra/prod/*"})
	cases := map[string]bool{
		"foo.token":          true,
		"infra/prod/db.yaml": true,
		"main.go":            false,
		// Defaults still apply on top of the merge.
		".env": true,
	}
	for path, want := range cases {
		got, _ := pathBlocked(path, patterns)
		if got != want {
			t.Errorf("with extras: pathBlocked(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestMergeBlocklistDedups(t *testing.T) {
	t.Parallel()
	got := mergeBlocklist([]string{".env", "*.token", ".env"})
	count := 0
	for _, p := range got {
		if p == ".env" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf(".env appears %d times after merge; want exactly 1", count)
	}
	// Token must appear once.
	tokenCount := 0
	for _, p := range got {
		if p == "*.token" {
			tokenCount++
		}
	}
	if tokenCount != 1 {
		t.Fatalf("*.token appears %d times; want 1", tokenCount)
	}
}

func TestSplitCommaList(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"":            nil,
		"  ":          nil,
		"a":           {"a"},
		"a,b,c":       {"a", "b", "c"},
		" a , b , c ": {"a", "b", "c"},
		"a,,b":        {"a", "b"},
	}
	for in, want := range cases {
		got := splitCommaList(in)
		if !equalStringSlice(got, want) {
			t.Errorf("splitCommaList(%q) = %+v, want %+v", in, got, want)
		}
	}
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestReadFileToolBlocksEnv(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	// Create a fake .env in the repo. read_file should refuse to open
	// it even though the file genuinely exists and is readable.
	writeFile(t, repo, ".env", "DATABASE_PASSWORD=hunter2\n")

	tool := readFileTool(repo, defaultBlockedPatterns())
	res, err := tool.Execute(context.Background(), glue.ToolCall{
		Name:      tool.Name,
		Arguments: json.RawMessage(`{"path":".env"}`),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected error result; got: %s", res.Content[0].Text)
	}
	body := res.Content[0].Text
	if !strings.Contains(body, "blocked by sensitive-file pattern") {
		t.Errorf("error message should explain blocklist hit; got %q", body)
	}
	if strings.Contains(body, "hunter2") {
		t.Errorf("error message LEAKED the env contents (%q); the blocklist must reject before reading", body)
	}
}

func TestReadFileToolBlocksUserExtras(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	writeFile(t, repo, "internal/notes.txt", "secret notes")

	// User adds 'internal/*' to the blocklist; defaults wouldn't catch it.
	tool := readFileTool(repo, mergeBlocklist([]string{"internal/*"}))
	res, _ := tool.Execute(context.Background(), glue.ToolCall{
		Name:      tool.Name,
		Arguments: json.RawMessage(`{"path":"internal/notes.txt"}`),
	})
	if !res.IsError {
		t.Fatalf("user extras should have blocked path; got: %s", res.Content[0].Text)
	}
}

func TestReadFileToolAllowsLegitimateFiles(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	writeFile(t, repo, "main.go", "package main\nfunc main(){}\n")

	tool := readFileTool(repo, defaultBlockedPatterns())
	res, err := tool.Execute(context.Background(), glue.ToolCall{
		Name:      tool.Name,
		Arguments: json.RawMessage(`{"path":"main.go"}`),
	})
	if err != nil || res.IsError {
		t.Fatalf("legitimate file should pass; err=%v isError=%v body=%q", err, res.IsError, res.Content[0].Text)
	}
	if !strings.Contains(res.Content[0].Text, "package main") {
		t.Fatalf("expected file content; got %q", res.Content[0].Text)
	}
}
