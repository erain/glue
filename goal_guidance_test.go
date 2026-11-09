// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package glue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestGoalGuidanceAtNextMaker(t *testing.T) {
	p := &recordingProvider{turns: [][]ProviderEvent{
		goalTurn("first maker", 0), goalTurn(`{"items":[{"title":"A"}],"summary":"A remains"}`, 0),
		goalTurn("second maker", 0), goalTurn(`{"done":true,"items":[{"title":"A","done":true}]}`, 0),
	}}
	queued := ""
	var iterations []int
	result, err := newGoalAgent(p).PursueGoal(context.Background(), GoalSpec{
		Objective: "finish A", Checklist: []ChecklistItem{{Title: "A"}}, StartIteration: 7, MaxIterations: 2,
		Guidance: func(ctx context.Context, iteration int) (string, error) {
			if ctx.Err() != nil {
				t.Fatal(ctx.Err())
			}
			iterations = append(iterations, iteration)
			return queued, nil
		},
		OnSession: func(phase string, iteration int, _ *Session) {
			if phase == "maker" && iteration == 7 {
				queued = "Please include examples."
			}
		},
	})
	if err != nil || result.Status != GoalAchieved {
		t.Fatalf("result %+v err %v", result, err)
	}
	if fmt.Sprint(iterations) != "[7 8]" || len(p.requests) != 4 {
		t.Fatalf("iterations %v requests %d", iterations, len(p.requests))
	}
	for i, req := range p.requests {
		var prompt string
		for _, msg := range req.Messages {
			if msg.Role == MessageRoleUser {
				for _, part := range msg.Content {
					prompt += part.Text
				}
			}
		}
		want := i >= 2
		if strings.Contains(prompt, "Please include examples.") != want {
			t.Fatalf("request %d guidance: %q", i, prompt)
		}
		if want && !strings.HasPrefix(prompt, "Operator guidance\nPlease include examples.\n\n") {
			t.Fatalf("request %d block: %q", i, prompt)
		}
		if !strings.Contains(prompt, "finish A") || !strings.Contains(prompt, "A") {
			t.Fatalf("lost objective/checklist: %q", prompt)
		}
	}
}

func TestGoalGuidanceErrorStopsBeforeMaker(t *testing.T) {
	sentinel := errors.New("guidance storage unavailable")
	p := &recordingProvider{}
	result, err := newGoalAgent(p).PursueGoal(context.Background(), GoalSpec{
		Objective: "finish A", Checklist: []ChecklistItem{{Title: "A"}},
		Guidance: func(context.Context, int) (string, error) { return "", sentinel },
	})
	if !errors.Is(err, sentinel) || result.Status != GoalErrored || len(result.Checklist) != 1 || p.calls != 0 {
		t.Fatalf("result %+v err %v calls %d", result, err, p.calls)
	}
}

func TestGoalGuidanceDoesNotRunPastGuardrails(t *testing.T) {
	for _, name := range []string{"canceled", "budget"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p := &recordingProvider{turns: [][]ProviderEvent{goalTurn(`{"items":[{"title":"A"}]}`, 10)}}
			spec := GoalSpec{Objective: "finish A", TokenBudget: 1, Guidance: func(context.Context, int) (string, error) { t.Fatal("guidance past guardrail"); return "", nil }}
			if name == "canceled" {
				cancel()
				spec.Checklist = []ChecklistItem{{Title: "A"}}
			}
			result, err := newGoalAgent(p).PursueGoal(ctx, spec)
			if name == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if name == "budget" && (err != nil || result.Status != GoalBudgetLimited) {
				t.Fatalf("result %+v err %v", result, err)
			}
		})
	}
}

func TestGoalGuidanceEmptyLeavesPromptUnchanged(t *testing.T) {
	for _, text := range []string{"", " \n\t", "  Please verify examples.  "} {
		t.Run(text, func(t *testing.T) {
			const prompt = "original prompt"
			want := prompt
			if strings.TrimSpace(text) != "" {
				want = "Operator guidance\nPlease verify examples.\n\n" + prompt
			}
			if got := withGoalGuidance(prompt, text); got != want {
				t.Fatalf("prompt %q want %q", got, want)
			}
		})
	}
}
