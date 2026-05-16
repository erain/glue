package glue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
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

// DefaultSummarizingSystemPrompt is the instruction the
// [SummarizingCompactor] sends to the provider when no override is
// configured. The prompt biases for fact retention over polish.
const DefaultSummarizingSystemPrompt = `You are summarizing a conversation transcript so it can be replaced with a single compact summary message.

Preserve, in order of importance:
- facts the user stated (names, dates, numbers, IDs, places)
- decisions made and their reasons
- outcomes of any actions taken
- open questions or pending follow-ups
- the user's stated preferences

Drop:
- pleasantries
- failed tangents
- verbatim quotes you can paraphrase

Write a single coherent narrative in third person ("the user…", "the assistant…"). Do not invent details. Do not include meta-commentary about summarization.`

// SummarizingCompactor is a token-aware [Compactor] that summarizes
// older transcript messages by calling the configured Provider. It
// replaces older messages with a single assistant-role marker whose
// text content is the summary and whose metadata records the
// compaction.
//
// SummarizingCompactor is the token-aware drop-in anticipated by
// ADR-0002 and designed in ADR-0007 §1. It composes with — and does
// not replace — [KeepRecentMessages]; callers pick the policy that
// matches their agent's lifetime.
//
// Provider must be set. The compactor calls Provider.Stream once per
// invocation with a single user message containing the formatted
// transcript-to-summarize.
//
// Token estimation is intentionally a heuristic in v0.1: a
// word-count-based proxy that does not need to match any specific
// tokenizer. A later PR can swap the implementation without changing
// this type's public surface.
//
// Errors from the underlying provider propagate. The compactor does
// not silently fall back to dropping context; callers that want a
// degraded-mode behavior should wire a [CompactorFunc] that tries
// SummarizingCompactor first and falls back to [KeepRecentMessages]
// explicitly.
type SummarizingCompactor struct {
	// Provider streams the summary. Required.
	Provider Provider

	// Model is the model id used for the summary call. When empty the
	// provider's default applies.
	Model string

	// TargetTokens is the soft cap: when the estimated transcript size
	// is below this value the compactor returns its input unchanged.
	// Zero or negative falls back to the default (8000).
	TargetTokens int

	// KeepRecent is the number of most-recent messages retained
	// verbatim. Zero or negative falls back to the default (8). When
	// the input transcript has KeepRecent or fewer messages the
	// compactor returns it unchanged regardless of TargetTokens.
	KeepRecent int

	// SystemPrompt is the instruction sent to the summarizer. When
	// empty [DefaultSummarizingSystemPrompt] is used.
	SystemPrompt string
}

// Default knobs for SummarizingCompactor. Documented as constants so
// callers can reason about behavior without reading the source.
const (
	defaultSummarizingTargetTokens = 8000
	defaultSummarizingKeepRecent   = 8
)

// Compact implements [Compactor]. See the type docs for behavior.
func (s *SummarizingCompactor) Compact(ctx context.Context, in []Message) ([]Message, error) {
	if s == nil {
		return nil, errors.New("glue: SummarizingCompactor: nil receiver")
	}
	if s.Provider == nil {
		return nil, errors.New("glue: SummarizingCompactor: Provider is required")
	}
	keep := s.KeepRecent
	if keep <= 0 {
		keep = defaultSummarizingKeepRecent
	}
	target := s.TargetTokens
	if target <= 0 {
		target = defaultSummarizingTargetTokens
	}

	if len(in) <= keep {
		return in, nil
	}
	if estimateTokens(in) <= target {
		return in, nil
	}

	older := in[:len(in)-keep]
	kept := in[len(in)-keep:]

	systemPrompt := s.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = DefaultSummarizingSystemPrompt
	}

	transcript := renderTranscriptForSummary(older)
	summaryReq := ProviderRequest{
		Model:        s.Model,
		SystemPrompt: systemPrompt,
		Messages: []Message{{
			Role:    MessageRoleUser,
			Content: []ContentPart{{Type: ContentTypeText, Text: transcript}},
		}},
	}

	stream, err := s.Provider.Stream(ctx, summaryReq)
	if err != nil {
		return nil, fmt.Errorf("glue: SummarizingCompactor: provider.Stream: %w", err)
	}

	summary, streamErr := collectSummaryText(ctx, stream)
	if streamErr != nil {
		return nil, fmt.Errorf("glue: SummarizingCompactor: %w", streamErr)
	}
	if strings.TrimSpace(summary) == "" {
		return nil, errors.New("glue: SummarizingCompactor: provider returned no text")
	}

	marker := Message{
		Role:    MessageRoleAssistant,
		Content: []ContentPart{{Type: ContentTypeText, Text: summary}},
		Metadata: map[string]any{
			"compaction":             "summarizing",
			"original_message_count": len(older),
		},
	}
	if first, last := transcriptTimeBounds(older); !first.IsZero() {
		marker.Metadata["original_first_ts"] = first.UTC().Format(time.RFC3339)
		marker.Metadata["original_last_ts"] = last.UTC().Format(time.RFC3339)
	}

	out := make([]Message, 0, 1+len(kept))
	out = append(out, marker)
	out = append(out, kept...)
	return out, nil
}

