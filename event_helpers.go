package glue

import (
	"fmt"
	"io"
)

// WithStreamWriter mirrors EventTextDelta.Delta to w on every prompt
// event. Composes additively with WithEvents and other event-related
// options — installing one does not displace any other.
//
// Errors from the writer are silently dropped: this is a convenience
// option, not a delivery-guaranteed pipe. Callers that need backpressure
// or error visibility should register a custom WithEvents handler
// instead.
//
// A nil writer is a no-op so callers can pass conditional writers
// without branching.
func WithStreamWriter(w io.Writer) PromptOption {
	return func(c *promptConfig) {
		if w == nil {
			return
		}
		c.auxEmits = append(c.auxEmits, func(e Event) {
			if e.Type == EventTextDelta && e.Delta != "" {
				_, _ = io.WriteString(w, e.Delta)
			}
		})
	}
}

// WithToolLogger mirrors EventToolStart events to w as
// "[tool] <name>\n". Composes additively with other event-related
// options. Errors from the writer are silently dropped; nil w is a
// no-op.
func WithToolLogger(w io.Writer) PromptOption {
	return func(c *promptConfig) {
		if w == nil {
			return
		}
		c.auxEmits = append(c.auxEmits, func(e Event) {
			if e.Type == EventToolStart && e.ToolName != "" {
				_, _ = fmt.Fprintf(w, "[tool] %s\n", e.ToolName)
			}
		})
	}
}
