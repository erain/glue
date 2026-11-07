package tui

import (
	"bytes"
	"strings"
	"testing"
)

func TestAssistantTextsSkipsEmptyAndNonAssistant(t *testing.T) {
	t.Parallel()
	m := &Model{transcript: []transcriptItem{
		{Kind: itemUser, Text: "hi"},
		{Kind: itemAssistant, Text: "first reply"},
		{Kind: itemTool, ToolName: "read_file"},
		{Kind: itemAssistant, Text: "   "},    // tool-only turn: blank text, skipped
		{Kind: itemAssistant, Text: "second"}, // raw markdown, not the rendered form
		{Kind: itemSystem, Text: "· note"},
	}}
	got := m.assistantTexts()
	want := []string{"first reply", "second"}
	if len(got) != len(want) {
		t.Fatalf("got %d texts %q, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("text[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestPickCopyTarget(t *testing.T) {
	t.Parallel()
	texts := []string{"alpha", "beta", "gamma"}
	cases := []struct {
		name      string
		texts     []string
		arg       string
		wantText  string
		wantLabel string
		wantErr   bool // expect a non-empty errMsg
	}{
		{"no replies", nil, "", "", "", true},
		{"default is last", texts, "", "gamma", "last reply", false},
		{"explicit first", texts, "1", "alpha", "reply 1", false},
		{"explicit middle, trimmed", texts, "  2 ", "beta", "reply 2", false},
		{"zero out of range", texts, "0", "", "", true},
		{"too high", texts, "4", "", "", true},
		{"non-numeric", texts, "abc", "", "", true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			text, label, errMsg := pickCopyTarget(c.texts, c.arg)
			if c.wantErr {
				if errMsg == "" {
					t.Fatalf("want errMsg, got none (text=%q)", text)
				}
				return
			}
			if errMsg != "" {
				t.Fatalf("unexpected errMsg: %q", errMsg)
			}
			if text != c.wantText || label != c.wantLabel {
				t.Fatalf("got (text=%q,label=%q), want (text=%q,label=%q)", text, label, c.wantText, c.wantLabel)
			}
		})
	}
}

func TestCopyToClipboardEmitsOSC52(t *testing.T) {
	var buf bytes.Buffer
	prev := clipboardWriter
	clipboardWriter = &buf
	defer func() { clipboardWriter = prev }()

	if err := copyToClipboard("hello # heading"); err != nil {
		t.Fatalf("copyToClipboard: %v", err)
	}
	out := buf.String()
	// OSC 52 clipboard set: ESC ] 52 ; c ; <base64> BEL/ST. We assert the
	// envelope and the base64 of the payload, not the exact terminator.
	if !strings.Contains(out, "\x1b]52;c;") {
		t.Fatalf("missing OSC 52 envelope in %q", out)
	}
	// base64("hello # heading") = aGVsbG8gIyBoZWFkaW5n
	if !strings.Contains(out, "aGVsbG8gIyBoZWFkaW5n") {
		t.Fatalf("payload not base64-encoded in clipboard sequence: %q", out)
	}
}

func TestRunSlashCopyAppendsConfirmation(t *testing.T) {
	var buf bytes.Buffer
	prev := clipboardWriter
	clipboardWriter = &buf
	defer func() { clipboardWriter = prev }()

	m := &Model{transcript: []transcriptItem{
		{Kind: itemAssistant, Text: "the answer"},
	}}
	m.runSlashCopy("")

	last := m.transcript[len(m.transcript)-1]
	if last.Kind != itemSystem || !strings.Contains(last.Text, "copied last reply") {
		t.Fatalf("expected confirmation system line, got %#v", last)
	}
	if !strings.Contains(last.Text, "10 chars") { // len("the answer") == 10
		t.Fatalf("expected char count in confirmation, got %q", last.Text)
	}
}

func TestRunSlashCopyNoReplies(t *testing.T) {
	m := &Model{}
	m.runSlashCopy("")
	last := m.transcript[len(m.transcript)-1]
	if last.Kind != itemSystem || !strings.Contains(last.Text, "nothing to copy") {
		t.Fatalf("expected 'nothing to copy' line, got %#v", last)
	}
}
