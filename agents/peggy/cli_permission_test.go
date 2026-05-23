package peggy

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/erain/glue"
)

func TestCLIPermissionAllowOnceDoesNotRemember(t *testing.T) {
	var stderr bytes.Buffer
	perm := NewCLIPermission(CLIPermissionOptions{
		Stdin:  strings.NewReader("y\nn\n"),
		Stderr: &stderr,
	})
	req := glue.PermissionRequest{Tool: "shell_exec", Action: "exec", Target: "go test ./..."}

	first, err := perm.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("first Decide: %v", err)
	}
	if !first.Allow || first.RememberFor != glue.RememberNever {
		t.Fatalf("first decision = %+v, want allow once", first)
	}
	second, err := perm.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("second Decide: %v", err)
	}
	if second.Allow {
		t.Fatalf("second decision = %+v, want deny after no cache", second)
	}
	if !strings.Contains(stderr.String(), "go test ./...") {
		t.Fatalf("stderr = %q, want target preview", stderr.String())
	}
}

func TestCLIPermissionRememberSession(t *testing.T) {
	perm := NewCLIPermission(CLIPermissionOptions{Stdin: strings.NewReader("s\n")})
	req := glue.PermissionRequest{Tool: "shell_exec", Action: "exec", Target: "go test ./..."}

	first, err := perm.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("first Decide: %v", err)
	}
	if !first.Allow || first.RememberFor != glue.RememberSession {
		t.Fatalf("first decision = %+v, want session allow", first)
	}
	second, err := perm.Decide(context.Background(), glue.PermissionRequest{
		Tool:   "shell_exec",
		Action: "exec",
		Target: "go vet ./...",
	})
	if err != nil {
		t.Fatalf("second Decide: %v", err)
	}
	if !second.Allow || second.RememberFor != glue.RememberSession {
		t.Fatalf("second decision = %+v, want cached session allow", second)
	}
}

func TestCLIPermissionRememberTarget(t *testing.T) {
	perm := NewCLIPermission(CLIPermissionOptions{Stdin: strings.NewReader("t\nn\n")})
	req := glue.PermissionRequest{Tool: "write_file", Action: "write_file", Target: "main.go"}

	first, err := perm.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("first Decide: %v", err)
	}
	if !first.Allow || first.RememberFor != glue.RememberSessionTarget {
		t.Fatalf("first decision = %+v, want target allow", first)
	}
	second, err := perm.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("second Decide: %v", err)
	}
	if !second.Allow {
		t.Fatalf("second decision = %+v, want cached target allow", second)
	}
	third, err := perm.Decide(context.Background(), glue.PermissionRequest{
		Tool:   "write_file",
		Action: "write_file",
		Target: "other.go",
	})
	if err != nil {
		t.Fatalf("third Decide: %v", err)
	}
	if third.Allow {
		t.Fatalf("third decision = %+v, want deny for different target", third)
	}
}

func TestCLIPermissionEOFAndNilStdinDeny(t *testing.T) {
	req := glue.PermissionRequest{Tool: "shell_exec", Action: "exec", Target: "go test ./..."}

	eofPerm := NewCLIPermission(CLIPermissionOptions{Stdin: strings.NewReader("")})
	eofDecision, err := eofPerm.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("EOF Decide: %v", err)
	}
	if eofDecision.Allow || !strings.Contains(eofDecision.Reason, "no input") {
		t.Fatalf("EOF decision = %+v, want no-input deny", eofDecision)
	}

	nilPerm := NewCLIPermission(CLIPermissionOptions{})
	nilDecision, err := nilPerm.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("nil Decide: %v", err)
	}
	if nilDecision.Allow || !strings.Contains(nilDecision.Reason, "stdin is not available") {
		t.Fatalf("nil decision = %+v, want stdin deny", nilDecision)
	}
}
