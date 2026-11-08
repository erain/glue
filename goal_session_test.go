package glue

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

func TestGoalSessionHook(t *testing.T) {
	provider := &recordingProvider{turns: [][]ProviderEvent{
		goalTurn(mustJSON(t, goalPlan{Items: []ChecklistItem{{Title: "A"}}}), 0),
		{{Type: ProviderEventStart}, {Type: ProviderEventToolCall, ToolCall: &ToolCall{ID: "call", Name: "observe", Arguments: []byte(`{}`)}}, {Type: ProviderEventDone}},
		goalTurn("made A", 0),
		goalTurn(mustJSON(t, goalVerdict{Done: true, Items: []ChecklistItem{{Title: "A", Done: true}}}), 0),
	}}
	tool := Tool{ToolSpec: ToolSpec{Name: "observe"}, Execute: func(context.Context, ToolCall) (ToolResult, error) { return TextResult("ok"), nil }}
	var mu sync.Mutex
	seen := map[string]map[EventType]int{}
	var phases []string
	result, err := newGoalAgent(provider).PursueGoal(context.Background(), GoalSpec{
		Objective: "finish A", StartIteration: 7, MaxIterations: 1, Tools: []Tool{tool},
		OnSession: func(phase string, iteration int, s *Session) {
			key := fmt.Sprintf("%s:%d", phase, iteration)
			phases = append(phases, key)
			seen[key] = map[EventType]int{}
			unsubscribe := s.Subscribe(func(e Event) { mu.Lock(); defer mu.Unlock(); seen[key][e.Type]++ })
			t.Cleanup(unsubscribe)
		},
	})
	if err != nil || result.Status != GoalAchieved {
		t.Fatalf("result %+v err %v", result, err)
	}
	if fmt.Sprint(phases) != "[planner:0 maker:7 checker:7]" {
		t.Fatal(phases)
	}
	for _, phase := range phases {
		if seen[phase][EventType("loop_start")] == 0 {
			t.Fatalf("%s missed loop start: %v", phase, seen[phase])
		}
	}
	for _, typ := range []EventType{"tool_start", "tool_end"} {
		if seen["maker:7"][typ] != 1 {
			t.Fatalf("maker %s: %v", typ, seen["maker:7"])
		}
	}
}
