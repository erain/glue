package tui

import "github.com/erain/glue"

// Tea messages produced by the agent run goroutine and consumed by Update.

// textDeltaMsg appends streaming assistant text to the current
// assistant transcript item.
type textDeltaMsg string

// toolStartMsg adds a tool card to the transcript (or transitions an
// existing pending one to running).
type toolStartMsg struct {
	CallID string
	Name   string
	Args   string // pretty-printed JSON for display
}

// toolEndMsg finalizes a tool card.
type toolEndMsg struct {
	CallID  string
	Text    string
	IsError bool
}

// turnDoneMsg is emitted when session.Prompt returns. Err is non-nil on
// failure; Text is the final assistant text (may be empty if streaming
// already rendered it).
type turnDoneMsg struct {
	Err  error
	Text string
}

// permRequestMsg surfaces a permission request from the loop. The Respond
// channel is one-shot; Update sends the decision once the user answers.
type permRequestMsg struct {
	Req     glue.PermissionRequest
	Respond chan<- glue.PermissionDecision
}

// systemMsg adds a system/info line to the transcript (used by slash
// commands).
type systemMsg string

// sessionSwitchedMsg fires after /fork, /clone, or a /tree selection
// finishes loading the chosen session from the store. The Update loop
// resets the transcript to the loaded messages and notes the change.
type sessionSwitchedMsg struct {
	ID       string
	Note     string
	Messages []glue.Message
}

// goalEventMsg wraps one GoalSpec.Emit event from the background
// Agent.PursueGoal goroutine started by /goal.
type goalEventMsg struct{ Ev glue.GoalEvent }

// goalDoneMsg fires when PursueGoal returns with its terminal result.
type goalDoneMsg struct {
	Res glue.GoalResult
	Err error
}

// fatalErrMsg marks a setup error that should end the session.
type fatalErrMsg struct{ Err error }
