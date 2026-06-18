# ADR-0018: OpenTelemetry Tracing for the Agent Loop

## Status

Accepted (2026-06-18).

## Context

Glue had no observability: a run was a black box once it left the
terminal. Diagnosing a slow turn, an unexpected tool failure, or a
runaway goal loop meant re-reading stdout or the JSON event stream by
hand. There was no way to answer "where did the latency go?", "how many
tokens did this objective burn, per iteration?", or "which tool errored
in last night's unattended run?".

Research into Vercel's **Eve** agent framework (June 2026) flagged
first-class tracing as the cleanest idea to borrow: Eve emits a span per
model call and per tool invocation over standard OpenTelemetry, so runs
drop into Jaeger / Honeycomb / Datadog with no bespoke tooling. Glue
already has the ideal substrate — `loop.Run` emits a strictly-ordered
`Event` stream (loop/turn/message/tool start-end, error), and
`Session.Subscribe(func(Event))` is a public, additive, fan-out sink.
Nothing in the hot path needed to change to observe it.

## Decision

Add a dependency-isolated `tracing/` package that translates the loop
event stream into an OpenTelemetry span tree, one trace per prompt:

```
glue.agent.run                 (one trace per loop.Run / prompt)
├── glue.agent.turn            (one per assistant turn)
│   ├── glue.llm.generate      (the provider call; model, tokens, finish reason)
│   └── glue.tool.<name>       (each tool call; is_error)
└── ...
```

Key choices:

- **Event-stream translation, not loop instrumentation.** A `Recorder`
  consumes the existing `Event` stream and maintains the span tree
  (root/turn/message + a map keyed by tool-call id so parallel tool
  calls close against the right span). The `loop` and root `glue`
  packages stay free of OpenTelemetry imports; all otel deps live in
  `tracing/` and `cmd/glue`. This mirrors how `WithStreamWriter` /
  `WithToolLogger` already attach to the event stream.

- **GenAI semantic conventions.** Spans carry `gen_ai.*` attributes
  (request/response model, `gen_ai.system` = provider, finish reason,
  input/output/total tokens from `Message.Usage`) plus `glue.*` detail
  (session id, tool name/call-id/is_error, turn index), so existing LLM
  dashboards light up without remapping.

- **Standard env contract, off by default.** `NewTracerProvider` /
  `Setup` read the standard `OTEL_*` environment
  (`OTEL_TRACES_EXPORTER` = `otlp` | `stdout` | `none`,
  `OTEL_EXPORTER_OTLP_*`, `OTEL_SERVICE_NAME`). With nothing configured
  the whole path is a no-op (`Enabled()` is false, `NewSessionRecorder`
  returns a nil-tracer recorder), so there is zero overhead and zero
  config until a backend is wired up.

- **Resilient span lifecycle.** The recorder closes any open spans on
  `EventLoopEnd`, on a fresh `EventLoopStart` (a prior crashed run can't
  leak spans into the next prompt), and on `EventError` (which also
  flags the root span so failed traces are filterable at the top). This
  matters because the loop returns early on cancellation, provider
  error, and max-turns without emitting the matching end events.

- **Wired at the host edge.** `cmd/glue` calls `tracing.Setup` once in
  `main` (with a flush-on-exit shutdown) and attaches a recorder via
  `Session.Subscribe` in the `run`, TUI, and `goal` paths. The goal loop
  gains a `GoalSpec.OnSession` seam so the host can observe its internal
  planner/maker/checker sessions without the core loop depending on any
  tracing backend.

## Consequences

- glue runs are now observable end-to-end against any OTLP backend with
  a single env var; `OTEL_TRACES_EXPORTER=stdout` gives a zero-backend
  local view.
- The module now depends on the OpenTelemetry SDK and OTLP/stdout
  exporters. They are only imported by `tracing/` and `cmd/glue`; the
  embeddable `glue` / `loop` libraries remain otel-free at the type
  level (the `glue` package references tracing only through the optional
  `GoalSpec.OnSession` hook, which is an ordinary `func(*Session)`).
- Provider HTTP-level spans are **not** yet nested under
  `glue.llm.generate` — context is not threaded into `Provider.Stream`.
  The event-stream design records accurate wall-clock per call; deeper
  propagation (and instrumenting the provider HTTP clients) is a
  follow-up.
- The daemon (`serve`) path is not yet traced; it can subscribe a
  recorder per session the same way when prioritized.
