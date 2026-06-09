package glue

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// goalTurn scripts one assistant turn whose final message carries the given
// text and (optionally) a token-usage total, so goal-loop tests can drive both
// the maker/checker text and the token budget.
func goalTurn(text string, totalTokens int64) []ProviderEvent {
	msg := Message{
		Role:    MessageRoleAssistant,
		Content: []ContentPart{{Type: ContentTypeText, Text: text}},
	}
	if totalTokens > 0 {
		msg.Usage = &Usage{InputTokens: totalTokens, TotalTokens: totalTokens}
	}
	return []ProviderEvent{
		{Type: ProviderEventStart},
		{Type: ProviderEventTextDelta, Delta: text},
		{Type: ProviderEventDone, Message: &msg},
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func newGoalAgent(provider Provider) *Agent {
	return NewAgent(AgentOptions{Provider: provider, Model: "fake-1"})
}

func TestPursueGoalAchieved(t *testing.T) {
	t.Parallel()

	provider := &recordingProvider{turns: [][]ProviderEvent{
		goalTurn(mustJSON(t, goalPlan{Items: []ChecklistItem{{Title: "A"}, {Title: "B"}}}), 0),
		goalTurn("worked on A", 0),
		goalTurn(mustJSON(t, goalVerdict{Done: false, Items: []ChecklistItem{{Title: "A", Done: true, Evidence: "A.go"}, {Title: "B"}}, Summary: "B remains"}), 0),
		goalTurn("worked on B", 0),
		goalTurn(mustJSON(t, goalVerdict{Done: true, Items: []ChecklistItem{{Title: "A", Done: true}, {Title: "B", Done: true}}, Summary: "all done"}), 0),
	}}

	res, err := newGoalAgent(provider).PursueGoal(context.Background(), GoalSpec{Objective: "ship A and B", MaxIterations: 5})
	if err != nil {
		t.Fatalf("PursueGoal: %v", err)
	}
	if res.Status != GoalAchieved {
		t.Fatalf("status = %q, want achieved", res.Status)
	}
	if res.Iterations != 2 {
		t.Fatalf("iterations = %d, want 2", res.Iterations)
	}
	if !allDone(res.Checklist) || len(res.Checklist) != 2 {
		t.Fatalf("checklist = %+v, want 2 items all done", res.Checklist)
	}
	if res.Summary != "all done" {
		t.Fatalf("summary = %q, want 'all done'", res.Summary)
	}
	if provider.calls != 5 {
		t.Fatalf("provider calls = %d, want 5 (plan + 2×(maker+checker))", provider.calls)
	}
}

func TestPursueGoalMaxIterations(t *testing.T) {
	t.Parallel()

	// Never reaches all-done, but the remaining set changes each round so the
	// no-progress guard does not fire — the iteration cap does.
	provider := &recordingProvider{turns: [][]ProviderEvent{
		goalTurn(mustJSON(t, goalPlan{Items: []ChecklistItem{{Title: "A"}, {Title: "B"}}}), 0),
		goalTurn("iter1", 0),
		goalTurn(mustJSON(t, goalVerdict{Items: []ChecklistItem{{Title: "A", Done: true}, {Title: "B"}}}), 0),
		goalTurn("iter2", 0),
		goalTurn(mustJSON(t, goalVerdict{Items: []ChecklistItem{{Title: "A"}, {Title: "B", Done: true}}}), 0),
	}}

	res, err := newGoalAgent(provider).PursueGoal(context.Background(), GoalSpec{Objective: "x", MaxIterations: 2})
	if err != nil {
		t.Fatalf("PursueGoal: %v", err)
	}
	if res.Status != GoalMaxIterations {
		t.Fatalf("status = %q, want max_iterations", res.Status)
	}
	if res.Iterations != 2 {
		t.Fatalf("iterations = %d, want 2", res.Iterations)
	}
}

func TestPursueGoalBlockedOnNoProgress(t *testing.T) {
	t.Parallel()

	// Checker returns the same unfinished set every round → blocked.
	stuck := mustJSON(t, goalVerdict{Items: []ChecklistItem{{Title: "A"}, {Title: "B"}}, Summary: "no change"})
	provider := &recordingProvider{turns: [][]ProviderEvent{
		goalTurn(mustJSON(t, goalPlan{Items: []ChecklistItem{{Title: "A"}, {Title: "B"}}}), 0),
		goalTurn("iter1", 0), goalTurn(stuck, 0),
		goalTurn("iter2", 0), goalTurn(stuck, 0),
		goalTurn("iter3", 0), goalTurn(stuck, 0),
	}}

	res, err := newGoalAgent(provider).PursueGoal(context.Background(), GoalSpec{Objective: "x", MaxIterations: 10, NoProgressLimit: 2})
	if err != nil {
		t.Fatalf("PursueGoal: %v", err)
	}
	if res.Status != GoalBlocked {
		t.Fatalf("status = %q, want blocked", res.Status)
	}
	if res.Iterations != 3 {
		t.Fatalf("iterations = %d, want 3 (blocks once unchanged for NoProgressLimit rounds)", res.Iterations)
	}
}

func TestPursueGoalBudgetLimited(t *testing.T) {
	t.Parallel()

	notDone := mustJSON(t, goalVerdict{Items: []ChecklistItem{{Title: "A"}, {Title: "B"}}})
	provider := &recordingProvider{turns: [][]ProviderEvent{
		goalTurn(mustJSON(t, goalPlan{Items: []ChecklistItem{{Title: "A"}, {Title: "B"}}}), 0),
		goalTurn("iter1", 100), goalTurn(notDone, 0),
		goalTurn("iter2", 100), goalTurn(notDone, 0),
	}}

	res, err := newGoalAgent(provider).PursueGoal(context.Background(), GoalSpec{Objective: "x", MaxIterations: 10, TokenBudget: 150})
	if err != nil {
		t.Fatalf("PursueGoal: %v", err)
	}
	if res.Status != GoalBudgetLimited {
		t.Fatalf("status = %q, want budget_limited", res.Status)
	}
	if res.Usage.TotalTokens != 200 {
		t.Fatalf("usage total = %d, want 200", res.Usage.TotalTokens)
	}
	if provider.calls != 5 {
		t.Fatalf("provider calls = %d, want 5 (budget trips at top of iteration 3)", provider.calls)
	}
}

func TestPursueGoalPlanFallbackToObjective(t *testing.T) {
	t.Parallel()

	// Planner yields no items → the whole objective becomes one deliverable,
	// which the first audit confirms.
	provider := &recordingProvider{turns: [][]ProviderEvent{
		goalTurn(mustJSON(t, goalPlan{Items: nil}), 0),
		goalTurn("did it", 0),
		goalTurn(mustJSON(t, goalVerdict{Done: true, Items: []ChecklistItem{{Title: "build the thing", Done: true}}}), 0),
	}}

	res, err := newGoalAgent(provider).PursueGoal(context.Background(), GoalSpec{Objective: "build the thing", MaxIterations: 3})
	if err != nil {
		t.Fatalf("PursueGoal: %v", err)
	}
	if res.Status != GoalAchieved {
		t.Fatalf("status = %q, want achieved", res.Status)
	}
}

func TestPursueGoalSeededChecklistSkipsPlanning(t *testing.T) {
	t.Parallel()

	// A seeded checklist (the resume path) must go straight to the maker —
	// the scripted turns contain no plan response, so a planning call would
	// desynchronize the provider script and fail the assertions below.
	provider := &recordingProvider{turns: [][]ProviderEvent{
		goalTurn("finished B", 0),
		goalTurn(mustJSON(t, goalVerdict{Done: true, Items: []ChecklistItem{{Title: "A", Done: true, Evidence: "A.go"}, {Title: "B", Done: true}}, Summary: "done"}), 0),
	}}

	var planned []ChecklistItem
	res, err := newGoalAgent(provider).PursueGoal(context.Background(), GoalSpec{
		Objective: "ship A and B",
		Checklist: []ChecklistItem{{Title: "A", Done: true, Evidence: "A.go"}, {Title: "B"}},
		Emit: func(ev GoalEvent) {
			if ev.Type == GoalEventPlan {
				planned = ev.Checklist
			}
		},
	})
	if err != nil {
		t.Fatalf("PursueGoal: %v", err)
	}
	if res.Status != GoalAchieved {
		t.Fatalf("status = %q, want achieved", res.Status)
	}
	if provider.calls != 2 {
		t.Fatalf("provider calls = %d, want 2 (no planning call)", provider.calls)
	}
	if len(planned) != 2 || !planned[0].Done || planned[0].Evidence != "A.go" || planned[1].Done {
		t.Fatalf("plan event checklist = %+v, want seeded state preserved", planned)
	}
}

func TestPursueGoalValidation(t *testing.T) {
	t.Parallel()

	if _, err := newGoalAgent(&recordingProvider{}).PursueGoal(context.Background(), GoalSpec{Objective: "   "}); err == nil || !strings.Contains(err.Error(), "objective is required") {
		t.Fatalf("err = %v, want objective-required", err)
	}
	if _, err := NewAgent(AgentOptions{}).PursueGoal(context.Background(), GoalSpec{Objective: "x"}); err == nil || !strings.Contains(err.Error(), "provider is required") {
		t.Fatalf("err = %v, want provider-required", err)
	}
}

func TestPursueGoalEmitsEvents(t *testing.T) {
	t.Parallel()

	provider := &recordingProvider{turns: [][]ProviderEvent{
		goalTurn(mustJSON(t, goalPlan{Items: []ChecklistItem{{Title: "A"}}}), 0),
		goalTurn("did A", 0),
		goalTurn(mustJSON(t, goalVerdict{Done: true, Items: []ChecklistItem{{Title: "A", Done: true}}}), 0),
	}}

	var seen []GoalEventType
	res, err := newGoalAgent(provider).PursueGoal(context.Background(), GoalSpec{
		Objective: "x",
		Emit:      func(ev GoalEvent) { seen = append(seen, ev.Type) },
	})
	if err != nil {
		t.Fatalf("PursueGoal: %v", err)
	}
	if res.Status != GoalAchieved {
		t.Fatalf("status = %q, want achieved", res.Status)
	}
	want := []GoalEventType{GoalEventPlan, GoalEventIterationStart, GoalEventMakerDone, GoalEventVerdict, GoalEventDone}
	if strings.Join(typeStrings(seen), ",") != strings.Join(typeStrings(want), ",") {
		t.Fatalf("events = %v, want %v", seen, want)
	}
}

func typeStrings(types []GoalEventType) []string {
	out := make([]string, len(types))
	for i, t := range types {
		out[i] = string(t)
	}
	return out
}
