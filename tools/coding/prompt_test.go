package coding

import (
	"strings"
	"testing"

	"github.com/erain/glue"
)

func bundleForPromptTest(t *testing.T) []glue.Tool {
	t.Helper()
	tools, _, err := Tools(Options{Enabled: true, WorkDir: t.TempDir(), AllowedBinaries: []string{"go"}})
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	return tools
}

// Snapshot tests pin the assembled prompts: a tool rename, a dropped
// snippet, or accidental variant drift fails loudly here instead of
// silently degrading the agent.
func TestSystemPromptDefaultSnapshot(t *testing.T) {
	t.Parallel()
	got := SystemPrompt(bundleForPromptTest(t), "")
	for _, want := range []string{
		"You are a coding agent operating directly in the user's workspace.",
		"Work in small, verifiable steps",
		"Tools:\n",
		"- read_file: Read a file (line-offset paging for large files)",
		"- edit_file: Make a surgical string replacement in an existing file",
		"- write_file: Create a new file (or overwrite when allowed)",
		"- list_dir: List a directory's entries",
		"- find_files: Find files by name glob",
		"- grep: Search file contents by regex",
		"- shell_exec: Run an allowlisted command (argv-style, no shell)",
		"Guidelines:\n",
		"- Use read_file to examine files instead of shell cat/sed.",
		"- Prefer edit_file for changing existing files; write_file is for new files or full rewrites.",
		"- Navigate with grep/find_files/list_dir instead of shell find or ls.",
		"- Use shell_exec for builds and tests; long output is kept head+tail with the full stream spooled to a named temp file.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("default prompt missing %q\n--\n%s", want, got)
		}
	}
}

func TestSystemPromptTerseSnapshot(t *testing.T) {
	t.Parallel()
	got := SystemPrompt(bundleForPromptTest(t), PromptVariantTerse)
	if !strings.Contains(got, "keep responses concise") {
		t.Fatalf("terse intro missing:\n%s", got)
	}
	if strings.Contains(got, "Work in small, verifiable steps") {
		t.Fatalf("terse variant leaked the default intro:\n%s", got)
	}
	// Tool lines and guidelines are shared across variants.
	if !strings.Contains(got, "- edit_file:") || !strings.Contains(got, "Guidelines:") {
		t.Fatalf("terse prompt lost the toolset sections:\n%s", got)
	}
	if len(got) >= len(SystemPrompt(bundleForPromptTest(t), "")) {
		t.Fatal("terse prompt should be shorter than the default")
	}
}

func TestSystemPromptTracksToolset(t *testing.T) {
	t.Parallel()
	all := bundleForPromptTest(t)
	var readOnly []glue.Tool
	for _, tool := range all {
		if tool.Name == "read_file" || tool.Name == "grep" {
			readOnly = append(readOnly, tool)
		}
	}
	got := SystemPrompt(readOnly, "")
	if strings.Contains(got, "edit_file") || strings.Contains(got, "shell_exec") {
		t.Fatalf("prompt mentions tools that are not registered:\n%s", got)
	}
	if !strings.Contains(got, "- read_file:") {
		t.Fatalf("prompt missing registered tool:\n%s", got)
	}
}

func TestSystemPromptNoToolsNoSections(t *testing.T) {
	t.Parallel()
	got := SystemPrompt(nil, "")
	if strings.Contains(got, "Tools:") || strings.Contains(got, "Guidelines:") {
		t.Fatalf("empty toolset must not render sections:\n%s", got)
	}
}
