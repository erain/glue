package glue

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/erain/glue/loop"
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
	// Guidance is sampled before each maker iteration, after guardrails, and
	// again before that iteration's checker, so the checker sees guidance that
	// arrived while the maker ran. Its text is prepended to the maker and
	// checker prompts. An error before the maker stops the loop and is returned
	// to the host; an error before the checker keeps the earlier text. Nil adds
	// no text.
	Guidance func(context.Context, int) (string, error)

	// OnSession runs before a planner, maker, or checker session is prompted.
	// Phases are "planner", "maker", and "checker". Planning uses iteration 0;
	// makers and checkers use the actual iteration, including StartIteration.
	// Observers own the unsubscribe functions returned by Session.Subscribe.
	OnSession func(phase string, iteration int, s *Session)

	// Objective is the natural-language goal. Required.
	Objective string
	// Context is background from the developer, such as the conversation the
	// goal came out of. Planner and maker prompts show it under its own
	// heading; it is not a list of extra requirements.
	Context string

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

	// StartIteration numbers this run's first iteration (default 1). A
	// resumed goal passes its prior Iterations+1 so maker/checker session
	// ids stay fresh (iter-4, check-4, …) instead of appending to the
	// previous run's transcripts. MaxIterations still counts iterations
	// in this run.
	StartIteration int

	// WorkDir records where this goal's tools are rooted (e.g. an isolated
	// git worktree). The loop itself never reads it — tools already carry
	// their root — but it is persisted on the GoalRecord so a host can
	// rebuild the right tool set when resuming.
	WorkDir string

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

	// RequirePersistence makes failed goal checkpoints stop execution and return
	// an error. A Store is required. The default keeps best-effort persistence.
	RequirePersistence bool
	// InitialUsage seeds cumulative usage when recovering an interrupted run.
	// Explicit continuation can leave it zero to begin a fresh run budget.
	InitialUsage Usage

	// Emit, when set, receives progress events as the loop runs.
	Emit func(GoalEvent)
}

// GoalStatus is the terminal state of a goal loop.
type GoalStatus string

