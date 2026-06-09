package loop

import (
	"fmt"
	"strings"
)

// HardenHistory repairs transcript invariants that providers reject
// with opaque 400s, returning a repaired copy (the input is not
// modified). It runs at the start of every [Run], so a transcript that
// was interrupted mid-turn (Esc-cancel, crash, resume, fork) or that
// mixes models is always safe to replay:
//
//  1. Every assistant tool call gets a result: calls whose tool-result
//     message is missing (an interrupted turn) receive a synthesized
//     error result saying the call was interrupted — both the Gemini
//     and OpenAI-compatible APIs reject dangling tool calls outright.
//  2. Tool-result messages whose call ID matches no assistant tool
//     call are dropped (the paired turn was edited away).
//  3. Assistant messages with no meaningful content — no text, no
//     thinking, no images, no tool calls — are dropped; replaying an
//     empty turn poisons some providers and helps none.
//  4. Tool-call IDs are normalized to `[A-Za-z0-9_-]{1,64}`
//     consistently across the call and its result.
//  5. Turns produced by a different model have provider-specific
//     thinking signatures stripped and thinking blocks dropped —
//     another model's reasoning artifacts are at best noise and at
//     worst hard errors when echoed to the wrong API.
func HardenHistory(messages []Message, activeModel string) []Message {
	msgs := cloneMessages(messages)
	normalizeToolCallIDs(msgs)

	// Index every tool-result by call ID and every call by ID.
	calls := map[string]ToolCall{}
	for _, m := range msgs {
		if m.Role != MessageRoleAssistant {
			continue
		}
		for _, c := range collectToolCalls(m) {
			calls[c.ID] = c
		}
	}
	results := map[string]bool{}
	for _, m := range msgs {
		if m.Role == MessageRoleTool && m.ToolCallID != "" {
			results[m.ToolCallID] = true
		}
	}

	out := make([]Message, 0, len(msgs))
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		switch m.Role {
		case MessageRoleAssistant:
			m = sanitizeForeignTurn(m, activeModel)
			if !hasMeaningfulContent(m) {
				continue
			}
			out = append(out, m)
			// Absorb this turn's contiguous tool results, then
			// synthesize any that are missing so the pairing is
			// complete before the next turn.
			turnCalls := collectToolCalls(m)
			seen := map[string]bool{}
			for i+1 < len(msgs) && msgs[i+1].Role == MessageRoleTool {
				i++
				tm := msgs[i]
				if _, known := calls[tm.ToolCallID]; !known {
					continue // orphaned result
				}
				seen[tm.ToolCallID] = true
				out = append(out, tm)
			}
			for _, c := range turnCalls {
				if seen[c.ID] || resultsElsewhere(msgs, i+1, c.ID) {
					continue
				}
				out = append(out, syntheticToolResult(c))
			}
		case MessageRoleTool:
			if _, known := calls[m.ToolCallID]; !known {
				continue // orphaned result with no paired call
			}
			out = append(out, m)
		default:
			out = append(out, m)
		}
	}
	return out
}

// resultsElsewhere reports whether a result for id appears at or after
// position from — a later (non-contiguous) result still counts.
func resultsElsewhere(msgs []Message, from int, id string) bool {
	for ; from < len(msgs); from++ {
		if msgs[from].Role == MessageRoleTool && msgs[from].ToolCallID == id {
			return true
		}
	}
	return false
}

// syntheticToolResult stands in for a tool call that never produced a
// result (interrupted turn). Marked both in the text and the metadata
// so nothing downstream mistakes it for real output.
func syntheticToolResult(call ToolCall) Message {
	return Message{
		Role:       MessageRoleTool,
		ToolCallID: call.ID,
		ToolName:   call.Name,
		IsError:    true,
		Content: []ContentPart{{
			Type: ContentTypeText,
			Text: "tool call was interrupted before producing a result; do not assume it ran",
		}},
		Metadata: map[string]any{"glue/synthetic": true},
	}
}

// hasMeaningfulContent reports whether an assistant message carries
// anything worth replaying.
func hasMeaningfulContent(m Message) bool {
	for _, p := range m.Content {
		switch p.Type {
		case ContentTypeText:
			if strings.TrimSpace(p.Text) != "" {
				return true
			}
		case ContentTypeThinking:
			if strings.TrimSpace(p.Thinking) != "" {
				return true
			}
		case ContentTypeImage:
			if p.Image != nil {
				return true
			}
		case ContentTypeToolCall:
			if p.ToolCall != nil {
				return true
			}
		}
	}
	return false
}

// sanitizeForeignTurn strips model-specific reasoning artifacts from
// assistant turns produced by a different model: thinking signatures
// are dropped everywhere and thinking blocks removed.
func sanitizeForeignTurn(m Message, activeModel string) Message {
	if activeModel == "" || m.Model == "" || m.Model == activeModel {
		return m
	}
	content := make([]ContentPart, 0, len(m.Content))
	for _, p := range m.Content {
		if p.Type == ContentTypeThinking {
			continue
		}
		p.Signature = ""
		content = append(content, p)
	}
	m.Content = content
	return m
}

// normalizeToolCallIDs rewrites tool-call IDs that providers reject
// (anything outside [A-Za-z0-9_-]{1,64}) consistently on the assistant
// call and its result messages. Returns the old→new mapping.
func normalizeToolCallIDs(msgs []Message) map[string]string {
	idMap := map[string]string{}
	used := map[string]bool{}

	sanitize := func(id string) string {
		var b strings.Builder
		for _, r := range id {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
				b.WriteRune(r)
			default:
				b.WriteByte('_')
			}
		}
		s := b.String()
		if len(s) > 64 {
			s = s[:64]
		}
		return s
	}

	next := 0
	mapped := func(id string) string {
		if id == "" {
			return id
		}
		if m, ok := idMap[id]; ok {
			return m
		}
		s := sanitize(id)
		if s == id {
			used[s] = true
			return id
		}
		if s == "" {
			s = "call"
		}
		candidate := s
		for used[candidate] {
			next++
			suffix := fmt.Sprintf("_%d", next)
			max := 64 - len(suffix)
			base := s
			if len(base) > max {
				base = base[:max]
			}
			candidate = base + suffix
		}
		used[candidate] = true
		idMap[id] = candidate
		return candidate
	}

	// Pre-mark already-valid IDs so collisions rename the invalid side.
	for _, m := range msgs {
		for _, p := range m.Content {
			if p.Type == ContentTypeToolCall && p.ToolCall != nil && sanitize(p.ToolCall.ID) == p.ToolCall.ID {
				used[p.ToolCall.ID] = true
			}
		}
	}

	for i := range msgs {
		for j := range msgs[i].Content {
			p := &msgs[i].Content[j]
			if p.Type == ContentTypeToolCall && p.ToolCall != nil {
				p.ToolCall.ID = mapped(p.ToolCall.ID)
			}
		}
		if msgs[i].Role == MessageRoleTool && msgs[i].ToolCallID != "" {
			msgs[i].ToolCallID = mapped(msgs[i].ToolCallID)
		}
	}
	return idMap
}
