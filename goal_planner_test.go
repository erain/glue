// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package glue

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func plannerToolTurn(id, args string) []ProviderEvent {
	return []ProviderEvent{
		{Type: ProviderEventStart},
		{Type: ProviderEventToolCall, ToolCall: &ToolCall{ID: id, Name: "look", Arguments: []byte(args)}},
		{Type: ProviderEventDone},
	}
}

func plannerAgent(provider Provider) *Agent {
	return NewAgent(AgentOptions{Provider: provider, Model: "fake-1", Tools: []Tool{{
		ToolSpec: ToolSpec{Name: "look", Parameters: []byte(`{"type":"object"}`)},
		Execute: func(_ context.Context, call ToolCall) (ToolResult, error) {
			return TextResult("saw " + string(call.Arguments)), nil
		},
	}}})
}

func TestPlannerToolTurnCap(t *testing.T) {
	t.Parallel()
	var turns [][]ProviderEvent
	for i := range PlannerMaxToolTurns + 1 {
		turns = append(turns, plannerToolTurn(fmt.Sprintf("c%d", i), fmt.Sprintf(`{"path":"dir%d"}`, i)))
	}
	turns = append(turns,
		goalTurn(mustJSON(t, goalPlan{Items: []ChecklistItem{{Title: "add Shout"}, {Title: "test Shout"}}}), 0),
		goalTurn("did it", 0),
		goalTurn(mustJSON(t, goalVerdict{Done: true, Items: []ChecklistItem{{Title: "add Shout", Done: true}, {Title: "test Shout", Done: true}}}), 0),
	)
	provider := &recordingProvider{turns: turns}
	res, err := plannerAgent(provider).PursueGoal(context.Background(), GoalSpec{Objective: "add Shout", MaxIterations: 1})
	if err != nil {
		t.Fatalf("PursueGoal: %v", err)
	}
	if res.Status != GoalAchieved || len(res.Checklist) != 2 {
		t.Fatalf("result = %+v, want achieved with the 2-item plan from the final request", res)
	}
	final := provider.requests[PlannerMaxToolTurns+1]
	if len(final.Tools) != 0 {
		t.Fatalf("final plan request offered %d tools, want none", len(final.Tools))
	}
	last := final.Messages[len(final.Messages)-1]
	if text := last.Content[0].Text; !strings.Contains(text, `saw {"path":"dir6"}`) || !strings.Contains(text, "You have no tools now") {
		t.Fatalf("final plan prompt lacks observations: %q", text)
	}
}

func TestPlannerRepeatedCallsGetFinalPlan(t *testing.T) {
	t.Parallel()
	var turns [][]ProviderEvent
	for i := range 5 {
		turns = append(turns, plannerToolTurn(fmt.Sprintf("c%d", i), `{"path":"."}`))
	}
	turns = append(turns,
		goalTurn(mustJSON(t, goalPlan{Items: []ChecklistItem{{Title: "one"}, {Title: "two"}}}), 0),
		goalTurn("did it", 0),
		goalTurn(mustJSON(t, goalVerdict{Done: true, Items: []ChecklistItem{{Title: "one", Done: true}, {Title: "two", Done: true}}}), 0),
	)
	provider := &recordingProvider{turns: turns}
	res, err := plannerAgent(provider).PursueGoal(context.Background(), GoalSpec{Objective: "plan me", MaxIterations: 1})
	if err != nil {
		t.Fatalf("PursueGoal: %v", err)
	}
	if res.Status != GoalAchieved || len(res.Checklist) != 2 || res.Checklist[0].Title != "one" {
		t.Fatalf("result = %+v, want the plan from the final request", res)
	}
	if got := len(provider.requests); got != 8 {
		t.Fatalf("provider calls = %d, want 5 planner + 1 final + maker + checker", got)
	}
}

func TestPlannerMaxTurnsRespectsSpec(t *testing.T) {
	t.Parallel()
	if got := plannerMaxTurns(0); got != PlannerMaxToolTurns+1 {
		t.Fatalf("plannerMaxTurns(0) = %d", got)
	}
	if got := plannerMaxTurns(3); got != 3 {
		t.Fatalf("plannerMaxTurns(3) = %d", got)
	}
}

func TestCheckerTurnCapGivesVerdict(t *testing.T) {
	t.Parallel()
	turns := [][]ProviderEvent{
		goalTurn(mustJSON(t, goalPlan{Items: []ChecklistItem{{Title: "hello.txt says hi"}}}), 0),
		goalTurn("wrote hello.txt", 0),
	}
	for i := range CheckerMaxToolTurns + 1 {
		turns = append(turns, plannerToolTurn(fmt.Sprintf("k%d", i), fmt.Sprintf(`{"check":%d}`, i)))
	}
	turns = append(turns, goalTurn(mustJSON(t, goalVerdict{Done: true, Items: []ChecklistItem{{Title: "hello.txt says hi", Done: true, Evidence: "cat shows hi"}}, Summary: "done"}), 0))
	provider := &recordingProvider{turns: turns}
	agent := plannerAgent(provider)
	res, err := agent.PursueGoal(context.Background(), GoalSpec{Objective: "hello", MaxIterations: 1, CheckerTools: agent.tools})
	if err != nil || res.Status != GoalAchieved {
		t.Fatalf("result = %+v, %v; want achieved via the final verdict", res, err)
	}
	final := provider.requests[len(provider.requests)-1]
	last := final.Messages[len(final.Messages)-1].Content[0].Text
	if len(final.Tools) != 0 || !strings.Contains(last, `saw {"check":12}`) || !strings.Contains(last, "You have no tools now") {
		t.Fatalf("final verdict request: tools=%d text=%q", len(final.Tools), last)
	}
}

func TestMakerMaxTurnsReachesChecker(t *testing.T) {
	t.Parallel()
	provider := &recordingProvider{turns: [][]ProviderEvent{
		goalTurn(mustJSON(t, goalPlan{Items: []ChecklistItem{{Title: "edit"}}}), 0),
		plannerToolTurn("m1", `{"path":"a"}`),
		plannerToolTurn("m2", `{"path":"b"}`),
		goalTurn(mustJSON(t, goalVerdict{Done: true, Items: []ChecklistItem{{Title: "edit", Done: true, Evidence: "ok"}}, Summary: "done"}), 0),
	}}
	res, err := plannerAgent(provider).PursueGoal(context.Background(), GoalSpec{Objective: "edit", MaxIterations: 1, MaxTurns: 2})
	if err != nil || res.Status != GoalAchieved {
		t.Fatalf("result = %+v, %v; a maker that runs out of turns should still be checked", res, err)
	}
}
