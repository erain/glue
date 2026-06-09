package tui

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/erain/glue"
)

func testChecklist() []glue.ChecklistItem {
	return []glue.ChecklistItem{{Title: "A", Done: true, Evidence: "A.go"}, {Title: "B"}}
}

func TestGoalEventLifecycleUpdatesCardInPlace(t *testing.T) {
	t.Parallel()
	m := makeTestModel(t)
	m.goal = &goalState{objective: "ship it", running: true, maxIterations: 10, cardIdx: -1}

	m.handleGoalEvent(glue.GoalEvent{Type: glue.GoalEventPlan, Checklist: testChecklist()})
	if m.goal.cardIdx != len(m.transcript)-1 {
		t.Fatalf("cardIdx = %d, want last transcript index %d", m.goal.cardIdx, len(m.transcript)-1)
	}
	cardCount := len(m.transcript)

	m.handleGoalEvent(glue.GoalEvent{Type: glue.GoalEventIterationStart, Iteration: 2, Usage: glue.Usage{TotalTokens: 12345}})
	m.handleGoalEvent(glue.GoalEvent{
		Type:      glue.GoalEventVerdict,
		Iteration: 2,
		Message:   "B remains",
		Checklist: testChecklist(),
		Usage:     glue.Usage{TotalTokens: 23456},
	})
	if len(m.transcript) != cardCount {
		t.Fatalf("transcript grew to %d items, want card updated in place", len(m.transcript))
	}

	plain := stripANSI(m.transcript[m.goal.cardIdx].render(renderCtx{width: 100}))
	for _, want := range []string{"ship it", "[x] A", "— A.go", "[ ] B", "iter 2/10", "1/2", "23.5k tok", "verdict: B remains"} {
		if !strings.Contains(plain, want) {
			t.Errorf("card missing %q\n%s", want, plain)
		}
	}
	seg := stripANSI(m.goal.statusSegment())
	if !strings.Contains(seg, "iter 2/10") || !strings.Contains(seg, "1/2") {
		t.Errorf("status segment = %q, want iter and fraction", seg)
	}
}

func TestGoalCardReattachesAfterTranscriptReset(t *testing.T) {
	t.Parallel()
	m := makeTestModel(t)
	m.goal = &goalState{objective: "x", running: true, maxIterations: 10, cardIdx: -1}
	m.handleGoalEvent(glue.GoalEvent{Type: glue.GoalEventPlan, Checklist: testChecklist()})

	// Simulate /clear: transcript nuked, card index detached.
	m.transcript = nil
	m.detachGoalCard()

	m.handleGoalEvent(glue.GoalEvent{Type: glue.GoalEventIterationStart, Iteration: 1})
	if len(m.transcript) != 1 || m.transcript[0].Kind != itemBlock || m.transcript[0].BlockTitle != "Goal" {
		t.Fatalf("transcript = %#v, want single re-appended goal card", m.transcript)
	}
}

func TestGoalDoneMapsCancelToPaused(t *testing.T) {
	t.Parallel()
	m := makeTestModel(t)
	m.goal = &goalState{objective: "x", running: true, paused: true, maxIterations: 10, cardIdx: -1, checklist: testChecklist()}

	m.handleGoalDone(goalDoneMsg{
		Res: glue.GoalResult{Status: glue.GoalErrored, Checklist: testChecklist(), Iterations: 1},
		Err: context.Canceled,
	})
	if m.goal.running {
		t.Fatal("goal still running after done")
	}
	if m.goal.status != "" {
		t.Fatalf("status = %q, want empty (paused, not terminal)", m.goal.status)
	}
	seg := stripANSI(m.goal.statusSegment())
	if !strings.Contains(seg, "paused") {
		t.Errorf("status segment = %q, want paused", seg)
	}
	// No "goal error:" line — pause is not an error.
	for _, it := range m.transcript {
		if it.Kind == itemSystem && strings.Contains(it.Text, "goal error") {
			t.Errorf("unexpected error line: %q", it.Text)
		}
	}
}

func TestSlashGoalSubcommandGating(t *testing.T) {
	t.Parallel()
	m := makeTestModel(t)

	lastSystem := func() string {
		for i := len(m.transcript) - 1; i >= 0; i-- {
			if m.transcript[i].Kind == itemSystem {
				return m.transcript[i].Text
			}
		}
		return ""
	}

	m.handleSlashGoal("")
	if !strings.Contains(lastSystem(), "usage: /goal") {
		t.Errorf("bare /goal with no goal: %q", lastSystem())
	}
	m.handleSlashGoal("pause")
	if !strings.Contains(lastSystem(), "no goal running") {
		t.Errorf("/goal pause with no goal: %q", lastSystem())
	}
	// With no in-memory goal, resume consults the store — no agent wired
	// here, so the lookup degrades with a message instead of panicking.
	m.handleSlashGoal("resume")
	if !strings.Contains(lastSystem(), "no agent wired") {
		t.Errorf("/goal resume with no goal/agent: %q", lastSystem())
	}

	m.goal = &goalState{objective: "x", running: true, maxIterations: 10, cardIdx: -1}
	m.handleSlashGoal("resume")
	if !strings.Contains(lastSystem(), "already running") {
		t.Errorf("/goal resume while running: %q", lastSystem())
	}

	m.goal = &goalState{objective: "x", running: false, maxIterations: 10, cardIdx: -1}
	m.handleSlashGoal("resume")
	if !strings.Contains(lastSystem(), "no checklist captured") {
		t.Errorf("/goal resume without checklist: %q", lastSystem())
	}

	m.handleSlashGoal("clear")
	if m.goal != nil || !strings.Contains(lastSystem(), "goal cleared") {
		t.Errorf("/goal clear: goal=%v msg=%q", m.goal, lastSystem())
	}
}

