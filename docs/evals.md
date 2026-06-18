# Evals: scoring agents and gating CI

The `evals` package runs a glue agent against a suite of cases, scores the
results, and reports a pass rate you can gate CI on. Use it to prove a
prompt/model/tool change actually improved behavior — and to catch
regressions before they ship.

See [ADR-0019](adr/0019-evals-harness.md) for the design.

## Concepts

- **Case** — a prompt plus per-case options and a list of scorers.
- **Scorer** — grades a result on a 0..1 scale.
- **Suite** — named cases with a shared pass threshold (default 0.5).
- **Runner** — runs each case in its own session against a `*glue.Agent`.
- **Report** — per-case + aggregate results; `Gate(minPassRate)` returns
  an error (→ non-zero exit) when the pass rate is too low.

A case **passes** when it ran without error and **every** scorer meets the
threshold.

### Built-in scorers

| Scorer | Passes when |
| --- | --- |
| `Contains(subs…)` | response contains every substring (case-insensitive) |
| `NotContains(subs…)` | response contains none of the substrings |
| `Regex(pattern)` | response matches the pattern |
| `Equals(text)` | response equals the text (whitespace-trimmed) |
| `Command(dir, name, args…)` | the command exits 0 — the coding gate |
| `Judge{Agent, Rubric}` | an LLM grades the response ≥ threshold |
| `ScorerFunc(name, fn)` | your custom function returns a passing score |

## Running a suite from the CLI

Write a suite as JSON (see [`examples/evals/smoke.json`](../examples/evals/smoke.json)):

```json
{
  "name": "smoke",
  "threshold": 0.5,
  "cases": [
    { "name": "greets", "prompt": "Greet the user.",
      "scorers": [{ "type": "regex", "pattern": "(?i)hello|hi" }] },
    { "name": "math", "prompt": "What is 17 + 25?",
      "scorers": [{ "type": "contains", "values": ["42"] }] },
    { "name": "quality", "prompt": "Explain recursion to a beginner.",
      "scorers": [{ "type": "judge", "rubric": "Mentions a base case and is correct." }] }
  ]
}
```

Run it and gate on the pass rate:

```sh
glue eval examples/evals/smoke.json --provider gemini --min-pass-rate 0.9
echo "exit: $?"   # non-zero if the gate fails
```

Useful flags: `--model` / `--judge-model`, `--coding` (give the agent the
local coding tools), `--work <dir>` (root for coding tools and `command`
scorers), `--parallel <n>`, `--json` (machine-readable report), `--env`.

> Eval runs are unattended and auto-approve side-effecting tools (like
> `glue goal --yolo`). Run `--coding` suites and `command` scorers on a
> feature branch, worktree, or sandbox.

### Coding gate example

Let the agent edit code, then score on whether the build/tests pass:

```json
{ "name": "fixes-build", "prompt": "Make `go build ./...` pass.",
  "scorers": [{ "type": "command", "command": "go", "args": ["build", "./..."], "dir": "." }] }
```

## Running a suite from Go

```go
suite := evals.Suite{
    Name: "smoke",
    Cases: []evals.Case{
        {Name: "greets", Prompt: "Greet the user.",
            Scorers: []evals.Scorer{evals.Regex("(?i)hello|hi")}},
        {Name: "math", Prompt: "What is 17 + 25?",
            Scorers: []evals.Scorer{evals.Contains("42")}},
    },
}

report, err := evals.Runner{Agent: agent, Parallelism: 4}.Run(ctx, suite)
if err != nil {
    log.Fatal(err)
}
report.WriteText(os.Stdout)
if err := report.Gate(1.0); err != nil { // require every case to pass
    log.Fatal(err)
}
```

This drops straight into a `go test` so suites run in the same CI as the
unit tests. For an LLM-as-judge scorer, give it a separate, tool-less
agent so it never grades its own work:

```go
judge := glue.NewAgent(glue.AgentOptions{Provider: provider, Model: "strong-model"})
scorer := evals.Judge{Agent: judge, Rubric: "Correct, concise, beginner-friendly."}
```
