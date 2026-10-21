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

// stubPool stands in for the connection pool: no network, a plain 200 back, so what is measured is only the wrapping on the outbound path.
type stubPool struct{}

func (stubPool) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("ok")),
		Request:    r,
	}, nil
}

// benchCall builds a client the way New assembles one, with only the pool replaced by the stub, and then sends the same GET repeatedly.
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

// withTracer installs a real SDK TracerProvider, with the sampler given by the caller.
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
