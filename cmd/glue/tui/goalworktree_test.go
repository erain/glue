package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/erain/glue"
)

// scratchRepo initializes a git repository with one commit so worktrees can
// branch off it.
func scratchRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
		{"commit", "--allow-empty", "-q", "-m", "init"},
	} {
		if _, err := gitOutput(dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	return dir
}

func TestEnsureGoalWorktreeCreatesAndReuses(t *testing.T) {
	t.Parallel()
	repo := scratchRepo(t)

	dir, err := ensureGoalWorktree(repo, "goal-abc123")
	if err != nil {
		t.Fatalf("ensureGoalWorktree: %v", err)
	}
	want := filepath.Join(repo, ".glue", "worktrees", "goal-abc123")
	if dir != want {
		t.Fatalf("dir = %q, want %q", dir, want)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("worktree dir missing: %v", err)
	}
	branch, err := gitOutput(dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || branch != "goal/abc123" {
		t.Fatalf("branch = %q err=%v, want goal/abc123", branch, err)
	}

	// Second call (resume) reuses the existing worktree.
	again, err := ensureGoalWorktree(repo, "goal-abc123")
	if err != nil || again != dir {
		t.Fatalf("reuse: dir=%q err=%v", again, err)
	}

	// Deleted worktree but surviving branch: re-attach instead of failing
	// on the duplicate branch name.
	if _, err := gitOutput(repo, "worktree", "remove", "--force", dir); err != nil {
		t.Fatalf("worktree remove: %v", err)
	}
	reattached, err := ensureGoalWorktree(repo, "goal-abc123")
	if err != nil || reattached != dir {
		t.Fatalf("re-attach: dir=%q err=%v", reattached, err)
	}
}

func TestEnsureGoalWorktreeOutsideRepo(t *testing.T) {
	t.Parallel()
	if _, err := ensureGoalWorktree(t.TempDir(), "goal-x"); err == nil || !strings.Contains(err.Error(), "git repository") {
		t.Fatalf("err = %v, want needs-a-git-repository", err)
	}
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
	if m.goal == nil || m.goal.workDir == "" || m.goal.branch != goalBranch(m.goal.id) {
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
