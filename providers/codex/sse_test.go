package codex

import (
	"context"
	"strings"
	"testing"
)

func TestReadSSEEvents_BasicFraming(t *testing.T) {
	body := "event: response.created\ndata: {\"response\":{\"id\":\"r1\"}}\n\n" +
		"event: response.output_text.delta\ndata: {\"delta\":\"hi\"}\n\n" +
		"event: response.completed\ndata: {\"response\":{\"id\":\"r1\"}}\n\n"
	ch, errs := readSSEEvents(context.Background(), strings.NewReader(body))
	var got []sseEvent
	for ev := range ch {
		got = append(got, ev)
	}
	if err := <-errs; err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d events: %+v", len(got), got)
	}
	if got[0].Event != "response.created" || !strings.Contains(got[0].Data, "r1") {
		t.Errorf("first event: %+v", got[0])
	}
	if got[1].Event != "response.output_text.delta" || !strings.Contains(got[1].Data, "hi") {
		t.Errorf("second event: %+v", got[1])
	}
	if got[2].Event != "response.completed" {
		t.Errorf("third event: %+v", got[2])
	}
}

func TestReadSSEEvents_IgnoresCommentsAndBlankPrefix(t *testing.T) {
	body := ": keep-alive\n\n" +
		"event: response.created\ndata: {\"ok\":true}\n\n"
	ch, _ := readSSEEvents(context.Background(), strings.NewReader(body))
	got := drainSSE(ch)
	// Comments don't produce events; blank line after them is harmless.
	if len(got) != 1 || got[0].Event != "response.created" {
		t.Fatalf("got %+v", got)
	}
}

func TestReadSSEEvents_MultilineData(t *testing.T) {
	body := "event: x\ndata: line1\ndata: line2\n\n"
	ch, _ := readSSEEvents(context.Background(), strings.NewReader(body))
	got := drainSSE(ch)
	if len(got) != 1 {
		t.Fatalf("got %d", len(got))
	}
	if got[0].Data != "line1\nline2" {
		t.Errorf("data = %q", got[0].Data)
	}
}

func TestReadSSEEvents_FlushesTrailingEventWithoutBlankLine(t *testing.T) {
	body := "event: a\ndata: {}\n" // no trailing blank line
	ch, _ := readSSEEvents(context.Background(), strings.NewReader(body))
	got := drainSSE(ch)
	if len(got) != 1 {
		t.Fatalf("trailing event not flushed: %+v", got)
	}
}

func drainSSE(ch <-chan sseEvent) []sseEvent {
	var out []sseEvent
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}
