package xgorm

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/jupiter-eng/xone/xonetest"
)

// Using xgorm means tracing is there, without the caller importing xtrace: xgorm.go imports it
// for them. The global TracerProvider is reset to noop first, so that a provider installed by
// another test cannot make this pass for free.
func TestTracingWorksWithoutImportingXtrace(t *testing.T) {
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(noop.NewTracerProvider())
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")
	xonetest.StartHooks(t)

	_, span := otel.Tracer("test").Start(context.Background(), "op")
	defer span.End()
	if !span.SpanContext().IsValid() {
		t.Fatal("using xgorm should give tracing, but the span is a noop one: xgorm did not pull xtrace in")
	}
}
