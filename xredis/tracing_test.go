package xredis

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/jupiter-eng/xone/xonetest"
)

// 用了 xredis 就有链路，使用者不用另外 import xtrace：xredis.go 替他 import 了。
// 先把全局的 TracerProvider 换回 noop，免得别的测试装过的 provider 让这里白白通过
func TestTracingWorksWithoutImportingXtrace(t *testing.T) {
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(noop.NewTracerProvider())
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")
	xonetest.StartHooks(t)

	_, span := otel.Tracer("test").Start(context.Background(), "op")
	defer span.End()
	if !span.SpanContext().IsValid() {
		t.Fatal("用了 xredis 就该有链路，拿到的却是 noop 的 Span：xredis 没把 xtrace 带进来")
	}
}
