package evals

import (
	"context"
	"testing"

	"github.com/erain/glue"
)

func TestJudgeScores(t *testing.T) {
	// The judge's grading prompt opens with "You are grading an AI
	// agent's response"; the scripted provider matches on that and
	// returns the verdict JSON the judge parses.
	provider := &scriptedProvider{
		replies: map[string]string{
			"grading an AI agent": `{"score": 0.9, "reason": "fully satisfies the rubric"}`,
		},
		def: `{"score": 0.0, "reason": "no match"}`,
	}
	judge := Judge{Agent: newAgent(provider), Rubric: "The response must greet the user warmly."}

	got, err := judge.Score(context.Background(), Result{
		Case:     Case{Name: "greets", Prompt: "greet the user"},
		Response: promptResult("Hello, so lovely to see you!"),
	})
	if err != nil {
		t.Fatalf("judge errored: %v", err)
	}
	if got.Value != 0.9 {
		t.Errorf("score = %v, want 0.9", got.Value)
	}
	if got.Reason == "" {
		t.Error("judge should carry a reason")
	}
	if got.Scorer != "judge" {
		t.Errorf("scorer name = %q, want judge", got.Scorer)
	}
}

func TestJudgeAsScorerInSuite(t *testing.T) {
	provider := &scriptedProvider{
		replies: map[string]string{
			"greet the user":      "Hello, friend!",                             // agent-under-test reply
			"grading an AI agent": `{"score": 0.95, "reason": "warm greeting"}`, // judge reply
		},
		def: "?",
	}
	agent := newAgent(provider)
	report, err := Runner{Agent: agent}.Run(context.Background(), Suite{
		Name: "judged",
		Cases: []Case{{
			Name:    "warm-greeting",
			Prompt:  "greet the user",
			Scorers: []Scorer{Judge{Agent: agent, Rubric: "Greet the user warmly."}},
		}},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !report.AllPassed() {
		t.Errorf("judged case should pass (0.95 >= 0.5): %+v", report.Cases[0])
	}
}

func TestJudgeMisconfigured(t *testing.T) {
	if _, err := (Judge{Rubric: "x"}).Score(context.Background(), Result{}); err == nil {
		t.Error("expected error when judge has no agent")
	}
	if _, err := (Judge{Agent: newAgent(&scriptedProvider{def: "{}"})}).Score(context.Background(), Result{}); err == nil {
		t.Error("expected error when judge has an empty rubric")
	}
}

func promptResult(text string) glue.PromptResult { return glue.PromptResult{Text: text} }
