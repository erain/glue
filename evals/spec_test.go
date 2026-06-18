package evals

import (
	"context"
	"os"
	"testing"
)

// TestExampleSuiteParses keeps the documented example suite valid: it must
// always parse and build (with a judge agent available for its judge case).
func TestExampleSuiteParses(t *testing.T) {
	data, err := os.ReadFile("../examples/evals/smoke.json")
	if err != nil {
		t.Fatalf("read example suite: %v", err)
	}
	suite, err := ParseSuite(data, BuildOptions{JudgeAgent: newAgent(&scriptedProvider{def: "{}"})})
	if err != nil {
		t.Fatalf("example suite failed to build: %v", err)
	}
	if len(suite.Cases) == 0 {
		t.Error("example suite has no cases")
	}
}

func TestParseSuiteBuildsScorers(t *testing.T) {
	data := []byte(`{
		"name": "smoke",
		"threshold": 0.6,
		"cases": [
			{
				"name": "greets",
				"prompt": "greet the user",
				"scorers": [
					{"type": "contains", "values": ["hello"]},
					{"type": "not_contains", "values": ["error"]},
					{"type": "regex", "pattern": "\\w+"},
					{"type": "equals", "value": "Hello there!"}
				]
			}
		]
	}`)
	suite, err := ParseSuite(data, BuildOptions{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if suite.Name != "smoke" || suite.PassThreshold != 0.6 {
		t.Errorf("suite meta = %q/%v", suite.Name, suite.PassThreshold)
	}
	if len(suite.Cases) != 1 || len(suite.Cases[0].Scorers) != 4 {
		t.Fatalf("got %d cases / %d scorers", len(suite.Cases), len(suite.Cases[0].Scorers))
	}

	// Smoke-run the built suite against a scripted provider.
	report, err := Runner{Agent: newAgent(&scriptedProvider{
		replies: map[string]string{"greet the user": "Hello there!"},
	})}.Run(context.Background(), suite)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !report.AllPassed() {
		t.Errorf("built suite should pass: %+v", report.Cases[0])
	}
}

func TestParseSuiteRejectsUnknownScorer(t *testing.T) {
	_, err := ParseSuite([]byte(`{"name":"s","cases":[{"name":"c","prompt":"p","scorers":[{"type":"bogus"}]}]}`), BuildOptions{})
	if err == nil {
		t.Error("expected error for unknown scorer type")
	}
}

func TestParseSuiteRejectsUnknownFields(t *testing.T) {
	_, err := ParseSuite([]byte(`{"name":"s","cases":[],"bogus":true}`), BuildOptions{})
	if err == nil {
		t.Error("expected error for unknown top-level field")
	}
}

func TestParseSuiteJudgeNeedsAgent(t *testing.T) {
	data := []byte(`{"name":"s","cases":[{"name":"c","prompt":"p","scorers":[{"type":"judge","rubric":"be nice"}]}]}`)
	if _, err := ParseSuite(data, BuildOptions{}); err == nil {
		t.Error("judge scorer without a judge agent should fail to build")
	}
	if _, err := ParseSuite(data, BuildOptions{JudgeAgent: newAgent(&scriptedProvider{def: "{}"})}); err != nil {
		t.Errorf("judge scorer with an agent should build: %v", err)
	}
}

func TestParseSuiteCommandNeedsCommand(t *testing.T) {
	_, err := ParseSuite([]byte(`{"name":"s","cases":[{"name":"c","prompt":"p","scorers":[{"type":"command"}]}]}`), BuildOptions{})
	if err == nil {
		t.Error("command scorer without a command should fail to build")
	}
}
