package glue

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadContextEmptyWorkDir(t *testing.T) {
	t.Parallel()

	got, err := LoadContext("")
	if err != nil {
		t.Fatalf("LoadContext: %v", err)
	}
	if got.AgentsMD != "" || len(got.Skills) != 0 {
		t.Fatalf("got = %#v, want empty context", got)
	}
}

func TestLoadContextMissingAGENTSIsNonFatal(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	got, err := LoadContext(dir)
	if err != nil {
		t.Fatalf("LoadContext: %v", err)
	}
	if got.AgentsMD != "" {
		t.Fatalf("AgentsMD = %q, want empty for missing file", got.AgentsMD)
	}
}

func TestLoadContextReadsAGENTSMD(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "AGENTS.md"), "Project rules.\n- be terse\n")
	got, err := LoadContext(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.AgentsMD, "Project rules") {
		t.Fatalf("AgentsMD = %q, want contains 'Project rules'", got.AgentsMD)
	}
}

func TestLoadContextLoadsSkills(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".agents", "skills", "triage", "SKILL.md"),
		"---\nname: triage\ndescription: Triage an issue\n---\n\nTriage the given issue.\n")
	writeFile(t, filepath.Join(dir, ".agents", "skills", "no-frontmatter", "SKILL.md"),
		"Just instructions.\n")

	got, err := LoadContext(dir)
	if err != nil {
		t.Fatalf("LoadContext: %v", err)
	}
	if len(got.Skills) != 2 {
		t.Fatalf("len(Skills) = %d, want 2", len(got.Skills))
	}
	triage, ok := got.Skills["triage"]
	if !ok {
		t.Fatal("triage skill missing")
	}
	if triage.Description != "Triage an issue" {
		t.Fatalf("triage.Description = %q", triage.Description)
	}
	if !strings.HasPrefix(triage.Instructions, "Triage the given issue") {
		t.Fatalf("triage.Instructions = %q", triage.Instructions)
	}
	if got := got.Skills["no-frontmatter"]; got.Description != "" || !strings.Contains(got.Instructions, "Just instructions") {
		t.Fatalf("no-frontmatter skill = %#v", got)
	}
}

func TestLoadContextMalformedFrontmatterErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Opens with --- but never closes.
	writeFile(t, filepath.Join(dir, ".agents", "skills", "broken", "SKILL.md"),
		"---\nname: broken\nno closing\n")

	_, err := LoadContext(dir)
	if err == nil || !strings.Contains(err.Error(), "frontmatter") {
		t.Fatalf("err = %v, want frontmatter error", err)
	}
}

func TestParseMarkdownWithFrontmatterDefaults(t *testing.T) {
	t.Parallel()

	got, err := parseMarkdownWithFrontmatter("Hello.\n", "default")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "default" {
		t.Fatalf("Name = %q, want default", got.Name)
	}
	if got.Body != "Hello." {
		t.Fatalf("Body = %q, want Hello.", got.Body)
	}
}

func TestParseMarkdownWithFrontmatterFields(t *testing.T) {
	t.Parallel()

	got, err := parseMarkdownWithFrontmatter("---\nname: alice\ndescription: greet\nmodel: gemini-x\n---\nHi\n", "fallback")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "alice" || got.Description != "greet" || got.Model != "gemini-x" || got.Body != "Hi" {
		t.Fatalf("got = %#v, want alice/greet/gemini-x/Hi", got)
	}
}

func TestComposeSystemPromptIncludesAGENTSAndSkillCatalog(t *testing.T) {
	t.Parallel()

	skills := map[string]Skill{
		"a": {Name: "a", Description: "one"},
		"b": {Name: "b"},
	}
	got := composeSystemPrompt("base", "agents", skills)
	if !strings.Contains(got, "base") || !strings.Contains(got, "agents") {
		t.Fatalf("missing base/agents: %q", got)
	}
	if !strings.Contains(got, "## Available Skills") {
		t.Fatalf("missing skill catalog header: %q", got)
	}
	// Catalog is sorted, so 'a' appears before 'b'.
	if strings.Index(got, "- a") > strings.Index(got, "- b") {
		t.Fatalf("skills not sorted: %q", got)
	}
}

func TestSessionSkillUsesContextAndAppendsArgs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "AGENTS.md"), "Be concise.")
	writeFile(t, filepath.Join(dir, ".agents", "skills", "greet", "SKILL.md"),
		"---\nname: greet\ndescription: Greet a person\n---\nGreet the person from arguments.\n")

	provider := &recordingProvider{turns: [][]ProviderEvent{textTurn("hi alice")}}
	agent := NewAgent(AgentOptions{Provider: provider, WorkDir: dir})
	session, err := agent.Session(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}

	res, err := session.Skill(context.Background(), "greet", map[string]string{"name": "Alice"})
	if err != nil {
		t.Fatalf("Skill: %v", err)
	}
	if res.Text != "hi alice" {
		t.Fatalf("Text = %q, want hi alice", res.Text)
	}

	// System prompt should include AGENTS.md and skill catalog.
	sys := provider.requests[0].SystemPrompt
	if !strings.Contains(sys, "Be concise.") || !strings.Contains(sys, "Available Skills") || !strings.Contains(sys, "greet") {
		t.Fatalf("system prompt = %q, want AGENTS + catalog", sys)
	}
	// User message should contain the skill instructions and the JSON args.
	user := provider.requests[0].Messages[0].Content[0].Text
	if !strings.Contains(user, "Greet the person from arguments") || !strings.Contains(user, `"name": "Alice"`) {
		t.Fatalf("user prompt = %q, want skill body + args", user)
	}
}

func TestSessionSkillUnknownErrors(t *testing.T) {
	t.Parallel()

	provider := &recordingProvider{turns: [][]ProviderEvent{textTurn("ok")}}
	agent := NewAgent(AgentOptions{Provider: provider})
	session, _ := agent.Session(context.Background(), "x")

	_, err := session.Skill(context.Background(), "ghost", nil)
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("err = %v, want ghost not found", err)
	}
	if provider.calls != 0 {
		t.Fatalf("provider called for missing skill: calls=%d", provider.calls)
	}
}

func TestSessionSkillProgrammaticEntryWins(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".agents", "skills", "greet", "SKILL.md"),
		"---\nname: greet\ndescription: from disk\n---\nfrom disk\n")

	programmatic := map[string]Skill{
		"greet": {Name: "greet", Description: "programmatic", Instructions: "from code"},
	}
	provider := &recordingProvider{turns: [][]ProviderEvent{textTurn("ok")}}
	agent := NewAgent(AgentOptions{Provider: provider, WorkDir: dir, Skills: programmatic})
	session, err := agent.Session(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Skill(context.Background(), "greet", nil); err != nil {
		t.Fatal(err)
	}
	user := provider.requests[0].Messages[0].Content[0].Text
	if !strings.Contains(user, "from code") {
		t.Fatalf("user = %q, want programmatic 'from code' to win over disk", user)
	}
}
