// The "glue eval" subcommand: run a declarative eval suite against an
// agent and gate (exit non-zero) on the aggregate pass rate. This is the
// CI-facing front end to the evals package.

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/erain/glue"
	"github.com/erain/glue/evals"
)

func evalCommand(ctx context.Context, args []string, stdout, stderr io.Writer, newProvider providerFactory) error {
	flags := flag.NewFlagSet("glue eval", flag.ContinueOnError)
	flags.SetOutput(stderr)

	provider := flags.String("provider", defaultProvider, "provider name: codex, gemini, nvidia, or openrouter")
	model := flags.String("model", "", "model id for the agent under test (default: provider default)")
	judgeModel := flags.String("judge-model", "", "model id for judge scorers (default: --model)")
	coding := flags.Bool("coding", false, "enable local coding tools for the agent under test")
	workDir := flags.String("work", ".", "working directory for coding tools and command scorers")
	storeDir := flags.String("store", ".glue/evals", "session store directory for eval runs")
	minPassRate := flags.Float64("min-pass-rate", 1.0, "minimum suite pass rate to exit zero (0..1)")
	parallel := flags.Int("parallel", 1, "max cases to run concurrently")
	jsonOut := flags.Bool("json", false, "emit the report as JSON instead of text")
	var allowedBinaries repeatedStrings
	flags.Var(&allowedBinaries, "allow-binary", "allowed shell_exec binary basename for --coding; repeatable")
	var envs envFiles
	flags.Var(&envs, "env", "env file path; repeatable")

	// The suite path is the first argument; flags follow it (Go's flag
	// parser stops at the first positional, so we split it off by hand).
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: glue eval <suite.json> [flags]")
	}
	suitePath := args[0]
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments after suite path: %s", strings.Join(flags.Args(), " "))
	}

	if err := loadEnvFiles(envs); err != nil {
		return err
	}
	providerName, effectiveModel, err := resolveProvider(*provider, *model)
	if err != nil {
		return err
	}

	// Agent under test: optional coding tools, auto-approving permission
	// (eval runs are unattended — run them on a feature branch / worktree).
	var tools []glue.Tool
	if *coding {
		tools, _, err = buildCodingTools(codingFlagConfig{
			Enabled:         true,
			WorkDir:         *workDir,
			AllowedBinaries: append([]string(nil), allowedBinaries...),
			AllowOverwrite:  true,
		})
		if err != nil {
			return err
		}
	}
	agent, err := newAgent(newProvider, agentConfig{
		Provider:   providerName,
		Model:      effectiveModel,
		StoreDir:   *storeDir,
		WorkDir:    *workDir,
		Tools:      tools,
		Permission: yoloPermission{},
		Coding:     *coding,
	})
	if err != nil {
		return err
	}

	// Judge agent: independent of the agent under test, no tools, so an
	// LLM-as-judge scorer never grades its own work.
	judge, err := newAgent(newProvider, agentConfig{
		Provider: providerName,
		Model:    judgeModelOr(*judgeModel, effectiveModel),
		StoreDir: *storeDir,
	})
	if err != nil {
		return err
	}

	data, err := os.ReadFile(suitePath)
	if err != nil {
		return fmt.Errorf("read suite: %w", err)
	}
	suite, err := evals.ParseSuite(data, evals.BuildOptions{JudgeAgent: judge})
	if err != nil {
		return err
	}

	report, err := evals.Runner{Agent: agent, Parallelism: *parallel}.Run(ctx, suite)
	if err != nil {
		return err
	}

	if *jsonOut {
		if err := writeJSONReport(stdout, report); err != nil {
			return err
		}
	} else {
		report.WriteText(stdout)
	}

	// Gate: a non-nil error here exits the process non-zero (CI gate).
	return report.Gate(*minPassRate)
}

func judgeModelOr(judge, fallback string) string {
	if strings.TrimSpace(judge) != "" {
		return judge
	}
	return fallback
}

// writeJSONReport emits a stable, machine-readable view of the report for
// CI consumers.
func writeJSONReport(w io.Writer, r evals.Report) error {
	type scoreJSON struct {
		Scorer string  `json:"scorer"`
		Value  float64 `json:"value"`
		Reason string  `json:"reason,omitempty"`
	}
	type caseJSON struct {
		Name   string      `json:"name"`
		Passed bool        `json:"passed"`
		Score  float64     `json:"score"`
		Error  string      `json:"error,omitempty"`
		Scores []scoreJSON `json:"scores,omitempty"`
	}
	out := struct {
		Suite     string     `json:"suite"`
		Threshold float64    `json:"threshold"`
		Passed    int        `json:"passed"`
		Total     int        `json:"total"`
		PassRate  float64    `json:"pass_rate"`
		Cases     []caseJSON `json:"cases"`
	}{
		Suite:     r.Suite,
		Threshold: r.PassThreshold,
		Passed:    r.Passed(),
		Total:     r.Total(),
		PassRate:  r.PassRate(),
	}
	for _, c := range r.Cases {
		cj := caseJSON{Name: c.Name, Passed: c.Passed, Score: c.Score}
		if c.Err != nil {
			cj.Error = c.Err.Error()
		}
		for _, s := range c.Scores {
			cj.Scores = append(cj.Scores, scoreJSON{Scorer: s.Scorer, Value: s.Value, Reason: s.Reason})
		}
		out.Cases = append(out.Cases, cj)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
