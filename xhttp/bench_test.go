package xhttp

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// stubPool 代替连接池：不走网络，直接回一个 200，量出来的只剩出站这一路包装的开销
type stubPool struct{}

func (stubPool) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("ok")),
		Request:    r,
	}, nil
}

// benchCall 按 New 的装配方式建 client，只把连接池换成桩，然后反复发同一个 GET
func benchCall(b *testing.B, cfg Config) {
	client := newResty(&http.Client{Transport: traced(cfg, stubPool{}), Timeout: cfg.Timeout}, cfg.Log)
	if cfg.Metric {
		installMetrics(client, newDurationHistogram())
	}

	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := client.R().SetContext(ctx).Get("http://svc.internal/users/42?token=t"); err != nil {
			b.Fatal(err)
		}
	}
}

// withTracer 换上一个真实的 SDK TracerProvider，采样器由调用方给
func withTracer(b *testing.B, s sdktrace.Sampler) {
	old := otel.GetTracerProvider()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(s))
	otel.SetTracerProvider(tp)
	b.Cleanup(func() {
		otel.SetTracerProvider(old)
		_ = tp.Shutdown(context.Background())
	})
}

func BenchmarkOutbound_TraceAndMetricsOff(b *testing.B) {
	c := DefaultConfig()
	c.Trace, c.Metric = false, false
	benchCall(b, c)
}

func BenchmarkOutbound_MetricsOnly(b *testing.B) {
	c := DefaultConfig()
	c.Trace = false
	benchCall(b, c)
}

func BenchmarkOutbound_Defaults_NotSampled(b *testing.B) {
	withTracer(b, sdktrace.NeverSample())
	benchCall(b, DefaultConfig())
}

func BenchmarkOutbound_Defaults_Sampled(b *testing.B) {
	withTracer(b, sdktrace.AlwaysSample())
	benchCall(b, DefaultConfig())
}
