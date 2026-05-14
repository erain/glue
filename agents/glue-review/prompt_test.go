package main

import (
	"strings"
	"testing"
)

func TestSystemPromptLoaded(t *testing.T) {
	t.Parallel()
	if strings.TrimSpace(systemPrompt) == "" {
		t.Fatal("systemPrompt is empty — go:embed of prompts/default.md failed")
	}
	if !strings.Contains(systemPrompt, "## glue-review") {
		t.Fatal("systemPrompt missing the canonical `## glue-review` header instruction")
	}
	if !strings.Contains(systemPrompt, "```markdown") {
		t.Fatal("systemPrompt missing the fenced ```markdown fix-block instruction")
	}
}