func TestPermissionQueuePromotesNextRequest(t *testing.T) {
	t.Parallel()
	m := makeTestModel(t)
	m.width, m.height = 100, 40
	m.ready = true

	first := make(chan glue.PermissionDecision, 1)
	second := make(chan glue.PermissionDecision, 1)
	m.Update(permRequestMsg{Req: glue.PermissionRequest{Tool: "write_file", Target: "a.go"}, Respond: first})
	m.Update(permRequestMsg{Req: glue.PermissionRequest{Tool: "run_command", Target: "go test"}, Respond: second})

	if m.pending == nil || m.pending.req.Tool != "write_file" {
		t.Fatalf("pending = %+v, want write_file first", m.pending)
	}
	if len(m.permQueue) != 1 {
		t.Fatalf("queue len = %d, want 1", len(m.permQueue))
	}

	m.handlePermissionKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if d := <-first; !d.Allow {
		t.Fatal("first request not allowed")
	}
	if m.pending == nil || m.pending.req.Tool != "run_command" {
		t.Fatalf("pending after promote = %+v, want run_command", m.pending)
	}
	if len(m.permQueue) != 0 {
		t.Fatalf("queue len = %d, want 0 after promote", len(m.permQueue))
	}

	m.handlePermissionKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if d := <-second; d.Allow {
		t.Fatal("second request unexpectedly allowed")
	}
	if m.pending != nil {
		t.Fatalf("pending = %+v, want nil after queue drained", m.pending)
	}
}

