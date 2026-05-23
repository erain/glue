package peggy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/erain/glue"
)

const permissionArgsPreviewBytes = 320

// CLIPermissionOptions configures [NewCLIPermission].
type CLIPermissionOptions struct {
	// Stdin supplies prompt answers. Nil denies every request.
	Stdin io.Reader

	// Stderr receives prompts and diagnostics. Nil discards output.
	Stderr io.Writer
}

// NewCLIPermission returns an interactive Permission implementation for
// Peggy's single-prompt CLI. Decisions remembered for the session live only
// in this object and are not persisted.
func NewCLIPermission(opts CLIPermissionOptions) glue.Permission {
	return &cliPermission{
		in:             opts.Stdin,
		out:            opts.Stderr,
		sessionAllows:  map[string]struct{}{},
		targetAllows:   map[string]struct{}{},
		permissionScan: bufio.NewReader(opts.Stdin),
	}
}

type cliPermission struct {
	mu sync.Mutex

	in  io.Reader
	out io.Writer

	permissionScan *bufio.Reader
	sessionAllows  map[string]struct{}
	targetAllows   map[string]struct{}
}

func (p *cliPermission) Decide(ctx context.Context, req glue.PermissionRequest) (glue.PermissionDecision, error) {
	if p == nil {
		return glue.PermissionDecision{Allow: false, Reason: "permission denied: no CLI permission handler"}, nil
	}
	if err := ctx.Err(); err != nil {
		return glue.PermissionDecision{}, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if _, ok := p.sessionAllows[permissionSessionKey(req)]; ok {
		return glue.PermissionDecision{Allow: true, RememberFor: glue.RememberSession}, nil
	}
	if _, ok := p.targetAllows[permissionTargetKey(req)]; ok {
		return glue.PermissionDecision{Allow: true, RememberFor: glue.RememberSessionTarget}, nil
	}
	if p.in == nil || p.permissionScan == nil {
		p.print("peggy: permission denied for %s %q: stdin is not available\n", req.Action, req.Target)
		return glue.PermissionDecision{Allow: false, Reason: "permission denied: stdin is not available"}, nil
	}

	p.printPermissionRequest(req)
	answer, err := p.permissionScan.ReadString('\n')
	if err != nil && len(answer) == 0 {
		reason := "permission denied: no input"
		if err != io.EOF {
			reason = "permission denied: " + err.Error()
		}
		p.print("peggy: %s\n", reason)
		return glue.PermissionDecision{Allow: false, Reason: reason}, nil
	}

	decision := parsePermissionAnswer(answer)
	if !decision.Allow {
		if decision.Reason == "" {
			decision.Reason = "permission denied by user"
		}
		p.print("peggy: permission denied\n")
		return decision, nil
	}

	switch decision.RememberFor {
	case glue.RememberSession:
		p.sessionAllows[permissionSessionKey(req)] = struct{}{}
	case glue.RememberSessionTarget:
		p.targetAllows[permissionTargetKey(req)] = struct{}{}
	}
	return decision, nil
}

func (p *cliPermission) printPermissionRequest(req glue.PermissionRequest) {
	p.print("\npeggy: permission requested\n")
	p.print("  tool:   %s\n", req.Tool)
	p.print("  action: %s\n", req.Action)
	if strings.TrimSpace(req.Target) != "" {
		p.print("  target: %s\n", req.Target)
	}
	if preview := permissionArgsPreview(req.Args); preview != "" {
		p.print("  args:   %s\n", preview)
	}
	p.print("Allow? [y] once, [s] session, [t] target, [n] deny: ")
}

func (p *cliPermission) print(format string, args ...any) {
	if p.out == nil {
		return
	}
	_, _ = fmt.Fprintf(p.out, format, args...)
}

func parsePermissionAnswer(answer string) glue.PermissionDecision {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes", "a", "allow", "o", "once":
		return glue.PermissionDecision{Allow: true, RememberFor: glue.RememberNever}
	case "s", "session", "yes-session", "yes for session":
		return glue.PermissionDecision{Allow: true, RememberFor: glue.RememberSession}
	case "t", "target", "yes-target", "yes for target":
		return glue.PermissionDecision{Allow: true, RememberFor: glue.RememberSessionTarget}
	default:
		return glue.PermissionDecision{Allow: false, Reason: "permission denied by user"}
	}
}

func permissionSessionKey(req glue.PermissionRequest) string {
	return req.Tool + "\x00" + req.Action
}

func permissionTargetKey(req glue.PermissionRequest) string {
	return permissionSessionKey(req) + "\x00" + req.Target
}

func permissionArgsPreview(raw json.RawMessage) string {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return ""
	}
	if len(text) <= permissionArgsPreviewBytes {
		return text
	}
	return text[:permissionArgsPreviewBytes] + "...(truncated)"
}
