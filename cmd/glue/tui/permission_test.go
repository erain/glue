package tui

import (
	"context"
	"errors"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/erain/glue"
)

func TestPermissionBridgeAllow(t *testing.T) {
	t.Parallel()
	var captured permRequestMsg
	bridge := newPermissionBridge(func(m tea.Msg) {
		captured = m.(permRequestMsg)
		// Simulate the Update loop answering on the keyboard.
		go func() {
			captured.Respond <- glue.PermissionDecision{Allow: true, RememberFor: glue.RememberSession}
		}()
	})

	got, err := bridge.Decide(context.Background(), glue.PermissionRequest{
		Tool: "write_file", Action: "write_file", Target: "a.go",
	})
	if err != nil {
		t.Fatalf("Decide err = %v", err)
	}
	if !got.Allow || got.RememberFor != glue.RememberSession {
		t.Fatalf("decision = %#v, want allow+session", got)
	}
	if captured.Req.Tool != "write_file" || captured.Req.Target != "a.go" {
		t.Fatalf("forwarded request mismatch: %#v", captured.Req)
	}
}

func TestPermissionBridgeDeny(t *testing.T) {
	t.Parallel()
	bridge := newPermissionBridge(func(m tea.Msg) {
		req := m.(permRequestMsg)
		go func() {
			req.Respond <- glue.PermissionDecision{Allow: false, Reason: "user said no"}
		}()
	})
	got, err := bridge.Decide(context.Background(), glue.PermissionRequest{Tool: "shell_exec"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Allow || got.Reason != "user said no" {
		t.Fatalf("decision = %#v", got)
	}
}

func TestPermissionBridgeContextCancel(t *testing.T) {
	t.Parallel()
	// Send swallows the request; nothing ever responds. Cancellation must
	// release the bridge and surface ctx.Err().
	bridge := newPermissionBridge(func(m tea.Msg) {})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := bridge.Decide(ctx, glue.PermissionRequest{Tool: "shell_exec"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
