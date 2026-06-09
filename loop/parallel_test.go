package loop

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"
)

// scriptThreeToolCallsThenStop returns a fake provider whose first turn
// issues three tool calls, in order, and whose second turn returns "ok".
func scriptThreeToolCallsThenStop(calls ...ToolCall) *fakeProvider {
	first := []ProviderEvent{{Type: ProviderEventStart}}
	for i := range calls {
		c := calls[i]
		first = append(first, ProviderEvent{Type: ProviderEventToolCall, ToolCall: &c})
	}
	first = append(first, ProviderEvent{Type: ProviderEventDone})
	return &fakeProvider{turns: [][]ProviderEvent{
		first,
		{
			{Type: ProviderEventStart},
			{Type: ProviderEventTextDelta, Delta: "ok"},
			{Type: ProviderEventDone},
		},
	}}
}

func TestRunParallelOverlapsExecutors(t *testing.T) {
	t.Parallel()

	provider := scriptThreeToolCallsThenStop(
		ToolCall{ID: "a", Name: "wait", Arguments: json.RawMessage(`{"ms":80}`)},
		ToolCall{ID: "b", Name: "wait", Arguments: json.RawMessage(`{"ms":80}`)},
		ToolCall{ID: "c", Name: "wait", Arguments: json.RawMessage(`{"ms":80}`)},
	)
	wait := Tool{
		ToolSpec: ToolSpec{Name: "wait"},
		Execute: func(_ context.Context, call ToolCall) (ToolResult, error) {
			var args struct {
				MS int `json:"ms"`
			}
			if err := json.Unmarshal(call.Arguments, &args); err != nil {
				return ToolResult{}, err
			}
			time.Sleep(time.Duration(args.MS) * time.Millisecond)
			return ToolResult{Content: []ContentPart{{Type: ContentTypeText, Text: call.ID}}}, nil
		},
	}

	start := time.Now()
	res, err := Run(context.Background(), RunRequest{
		Provider: provider,
		Tools:    []Tool{wait},
		Parallel: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	elapsed := time.Since(start)

	// 3 tool calls × 80ms sequentially = 240ms; parallel should be ~80ms.
	// Allow generous slack for CI scheduling: must be < 200ms.
	if elapsed >= 200*time.Millisecond {
		t.Fatalf("elapsed = %v, want < 200ms (parallel should overlap waits)", elapsed)
	}

	// 5 new messages: assistant1, tool1, tool2, tool3, assistant2.
	if len(res.NewMessages) != 5 {
		t.Fatalf("NewMessages = %d, want 5", len(res.NewMessages))
	}
	wantOrder := []string{"a", "b", "c"}
	for i, want := range wantOrder {
		got := res.NewMessages[i+1]
		if got.ToolCallID != want {
			t.Fatalf("NewMessages[%d].ToolCallID = %q, want %q", i+1, got.ToolCallID, want)
		}
	}
}

func TestRunParallelPreservesSourceOrderEvenIfFinishOrderDiffers(t *testing.T) {
	t.Parallel()

	// Tool "a" sleeps longer than "b"; in parallel both run concurrently
	// but the loop must still append the tool messages in source order
	// (a then b) regardless of which finishes first.
	provider := scriptThreeToolCallsThenStop(
		ToolCall{ID: "a", Name: "wait", Arguments: json.RawMessage(`{"ms":80,"label":"first"}`)},
		ToolCall{ID: "b", Name: "wait", Arguments: json.RawMessage(`{"ms":10,"label":"second"}`)},
	)
	wait := Tool{
		ToolSpec: ToolSpec{Name: "wait"},
		Execute: func(_ context.Context, call ToolCall) (ToolResult, error) {
			var args struct {
				MS    int    `json:"ms"`
				Label string `json:"label"`
			}
			if err := json.Unmarshal(call.Arguments, &args); err != nil {
				return ToolResult{}, err
			}
			time.Sleep(time.Duration(args.MS) * time.Millisecond)
			return ToolResult{Content: []ContentPart{{Type: ContentTypeText, Text: args.Label}}}, nil
		},
	}

	res, err := Run(context.Background(), RunRequest{
		Provider: provider,
		Tools:    []Tool{wait},
		Parallel: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := res.NewMessages[1].Content[0].Text; got != "first" {
		t.Fatalf("[1].Text = %q, want first", got)
	}
	if got := res.NewMessages[2].Content[0].Text; got != "second" {
		t.Fatalf("[2].Text = %q, want second", got)
	}
}

func TestRunParallelEmitsToolEndsInSourceOrder(t *testing.T) {
	t.Parallel()

	provider := scriptThreeToolCallsThenStop(
		ToolCall{ID: "alpha", Name: "wait", Arguments: json.RawMessage(`{"ms":50}`)},
		ToolCall{ID: "beta", Name: "wait", Arguments: json.RawMessage(`{"ms":5}`)},
		ToolCall{ID: "gamma", Name: "wait", Arguments: json.RawMessage(`{"ms":25}`)},
	)
	wait := Tool{
		ToolSpec: ToolSpec{Name: "wait"},
		Execute: func(_ context.Context, call ToolCall) (ToolResult, error) {
			var args struct {
				MS int `json:"ms"`
			}
			_ = json.Unmarshal(call.Arguments, &args)
			time.Sleep(time.Duration(args.MS) * time.Millisecond)
			return ToolResult{Content: []ContentPart{{Type: ContentTypeText, Text: call.ID}}}, nil
		},
	}

	var endIDs []string
	_, err := Run(context.Background(), RunRequest{
		Provider: provider,
		Tools:    []Tool{wait},
		Parallel: true,
		Emit: func(e Event) {
			if e.Type == EventToolEnd {
				endIDs = append(endIDs, e.ToolCallID)
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"alpha", "beta", "gamma"}
	if len(endIDs) != len(want) {
		t.Fatalf("EventToolEnd ids = %v, want %v", endIDs, want)
	}
	for i, w := range want {
		if endIDs[i] != w {
			t.Fatalf("end[%d] = %q, want %q (full: %v)", i, endIDs[i], w, endIDs)
		}
	}
}

func TestRunParallelExecutesAllToolsConcurrently(t *testing.T) {
	t.Parallel()

	// Use a counter to verify all goroutines were running at the same time.
	// Each tool increments a "running" counter on entry, sleeps, decrements
	// on exit, and reports the peak it observed via the result text.
	var running atomic.Int32
	var peak atomic.Int32
	provider := scriptThreeToolCallsThenStop(
		ToolCall{ID: "a", Name: "concur", Arguments: json.RawMessage(`{}`)},
		ToolCall{ID: "b", Name: "concur", Arguments: json.RawMessage(`{}`)},
		ToolCall{ID: "c", Name: "concur", Arguments: json.RawMessage(`{}`)},
	)
	concur := Tool{
		ToolSpec: ToolSpec{Name: "concur"},
		Execute: func(_ context.Context, _ ToolCall) (ToolResult, error) {
			cur := running.Add(1)
			for {
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			time.Sleep(40 * time.Millisecond)
			running.Add(-1)
			return ToolResult{}, nil
		},
	}

	if _, err := Run(context.Background(), RunRequest{Provider: provider, Tools: []Tool{concur}, Parallel: true}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := peak.Load(); got < 2 {
		t.Fatalf("peak concurrency = %d, want >= 2 (parallel mode)", got)
	}
}

func TestRunSequentialDoesNotOverlap(t *testing.T) {
	t.Parallel()

	var running atomic.Int32
	var peak atomic.Int32
	provider := scriptThreeToolCallsThenStop(
		ToolCall{ID: "a", Name: "concur", Arguments: json.RawMessage(`{}`)},
		ToolCall{ID: "b", Name: "concur", Arguments: json.RawMessage(`{}`)},
	)
	concur := Tool{
		ToolSpec: ToolSpec{Name: "concur"},
		Execute: func(_ context.Context, _ ToolCall) (ToolResult, error) {
			cur := running.Add(1)
			for {
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			running.Add(-1)
			return ToolResult{}, nil
		},
	}

	// Default sequential.
	if _, err := Run(context.Background(), RunRequest{Provider: provider, Tools: []Tool{concur}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := peak.Load(); got != 1 {
		t.Fatalf("sequential peak = %d, want 1", got)
	}
}
