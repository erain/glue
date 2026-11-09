// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package glue

import (
	"context"
	"errors"
	"testing"
)

type failingGoalStore struct {
	*memStore
	failAt, writes int
	err            error
}

func (s *failingGoalStore) Save(ctx context.Context, id string, state SessionState) error {
	if id == "checkpoint" {
		s.writes++
		if s.writes == s.failAt {
			return s.err
		}
	}
	return s.memStore.Save(ctx, id, state)
}
func TestRequiredGoalCheckpointFailures(t *testing.T) {
	for _, tc := range []struct {
		name          string
		failAt, calls int
	}{{"before planning", 1, 0}, {"before maker", 2, 1}, {"after checker", 3, 3}, {"terminal", 4, 3}} {
		t.Run(tc.name, func(t *testing.T) {
			sentinel := errors.New("disk unavailable")
			store := &failingGoalStore{memStore: newMemStore(), failAt: tc.failAt, err: sentinel}
			done := tc.name == "terminal"
			verdict := goalVerdict{Done: done, Items: []ChecklistItem{{Title: "A", Done: done}}}
			p := &recordingProvider{turns: [][]ProviderEvent{goalTurn(`{"items":[{"title":"A"}]}`, 0), goalTurn("work", 0), goalTurn(mustJSON(t, verdict), 0)}}
			agent := NewAgent(AgentOptions{Provider: p, Store: store})
			sawDone := false
			result, err := agent.PursueGoal(context.Background(), GoalSpec{Objective: "A", SessionPrefix: "checkpoint", RequirePersistence: true, Emit: func(e GoalEvent) {
				if e.Type == GoalEventDone {
					sawDone = true
				}
			}})
			if !errors.Is(err, sentinel) || p.calls != tc.calls || result.Status != GoalErrored || sawDone {
				t.Fatalf("result %+v err %v calls %d done %v", result, err, p.calls, sawDone)
			}
		})
	}
}
func TestRequiredGoalCheckpointBeforePlanning(t *testing.T) {
	store := newMemStore()
	p := &recordingProvider{turns: [][]ProviderEvent{goalTurn(`{"items":[{"title":"A"}]}`, 0), goalTurn("work", 0), goalTurn(`{"done":true,"items":[{"title":"A","done":true}]}`, 0)}}
	agent := NewAgent(AgentOptions{Provider: p, Store: store})
	_, err := agent.PursueGoal(context.Background(), GoalSpec{Objective: "A", SessionPrefix: "checkpoint", RequirePersistence: true, OnSession: func(phase string, _ int, _ *Session) {
		if phase == "planner" {
			rec, ok, err := agent.LoadGoal(context.Background(), "checkpoint")
			if err != nil || !ok || rec.Objective != "A" || rec.Status != GoalRunning {
				t.Fatalf("initial record %+v found %v err %v", rec, ok, err)
			}
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
}
func TestGoalCheckpointBestEffortAndRequiredStore(t *testing.T) {
	for _, required := range []bool{false, true} {
		t.Run(map[bool]string{false: "best effort", true: "required"}[required], func(t *testing.T) {
			p := &recordingProvider{turns: [][]ProviderEvent{goalTurn("work", 0), goalTurn(`{"done":true,"items":[{"title":"A","done":true}]}`, 0)}}
			result, err := newGoalAgent(p).PursueGoal(context.Background(), GoalSpec{Objective: "A", Checklist: []ChecklistItem{{Title: "A"}}, RequirePersistence: required})
			if required {
				if err == nil || p.calls != 0 || len(result.Checklist) != 1 {
					t.Fatalf("required store err %v calls %d", err, p.calls)
				}
			} else if err != nil || result.Status != GoalAchieved {
				t.Fatalf("result %+v err %v", result, err)
			}
		})
	}
}
func TestResumedGoalInitialCheckpointPreservesCounters(t *testing.T) {
	store := newMemStore()
	agent := NewAgent(AgentOptions{Provider: &recordingProvider{}, Store: store})
	result, err := agent.PursueGoal(context.Background(), GoalSpec{Objective: "A", SessionPrefix: "checkpoint", Checklist: []ChecklistItem{{Title: "A"}}, StartIteration: 5, InitialUsage: Usage{TotalTokens: 50}, TokenBudget: 50, RequirePersistence: true})
	if err != nil || result.Status != GoalBudgetLimited || result.Iterations != 4 || result.Usage.TotalTokens != 50 {
		t.Fatalf("result %+v err %v", result, err)
	}
	rec, ok, err := agent.LoadGoal(context.Background(), "checkpoint")
	if err != nil || !ok || rec.Iterations != 4 || rec.Usage.TotalTokens != 50 {
		t.Fatalf("record %+v found %v err %v", rec, ok, err)
	}
}
func TestSaveInitialGoalRecord(t *testing.T) {
	agent := NewAgent(AgentOptions{Store: newMemStore()})
	rec := GoalRecord{ID: "checkpoint", Objective: "A", Status: GoalRunning, WorkDir: "workspace"}
	if err := agent.SaveGoal(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	got, ok, err := agent.LoadGoal(context.Background(), rec.ID)
	if err != nil || !ok || got.WorkDir != rec.WorkDir {
		t.Fatalf("record %+v found %v err %v", got, ok, err)
	}
	for _, rec := range []GoalRecord{{}, {ID: "checkpoint"}, {Objective: "A"}} {
		if err := agent.SaveGoal(context.Background(), rec); err == nil {
			t.Fatal("accepted absent identity/objective")
		}
	}
	if err := (*Agent)(nil).SaveGoal(context.Background(), rec); err == nil {
		t.Fatal("accepted nil agent")
	}
}

func TestRequiredBudgetCheckpointFailure(t *testing.T) {
	sentinel := errors.New("disk unavailable")
	store := &failingGoalStore{memStore: newMemStore(), failAt: 3, err: sentinel}
	agent := NewAgent(AgentOptions{Provider: &recordingProvider{}, Store: store})
	result, err := agent.PursueGoal(context.Background(), GoalSpec{Objective: "A", SessionPrefix: "checkpoint", Checklist: []ChecklistItem{{Title: "A"}}, InitialUsage: Usage{TotalTokens: 1}, TokenBudget: 1, RequirePersistence: true})
	if !errors.Is(err, sentinel) || result.Status != GoalErrored {
		t.Fatalf("result %+v err %v", result, err)
	}
}

func TestGoalCheckpointFailureRemainsBestEffortByDefault(t *testing.T) {
	store := &failingGoalStore{memStore: newMemStore(), failAt: 1, err: errors.New("disk unavailable")}
	p := &recordingProvider{turns: [][]ProviderEvent{goalTurn("work", 0), goalTurn(`{"done":true,"items":[{"title":"A","done":true}]}`, 0)}}
	result, err := NewAgent(AgentOptions{Provider: p, Store: store}).PursueGoal(context.Background(), GoalSpec{Objective: "A", SessionPrefix: "checkpoint", Checklist: []ChecklistItem{{Title: "A"}}})
	if err != nil || result.Status != GoalAchieved {
		t.Fatalf("result %+v err %v", result, err)
	}
}
