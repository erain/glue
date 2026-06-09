package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/erain/glue"
)

func TestPickerNavigationClampsAtBounds(t *testing.T) {
	t.Parallel()
	p := &sessionPicker{items: []glue.SessionSummary{{ID: "a"}, {ID: "b"}, {ID: "c"}}}
	p.up() // already at top
	if p.cursor != 0 {
		t.Fatalf("up at top: cursor = %d, want 0", p.cursor)
	}
	p.down()
	p.down()
	p.down() // past end
	if p.cursor != 2 {
		t.Fatalf("down past end: cursor = %d, want 2", p.cursor)
	}
	s, ok := p.selected()
	if !ok || s.ID != "c" {
		t.Fatalf("selected = %+v, want id=c", s)
	}
}

func TestPickerSelectedEmpty(t *testing.T) {
	t.Parallel()
	p := &sessionPicker{}
	if _, ok := p.selected(); ok {
		t.Fatal("selected on empty picker should be !ok")
	}
}

func TestTranscriptFromMessagesUserAssistantPair(t *testing.T) {
	t.Parallel()
	msgs := []glue.Message{
		{Role: glue.MessageRoleUser, Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: "hello"}}},
		{Role: glue.MessageRoleAssistant, Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: "hi back"}}},
	}
	out := transcriptFromMessages(msgs)
	if len(out) != 2 {
		t.Fatalf("got %d items, want 2", len(out))
	}
	if out[0].Kind != itemUser || out[0].Text != "hello" {
		t.Fatalf("user item = %+v", out[0])
	}
	if out[1].Kind != itemAssistant || out[1].Text != "hi back" {
		t.Fatalf("assistant item = %+v", out[1])
	}
}

func TestTranscriptFromMessagesPairsToolCallWithResult(t *testing.T) {
	t.Parallel()
	args, _ := json.Marshal(map[string]string{"path": "x.go"})
	msgs := []glue.Message{
		{Role: glue.MessageRoleAssistant, Content: []glue.ContentPart{
			{Type: glue.ContentTypeText, Text: "reading the file"},
			{Type: glue.ContentTypeToolCall, ToolCall: &glue.ToolCall{ID: "c1", Name: "read_file", Arguments: args}},
		}},
		{Role: glue.MessageRoleTool, ToolCallID: "c1", Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: "package x"}}},
	}
	out := transcriptFromMessages(msgs)
	// Expect: assistant text item, tool item with result paired in.
	var asst, tool *transcriptItem
	for i := range out {
		switch out[i].Kind {
		case itemAssistant:
			asst = &out[i]
		case itemTool:
			tool = &out[i]
		}
	}
	if asst == nil || tool == nil {
		t.Fatalf("missing items: %+v", out)
	}
	if !strings.Contains(asst.Text, "reading the file") {
		t.Fatalf("assistant text = %q", asst.Text)
	}
	if tool.ToolName != "read_file" || tool.ToolResult != "package x" || tool.ToolPhase != tsDone {
		t.Fatalf("tool item = %+v", tool)
	}
}

func TestHumanAge(t *testing.T) {
	t.Parallel()
	// Just sanity: cover the buckets so we don't regress them.
	cases := []string{"just now", "m ago", "h ago", "d ago"}
	got := []string{
		humanAge(30_000_000_000),          // 30s
		humanAge(30 * 60 * 1_000_000_000), // 30 min
		humanAge(3 * 3600 * 1_000_000_000),
		humanAge(48 * 3600 * 1_000_000_000),
	}
	for i, want := range cases {
		if !strings.Contains(got[i], want) {
			t.Errorf("humanAge case %d = %q, want substring %q", i, got[i], want)
		}
	}
}
