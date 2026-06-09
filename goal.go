package glue

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// GoalSpec configures an autonomous goal loop driven by [Agent.PursueGoal].
//
// The loop is glue's take on "loop engineering" / the `/goal` command: a
// single persistent Objective drives plan → act → verify → repeat until the
// goal is verifiably met or a guardrail trips. The maker (which acts) and the
// checker (which audits) run in separate sessions so the writer does not grade
// its own homework, and each maker iteration runs in a fresh session seeded
// from the durable checklist (a Ralph-style loop) rather than an ever-growing
// transcript. See docs/adr/0016-goal-loop.md.
type GoalSpec struct {
	// Objective is the natural-language goal. Required.
	Objective string

	// SessionPrefix is the base for the generated maker/checker session ids.
	// Defaults to "goal".
	SessionPrefix string

	// Model overrides the maker model. Empty uses the agent default.
	Model string
	// CheckerModel overrides the verifier model. Empty defaults to Model
	// (i.e. the maker model, or the agent default when Model is empty).
	CheckerModel string

	// MaxIterations caps the outer loop. Defaults to 10.
	MaxIterations int
	// MaxTurns overrides the inner per-iteration turn budget for both maker
	// and checker. Zero uses the agent/loop default.
	MaxTurns int
	// TokenBudget caps total tokens across planning, makers, and checkers.
	// Zero means unlimited.
	TokenBudget int64
	// NoProgressLimit stops the loop (status GoalBlocked) after this many
	// consecutive iterations leave the set of unfinished items unchanged.
	// Defaults to 2.
	NoProgressLimit int

	// Checklist seeds the loop with an existing plan instead of asking the
	// model to decompose the objective; done flags and evidence are kept as
	// given. This is how a paused or restarted goal resumes from its last
	// verified state without re-planning.
	Checklist []ChecklistItem

	// Tools overrides the maker tool set. Empty uses the agent's tools.
	Tools []Tool
	// CheckerTools overrides the verifier tool set. Empty uses the agent's
	// tools; the checker is instructed to verify, not modify.
	CheckerTools []Tool

	// SystemPrompt overrides the maker system prompt. Empty uses the agent's.
	SystemPrompt string
	// CheckerSystemPrompt overrides the verifier system prompt. Empty uses a
	// built-in audit prompt.
	CheckerSystemPrompt string

	// Permission overrides the permission policy for side-effecting tools
	// in the planner, maker, and checker sessions. Nil uses the agent's
	// policy.
	Permission Permission

	// Emit, when set, receives progress events as the loop runs.
	Emit func(GoalEvent)
}

// GoalStatus is the terminal state of a goal loop.
type GoalStatus string

const (
	// GoalAchieved means the checker confirmed every deliverable.
	GoalAchieved GoalStatus = "achieved"
	// GoalBlocked means the loop stopped because no progress was made for
	// NoProgressLimit consecutive iterations.
	GoalBlocked GoalStatus = "blocked"
	// GoalBudgetLimited means the token budget was exhausted.
	GoalBudgetLimited GoalStatus = "budget_limited"
	// GoalMaxIterations means the iteration cap was reached unmet.
	GoalMaxIterations GoalStatus = "max_iterations"
	// GoalErrored means a maker/checker run failed or the context was
	// cancelled.
	GoalErrored GoalStatus = "errored"
)

// ChecklistItem is one verifiable deliverable derived from the objective.
type ChecklistItem struct {
	Title    string `json:"title"`
	Done     bool   `json:"done"`
	Evidence string `json:"evidence,omitempty"`
}

// GoalResult is the outcome of [Agent.PursueGoal].
type GoalResult struct {
	Status     GoalStatus
	Objective  string
	Checklist  []ChecklistItem
	Iterations int
	Usage      Usage // summed across planning, makers, and checkers
	Summary    string
}

// GoalEventType identifies a [GoalEvent].
type GoalEventType string

const (
	GoalEventPlan           GoalEventType = "plan"
	GoalEventIterationStart GoalEventType = "iteration_start"
	GoalEventMakerDone      GoalEventType = "maker_done"
	GoalEventVerdict        GoalEventType = "verdict"
	GoalEventDone           GoalEventType = "done"
	GoalEventBlocked        GoalEventType = "blocked"
	GoalEventBudget         GoalEventType = "budget_limited"
	GoalEventMaxIterations  GoalEventType = "max_iterations"
)

// GoalEvent is an observability event emitted during a goal loop.
type GoalEvent struct {
	Type      GoalEventType
	Iteration int
	Message   string
	Checklist []ChecklistItem
	Usage     Usage
}

