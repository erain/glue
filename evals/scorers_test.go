package evals

import (
	"context"
	"testing"

	"github.com/erain/glue"
)

func score(t *testing.T, s Scorer, text string) Score {
	t.Helper()
	out, err := s.Score(context.Background(), Result{Response: glue.PromptResult{Text: text}})
	if err != nil {
		t.Fatalf("scorer %s errored: %v", s.Name(), err)
	}
	return out
}

func TestContainsScorer(t *testing.T) {
	s := Contains("Hello", "world")
	if v := score(t, s, "well, HELLO WORLD!").Value; v != 1 {
		t.Errorf("case-insensitive contains should pass, got %v", v)
	}
	if v := score(t, s, "hello there").Value; v != 0 {
		t.Errorf("missing substring should fail, got %v", v)
	}
}

func TestNotContainsScorer(t *testing.T) {
	s := NotContains("secret")
	if v := score(t, s, "all clear").Value; v != 1 {
		t.Errorf("clean text should pass, got %v", v)
	}
	if v := score(t, s, "the SECRET is out").Value; v != 0 {
		t.Errorf("banned text should fail, got %v", v)
	}
}

func TestEqualsScorer(t *testing.T) {
	s := Equals("  done  ")
	if v := score(t, s, "done").Value; v != 1 {
		t.Errorf("trimmed equality should pass, got %v", v)
	}
	if v := score(t, s, "not done").Value; v != 0 {
		t.Errorf("inequality should fail, got %v", v)
	}
}

func TestRegexScorer(t *testing.T) {
	if v := score(t, Regex(`\d{3}-\d{4}`), "call 555-1234 now").Value; v != 1 {
		t.Errorf("regex should match, got %v", v)
	}
	if _, err := Regex("(").Score(context.Background(), Result{}); err == nil {
		t.Error("invalid regex should error")
	}
}

func TestCommandScorer(t *testing.T) {
	pass, err := Command("", "true").Score(context.Background(), Result{})
	if err != nil {
		t.Fatalf("command scorer errored: %v", err)
	}
	if pass.Value != 1 {
		t.Errorf("`true` should score 1, got %v", pass.Value)
	}

	fail, err := Command("", "false").Score(context.Background(), Result{})
	if err != nil {
		t.Fatalf("command scorer errored: %v", err)
	}
	if fail.Value != 0 {
		t.Errorf("`false` should score 0, got %v", fail.Value)
	}
}
