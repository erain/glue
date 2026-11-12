// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erain/glue"
)

// gatedProvider plays scripted turns and holds turn gateAt until released.
type gatedProvider struct {
	mu       sync.Mutex
	turns    [][]glue.ProviderEvent
	requests []glue.ProviderRequest
	gateAt   int
	reached  chan struct{}
	release  chan struct{}
}

func (p *gatedProvider) Stream(ctx context.Context, req glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	p.mu.Lock()
	n := len(p.requests)
	p.requests = append(p.requests, req)
	events := p.turns[n]
	p.mu.Unlock()
	if n == p.gateAt {
		close(p.reached)
		select {
		case <-p.release:
		case <-ctx.Done():
		}
	}
	ch := make(chan glue.ProviderEvent, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func TestOperatorMessageDeliveredEvent(t *testing.T) {
	p := &gatedProvider{gateAt: 1, reached: make(chan struct{}), release: make(chan struct{}), turns: [][]glue.ProviderEvent{
		turn(`{"items":[{"title":"add Shout"}]}`),
		{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "w1", Name: "write_file", Arguments: []byte(`{"path":"probe.txt","content":"x\n"}`)}}, {Type: glue.ProviderEventDone}},
		turn("created greet.go and shout.go as told"),
		turn(`{"done":true,"items":[{"title":"add Shout","done":true,"evidence":"ok"}],"summary":"done"}`),
	}}
	m, err := New(Options{StateDir: t.TempDir(), ProviderFactory: func(string) (glue.Provider, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	job, err := m.Create(context.Background(), Spec{Objective: "add Shout", Repo: RepoSpec{Path: repoFixture(t)}, Budgets: Budgets{MaxIterations: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("maker never started")
	}
	const msg = "Greet does not exist yet; create greet.go first"
	if err := m.Message(context.Background(), job.ID, msg); err != nil {
		t.Fatal(err)
	}
	close(p.release)
	if got := waitJob(t, m, job.ID); got.Status != Achieved {
		t.Fatalf("status = %s", got.Status)
	}
	p.mu.Lock()
	next := p.requests[2].Messages
	p.mu.Unlock()
	last := next[len(next)-1]
	if last.Role != glue.MessageRoleUser || !strings.Contains(last.Content[0].Text, msg) {
		t.Fatalf("maker turn after the message did not receive it: %+v", last)
	}
	events, err := m.Events(context.Background(), job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var delivered bool
	for _, ev := range events {
		if ev.Type == "operator_message_delivered" && ev.Phase == "maker" && strings.Contains(payloadText(ev.Payload), msg) && !strings.Contains(payloadText(ev.Payload), "Message from the developer") {
			delivered = true
		}
	}
	if !delivered {
		t.Fatal("no operator_message_delivered event in the maker phase")
	}
}

func payloadText(p any) string {
	if m, ok := p.(map[string]any); ok {
		s, _ := m["text"].(string)
		return s
	}
	return ""
}
