// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package loop

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// blockingFirstTurn streams its first turn's events, then holds the stream
// open until the request context ends; later turns come from p.turns.
type blockingFirstTurn struct {
	mu       sync.Mutex
	first    []ProviderEvent
	turns    [][]ProviderEvent
	requests []ProviderRequest
	started  chan struct{}
}

func (p *blockingFirstTurn) Stream(ctx context.Context, req ProviderRequest) (<-chan ProviderEvent, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	n := len(p.requests)
	p.mu.Unlock()
	ch := make(chan ProviderEvent, 16)
	if n > 1 {
		for _, e := range p.turns[n-2] {
			ch <- e
		}
		close(ch)
		return ch, nil
	}
	go func() {
		defer close(ch)
		for _, e := range p.first {
			ch <- e
		}
		close(p.started)
		<-ctx.Done()
	}()
	return ch, nil
}

func textOf(m Message) string {
	var b strings.Builder
	for _, c := range m.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}

func TestInterruptCancelsRunningTool(t *testing.T) {
	p := &requestLog{turns: [][]ProviderEvent{
		{{Type: ProviderEventStart}, {Type: ProviderEventToolCall, ToolCall: &ToolCall{ID: "c1", Name: "sleep", Arguments: []byte(`{}`)}}, {Type: ProviderEventDone}},
		{{Type: ProviderEventStart}, {Type: ProviderEventTextDelta, Delta: "ok, stopping"}, {Type: ProviderEventDone}},
	}}
	interrupt := make(chan struct{}, 1)
	started := make(chan struct{})
	var mu sync.Mutex
	var queued []string
	var interrupted []Event
	go func() {
		<-started
		mu.Lock()
		queued = append(queued, "stop, use the cache instead")
		mu.Unlock()
		interrupt <- struct{}{}
	}()
	t0 := time.Now()
	res, err := Run(context.Background(), RunRequest{
		Provider: p,
		Messages: []Message{{Role: MessageRoleUser, Content: []ContentPart{{Type: ContentTypeText, Text: "start"}}}},
		Tools: []Tool{{ToolSpec: ToolSpec{Name: "sleep"}, Execute: func(ctx context.Context, _ ToolCall) (ToolResult, error) {
			close(started)
			select {
			case <-ctx.Done():
				return ToolResult{}, ctx.Err()
			case <-time.After(60 * time.Second):
				return ToolResult{Content: []ContentPart{{Type: ContentTypeText, Text: "slept"}}}, nil
			}
		}}},
		Steer: func() []string {
			mu.Lock()
			defer mu.Unlock()
			out := queued
			queued = nil
			return out
		},
		Interrupt: interrupt,
		Emit: func(e Event) {
			if e.Type == EventTurnInterrupted {
				interrupted = append(interrupted, e)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("interrupt took %s", d)
	}
	if len(interrupted) != 1 || interrupted[0].Metadata["canceled"] != "tools" || strings.Join(interrupted[0].Metadata["tools"].([]string), ",") != "sleep" {
		t.Fatalf("turn_interrupted = %+v", interrupted)
	}
	second := p.requests[1].Messages
	// assistant tool call, its tool result, then the developer's message
	if n := len(second); n < 3 || second[n-3].Role != MessageRoleAssistant || second[n-2].Role != MessageRoleTool || second[n-2].ToolCallID != "c1" || !strings.Contains(textOf(second[n-2]), "canceled by operator") || textOf(second[n-1]) != "stop, use the cache instead" {
		t.Fatalf("second request = %+v", second)
	}
	if textOf(res.Messages[len(res.Messages)-1]) != "ok, stopping" {
		t.Fatalf("final = %+v", res.Messages[len(res.Messages)-1])
	}
}

func TestInterruptDiscardsPartialTurn(t *testing.T) {
	p := &blockingFirstTurn{
		first:   []ProviderEvent{{Type: ProviderEventStart}, {Type: ProviderEventTextDelta, Delta: "PARTIAL half a thought"}},
		turns:   [][]ProviderEvent{{{Type: ProviderEventStart}, {Type: ProviderEventTextDelta, Delta: "redirected"}, {Type: ProviderEventDone}}},
		started: make(chan struct{}),
	}
	interrupt := make(chan struct{}, 1)
	var queued []string
	var mu sync.Mutex
	go func() {
		<-p.started
		mu.Lock()
		queued = append(queued, "actually, do the other thing")
		mu.Unlock()
		interrupt <- struct{}{}
	}()
	var kinds []string
	res, err := Run(context.Background(), RunRequest{
		Provider: p,
		Messages: []Message{{Role: MessageRoleUser, Content: []ContentPart{{Type: ContentTypeText, Text: "start"}}}},
		Steer: func() []string {
			mu.Lock()
			defer mu.Unlock()
			out := queued
			queued = nil
			return out
		},
		Interrupt: interrupt,
		Emit: func(e Event) {
			if e.Type == EventTurnInterrupted {
				kinds = append(kinds, e.Metadata["canceled"].(string))
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(kinds, ",") != "model" {
		t.Fatalf("interrupts = %v", kinds)
	}
	for _, m := range append(p.requests[1].Messages, res.Messages...) {
		if strings.Contains(textOf(m), "PARTIAL") {
			t.Fatalf("partial assistant text kept: %+v", m)
		}
	}
	if last := p.requests[1].Messages[len(p.requests[1].Messages)-1]; textOf(last) != "actually, do the other thing" {
		t.Fatalf("second request ends with %+v", last)
	}
	if textOf(res.Messages[len(res.Messages)-1]) != "redirected" {
		t.Fatalf("final = %+v", res.Messages)
	}
}

func TestInterruptBetweenTurnsIsDropped(t *testing.T) {
	p := &requestLog{turns: [][]ProviderEvent{{{Type: ProviderEventStart}, {Type: ProviderEventTextDelta, Delta: "fine"}, {Type: ProviderEventDone}}}}
	interrupt := make(chan struct{}, 1)
	interrupt <- struct{}{} // arrived before the run started
	res, err := Run(context.Background(), RunRequest{Provider: p, Messages: []Message{{Role: MessageRoleUser, Content: []ContentPart{{Type: ContentTypeText, Text: "hi"}}}}, Interrupt: interrupt})
	if err != nil || textOf(res.Messages[len(res.Messages)-1]) != "fine" || len(p.requests) != 1 {
		t.Fatalf("res %+v err %v requests %d", res.Messages, err, len(p.requests))
	}
}
