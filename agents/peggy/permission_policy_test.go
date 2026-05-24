package peggy

import (
	"context"
	"strings"
	"testing"

	"github.com/erain/glue"
	"github.com/erain/glue/daemon"
)

type tierRecordingPermission struct {
	called   int
	decision glue.PermissionDecision
}

func (p *tierRecordingPermission) Decide(context.Context, glue.PermissionRequest) (glue.PermissionDecision, error) {
	p.called++
	return p.decision, nil
}

func TestTieredPermissionPromptDelegates(t *testing.T) {
	inner := &tierRecordingPermission{decision: glue.PermissionDecision{Allow: true, RememberFor: glue.RememberSession}}
	perm := NewTieredPermission(inner, PermissionTierPrompt, PermissionChannelCLI)

	decision, err := perm.Decide(context.Background(), glue.PermissionRequest{Tool: "shell_exec"})
	if err != nil {
		t.Fatal(err)
	}
	if inner.called != 1 {
		t.Fatalf("inner calls = %d, want 1", inner.called)
	}
	if !decision.Allow || decision.RememberFor != glue.RememberSession {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestTieredPermissionReadOnlySkipsInner(t *testing.T) {
	inner := &tierRecordingPermission{decision: glue.PermissionDecision{Allow: true}}
	perm := NewTieredPermission(inner, PermissionTierReadOnly, PermissionChannelTelegram)

	decision, err := perm.Decide(context.Background(), glue.PermissionRequest{Tool: "write_file"})
	if err != nil {
		t.Fatal(err)
	}
	if inner.called != 0 {
		t.Fatalf("inner calls = %d, want 0", inner.called)
	}
	if decision.Allow || !strings.Contains(decision.Reason, "telegram channel is read-only") {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestTieredPermissionTrustedSkipsInner(t *testing.T) {
	inner := &tierRecordingPermission{decision: glue.PermissionDecision{Allow: false}}
	perm := NewTieredPermission(inner, PermissionTierTrusted, PermissionChannelCLI)

	decision, err := perm.Decide(context.Background(), glue.PermissionRequest{Tool: "shell_exec"})
	if err != nil {
		t.Fatal(err)
	}
	if inner.called != 0 {
		t.Fatalf("inner calls = %d, want 0", inner.called)
	}
	if !decision.Allow {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestDaemonPermissionPolicyDerivesChannelFromClientID(t *testing.T) {
	policy := NewDaemonPermissionPolicy(PermissionSettings{
		DefaultTier: string(PermissionTierPrompt),
		Channels: map[string]string{
			PermissionChannelTelegram: string(PermissionTierReadOnly),
			PermissionChannelCLI:      string(PermissionTierTrusted),
		},
	})

	telegramDecision, err := policy.DecidePermission(context.Background(), daemon.PermissionContext{
		ClientID:  "telegram:123",
		SessionID: "default",
	}, glue.PermissionRequest{SessionID: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if telegramDecision.Action != daemon.PermissionPolicyDeny || !strings.Contains(telegramDecision.Reason, "telegram channel is read-only") {
		t.Fatalf("telegram decision = %+v", telegramDecision)
	}

	cliDecision, err := policy.DecidePermission(context.Background(), daemon.PermissionContext{
		ClientID:  "cli:123",
		SessionID: "telegram:123",
	}, glue.PermissionRequest{SessionID: "telegram:123"})
	if err != nil {
		t.Fatal(err)
	}
	if cliDecision.Action != daemon.PermissionPolicyAllow {
		t.Fatalf("cli decision = %+v", cliDecision)
	}
}

func TestDaemonPermissionPolicyFallsBackToSessionPrefix(t *testing.T) {
	policy := NewDaemonPermissionPolicy(PermissionSettings{
		DefaultTier: string(PermissionTierPrompt),
		Channels:    map[string]string{PermissionChannelTelegram: string(PermissionTierReadOnly)},
	})

	decision, err := policy.DecidePermission(context.Background(), daemon.PermissionContext{
		SessionID: "telegram:123",
	}, glue.PermissionRequest{SessionID: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != daemon.PermissionPolicyDeny {
		t.Fatalf("decision = %+v", decision)
	}
}
