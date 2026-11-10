package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erain/glue"
)

type waitingProvider struct {
	scriptProvider
	calls              int
	continued, release chan struct{}
}

func (p *waitingProvider) Stream(ctx context.Context, req glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	p.calls++
	if p.calls == 3 {
		close(p.continued)
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return p.scriptProvider.Stream(ctx, req)
}
func TestJobWaitingForInput(t *testing.T) {
	p := &waitingProvider{continued: make(chan struct{}), release: make(chan struct{})}
	p.turns = [][]glue.ProviderEvent{turn(`{"items":[{"title":"write"}]}`), {{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "write", Name: "write_file", Arguments: []byte(`{"path":"hello.txt","content":"hello"}`)}}, {Type: glue.ProviderEventDone}}, turn("done"), turn(`{"done":true,"items":[{"title":"write","done":true}]}`)}
	m, err := New(Options{StateDir: t.TempDir(), ProviderFactory: func(string) (glue.Provider, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	job, err := m.Create(context.Background(), Spec{Objective: "write", Repo: RepoSpec{Path: repoFixture(t)}, Policy: "ask-all"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	permissionID := ""
	for permissionID == "" {
		events, err := m.Events(context.Background(), job.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			if e.Type == "permission_request" {
				permissionID = e.Payload.(map[string]any)["permission_id"].(string)
			}
		}
		if permissionID != "" {
			current, err := m.Get(context.Background(), job.ID)
			if err != nil || current.Status != WaitingForInput {
				t.Fatalf("job %+v err %v", current, err)
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no waiting permission")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := m.Decide(context.Background(), job.ID, permissionID, glue.PermissionDecision{Allow: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.continued:
	case <-time.After(10 * time.Second):
		t.Fatal("did not continue")
	}
	current, err := m.Get(context.Background(), job.ID)
	if err != nil || current.Status != Running {
		t.Fatalf("job %+v err %v", current, err)
	}
	if err := m.Decide(context.Background(), job.ID, permissionID, glue.PermissionDecision{Allow: true}); !errors.Is(err, ErrConflict) {
		t.Fatal("accepted repeated decision")
	}
	close(p.release)
	finished := waitJob(t, m, job.ID)
	if finished.Status != Achieved {
		t.Fatal(finished)
	}
	if data, err := os.ReadFile(filepath.Join(finished.Workspace, "hello.txt")); err != nil || string(data) != "hello" {
		t.Fatalf("file %q err %v", data, err)
	}
}
func TestJobWallClockBudget(t *testing.T) {
	p := blockingJobProvider{make(chan struct{})}
	timer := make(chan func(), 1)
	m, err := New(Options{StateDir: t.TempDir(), ProviderFactory: func(string) (glue.Provider, error) { return p, nil }, AfterFunc: func(d time.Duration, f func()) func() bool {
		if d != time.Hour {
			t.Errorf("duration %s", d)
		}
		timer <- f
		return func() bool { return true }
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	job, err := m.Create(context.Background(), Spec{Objective: "wait", Repo: RepoSpec{Path: repoFixture(t)}, Budgets: Budgets{WallClock: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.started:
	case <-time.After(10 * time.Second):
		t.Fatal("provider did not start")
	}
	select {
	case fire := <-timer:
		fire()
	case <-time.After(time.Second):
		t.Fatal("no timer registered")
	}
	if got := waitJob(t, m, job.ID); got.Status != BudgetLimited {
		t.Fatal(got)
	}
}

func TestJobWaitsForEveryPermission(t *testing.T) {
	m, err := New(Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	job, err := m.Create(context.Background(), Spec{Objective: "permissions", Repo: RepoSpec{Path: "unused"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &execution{ctx: ctx, cancel: cancel}
	m.active[job.ID] = run
	if err := m.record(job.ID, "job_started", nil, "", nil, Running); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	t.Cleanup(func() { cancel(); wg.Wait() })
	results := make(chan glue.PermissionDecision, 2)
	for _, tool := range []string{"one", "two"} {
		go func(tool string) {
			defer wg.Done()
			d, err := (jobPermission{manager: m, id: job.ID, run: run}).Decide(ctx, glue.PermissionRequest{Tool: tool})
			if err != nil {
				t.Error(err)
			}
			results <- d
		}(tool)
	}
	var ids []string
	deadline := time.Now().Add(5 * time.Second)
	for len(ids) != 2 {
		ids = nil
		events, err := m.Events(ctx, job.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			if e.Type == "permission_request" {
				ids = append(ids, e.Payload.(map[string]any)["permission_id"].(string))
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("permissions did not arrive")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := m.Decide(ctx, job.ID, ids[0], glue.PermissionDecision{Allow: true}); err != nil {
		t.Fatal(err)
	}
	current, err := m.Get(ctx, job.ID)
	if err != nil || current.Status != WaitingForInput {
		t.Fatalf("job %+v err %v", current, err)
	}
	if err := m.Decide(ctx, job.ID, ids[1], glue.PermissionDecision{Allow: false}); err != nil {
		t.Fatal(err)
	}
	current, err = m.Get(ctx, job.ID)
	if err != nil || current.Status != Running {
		t.Fatalf("job %+v err %v", current, err)
	}
	wg.Wait()
	allowed := 0
	for i := 0; i < 2; i++ {
		if (<-results).Allow {
			allowed++
		}
	}
	if allowed != 1 {
		t.Fatalf("allowed %d", allowed)
	}
}
func TestJobCloseWhileWaiting(t *testing.T) {
	p := &scriptProvider{turns: [][]glue.ProviderEvent{turn(`{"items":[{"title":"write"}]}`), {{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "write", Name: "write_file", Arguments: []byte(`{"path":"hello.txt","content":"hello"}`)}}, {Type: glue.ProviderEventDone}}}}
	dir := t.TempDir()
	m, err := New(Options{StateDir: dir, ProviderFactory: func(string) (glue.Provider, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	job, err := m.Create(context.Background(), Spec{Objective: "write", Repo: RepoSpec{Path: repoFixture(t)}, Policy: "ask-all"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		current, err := m.Get(context.Background(), job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status == WaitingForInput {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not waiting")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(Options{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	current, err := reopened.Get(context.Background(), job.ID)
	if err != nil || current.Status != Interrupted {
		t.Fatalf("job %+v err %v", current, err)
	}
	events, err := reopened.Events(context.Background(), job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	expired := false
	for _, e := range events {
		if e.Type == "permission_expired" {
			expired = true
		}
	}
	if !expired {
		t.Fatal("permission did not expire")
	}
	if _, err := os.Stat(filepath.Join(current.Workspace, "hello.txt")); !os.IsNotExist(err) {
		t.Fatalf("unapproved write: %v", err)
	}
}

func TestJobStopsBudgetTimer(t *testing.T) {
	p := &scriptProvider{turns: [][]glue.ProviderEvent{turn(`{"items":[{"title":"A"}]}`), turn("done"), turn(`{"done":true,"items":[{"title":"A","done":true}]}`)}}
	var stopCount atomic.Int32
	var fire func()
	m, err := New(Options{StateDir: t.TempDir(), ProviderFactory: func(string) (glue.Provider, error) { return p, nil }, AfterFunc: func(_ time.Duration, f func()) func() bool {
		fire = f
		return func() bool { stopCount.Add(1); return true }
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	job, err := m.Create(context.Background(), Spec{Objective: "A", Repo: RepoSpec{Path: repoFixture(t)}, Budgets: Budgets{WallClock: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if got := waitJob(t, m, job.ID); got.Status != Achieved {
		t.Fatal(got)
	}
	if stopCount.Load() != 1 {
		t.Fatalf("timer stops %d", stopCount.Load())
	}
	fire()
	current, err := m.Get(context.Background(), job.ID)
	if err != nil || current.Status != Achieved {
		t.Fatalf("job %+v err %v", current, err)
	}
}

func TestJobRememberSessionSkipsSecondPrompt(t *testing.T) {
	p := &scriptProvider{turns: [][]glue.ProviderEvent{
		turn(`{"items":[{"title":"write twice"}]}`),
		{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "w1", Name: "write_file", Arguments: []byte(`{"path":"hello.txt","content":"v1"}`)}}, {Type: glue.ProviderEventDone}},
		{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "w2", Name: "write_file", Arguments: []byte(`{"path":"hello.txt","content":"v2"}`)}}, {Type: glue.ProviderEventDone}},
		turn("done"),
		turn(`{"done":true,"items":[{"title":"write twice","done":true}]}`),
	}}
	m, err := New(Options{StateDir: t.TempDir(), ProviderFactory: func(string) (glue.Provider, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	job, err := m.Create(context.Background(), Spec{Objective: "write twice", Repo: RepoSpec{Path: repoFixture(t)}, Policy: "ask-all"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	permID := ""
	for permID == "" {
		events, _ := m.Events(context.Background(), job.ID, 0)
		for _, e := range events {
			if e.Type == "permission_request" {
				permID = e.Payload.(map[string]any)["permission_id"].(string)
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no waiting permission")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := m.Decide(context.Background(), job.ID, permID, glue.PermissionDecision{Allow: true, RememberFor: glue.RememberSession}); err != nil {
		t.Fatal(err)
	}
	finished := waitJob(t, m, job.ID)
	if finished.Status != Achieved {
		t.Fatalf("status = %s, want achieved", finished.Status)
	}
	perms, err := m.Permissions(context.Background(), job.ID, "")
	if err != nil || len(perms) != 1 {
		t.Fatalf("permissions = %+v, err = %v; want exactly 1 prompt", perms, err)
	}
}
