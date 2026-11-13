// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/erain/glue"
)

func textMsg(role glue.MessageRole, text string) glue.Message {
	return glue.Message{Role: role, Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: text}}}
}

func TestTUIRemoteCommand(t *testing.T) {
	st := newMemStore()
	agent := glue.NewAgent(glue.AgentOptions{Provider: fakeProvider{}, Store: st})
	env := textMsg(glue.MessageRoleUser, "<environment_context>\ncwd: /home/dev/secret-project\n</environment_context>")
	env.Metadata = map[string]any{glue.MetadataKeyEnvContext: true}
	_ = st.Save(context.Background(), "tui:abc", glue.SessionState{Version: glue.SessionStateVersion, ID: "tui:abc", Messages: []glue.Message{
		env,
		textMsg(glue.MessageRoleUser, "why does TestPool flake?"),
		textMsg(glue.MessageRoleAssistant, "TestPool races on the shared counter in pool.go:42."),
	}})
	var got RemoteRequest
	m := newModel(context.Background(), Config{Agent: agent, SessionID: "tui:abc", SubmitRemote: func(_ context.Context, req RemoteRequest, progress func(string)) (string, error) {
		got = req
		progress("checking local state")
		return "job-0123456789ab", nil
	}})
	cmd, ok := parseSlashCommand("/remote fix the flaky test we just found")
	if !ok {
		t.Fatal("not parsed")
	}
	_, run := m.handleSlash(cmd)
	if run == nil {
		t.Fatal("no command returned")
	}
	msg := run()
	if got.Objective != "fix the flaky test we just found" || got.Snapshot {
		t.Fatalf("request = %+v", got)
	}
	if !strings.Contains(got.Context, "races on the shared counter") || !strings.Contains(got.Context, "developer: why does TestPool flake?") {
		t.Fatalf("context = %q", got.Context)
	}
	if strings.Contains(got.Context, "secret-project") {
		t.Fatalf("environment context leaked: %q", got.Context)
	}
	if s, _ := msg.(systemMsg); !strings.Contains(string(s), "remote job job-0123456789ab") || !strings.Contains(string(s), "glue job attach job-0123456789ab") {
		t.Fatalf("final message = %v", msg)
	}
	cmd, _ = parseSlashCommand("/remote --snapshot try it on my working tree")
	_, run = m.handleSlash(cmd)
	run()
	if !got.Snapshot || got.Objective != "try it on my working tree" {
		t.Fatalf("--snapshot request = %+v", got)
	}
	m2 := newModel(context.Background(), Config{})
	cmd, _ = parseSlashCommand("/remote x")
	if _, run := m2.handleSlash(cmd); run != nil {
		t.Fatal("/remote without SubmitRemote should not submit")
	}
}

func TestTUIRemoteContextTruncated(t *testing.T) {
	long := strings.Repeat("a", 3000)
	msgs := []glue.Message{
		textMsg(glue.MessageRoleUser, "OLDEST "+long),
		textMsg(glue.MessageRoleAssistant, long),
		{Role: glue.MessageRoleTool, Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: "tool output"}}},
		textMsg(glue.MessageRoleUser, "latest question"),
	}
	got := remoteContext(msgs)
	if n := len([]rune(got)); n != remoteContextLimit {
		t.Fatalf("len = %d", n)
	}
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "developer: latest question") || strings.Contains(got, "OLDEST") || strings.Contains(got, "tool output") {
		t.Fatalf("context = %q…%q", got[:40], got[len(got)-40:])
	}
	summary := textMsg(glue.MessageRoleUser, "SUMMARY: we found the race")
	summary.Metadata = map[string]any{"compaction": "summarize"}
	got = remoteContext([]glue.Message{textMsg(glue.MessageRoleUser, "before the summary"), summary, textMsg(glue.MessageRoleAssistant, "after")})
	if strings.Contains(got, "before the summary") || !strings.Contains(got, "SUMMARY: we found the race") || !strings.HasSuffix(got, "assistant: after") {
		t.Fatalf("summary context = %q", got)
	}
	if remoteContext(nil) != "" {
		t.Fatal("empty session should give empty context")
	}
}