const defaultCheckerSystemPrompt = `You are an independent verifier auditing whether a coding goal has been met.
You did not write this code and you must not trust claims that work is done.
For each checklist item, gather concrete evidence yourself — read the actual
files, run the build and tests, and inspect diffs — and never accept proxy
signals (a passing-looking summary, effort spent, or "should work") as proof.
Do not modify the project; your job is only to judge. Mark an item done only
when you have verified evidence, and report the overall goal done only when
every item is genuinely complete.`

// PursueGoal runs an autonomous goal loop until the objective is verifiably
// met or a guardrail trips. It returns the terminal [GoalResult]; a non-nil
// error is returned only for context cancellation or an underlying run failure
// (an unmet-but-clean stop such as GoalBlocked or GoalMaxIterations is not an
// error).
func (a *Agent) PursueGoal(ctx context.Context, spec GoalSpec) (GoalResult, error) {
	if a == nil {
		return GoalResult{}, errors.New("glue: nil agent")
	}
	if a.provider == nil {
		return GoalResult{}, errors.New("glue: agent provider is required")
	}
	spec = spec.withDefaults()
	if spec.Objective == "" {
		return GoalResult{}, errors.New("glue: goal objective is required")
	}

	result := GoalResult{Objective: spec.Objective}
	emit := func(ev GoalEvent) {
		if spec.Emit != nil {
			spec.Emit(ev)
		}
	}

	// 1) Plan: decompose the objective into verifiable deliverables — unless
	// the caller seeded a checklist (a resumed goal keeps its verified state).
	var checklist []ChecklistItem
	if len(spec.Checklist) > 0 {
		checklist = cloneChecklist(spec.Checklist)
	} else {
		planned, planUsage, err := a.planGoal(ctx, spec)
		addUsage(&result.Usage, planUsage)
		checklist = planned
		if err != nil || len(checklist) == 0 {
			// A failed plan is recoverable: treat the whole objective as one item.
			checklist = []ChecklistItem{{Title: spec.Objective}}
		}
	}
	emit(GoalEvent{Type: GoalEventPlan, Checklist: cloneChecklist(checklist), Usage: result.Usage})

	var prevRemaining string
	noProgress := 0

	for i := 1; i <= spec.MaxIterations; i++ {
		if err := ctx.Err(); err != nil {
			result.Status = GoalErrored
			result.Checklist = cloneChecklist(checklist)
			return result, err
		}
		if spec.TokenBudget > 0 && result.Usage.TotalTokens >= spec.TokenBudget {
			result.Status = GoalBudgetLimited
			result.Checklist = cloneChecklist(checklist)
			emit(GoalEvent{Type: GoalEventBudget, Iteration: result.Iterations, Checklist: cloneChecklist(checklist), Usage: result.Usage})
			return result, nil
		}
		result.Iterations = i
		emit(GoalEvent{Type: GoalEventIterationStart, Iteration: i, Checklist: cloneChecklist(checklist), Usage: result.Usage})

		// 2) Maker: a fresh session works the open items.
		makerUsage, err := a.runGoalMaker(ctx, spec, i, checklist)
		addUsage(&result.Usage, makerUsage)
		if err != nil {
			result.Status = GoalErrored
			result.Checklist = cloneChecklist(checklist)
			return result, fmt.Errorf("glue: goal maker iteration %d: %w", i, err)
		}
		emit(GoalEvent{Type: GoalEventMakerDone, Iteration: i, Usage: result.Usage})

		// 3) Checker: a separate session audits against real evidence.
		verdict, checkUsage, err := a.runGoalChecker(ctx, spec, i, checklist)
		addUsage(&result.Usage, checkUsage)
		if err != nil {
			result.Status = GoalErrored
			result.Checklist = cloneChecklist(checklist)
			return result, fmt.Errorf("glue: goal checker iteration %d: %w", i, err)
		}
		if len(verdict.Items) > 0 {
			checklist = verdict.Items
		}
		result.Summary = strings.TrimSpace(verdict.Summary)
		result.Checklist = cloneChecklist(checklist)
		emit(GoalEvent{Type: GoalEventVerdict, Iteration: i, Message: result.Summary, Checklist: cloneChecklist(checklist), Usage: result.Usage})

		// 4) Decide.
		if verdict.Done && allDone(checklist) {
			result.Status = GoalAchieved
			emit(GoalEvent{Type: GoalEventDone, Iteration: i, Message: result.Summary, Checklist: cloneChecklist(checklist), Usage: result.Usage})
			return result, nil
		}
		remaining := remainingKey(checklist)
		if i > 1 && remaining == prevRemaining {
			noProgress++
		} else {
			noProgress = 0
		}
		prevRemaining = remaining
		if noProgress >= spec.NoProgressLimit {
			result.Status = GoalBlocked
			emit(GoalEvent{Type: GoalEventBlocked, Iteration: i, Checklist: cloneChecklist(checklist), Usage: result.Usage})
			return result, nil
		}
	}

	result.Status = GoalMaxIterations
	result.Checklist = cloneChecklist(checklist)
	emit(GoalEvent{Type: GoalEventMaxIterations, Iteration: result.Iterations, Checklist: cloneChecklist(checklist), Usage: result.Usage})
	return result, nil
}

