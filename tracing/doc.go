// Package tracing turns the glue agent loop's event stream into
// OpenTelemetry spans.
//
// The loop ([github.com/erain/glue/loop.Run]) already emits a structured,
// strictly-ordered [loop.Event] stream — loop start/end, turn start/end,
// message start/end, tool start/end, and error. A [Recorder] subscribes to
// that stream (via [github.com/erain/glue.Session.Subscribe] or any other
// aux event sink) and emits a span tree per prompt:
//
//	glue.agent.run                 (one trace per prompt / loop.Run)
//	├── glue.agent.turn            (one per assistant turn)
//	│   ├── glue.llm.generate      (the provider call)
//	│   └── glue.tool.<name>       (each tool invocation)
//	└── ...
//
// Spans carry GenAI-flavoured attributes (model, provider, finish reason,
// token usage) so traces drop straight into Jaeger, Honeycomb, Datadog, or
// any OTLP backend.
//
// The core loop and the root glue package stay free of OpenTelemetry
// imports: all otel dependencies live here and in cmd/glue. When no
// exporter is configured (see [NewTracerProvider]) the whole path is a
// no-op, so tracing costs nothing until a backend is wired up.
package tracing
