package glue

import (
	"context"
	"fmt"
	"strings"
)

// Compactor rewrites a session transcript before it is sent to the
// provider. Implementations should preserve the user's most recent intent
// and the assistant context that depends on it; older turns can be
// dropped, summarized, or replaced. Compactors must not mutate the input
// slice; they return a new slice.
type Compactor interface {
	Compact(ctx context.Context, messages []Message) ([]Message, error)
}

// CompactorFunc adapts a function to the [Compactor] interface.
type CompactorFunc func(ctx context.Context, messages []Message) ([]Message, error)

// Compact implements [Compactor].
func (f CompactorFunc) Compact(ctx context.Context, messages []Message) ([]Message, error) {
	return f(ctx, messages)
}

// KeepRecentMessages returns a [Compactor] that keeps the last n messages
// of a transcript and replaces everything older with a single
// system-style summary message that records how many turns were dropped.
//
// This is the simplest useful compaction policy: it has no token model
// and never calls a provider. It is appropriate when a session is allowed
// to accumulate but the caller can tolerate losing older context past a
// fixed window.
//
// n must be positive. If the input transcript has n or fewer messages,
// the compactor returns it unchanged.
func KeepRecentMessages(n int) Compactor {
	return CompactorFunc(func(_ context.Context, messages []Message) ([]Message, error) {
		if n <= 0 {
			return nil, fmt.Errorf("glue: KeepRecentMessages: n must be positive, got %d", n)
		}
		if len(messages) <= n {
			return messages, nil
		}
		dropped := len(messages) - n
		summary := buildCompactionSummary(messages[:dropped])
		summary.Metadata = map[string]any{"compaction": "keep_recent", "dropped": dropped}

		out := make([]Message, 0, n+1)
		out = append(out, summary)
		out = append(out, messages[dropped:]...)
		return out, nil
	})
}

// buildCompactionSummary produces a short, role-assistant note describing
// what was dropped. The intent is to give the model a stable signal that
// older context existed without claiming to reproduce it.
func buildCompactionSummary(dropped []Message) Message {
	var b strings.Builder
	b.WriteString("Earlier conversation context omitted by compaction (")
	fmt.Fprintf(&b, "%d message", len(dropped))
	if len(dropped) != 1 {
		b.WriteString("s")
	}
	b.WriteString(" dropped). Continue from the messages that follow.")

	return Message{
		Role: MessageRoleAssistant,
		Content: []ContentPart{{
			Type: ContentTypeText,
			Text: b.String(),
		}},
	}
}