func (s GoalSpec) withDefaults() GoalSpec {
	s.Objective = strings.TrimSpace(s.Objective)
	if s.SessionPrefix == "" {
		s.SessionPrefix = "goal"
	}
	if s.MaxIterations <= 0 {
		s.MaxIterations = 10
	}
	if s.NoProgressLimit <= 0 {
		s.NoProgressLimit = 2
	}
	if s.CheckerModel == "" {
		s.CheckerModel = s.Model
	}
	if strings.TrimSpace(s.CheckerSystemPrompt) == "" {
		s.CheckerSystemPrompt = defaultCheckerSystemPrompt
	}
	return s
}

type goalPlan struct {
	Items []ChecklistItem `json:"items"`
}

type goalVerdict struct {
	Done    bool            `json:"done"`
	Items   []ChecklistItem `json:"items"`
	Summary string          `json:"summary"`
}

func (a *Agent) planGoal(ctx context.Context, spec GoalSpec) ([]ChecklistItem, Usage, error) {
	sess, err := a.Session(ctx, spec.SessionPrefix+":plan")
	if err != nil {
		return nil, Usage{}, err
	}
	opts := []PromptOption{WithJSONSchema(goalPlanSchema)}
	if spec.Model != "" {
		opts = append(opts, WithModel(spec.Model))
	}
	if spec.MaxTurns > 0 {
		opts = append(opts, WithMaxTurns(spec.MaxTurns))
	}
	if spec.Permission != nil {
		opts = append(opts, WithPermission(spec.Permission))
	}
	var plan goalPlan
	res, err := sess.PromptJSON(ctx, planPrompt(spec.Objective), &plan, opts...)
	var usage Usage
	sumUsage(&usage, res.NewMessages)
	if err != nil {
		return nil, usage, err
	}
	out := make([]ChecklistItem, 0, len(plan.Items))
	for _, item := range plan.Items {
		title := strings.TrimSpace(item.Title)
		if title == "" {
			continue
		}
		out = append(out, ChecklistItem{Title: title}) // always start not-done
	}
	return out, usage, nil
}

func (a *Agent) runGoalMaker(ctx context.Context, spec GoalSpec, iter int, checklist []ChecklistItem) (Usage, error) {
	sess, err := a.Session(ctx, fmt.Sprintf("%s:iter-%d", spec.SessionPrefix, iter))
	if err != nil {
		return Usage{}, err
	}
	var opts []PromptOption
	if spec.Model != "" {
		opts = append(opts, WithModel(spec.Model))
	}
	if spec.SystemPrompt != "" {
		opts = append(opts, WithSystemPrompt(spec.SystemPrompt))
	}
	if len(spec.Tools) > 0 {
		opts = append(opts, WithTools(spec.Tools))
	}
	if spec.MaxTurns > 0 {
		opts = append(opts, WithMaxTurns(spec.MaxTurns))
	}
	if spec.Permission != nil {
		opts = append(opts, WithPermission(spec.Permission))
	}
	res, err := sess.Prompt(ctx, makerPrompt(spec.Objective, iter, checklist), opts...)
	var usage Usage
	sumUsage(&usage, res.NewMessages)
	return usage, err
}