// renderTranscriptForSummary formats older messages into a single
// plain-text block for the summarizer. Roles are tagged so the
// provider sees the alternation; tool calls and results are rendered
// as parenthetical notes rather than dropped, so the summary can
// reference them.
func renderTranscriptForSummary(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		tag := "user"
		switch m.Role {
		case MessageRoleAssistant:
			tag = "assistant"
		case MessageRoleTool:
			tag = "tool"
		}
		b.WriteString(strings.ToUpper(tag))
		b.WriteString(": ")
		first := true
		for _, p := range m.Content {
			switch p.Type {
			case ContentTypeText:
				if !first {
					b.WriteString(" ")
				}
				b.WriteString(p.Text)
				first = false
			case ContentTypeToolCall:
				if p.ToolCall == nil {
					continue
				}
				if !first {
					b.WriteString(" ")
				}
				fmt.Fprintf(&b, "[tool_call name=%s args=%s]", p.ToolCall.Name, string(p.ToolCall.Arguments))
				first = false
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// collectSummaryText drains a provider stream into a single string,
// preferring the final ProviderEventDone message's text content. The
// fallback to text-delta accumulation matches providers that omit a
// final message (none of the shipped providers do, but it keeps the
// compactor robust against fake providers in tests).
func collectSummaryText(ctx context.Context, stream <-chan ProviderEvent) (string, error) {
	var (
		deltas   strings.Builder
		finalMsg *Message
	)
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case ev, ok := <-stream:
			if !ok {
				if finalMsg != nil {
					if text := assistantMessageText(finalMsg); text != "" {
						return text, nil
					}
				}
				return deltas.String(), nil
			}
			switch ev.Type {
			case ProviderEventTextDelta:
				deltas.WriteString(ev.Delta)
			case ProviderEventDone:
				finalMsg = ev.Message
			case ProviderEventError:
				if ev.Error != "" {
					return "", errors.New(ev.Error)
				}
				return "", errors.New("provider stream errored")
			}
		}
	}
}

func assistantMessageText(m *Message) string {
	if m == nil {
		return ""
	}
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

// transcriptTimeBounds returns the earliest and latest CreatedAt of
// the messages, or the zero time if none carry a timestamp.
func transcriptTimeBounds(msgs []Message) (first, last time.Time) {
	for _, m := range msgs {
		if m.CreatedAt.IsZero() {
			continue
		}
		if first.IsZero() || m.CreatedAt.Before(first) {
			first = m.CreatedAt
		}
		if last.IsZero() || m.CreatedAt.After(last) {
			last = m.CreatedAt
		}
	}
	return first, last
}

// estimateTokens returns a heuristic token count for a transcript.
// Uses words × 0.75, a widely-cited English approximation. This is
// intentionally cheap and provider-agnostic; a real tokenizer can
// drop in later without changing the public SummarizingCompactor
// surface.
func estimateTokens(msgs []Message) int {
	words := 0
	for _, m := range msgs {
		for _, p := range m.Content {
			switch p.Type {
			case ContentTypeText:
				words += countWords(p.Text)
			case ContentTypeThinking:
				words += countWords(p.Thinking)
			case ContentTypeToolCall:
				if p.ToolCall != nil {
					words += countWords(p.ToolCall.Name)
					words += countWords(string(p.ToolCall.Arguments))
				}
			}
		}
	}
	if words == 0 {
		return 0
	}
	// 0.75 tokens per word; round up so a 1-word transcript still
	// scores ≥1 token.
	tokens := (words*3 + 3) / 4
	if tokens < 1 {
		tokens = 1
	}
	return tokens
}

// countWords returns the number of whitespace-separated runs in s.
// Conservative: treats any whitespace as a separator and ignores
// empty trailing tokens.
func countWords(s string) int {
	n := 0
	inWord := false
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			inWord = false
		default:
			if !inWord {
				n++
				inWord = true
			}
		}
	}
	return n
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
