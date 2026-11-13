// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/erain/glue"

	"github.com/erain/glue/internal/eventlog"
)

// Message queues guidance for the next maker boundary of a created or active job.
// Accepted guidance is durable before return and is also shown to its checker.
func (m *Manager) Message(ctx context.Context, id, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return invalid("text", "required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("jobs: manager is closed")
	}
	if err := m.record(id, "operator_message", map[string]any{"text": text}, "", nil, ""); err != nil {
		return err
	}
	if run := m.active[id]; run != nil {
		run.mu.Lock()
		maker := run.maker
		run.mu.Unlock()
		// Deliver at the maker's next model turn. The durable guidance above
		// still reaches later iterations and the checker.
		maker.Steer(operatorSteerPrefix + text)
	}
	return nil
}

// operatorSteerPrefix marks a message steered into a running maker turn.
const operatorSteerPrefix = "Message from the developer, sent while you were working. Take it into account now:\n"

func steerMessageText(e glue.Event) string {
	if e.Message == nil {
		return ""
	}
	for _, c := range e.Message.Content {
		if c.Type == glue.ContentTypeText {
			return strings.TrimPrefix(c.Text, operatorSteerPrefix)
		}
	}
	return ""
}

func (m *Manager) guidance(ctx context.Context, id string, iteration int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	events, err := m.Events(ctx, id, 0)
	if err != nil {
		return "", err
	}
	delivered := map[string]bool{}
	var messages []eventlog.Envelope
	var texts []string
	for _, e := range events {
		if e.Type == "answer" {
			// Answers to ask_user are guidance for every later session too,
			// so a new maker or the checker does not ask again.
			var a struct{ Question, Text string }
			raw, _ := json.Marshal(e.Payload)
			if json.Unmarshal(raw, &a) == nil && a.Text != "" {
				texts = append(texts, fmt.Sprintf("You asked the developer: %q. The developer answered: %s", a.Question, a.Text))
			}
			continue
		}
		if e.Type != "operator_message" && e.Type != "operator_message_delivered" {
			continue
		}
		data, err := json.Marshal(e.Payload)
		if err != nil {
			return "", err
		}
		var payload struct {
			Text      string `json:"text"`
			MessageID string `json:"message_id"`
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			return "", fmt.Errorf("jobs: decode %s: %w", e.Type, err)
		}
		if e.Type == "operator_message_delivered" {
			delivered[payload.MessageID] = true
		} else {
			messages = append(messages, e)
			texts = append(texts, payload.Text)
		}
	}
	for _, message := range messages {
		if delivered[message.ID] {
			continue
		}
		if err := m.record(id, "operator_message_delivered", map[string]any{"message_id": message.ID, "iteration": iteration}, "maker", &iteration, ""); err != nil {
			return "", err
		}
	}
	// Keep guidance in subsequent fresh sessions, including after a restart.
	return strings.Join(texts, "\n\n"), nil
}

// Interrupt queues text like Message and also cancels the running maker's
// current model request or tool call, so its next turn starts with the text
// instead of waiting for the current step to finish.
func (m *Manager) Interrupt(ctx context.Context, id, text string) error {
	if err := m.Message(ctx, id, text); err != nil {
		return err
	}
	m.mu.Lock()
	run := m.active[id]
	m.mu.Unlock()
	if run != nil {
		run.mu.Lock()
		maker := run.maker
		run.mu.Unlock()
		maker.Interrupt()
	}
	return nil
}