func (a *Agent) runGoalChecker(ctx context.Context, spec GoalSpec, iter int, checklist []ChecklistItem) (goalVerdict, Usage, error) {
	sess, err := a.Session(ctx, fmt.Sprintf("%s:check-%d", spec.SessionPrefix, iter))
	if err != nil {
		return goalVerdict{}, Usage{}, err
	}
	opts := []PromptOption{
		WithJSONSchema(goalVerdictSchema),
		WithSystemPrompt(spec.CheckerSystemPrompt),
	}
	if spec.CheckerModel != "" {
		opts = append(opts, WithModel(spec.CheckerModel))
	}
	if len(spec.CheckerTools) > 0 {
		opts = append(opts, WithTools(spec.CheckerTools))
	}
	if spec.MaxTurns > 0 {
		opts = append(opts, WithMaxTurns(spec.MaxTurns))
	}
	if spec.Permission != nil {
		opts = append(opts, WithPermission(spec.Permission))
	}
	var verdict goalVerdict
	res, err := sess.PromptJSON(ctx, checkerPrompt(spec.Objective, checklist), &verdict, opts...)
	var usage Usage
	sumUsage(&usage, res.NewMessages)
	return verdict, usage, err
}

// ---- prompts ----

func planPrompt(objective string) string {
	return fmt.Sprintf(`Decompose this goal into a checklist of concrete, independently verifiable deliverables.

Goal: %s

Each item must be checkable against real evidence (a file exists, a test passes, a command succeeds). Prefer 2–8 items; avoid vague items like "make it good". Return the items with empty/false done — nothing has been built yet.`, objective)
}

func makerPrompt(objective string, iter int, checklist []ChecklistItem) string {
	return fmt.Sprintf(`You are working autonomously toward a goal. This is iteration %d.

Goal: %s

Current checklist (verified state — trust only what is marked done):
%s

Read the actual code and tests to reconstruct context, then make real progress on the unfinished items. Run the build and tests to validate your work. Do not claim the goal is complete — a separate verifier decides that. Focus on moving at least one unfinished item to genuinely done.`, iter, objective, renderChecklist(checklist))
}

func checkerPrompt(objective string, checklist []ChecklistItem) string {
	return fmt.Sprintf(`Audit progress toward this goal and return a verdict.

Goal: %s

Checklist to verify:
%s

For every item, independently gather evidence (read files, run the build and tests, inspect diffs) and decide whether it is genuinely complete. Do not trust prior claims. Return every item with its verified done flag and a short evidence note, set "done" true only if all items are complete, and give a one-line summary of what remains.`, objective, renderChecklist(checklist))
}

func renderChecklist(items []ChecklistItem) string {
	if len(items) == 0 {
		return "  (empty)"
	}
	var b strings.Builder
	for i, item := range items {
		box := "[ ]"
		if item.Done {
			box = "[x]"
		}
		b.WriteString(fmt.Sprintf("  %s %s", box, item.Title))
		if item.Evidence != "" {
			b.WriteString(" — " + item.Evidence)
		}
		if i < len(items)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// ---- guardrail / usage helpers ----

func allDone(items []ChecklistItem) bool {
	if len(items) == 0 {
		return false
	}
	for _, item := range items {
		if !item.Done {
			return false
		}
	}
	return true
}

// remainingKey is a stable signature of the unfinished items, used for
// no-progress detection.
func remainingKey(items []ChecklistItem) string {
	var open []string
	for _, item := range items {
		if !item.Done {
			open = append(open, strings.TrimSpace(item.Title))
		}
	}
	sort.Strings(open)
	return strings.Join(open, "\x00")
}

func cloneChecklist(items []ChecklistItem) []ChecklistItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]ChecklistItem, len(items))
	copy(out, items)
	return out
}

func sumUsage(dst *Usage, msgs []Message) {
	for i := range msgs {
		u := msgs[i].Usage
		if u == nil {
			continue
		}
		dst.InputTokens += u.InputTokens
		dst.OutputTokens += u.OutputTokens
		dst.CacheReadTokens += u.CacheReadTokens
		dst.CacheWriteTokens += u.CacheWriteTokens
		dst.TotalTokens += u.TotalTokens
	}
}

func addUsage(dst *Usage, src Usage) {
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
	dst.CacheReadTokens += src.CacheReadTokens
	dst.CacheWriteTokens += src.CacheWriteTokens
	dst.TotalTokens += src.TotalTokens
}

var goalPlanSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"items": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":       "object",
				"properties": map[string]any{"title": map[string]any{"type": "string"}},
				"required":   []any{"title"},
			},
		},
	},
	"required": []any{"items"},
}

var goalVerdictSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"done": map[string]any{"type": "boolean"},
		"items": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"title":    map[string]any{"type": "string"},
					"done":     map[string]any{"type": "boolean"},
					"evidence": map[string]any{"type": "string"},
				},
				"required": []any{"title", "done"},
			},
		},
		"summary": map[string]any{"type": "string"},
	},
	"required": []any{"done", "items"},
}
