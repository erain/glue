package evals

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/erain/glue"
)

// SuiteSpec is the JSON-friendly, declarative form of a [Suite]. It lets a
// suite live in a data file (run by `glue eval`) instead of Go, at the
// cost of only the built-in scorer types.
//
// Example:
//
//	{
//	  "name": "smoke",
//	  "threshold": 0.5,
//	  "cases": [
//	    {
//	      "name": "greets",
//	      "prompt": "Say hello to the user.",
//	      "scorers": [{"type": "contains", "values": ["hello"]}]
//	    }
//	  ]
//	}
type SuiteSpec struct {
	Name      string     `json:"name"`
	Threshold float64    `json:"threshold,omitempty"`
	Cases     []CaseSpec `json:"cases"`
}

// CaseSpec is the declarative form of a [Case].
type CaseSpec struct {
	Name    string       `json:"name"`
	Prompt  string       `json:"prompt"`
	Model   string       `json:"model,omitempty"`
	Scorers []ScorerSpec `json:"scorers,omitempty"`
}

// ScorerSpec is the declarative form of a [Scorer]. Type selects which
// fields are read:
//
//	contains / not_contains  → values
//	regex                    → pattern
//	equals                   → value
//	command                  → command, args, dir
//	judge                    → rubric, model
type ScorerSpec struct {
	Type    string   `json:"type"`
	Values  []string `json:"values,omitempty"`
	Pattern string   `json:"pattern,omitempty"`
	Value   string   `json:"value,omitempty"`
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	Dir     string   `json:"dir,omitempty"`
	Rubric  string   `json:"rubric,omitempty"`
	Model   string   `json:"model,omitempty"`
}

// BuildOptions supplies dependencies a declarative suite cannot carry
// itself.
type BuildOptions struct {
	// JudgeAgent grades "judge" scorers. Required only when the suite
	// uses them; building fails otherwise.
	JudgeAgent *glue.Agent
}

// ParseSuite decodes a [SuiteSpec] from JSON and builds a runnable
// [Suite]. It rejects unknown scorer types so a typo in a data file is a
// loud error rather than a silently-skipped check.
func ParseSuite(data []byte, opts BuildOptions) (Suite, error) {
	var spec SuiteSpec
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return Suite{}, fmt.Errorf("evals: decode suite: %w", err)
	}
	return spec.Build(opts)
}

// Build converts a parsed spec into a runnable [Suite].
func (spec SuiteSpec) Build(opts BuildOptions) (Suite, error) {
	suite := Suite{Name: spec.Name, PassThreshold: spec.Threshold}
	for i, cs := range spec.Cases {
		c := Case{Name: cs.Name, Prompt: cs.Prompt}
		if cs.Model != "" {
			c.Options = append(c.Options, glue.WithModel(cs.Model))
		}
		for j, ss := range cs.Scorers {
			scorer, err := ss.build(opts)
			if err != nil {
				return Suite{}, fmt.Errorf("case %d (%q) scorer %d: %w", i, cs.Name, j, err)
			}
			c.Scorers = append(c.Scorers, scorer)
		}
		suite.Cases = append(suite.Cases, c)
	}
	if err := suite.validate(); err != nil {
		return Suite{}, err
	}
	return suite, nil
}

func (ss ScorerSpec) build(opts BuildOptions) (Scorer, error) {
	switch ss.Type {
	case "contains":
		return Contains(ss.Values...), nil
	case "not_contains":
		return NotContains(ss.Values...), nil
	case "regex":
		return Regex(ss.Pattern), nil
	case "equals":
		return Equals(ss.Value), nil
	case "command":
		if ss.Command == "" {
			return nil, fmt.Errorf("command scorer needs a command")
		}
		return Command(ss.Dir, ss.Command, ss.Args...), nil
	case "judge":
		if opts.JudgeAgent == nil {
			return nil, fmt.Errorf("judge scorer needs a judge agent (none configured)")
		}
		if ss.Rubric == "" {
			return nil, fmt.Errorf("judge scorer needs a rubric")
		}
		return Judge{Agent: opts.JudgeAgent, Rubric: ss.Rubric, Model: ss.Model}, nil
	case "":
		return nil, fmt.Errorf("scorer has no type")
	default:
		return nil, fmt.Errorf("unknown scorer type %q", ss.Type)
	}
}
