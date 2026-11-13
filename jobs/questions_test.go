// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erain/glue"
)

func askTurn(question string) []glue.ProviderEvent {
	return []glue.ProviderEvent{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "q1", Name: "ask_user", Arguments: []byte(`{"question":"` + question + `","options":["create it","stop"]}`)}}, {Type: glue.ProviderEventDone}}
}

// recordingScript plays turns and records the last request's final message.
type recordingScript struct {
	scriptProvider
	last chan string
}

func (p *recordingScript) Stream(ctx context.Context, req glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	if n := len(req.Messages); n > 0 {
		for _, c := range req.Messages[n-1].Content {
			if c.Text != "" {
				select {
				case p.last <- c.Text:
				default:
				}
			}
		}
	}
	return p.scriptProvider.Stream(ctx, req)
}

func questionJob(t *testing.T, spec Spec) (*Manager, *recordingScript, Job) {
	t.Helper()
	p := &recordingScript{last: make(chan string, 64), scriptProvider: scriptProvider{turns: [][]glue.ProviderEvent{
		turn(`{"items":[{"title":"add Shout"}]}`),
		askTurn("Greet does not exist. Should I create it?"),
		turn("created Greet and Shout"),
		turn(`{"done":true,"items":[{"title":"add Shout","done":true,"evidence":"ok"}],"summary":"done"}`),
	}}}
	m, err := New(Options{StateDir: t.TempDir(), ProviderFactory: func(string) (glue.Provider, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	spec.Objective, spec.Repo = "add Shout using Greet", RepoSpec{Path: repoFixture(t)}
	job, err := m.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	return m, p, job
}

func waitStatus(t *testing.T, m *Manager, id string, want Status) Detail {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if d, err := m.Inspect(context.Background(), id); err == nil && d.Status == want {
			return d
		}
	}
	t.Fatalf("job never reached %s", want)
	return Detail{}
}

func TestAskUserBlocksUntilAnswered(t *testing.T) {
	m, p, job := questionJob(t, Spec{})
	d := waitStatus(t, m, job.ID, WaitingForInput)
	if d.Question == nil || !strings.Contains(d.Question.Question, "Greet does not exist") || len(d.Question.Options) != 2 {
		t.Fatalf("open question = %+v", d.Question)
	}
	if err := m.Answer(context.Background(), job.ID, "", "Yes, create Greet first."); err != nil {
		t.Fatal(err)
	}
	if got := waitJob(t, m, job.ID); got.Status != Achieved {
		t.Fatalf("status = %s", got.Status)
	}
	var sawAnswer bool
	for drained := false; !drained; {
		select {
		case text := <-p.last:
			sawAnswer = sawAnswer || strings.Contains(text, "The developer answered: Yes, create Greet first.")
		default:
			drained = true
		}
	}
	if !sawAnswer {
		t.Fatal("the model never received the answer")
	}
	if d, _ := m.Inspect(context.Background(), job.ID); d.Question != nil {
		t.Fatalf("question still open after the answer: %+v", d.Question)
	}
	if err := m.Answer(context.Background(), job.ID, "", "again"); err == nil {
		t.Fatal("answering with no open question should fail")
	}
}

func TestQuestionTimeoutAssumes(t *testing.T) {
	m, _, job := questionJob(t, Spec{QuestionTimeout: 50 * time.Millisecond})
	if got := waitJob(t, m, job.ID); got.Status != Achieved {
		t.Fatalf("status = %s", got.Status)
	}
	res, err := m.Result(context.Background(), job.ID)
	if err != nil || len(res.Assumptions) != 1 || !strings.Contains(res.Assumptions[0], "Greet does not exist") {
		t.Fatalf("assumptions = %v, %v", res.Assumptions, err)
	}
}

func TestAskUserAssumeMode(t *testing.T) {
	m, _, job := questionJob(t, Spec{OnQuestion: OnQuestionAssume})
	if got := waitJob(t, m, job.ID); got.Status != Achieved {
		t.Fatalf("status = %s", got.Status)
	}
	for _, typ := range eventTypes(t, m, job.ID) {
		if typ == "question" {
			t.Fatal("assume mode must not wait for an answer")
		}
	}
	if res, _ := m.Result(context.Background(), job.ID); len(res.Assumptions) != 1 {
		t.Fatalf("assumptions = %v", res.Assumptions)
	}
	if _, err := m.Create(context.Background(), Spec{Objective: "x", Repo: RepoSpec{Path: repoFixture(t)}, OnQuestion: "maybe"}); err == nil {
		t.Fatal("unknown on_question accepted")
	}
}

func TestQuestionSurvivesRestart(t *testing.T) {
	m := setupManager(t, &countingProvider{})
	job, err := m.Create(context.Background(), Spec{Objective: "x", Repo: RepoSpec{Path: repoFixture(t)}})
	if err != nil {
		t.Fatal(err)
	}
	// A question recorded by a process that has since gone away.
	if err := m.record(job.ID, "question", map[string]any{"question_id": job.ID + ":question:9", "question": "Which branch?"}, "", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Answer(context.Background(), job.ID, "", "Use release-1"); err != nil {
		t.Fatalf("answer with no running process: %v", err)
	}
	// After the restart the model asks the same question; it gets the stored answer at once.
	got, err := m.ask(context.Background(), job.ID, &execution{}, "Which branch?", nil)
	if err != nil || got != "The developer answered: Use release-1" {
		t.Fatalf("ask after restart = %q, %v", got, err)
	}
}

func TestAnswerBecomesGuidance(t *testing.T) {
	m := setupManager(t, &countingProvider{})
	job, err := m.Create(context.Background(), Spec{Objective: "x", Repo: RepoSpec{Path: repoFixture(t)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.record(job.ID, "question", map[string]any{"question_id": job.ID + ":question:3", "question": "What retry limit?"}, "", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Answer(context.Background(), job.ID, "", "7"); err != nil {
		t.Fatal(err)
	}
	g, err := m.guidance(context.Background(), job.ID, 2)
	if err != nil || !strings.Contains(g, `You asked the developer: "What retry limit?". The developer answered: 7`) {
		t.Fatalf("guidance = %q, %v", g, err)
	}
}

func TestSpecContextInPrompts(t *testing.T) {
	p := &requestRecorder{scriptProvider: scriptProvider{turns: [][]glue.ProviderEvent{
		turn(`{"items":[{"title":"fix the race"}]}`),
		turn("fixed"),
		turn(`{"done":true,"items":[{"title":"fix the race","done":true,"evidence":"ok"}],"summary":"done"}`),
	}}}
	m, err := New(Options{StateDir: t.TempDir(), ProviderFactory: func(string) (glue.Provider, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	job, err := m.Create(context.Background(), Spec{Objective: "fix the flaky test", Context: "developer: TestPool races on the counter in pool.go:42", Repo: RepoSpec{Path: repoFixture(t)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if got := waitJob(t, m, job.ID); got.Status != Achieved {
		t.Fatalf("status = %s", got.Status)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, want := range []bool{true, true, false} { // planner, maker, checker
		has := strings.Contains(p.texts[i], "Context from the developer's session") && strings.Contains(p.texts[i], "pool.go:42")
		if has != want {
			t.Fatalf("request %d has context = %v:\n%s", i, has, p.texts[i])
		}
	}
	// The checker (request 3) audits: no file-writing tools, no ask_user.
	if !slices.Contains(p.tools[1], "write_file") || !slices.Contains(p.tools[1], "ask_user") {
		t.Fatalf("maker tools = %v", p.tools[1])
	}
	for _, name := range []string{"write_file", "edit_file", "ask_user"} {
		if slices.Contains(p.tools[2], name) {
			t.Fatalf("checker has %s: %v", name, p.tools[2])
		}
	}
	if slices.Contains(p.tools[0], "write_file") || slices.Contains(p.tools[0], "edit_file") || slices.Contains(p.tools[0], "shell_exec") {
		t.Fatalf("planner can write files or run commands: %v", p.tools[0])
	}
	if _, err := m.Create(context.Background(), Spec{Objective: "x", Context: strings.Repeat("x", MaxContextBytes+1), Repo: RepoSpec{Path: repoFixture(t)}}); err == nil {
		t.Fatal("oversized context accepted")
	}
}

// requestRecorder records the user text of each request's first user message.
type requestRecorder struct {
	scriptProvider
	mu    sync.Mutex
	texts []string
	tools [][]string
}

func (p *requestRecorder) Stream(ctx context.Context, req glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	var text string
	for _, msg := range req.Messages {
		if msg.Role == glue.MessageRoleUser {
			for _, c := range msg.Content {
				text += c.Text
			}
			break
		}
	}
	var names []string
	for _, s := range req.Tools {
		names = append(names, s.Name)
	}
	p.mu.Lock()
	p.texts = append(p.texts, text)
	p.tools = append(p.tools, names)
	p.mu.Unlock()
	return p.scriptProvider.Stream(ctx, req)
}

func TestManagerInterruptCancelsShellCommand(t *testing.T) {
	p := &requestRecorder{scriptProvider: scriptProvider{turns: [][]glue.ProviderEvent{
		turn(`{"items":[{"title":"wait"}]}`),
		{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "s1", Name: "shell_exec", Arguments: []byte(`{"argv":["sleep","60"]}`)}}, {Type: glue.ProviderEventDone}},
		turn("switched to the cache"),
		turn(`{"done":true,"items":[{"title":"wait","done":true,"evidence":"ok"}],"summary":"done"}`),
	}}}
	m, err := New(Options{StateDir: t.TempDir(), ProviderFactory: func(string) (glue.Provider, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	job, err := m.Create(context.Background(), Spec{Objective: "wait", Policy: "yolo", AllowedBinaries: []string{"sleep"}, Repo: RepoSpec{Path: repoFixture(t)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if slices.Contains(eventTypes(t, m, job.ID), "tool_start") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shell_exec never started")
		}
	}
	t0 := time.Now()
	if err := m.Interrupt(context.Background(), job.ID, "stop waiting, use the cache"); err != nil {
		t.Fatal(err)
	}
	if got := waitJob(t, m, job.ID); got.Status != Achieved {
		t.Fatalf("status = %s", got.Status)
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("interrupt took %s", d)
	}
	types := eventTypes(t, m, job.ID)
	if !slices.Contains(types, "turn_interrupted") || !slices.Contains(types, "operator_message_delivered") {
		t.Fatalf("events = %v", types)
	}
}
