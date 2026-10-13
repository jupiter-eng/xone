package main

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/jupiter-eng/xone/xmetric"
)

// countingExporter 只计数、不上报的 exporter，压测用（Service.SpanDiscard）。
//
// 挂在 BatchSpanProcessor 后面，量的是文档推荐的那条导出管线
// （xtrace.AddSpanProcessor(sdktrace.NewBatchSpanProcessor(exp))）本身的开销，
// 而不是 SpanFile 那个同步写文件的 exporter——那是 e2e 自己的开销。
//
// 每个 Span 计进 e2e_spans_exported_total{kind, route}：route 取服务端 Span 的
// http.route，其余是空串。压测拿它和 e2e_http_requests_total 对账：
// 两者对不上，就是有 Span 在导出之前被丢掉了（队列满了 BatchSpanProcessor 会静默丢）
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
