package main

import (
	"strings"
	"testing"
)

func TestSystemPromptForDefault(t *testing.T) {
	t.Parallel()
	got, err := systemPromptFor("")
	if err != nil {
		t.Fatalf("systemPromptFor(\"\"): %v", err)
	}
	if !strings.Contains(got, "Output format") {
		t.Fatalf("default prompt missing expected section: %q", got)
	}
}

func TestSystemPromptForExplicitVersion(t *testing.T) {
	t.Parallel()
	got, err := systemPromptFor("v1")
	if err != nil {
		t.Fatalf("systemPromptFor(v1): %v", err)
	}
	if !strings.Contains(got, "[critical|major|minor]") {
		t.Fatalf("v1 prompt missing severity instructions")
	}
}

func TestSystemPromptForUnknownVersionListsAvailable(t *testing.T) {
	t.Parallel()
	_, err := systemPromptFor("v999")
	if err == nil {
		t.Fatal("expected error for unknown version")
	}
	if !strings.Contains(err.Error(), "available:") {
		t.Fatalf("error should list available versions, got %v", err)
	}
}

func TestAvailablePromptVersionsIncludesV1(t *testing.T) {
	t.Parallel()
	got := availablePromptVersions()
	found := false
	for _, v := range got {
		if v == "v1" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("v1 missing from available versions: %+v", got)
	}
}
