// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package loop

import (
	"context"
	"sync"
	"testing"
)

// requestLog records every request a scripted provider receives.
type requestLog struct {
	mu       sync.Mutex
	turns    [][]ProviderEvent
	requests []ProviderRequest
}

func (p *requestLog) Stream(_ context.Context, req ProviderRequest) (<-chan ProviderEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	events := p.turns[len(p.requests)-1]
	ch := make(chan ProviderEvent, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func TestSteeringMessageReachesNextTurn(t *testing.T) {
	p := &requestLog{turns: [][]ProviderEvent{
		{{Type: ProviderEventStart}, {Type: ProviderEventToolCall, ToolCall: &ToolCall{ID: "c1", Name: "work", Arguments: []byte(`{}`)}}, {Type: ProviderEventDone}},
		{{Type: ProviderEventStart}, {Type: ProviderEventTextDelta, Delta: "ok"}, {Type: ProviderEventDone}},
	}}
	var queued []string
	var steered []Event
	_, err := Run(context.Background(), RunRequest{
		Provider: p,
		Messages: []Message{{Role: MessageRoleUser, Content: []ContentPart{{Type: ContentTypeText, Text: "start"}}}},
		Tools: []Tool{{ToolSpec: ToolSpec{Name: "work"}, Execute: func(context.Context, ToolCall) (ToolResult, error) {
			queued = append(queued, "stop searching; Greet does not exist yet")
			return ToolResult{Content: []ContentPart{{Type: ContentTypeText, Text: "done"}}}, nil
		}}},
		Steer: func() []string { out := queued; queued = nil; return out },
		Emit: func(e Event) {
			if e.Type == EventSteer {
				steered = append(steered, e)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.requests) != 2 {
		t.Fatalf("requests = %d", len(p.requests))
	}
	first, second := p.requests[0].Messages, p.requests[1].Messages
	last := second[len(second)-1]
	if len(first) != 1 || last.Role != MessageRoleUser || last.Content[0].Text != "stop searching; Greet does not exist yet" {
		t.Fatalf("second request should end with the steered message, got %+v", last)
	}
	if second[len(second)-2].Role != MessageRoleTool {
		t.Fatalf("steered message must follow the tool result, got %+v", second)
	}
	if len(steered) != 1 || steered[0].Message == nil {
		t.Fatalf("steer events = %+v", steered)
	}
}
