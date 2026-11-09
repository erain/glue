// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erain/glue"
	filestore "github.com/erain/glue/stores/file"
)

type crashProvider struct {
	scriptProvider
	calls int
	ready func()
}

func (p *crashProvider) Stream(ctx context.Context, req glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	p.calls++
	if p.calls == 4 {
		p.ready()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return p.scriptProvider.Stream(ctx, req)
}
func TestJobResumesAfterHardStop(t *testing.T) {
	if state := os.Getenv("GLUE_TEST_RESTART_STATE"); state != "" {
		id := ""
		store := filestore.New(filepath.Join(state, "sessions"))
		p := &crashProvider{scriptProvider: scriptProvider{turns: [][]glue.ProviderEvent{turn(`{"items":[{"title":"A"},{"title":"B"}]}`), turn("A done"), turn(`{"items":[{"title":"A","done":true},{"title":"B"}]}`)}}}
		p.ready = func() {
			err := store.Save(context.Background(), id+":iter-2", glue.SessionState{ID: id + ":iter-2", Messages: []glue.Message{{Role: glue.MessageRoleUser, Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: "stale abandoned turn"}}}}})
			if err != nil {
				panic(err)
			}
			fmt.Println(id)
		}
		m, err := New(Options{StateDir: state, ProviderFactory: func(string) (glue.Provider, error) { return p, nil }})
		if err != nil {
			t.Fatal(err)
		}
		defer m.Close()
		job, err := m.Create(context.Background(), Spec{Objective: "finish A and B", Repo: RepoSpec{Path: os.Getenv("GLUE_TEST_RESTART_REPO")}})
		if err != nil {
			t.Fatal(err)
		}
		id = job.ID
		if err := m.Start(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	state := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestJobResumesAfterHardStop$")
	cmd.Env = append(os.Environ(), "GLUE_TEST_RESTART_STATE="+state, "GLUE_TEST_RESTART_REPO="+repoFixture(t))
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	reader := bufio.NewScanner(stdout)
	if !reader.Scan() {
		t.Fatalf("child not ready: %v", reader.Err())
	}
	id := reader.Text()
	if !strings.HasPrefix(id, "job-") {
		t.Fatalf("child output %q", id)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("expected killed child")
	}
	p := &restartProvider{scriptProvider: scriptProvider{turns: [][]glue.ProviderEvent{turn("B done"), turn(`{"done":true,"items":[{"title":"A","done":true},{"title":"B","done":true}]}`)}}}
	m, err := New(Options{StateDir: state, ProviderFactory: func(string) (glue.Provider, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	got := waitJob(t, m, id)
	if got.Status != Achieved {
		t.Fatal(got)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.requests) != 2 {
		t.Fatalf("requests %d", len(p.requests))
	}
	data, _ := json.Marshal(p.requests[0])
	if strings.Contains(string(data), "stale abandoned turn") {
		t.Fatal("reused abandoned maker session")
	}
	events, err := m.Events(context.Background(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	resumed := false
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Fatal("event sequence gap")
		}
		if e.Type == "job_resumed" {
			resumed = true
		}
	}
	if !resumed {
		t.Fatal("missing job_resumed")
	}
	detail, err := m.Inspect(context.Background(), id)
	if err != nil || detail.Iteration != 2 {
		t.Fatalf("detail %+v err %v", detail, err)
	}
}

type restartProvider struct {
	scriptProvider
	requests []glue.ProviderRequest
}

func (p *restartProvider) Stream(ctx context.Context, req glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	return p.scriptProvider.Stream(ctx, req)
}
func TestJobResumesBeforeRepositoryPreparation(t *testing.T) {
	state := t.TempDir()
	m, err := New(Options{StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	job, err := m.Create(context.Background(), Spec{Objective: "A", Repo: RepoSpec{Path: repoFixture(t)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.startIntent(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	p := &scriptProvider{turns: [][]glue.ProviderEvent{turn(`{"items":[{"title":"A"}]}`), turn("A done"), turn(`{"done":true,"items":[{"title":"A","done":true}]}`)}}
	m, err = New(Options{StateDir: state, ProviderFactory: func(string) (glue.Provider, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	if got := waitJob(t, m, job.ID); got.Status != Achieved {
		t.Fatal(got)
	}
}

func restartFixture(t *testing.T, budgets Budgets, rec glue.GoalRecord) (*Manager, Job) {
	t.Helper()
	m, err := New(Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	job, err := m.Create(context.Background(), Spec{Objective: "A", Repo: RepoSpec{Path: repoFixture(t)}, Budgets: budgets, Policy: "ask-all"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.startIntent(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	job, err = m.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	rec.ID = job.ID
	rec.Objective = "A"
	rec.WorkDir = job.Workspace
	if err := glue.NewAgent(glue.AgentOptions{Store: filestore.New(filepath.Join(m.options.StateDir, "sessions"))}).SaveGoal(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	return m, job
}
func TestRestartExpiresDeadPermissions(t *testing.T) {
	m, job := restartFixture(t, Budgets{}, glue.GoalRecord{Status: glue.GoalRunning, Checklist: []glue.ChecklistItem{{Title: "A"}}})
	if err := m.record(job.ID, "permission_request", map[string]any{"permission_id": "old", "request": glue.PermissionRequest{Tool: "write_file"}}, "", nil, WaitingForInput); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	p := &scriptProvider{turns: [][]glue.ProviderEvent{{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "write", Name: "write_file", Arguments: []byte(`{"path":"A.txt","content":"done"}`)}}, {Type: glue.ProviderEventDone}}, turn("written"), turn(`{"done":true,"items":[{"title":"A","done":true}]}`)}}
	next, err := New(Options{StateDir: m.options.StateDir, ProviderFactory: func(string) (glue.Provider, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { next.Close() })
	deadline := time.Now().Add(10 * time.Second)
	for {
		pending, err := next.Permissions(context.Background(), job.ID, "pending")
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) > 0 {
			if len(pending) != 1 || pending[0].ID == "old" {
				t.Fatal(pending)
			}
			expired, err := next.Permissions(context.Background(), job.ID, "expired")
			if err != nil || len(expired) != 1 || expired[0].ID != "old" {
				t.Fatalf("expired %v err %v", expired, err)
			}
			if err := next.Decide(context.Background(), job.ID, "old", glue.PermissionDecision{Allow: true}); !errors.Is(err, ErrConflict) {
				t.Fatal(err)
			}
			if err := next.Decide(context.Background(), job.ID, pending[0].ID, glue.PermissionDecision{Allow: true}); err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("new permission not requested")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := waitJob(t, next, job.ID); got.Status != Achieved {
		t.Fatal(got)
	}
}
func TestRestartPreservesBudgetLimitsAndTerminalCheckpoint(t *testing.T) {
	for _, tc := range []struct {
		name    string
		budgets Budgets
		rec     glue.GoalRecord
		want    Status
		fresh   bool
	}{
		{"tokens", Budgets{TokenBudget: 5}, glue.GoalRecord{Usage: glue.Usage{TotalTokens: 5}}, BudgetLimited, false},
		{"iterations", Budgets{MaxIterations: 1}, glue.GoalRecord{Iterations: 1}, MaxIterations, false},
		{"wall clock", Budgets{WallClock: time.Nanosecond}, glue.GoalRecord{}, BudgetLimited, false},
		{"fresh continuation", Budgets{MaxIterations: 1, TokenBudget: 5}, glue.GoalRecord{Iterations: 1, Status: glue.GoalBlocked, Usage: glue.Usage{TotalTokens: 5}}, Achieved, true},
		{"finished", Budgets{}, glue.GoalRecord{Status: glue.GoalAchieved}, Achieved, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.rec.Status == "" {
				tc.rec.Status = glue.GoalRunning
			}
			tc.rec.Checklist = []glue.ChecklistItem{{Title: "A"}}
			m, job := restartFixture(t, tc.budgets, tc.rec)
			var recordErr error
			if tc.fresh {
				recordErr = m.record(job.ID, "job_resumed", map[string]any{"from_iteration": 2, "reason": "continue"}, "", nil, Running)
			} else {
				recordErr = m.record(job.ID, "plan", glue.GoalEvent{Type: glue.GoalEventPlan, Usage: tc.rec.Usage}, "", nil, "")
			}
			if recordErr != nil {
				t.Fatal(recordErr)
			}
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			next, err := New(Options{StateDir: m.options.StateDir, ProviderFactory: func(string) (glue.Provider, error) {
				calls.Add(1)
				return &scriptProvider{turns: [][]glue.ProviderEvent{turn("work"), turn(`{"done":true,"items":[{"title":"A","done":true}]}`)}}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { next.Close() })
			expectedCalls := int32(0)
			if tc.fresh {
				expectedCalls = 1
			}
			if got := waitJob(t, next, job.ID); got.Status != tc.want || calls.Load() != expectedCalls {
				t.Fatalf("job %+v calls %d", got, calls.Load())
			}
		})
	}
}
