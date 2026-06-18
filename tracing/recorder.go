package tracing

import (
	"context"
	"sync"

	"github.com/erain/glue/loop"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Span names. Kept stable and namespaced so dashboards can group by them.
const (
	spanRun        = "glue.agent.run"
	spanTurn       = "glue.agent.turn"
	spanGenerate   = "glue.llm.generate"
	spanToolPrefix = "glue.tool."
)

// Attribute keys. The gen_ai.* keys follow the OpenTelemetry GenAI
// semantic conventions so traces interoperate with existing LLM
// dashboards; glue.* keys carry framework-specific detail.
const (
	attrSystem        = attribute.Key("gen_ai.system")
	attrRequestModel  = attribute.Key("gen_ai.request.model")
	attrResponseModel = attribute.Key("gen_ai.response.model")
	attrFinishReason  = attribute.Key("gen_ai.response.finish_reason")
	attrInputTokens   = attribute.Key("gen_ai.usage.input_tokens")
	attrOutputTokens  = attribute.Key("gen_ai.usage.output_tokens")
	attrTotalTokens   = attribute.Key("gen_ai.usage.total_tokens")

	attrCacheReadTokens  = attribute.Key("glue.usage.cache_read_tokens")
	attrCacheWriteTokens = attribute.Key("glue.usage.cache_write_tokens")
	attrSessionID        = attribute.Key("glue.session.id")
	attrToolName         = attribute.Key("glue.tool.name")
	attrToolCallID       = attribute.Key("glue.tool.call_id")
	attrToolError        = attribute.Key("glue.tool.is_error")
	attrTurnIndex        = attribute.Key("glue.turn.index")
)

// Recorder translates a single session's loop event stream into
// OpenTelemetry spans. Pass [Recorder.Handle] to
// [github.com/erain/glue.Session.Subscribe]; each prompt (one loop.Run,
// bracketed by EventLoopStart/EventLoopEnd) becomes its own trace.
//
// A Recorder is safe for the serial event delivery a single session
// provides (prompts are serialized by the session run lock) and guards
// its state with a mutex regardless. Do not share one Recorder across
// sessions whose prompts can overlap — give each session its own.
type Recorder struct {
	tracer trace.Tracer
	base   context.Context
	attrs  []attribute.KeyValue

	mu      sync.Mutex
	root    trace.Span
	rootCtx context.Context
	turn    trace.Span
	turnCtx context.Context
	msg     trace.Span
	tools   map[string]trace.Span
	turnNum int
}

// Option configures a [Recorder].
type Option func(*Recorder)

// WithBaseContext sets the parent context the root span is started from,
// so loop traces nest under an existing caller span. Defaults to
// context.Background (each prompt roots its own trace).
func WithBaseContext(ctx context.Context) Option {
	return func(r *Recorder) {
		if ctx != nil {
			r.base = ctx
		}
	}
}

// WithSessionID tags every span with the originating session id.
func WithSessionID(id string) Option {
	return func(r *Recorder) {
		if id != "" {
			r.attrs = append(r.attrs, attrSessionID.String(id))
		}
	}
}

// WithAttributes adds static attributes to every root span.
func WithAttributes(attrs ...attribute.KeyValue) Option {
	return func(r *Recorder) { r.attrs = append(r.attrs, attrs...) }
}

// NewRecorder builds a Recorder that emits spans on tracer. A nil tracer
// yields a Recorder whose Handle is a no-op, so callers can wire tracing
// unconditionally and let an unconfigured backend disable it.
func NewRecorder(tracer trace.Tracer, opts ...Option) *Recorder {
	r := &Recorder{tracer: tracer, base: context.Background(), tools: map[string]trace.Span{}}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// NewSessionRecorder returns a Recorder bound to the global tracer for the
// given session, or a no-op Recorder when tracing is disabled (see
// [Enabled]). Wire it unconditionally:
//
//	rec := tracing.NewSessionRecorder(session.ID())
//	defer session.Subscribe(rec.Handle)()
func NewSessionRecorder(sessionID string) *Recorder {
	if !Enabled() {
		return NewRecorder(nil)
	}
	return NewRecorder(otel.Tracer(ScopeName), WithSessionID(sessionID))
}

// Handle consumes one loop event and updates the span tree. It matches the
// signature of a glue session event handler.
func (r *Recorder) Handle(e loop.Event) {
	if r == nil || r.tracer == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	switch e.Type {
	case loop.EventLoopStart:
		r.startRun()
	case loop.EventTurnStart:
		r.startTurn()
	case loop.EventMessageStart:
		r.startMessage(e)
	case loop.EventMessageEnd:
		r.endMessage(e)
	case loop.EventToolStart:
		r.startTool(e)
	case loop.EventToolEnd:
		r.endTool(e)
	case loop.EventTurnEnd:
		r.endTurn()
	case loop.EventError:
		r.recordError(e)
	case loop.EventLoopEnd:
		r.endRun()
	}
}

func (r *Recorder) startRun() {
	// Defensive: a prior trace that never saw loop_end is closed out so a
	// crashed/cancelled run cannot leak spans into the next prompt.
	r.closeAll()
	r.turnNum = 0
	r.rootCtx, r.root = r.tracer.Start(r.base, spanRun, trace.WithAttributes(r.attrs...))
}

func (r *Recorder) startTurn() {
	parent := r.parentCtx(r.rootCtx)
	r.turnCtx, r.turn = r.tracer.Start(parent, spanTurn, trace.WithAttributes(attrTurnIndex.Int(r.turnNum)))
	r.turnNum++
}

func (r *Recorder) startMessage(e loop.Event) {
	parent := r.parentCtx(r.turnCtx)
	var attrs []attribute.KeyValue
	if e.Message != nil {
		if e.Message.Model != "" {
			attrs = append(attrs, attrRequestModel.String(e.Message.Model))
		}
		if e.Message.Provider != "" {
			attrs = append(attrs, attrSystem.String(e.Message.Provider))
		}
	}
	_, r.msg = r.tracer.Start(parent, spanGenerate, trace.WithAttributes(attrs...))
}

func (r *Recorder) endMessage(e loop.Event) {
	if r.msg == nil {
		return
	}
	if m := e.Message; m != nil {
		if m.Model != "" {
			r.msg.SetAttributes(attrResponseModel.String(m.Model))
		}
		if m.Provider != "" {
			r.msg.SetAttributes(attrSystem.String(m.Provider))
		}
		if m.StopReason != "" {
			r.msg.SetAttributes(attrFinishReason.String(string(m.StopReason)))
		}
		if u := m.Usage; u != nil {
			r.msg.SetAttributes(
				attrInputTokens.Int64(u.InputTokens),
				attrOutputTokens.Int64(u.OutputTokens),
				attrTotalTokens.Int64(u.TotalTokens),
				attrCacheReadTokens.Int64(u.CacheReadTokens),
				attrCacheWriteTokens.Int64(u.CacheWriteTokens),
			)
		}
	}
	r.msg.End()
	r.msg = nil
}

func (r *Recorder) startTool(e loop.Event) {
	parent := r.parentCtx(r.turnCtx)
	name := e.ToolName
	if name == "" {
		name = "unknown"
	}
	_, span := r.tracer.Start(parent, spanToolPrefix+name, trace.WithAttributes(
		attrToolName.String(name),
		attrToolCallID.String(e.ToolCallID),
	))
	// Key by call id when present so parallel tool calls within a turn end
	// against the right span; fall back to name for providers that omit ids.
	r.tools[r.toolKey(e)] = span
}

func (r *Recorder) endTool(e loop.Event) {
	key := r.toolKey(e)
	span, ok := r.tools[key]
	if !ok {
		return
	}
	delete(r.tools, key)
	isErr := e.ToolResult != nil && e.ToolResult.IsError
	span.SetAttributes(attrToolError.Bool(isErr))
	if isErr {
		span.SetStatus(codes.Error, "tool reported an error")
	}
	span.End()
}

func (r *Recorder) endTurn() {
	// A normal turn has already ended its message; close any stragglers so
	// an error path cannot leak the span.
	if r.msg != nil {
		r.msg.End()
		r.msg = nil
	}
	if r.turn != nil {
		r.turn.End()
		r.turn = nil
		r.turnCtx = nil
	}
}

func (r *Recorder) recordError(e loop.Event) {
	// Attribute the failure to the deepest open span for precision, and
	// also flag the root so the whole trace surfaces as failed in backends
	// that filter on root status.
	if span := r.innermost(); span != nil {
		span.SetStatus(codes.Error, e.Error)
	}
	if r.root != nil {
		r.root.SetStatus(codes.Error, e.Error)
	}
}

func (r *Recorder) endRun() {
	r.closeAll()
}

// closeAll ends every open span (tools, message, turn, root) so no span
// leaks if the loop exits without the matching end events (cancellation,
// provider error, max-turns).
func (r *Recorder) closeAll() {
	for key, span := range r.tools {
		span.End()
		delete(r.tools, key)
	}
	if r.msg != nil {
		r.msg.End()
		r.msg = nil
	}
	if r.turn != nil {
		r.turn.End()
		r.turn = nil
		r.turnCtx = nil
	}
	if r.root != nil {
		r.root.End()
		r.root = nil
		r.rootCtx = nil
	}
}

// innermost returns the deepest currently-open span for error attribution,
// preferring an open message, then the turn, then the root.
func (r *Recorder) innermost() trace.Span {
	switch {
	case r.msg != nil:
		return r.msg
	case r.turn != nil:
		return r.turn
	default:
		return r.root
	}
}

func (r *Recorder) parentCtx(ctx context.Context) context.Context {
	if ctx != nil {
		return ctx
	}
	if r.rootCtx != nil {
		return r.rootCtx
	}
	return r.base
}

func (r *Recorder) toolKey(e loop.Event) string {
	if e.ToolCallID != "" {
		return e.ToolCallID
	}
	return "name:" + e.ToolName
}
