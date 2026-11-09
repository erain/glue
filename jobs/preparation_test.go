// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/erain/glue"
	filestore "github.com/erain/glue/stores/file"
)

func TestJobPreparationPersistsIntentBeforeRepositoryWork(t *testing.T) {
	m, err := New(Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	job, err := m.Create(context.Background(), Spec{Objective: "A", Repo: RepoSpec{Path: filepath.Join(t.TempDir(), "missing")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	got := waitJob(t, m, job.ID)
	if got.Status != Errored || got.GoalID != job.ID || got.Workspace == "" {
		t.Fatalf("lost preparation intent: %+v", got)
	}
	agent := glue.NewAgent(glue.AgentOptions{Store: filestore.New(filepath.Join(m.options.StateDir, "sessions"))})
	rec, ok, err := agent.LoadGoal(context.Background(), job.ID)
	if err != nil || !ok || rec.Objective != "A" || rec.WorkDir != got.Workspace {
		t.Fatalf("record %+v found %v err %v", rec, ok, err)
	}
}
func TestJobPreparationStopsWhenInitialCheckpointFails(t *testing.T) {
	m, err := New(Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	job, err := m.Create(context.Background(), Spec{Objective: "A", Repo: RepoSpec{Path: repoFixture(t)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.options.StateDir, "sessions"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), job.ID); err == nil {
		t.Fatal("started without initial checkpoint")
	}
	got, err := m.Get(context.Background(), job.ID)
	if err != nil || got.Status != Created || got.LastSeq != 1 {
		t.Fatalf("job %+v err %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(m.options.StateDir, "worktrees")); !os.IsNotExist(err) {
		t.Fatalf("prepared workspace: %v", err)
	}
}
func TestJobPreparationRecoversCheckoutBeforeMetadata(t *testing.T) {
	for _, clone := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "clone"}[clone], func(t *testing.T) {
			repo := repoFixture(t)
			m, err := New(Options{StateDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { m.Close() })
			source := RepoSpec{Path: repo}
			if clone {
				source = RepoSpec{URL: repo}
			}
			job, err := m.Create(context.Background(), Spec{Objective: "A", Repo: source})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.store.DB.Exec(`CREATE TRIGGER reject_base BEFORE UPDATE ON jobs WHEN NEW.base_sha!='' BEGIN SELECT RAISE(FAIL,'fixture failure'); END`); err != nil {
				t.Fatal(err)
			}
			if err := m.Start(context.Background(), job.ID); err != nil {
				t.Fatal(err)
			}
			interrupted := waitJob(t, m, job.ID)
			if interrupted.Status != Errored || interrupted.Workspace == "" || interrupted.BaseSHA != "" {
				t.Fatal(interrupted)
			}
			git := func(dir string, args ...string) string {
				t.Helper()
				cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %s", err, out)
				}
				return string(out)
			}
			base := git(interrupted.Workspace, "rev-parse", "HEAD")
			git(repo, "commit", "--allow-empty", "-qm", "source advanced")
			if _, err := m.store.DB.Exec("DROP TRIGGER reject_base"); err != nil {
				t.Fatal(err)
			}
			workspace, err := m.prepareWorkspace(context.Background(), interrupted)
			if err != nil {
				t.Fatal(err)
			}
			if workspace.BaseSHA+"\n" != base || workspace.Path != interrupted.Workspace {
				t.Fatalf("workspace %+v original base %s", workspace, base)
			}
			if err := os.WriteFile(filepath.Join(workspace.Path, "kept.txt"), []byte("unsaved work"), 0600); err != nil {
				t.Fatal(err)
			}
			saved, err := m.Get(context.Background(), job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.prepareWorkspace(context.Background(), saved); err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(filepath.Join(workspace.Path, "kept.txt")); err != nil || string(data) != "unsaved work" {
				t.Fatalf("data %q err %v", data, err)
			}
		})
	}
}
func TestJobPreparationPreservesUnknownPartialDirectory(t *testing.T) {
	m, err := New(Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	job, err := m.Create(context.Background(), Spec{Objective: "A", Repo: RepoSpec{Path: repoFixture(t)}})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(m.options.StateDir, "worktrees", job.ID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "unknown.txt")
	if err := os.WriteFile(path, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if got := waitJob(t, m, job.ID); got.Status != Errored {
		t.Fatal(got)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "preserve" {
		t.Fatalf("data %q err %v", data, err)
	}
}

func TestJobPreparationRequiresGoalCheckpointDuringExecution(t *testing.T) {
	p := &scriptProvider{turns: [][]glue.ProviderEvent{turn(`{"items":[{"title":"A"}]}`), turn("work"), turn(`{"done":true,"items":[{"title":"A","done":true}]}`)}}
	state := t.TempDir()
	id := ""
	fixtureErr := make(chan error, 1)
	m, err := New(Options{StateDir: state, ProviderFactory: func(string) (glue.Provider, error) {
		path := filepath.Join(state, "sessions", url.PathEscape(id)+".json")
		err := os.Remove(path)
		if err == nil {
			err = os.Mkdir(path, 0700)
		}
		fixtureErr <- err
		return p, err
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	job, err := m.Create(context.Background(), Spec{Objective: "A", Repo: RepoSpec{Path: repoFixture(t)}})
	if err != nil {
		t.Fatal(err)
	}
	id = job.ID
	if err := m.Start(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if got := waitJob(t, m, id); got.Status != Errored {
		t.Fatal(got)
	}
	if err := <-fixtureErr; err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.turns) != 3 {
		t.Fatal("model executed without a required checkpoint")
	}
}

func TestJobPreparationNormalizesRepositoryPath(t *testing.T) {
	m, err := New(Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	job, err := m.Create(context.Background(), Spec{Objective: "A", Repo: RepoSpec{Path: "relative-repository"}})
	if err != nil {
		t.Fatal(err)
	}
	expected, err := filepath.Abs("relative-repository")
	if err != nil {
		t.Fatal(err)
	}
	if job.Spec.Repo.Path != expected {
		t.Fatalf("path %q want %q", job.Spec.Repo.Path, expected)
	}
}
