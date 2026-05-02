package glue

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadContextLoadsRoles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "roles", "reviewer.md"),
		"---\nname: reviewer\ndescription: Reviews diffs\nmodel: gemini-pro\n---\nReview the change carefully.\n")
	writeFile(t, filepath.Join(dir, "roles", "no-frontmatter.md"), "Just instructions\n")

	got, err := LoadContext(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Roles) != 2 {
		t.Fatalf("len(Roles) = %d, want 2", len(got.Roles))
	}
	r, ok := got.Roles["reviewer"]
	if !ok {
		t.Fatal("reviewer role missing")
	}
	if r.Description != "Reviews diffs" || r.Model != "gemini-pro" || !strings.Contains(r.Instructions, "Review the change") {
		t.Fatalf("reviewer = %#v", r)
	}
}

func TestLoadContextRolesMalformedFrontmatterErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "roles", "broken.md"), "---\nname: broken\nno close\n")
	_, err := LoadContext(dir)
	if err == nil || !strings.Contains(err.Error(), "frontmatter") {
		t.Fatalf("err = %v, want frontmatter error", err)
	}
}

func TestRolePrecedenceCallBeatsSessionBeatsAgent(t *testing.T) {
	t.Parallel()

	roles := []Role{
		{Name: "agent-default", Instructions: "agent says A"},
		{Name: "session-default", Instructions: "session says S"},
		{Name: "call", Instructions: "call says C"},
	}
	provider := &recordingProvider{turns: [][]ProviderEvent{textTurn("ok"), textTurn("ok"), textTurn("ok")}}
	agent := NewAgent(AgentOptions{Provider: provider, Roles: roles, Role: "agent-default"})

	// Without overrides → agent default.
	session, _ := agent.Session(context.Background(), "x")
	if _, err := session.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(provider.requests[0].SystemPrompt, "agent says A") {
		t.Fatalf("call 1 system = %q, want agent role", provider.requests[0].SystemPrompt)
	}

	// Session default overrides agent default.
	sessionWithRole, _ := agent.Session(context.Background(), "y", WithSessionRole("session-default"))
	if _, err := sessionWithRole.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(provider.requests[1].SystemPrompt, "session says S") {
		t.Fatalf("call 2 system = %q, want session role", provider.requests[1].SystemPrompt)
	}
	if strings.Contains(provider.requests[1].SystemPrompt, "agent says A") {
		t.Fatalf("call 2 unexpectedly contains agent role: %q", provider.requests[1].SystemPrompt)
	}

	// Per-call WithRole beats session default.
	if _, err := sessionWithRole.Prompt(context.Background(), "go", WithRole("call")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(provider.requests[2].SystemPrompt, "call says C") {
		t.Fatalf("call 3 system = %q, want call role", provider.requests[2].SystemPrompt)
	}
}

func TestRoleModelPrecedence(t *testing.T) {
	t.Parallel()

	roles := []Role{{Name: "fast", Model: "role-fast", Instructions: "be fast"}}
	provider := &recordingProvider{turns: [][]ProviderEvent{textTurn("ok"), textTurn("ok"), textTurn("ok")}}
	agent := NewAgent(AgentOptions{Provider: provider, Model: "agent-model", Roles: roles})

	// No role → agent model.
	session, _ := agent.Session(context.Background(), "x")
	if _, err := session.Prompt(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if got := provider.requests[0].Model; got != "agent-model" {
		t.Fatalf("call 1 model = %q, want agent-model", got)
	}

	// Role with Model overrides agent model.
	if _, err := session.Prompt(context.Background(), "b", WithRole("fast")); err != nil {
		t.Fatal(err)
	}
	if got := provider.requests[1].Model; got != "role-fast" {
		t.Fatalf("call 2 model = %q, want role-fast", got)
	}

	// Explicit WithModel beats role's Model.
	if _, err := session.Prompt(context.Background(), "c", WithRole("fast"), WithModel("explicit")); err != nil {
		t.Fatal(err)
	}
	if got := provider.requests[2].Model; got != "explicit" {
		t.Fatalf("call 3 model = %q, want explicit (WithModel beats role)", got)
	}
}

func TestRoleUnknownNameErrors(t *testing.T) {
	t.Parallel()

	provider := &recordingProvider{turns: [][]ProviderEvent{textTurn("ok")}}
	agent := NewAgent(AgentOptions{Provider: provider})
	session, _ := agent.Session(context.Background(), "x")

	_, err := session.Prompt(context.Background(), "go", WithRole("ghost"))
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("err = %v, want unknown role 'ghost'", err)
	}
	if provider.calls != 0 {
		t.Fatalf("provider called for unknown role: calls=%d", provider.calls)
	}
}

func TestRoleAppliesInstructionsAsBlock(t *testing.T) {
	t.Parallel()

	roles := []Role{{Name: "r", Instructions: "be terse"}}
	provider := &recordingProvider{turns: [][]ProviderEvent{textTurn("ok")}}
	agent := NewAgent(AgentOptions{Provider: provider, SystemPrompt: "base", Roles: roles})
	session, _ := agent.Session(context.Background(), "x")
	if _, err := session.Prompt(context.Background(), "go", WithRole("r")); err != nil {
		t.Fatal(err)
	}
	sys := provider.requests[0].SystemPrompt
	if !strings.Contains(sys, "base") || !strings.Contains(sys, "## Role: r") || !strings.Contains(sys, "be terse") {
		t.Fatalf("system = %q, want base + role block", sys)
	}
}

func TestRolesFromWorkDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "roles", "fast.md"),
		"---\nname: fast\nmodel: disk-fast\n---\nbe fast\n")

	provider := &recordingProvider{turns: [][]ProviderEvent{textTurn("ok")}}
	agent := NewAgent(AgentOptions{Provider: provider, WorkDir: dir, Model: "agent-default"})
	session, _ := agent.Session(context.Background(), "x")
	if _, err := session.Prompt(context.Background(), "go", WithRole("fast")); err != nil {
		t.Fatal(err)
	}
	if got := provider.requests[0].Model; got != "disk-fast" {
		t.Fatalf("model = %q, want disk-fast (loaded from WorkDir)", got)
	}
}

func TestProgrammaticRoleWinsOnDiskCollision(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "roles", "r.md"),
		"---\nname: r\nmodel: disk-model\n---\ndisk says X\n")

	roles := []Role{{Name: "r", Model: "code-model", Instructions: "code says Y"}}
	provider := &recordingProvider{turns: [][]ProviderEvent{textTurn("ok")}}
	agent := NewAgent(AgentOptions{Provider: provider, WorkDir: dir, Roles: roles})
	session, _ := agent.Session(context.Background(), "x")
	if _, err := session.Prompt(context.Background(), "go", WithRole("r")); err != nil {
		t.Fatal(err)
	}
	if got := provider.requests[0].Model; got != "code-model" {
		t.Fatalf("model = %q, want code-model (programmatic wins)", got)
	}
	if !strings.Contains(provider.requests[0].SystemPrompt, "code says Y") {
		t.Fatalf("system did not pick up programmatic role instructions: %q", provider.requests[0].SystemPrompt)
	}
}
