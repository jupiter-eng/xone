package main

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/jupiter-eng/xone/xmetric"
)

// countingExporter is an exporter that only counts and reports nothing, for the load tests
// (Service.SpanDiscard).
//
// Hung behind a BatchSpanProcessor, it measures the overhead of the export pipeline the
// documentation recommends (xtrace.AddSpanProcessor(sdktrace.NewBatchSpanProcessor(exp))) itself
// rather than of the SpanFile exporter writing to a file synchronously, which is e2e's own
// overhead.
//
// Every span counts into e2e_spans_exported_total{kind, route}: route is a server span's
// http.route and an empty string otherwise. A load test reconciles it against
// e2e_http_requests_total: the two disagreeing means spans were dropped before the export (a
// BatchSpanProcessor drops silently when its queue is full)
type countingExporter struct{}

func (countingExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	for _, s := range spans {
		route := ""
		for _, kv := range s.Attributes() {
			if kv.Key == attribute.Key("http.route") {
				route = kv.Value.AsString()
				break
			}
		}
		xmetric.CounterInc("spans_exported_total", xmetric.T("kind", s.SpanKind().String()), xmetric.T("route", route))
	}
	return nil
}

func (countingExporter) Shutdown(context.Context) error { return nil }
