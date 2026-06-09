package loop

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Guardrails detect two pathological loop shapes that waste tokens
// fastest on open-weight models: repeating the same tool call with
// identical arguments, and burning turn after turn on nothing but
// failing tool calls. The response is graduated (Gemini CLI's policy):
// first inject a corrective user message, only halt when the model
// ignores it.

// GuardrailPolicy bounds the loop guardrails. The zero value enables
// them with defaults; set Disabled to turn them off.
type GuardrailPolicy struct {
	// Disabled turns the guardrails off entirely.
	Disabled bool

	// RepeatNudge is the number of consecutive identical tool calls
	// (same name, same canonical arguments) that triggers a corrective
	// user message. Zero means DefaultRepeatNudge.
	RepeatNudge int

	// RepeatHalt is the number of consecutive identical calls that
	// ends the run with [ErrRepeatedToolCalls]. Zero means
	// DefaultRepeatHalt.
	RepeatHalt int

	// MistakeNudge is the number of consecutive all-error tool rounds
	// that triggers a corrective user message. Zero means
	// DefaultMistakeNudge.
	MistakeNudge int

	// MistakeHalt is the number of consecutive all-error tool rounds
	// that ends the run with [ErrTooManyMistakes]. Zero means
	// DefaultMistakeHalt.
	MistakeHalt int
}

// Guardrail defaults, matching the thresholds Cline and Gemini CLI
// converged on.
const (
	DefaultRepeatNudge  = 3
	DefaultRepeatHalt   = 5
	DefaultMistakeNudge = 3
	DefaultMistakeHalt  = 6
)

// EventGuardrail is emitted when a guardrail injects a corrective
// message or halts the run. Event.Error carries the reason;
// Event.Metadata carries "kind" ("repeat" or "mistakes"), "count", and
// "action" ("nudge" or "halt").
const EventGuardrail EventType = "guardrail"

// ErrRepeatedToolCalls ends a run after the model kept issuing the
// same tool call with identical arguments despite a corrective nudge.
var ErrRepeatedToolCalls = errors.New("loop: model repeated the same tool call past the guardrail limit")

// ErrTooManyMistakes ends a run after every tool call failed for
// several consecutive rounds despite a corrective nudge.
var ErrTooManyMistakes = errors.New("loop: too many consecutive failed tool rounds")

const repeatNudgeMessage = "You have repeated the exact same tool call several times. It will keep producing the same result. Step back, reconsider the approach, and either change the arguments or use a different tool."

const mistakeNudgeMessage = "Your last several tool calls all failed. Stop and re-read the error messages carefully before acting again — they describe what to fix. If the same approach keeps failing, try a different one."

func (p GuardrailPolicy) withDefaults() GuardrailPolicy {
	if p.RepeatNudge <= 0 {
		p.RepeatNudge = DefaultRepeatNudge
	}
	if p.RepeatHalt <= 0 {
		p.RepeatHalt = DefaultRepeatHalt
	}
	if p.MistakeNudge <= 0 {
		p.MistakeNudge = DefaultMistakeNudge
	}
	if p.MistakeHalt <= 0 {
		p.MistakeHalt = DefaultMistakeHalt
	}
	return p
}

type guardAction int

const (
	guardOK guardAction = iota
	guardNudge
	guardHalt
)

type guardVerdict struct {
	action  guardAction
	kind    string // "repeat" | "mistakes"
	count   int
	message string // nudge text
	err     error  // halt error
}

// guardState tracks consecutive identical calls and consecutive
// all-error tool rounds across the turns of one Run.
type guardState struct {
	policy GuardrailPolicy

	lastCallKey  string
	repeatCount  int
	repeatNudged bool

	mistakeRounds int
	mistakeNudged bool
}

func newGuardState(policy GuardrailPolicy) *guardState {
	return &guardState{policy: policy.withDefaults()}
}

// observe inspects one tool round (the calls of an assistant turn and
// their results) and returns the action to take.
func (g *guardState) observe(calls []ToolCall, results []Message) guardVerdict {
	if g.policy.Disabled {
		return guardVerdict{action: guardOK}
	}

	// Identical-call tracking: a round consisting of exactly one call
	// whose name+arguments hash matches the previous round's extends
	// the streak; anything else resets it.
	if len(calls) == 1 {
		key := toolCallKey(calls[0])
		if key == g.lastCallKey {
			g.repeatCount++
		} else {
			g.lastCallKey = key
			g.repeatCount = 1
			g.repeatNudged = false
		}
	} else {
		g.lastCallKey = ""
		g.repeatCount = 0
		g.repeatNudged = false
	}

	// Mistake tracking: a round where every result is an error extends
	// the streak; any success resets it.
	allErrors := len(results) > 0
	for _, m := range results {
		if !m.IsError {
			allErrors = false
			break
		}
	}
	if allErrors {
		g.mistakeRounds++
	} else {
		g.mistakeRounds = 0
		g.mistakeNudged = false
	}

	if g.repeatCount >= g.policy.RepeatHalt {
		return guardVerdict{action: guardHalt, kind: "repeat", count: g.repeatCount, err: fmt.Errorf("%w (%d identical calls to %s)", ErrRepeatedToolCalls, g.repeatCount, calls[0].Name)}
	}
	if g.mistakeRounds >= g.policy.MistakeHalt {
		return guardVerdict{action: guardHalt, kind: "mistakes", count: g.mistakeRounds, err: fmt.Errorf("%w (%d consecutive failed rounds)", ErrTooManyMistakes, g.mistakeRounds)}
	}
	if g.repeatCount >= g.policy.RepeatNudge && !g.repeatNudged {
		g.repeatNudged = true
		return guardVerdict{action: guardNudge, kind: "repeat", count: g.repeatCount, message: repeatNudgeMessage}
	}
	if g.mistakeRounds >= g.policy.MistakeNudge && !g.mistakeNudged {
		g.mistakeNudged = true
		return guardVerdict{action: guardNudge, kind: "mistakes", count: g.mistakeRounds, message: mistakeNudgeMessage}
	}
	return guardVerdict{action: guardOK}
}

// toolCallKey canonicalizes a call for identity comparison: name plus
// a hash of the raw arguments with whitespace collapsed.
func toolCallKey(call ToolCall) string {
	args := strings.Join(strings.Fields(string(call.Arguments)), "")
	sum := sha256.Sum256([]byte(call.Name + "\x00" + args))
	return hex.EncodeToString(sum[:8])
}

// guardrailUserMessage is the injected corrective message, marked in
// metadata so stores and UIs can identify it.
func guardrailUserMessage(text, kind string) Message {
	return Message{
		Role:     MessageRoleUser,
		Content:  []ContentPart{{Type: ContentTypeText, Text: text}},
		Metadata: map[string]any{"glue/guardrail": kind},
	}
}
