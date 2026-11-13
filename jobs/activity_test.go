// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"testing"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/internal/eventlog"
)

func activityEvents() []eventlog.Envelope {
	t0 := time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)
	it := 1
	usage := func(n int64) *glue.Usage { return &glue.Usage{InputTokens: n, TotalTokens: n} }
	return []eventlog.Envelope{
		{Seq: 1, Time: t0, Type: "plan", Phase: "planner", Payload: glue.GoalEvent{Type: glue.GoalEventPlan, Usage: glue.Usage{TotalTokens: 1000}}},
		{Seq: 2, Time: t0.Add(time.Second), Type: "iteration_start", Phase: "maker", Iteration: &it, Payload: glue.GoalEvent{Type: glue.GoalEventIterationStart, Iteration: 1, Usage: glue.Usage{TotalTokens: 1000}}},
		{Seq: 3, Time: t0.Add(2 * time.Second), Type: "tool_start", Phase: "maker", Payload: glue.Event{Type: glue.EventToolStart, ToolName: "read_file"}},
		{Seq: 4, Time: t0.Add(3 * time.Second), Type: "turn_end", Phase: "maker", Payload: glue.Event{Type: glue.EventTurnEnd, Message: &glue.Message{Role: glue.MessageRoleAssistant, Usage: usage(500)}}},
		{Seq: 5, Time: t0.Add(4 * time.Second), Type: "tool_start", Phase: "maker", Payload: glue.Event{Type: glue.EventToolStart, ToolName: "shell_exec"}},
		{Seq: 6, Time: t0.Add(5 * time.Second), Type: "turn_end", Phase: "maker", Payload: glue.Event{Type: glue.EventTurnEnd, Message: &glue.Message{Role: glue.MessageRoleAssistant, Usage: usage(700)}}},
	}
}

func TestDetailUsageUpdatesPerTurn(t *testing.T) {
	d, _, err := SummarizeEvents(Job{ID: "job-1"}, activityEvents())
	if err != nil {
		t.Fatal(err)
	}
	if d.Usage.TotalTokens != 2200 {
		t.Fatalf("usage = %d, want 1000 from the goal plus 1200 from two turns", d.Usage.TotalTokens)
	}
	// A later goal event carries cumulative usage and replaces the running sum.
	events := append(activityEvents(), eventlog.Envelope{Seq: 7, Type: "maker_done", Phase: "maker", Payload: glue.GoalEvent{Type: glue.GoalEventMakerDone, Iteration: 1, Usage: glue.Usage{TotalTokens: 2300}}})
	d, _, _ = SummarizeEvents(Job{ID: "job-1"}, events)
	if d.Usage.TotalTokens != 2300 {
		t.Fatalf("usage after maker_done = %d, want 2300", d.Usage.TotalTokens)
	}
}

func TestDetailActivity(t *testing.T) {
	d, _, err := SummarizeEvents(Job{ID: "job-1"}, activityEvents())
	if err != nil {
		t.Fatal(err)
	}
	a := d.Activity
	if a.Phase != "maker" || a.LastTool != "shell_exec" || a.ModelTurns != 2 || !a.LastEventAt.Equal(time.Date(2026, 11, 12, 15, 0, 5, 0, time.UTC)) {
		t.Fatalf("activity = %+v", a)
	}
}
