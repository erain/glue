package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// sseEvent is one parsed Server-Sent-Event frame. Event is the value of
// the "event:" line; Data is the JSON payload from the (possibly
// multi-line) "data:" lines, joined with newlines per the SSE spec.
type sseEvent struct {
	Event string
	Data  string
}

// readSSEEvents pulls one sseEvent at a time from r, sending them to
// the returned channel. The channel is closed when r EOFs, ctx is
// cancelled, or a read error occurs.
//
// The reader respects ctx.Done() between event blocks.
func readSSEEvents(ctx context.Context, r io.Reader) (<-chan sseEvent, <-chan error) {
	events := make(chan sseEvent)
	errs := make(chan error, 1)
	go func() {
		defer close(events)
		defer close(errs)

		scanner := bufio.NewScanner(r)
		// SSE event lines can be large (Codex SSE carries full
		// output items in a single data: line). Allow up to 4 MiB.
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

		var (
			event string
			data  strings.Builder
		)
		flush := func() bool {
			if event == "" && data.Len() == 0 {
				return true
			}
			select {
			case <-ctx.Done():
				return false
			case events <- sseEvent{Event: event, Data: data.String()}:
				event = ""
				data.Reset()
				return true
			}
		}

		for scanner.Scan() {
			if err := ctx.Err(); err != nil {
				errs <- err
				return
			}
			line := scanner.Text()
			switch {
			case line == "":
				if !flush() {
					return
				}
			case strings.HasPrefix(line, ":"):
				// SSE comment; keep-alive. Ignore.
			case strings.HasPrefix(line, "event:"):
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		// Flush any final event that wasn't terminated by a blank line.
		if event != "" || data.Len() > 0 {
			_ = flush()
		}
		if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
			errs <- fmt.Errorf("codex: read sse: %w", err)
		}
	}()
	return events, errs
}

// Responses SSE event payloads. Only the fields glue actually uses are
// modeled; everything else is ignored.

type respCreatedPayload struct {
	Response struct {
		ID    string `json:"id"`
		Model string `json:"model"`
	} `json:"response"`
}

type respTextDeltaPayload struct {
	Delta string `json:"delta"`
}

type respOutputItemDonePayload struct {
	Item struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		CallID    string          `json:"call_id"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"item"`
}

type respCompletedPayload struct {
	Response struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			InputTokens       int64 `json:"input_tokens"`
			OutputTokens      int64 `json:"output_tokens"`
			TotalTokens       int64 `json:"total_tokens"`
			CachedInputTokens int64 `json:"cached_input_tokens,omitempty"`
		} `json:"usage"`
	} `json:"response"`
}

type respFailedPayload struct {
	Response struct {
		ID    string `json:"id"`
		Error *struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	} `json:"response"`
}
