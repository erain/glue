// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/erain/glue"
)

func TestYoloDoesNotOverrideHostHandler(t *testing.T) {
	m, err := New(Options{StateDir: t.TempDir(), Permission: glue.DenyAll{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job, err := m.Create(ctx, Spec{Objective: "deny", Repo: RepoSpec{Path: "unused"}, Policy: "yolo"})
	if err != nil {
		t.Fatal(err)
	}
	run := &execution{ctx: ctx, cancel: cancel}
	m.active[job.ID] = run
	decision, err := (jobPermission{manager: m, id: job.ID, run: run}).Decide(ctx, glue.PermissionRequest{Tool: "write_file"})
	if err != nil || decision.Allow {
		t.Fatalf("decision %+v err %v", decision, err)
	}
	history, err := m.Permissions(ctx, job.ID, "denied")
	if err != nil || len(history) != 1 {
		t.Fatalf("history %v err %v", history, err)
	}
}

func TestPolicyEvaluate(t *testing.T) {
	ws, outside := t.TempDir(), t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(ws, "escape-file.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "escape-dir")); err != nil {
		t.Fatal(err)
	}
	wsAuto := WorkspaceAutoPolicy()
	askAll, _ := BuiltInPolicy("ask-all")
	yolo, _ := BuiltInPolicy("yolo")
	ordered := Policy{
		Default: DecisionDeny,
		Rules: []PolicyRule{
			{Name: "deny-secret", Tool: "write_file", Target: "secret/*", Decision: DecisionDeny},
			{Name: "allow-writes", Tool: "write_file", Target: "*", Decision: DecisionAllow},
			{Name: "allow-dry-push", Action: "exec", ArgvPrefix: []string{"git", "push", "--dry-run"}, Decision: DecisionAllow},
			{Name: "deny-push", Action: "exec", ArgvPrefix: []string{"git", "push"}, Decision: DecisionDeny},
		},
	}
	cases := []struct {
		name   string
		policy Policy
		req    glue.PermissionRequest
		want   Decision
	}{
		{"write root", wsAuto, glue.PermissionRequest{Tool: "write_file", Target: "README.md"}, DecisionAllow},
		{"write nested", wsAuto, glue.PermissionRequest{Tool: "write_file", Target: "pkg/sub/a.go"}, DecisionAllow},
		{"write overwrite suffix", wsAuto, glue.PermissionRequest{Tool: "write_file", Target: "README.md (overwrite)"}, DecisionAllow},
		{"edit nested", wsAuto, glue.PermissionRequest{Tool: "edit_file", Target: "internal/x.go"}, DecisionAllow},
		{"edit replace_all suffix", wsAuto, glue.PermissionRequest{Tool: "edit_file", Target: "internal/x.go (replace_all)"}, DecisionAllow},
		{"write abs inside", wsAuto, glue.PermissionRequest{Tool: "write_file", Target: filepath.Join(ws, "inside.txt")}, DecisionAllow},
		{"write dotdot escape", wsAuto, glue.PermissionRequest{Tool: "write_file", Target: "../outside.txt"}, DecisionAsk},
		{"edit nested dotdot escape", wsAuto, glue.PermissionRequest{Tool: "edit_file", Target: "a/../../outside.txt"}, DecisionAsk},
		{"write abs outside", wsAuto, glue.PermissionRequest{Tool: "write_file", Target: outsideFile}, DecisionAsk},
		{"edit symlink file escape", wsAuto, glue.PermissionRequest{Tool: "edit_file", Target: "escape-file.txt"}, DecisionAsk},
		{"write symlink dir escape", wsAuto, glue.PermissionRequest{Tool: "write_file", Target: "escape-dir/new.txt"}, DecisionAsk},
		{"exec go test", wsAuto, glue.PermissionRequest{Tool: "shell_exec", Action: "exec", Target: "go test ./...", Args: json.RawMessage(`{"argv":["go","test","./..."]}`)}, DecisionAllow},
		{"exec git status", wsAuto, glue.PermissionRequest{Tool: "shell_exec", Action: "exec", Target: "git status"}, DecisionAllow},
		{"exec git commit", wsAuto, glue.PermissionRequest{Tool: "shell_exec", Action: "exec", Target: "git commit -m fix"}, DecisionAllow},
		{"exec git local config", wsAuto, glue.PermissionRequest{Tool: "shell_exec", Action: "exec", Target: "git config user.name test"}, DecisionAllow},
		{"exec make", wsAuto, glue.PermissionRequest{Tool: "shell_exec", Action: "exec", Target: "make verify"}, DecisionAllow},
		{"exec python3", wsAuto, glue.PermissionRequest{Tool: "shell_exec", Action: "exec", Target: "python3 -V"}, DecisionAllow},
		{"exec git push denied prefix", wsAuto, glue.PermissionRequest{Tool: "shell_exec", Action: "exec", Target: "git push origin main"}, DecisionAsk},
		{"exec git remote denied prefix", wsAuto, glue.PermissionRequest{Tool: "shell_exec", Action: "exec", Target: "git remote add up https://example.com"}, DecisionAsk},
		{"exec git config global denied prefix", wsAuto, glue.PermissionRequest{Tool: "shell_exec", Action: "exec", Target: "git config --global user.name x"}, DecisionAsk},
		{"exec curl denied prefix", wsAuto, glue.PermissionRequest{Tool: "shell_exec", Action: "exec", Target: "curl https://example.com"}, DecisionAsk},
		{"exec wget denied prefix", wsAuto, glue.PermissionRequest{Tool: "shell_exec", Action: "exec", Target: "wget https://example.com"}, DecisionAsk},
		{"exec ssh denied prefix", wsAuto, glue.PermissionRequest{Tool: "shell_exec", Action: "exec", Target: "ssh host"}, DecisionAsk},
		{"exec scp denied prefix", wsAuto, glue.PermissionRequest{Tool: "shell_exec", Action: "exec", Target: "scp a host:b"}, DecisionAsk},
		{"exec unknown binary fallthrough", wsAuto, glue.PermissionRequest{Tool: "shell_exec", Action: "exec", Target: "bash -c echo"}, DecisionAsk},
		{"exec dir escape", wsAuto, glue.PermissionRequest{Tool: "shell_exec", Action: "exec", Args: json.RawMessage(`{"argv":["go","test"],"dir":"../outside"}`)}, DecisionAsk},
		{"unknown tool fallthrough", wsAuto, glue.PermissionRequest{Tool: "mcp_github_issue", Action: "mcp_call", Target: "github.issue"}, DecisionAsk},
		{"ask-all profile", askAll, glue.PermissionRequest{Tool: "write_file", Target: "README.md"}, DecisionAsk},
		{"yolo profile", yolo, glue.PermissionRequest{Tool: "shell_exec", Action: "exec", Target: "git push origin main"}, DecisionAllow},
		{"rule order deny before allow", ordered, glue.PermissionRequest{Tool: "write_file", Target: "secret/key.pem"}, DecisionDeny},
		{"rule order allow after specific deny", ordered, glue.PermissionRequest{Tool: "write_file", Target: "public/app.go"}, DecisionAllow},
		{"rule order specific prefix before general", ordered, glue.PermissionRequest{Tool: "shell_exec", Action: "exec", Target: "git push --dry-run origin"}, DecisionAllow},
		{"rule order general prefix after specific", ordered, glue.PermissionRequest{Tool: "shell_exec", Action: "exec", Target: "git push origin"}, DecisionDeny},
		{"custom default fallthrough", ordered, glue.PermissionRequest{Tool: "other_tool", Target: "x"}, DecisionDeny},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if eval := tc.policy.Evaluate(ws, tc.req); eval.Decision != tc.want || eval.Rule == "" {
				t.Fatalf("Evaluate(%+v) = %+v, want %q", tc.req, eval, tc.want)
			}
		})
	}
}

func TestPolicyFileValidation(t *testing.T) {
	dir := t.TempDir()
	for name, raw := range map[string]string{
		"empty":              ``,
		"unknown top field":  `{"unknown":true,"default":"ask"}`,
		"unknown rule field": `{"default":"ask","rules":[{"tool":"write_file","unknown":1,"decision":"allow"}]}`,
		"bad default":        `{"default":"maybe"}`,
		"missing default":    `{}`,
		"bad rule decision":  `{"default":"ask","rules":[{"tool":"write_file","decision":"never"}]}`,
		"empty decision":     `{"default":"ask","rules":[{"tool":"write_file"}]}`,
		"bad glob":           `{"default":"ask","rules":[{"tool":"write_file","target":"[unclosed","decision":"allow"}]}`,
		"empty argv token":   `{"default":"ask","rules":[{"action":"exec","argv_prefix":[""],"decision":"allow"}]}`,
		"trailing json":      `{"default":"ask"} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(dir, name+".json")
			if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadPolicyFile(p); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
	validPath := filepath.Join(dir, "valid.json")
	if err := os.WriteFile(validPath, []byte(`{"default":"ask","rules":[{"tool":"write_file","target":"*.md","decision":"allow"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadPolicyFile(validPath)
	if err != nil {
		t.Fatal(err)
	}
	if eval := loaded.Evaluate(dir, glue.PermissionRequest{Tool: "write_file", Target: "README.md"}); eval.Decision != DecisionAllow {
		t.Fatalf("eval = %+v", eval)
	}
}
