package main

import (
	"embed"
	"fmt"
	"path"
	"sort"
	"strings"
)

// systemPrompts holds every shipped system prompt by version. They are
// kept in source so we can A/B prompt revisions and roll back without
// re-running history. Adding a new version means: add prompts/vN.md,
// bump defaultPromptVersion when you want it to become the default.
//
//go:embed prompts/*.md
var systemPromptsFS embed.FS

// defaultPromptVersion picks the prompt loaded when --prompt-version is
// empty. Bump only when you want every consumer to see the new prompt
// without an explicit opt-in.
//
// History:
//   - v1 — initial prompt (1.0 release).
//   - v2 — adds `Fix: <ai prompt>` at the end of every Issues /
//     Suggestions entry, so the Action can render a copy-pastable
//     coding-agent prompt next to each inline comment.
const defaultPromptVersion = "v2"

// systemPromptFor returns the embedded prompt for the requested
// version. An unknown version returns an error listing the available
// versions instead of falling back silently — silent fallback would
// hide A/B test misconfiguration.
func systemPromptFor(version string) (string, error) {
	if strings.TrimSpace(version) == "" {
		version = defaultPromptVersion
	}
	data, err := systemPromptsFS.ReadFile(path.Join("prompts", version+".md"))
	if err != nil {
		return "", fmt.Errorf("unknown prompt version %q (available: %s)", version, strings.Join(availablePromptVersions(), ", "))
	}
	return strings.TrimRight(string(data), "\n") + "\n", nil
}

// availablePromptVersions lists every shipped prompt version, sorted
// for stable error messages and CLI help text.
func availablePromptVersions() []string {
	entries, err := systemPromptsFS.ReadDir("prompts")
	if err != nil {
		return nil
	}
	out := []string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ".md"))
	}
	sort.Strings(out)
	return out
}
