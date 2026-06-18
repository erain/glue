package evals

import (
	"context"
	"fmt"
	"sync"

	"github.com/erain/glue"
)

// DefaultPassThreshold is the minimum scorer value (0..1) counted as a
// pass when a [Suite] does not set its own PassThreshold. Built-in
// deterministic scorers report 1 or 0, so the midpoint cleanly separates
// pass from fail while still admitting graded [Judge] scores.
const DefaultPassThreshold = 0.5

// Case is one eval: a prompt run against an agent, graded by scorers.
type Case struct {
	// Name identifies the case in reports and session ids. Required and
	// unique within a suite.
	Name string

	// Prompt is the user message sent to the agent.
	Prompt string

	// Options are per-case prompt options (model, tools, permission,
	// system prompt, …). They let one suite exercise several
	// configurations without separate agents.
	Options []glue.PromptOption

	// Scorers grade the result. A case with no scorers passes as long as
	// the run itself succeeds (useful as a smoke test).
	Scorers []Scorer
}

// Score is one scorer's grade for a case.
type Score struct {
	// Scorer is the grading scorer's name.
	Scorer string
	// Value is the grade in [0,1]; it is clamped to that range.
	Value float64
	// Reason is a short human explanation, shown in reports.
	Reason string
}

// Passed reports whether this score meets threshold.
func (s Score) Passed(threshold float64) bool { return s.Value >= threshold }

// Result is what a [Case] produced, handed to each [Scorer].
type Result struct {
	// Case is the case that produced this result.
	Case Case
	// Response is the agent's prompt result (text + transcript).
	Response glue.PromptResult
	// Err is a non-nil run error (provider failure, cancellation). When
	// set, scorers are skipped and the case fails.
	Err error
}

// Text is the agent's final assistant text, the common scoring target.
func (r Result) Text() string { return r.Response.Text }

// Scorer grades a [Result] on a 0..1 scale.
type Scorer interface {
	// Name identifies the scorer in reports.
	Name() string
	// Score grades the result. Returning an error fails the case; use a
	// zero Value with a Reason instead when a low score is the intended
	// signal rather than a harness failure.
	Score(ctx context.Context, r Result) (Score, error)
}

// Suite is a named set of cases with a shared pass threshold.
type Suite struct {
	// Name labels the suite in reports and session ids.
	Name string
	// Cases are run in order (or concurrently; see [Runner.Parallelism]).
	Cases []Case
	// PassThreshold is the minimum scorer value counted as a pass. Zero
	// uses [DefaultPassThreshold].
	PassThreshold float64
}

func (s Suite) threshold() float64 {
	if s.PassThreshold <= 0 {
		return DefaultPassThreshold
	}
	return s.PassThreshold
}

// Runner executes a [Suite] against an agent.
type Runner struct {
	// Agent runs each case. Required. Prefer an in-memory agent (no
	// Store) or accept that each case persists under a distinct
	// "eval/<suite>/<case>" session id.
	Agent *glue.Agent

	// Parallelism caps concurrently-running cases. Zero or one runs
	// cases sequentially in suite order.
	Parallelism int
}

// Run executes every case and returns a [Report]. It returns an error
// only for a misconfigured runner; a case that errors at runtime is
// captured in its [CaseReport] (Passed=false) rather than aborting the
// suite.
func (r Runner) Run(ctx context.Context, suite Suite) (Report, error) {
	if r.Agent == nil {
		return Report{}, fmt.Errorf("evals: runner has no agent")
	}
	if err := suite.validate(); err != nil {
		return Report{}, err
	}

	threshold := suite.threshold()
	reports := make([]CaseReport, len(suite.Cases))

	parallelism := r.Parallelism
	if parallelism < 1 {
		parallelism = 1
	}
	sem := make(chan struct{}, parallelism)
	var wg sync.WaitGroup
	for i, c := range suite.Cases {
		if err := ctx.Err(); err != nil {
			// Context cancelled mid-suite: record the remaining cases as
			// errored so the report stays complete and the gate fails.
			reports[i] = CaseReport{Name: c.Name, Err: err}
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, c Case) {
			defer wg.Done()
			defer func() { <-sem }()
			reports[i] = r.runCase(ctx, suite, c, threshold)
		}(i, c)
	}
	wg.Wait()

	return Report{
		Suite:         suite.Name,
		PassThreshold: threshold,
		Cases:         reports,
	}, nil
}

func (r Runner) runCase(ctx context.Context, suite Suite, c Case, threshold float64) CaseReport {
	report := CaseReport{Name: c.Name}

	sess, err := r.Agent.Session(ctx, sessionID(suite.Name, c.Name))
	if err != nil {
		report.Err = fmt.Errorf("open session: %w", err)
		return report
	}
	resp, runErr := sess.Prompt(ctx, c.Prompt, c.Options...)
	result := Result{Case: c, Response: resp, Err: runErr}
	if runErr != nil {
		report.Err = runErr
		return report
	}

	report.Passed = true // a case with no scorers passes on a clean run
	for _, scorer := range c.Scorers {
		score, err := scorer.Score(ctx, result)
		if err != nil {
			report.Err = fmt.Errorf("scorer %q: %w", scorer.Name(), err)
			report.Passed = false
			return report
		}
		score.Scorer = scorer.Name()
		score.Value = clamp01(score.Value)
		report.Scores = append(report.Scores, score)
		if !score.Passed(threshold) {
			report.Passed = false
		}
	}
	report.Score = meanScore(report.Scores)
	return report
}

func (s Suite) validate() error {
	if s.Name == "" {
		return fmt.Errorf("evals: suite has no name")
	}
	seen := make(map[string]struct{}, len(s.Cases))
	for i, c := range s.Cases {
		if c.Name == "" {
			return fmt.Errorf("evals: case %d has no name", i)
		}
		if _, dup := seen[c.Name]; dup {
			return fmt.Errorf("evals: duplicate case name %q", c.Name)
		}
		seen[c.Name] = struct{}{}
	}
	return nil
}

func sessionID(suite, name string) string {
	return fmt.Sprintf("eval/%s/%s", suite, name)
}

func clamp01(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}

func meanScore(scores []Score) float64 {
	if len(scores) == 0 {
		return 1
	}
	var sum float64
	for _, s := range scores {
		sum += s.Value
	}
	return sum / float64(len(scores))
}
