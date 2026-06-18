package evals

import (
	"fmt"
	"io"
	"strings"
)

// CaseReport is the outcome of one [Case].
type CaseReport struct {
	// Name is the case name.
	Name string
	// Passed is true when the run succeeded and every scorer met the
	// suite threshold.
	Passed bool
	// Score is the mean scorer value in [0,1] (1 when there are no
	// scorers).
	Score float64
	// Scores holds each scorer's grade.
	Scores []Score
	// Err is set when the run or a scorer failed; the case did not pass.
	Err error
}

// Report aggregates a suite run.
type Report struct {
	// Suite is the suite name.
	Suite string
	// PassThreshold is the per-scorer pass cutoff that was applied.
	PassThreshold float64
	// Cases holds one entry per case, in suite order.
	Cases []CaseReport
}

// Total is the number of cases run.
func (r Report) Total() int { return len(r.Cases) }

// Passed counts cases that passed.
func (r Report) Passed() int {
	n := 0
	for _, c := range r.Cases {
		if c.Passed {
			n++
		}
	}
	return n
}

// Failed counts cases that did not pass.
func (r Report) Failed() int { return r.Total() - r.Passed() }

// PassRate is passed/total in [0,1]; an empty report rates 1.
func (r Report) PassRate() float64 {
	if len(r.Cases) == 0 {
		return 1
	}
	return float64(r.Passed()) / float64(len(r.Cases))
}

// AllPassed reports whether every case passed.
func (r Report) AllPassed() bool { return r.Failed() == 0 }

// Gate returns nil when the pass rate is at least minPassRate, otherwise a
// non-nil error describing the shortfall. CI callers exit non-zero on a
// non-nil result; pass 1.0 to require every case to pass.
func (r Report) Gate(minPassRate float64) error {
	if got := r.PassRate(); got < minPassRate {
		return fmt.Errorf("evals: suite %q gate failed: pass rate %.0f%% (%d/%d) below required %.0f%%",
			r.Suite, got*100, r.Passed(), r.Total(), minPassRate*100)
	}
	return nil
}

// WriteText writes a human-readable summary: one line per case plus a
// totals line.
func (r Report) WriteText(w io.Writer) {
	fmt.Fprintf(w, "eval suite %q (threshold %.2f)\n", r.Suite, r.PassThreshold)
	for _, c := range r.Cases {
		mark := "PASS"
		if !c.Passed {
			mark = "FAIL"
		}
		fmt.Fprintf(w, "  [%s] %s (score %.2f)\n", mark, c.Name, c.Score)
		if c.Err != nil {
			fmt.Fprintf(w, "        error: %v\n", c.Err)
		}
		for _, s := range c.Scores {
			smark := "ok"
			if !s.Passed(r.PassThreshold) {
				smark = "x"
			}
			line := fmt.Sprintf("        - %s: %.2f [%s]", s.Scorer, s.Value, smark)
			if s.Reason != "" {
				line += " — " + strings.TrimSpace(s.Reason)
			}
			fmt.Fprintln(w, line)
		}
	}
	fmt.Fprintf(w, "  %d/%d passed (%.0f%%)\n", r.Passed(), r.Total(), r.PassRate()*100)
}
