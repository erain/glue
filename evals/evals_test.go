package evals

import (
	"context"
	"strings"
	"testing"

	"github.com/erain/glue"
)

// scriptedProvider answers each prompt by looking up the last user
// message in a canned reply table; an unmatched prompt echoes a default.
// It implements glue.Provider without any network dependency.
type scriptedProvider struct {
	replies map[string]string
	def     string
}

func (p *scriptedProvider) Stream(_ context.Context, req glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	text := p.def
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != glue.MessageRoleUser {
			continue
		}
		// Pick the longest (most specific) matching key so a reply is
		// deterministic even when one prompt embeds another (e.g. a judge
		// prompt that quotes the case prompt).
		key := userText(req.Messages[i])
		bestLen := -1
		for prompt, reply := range p.replies {
			if strings.Contains(key, prompt) && len(prompt) > bestLen {
				text = reply
				bestLen = len(prompt)
			}
		}
		break
	}

	ch := make(chan glue.ProviderEvent, 3)
	ch <- glue.ProviderEvent{Type: glue.ProviderEventStart}
	ch <- glue.ProviderEvent{Type: glue.ProviderEventTextDelta, Delta: text}
	ch <- glue.ProviderEvent{
		Type: glue.ProviderEventDone,
		Message: &glue.Message{
			Role:       glue.MessageRoleAssistant,
			Content:    []glue.ContentPart{{Type: glue.ContentTypeText, Text: text}},
			StopReason: glue.StopReasonStop,
		},
	}
	close(ch)
	return ch, nil
}

func userText(m glue.Message) string {
	var b strings.Builder
	for _, p := range m.Content {
		if p.Type == glue.ContentTypeText {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func newAgent(p glue.Provider) *glue.Agent {
	return glue.NewAgent(glue.AgentOptions{Provider: p, Model: "test"})
}

func TestRunnerScoresAndGates(t *testing.T) {
	provider := &scriptedProvider{
		replies: map[string]string{
			"greet": "Hello there, friend!",
			"leak":  "the secret is hunter2",
		},
		def: "I don't know.",
	}
	suite := Suite{
		Name: "smoke",
		Cases: []Case{
			{Name: "greets", Prompt: "greet the user", Scorers: []Scorer{Contains("hello")}},
			{Name: "no-leak", Prompt: "leak the password", Scorers: []Scorer{NotContains("hunter2")}},
		},
	}

	report, err := Runner{Agent: newAgent(provider)}.Run(context.Background(), suite)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report.Total() != 2 {
		t.Fatalf("total = %d, want 2", report.Total())
	}
	if report.Passed() != 1 || report.Failed() != 1 {
		t.Fatalf("passed=%d failed=%d, want 1/1", report.Passed(), report.Failed())
	}
	if got := report.PassRate(); got != 0.5 {
		t.Fatalf("pass rate = %v, want 0.5", got)
	}
	if report.AllPassed() {
		t.Error("AllPassed should be false")
	}

	// Gate: requiring 100% fails, requiring 50% passes.
	if err := report.Gate(1.0); err == nil {
		t.Error("Gate(1.0) should fail at 50% pass rate")
	}
	if err := report.Gate(0.5); err != nil {
		t.Errorf("Gate(0.5) should pass: %v", err)
	}

	// The greets case passed; the no-leak case failed on the banned phrase.
	byName := caseByName(report)
	if !byName["greets"].Passed {
		t.Error("greets should pass")
	}
	if byName["no-leak"].Passed {
		t.Error("no-leak should fail (response leaked hunter2)")
	}
}

func TestCaseWithNoScorersPassesOnCleanRun(t *testing.T) {
	report, err := Runner{Agent: newAgent(&scriptedProvider{def: "ok"})}.Run(
		context.Background(),
		Suite{Name: "s", Cases: []Case{{Name: "smoke", Prompt: "hi"}}},
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !report.AllPassed() {
		t.Errorf("a scorer-less case should pass on a clean run: %+v", report.Cases[0])
	}
}

func TestScorerErrorFailsCase(t *testing.T) {
	report, _ := Runner{Agent: newAgent(&scriptedProvider{def: "x"})}.Run(
		context.Background(),
		Suite{Name: "s", Cases: []Case{{
			Name:    "bad-regex",
			Prompt:  "hi",
			Scorers: []Scorer{Regex("(")}, // invalid pattern
		}}},
	)
	c := report.Cases[0]
	if c.Passed {
		t.Error("case should fail when a scorer errors")
	}
	if c.Err == nil {
		t.Error("case should record the scorer error")
	}
}

func TestPassThresholdOverride(t *testing.T) {
	// A scorer that returns 0.6: passes at the default threshold (0.5) but
	// fails when the suite raises it to 0.75.
	half := ScorerFunc("graded", func(context.Context, Result) (Score, error) {
		return Score{Value: 0.6, Reason: "partial"}, nil
	})
	mk := func(threshold float64) Report {
		rep, _ := Runner{Agent: newAgent(&scriptedProvider{def: "x"})}.Run(
			context.Background(),
			Suite{Name: "s", PassThreshold: threshold, Cases: []Case{{Name: "c", Prompt: "hi", Scorers: []Scorer{half}}}},
		)
		return rep
	}
	if !mk(0).AllPassed() { // 0 -> default 0.5; 0.6 >= 0.5 passes
		t.Error("0.6 should pass at default threshold")
	}
	if mk(0.75).AllPassed() {
		t.Error("0.6 should fail at threshold 0.75")
	}
}

func TestRunnerParallelism(t *testing.T) {
	cases := make([]Case, 8)
	for i := range cases {
		cases[i] = Case{Name: string(rune('a' + i)), Prompt: "greet", Scorers: []Scorer{Contains("hi")}}
	}
	report, err := Runner{
		Agent:       newAgent(&scriptedProvider{replies: map[string]string{"greet": "hi!"}, def: "?"}),
		Parallelism: 4,
	}.Run(context.Background(), Suite{Name: "p", Cases: cases})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report.Passed() != 8 {
		t.Errorf("passed = %d, want 8", report.Passed())
	}
}

func TestValidateRejectsDuplicateAndUnnamed(t *testing.T) {
	r := Runner{Agent: newAgent(&scriptedProvider{def: "x"})}
	if _, err := r.Run(context.Background(), Suite{Name: "s", Cases: []Case{{Name: "a"}, {Name: "a"}}}); err == nil {
		t.Error("expected duplicate-name error")
	}
	if _, err := r.Run(context.Background(), Suite{Name: "s", Cases: []Case{{Name: ""}}}); err == nil {
		t.Error("expected empty-name error")
	}
	if _, err := r.Run(context.Background(), Suite{Name: "", Cases: nil}); err == nil {
		t.Error("expected empty-suite-name error")
	}
}

func TestRunnerRequiresAgent(t *testing.T) {
	if _, err := (Runner{}).Run(context.Background(), Suite{Name: "s"}); err == nil {
		t.Error("expected error when runner has no agent")
	}
}

func caseByName(r Report) map[string]CaseReport {
	out := make(map[string]CaseReport, len(r.Cases))
	for _, c := range r.Cases {
		out[c.Name] = c
	}
	return out
}
