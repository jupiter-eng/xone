package xredis

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/jupiter-eng/xone/xonetest"
)

// Using xredis brings tracing and the caller does not have to import xtrace: xredis.go imports it for them.
// The global TracerProvider is reset to a noop first, so that a provider installed by another test cannot make this pass for nothing.
func TestTracingWorksWithoutImportingXtrace(t *testing.T) {
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(noop.NewTracerProvider())
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")
	xonetest.StartHooks(t)

	_, span := otel.Tracer("test").Start(context.Background(), "op")
	defer span.End()
	if !span.SpanContext().IsValid() {
		t.Fatal("using xredis must bring tracing, but the Span is a noop one: xredis did not pull xtrace in")
	}
}
