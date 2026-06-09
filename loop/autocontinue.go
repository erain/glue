package loop

import (
	"regexp"
	"strings"
)

// Auto-continue ("next-speaker check") recovers the classic Gemini
// stall: the model narrates the action it is about to take — "I will
// now update the router." — and stops without calling a tool. Gemini
// CLI fixes this with fast-path heuristics plus an auxiliary model
// call; glue ships the deterministic heuristic only, opt-in via
// [RunRequest.AutoContinue], and bounds it to maxAutoContinues per run
// so a model that keeps narrating cannot loop forever.

// maxAutoContinues bounds the synthetic "Please continue." nudges in a
// single Run.
const maxAutoContinues = 2

// EventAutoContinue is emitted when the loop injects a synthetic
// "Please continue." user message after detecting a narrated-intent
// stall. Event.Message carries the assistant turn that stalled.
const EventAutoContinue EventType = "auto_continue"

// autoContinueMessage is the nudge injected after a stall.
const autoContinueMessage = "Please continue."

// intentRe matches announced-future-action phrasing. It is applied to
// the final sentence only, and never when that closing region asks the
// user a question.
var intentRe = regexp.MustCompile(`(?i)\b(i('| wi)ll( now)?|let me( now)?|i am going to|i'm going to|next,? i('| wi)ll|now i('| wi)ll|proceeding to|about to)\b`)

// stallIntent reports whether an assistant turn looks like a narrated
// action that never happened: future-intent phrasing in the closing
// sentence, no question to the user, and no tool call made.
func stallIntent(m Message) bool {
	if len(collectToolCalls(m)) > 0 {
		return false
	}
	text := strings.TrimSpace(assistantTurnText(m))
	if text == "" {
		return false
	}
	closing := closingSentence(text)
	if strings.Contains(closing, "?") {
		return false
	}
	return intentRe.MatchString(closing)
}

func assistantTurnText(m Message) string {
	var b strings.Builder
	for _, p := range m.Content {
		if p.Type == ContentTypeText && p.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// closingSentence returns the last sentence-ish fragment of text.
func closingSentence(text string) string {
	text = strings.TrimSpace(text)
	text = strings.TrimRight(text, ".!…:")
	cut := -1
	for _, sep := range []string{". ", "! ", "…", ".\n", "!\n", "\n"} {
		if i := strings.LastIndex(text, sep); i > cut {
			cut = i + len(sep) - 1
		}
	}
	if cut >= 0 && cut+1 < len(text) {
		return strings.TrimSpace(text[cut+1:])
	}
	return text
}

// autoContinueUserMessage is the synthetic nudge, marked in metadata
// so stores, UIs, and compactors can identify it.
func autoContinueUserMessage() Message {
	return Message{
		Role:     MessageRoleUser,
		Content:  []ContentPart{{Type: ContentTypeText, Text: autoContinueMessage}},
		Metadata: map[string]any{"glue/auto-continue": true},
	}
}
