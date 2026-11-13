// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erain/glue"
	filestore "github.com/erain/glue/stores/file"
)

func TestContinueBlockedJob(t *testing.T) {
	p := &scriptProvider{turns: [][]glue.ProviderEvent{turn(`{"items":[{"title":"A"},{"title":"B","done":false}]}`)}}
	for i := 0; i < 3; i++ {
		p.turns = append(p.turns, turn("attempt"), turn(`{"items":[{"title":"A","done":true},{"title":"B"}]}`))
	}
	p.turns = append(p.turns, turn("follow guidance"), turn(`{"done":true,"items":[{"title":"A","done":true},{"title":"B","done":true}]}`))
	m, err := New(Options{StateDir: t.TempDir(), ProviderFactory: func(string) (glue.Provider, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	job, err := m.Create(context.Background(), Spec{Objective: "finish A and B", Repo: RepoSpec{Path: repoFixture(t)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	before := waitJob(t, m, job.ID)
	if before.Status != Blocked {
		t.Fatal(before)
	}
	if err := m.Continue(context.Background(), job.ID, "Finish B using examples.", 1); err != nil {
		t.Fatal(err)
	}
	after := waitJob(t, m, job.ID)
	if after.Status != Achieved || after.ID != before.ID || after.Workspace != before.Workspace || after.BaseSHA != before.BaseSHA {
		t.Fatalf("before %+v after %+v", before, after)
	}
	cmd := exec.Command("git", "-C", after.Workspace, "branch", "--show-current")
	branch, err := cmd.Output()
	if err != nil || string(branch) != "glue/"+job.ID+"\n" {
		t.Fatalf("branch %q err %v", branch, err)
	}
	agent := glue.NewAgent(glue.AgentOptions{Store: filestore.New(filepath.Join(m.options.StateDir, "sessions"))})
	rec, ok, err := agent.LoadGoal(context.Background(), job.ID)
	if err != nil || !ok || rec.Iterations != 4 || len(rec.Checklist) != 2 || !rec.Checklist[0].Done || !rec.Checklist[1].Done {
		t.Fatalf("record %+v found %v err %v", rec, ok, err)
	}
	events, err := m.Events(context.Background(), job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	resumed, delivered := 0, 0
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Fatal("sequence gap")
		}
		if e.Type == "job_resumed" {
			resumed++
		}
		if e.Type == "operator_message_delivered" {
			delivered++
			if e.Iteration == nil || *e.Iteration != 4 {
				t.Fatal(e)
			}
		}
	}
	if resumed != 1 || delivered != 1 {
		t.Fatalf("resumed %d delivered %d", resumed, delivered)
	}
	if err := m.Continue(context.Background(), job.ID, "", 1); !errors.Is(err, ErrConflict) {
		t.Fatal("an achieved job continued without new instructions:", err)
	}
}

func TestContinueAchievedJobWithFeedback(t *testing.T) {
	m, job, _ := achievedJob(t)
	p := &requestRecorder{scriptProvider: scriptProvider{turns: [][]glue.ProviderEvent{
		turn("renamed it"),
		turn(`{"done":true,"items":[{"title":"write file","done":true},{"title":"follow-up","done":true,"evidence":"renamed"}]}`),
	}}}
	m.options.ProviderFactory = func(string) (glue.Provider, error) { return p, nil }
	if err := m.Continue(context.Background(), job.ID, "Address this feedback on the pull request:\nrename Get to Acquire", 1); err != nil {
		t.Fatal(err)
	}
	if got := waitJob(t, m, job.ID); got.Status != Achieved {
		t.Fatal(got)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.texts) != 2 {
		t.Fatalf("requests = %d", len(p.texts))
	}
	for i, text := range p.texts { // maker, then checker
		if !strings.Contains(text, "[ ] Follow-up: Address this feedback on the pull request:") || !strings.Contains(text, "rename Get to Acquire") {
			t.Fatalf("request %d lacks the follow-up item or guidance:\n%s", i, text)
		}
	}
}

func TestContinueAllowedStatesAndValidation(t *testing.T) {
	for _, status := range []Status{Created, Running, WaitingForInput, Achieved, Blocked, MaxIterations, BudgetLimited, Interrupted, Errored, Canceled} {
		t.Run(string(status), func(t *testing.T) {
			m, job, _ := achievedJob(t)
			if err := m.record(job.ID, "fixture_state", nil, "", nil, status); err != nil {
				t.Fatal(err)
			}
			p := &scriptProvider{turns: [][]glue.ProviderEvent{turn("finish"), turn(`{"done":true,"items":[{"title":"write file","done":true}]}`)}}
			m.options.ProviderFactory = func(string) (glue.Provider, error) { return p, nil }
			allowed := status == Blocked || status == MaxIterations || status == BudgetLimited || status == Interrupted
			err := m.Continue(context.Background(), job.ID, "", 1)
			if allowed {
				if err != nil {
					t.Fatal(err)
				}
				if got := waitJob(t, m, job.ID); got.Status != Achieved {
					t.Fatal(got)
				}
			} else if !errors.Is(err, ErrConflict) {
				t.Fatalf("state %s err %v", status, err)
			}
		})
	}
	m, job, _ := achievedJob(t)
	if err := m.Continue(context.Background(), job.ID, "", -1); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Continue(ctx, job.ID, "", 1); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestContinueRollsBackWhenGuidanceCannotPersist(t *testing.T) {
	m, job, _ := achievedJob(t)
	if err := m.record(job.ID, "fixture_state", nil, "", nil, Blocked); err != nil {
		t.Fatal(err)
	}
	before, err := m.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.store.DB.Exec(`CREATE TRIGGER reject_guidance BEFORE INSERT ON events WHEN json_extract(NEW.payload_json,'$.type')='operator_message' BEGIN SELECT RAISE(FAIL,'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := m.Continue(context.Background(), job.ID, "do more", 1); err == nil {
		t.Fatal("ignored guidance failure")
	}
	after, err := m.Get(context.Background(), job.ID)
	if err != nil || after.Status != before.Status || after.LastSeq != before.LastSeq || after.Spec.Budgets != before.Spec.Budgets {
		t.Fatalf("before %+v after %+v err %v", before, after, err)
	}
}

func TestContinueRequiresSavedGoal(t *testing.T) {
	m, err := New(Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	job, err := m.Create(context.Background(), Spec{Objective: "A", Repo: RepoSpec{Path: "unused"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.record(job.ID, "fixture_state", nil, "", nil, Interrupted); err != nil {
		t.Fatal(err)
	}
	if err := m.Continue(context.Background(), job.ID, "", 0); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
}
