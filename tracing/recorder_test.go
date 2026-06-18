package tracing

import (
	"testing"

	"github.com/erain/glue/loop"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// newTestRecorder returns a Recorder writing into an in-memory span
// recorder so tests can assert on the emitted span tree.
func newTestRecorder(t *testing.T, opts ...Option) (*Recorder, *tracetest.SpanRecorder) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	return NewRecorder(tp.Tracer("test"), opts...), sr
}

func feed(r *Recorder, events ...loop.Event) {
	for _, e := range events {
		r.Handle(e)
	}
}

func endedByName(spans []sdktrace.ReadOnlySpan) map[string]sdktrace.ReadOnlySpan {
	out := make(map[string]sdktrace.ReadOnlySpan, len(spans))
	for _, s := range spans {
		out[s.Name()] = s
	}
	return out
}

func attrValue(s sdktrace.ReadOnlySpan, key attribute.Key) (attribute.Value, bool) {
	for _, kv := range s.Attributes() {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func TestRecorderHappyPath(t *testing.T) {
	rec, sr := newTestRecorder(t, WithSessionID("sess-1"))

	feed(rec,
		loop.Event{Type: loop.EventLoopStart},
		loop.Event{Type: loop.EventTurnStart},
		loop.Event{Type: loop.EventMessageStart, Message: &loop.Message{Model: "claude", Provider: "anthropic"}},
		loop.Event{Type: loop.EventMessageEnd, Message: &loop.Message{
			Model:      "claude",
			Provider:   "anthropic",
			StopReason: loop.StopReasonToolUse,
			Usage:      &loop.Usage{InputTokens: 10, OutputTokens: 7, TotalTokens: 17},
		}},
		loop.Event{Type: loop.EventToolStart, ToolName: "read_file", ToolCallID: "call-1"},
		loop.Event{Type: loop.EventToolEnd, ToolName: "read_file", ToolCallID: "call-1", ToolResult: &loop.ToolResult{}},
		loop.Event{Type: loop.EventTurnEnd},
		loop.Event{Type: loop.EventLoopEnd},
	)

	spans := sr.Ended()
	if len(spans) != 4 {
		t.Fatalf("expected 4 spans, got %d: %v", len(spans), spanNames(spans))
	}
	byName := endedByName(spans)

	run := mustSpan(t, byName, spanRun)
	turn := mustSpan(t, byName, spanTurn)
	gen := mustSpan(t, byName, spanGenerate)
	tool := mustSpan(t, byName, spanToolPrefix+"read_file")

	// Span tree: run -> turn -> {generate, tool}.
	if !turn.Parent().Equal(run.SpanContext()) {
		t.Error("turn is not a child of run")
	}
	if !gen.Parent().Equal(turn.SpanContext()) {
		t.Error("generate is not a child of turn")
	}
	if !tool.Parent().Equal(turn.SpanContext()) {
		t.Error("tool is not a child of turn")
	}
	// All share one trace.
	if run.SpanContext().TraceID() != tool.SpanContext().TraceID() {
		t.Error("spans are not in the same trace")
	}

	// Session id propagates to the root.
	if v, ok := attrValue(run, attrSessionID); !ok || v.AsString() != "sess-1" {
		t.Errorf("missing/incorrect session id attr: %v ok=%v", v.AsString(), ok)
	}
	// Usage + finish reason land on the generate span.
	if v, ok := attrValue(gen, attrInputTokens); !ok || v.AsInt64() != 10 {
		t.Errorf("input tokens = %v ok=%v, want 10", v.AsInt64(), ok)
	}
	if v, ok := attrValue(gen, attrFinishReason); !ok || v.AsString() != string(loop.StopReasonToolUse) {
		t.Errorf("finish reason = %v ok=%v", v.AsString(), ok)
	}
	// Tool error flag is false for a clean result.
	if v, ok := attrValue(tool, attrToolError); !ok || v.AsBool() {
		t.Errorf("tool is_error = %v ok=%v, want false", v.AsBool(), ok)
	}
}

func TestRecorderToolError(t *testing.T) {
	rec, sr := newTestRecorder(t)
	feed(rec,
		loop.Event{Type: loop.EventLoopStart},
		loop.Event{Type: loop.EventTurnStart},
		loop.Event{Type: loop.EventToolStart, ToolName: "shell_exec", ToolCallID: "c1"},
		loop.Event{Type: loop.EventToolEnd, ToolName: "shell_exec", ToolCallID: "c1", ToolResult: &loop.ToolResult{IsError: true}},
		loop.Event{Type: loop.EventTurnEnd},
		loop.Event{Type: loop.EventLoopEnd},
	)
	tool := mustSpan(t, endedByName(sr.Ended()), spanToolPrefix+"shell_exec")
	if v, ok := attrValue(tool, attrToolError); !ok || !v.AsBool() {
		t.Errorf("tool is_error = %v ok=%v, want true", v.AsBool(), ok)
	}
	if tool.Status().Code != codes.Error {
		t.Errorf("tool status = %v, want Error", tool.Status().Code)
	}
}

func TestRecorderParallelTools(t *testing.T) {
	rec, sr := newTestRecorder(t)
	// Parallel mode emits both tool_start events before either tool_end.
	feed(rec,
		loop.Event{Type: loop.EventLoopStart},
		loop.Event{Type: loop.EventTurnStart},
		loop.Event{Type: loop.EventToolStart, ToolName: "a", ToolCallID: "c1"},
		loop.Event{Type: loop.EventToolStart, ToolName: "b", ToolCallID: "c2"},
		loop.Event{Type: loop.EventToolEnd, ToolName: "b", ToolCallID: "c2", ToolResult: &loop.ToolResult{}},
		loop.Event{Type: loop.EventToolEnd, ToolName: "a", ToolCallID: "c1", ToolResult: &loop.ToolResult{}},
		loop.Event{Type: loop.EventTurnEnd},
		loop.Event{Type: loop.EventLoopEnd},
	)
	byName := endedByName(sr.Ended())
	a := mustSpan(t, byName, spanToolPrefix+"a")
	b := mustSpan(t, byName, spanToolPrefix+"b")
	if v, _ := attrValue(a, attrToolCallID); v.AsString() != "c1" {
		t.Errorf("span a call id = %q, want c1", v.AsString())
	}
	if v, _ := attrValue(b, attrToolCallID); v.AsString() != "c2" {
		t.Errorf("span b call id = %q, want c2", v.AsString())
	}
}

func TestRecorderErrorClosesOpenSpans(t *testing.T) {
	rec, sr := newTestRecorder(t)
	// A provider error mid-turn: no turn_end is emitted, loop_end follows.
	feed(rec,
		loop.Event{Type: loop.EventLoopStart},
		loop.Event{Type: loop.EventTurnStart},
		loop.Event{Type: loop.EventMessageStart, Message: &loop.Message{Model: "x"}},
		loop.Event{Type: loop.EventError, Error: "stream closed"},
		loop.Event{Type: loop.EventLoopEnd},
	)
	spans := sr.Ended()
	if len(spans) != 3 {
		t.Fatalf("expected run+turn+generate (3) spans, got %d: %v", len(spans), spanNames(spans))
	}
	byName := endedByName(spans)
	run := mustSpan(t, byName, spanRun)
	if run.Status().Code != codes.Error {
		t.Errorf("root status = %v, want Error", run.Status().Code)
	}
	gen := mustSpan(t, byName, spanGenerate)
	if gen.Status().Code != codes.Error {
		t.Errorf("generate status = %v, want Error (innermost open span)", gen.Status().Code)
	}
}

func TestRecorderNewRunClosesLeakedSpans(t *testing.T) {
	rec, sr := newTestRecorder(t)
	// First run never receives loop_end (e.g. process killed mid-prompt);
	// the next loop_start must not leak the prior trace's open spans.
	feed(rec,
		loop.Event{Type: loop.EventLoopStart},
		loop.Event{Type: loop.EventTurnStart},
		loop.Event{Type: loop.EventLoopStart},
		loop.Event{Type: loop.EventTurnStart},
		loop.Event{Type: loop.EventTurnEnd},
		loop.Event{Type: loop.EventLoopEnd},
	)
	// 2 runs + 2 turns = 4 spans, all ended, none leaked.
	if got := len(sr.Ended()); got != 4 {
		t.Fatalf("expected 4 ended spans, got %d: %v", got, spanNames(sr.Ended()))
	}
}

func TestRecorderNilTracerNoop(t *testing.T) {
	var rec *Recorder
	rec.Handle(loop.Event{Type: loop.EventLoopStart}) // nil receiver: must not panic

	rec = NewRecorder(nil)
	feed(rec,
		loop.Event{Type: loop.EventLoopStart},
		loop.Event{Type: loop.EventTurnStart},
		loop.Event{Type: loop.EventLoopEnd},
	) // nil tracer: must not panic
}

func TestRecorderBaseContextParenting(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	tracer := tp.Tracer("test")

	outerCtx, outer := tracer.Start(t.Context(), "outer")
	rec := NewRecorder(tracer, WithBaseContext(outerCtx))
	feed(rec,
		loop.Event{Type: loop.EventLoopStart},
		loop.Event{Type: loop.EventLoopEnd},
	)
	outer.End()

	run := mustSpan(t, endedByName(sr.Ended()), spanRun)
	if !run.Parent().Equal(outer.SpanContext()) {
		t.Error("run span should nest under the provided base context")
	}
}

func mustSpan(t *testing.T, byName map[string]sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	s, ok := byName[name]
	if !ok {
		t.Fatalf("missing span %q", name)
	}
	return s
}

func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	names := make([]string, len(spans))
	for i, s := range spans {
		names[i] = s.Name()
	}
	return names
}
