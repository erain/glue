package glue

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// Namespaced metadata keys carrying the durable goal record on the session
// stored at the goal's SessionPrefix id — the same convention as the
// glue/tree:* lineage keys.
const (
	goalMetaObjective  = "glue/goal:objective"
	goalMetaStatus     = "glue/goal:status"
	goalMetaChecklist  = "glue/goal:checklist" // JSON-encoded []ChecklistItem
	goalMetaIterations = "glue/goal:iterations"
	goalMetaUsage      = "glue/goal:usage" // JSON-encoded Usage
	goalMetaSummary    = "glue/goal:summary"
)

// GoalRecord is the durable snapshot of a goal loop, checkpointed by
// [Agent.PursueGoal] as it runs (when the agent has a Store) and read back
// by [Agent.LoadGoal] / [Agent.ListGoals]. ID is the GoalSpec.SessionPrefix
// the goal ran under.
type GoalRecord struct {
	ID         string          `json:"id"`
	Objective  string          `json:"objective"`
	Status     GoalStatus      `json:"status"`
	Checklist  []ChecklistItem `json:"checklist,omitempty"`
	Iterations int             `json:"iterations"`
	Usage      Usage           `json:"usage"`
	Summary    string          `json:"summary,omitempty"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

// Resumable reports whether the goal can be continued from its checklist:
// any unmet, non-running state with verified items to seed from. A stale
// "running" record left behind by a crashed process is excluded — a caller
// that knows the owning process is gone may still choose to resume it.
func (r GoalRecord) Resumable() bool {
	switch r.Status {
	case GoalPaused, GoalErrored, GoalBlocked, GoalMaxIterations, GoalBudgetLimited:
		return len(r.Checklist) > 0
	default:
		return false
	}
}

// saveGoalRecord checkpoints the record onto the session at rec.ID.
// Best-effort by design: persistence failures must never abort the loop,
// so the error is intentionally dropped by callers inside PursueGoal.
func (a *Agent) saveGoalRecord(ctx context.Context, rec GoalRecord) error {
	if a.store == nil {
		return nil
	}
	state, ok, err := a.store.Load(ctx, rec.ID)
	if err != nil {
		return err
	}
	now := time.Now()
	if !ok {
		state = SessionState{Version: 1, ID: rec.ID, CreatedAt: now}
	}
	if state.Metadata == nil {
		state.Metadata = map[string]any{}
	}
	checklist, err := json.Marshal(rec.Checklist)
	if err != nil {
		return err
	}
	usage, err := json.Marshal(rec.Usage)
	if err != nil {
		return err
	}
	state.Metadata[goalMetaObjective] = rec.Objective
	state.Metadata[goalMetaStatus] = string(rec.Status)
	state.Metadata[goalMetaChecklist] = string(checklist)
	state.Metadata[goalMetaIterations] = rec.Iterations
	state.Metadata[goalMetaUsage] = string(usage)
	state.Metadata[goalMetaSummary] = rec.Summary
	state.UpdatedAt = now
	return a.store.Save(ctx, rec.ID, state)
}

// LoadGoal returns the durable record for the goal that ran under id (its
// GoalSpec.SessionPrefix). ok is false when the session does not exist or
// carries no goal metadata.
func (a *Agent) LoadGoal(ctx context.Context, id string) (GoalRecord, bool, error) {
	if a == nil || a.store == nil {
		return GoalRecord{}, false, nil
	}
	state, ok, err := a.store.Load(ctx, id)
	if err != nil || !ok {
		return GoalRecord{}, false, err
	}
	rec, ok := goalRecordFromState(state)
	return rec, ok, nil
}

// ListGoalsOptions filters and pages a goal record listing.
type ListGoalsOptions struct {
	// Prefix restricts results to goal ids beginning with Prefix
	// (e.g. "goal-" for goals started by the TUI).
	Prefix string

	// Limit caps returned records. Non-positive values return all matches
	// the underlying session listing yields.
	Limit int
}

// ListGoals returns durable goal records, most recently updated first. It
// requires the store to implement the optional [SessionLister] capability
// and returns [ErrSessionListingNotSupported] otherwise.
func (a *Agent) ListGoals(ctx context.Context, opts ListGoalsOptions) ([]GoalRecord, error) {
	// Over-fetch sessions: most ids under the prefix are per-iteration
	// maker/checker sessions, not goal records. The lister's own default
	// page would miss records, so ask for a generous fixed window.
	summaries, err := a.ListSessions(ctx, ListSessionsOptions{Prefix: opts.Prefix, Limit: 1000})
	if err != nil {
		return nil, err
	}
	var out []GoalRecord
	for _, s := range summaries {
		// Iteration sessions are named {prefix}:plan / :iter-N / :check-N;
		// records live at the bare prefix. Skipping ids with ':' after the
		// prefix avoids a store Load per non-record session.
		if strings.Contains(strings.TrimPrefix(s.ID, opts.Prefix), ":") {
			continue
		}
		state, ok, err := a.store.Load(ctx, s.ID)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if rec, ok := goalRecordFromState(state); ok {
			out = append(out, rec)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, nil
}

// goalRecordFromState decodes the glue/goal:* metadata, tolerating missing
// or malformed keys for forward compatibility. ok is false when the state
// carries no goal objective at all.
func goalRecordFromState(state SessionState) (GoalRecord, bool) {
	objective, ok := state.Metadata[goalMetaObjective].(string)
	if !ok || strings.TrimSpace(objective) == "" {
		return GoalRecord{}, false
	}
	rec := GoalRecord{
		ID:        state.ID,
		Objective: objective,
		UpdatedAt: state.UpdatedAt,
	}
	if s, ok := state.Metadata[goalMetaStatus].(string); ok {
		rec.Status = GoalStatus(s)
	}
	if s, ok := state.Metadata[goalMetaChecklist].(string); ok {
		_ = json.Unmarshal([]byte(s), &rec.Checklist)
	}
	switch n := state.Metadata[goalMetaIterations].(type) {
	case int:
		rec.Iterations = n
	case float64: // JSON round-trip through the store decodes numbers as float64
		rec.Iterations = int(n)
	}
	if s, ok := state.Metadata[goalMetaUsage].(string); ok {
		_ = json.Unmarshal([]byte(s), &rec.Usage)
	}
	if s, ok := state.Metadata[goalMetaSummary].(string); ok {
		rec.Summary = s
	}
	return rec, true
}
