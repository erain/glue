package jobs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erain/glue"
)

type scriptProvider struct {
	mu       sync.Mutex
	turns    [][]glue.ProviderEvent
	requests []glue.ProviderRequest
}

func (p *scriptProvider) Stream(_ context.Context, req glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	ch := make(chan glue.ProviderEvent, 8)
	if len(p.turns) == 0 {
		return nil, errors.New("unexpected provider turn")
	}
	for _, e := range p.turns[0] {
		ch <- e
	}
	p.turns = p.turns[1:]
	close(ch)
	return ch, nil
}
func turn(text string) []glue.ProviderEvent {
	return []glue.ProviderEvent{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventTextDelta, Delta: text}, {Type: glue.ProviderEventDone, Message: &glue.Message{Role: glue.MessageRoleAssistant, Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: text}}}}}
}
func repoFixture(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-qb", "main"}, {"config", "user.email", "test@example.com"}, {"config", "user.name", "test"}, {"commit", "--allow-empty", "-qm", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	return repo
}
func waitJob(t *testing.T, m *Manager, id string) Job {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		job, err := m.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if job.Status != Created && job.Status != Running && job.Status != WaitingForInput && job.Status != WaitingForCredentials {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("job stuck: %+v", job)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func achievedJob(t *testing.T) (*Manager, Job, string) {
	t.Helper()
	repo := repoFixture(t)
	p := &scriptProvider{turns: [][]glue.ProviderEvent{
		turn(`{"items":[{"title":"write file"}]}`),
		{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "write", Name: "write_file", Arguments: []byte(`{"path":"created.txt","content":"hello"}`)}}, {Type: glue.ProviderEventDone}},
		turn("wrote file"), turn(`{"done":true,"items":[{"title":"write file","done":true,"evidence":"created.txt"}],"summary":"done"}`),
	}}
	m, err := New(Options{StateDir: t.TempDir(), ProviderFactory: func(string) (glue.Provider, error) { return p, nil }, Permission: glue.AllowAll{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	job, err := m.Create(context.Background(), Spec{Objective: "write file", Repo: RepoSpec{Path: repo}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	return m, waitJob(t, m, job.ID), repo
}
func TestJobAchieved(t *testing.T) {
	_, job, repo := achievedJob(t)
	if job.Status != Achieved {
		t.Fatalf("job %+v", job)
	}
	data, err := os.ReadFile(filepath.Join(job.Workspace, "created.txt"))
	if err != nil || string(data) != "hello" {
		t.Fatalf("edit %q err %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(repo, "created.txt")); !os.IsNotExist(err) {
		t.Fatalf("caller edited: %v", err)
	}
	cmd := exec.Command("git", "-C", job.Workspace, "branch", "--show-current")
	out, err := cmd.Output()
	if err != nil || string(out) != "glue/"+job.ID+"\n" {
		t.Fatalf("branch %s err %v", out, err)
	}
	if job.BaseSHA == "" || job.GoalID != job.ID {
		t.Fatal(job)
	}
}

func TestJobsSurviveManagerRestart(t *testing.T) {
	m, job, _ := achievedJob(t)
	dir := m.options.StateDir
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	other, err := New(Options{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	got, err := other.Get(context.Background(), job.ID)
	if err != nil || got.Status != Achieved {
		t.Fatalf("reopened %+v err %v", got, err)
	}
	events, err := other.Events(context.Background(), job.ID, 0)
	if err != nil || events[len(events)-1].Type != "job_done" {
		t.Fatalf("events %v err %v", events, err)
	}
}
func TestJobEventsIncludeMakerToolCalls(t *testing.T) {
	m, job, _ := achievedJob(t)
	events, err := m.Events(context.Background(), job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Fatalf("seq %d at %d", e.Seq, i)
		}
		if e.Type == "tool_start" || e.Type == "tool_end" {
			if e.Phase != "maker" || e.Iteration == nil || *e.Iteration != 1 {
				t.Fatalf("event %+v", e)
			}
			seen[e.Type] = true
		}
	}
	if !seen["tool_start"] || !seen["tool_end"] {
		t.Fatal(seen)
	}
}
func TestJobBlockedAndMaxIterations(t *testing.T) {
	for _, tc := range []struct {
		want              Status
		limit, iterations int
	}{{Blocked, 5, 3}, {MaxIterations, 1, 1}} {
		t.Run(string(tc.want), func(t *testing.T) {
			p := &scriptProvider{turns: [][]glue.ProviderEvent{turn(`{"items":[{"title":"A"}]}`)}}
			for i := 0; i < tc.iterations; i++ {
				p.turns = append(p.turns, turn("no progress"), turn(`{"done":false,"items":[{"title":"A"}],"summary":"unfinished"}`))
			}
			m, err := New(Options{StateDir: t.TempDir(), ProviderFactory: func(string) (glue.Provider, error) { return p, nil }})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { m.Close() })
			job, err := m.Create(context.Background(), Spec{Objective: "A", Repo: RepoSpec{Path: repoFixture(t)}, Budgets: Budgets{MaxIterations: tc.limit}})
			if err != nil {
				t.Fatal(err)
			}
			if err := m.Start(context.Background(), job.ID); err != nil {
				t.Fatal(err)
			}
			if got := waitJob(t, m, job.ID); got.Status != tc.want {
				t.Fatalf("job %+v", got)
			}
		})
	}
}

type blockingJobProvider struct{ started chan struct{} }

func (p blockingJobProvider) Stream(ctx context.Context, _ glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	ch := make(chan glue.ProviderEvent)
	close(p.started)
	go func() { <-ctx.Done(); close(ch) }()
	return ch, nil
}
func TestJobCancel(t *testing.T) {
	for _, failWrite := range []bool{false, true} {
		t.Run(fmt.Sprint(failWrite), func(t *testing.T) {
			p := blockingJobProvider{make(chan struct{})}
			m, err := New(Options{StateDir: t.TempDir(), ProviderFactory: func(string) (glue.Provider, error) { return p, nil }})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { m.Close() })
			job, err := m.Create(context.Background(), Spec{Objective: "wait", Repo: RepoSpec{Path: repoFixture(t)}})
			if err != nil {
				t.Fatal(err)
			}
			if err := m.Start(context.Background(), job.ID); err != nil {
				t.Fatal(err)
			}
			select {
			case <-p.started:
			case <-time.After(15 * time.Second):
				t.Fatal("provider did not start")
			}
			if err := m.Start(context.Background(), job.ID); err == nil {
				t.Fatal("started an active job twice")
			}
			if failWrite {
				if _, err := m.store.DB.Exec("DROP TABLE events"); err != nil {
					t.Fatal(err)
				}
			}
			if err := m.Cancel(context.Background(), job.ID); err != nil {
				t.Fatal(err)
			}
			if failWrite {
				if err := m.Close(); err == nil {
					t.Fatal("Close hid terminal event failure")
				}
				return
			}
			if got := waitJob(t, m, job.ID); got.Status != Canceled {
				t.Fatal(got)
			}
		})
	}
}

func TestJobPreparationFailure(t *testing.T) {
	m, err := New(Options{StateDir: t.TempDir(), ProviderFactory: func(string) (glue.Provider, error) {
		t.Error("constructed provider after failed preparation")
		return nil, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	job, err := m.Create(context.Background(), Spec{Objective: "fail", Repo: RepoSpec{Path: filepath.Join(t.TempDir(), "missing")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if got := waitJob(t, m, job.ID); got.Status != Errored {
		t.Fatal(got)
	}
	events, err := m.Events(context.Background(), job.ID, 0)
	if err != nil || events[len(events)-1].Type != "job_done" {
		t.Fatalf("events %v err %v", events, err)
	}
}

func newInteractiveMgr(t *testing.T, stateDir string, p *scriptProvider) *Manager {
	m, err := New(Options{StateDir: cmp.Or(stateDir, t.TempDir()), ProviderFactory: func(string) (glue.Provider, error) { return p, nil }, Permission: glue.AllowAll{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}
func waitInteractive(t *testing.T, m *Manager, id string) Job {
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if j, err := m.Get(context.Background(), id); err == nil && j.Status == WaitingForInput && j.StatusReason == ReasonAwaitingTurn {
			return j
		}
	}
	t.Fatalf("interactive job %s did not reach awaiting_turn", id)
	return Job{}
}
func startInteractive(t *testing.T, m *Manager, repo, obj string) Job {
	job, err := m.Create(context.Background(), Spec{Mode: ModeInteractive, Objective: obj, Repo: RepoSpec{Path: repo}})
	if err != nil || m.Start(context.Background(), job.ID) != nil {
		t.Fatalf("create/start: %v", err)
	}
	return waitInteractive(t, m, job.ID)
}
func toolCallEvents(id, name, args string) []glue.ProviderEvent {
	return []glue.ProviderEvent{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: id, Name: name, Arguments: []byte(args)}}, {Type: glue.ProviderEventDone}}
}
func lastMsgText(p *scriptProvider) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return fmt.Sprint(p.requests[len(p.requests)-1].Messages)
}
func TestInteractiveJobMultiTurnPreservesSessionAndWorkspace(t *testing.T) {
	ctx, repo := context.Background(), repoFixture(t)
	p := &scriptProvider{turns: [][]glue.ProviderEvent{turn("hello turn 1"), toolCallEvents("w1", "write_file", `{"path":"notes.txt","content":"interactive edit\n"}`), turn("wrote notes.txt"), turn("recalled history")}}
	m := newInteractiveMgr(t, "", p)
	if _, err := m.Create(ctx, Spec{Mode: "bogus", Objective: "x", Repo: RepoSpec{Path: repo}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid on unknown mode, got %v", err)
	}
	job := startInteractive(t, m, repo, "explore repo")
	if job.GoalID != "" || m.Message(ctx, job.ID, "write notes.txt") != nil {
		t.Fatalf("turn 1 state=%+v", job)
	}
	waitInteractive(t, m, job.ID)
	if patch, err := m.Patch(ctx, job.ID); err != nil || !strings.Contains(string(patch), "+interactive edit") || m.Message(ctx, job.ID, "summarize prior turns") != nil {
		t.Fatalf("err=%v patch=%q", err, patch)
	}
	waitInteractive(t, m, job.ID)
	if raw := lastMsgText(p); !strings.Contains(raw, "hello turn 1") || !strings.Contains(raw, "wrote notes.txt") {
		t.Fatalf("turn 3 missing persistent chat history: %s", raw)
	}
}
func TestInteractiveJobRestartRecoveryAwaitingTurn(t *testing.T) {
	ctx, repo, stateDir := context.Background(), repoFixture(t), t.TempDir()
	p := &scriptProvider{turns: [][]glue.ProviderEvent{turn("before restart"), turn("after restart")}}
	m1 := newInteractiveMgr(t, stateDir, p)
	j1 := startInteractive(t, m1, repo, "turn 1")
	_ = m1.Close()
	m2 := newInteractiveMgr(t, stateDir, p)
	if j2, err := m2.Get(ctx, j1.ID); err != nil || j2.Status != WaitingForInput || j2.StatusReason != ReasonAwaitingTurn || j2.Workspace != j1.Workspace || m2.Message(ctx, j1.ID, "turn 2") != nil {
		t.Fatalf("recovered job=%+v err=%v", j2, err)
	}
	waitInteractive(t, m2, j1.ID)
	if raw := lastMsgText(p); !strings.Contains(raw, "before restart") {
		t.Fatalf("session lost across restart: %s", raw)
	}
}
func TestInteractiveAwaitingTurnDoesNotSpamNotify(t *testing.T) {
	fastNotify(t)
	ctx, repo, sink := context.Background(), repoFixture(t), newNotifySink(t, 0)
	m := newInteractiveMgr(t, "", &scriptProvider{turns: [][]glue.ProviderEvent{turn("turn 1 done"), toolCallEvents("q1", "ask_user", `{"question":"Proceed?"}`), turn("thanks")}})
	m.SetCredentials(Credentials{NotifyURLs: []string{sink.ts.URL}})
	job := startInteractive(t, m, repo, "chat")
	if m.Message(ctx, job.ID, "ask me") != nil {
		t.Fatal("message failed")
	}
	if got := sink.wait(t, 1); got[0].Event != "waiting_for_input" || got[0].Question != "Proceed?" || m.Answer(ctx, job.ID, "", "yes") != nil {
		t.Fatalf("expected question notification, got %+v", got)
	}
	waitInteractive(t, m, job.ID)
	_ = m.Cancel(ctx, job.ID)
	_ = m.Close()
	if len(sink.got) != 2 || sink.got[1].Event != "done" || sink.got[1].Status != Canceled {
		t.Fatalf("expected only question + done notifications, got %+v", sink.got)
	}
}
