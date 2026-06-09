package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/erain/glue"
	"github.com/erain/glue/cmd/glue/worktree"
)

// scratchRepo initializes a git repository with one commit so worktrees can
// branch off it. The worktree mechanics themselves are tested in the
// cmd/glue/worktree package; here we test the /goal -w flow around them.
func scratchRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
		{"commit", "--allow-empty", "-q", "-m", "init"},
	} {
		if _, err := worktree.Git(dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	return dir
}

func TestSlashGoalWorktreeFlagGating(t *testing.T) {
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

	m.handleSlashGoal("-w")
	if !strings.Contains(lastSystem(), "usage: /goal -w") {
		t.Errorf("bare -w: %q", lastSystem())
	}

	// BuildTools is nil on the test model: isolation must refuse before
	// any goal state is created.
	m.send = func(tea.Msg) {}
	m.handleSlashGoal("-w fix the bug")
	if !strings.Contains(lastSystem(), "needs coding tools") || m.goal != nil {
		t.Fatalf("isolated start without BuildTools: goal=%v msg=%q", m.goal, lastSystem())
	}
}

func TestIsolatedGoalRunsInWorktreeAndPersistsWorkDir(t *testing.T) {
	t.Parallel()
	repo := scratchRepo(t)
	store := newGoalMemStore()
	provider := &scriptedGoalProvider{turns: [][]glue.ProviderEvent{
		scriptedTurn(`{"items":[{"title":"A"}]}`),
		scriptedTurn("did A"),
		scriptedTurn(`{"done":true,"items":[{"title":"A","done":true}],"summary":"done"}`),
	}}
	agent := glue.NewAgent(glue.AgentOptions{Provider: provider, Model: "fake-1", Store: store})

	m := makeTestModel(t)
	m.cfg.Agent = agent
	m.cfg.WorkDir = repo
	var builtAt string
	m.cfg.BuildTools = func(dir string) ([]glue.Tool, error) {
		builtAt = dir
		return []glue.Tool{{ToolSpec: glue.ToolSpec{Name: "stub"}}}, nil
	}
	msgs := make(chan tea.Msg, 64)
	m.send = func(msg tea.Msg) { msgs <- msg }

	m.handleSlashGoal("-w ship A")
	if m.goal == nil || m.goal.workDir == "" || m.goal.branch != worktree.Branch(m.goal.id) {
		t.Fatalf("goal = %+v, want isolated with branch", m.goal)
	}
	if builtAt != m.goal.workDir {
		t.Fatalf("tools built at %q, want worktree %q", builtAt, m.goal.workDir)
	}

	deadline := time.After(10 * time.Second)
	for m.goalRunning() {
		select {
		case msg := <-msgs:
			m.Update(msg)
		case <-deadline:
			t.Fatal("timed out waiting for isolated goal")
		}
	}
	if m.goal.status != glue.GoalAchieved {
		t.Fatalf("status = %q, want achieved", m.goal.status)
	}

	rec, ok, err := agent.LoadGoal(context.Background(), m.goal.id)
	if err != nil || !ok {
		t.Fatalf("LoadGoal: ok=%v err=%v", ok, err)
	}
	if rec.WorkDir != m.goal.workDir {
		t.Fatalf("record workdir = %q, want %q", rec.WorkDir, m.goal.workDir)
	}
}
