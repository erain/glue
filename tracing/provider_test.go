package tracing

import (
	"context"
	"testing"
)

func TestExporterKind(t *testing.T) {
	cases := []struct {
		name           string
		exporter       string
		endpoint       string
		tracesEndpoint string
		want           string
	}{
		{name: "unset is off", want: "none"},
		{name: "explicit none", exporter: "none", want: "none"},
		{name: "explicit stdout", exporter: "stdout", want: "stdout"},
		{name: "explicit otlp", exporter: "otlp", want: "otlp"},
		{name: "endpoint implies otlp", endpoint: "http://localhost:4318", want: "otlp"},
		{name: "traces endpoint implies otlp", tracesEndpoint: "http://localhost:4318/v1/traces", want: "otlp"},
		{name: "explicit beats endpoint", exporter: "stdout", endpoint: "http://localhost:4318", want: "stdout"},
		{name: "case-insensitive", exporter: "STDOUT", want: "stdout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OTEL_TRACES_EXPORTER", tc.exporter)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", tc.endpoint)
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", tc.tracesEndpoint)
			if got := exporterKind(); got != tc.want {
				t.Errorf("exporterKind() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestServiceName(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "")
	if got := serviceName(); got != DefaultServiceName {
		t.Errorf("default service name = %q, want %q", got, DefaultServiceName)
	}
	t.Setenv("OTEL_SERVICE_NAME", "peggy")
	if got := serviceName(); got != "peggy" {
		t.Errorf("service name = %q, want peggy", got)
	}
}

func TestNewTracerProviderDisabledByDefault(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	tp, err := NewTracerProvider(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tp != nil {
		t.Fatalf("expected nil provider when tracing is unconfigured, got %v", tp)
	}
}

func TestNewTracerProviderStdout(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "stdout")
	tp, err := NewTracerProvider(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tp == nil {
		t.Fatal("expected a provider for the stdout exporter")
	}
	_ = tp.Shutdown(context.Background())
}

func TestSetupDisabledReturnsNilShutdown(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	shutdown, err := Setup(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shutdown != nil {
		t.Error("expected nil shutdown when tracing is disabled")
	}
	if Enabled() {
		t.Error("Enabled() should be false when Setup did not configure an exporter")
	}
}
