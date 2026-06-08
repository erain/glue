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

// fatalErrMsg marks a setup error that should end the session.
type fatalErrMsg struct{ Err error }
