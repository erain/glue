package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/erain/glue"
	"github.com/erain/glue/cmd/glue/worktree"
	filestore "github.com/erain/glue/stores/file"
)

// Exit codes for `glue goal`, so cron/CI schedulers can branch on the
// outcome (retry a budget stop, alert on blocked, …).
const (
	goalExitAchieved      = 0
	goalExitErrored       = 1
	goalExitBlocked       = 2
	goalExitMaxIterations = 3
	goalExitBudgetLimited = 4
)

// goalCommand implements `glue goal`: the headless goal runner —
// the unit cron, CI, or a peggy schedule invokes for unattended goals.
func goalCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, newProvider providerFactory) (int, error) {
	flags := flag.NewFlagSet("glue goal", flag.ContinueOnError)
	flags.SetOutput(stderr)

	providerName := flags.String("provider", defaultProvider, "provider name: codex, gemini, nvidia, or openrouter")
	model := flags.String("model", "", "model id (default: the provider's default model); gemini/<model> accepted")
	storeDir := flags.String("store", ".glue/sessions", "session store directory (shared with glue run, so the TUI sees the same goals)")
	workDir := flags.String("work", ".", "working directory for AGENTS.md and coding tools")
	coding := flags.Bool("coding", false, "enable local coding tools for the goal")
	codingAllowOverwrite := flags.Bool("coding-allow-overwrite", false, "allow write_file to replace existing files after model and permission approval")
	yolo := flags.Bool("yolo", false, "auto-allow all side-effecting tool calls; required for unattended runs. always use on a feature branch or with --worktree.")
	maxIterations := flags.Int("max-iterations", 10, "outer-loop iteration cap for this run")
	budget := flags.Int64("budget", 0, "total token budget across planning, makers, and checkers (0 = unlimited)")
	useWorktree := flags.Bool("worktree", false, "isolate the goal in .glue/worktrees/<goal-id> on branch goal/<id> (needs --coding and a git repository)")
	list := flags.Bool("list", false, "list stored goal records and exit")
	resume := flags.Bool("resume", false, "resume the most recent unfinished goal, or the record id given as the argument")
	var envs envFiles
	flags.Var(&envs, "env", "env file path; repeatable")
	var allowedBinaries repeatedStrings
	flags.Var(&allowedBinaries, "allow-binary", "allowed shell_exec binary basename for --coding; repeatable")

	if err := flags.Parse(args); err != nil {
		return goalExitErrored, nil // flag package already printed the error
	}
	if err := loadEnvFiles(envs); err != nil {
		return goalExitErrored, err
	}

	store := filestore.New(*storeDir)

	if *list {
		agent := glue.NewAgent(glue.AgentOptions{Store: store})
		recs, err := agent.ListGoals(ctx, glue.ListGoalsOptions{Prefix: "goal-", Limit: 20})
		if err != nil {
			return goalExitErrored, err
		}
		if len(recs) == 0 {
			fmt.Fprintln(stdout, "no goals on the store yet")
			return goalExitAchieved, nil
		}
		for _, rec := range recs {
			marker := " "
			if rec.WorkDir != "" {
				marker = "⎇"
			}
			fmt.Fprintf(stdout, "%-14s %5s ✓ %s %-18s %s\n",
				rec.Status, plainFraction(rec.Checklist), marker, rec.ID, rec.Objective)
		}
		return goalExitAchieved, nil
	}

	resolvedProvider, effectiveModel, err := resolveProvider(*providerName, *model)
	if err != nil {
		return goalExitErrored, err
	}
	providerImpl, err := newProvider(resolvedProvider)
	if err != nil {
		return goalExitErrored, err
	}
	effectiveAllowOverwrite := *codingAllowOverwrite || *yolo
	buildToolsAt := func(dir string) ([]glue.Tool, error) {
		t, _, err := buildCodingTools(codingFlagConfig{
			Enabled:         *coding,
			WorkDir:         dir,
			AllowedBinaries: append([]string(nil), allowedBinaries...),
			AllowOverwrite:  effectiveAllowOverwrite,
		})
		return t, err
	}
	tools, err := buildToolsAt(*workDir)
	if err != nil {
		return goalExitErrored, err
	}
	var permission glue.Permission
	switch {
	case *yolo:
		permission = yoloPermission{}
		fmt.Fprintln(stderr, "glue goal: --yolo enabled; permission prompts are off (use on a feature branch or with --worktree).")
	case *coding:
		permission = newLocalPromptPermission(stdin, stderr)
	}
	agent := glue.NewAgent(glue.AgentOptions{
		Provider:   providerImpl,
		Model:      normalizeModel(effectiveModel),
		Tools:      tools,
		Store:      store,
		WorkDir:    *workDir,
		Permission: permission,
	})

	spec := glue.GoalSpec{
		MaxIterations: *maxIterations,
		TokenBudget:   *budget,
		Permission:    permission,
	}

	switch {
	case *resume:
		rec, err := pickResumeRecord(ctx, agent, flags.Arg(0))
		if err != nil {
			return goalExitErrored, err
		}
		spec.Objective = rec.Objective
		spec.SessionPrefix = rec.ID
		spec.Checklist = rec.Checklist
		spec.StartIteration = rec.Iterations + 1
		if rec.WorkDir != "" {
			if !*coding {
				return goalExitErrored, fmt.Errorf("goal %s ran isolated in a worktree; resuming it needs --coding", rec.ID)
			}
			dir, err := worktree.Ensure(*workDir, rec.ID)
			if err != nil {
				return goalExitErrored, err
			}
			wtTools, err := buildToolsAt(dir)
			if err != nil {
				return goalExitErrored, err
			}
			spec.Tools, spec.CheckerTools, spec.WorkDir = wtTools, wtTools, dir
		}
		fmt.Fprintf(stdout, "resuming %s (iteration %d): %s\n", rec.ID, spec.StartIteration, rec.Objective)
	default:
		objective := strings.TrimSpace(strings.Join(flags.Args(), " "))
		if objective == "" {
			return goalExitErrored, fmt.Errorf(`usage: glue goal "<objective>" [flags], glue goal --resume [id], or glue goal --list`)
		}
		spec.Objective = objective
		spec.SessionPrefix = "goal-" + newGoalID()
		if *useWorktree {
			if !*coding {
				return goalExitErrored, fmt.Errorf("--worktree needs --coding (the isolated goal would have no tools)")
			}
			dir, err := worktree.Ensure(*workDir, spec.SessionPrefix)
			if err != nil {
				return goalExitErrored, err
			}
			wtTools, err := buildToolsAt(dir)
			if err != nil {
				return goalExitErrored, err
			}
			spec.Tools, spec.CheckerTools, spec.WorkDir = wtTools, wtTools, dir
			fmt.Fprintf(stdout, "isolated on branch %s (%s)\n", worktree.Branch(spec.SessionPrefix), dir)
		}
	}

	spec.Emit = func(ev glue.GoalEvent) {
		switch ev.Type {
		case glue.GoalEventPlan:
			fmt.Fprintf(stdout, "goal %s: %s\nplan (%d items):\n%s\n", spec.SessionPrefix, spec.Objective, len(ev.Checklist), plainChecklist(ev.Checklist))
		case glue.GoalEventIterationStart:
			fmt.Fprintf(stdout, "— iteration %d (%d tokens so far)\n", ev.Iteration, ev.Usage.TotalTokens)
		case glue.GoalEventVerdict:
			fmt.Fprintf(stdout, "  verdict: %s ✓ — %s\n", plainFraction(ev.Checklist), ev.Message)
		}
	}

	res, runErr := agent.PursueGoal(ctx, spec)
	fmt.Fprintf(stdout, "\n%s\n", plainChecklist(res.Checklist))
	fmt.Fprintf(stdout, "goal %s after %d iteration(s), %d tokens", res.Status, res.Iterations, res.Usage.TotalTokens)
	if res.Summary != "" {
		fmt.Fprintf(stdout, " — %s", res.Summary)
	}
	fmt.Fprintln(stdout)
	if spec.WorkDir != "" {
		fmt.Fprintf(stdout, "changes on branch %s (%s) — review and merge\n", worktree.Branch(spec.SessionPrefix), spec.WorkDir)
	}
	if runErr != nil {
		return goalExitErrored, runErr
	}
	switch res.Status {
	case glue.GoalAchieved:
		return goalExitAchieved, nil
	case glue.GoalBlocked:
		return goalExitBlocked, nil
	case glue.GoalMaxIterations:
		return goalExitMaxIterations, nil
	case glue.GoalBudgetLimited:
		return goalExitBudgetLimited, nil
	default:
		return goalExitErrored, nil
	}
}

