package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/erain/glue"
)

// permissionBridge implements glue.Permission by forwarding requests to
// the bubbletea Update loop as permRequestMsg, then blocking on a
// per-request response channel. Cancellation via ctx returns a denial.
type permissionBridge struct {
	send func(tea.Msg)
}

func newPermissionBridge(send func(tea.Msg)) *permissionBridge {
	return &permissionBridge{send: send}
}

func (p *permissionBridge) Decide(ctx context.Context, req glue.PermissionRequest) (glue.PermissionDecision, error) {
	respond := make(chan glue.PermissionDecision, 1)
	p.send(permRequestMsg{Req: req, Respond: respond})
	select {
	case d := <-respond:
		return d, nil
	case <-ctx.Done():
		return glue.PermissionDecision{Allow: false, Reason: "cancelled: " + ctx.Err().Error()}, ctx.Err()
	}
}
