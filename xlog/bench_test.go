package xlog

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

func benchLogger(b *testing.B) *slog.Logger {
	h := slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(newCtxHandler(h))
}

func BenchmarkHandle_Bare(b *testing.B) {
	l := benchLogger(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.InfoContext(ctx, "请求完成", "状态", 200)
	}
}

// 和 BenchmarkHandle_Bare 对照：框架默认的 4 个身份字段加上 3 个 Fields
func BenchmarkHandle_WithIdentityFields(b *testing.B) {
	base := []slog.Attr{slog.String("service", "order-api"), slog.String("version", "v1.2.3"),
		slog.String("hostname", "order-api-7d9f5c8b6-x2k4q"), slog.Int("pid", 1)}
	l := withStatic(benchLogger(b), base, map[string]string{"pod": "order-api-7d9f5c8b6-x2k4q", "node": "node-3", "zone": "az1"})
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.InfoContext(ctx, "请求完成", "状态", 200)
	}
}

func BenchmarkHandle_TraceExtractorButNoSpanInCtx(b *testing.B) {
	SetTraceExtractor(func(context.Context) (string, string) { return "", "" })
	b.Cleanup(func() { SetTraceExtractor(nil) })
	l := benchLogger(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.InfoContext(ctx, "请求完成", "状态", 200)
	}
}

func BenchmarkHandle_WithTraceAndScope(b *testing.B) {
	SetTraceExtractor(func(context.Context) (string, string) {
		return "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	})
	b.Cleanup(func() { SetTraceExtractor(nil) })
	l := benchLogger(b)
	ctx := CtxWithScope(context.Background())
	AddKV(ctx, "uid", 12345)
	AddKV(ctx, "route", "/order")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.InfoContext(ctx, "请求完成", "状态", 200)
	}
}

func BenchmarkHandle_SlowPathWithGroup(b *testing.B) {
	SetTraceExtractor(func(context.Context) (string, string) {
		return "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	})
	b.Cleanup(func() { SetTraceExtractor(nil) })
	l := benchLogger(b).With("svc", "demo").WithGroup("http")
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.InfoContext(ctx, "请求完成", "状态", 200)
	}
}
