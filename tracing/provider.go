package tracing

import (
	"context"
	"os"
	"strings"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// DefaultServiceName tags spans whose service name is not overridden by
// OTEL_SERVICE_NAME.
const DefaultServiceName = "glue"

// ScopeName is the instrumentation scope (otel "tracer name") for spans
// emitted by glue. Backends group spans by this name.
const ScopeName = "github.com/erain/glue"

// globalEnabled records whether Setup installed a real exporter, so
// session helpers can cheaply skip span work when tracing is off.
var globalEnabled atomic.Bool

// Enabled reports whether tracing was configured by a successful Setup.
func Enabled() bool { return globalEnabled.Load() }

// Setup builds a tracer provider from the OTEL_* environment and, when one
// is configured, registers it as the global OpenTelemetry provider and
// marks tracing enabled. It returns a shutdown func that flushes buffered
// spans, or a nil shutdown when tracing is not configured (the caller
// should guard against nil). See [NewTracerProvider] for the env contract.
func Setup(ctx context.Context) (shutdown func(context.Context) error, err error) {
	tp, err := NewTracerProvider(ctx)
	if err != nil || tp == nil {
		return nil, err
	}
	otel.SetTracerProvider(tp)
	globalEnabled.Store(true)
	return tp.Shutdown, nil
}

// NewTracerProvider builds an OpenTelemetry tracer provider from the
// standard OTEL_* environment, or returns (nil, nil) when tracing is not
// configured so the caller can skip wiring entirely.
//
// Exporter selection (OpenTelemetry conventions):
//
//	OTEL_TRACES_EXPORTER=otlp    OTLP/HTTP exporter (the default when an
//	                             OTEL_EXPORTER_OTLP[_TRACES]_ENDPOINT is set)
//	OTEL_TRACES_EXPORTER=stdout  pretty-print spans to stderr (local debug)
//	OTEL_TRACES_EXPORTER=none    disabled (also the default when nothing is set)
//
// The OTLP exporter honours all standard OTEL_EXPORTER_OTLP_* variables
// (endpoint, headers, protocol) so it points at Jaeger/Honeycomb/Datadog
// without glue-specific configuration. Service name comes from
// OTEL_SERVICE_NAME (default "glue").
//
// The returned provider is NOT registered globally; the caller decides
// whether to call otel.SetTracerProvider and is responsible for calling
// Shutdown to flush batched spans on exit.
func NewTracerProvider(ctx context.Context) (*sdktrace.TracerProvider, error) {
	exp, err := newExporter(ctx, exporterKind())
	if err != nil || exp == nil {
		return nil, err
	}

	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(attribute.String("service.name", serviceName())),
	)
	if err != nil {
		// A partial resource (e.g. an unparseable OTEL_RESOURCE_ATTRIBUTES)
		// should not defeat tracing entirely; fall back to a minimal one.
		res = resource.NewSchemaless(attribute.String("service.name", serviceName()))
	}

	return sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	), nil
}

func newExporter(ctx context.Context, kind string) (sdktrace.SpanExporter, error) {
	switch kind {
	case "none", "":
		return nil, nil
	case "stdout", "console":
		return stdouttrace.New(stdouttrace.WithWriter(os.Stderr), stdouttrace.WithPrettyPrint())
	default: // "otlp" and anything else maps to the standard OTLP/HTTP exporter
		return otlptracehttp.New(ctx)
	}
}

// exporterKind resolves the configured exporter, honouring an explicit
// OTEL_TRACES_EXPORTER and otherwise enabling OTLP whenever an endpoint is
// present. Unset with no endpoint means tracing is off.
func exporterKind() string {
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_TRACES_EXPORTER"))); v != "" {
		return v
	}
	if endpointConfigured() {
		return "otlp"
	}
	return "none"
}

func endpointConfigured() bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
}

func serviceName() string {
	if v := strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME")); v != "" {
		return v
	}
	return DefaultServiceName
}
