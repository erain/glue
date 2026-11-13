package jobs

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erain/glue"
)

func TestCreateHasNoSideEffects(t *testing.T) {
	dir := t.TempDir()
	var calls atomic.Int32
	m, err := New(Options{StateDir: dir, ProviderFactory: func(string) (glue.Provider, error) { calls.Add(1); return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	job, err := m.Create(context.Background(), Spec{Objective: "fix tests", Repo: RepoSpec{URL: "https://example.invalid/repo.git"}})
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^job-[a-f0-9]{12}$`).MatchString(job.ID) || job.Status != Created || job.LastSeq != 1 {
		t.Fatalf("job %+v", job)
	}
	if calls.Load() != 0 || job.Workspace != "" || job.BaseSHA != "" {
		t.Fatal("execution started during Create")
	}
	for _, name := range []string{"worktrees", "repos"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("created workspace %s: %v", name, err)
		}
	}
	events, err := m.Events(context.Background(), job.ID, 0)
	if err != nil || len(events) != 1 || events[0].Type != "job_created" {
		t.Fatalf("events %v err %v", events, err)
	}
	if job.Spec.Provider != "gemini" || job.Spec.Model != "gemini-3-flash-preview" || job.Spec.Thinking != "low" || job.Spec.Delivery != "none" {
		t.Fatal(job.Spec)
	}
	if explicit, err := m.Create(context.Background(), Spec{ID: "job-0123456789ab", Objective: "explicit id", Repo: RepoSpec{URL: "https://example.invalid/repo.git"}}); err != nil || explicit.ID != "job-0123456789ab" || explicit.Spec.ID != "" {
		t.Fatalf("explicit job=%+v err=%v", explicit, err)
	}
	for _, badID := range []string{"job-0123456789ab", "bad-id"} {
		if _, err := m.Create(context.Background(), Spec{ID: badID, Objective: "bad", Repo: RepoSpec{URL: "https://example.invalid/repo.git"}}); err == nil {
			t.Fatalf("expected error for id %q", badID)
		}
	}
}
func TestCreatedJobsSurviveManagerRestart(t *testing.T) {
	dir := t.TempDir()
	m, err := New(Options{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	first, err := m.Create(context.Background(), Spec{Objective: "first", Repo: RepoSpec{Path: dir}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Create(context.Background(), Spec{Objective: "second", Repo: RepoSpec{Path: dir}, Provider: "openrouter", Model: "custom"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	other, err := New(Options{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	got, err := other.Get(context.Background(), first.ID)
	if err != nil || got.Spec.Objective != "first" {
		t.Fatalf("job %+v err %v", got, err)
	}
	list, err := other.List(context.Background())
	if err != nil || len(list) != 2 || list[0].ID != second.ID {
		t.Fatalf("list %v err %v", list, err)
	}
	events, err := other.Events(context.Background(), first.ID, 0)
	if err != nil || len(events) != 1 {
		t.Fatalf("events %v err %v", events, err)
	}
	resumed, err := other.Events(context.Background(), first.ID, 1)
	if err != nil || len(resumed) != 0 {
		t.Fatalf("resumed %v err %v", resumed, err)
	}
}
func TestInvalidSpecs(t *testing.T) {
	m, err := New(Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	valid := Spec{Objective: "task", Repo: RepoSpec{Path: "/tmp/repo"}}
	for _, name := range []string{"objective", "source", "both sources", "budget", "wall clock", "delivery", "verify", "provider", "thinking"} {
		t.Run(name, func(t *testing.T) {
			s := valid
			switch name {
			case "objective":
				s.Objective = " "
			case "source":
				s.Repo = RepoSpec{}
			case "both sources":
				s.Repo.URL = "https://example.invalid/repo"
			case "budget":
				s.Budgets.MaxIterations = -1
			case "wall clock":
				s.Budgets.WallClock = -time.Second
			case "delivery":
				s.Delivery = "merge"
			case "verify":
				s.Verify = [][]string{{}}
			case "provider":
				s.Provider = "unknown"
			case "thinking":
				s.Thinking = "impossible"
			}
			if _, err := m.Create(context.Background(), s); err == nil {
				t.Fatalf("accepted %+v", s)
			}
		})
	}
	list, err := m.List(context.Background())
	if err != nil || len(list) != 0 {
		t.Fatalf("invalid jobs persisted: %v %v", list, err)
	}
}

func TestCreateRollsBackWithoutEvent(t *testing.T) {
	m, err := New(Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	if _, err := m.store.DB.Exec("DROP TABLE events"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(context.Background(), Spec{Objective: "task", Repo: RepoSpec{Path: "/tmp/repo"}}); err == nil {
		t.Fatal("accepted missing event storage")
	}
	list, err := m.List(context.Background())
	if err != nil || len(list) != 0 {
		t.Fatalf("partial jobs %v err %v", list, err)
	}
}
