// Package echo is a minimal example of a custom Glue provider.
//
// It implements the [glue.Provider] interface by echoing the most recent
// user message back as the assistant's text. There is no model, no
// network call, and no tool support — just the smallest amount of code
// that satisfies the interface so a downstream package can be wired into
// glue.NewAgent without importing providers/gemini.
//
// Use this as a template when adding a new production provider:
//
//   - copy the [Provider] type and [Provider.Stream] method shape
//   - replace the body of stream() with your network code
//   - emit ProviderEventStart, then any number of ProviderEventTextDelta
//     / ProviderEventThinkingDelta / ProviderEventToolCall events,
//     then exactly one ProviderEventDone or ProviderEventError
//   - remember that the channel must be closed (the loop relies on a
//     closed channel after Done to release the receive goroutine)
package echo

import (
	"context"
	"strings"
	"time"

	"github.com/erain/glue"
)

// Provider is a Glue provider that echoes the user's last text back.
type Provider struct {
	// Prefix is prepended to the echoed text. Optional.
	Prefix string
}

// New constructs an echo provider.
func New() *Provider { return &Provider{} }

// Stream implements [glue.Provider].
func (p *Provider) Stream(ctx context.Context, req glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	events := make(chan glue.ProviderEvent, 4)

	go func() {
		defer close(events)

		// Note: do NOT include Message in the Start event when the
		// goroutine will continue to mutate the same struct before
		// emitting Done. The loop dereferences event.Message at read
		// time, so an aliased pointer races against later mutations
		// and causes content to be duplicated. Send a Start event
		// without Message and let the loop synthesize a default
		// assistant message; emit a fully-populated Message only on
		// Done.
		if !send(ctx, events, glue.ProviderEvent{Type: glue.ProviderEventStart}) {
			return
		}

		text := lastUserText(req.Messages)
		if p.Prefix != "" {
			text = p.Prefix + text
		}
		if text != "" {
			if !send(ctx, events, glue.ProviderEvent{Type: glue.ProviderEventTextDelta, Delta: text}) {
				return
			}
		}

		final := glue.Message{
			Role:       glue.MessageRoleAssistant,
			Provider:   "echo",
			Model:      req.Model,
			CreatedAt:  time.Now().UTC(),
			StopReason: glue.StopReasonStop,
		}
		if text != "" {
			final.Content = []glue.ContentPart{{Type: glue.ContentTypeText, Text: text}}
		}
		send(ctx, events, glue.ProviderEvent{Type: glue.ProviderEventDone, Message: &final})
	}()

	return events, nil
}

// lastUserText returns the text of the most recent user message in the
// transcript, or "" if there isn't one. A real provider would convert
// the entire transcript into the backend's native shape.
func lastUserText(messages []glue.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role != glue.MessageRoleUser {
			continue
		}
		var b strings.Builder
		for _, part := range m.Content {
			if part.Type == glue.ContentTypeText {
				b.WriteString(part.Text)
			}
		}
		return b.String()
	}
	return ""
}

// send respects ctx cancellation so a provider stuck on a slow consumer
// can still terminate when the loop's context is canceled.
func send(ctx context.Context, events chan<- glue.ProviderEvent, event glue.ProviderEvent) bool {
	select {
	case <-ctx.Done():
		return false
	case events <- event:
		return true
	}
}
