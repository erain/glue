// Package evals is a Go-native eval harness for glue agents: define a
// suite of cases, run them against an agent, score the results, and gate
// CI on the aggregate pass rate.
//
// It borrows Eve's evals-as-gates idea but stays compiled and dependency
// free, matching how glue agents are built (ADR-0012): a [Suite] is
// ordinary Go, so cases, scorers, and thresholds live next to the agent
// they exercise and run under `go test` or a `glue eval` command.
//
// A minimal suite:
//
//	suite := evals.Suite{
//		Name: "smoke",
//		Cases: []evals.Case{{
//			Name:    "greets",
//			Prompt:  "Say hello to the user.",
//			Scorers: []evals.Scorer{evals.Contains("hello")},
//		}},
//	}
//	report, err := evals.Runner{Agent: agent}.Run(ctx, suite)
//	if err != nil {
//		log.Fatal(err)
//	}
//	report.WriteText(os.Stdout)
//	if err := report.Gate(1.0); err != nil { // require every case to pass
//		os.Exit(1)
//	}
//
// Scorers grade a [Result] on a 0..1 scale. The package ships
// deterministic scorers ([Contains], [NotContains], [Regex], [Equals],
// [Command], [ScorerFunc]) and an LLM-as-judge scorer ([Judge]). A case
// passes when it ran without error and every scorer meets the suite's
// pass threshold.
package evals
