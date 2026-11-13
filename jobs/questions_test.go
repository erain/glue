// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"context"
	"strings"
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
