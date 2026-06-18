# Tracing glue runs with OpenTelemetry

Glue emits OpenTelemetry traces for every agent run, so you can see where
latency, tokens, and tool failures actually go — in Jaeger, Honeycomb,
Datadog, Grafana Tempo, or any OTLP-compatible backend.

Tracing is **off until you configure an exporter**, so there is zero
overhead by default. See [ADR-0018](adr/0018-otel-tracing.md) for the
design.

## The span tree

Each prompt (one `loop.Run`) is one trace:

```
glue.agent.run                 root span — one per prompt
├── glue.agent.turn            one per assistant turn
│   ├── glue.llm.generate      the provider call
│   └── glue.tool.<name>       each tool invocation
└── glue.agent.turn
    └── ...
```

Attributes follow the OpenTelemetry GenAI semantic conventions where
they apply, so existing LLM dashboards work unchanged:

| Span | Attributes |
| --- | --- |
| `glue.agent.run` | `glue.session.id` |
| `glue.agent.turn` | `glue.turn.index` |
| `glue.llm.generate` | `gen_ai.request.model`, `gen_ai.response.model`, `gen_ai.system` (provider), `gen_ai.response.finish_reason`, `gen_ai.usage.input_tokens` / `output_tokens` / `total_tokens`, `glue.usage.cache_read_tokens` / `cache_write_tokens` |
| `glue.tool.<name>` | `glue.tool.name`, `glue.tool.call_id`, `glue.tool.is_error` |

A failed run flags both the deepest open span and the root span with an
`Error` status, so failed traces are easy to filter.

## Enabling it

Tracing reads the standard `OTEL_*` environment — no glue-specific
config.

**Send to an OTLP backend** (Jaeger, Honeycomb, Datadog, Tempo, …):

```sh
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
export OTEL_SERVICE_NAME=glue            # optional, defaults to "glue"
glue run --coding "fix the failing test"
```

Setting an endpoint is enough to enable the OTLP/HTTP exporter. All the
standard `OTEL_EXPORTER_OTLP_*` variables (headers, protocol, per-signal
endpoints) are honored.

**Print spans locally** with no backend (handy for debugging):

```sh
OTEL_TRACES_EXPORTER=stdout glue run --coding "..."
```

**Force it off** even if an endpoint is set:

```sh
OTEL_TRACES_EXPORTER=none glue run ...
```

`glue goal` traces its planner, every maker, and every checker session,
which makes unattended goal loops observable end to end.

## Embedding glue

If you embed the `glue` library, wire tracing yourself:

```go
import (
    "github.com/erain/glue"
    "github.com/erain/glue/tracing"
)

// Once at startup: configure the global tracer provider from OTEL_*.
shutdown, err := tracing.Setup(ctx)
if err == nil && shutdown != nil {
    defer shutdown(context.Background()) // flush on exit
}

// Per session: attach a recorder. No-op when tracing is disabled.
sess, _ := agent.Session(ctx, "my-session")
defer sess.Subscribe(tracing.NewSessionRecorder(sess.ID()).Handle)()
```

For the goal loop, set `GoalSpec.OnSession` to subscribe a recorder to
each internal session:

```go
spec.OnSession = func(s *glue.Session) {
    s.Subscribe(tracing.NewSessionRecorder(s.ID()).Handle)
}
```

The `loop` and core `glue` packages carry no OpenTelemetry imports; all
of it lives in `tracing/`.

## Not yet traced

- **Provider HTTP spans** are not nested under `glue.llm.generate` —
  span timing is accurate, but the underlying HTTP request is not a
  child span yet.
- **The `serve` daemon** does not attach recorders to its sessions yet.

Both are straightforward follow-ups on the same `Session.Subscribe`
seam.
