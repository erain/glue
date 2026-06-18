package evals

import (
	"context"
	"fmt"
	"strings"

	"github.com/erain/glue"
)

// Judge is an LLM-as-judge [Scorer]: it asks a grading agent to rate how
// well the response satisfies a rubric, on a 0..1 scale. Use it for
// open-ended quality that deterministic scorers can't capture (tone,
// correctness of prose, following multi-part instructions).
//
// The grading agent should be independent of the agent under test —
// typically a strong model with no tools — so the judge is not grading
// its own work.
type Judge struct {
	// Agent grades the response. Required.
	Agent *glue.Agent
	// Model overrides the grading model. Empty uses the agent default.
	Model string
	// Rubric describes what a good response looks like. Required.
	Rubric string
	// Name overrides the scorer name in reports (default "judge").
	Name_ string
}

// Name implements [Scorer].
func (j Judge) Name() string {
	if j.Name_ != "" {
		return j.Name_
	}
	return "judge"
}

type judgeVerdict struct {
	Score  float64 `json:"score"`
	Reason string  `json:"reason"`
}

var judgeSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"score":  map[string]any{"type": "number", "description": "quality from 0.0 (fails the rubric) to 1.0 (fully satisfies it)"},
		"reason": map[string]any{"type": "string", "description": "one or two sentences justifying the score"},
	},
	"required": []string{"score", "reason"},
}

// Score implements [Scorer]. A misconfigured judge (no agent or rubric)
// or a grading-call failure returns an error, failing the case as a
// harness problem rather than silently scoring zero.
func (j Judge) Score(ctx context.Context, r Result) (Score, error) {
	if j.Agent == nil {
		return Score{}, fmt.Errorf("judge: no grading agent")
	}
	if strings.TrimSpace(j.Rubric) == "" {
		return Score{}, fmt.Errorf("judge: empty rubric")
	}

	sess, err := j.Agent.Session(ctx, "eval-judge/"+r.Case.Name)
	if err != nil {
		return Score{}, fmt.Errorf("judge: open session: %w", err)
	}

	opts := []glue.PromptOption{glue.WithJSONSchema(judgeSchema)}
	if j.Model != "" {
		opts = append(opts, glue.WithModel(j.Model))
	}

	var verdict judgeVerdict
	if _, err := sess.PromptJSON(ctx, j.gradePrompt(r), &verdict, opts...); err != nil {
		return Score{}, fmt.Errorf("judge: grading call: %w", err)
	}

	return Score{
		Scorer: j.Name(),
		Value:  verdict.Score,
		Reason: verdict.Reason,
	}, nil
}

func (j Judge) gradePrompt(r Result) string {
	var b strings.Builder
	b.WriteString("You are grading an AI agent's response. Score how well it satisfies the rubric.\n\n")
	b.WriteString("RUBRIC:\n")
	b.WriteString(strings.TrimSpace(j.Rubric))
	b.WriteString("\n\nThe user asked:\n")
	b.WriteString(strings.TrimSpace(r.Case.Prompt))
	b.WriteString("\n\nThe agent responded:\n")
	b.WriteString(strings.TrimSpace(r.Text()))
	b.WriteString("\n\nReturn a score from 0.0 to 1.0 and a short reason. Be a strict grader: reserve scores above 0.8 for responses that clearly satisfy every part of the rubric.")
	return b.String()
}
