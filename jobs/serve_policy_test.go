// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

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
	autoDefault, err := BuiltInPolicy("")
	if err != nil || autoDefault.Name != "auto" || autoDefault.Default != DecisionAllow || len(autoDefault.Rules) != 0 {
		t.Fatalf("BuiltInPolicy(\"\") = %+v, err = %v", autoDefault, err)
	}
	autoNamed, err := BuiltInPolicy("auto")
	if err != nil || autoNamed.Name != "auto" || autoNamed.Default != DecisionAllow || len(autoNamed.Rules) != 0 {
		t.Fatalf("BuiltInPolicy(\"auto\") = %+v, err = %v", autoNamed, err)
	}
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
		{"auto default profile", autoDefault, glue.PermissionRequest{Tool: "shell_exec", Action: "exec", Target: "bash -c 'rg foo | head'"}, DecisionAllow},
		{"auto named profile", autoNamed, glue.PermissionRequest{Tool: "shell_exec", Action: "exec", Target: "git push origin main"}, DecisionAllow},
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

func TestWorkspaceAutoJob(t *testing.T) {
	repo := repoFixture(t)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "README.md"}, {"commit", "-qm", "seed readme"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	p := &scriptProvider{turns: [][]glue.ProviderEvent{
		turn(`{"items":[{"title":"edit and push"}]}`),
		{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "edit", Name: "edit_file", Arguments: []byte(`{"path":"README.md","old_string":"hello","new_string":"updated"}`)}}, {Type: glue.ProviderEventDone}},
		{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "push", Name: "shell_exec", Arguments: []byte(`{"argv":["git","push","origin","main"]}`)}}, {Type: glue.ProviderEventDone}},
		turn("stopped on push"),
		turn(`{"done":true,"items":[{"title":"edit and push","done":true}]}`),
	}}
	m, err := New(Options{StateDir: t.TempDir(), ProviderFactory: func(string) (glue.Provider, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	if _, err := m.Create(context.Background(), Spec{Objective: "bad", Repo: RepoSpec{Path: repo}, Policy: "unknown"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for unknown policy, got %v", err)
	}
	job, err := m.Create(context.Background(), Spec{Objective: "edit and push", Repo: RepoSpec{Path: repo}, Policy: "workspace-auto"})
	if err != nil {
		t.Fatal(err)
	}
	if job.Spec.Policy != "workspace-auto" {
		t.Fatalf("policy = %q, want workspace-auto", job.Spec.Policy)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		cur, err := m.Get(context.Background(), job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cur.Status == WaitingForInput {
			job = cur
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not reach waiting_for_input (status=%s)", cur.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if data, err := os.ReadFile(filepath.Join(job.Workspace, "README.md")); err != nil || string(data) != "updated\n" {
		t.Fatalf("README.md = %q, err = %v", data, err)
	}
	events, err := m.Events(context.Background(), job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var gotAuto, gotReq bool
	var permID string
	for _, e := range events {
		switch e.Type {
		case "permission_auto":
			payload, _ := e.Payload.(map[string]any)
			if payload["tool"] == "edit_file" && payload["target"] == "README.md" && payload["decision"] == "allow" && payload["rule"] == "workspace-edit" {
				gotAuto = true
			}
		case "permission_request":
			payload, _ := e.Payload.(map[string]any)
			permID, _ = payload["permission_id"].(string)
			req, _ := payload["request"].(map[string]any)
			if req["tool"] == "shell_exec" && req["target"] == "git push origin main" {
				gotReq = true
			}
		}
	}
	if !gotAuto || !gotReq || permID == "" {
		t.Fatalf("gotAuto=%v gotReq=%v permID=%q events=%v", gotAuto, gotReq, permID, events)
	}
	if err := m.Decide(context.Background(), job.ID, permID, glue.PermissionDecision{Allow: false, Reason: "no push"}); err != nil {
		t.Fatal(err)
	}
	if got := waitJob(t, m, job.ID); got.Status != Achieved {
		t.Fatalf("final job = %+v", got)
	}
	denyMgr, err := New(Options{StateDir: t.TempDir(), Policy: &Policy{Name: "lockdown", Default: DecisionDeny}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { denyMgr.Close() })
	denyJob, err := denyMgr.Create(context.Background(), Spec{Objective: "deny", Repo: RepoSpec{Path: repo}})
	if err != nil {
		t.Fatal(err)
	}
	dec, err := (jobPermission{manager: denyMgr, id: denyJob.ID, run: &execution{ctx: context.Background(), cancel: func() {}}}).Decide(context.Background(), glue.PermissionRequest{Tool: "write_file", Target: "x.txt"})
	if err != nil || dec.Allow {
		t.Fatalf("deny decision = %+v, err = %v", dec, err)
	}
}

func TestDefaultAutoJob(t *testing.T) {
	repo, outside := repoFixture(t), t.TempDir()
	outsideSecret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideSecret, []byte("untouched\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const fakeToken = "ghp_AutoPolicyRedactionToken1234567890"
	p := &scriptProvider{turns: [][]glue.ProviderEvent{
		turn(`{"items":[{"title":"autonomous work"}]}`),
		{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "w1", Name: "write_file", Arguments: []byte(`{"path":"notes.txt","content":"hello\n"}`)}}, {Type: glue.ProviderEventDone}},
		{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "w2", Name: "write_file", Arguments: []byte(`{"path":"../escape.txt","content":"bad\n"}`)}}, {Type: glue.ProviderEventDone}},
		{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "w3", Name: "write_file", Arguments: []byte(`{"path":".env","content":"SECRET=1\n"}`)}}, {Type: glue.ProviderEventDone}},
		{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "sh1", Name: "shell_exec", Arguments: []byte(`{"argv":["env"]}`)}}, {Type: glue.ProviderEventDone}},
		{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "sh2", Name: "shell_exec", Arguments: []byte(`{"argv":["sh","-c","ln -s ` + outside + ` escape-dir && printf 'pipe-ok %s\\n' ` + fakeToken + `"]}`)}}, {Type: glue.ProviderEventDone}},
		{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "e1", Name: "edit_file", Arguments: []byte(`{"path":"escape-dir/secret.txt","old_string":"untouched","new_string":"pwned"}`)}}, {Type: glue.ProviderEventDone}},
		turn("done"),
		turn(`{"done":true,"items":[{"title":"autonomous work","done":true}],"summary":"ok"}`),
	}}
	m, err := New(Options{StateDir: t.TempDir(), ProviderFactory: func(string) (glue.Provider, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	job, err := m.Create(context.Background(), Spec{Objective: "autonomous work", Repo: RepoSpec{Path: repo}})
	if err != nil || job.Spec.Policy != "auto" {
		t.Fatalf("job = %+v, err = %v", job, err)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	finished := waitJob(t, m, job.ID)
	if finished.Status != Achieved {
		t.Fatalf("status = %s, want achieved", finished.Status)
	}
	if data, err := os.ReadFile(filepath.Join(finished.Workspace, "notes.txt")); err != nil || string(data) != "hello\n" {
		t.Fatalf("notes.txt = %q, err = %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(finished.Workspace), "escape.txt")); !os.IsNotExist(err) {
		t.Fatalf("escaped workspace write succeeded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(finished.Workspace, ".env")); !os.IsNotExist(err) {
		t.Fatalf("blocklisted .env write succeeded: %v", err)
	}
	if data, err := os.ReadFile(outsideSecret); err != nil || string(data) != "untouched\n" {
		t.Fatalf("symlink edit escaped workspace: %q, err = %v", data, err)
	}
	events, err := m.Events(context.Background(), job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	rawEvents, _ := json.Marshal(events)
	if bytes.Contains(rawEvents, []byte(fakeToken)) || !bytes.Contains(rawEvents, []byte("pipe-ok [REDACTED]")) {
		t.Fatalf("expected redacted shell pipeline output in events: %s", rawEvents)
	}
	for _, e := range events {
		if e.Type == "permission_request" {
			t.Fatalf("unexpected permission_request under auto policy: %+v", e)
		}
	}
}