const (
	// GoalRunning is never returned by PursueGoal; it appears only on the
	// durable [GoalRecord] while the loop is in flight.
	GoalRunning GoalStatus = "running"
	// GoalPaused is never returned by PursueGoal; it is the durable record
	// of a context cancellation — the loop stopped cleanly mid-objective
	// and can resume from its checklist.
	GoalPaused GoalStatus = "paused"
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

	result := GoalResult{Objective: spec.Objective, Iterations: spec.StartIteration - 1, Usage: spec.InitialUsage, Checklist: cloneChecklist(spec.Checklist)}
	emit := func(ev GoalEvent) {
		if spec.Emit != nil {
			spec.Emit(ev)
		}
	}

	// checkpoint is best-effort unless the host requires durable execution.
	// Terminal checkpoints outlive caller cancellation.
	var checklist = cloneChecklist(spec.Checklist)
	checkpoint := func(status GoalStatus) error {
		err := a.SaveGoal(context.WithoutCancel(ctx), GoalRecord{
			ID: spec.SessionPrefix, Objective: spec.Objective, Status: status,
			Checklist: cloneChecklist(checklist), Iterations: result.Iterations,
			Usage: result.Usage, Summary: result.Summary, WorkDir: spec.WorkDir,
		})
		if err != nil && spec.RequirePersistence {
			result.Status = GoalErrored
			return fmt.Errorf("glue: checkpoint goal %s: %w", spec.SessionPrefix, err)
		}
		return nil
	}
	if err := checkpoint(GoalRunning); err != nil {
		return result, err
	}
	// pauseOrErrored maps a run failure to its durable status: a cancelled
	// context is a clean pause (resumable), anything else an error.
	pauseOrErrored := func() GoalStatus {
		if ctx.Err() != nil {
			return GoalPaused
		}
		return GoalErrored
	}

	// 1) Plan: decompose the objective into verifiable deliverables — unless
	// the caller seeded a checklist (a resumed goal keeps its verified state).
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
	if err := checkpoint(GoalRunning); err != nil {
		return result, err
	}

	var prevRemaining string
	noProgress := 0

	for n := 0; n < spec.MaxIterations; n++ {
		i := spec.StartIteration + n
		if err := ctx.Err(); err != nil {
			result.Status = GoalErrored
			result.Checklist = cloneChecklist(checklist)
			return result, errors.Join(err, checkpoint(GoalPaused))
		}
		if spec.TokenBudget > 0 && result.Usage.TotalTokens >= spec.TokenBudget {
			result.Status = GoalBudgetLimited
			result.Checklist = cloneChecklist(checklist)
			emit(GoalEvent{Type: GoalEventBudget, Iteration: result.Iterations, Checklist: cloneChecklist(checklist), Usage: result.Usage})
			if err := checkpoint(GoalBudgetLimited); err != nil {
				return result, err
			}
			return result, nil
		}
		var guidance string
		if spec.Guidance != nil {
			var err error
			guidance, err = spec.Guidance(ctx, i)
			if err != nil {
				result.Status = GoalErrored
				result.Checklist = cloneChecklist(checklist)
				return result, errors.Join(fmt.Errorf("glue: goal guidance iteration %d: %w", i, err), checkpoint(pauseOrErrored()))
			}
		}
		result.Iterations = i
		emit(GoalEvent{Type: GoalEventIterationStart, Iteration: i, Checklist: cloneChecklist(checklist), Usage: result.Usage})

		// 2) Maker: a fresh session works the open items.
		makerUsage, err := a.runGoalMaker(ctx, spec, i, checklist, guidance)
		addUsage(&result.Usage, makerUsage)
		if err != nil && ctx.Err() == nil && isLoopBudgetStop(err) {
			// The maker ran out of turns or a guardrail stopped it. Its edits
			// are on disk, so let the checker judge them instead of erroring.
			err = nil
		}
		if err != nil {
			result.Status = GoalErrored
			result.Checklist = cloneChecklist(checklist)
			return result, errors.Join(fmt.Errorf("glue: goal maker iteration %d: %w", i, err), checkpoint(pauseOrErrored()))
		}
		emit(GoalEvent{Type: GoalEventMakerDone, Iteration: i, Usage: result.Usage})

		// 3) Checker: a separate session audits against real evidence. Guidance
		// is read again so the checker sees input that arrived during the maker.
		if spec.Guidance != nil {
			if g, err := spec.Guidance(ctx, i); err == nil {
				guidance = g
			}
		}
		verdict, checkUsage, err := a.runGoalChecker(ctx, spec, i, checklist, guidance)
		addUsage(&result.Usage, checkUsage)
		if err != nil {
			result.Status = GoalErrored
			result.Checklist = cloneChecklist(checklist)
			return result, errors.Join(fmt.Errorf("glue: goal checker iteration %d: %w", i, err), checkpoint(pauseOrErrored()))
		}
		if len(verdict.Items) > 0 {
			checklist = verdict.Items
		}
		result.Summary = strings.TrimSpace(verdict.Summary)
		result.Checklist = cloneChecklist(checklist)
		emit(GoalEvent{Type: GoalEventVerdict, Iteration: i, Message: result.Summary, Checklist: cloneChecklist(checklist), Usage: result.Usage})
		if err := checkpoint(GoalRunning); err != nil {
			return result, err
		}

		// 4) Decide.
		if verdict.Done && allDone(checklist) {
			result.Status = GoalAchieved
			if err := checkpoint(GoalAchieved); err != nil {
				return result, err
			}
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
			if err := checkpoint(GoalBlocked); err != nil {
				return result, err
			}
			return result, nil
		}
	}

	result.Status = GoalMaxIterations
	result.Checklist = cloneChecklist(checklist)
	emit(GoalEvent{Type: GoalEventMaxIterations, Iteration: result.Iterations, Checklist: cloneChecklist(checklist), Usage: result.Usage})
	if err := checkpoint(GoalMaxIterations); err != nil {
		return result, err
	}
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
	if s.StartIteration <= 0 {
		s.StartIteration = 1
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
	if spec.OnSession != nil {
		spec.OnSession("planner", 0, sess)
	}
	opts := []PromptOption{WithJSONSchema(goalPlanSchema)}
	if spec.Model != "" {
		opts = append(opts, WithModel(spec.Model))
	}
	if spec.Permission != nil {
		opts = append(opts, WithPermission(spec.Permission))
	}
	opts = append(opts, WithMaxTurns(plannerMaxTurns(spec.MaxTurns)))
	var plan goalPlan
	res, err := sess.PromptJSON(ctx, withGoalContext(planPrompt(spec.Objective), spec.Context), &plan, opts...)
	var usage Usage
	sumUsage(&usage, res.NewMessages)
	if err != nil && ctx.Err() == nil && isLoopBudgetStop(err) {
		// The planner explored without answering. Ask once more in a fresh
		// session with no tools, handing over what it observed.
		plan = goalPlan{}
		var final PromptResult
		final, err = a.finalPlan(ctx, spec, plannerObservations(res.NewMessages), &plan)
		sumUsage(&usage, final.NewMessages)
	}
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

// PlannerMaxToolTurns bounds how many tool-using turns the goal planner may
// take before it must answer with a plan.
const PlannerMaxToolTurns = 6

// plannerMaxTurns returns the loop turn budget for the planner: the tool
// turns plus one turn for the answer, and never more than the spec allows.
func plannerMaxTurns(specMax int) int {
	n := PlannerMaxToolTurns + 1
	if specMax > 0 && specMax < n {
		n = specMax
	}
	return n
}

// finalPlan asks for the plan in a fresh session without tools.
func (a *Agent) finalPlan(ctx context.Context, spec GoalSpec, observations string, plan *goalPlan) (PromptResult, error) {
	sess, err := a.Session(ctx, spec.SessionPrefix+":plan-final")
	if err != nil {
		return PromptResult{}, err
	}
	if spec.OnSession != nil {
		spec.OnSession("planner", 0, sess)
	}
	opts := []PromptOption{WithJSONSchema(goalPlanSchema), WithTools(nil), WithMaxTurns(1)}
	if spec.Model != "" {
		opts = append(opts, WithModel(spec.Model))
	}
	text := withGoalContext(planPrompt(spec.Objective), spec.Context) + "\n\nYou have no tools now. Plan from the objective and what was already observed in the workspace:\n" + observations
	return sess.PromptJSON(ctx, text, plan, opts...)
}

// plannerObservations summarizes the tool results a planner run produced,
// newest last, within a fixed size.
func plannerObservations(msgs []Message) string {
	const maxLen = 6000
	var parts []string
	for _, m := range msgs {
		if m.Role != MessageRoleTool {
			continue
		}
		var b strings.Builder
		for _, c := range m.Content {
			if c.Type == ContentTypeText {
				b.WriteString(c.Text)
			}
		}
		text := strings.TrimSpace(b.String())
		if len(text) > 800 {
			text = text[:800] + " …"
		}
		parts = append(parts, fmt.Sprintf("- %s: %s", cmp.Or(m.ToolName, "tool"), text))
	}
	out := strings.Join(parts, "\n")
	for len(out) > maxLen && len(parts) > 1 {
		parts = parts[1:]
		out = strings.Join(parts, "\n")
	}
	if out == "" {
		return "(nothing)"
	}
	return out
}

func (a *Agent) runGoalMaker(ctx context.Context, spec GoalSpec, iter int, checklist []ChecklistItem, guidance string) (Usage, error) {
	sess, err := a.Session(ctx, fmt.Sprintf("%s:iter-%d", spec.SessionPrefix, iter))
	if err != nil {
		return Usage{}, err
	}
	if spec.OnSession != nil {
		spec.OnSession("maker", iter, sess)
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
	res, err := sess.Prompt(ctx, withGoalGuidance(withGoalContext(makerPrompt(spec.Objective, iter, checklist), spec.Context), guidance), opts...)
	var usage Usage
	sumUsage(&usage, res.NewMessages)
	return usage, err
}

func (a *Agent) runGoalChecker(ctx context.Context, spec GoalSpec, iter int, checklist []ChecklistItem, guidance string) (goalVerdict, Usage, error) {
	sess, err := a.Session(ctx, fmt.Sprintf("%s:check-%d", spec.SessionPrefix, iter))
	if err != nil {
		return goalVerdict{}, Usage{}, err
	}
	if spec.OnSession != nil {
		spec.OnSession("checker", iter, sess)
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
	opts = append(opts, WithMaxTurns(checkerMaxTurns(spec.MaxTurns)))
	if spec.Permission != nil {
		opts = append(opts, WithPermission(spec.Permission))
	}
	var verdict goalVerdict
	res, err := sess.PromptJSON(ctx, withGoalGuidance(checkerPrompt(spec.Objective, checklist), guidance), &verdict, opts...)
	var usage Usage
	sumUsage(&usage, res.NewMessages)
	if err != nil && ctx.Err() == nil && isLoopBudgetStop(err) {
		// The checker kept gathering evidence without answering. Ask once
		// more in a fresh session with no tools, handing over what it saw.
		verdict = goalVerdict{}
		var final PromptResult
		final, err = a.finalVerdict(ctx, spec, iter, checklist, guidance, plannerObservations(res.NewMessages), &verdict)
		sumUsage(&usage, final.NewMessages)
	}
	return verdict, usage, err
}

// CheckerMaxToolTurns bounds how many tool-using turns a goal checker may take
// before it must return a verdict.
const CheckerMaxToolTurns = 12

func checkerMaxTurns(specMax int) int {
	n := CheckerMaxToolTurns + 1
	if specMax > 0 && specMax < n {
		n = specMax
	}
	return n
}

// isLoopBudgetStop reports whether err means a session stopped for running
// out of turns or tripping a loop guardrail, rather than failing.
func isLoopBudgetStop(err error) bool {
	return errors.Is(err, loop.ErrMaxTurns) || errors.Is(err, loop.ErrRepeatedToolCalls) || errors.Is(err, loop.ErrTooManyMistakes)
}

// finalVerdict asks for the verdict in a fresh session without tools.
func (a *Agent) finalVerdict(ctx context.Context, spec GoalSpec, iter int, checklist []ChecklistItem, guidance, observations string, verdict *goalVerdict) (PromptResult, error) {
	sess, err := a.Session(ctx, fmt.Sprintf("%s:check-%d-final", spec.SessionPrefix, iter))
	if err != nil {
		return PromptResult{}, err
	}
	if spec.OnSession != nil {
		spec.OnSession("checker", iter, sess)
	}
	opts := []PromptOption{WithJSONSchema(goalVerdictSchema), WithSystemPrompt(spec.CheckerSystemPrompt), WithTools(nil), WithMaxTurns(1)}
	if spec.CheckerModel != "" {
		opts = append(opts, WithModel(spec.CheckerModel))
	}
	text := withGoalGuidance(checkerPrompt(spec.Objective, checklist), guidance) + "\n\nYou have no tools now. Decide from the evidence already gathered in the workspace; mark an item done only if this evidence shows it:\n" + observations
	return sess.PromptJSON(ctx, text, verdict, opts...)
}

func withGoalGuidance(prompt, guidance string) string {
	if guidance = strings.TrimSpace(guidance); guidance == "" {
		return prompt
	}
	return "Operator guidance\n" + guidance + "\n\n" + prompt
}

// withGoalContext appends the developer's background context to a prompt.
func withGoalContext(prompt, context string) string {
	if context = strings.TrimSpace(context); context == "" {
		return prompt
	}
	return prompt + "\n\nContext from the developer's session (background for the goal, not extra requirements):\n" + context
}

// ---- prompts ----

func planPrompt(objective string) string {
	return fmt.Sprintf(`Decompose this goal into a checklist of concrete, independently verifiable deliverables.

Goal: %s

Each item must be checkable against real evidence (a file exists, a test passes, a command succeeds). Prefer 2–8 items; avoid vague items like "make it good". Return the items with empty/false done — nothing has been built yet. Look at the workspace only as much as you need to plan; the maker will explore in detail.`, objective)
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

For every item, independently gather evidence (read files, run the build and tests, inspect diffs) and decide whether it is genuinely complete. Do not trust prior claims. Check each item once with the most direct evidence, then answer; do not re-check facts you have already confirmed. Return every item with its verified done flag and a short evidence note, set "done" true only if all items are complete, and give a one-line summary of what remains.`, objective, renderChecklist(checklist))
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
