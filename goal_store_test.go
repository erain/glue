package glue

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"
)

// listingMemStore adds the optional SessionLister capability to memStore so
// goal-record tests can exercise ListGoals.
type listingMemStore struct{ *memStore }

func (s listingMemStore) ListSessions(_ context.Context, opts ListSessionsOptions) ([]SessionSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []SessionSummary
	for id, st := range s.states {
		if opts.Prefix != "" && !strings.HasPrefix(id, opts.Prefix) {
			continue
		}
		out = append(out, SessionSummary{ID: id, CreatedAt: st.CreatedAt, UpdatedAt: st.UpdatedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, nil
}

func TestPursueGoalCheckpointsRecord(t *testing.T) {
	t.Parallel()

	store := listingMemStore{newMemStore()}
	provider := &recordingProvider{turns: [][]ProviderEvent{
		goalTurn(mustJSON(t, goalPlan{Items: []ChecklistItem{{Title: "A"}}}), 0),
		goalTurn("worked on A", 0),
		goalTurn(mustJSON(t, goalVerdict{Done: true, Items: []ChecklistItem{{Title: "A", Done: true, Evidence: "A.go"}}, Summary: "all done"}), 0),
	}}
	agent := NewAgent(AgentOptions{Provider: provider, Model: "fake-1", Store: store})

	res, err := agent.PursueGoal(context.Background(), GoalSpec{Objective: "ship A", SessionPrefix: "goal-rec1", WorkDir: "/tmp/wt"})
	if err != nil {
		t.Fatalf("PursueGoal: %v", err)
	}
	if res.Status != GoalAchieved {
		t.Fatalf("status = %q, want achieved", res.Status)
	}

	rec, ok, err := agent.LoadGoal(context.Background(), "goal-rec1")
	if err != nil || !ok {
		t.Fatalf("LoadGoal: ok=%v err=%v", ok, err)
	}
	if rec.Objective != "ship A" || rec.Status != GoalAchieved || rec.Iterations != 1 {
		t.Fatalf("record = %+v, want achieved after 1 iteration", rec)
	}
	if len(rec.Checklist) != 1 || !rec.Checklist[0].Done || rec.Checklist[0].Evidence != "A.go" {
		t.Fatalf("record checklist = %+v, want verified item round-tripped", rec.Checklist)
	}
	if rec.Summary != "all done" {
		t.Fatalf("record summary = %q", rec.Summary)
	}
	if rec.WorkDir != "/tmp/wt" {
		t.Fatalf("record workdir = %q, want /tmp/wt round-tripped", rec.WorkDir)
	}
	if rec.Resumable() {
		t.Fatal("achieved goal must not be resumable")
	}
}

func TestPursueGoalCancelPersistsPaused(t *testing.T) {
	t.Parallel()

	store := listingMemStore{newMemStore()}
	agent := NewAgent(AgentOptions{Provider: &recordingProvider{}, Model: "fake-1", Store: store})

	ctx, cancel := context.WithCancel(context.Background())
	seed := []ChecklistItem{{Title: "A", Done: true}, {Title: "B"}}
	_, err := agent.PursueGoal(ctx, GoalSpec{
		Objective:     "ship it",
		SessionPrefix: "goal-rec2",
		Checklist:     seed,
		// Cancel as soon as the (seeded) plan event fires, before the
		// first maker call — the loop's top-of-iteration check trips.
		Emit: func(ev GoalEvent) {
			if ev.Type == GoalEventPlan {
				cancel()
			}
		},
	})
	if err == nil {
		t.Fatal("want cancellation error")
	}

	rec, ok, err := agent.LoadGoal(context.Background(), "goal-rec2")
	if err != nil || !ok {
		t.Fatalf("LoadGoal: ok=%v err=%v", ok, err)
	}
	if rec.Status != GoalPaused {
		t.Fatalf("status = %q, want paused", rec.Status)
	}
	if len(rec.Checklist) != 2 || !rec.Checklist[0].Done {
		t.Fatalf("checklist = %+v, want seeded state preserved", rec.Checklist)
	}
	if !rec.Resumable() {
		t.Fatal("paused goal with checklist must be resumable")
	}
}

func TestPursueGoalStartIterationNumbersSessions(t *testing.T) {
	t.Parallel()

	store := listingMemStore{newMemStore()}
	notDone := mustJSON(t, goalVerdict{Items: []ChecklistItem{{Title: "A"}}})
	provider := &recordingProvider{turns: [][]ProviderEvent{
		goalTurn("iter3 work", 0),
		goalTurn(notDone, 0),
	}}
	agent := NewAgent(AgentOptions{Provider: provider, Model: "fake-1", Store: store})

	res, err := agent.PursueGoal(context.Background(), GoalSpec{
		Objective:      "x",
		SessionPrefix:  "goal-rec3",
		Checklist:      []ChecklistItem{{Title: "A"}},
		StartIteration: 3,
		MaxIterations:  1,
	})
	if err != nil {
		t.Fatalf("PursueGoal: %v", err)
	}
	if res.Status != GoalMaxIterations {
		t.Fatalf("status = %q, want max_iterations", res.Status)
	}
	if res.Iterations != 3 {
		t.Fatalf("iterations = %d, want 3 (continues prior numbering)", res.Iterations)
	}
	for _, id := range []string{"goal-rec3:iter-3", "goal-rec3:check-3"} {
		if _, ok := store.states[id]; !ok {
			t.Errorf("missing session %q; stored: %v", id, storeIDs(store.memStore))
		}
	}
}

func TestLoadGoalMissing(t *testing.T) {
	t.Parallel()

	agent := NewAgent(AgentOptions{Provider: &recordingProvider{}, Store: listingMemStore{newMemStore()}})
	if _, ok, err := agent.LoadGoal(context.Background(), "nope"); ok || err != nil {
		t.Fatalf("ok=%v err=%v, want absent", ok, err)
	}
	storeless := NewAgent(AgentOptions{Provider: &recordingProvider{}})
	if _, ok, err := storeless.LoadGoal(context.Background(), "nope"); ok || err != nil {
		t.Fatalf("storeless: ok=%v err=%v, want absent", ok, err)
	}
}

func TestListGoalsOrdersAndFilters(t *testing.T) {
	t.Parallel()

	store := listingMemStore{newMemStore()}
	agent := NewAgent(AgentOptions{Provider: &recordingProvider{}, Store: store})

	base := time.Now().Add(-time.Hour)
	put := func(id string, meta map[string]any, updated time.Time) {
		store.states[id] = SessionState{Version: 1, ID: id, Metadata: meta, CreatedAt: base, UpdatedAt: updated}
	}
	goalMeta := func(objective, status string) map[string]any {
		return map[string]any{
			goalMetaObjective:  objective,
			goalMetaStatus:     status,
			goalMetaChecklist:  `[{"title":"A","done":true}]`,
			goalMetaIterations: float64(2), // as decoded from a JSON round-trip
			goalMetaUsage:      `{"total_tokens":42}`,
			goalMetaSummary:    "s",
		}
	}
	put("goal-old", goalMeta("older goal", string(GoalPaused)), base.Add(1*time.Minute))
	put("goal-new", goalMeta("newer goal", string(GoalAchieved)), base.Add(30*time.Minute))
	put("goal-old:iter-1", nil, base)                // iteration session: skipped by shape
	put("goal-junk", map[string]any{"x": "y"}, base) // no goal metadata: skipped
	put("tui:chat", nil, base)                       // outside prefix

	recs, err := agent.ListGoals(context.Background(), ListGoalsOptions{Prefix: "goal-"})
	if err != nil {
		t.Fatalf("ListGoals: %v", err)
	}
	if len(recs) != 2 || recs[0].ID != "goal-new" || recs[1].ID != "goal-old" {
		t.Fatalf("records = %+v, want goal-new then goal-old", recs)
	}
	if recs[1].Iterations != 2 || recs[1].Status != GoalPaused || !recs[1].Resumable() {
		t.Fatalf("goal-old record = %+v, want decoded paused/resumable with 2 iterations", recs[1])
	}

	limited, err := agent.ListGoals(context.Background(), ListGoalsOptions{Prefix: "goal-", Limit: 1})
	if err != nil {
		t.Fatalf("ListGoals limited: %v", err)
	}
	if len(limited) != 1 || limited[0].ID != "goal-new" {
		t.Fatalf("limited = %+v, want only goal-new", limited)
	}
}

func storeIDs(m *memStore) []string {
	ids := make([]string, 0, len(m.states))
	for id := range m.states {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
