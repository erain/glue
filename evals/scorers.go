package evals

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// scorerFunc adapts a plain function to the [Scorer] interface.
type scorerFunc struct {
	name string
	fn   func(ctx context.Context, r Result) (Score, error)
}

func (s scorerFunc) Name() string { return s.name }
func (s scorerFunc) Score(ctx context.Context, r Result) (Score, error) {
	return s.fn(ctx, r)
}

// ScorerFunc builds a [Scorer] from a name and grading function. Use it
// for one-off, suite-specific checks without declaring a type.
func ScorerFunc(name string, fn func(ctx context.Context, r Result) (Score, error)) Scorer {
	return scorerFunc{name: name, fn: fn}
}

// boolScore maps a pass/fail to a 1/0 Score with a reason.
func boolScore(scorer string, pass bool, passReason, failReason string) Score {
	if pass {
		return Score{Scorer: scorer, Value: 1, Reason: passReason}
	}
	return Score{Scorer: scorer, Value: 0, Reason: failReason}
}

// Contains passes (1) when the response text contains every substring
// (case-insensitive), else fails (0). Zero substrings always passes.
func Contains(substrs ...string) Scorer {
	return ScorerFunc("contains", func(_ context.Context, r Result) (Score, error) {
		text := strings.ToLower(r.Text())
		for _, sub := range substrs {
			if !strings.Contains(text, strings.ToLower(sub)) {
				return boolScore("contains", false, "", fmt.Sprintf("missing %q", sub)), nil
			}
		}
		return boolScore("contains", true, "all substrings present", ""), nil
	})
}

// NotContains passes (1) when the response text contains none of the
// substrings (case-insensitive), else fails (0). Useful for banned
// phrases or leaked secrets.
func NotContains(substrs ...string) Scorer {
	return ScorerFunc("not_contains", func(_ context.Context, r Result) (Score, error) {
		text := strings.ToLower(r.Text())
		for _, sub := range substrs {
			if strings.Contains(text, strings.ToLower(sub)) {
				return boolScore("not_contains", false, "", fmt.Sprintf("found banned %q", sub)), nil
			}
		}
		return boolScore("not_contains", true, "no banned substrings", ""), nil
	})
}

// Regex passes (1) when the response text matches pattern. A bad pattern
// fails the case with a harness error.
func Regex(pattern string) Scorer {
	return ScorerFunc("regex", func(_ context.Context, r Result) (Score, error) {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return Score{}, fmt.Errorf("compile %q: %w", pattern, err)
		}
		ok := re.MatchString(r.Text())
		return boolScore("regex", ok, "matched "+pattern, "no match for "+pattern), nil
	})
}

// Equals passes (1) when the response text equals expected after trimming
// surrounding whitespace on both sides.
func Equals(expected string) Scorer {
	return ScorerFunc("equals", func(_ context.Context, r Result) (Score, error) {
		got := strings.TrimSpace(r.Text())
		want := strings.TrimSpace(expected)
		return boolScore("equals", got == want, "exact match", "text did not equal expected"), nil
	})
}

// CommandScorer runs a shell command and passes (1) on exit code 0. It is
// the coding-agent gate: after the agent edits files, run the build or
// tests and score on the outcome.
type CommandScorer struct {
	// Name overrides the scorer name in reports (default "command").
	Name_ string
	// Command is the program to run.
	Command string
	// Args are the program arguments.
	Args []string
	// Dir is the working directory; empty uses the current directory.
	Dir string
}

// Command builds a [CommandScorer] for `name args...` in dir.
func Command(dir, name string, args ...string) Scorer {
	return CommandScorer{Command: name, Args: args, Dir: dir}
}

// Name implements [Scorer].
func (c CommandScorer) Name() string {
	if c.Name_ != "" {
		return c.Name_
	}
	return "command"
}

// Score implements [Scorer]: it runs the command and grades on its exit
// status. The combined output is truncated into the reason for context.
func (c CommandScorer) Score(ctx context.Context, _ Result) (Score, error) {
	cmd := exec.CommandContext(ctx, c.Command, c.Args...)
	cmd.Dir = c.Dir
	out, err := cmd.CombinedOutput()
	label := strings.TrimSpace(c.Command + " " + strings.Join(c.Args, " "))
	if err != nil {
		return Score{
			Scorer: c.Name(),
			Value:  0,
			Reason: fmt.Sprintf("%s failed: %v%s", label, err, outputTail(out)),
		}, nil
	}
	return Score{Scorer: c.Name(), Value: 1, Reason: label + " ok"}, nil
}

// outputTail returns a short, single-line-prefixed tail of command output
// for inclusion in a score reason.
func outputTail(out []byte) string {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return ""
	}
	const max = 200
	if len(s) > max {
		s = "…" + s[len(s)-max:]
	}
	return "\n          " + strings.ReplaceAll(s, "\n", "\n          ")
}
