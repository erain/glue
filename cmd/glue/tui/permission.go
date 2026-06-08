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

// alwaysAllowPermission is the --yolo bypass: every request is approved
// immediately without surfacing a prompt to the user. RememberFor:
// RememberSession lets any downstream daemon protocol that persists
// remembers record the policy coherently — a yolo run trusts every
// action for the duration of the session, not forever.
type alwaysAllowPermission struct{}

func (alwaysAllowPermission) Decide(_ context.Context, _ glue.PermissionRequest) (glue.PermissionDecision, error) {
	return glue.PermissionDecision{
		Allow:       true,
		Reason:      "auto-approved by --yolo",
		RememberFor: glue.RememberSession,
	}, nil
}
