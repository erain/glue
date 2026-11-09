// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
	return m.record(id, "operator_message", map[string]any{"text": text}, "", nil, "")
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
