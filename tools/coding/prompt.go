package coding

import (
	"strings"

	"github.com/erain/glue"
)

// PromptVariantTerse selects the minimal system-prompt flavor for
// frontier models that need little steering. The empty string selects
// the default (more explicit) variant for open-weight models.
const PromptVariantTerse = "terse"

const terseIntro = `You are a coding agent operating directly in the user's workspace. Make the requested changes using the tools below, verify them (build/tests) when possible, and keep responses concise.`

const defaultIntro = `You are a coding agent operating directly in the user's workspace. Work in small, verifiable steps:

1. Read the relevant files before changing them.
2. Make focused edits with the editing tools — do not echo whole files into chat.
3. Verify your changes by running builds or tests when a shell tool is available.
4. Report what you changed and how you verified it, concisely.

Use one tool call when one suffices; stop and ask only when genuinely blocked.`

// SystemPrompt assembles a coding system prompt from the active
// toolset: one line per tool (from each tool's PromptSnippet) plus the
// deduplicated union of their PromptGuidelines. The prompt therefore
// can never drift from the tools actually registered — pi's
// tool-owned-prompt pattern. Tools without a snippet are omitted.
//
// variant selects the intro flavor: [PromptVariantTerse] for frontier
// models, "" for the default explicit variant (open-weight models
// benefit from the extra structure). Pick via
// providers.CapabilitiesFor(name).PromptVariant.
func SystemPrompt(tools []glue.Tool, variant string) string {
	var b strings.Builder
	if variant == PromptVariantTerse {
		b.WriteString(terseIntro)
	} else {
		b.WriteString(defaultIntro)
	}

	var lines []string
	var guidelines []string
	seen := map[string]bool{}
	for _, t := range tools {
		if t.PromptSnippet != "" {
			lines = append(lines, "- "+t.Name+": "+t.PromptSnippet)
		}
		for _, g := range t.PromptGuidelines {
			if g == "" || seen[g] {
				continue
			}
			seen[g] = true
			guidelines = append(guidelines, "- "+g)
		}
	}
	if len(lines) > 0 {
		b.WriteString("\n\nTools:\n")
		b.WriteString(strings.Join(lines, "\n"))
	}
	if len(guidelines) > 0 {
		b.WriteString("\n\nGuidelines:\n")
		b.WriteString(strings.Join(guidelines, "\n"))
	}
	return b.String()
}