func TestFormatTokens(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   int64
		want string
	}{{0, "0"}, {987, "987"}, {1500, "1.5k"}, {23456, "23.5k"}, {1_200_000, "1.2M"}}
	for _, c := range cases {
		if got := formatTokens(c.in); got != c.want {
			t.Errorf("formatTokens(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// scriptedGoalProvider replays one fixed event stream per Stream call, in
// order — mirrors the glue package's recordingProvider for goal loops.
type scriptedGoalProvider struct {
	mu    sync.Mutex
	calls int
	turns [][]glue.ProviderEvent
}

func (p *scriptedGoalProvider) Stream(_ context.Context, _ glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	p.mu.Lock()
	turn := p.turns[p.calls%len(p.turns)]
	p.calls++
	p.mu.Unlock()
	ch := make(chan glue.ProviderEvent, len(turn))
	for _, e := range turn {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func scriptedTurn(text string) []glue.ProviderEvent {
	msg := glue.Message{
		Role:    glue.MessageRoleAssistant,
		Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: text}},
	}
	return []glue.ProviderEvent{
		{Type: glue.ProviderEventStart},
		{Type: glue.ProviderEventTextDelta, Delta: text},
		{Type: glue.ProviderEventDone, Message: &msg},
	}
}

// goalMemStore is a minimal in-memory glue.Store + SessionLister for
// resume-across-restart tests.
type goalMemStore struct {
	mu     sync.Mutex
	states map[string]glue.SessionState
}

func newGoalMemStore() *goalMemStore {
	return &goalMemStore{states: map[string]glue.SessionState{}}
}

func (s *goalMemStore) Load(_ context.Context, id string) (glue.SessionState, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.states[id]
	return st, ok, nil
}

func (s *goalMemStore) Save(_ context.Context, id string, state glue.SessionState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[id] = state
	return nil
}

func (s *goalMemStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.states, id)
	return nil
}

func (s *goalMemStore) ListSessions(_ context.Context, opts glue.ListSessionsOptions) ([]glue.SessionSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []glue.SessionSummary
	for id, st := range s.states {
		if opts.Prefix != "" && !strings.HasPrefix(id, opts.Prefix) {
			continue
		}
		out = append(out, glue.SessionSummary{ID: id, CreatedAt: st.CreatedAt, UpdatedAt: st.UpdatedAt})
	}
	return out, nil
}

// pausedGoalRecordState crafts the durable glue/goal:* record a paused goal
// leaves behind — written literally because the key strings are a stable
// on-store format.
func pausedGoalRecordState(id string) glue.SessionState {
	return glue.SessionState{
		Version: 1,
		ID:      id,
		Metadata: map[string]any{
			"glue/goal:objective":  "finish B",
			"glue/goal:status":     string(glue.GoalPaused),
			"glue/goal:checklist":  `[{"title":"A","done":true,"evidence":"A.go"},{"title":"B"}]`,
			"glue/goal:iterations": 2,
		},
		CreatedAt: time.Now().Add(-time.Hour),
		UpdatedAt: time.Now().Add(-time.Minute),
	}
}

func TestGoalResumeAcrossRestart(t *testing.T) {
	t.Parallel()
	store := newGoalMemStore()
	store.states["goal-prev"] = pausedGoalRecordState("goal-prev")

	// The resumed run must skip planning: only a maker turn and a checker
	// verdict are scripted, and a planning call would consume the maker
	// turn and fail the assertions below.
	provider := &scriptedGoalProvider{turns: [][]glue.ProviderEvent{
		scriptedTurn("finished B"),
		scriptedTurn(`{"done":true,"items":[{"title":"A","done":true},{"title":"B","done":true}],"summary":"done"}`),
	}}
	agent := glue.NewAgent(glue.AgentOptions{Provider: provider, Model: "fake-1", Store: store})

	m := makeTestModel(t)
	m.cfg.Agent = agent
	msgs := make(chan tea.Msg, 64)
	m.send = func(msg tea.Msg) { msgs <- msg }

	m.handleSlashGoal("resume")
	if m.goal == nil || m.goal.id != "goal-prev" {
		t.Fatalf("goal = %+v, want resumed under goal-prev", m.goal)
	}

	deadline := time.After(10 * time.Second)
	for m.goalRunning() {
		select {
		case msg := <-msgs:
			m.Update(msg)
		case <-deadline:
			t.Fatal("timed out waiting for resumed goal to finish")
		}
	}
	if m.goal.status != glue.GoalAchieved {
		t.Fatalf("status = %q, want achieved", m.goal.status)
	}
	if m.goal.iteration != 3 {
		t.Fatalf("iteration = %d, want 3 (continues prior numbering)", m.goal.iteration)
	}
	if provider.calls != 2 {
		t.Fatalf("provider calls = %d, want 2 (no planning call on resume)", provider.calls)
	}
	if _, ok := store.states["goal-prev:iter-3"]; !ok {
		t.Error("missing goal-prev:iter-3 session — resumed run reused old iteration ids?")
	}
}

func TestGoalListRendersStoredRecords(t *testing.T) {
	t.Parallel()
	store := newGoalMemStore()
	store.states["goal-prev"] = pausedGoalRecordState("goal-prev")
	agent := glue.NewAgent(glue.AgentOptions{Provider: &scriptedGoalProvider{}, Store: store})

	m := makeTestModel(t)
	m.cfg.Agent = agent
	m.listStoredGoals()

	last := m.transcript[len(m.transcript)-1]
	if last.Kind != itemBlock || last.BlockTitle != "Goals" {
		t.Fatalf("last item = %+v, want Goals block", last)
	}
	plain := stripANSI(last.render(renderCtx{width: 120}))
	for _, want := range []string{"paused", "1/2", "finish B", "ago"} {
		if !strings.Contains(plain, want) {
			t.Errorf("goal list missing %q\n%s", want, plain)
		}
	}
}

func TestStartGoalRunsLoopToAchieved(t *testing.T) {
	t.Parallel()
	provider := &scriptedGoalProvider{turns: [][]glue.ProviderEvent{
		scriptedTurn(`{"items":[{"title":"A"}]}`),
		scriptedTurn("worked on A"),
		scriptedTurn(`{"done":true,"items":[{"title":"A","done":true,"evidence":"verified"}],"summary":"all done"}`),
	}}
	agent := glue.NewAgent(glue.AgentOptions{Provider: provider, Model: "fake-1"})

	m := makeTestModel(t)
	m.cfg.Agent = agent
	msgs := make(chan tea.Msg, 64)
	m.send = func(msg tea.Msg) { msgs <- msg }

	m.startGoal(goalStart{objective: "ship A"})
	if !m.goalRunning() {
		t.Fatal("goal not running after startGoal")
	}

	deadline := time.After(10 * time.Second)
	for m.goalRunning() {
		select {
		case msg := <-msgs:
			m.Update(msg)
		case <-deadline:
			t.Fatal("timed out waiting for goalDoneMsg")
		}
	}
	if m.goal.status != glue.GoalAchieved {
		t.Fatalf("status = %q, want achieved", m.goal.status)
	}
	seg := stripANSI(m.goal.statusSegment())
	if !strings.Contains(seg, "achieved") {
		t.Errorf("status segment = %q, want achieved", seg)
	}
	plain := stripANSI(m.transcript[m.goal.cardIdx].render(renderCtx{width: 100}))
	for _, want := range []string{"[x] A", "— verified", "achieved", "1/1"} {
		if !strings.Contains(plain, want) {
			t.Errorf("final card missing %q\n%s", want, plain)
		}
	}
}