// pickResumeRecord finds the record to continue: the given id, or the most
// recent unfinished one. A stale "running" record is resumable here — this
// is a fresh process, so its owner is gone.
func pickResumeRecord(ctx context.Context, agent *glue.Agent, id string) (glue.GoalRecord, error) {
	if id != "" {
		rec, ok, err := agent.LoadGoal(ctx, id)
		if err != nil {
			return glue.GoalRecord{}, err
		}
		if !ok {
			return glue.GoalRecord{}, fmt.Errorf("no goal record %q (try glue goal --list)", id)
		}
		if len(rec.Checklist) == 0 {
			return glue.GoalRecord{}, fmt.Errorf("goal %q has no checklist to resume from", id)
		}
		return rec, nil
	}
	recs, err := agent.ListGoals(ctx, glue.ListGoalsOptions{Prefix: "goal-", Limit: 50})
	if err != nil {
		return glue.GoalRecord{}, err
	}
	for _, rec := range recs {
		if (rec.Resumable() || rec.Status == glue.GoalRunning) && len(rec.Checklist) > 0 {
			return rec, nil
		}
	}
	return glue.GoalRecord{}, fmt.Errorf("no unfinished goal to resume (try glue goal --list)")
}

func plainChecklist(items []glue.ChecklistItem) string {
	if len(items) == 0 {
		return "  (no checklist)"
	}
	lines := make([]string, 0, len(items))
	for _, it := range items {
		box := "[ ]"
		if it.Done {
			box = "[x]"
		}
		line := "  " + box + " " + it.Title
		if it.Evidence != "" {
			line += " — " + it.Evidence
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func plainFraction(items []glue.ChecklistItem) string {
	done := 0
	for _, it := range items {
		if it.Done {
			done++
		}
	}
	return fmt.Sprintf("%d/%d", done, len(items))
}

// newGoalID returns a 12-char hex id, matching the TUI's goal id shape.
func newGoalID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
