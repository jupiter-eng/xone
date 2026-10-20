package middleware

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/jupiter-eng/xone/internal/testkit"
)

// The same requests and the same chain as xgin/middleware/bench_test.go, so the numbers compare
// directly. The benchmark for masking itself lives on xgin's side (the implementation is in
// internal/web and is shared).

func benchEcho(mw ...echo.MiddlewareFunc) *echo.Echo {
	e := echo.New()
	e.Use(mw...)
	e.GET("/order/:id", func(c echo.Context) error { return c.String(200, "ok") })
	return e
}

func runReqs(b *testing.B, e *echo.Echo) {
	testkit.QuietSlog(b)
	req := httptest.NewRequest("GET", "/order/123", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("User-Agent", "bench/1.0")
	req.Header.Set("X-Request-Id", "abc-123")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.ServeHTTP(httptest.NewRecorder(), req)
	}
}

// discardExporter accepts spans and drops them: what is measured is what tracing costs on the
// request path, not exporting.
type discardExporter struct{}

func (discardExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return nil }
func (discardExporter) Shutdown(context.Context) error                             { return nil }

// sdkTracing installs a global TracerProvider / Propagator shaped like the one xtrace installs
// with tracing on, and restores them afterwards. The reason is xgin's: under a noop provider
// Trace costs almost nothing, and the numbers come out several times too low.
func sdkTracing(b *testing.B) {
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(1))),
		sdktrace.WithBatcher(discardExporter{}),
	)
	oldTP, oldProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	b.Cleanup(func() {
		otel.SetTracerProvider(oldTP)
		otel.SetTextMapPropagator(oldProp)
		_ = tp.Shutdown(context.Background())
	})
}

// runChain measures the same chain twice, once with noop tracing and once with the real SDK.
//
// Measured (-benchtime=100000x -count=3, go1.25 linux/amd64, interleaved with xgin's on the same
// machine; orders of magnitude only):
//
//	                       noop                SDK sampled          xgin, same row (noop / SDK sampled)
//	Chain_0_BareEcho       0.96µs  10 allocs                        0.91µs  9 allocs
//	Chain_1_LogScopeOnly   1.23µs  14 allocs                        1.34µs 12 allocs
//	Chain_2_PlusTrace      2.36µs  25 allocs   5.7µs  32 allocs     2.32µs 22 allocs / 5.8µs  29 allocs
//	Chain_5_Full           6.12µs  32 allocs   10.2µs 39 allocs     5.98µs 28 allocs / 10.3µs 35 allocs
//
// The durations are within noise of each other; the extra 1-4 allocations per request were not
// chased down, since the numbers never reached the point of being worth a code change.
func runChain(b *testing.B, mw ...echo.MiddlewareFunc) {
	b.Run("noop", func(b *testing.B) { runReqs(b, benchEcho(mw...)) })
	b.Run("SDK_sampled", func(b *testing.B) {
		testkit.QuietSlog(b)
		sdkTracing(b)
		e := benchEcho(mw...)
		// Confirm what was measured is the branch that writes X-Trace-Id, not another no-op run.
		w := httptest.NewRecorder()
		e.ServeHTTP(w, httptest.NewRequest("GET", "/order/123", nil))
		if w.Header().Get(TraceIDHeader) == "" {
			b.Fatal("SDK tracing is installed but the response carries no " + TraceIDHeader)
		}
		runReqs(b, e)
	})
}

func BenchmarkChain_0_BareEcho(b *testing.B)     { runReqs(b, benchEcho()) }
func BenchmarkChain_1_LogScopeOnly(b *testing.B) { runReqs(b, benchEcho(LogScope())) }
func BenchmarkChain_2_PlusTrace(b *testing.B)    { runChain(b, LogScope(), Trace()) }
func BenchmarkChain_3_PlusLog(b *testing.B)      { runChain(b, LogScope(), Trace(), Log()) }
func BenchmarkChain_4_PlusMetric(b *testing.B)   { runChain(b, LogScope(), Trace(), Log(), Metric()) }
func BenchmarkChain_5_Full(b *testing.B) {
	runChain(b, LogScope(), Trace(), Log(), Metric(), Recover(nil))
}

// benchJSON is the same order request body as xgin's; keep the two identical so the numbers
// stay comparable.
var benchJSON = []byte(`{"order_id":"20260923-000123","user_id":10086,"items":[{"sku":"A-1001","qty":2,"price":"19.90"},{"sku":"B-2002","qty":1,"price":"5.00"}],"address":"Friedrichstrasse 88, 10117 Berlin","remark":"weekday delivery"}`)

// BenchmarkLog_WithRequestAndResponseBody covers the whole path with WithBody on: buffering the
// request body, capturing the response, masking both bodies and writing one log line.
func BenchmarkLog_WithRequestAndResponseBody(b *testing.B) {
	testkit.QuietSlog(b)
	e := echo.New()
	e.Use(Log(WithBody(true, true)))
	e.POST("/order", func(c echo.Context) error {
		_, _ = io.Copy(io.Discard, c.Request().Body)
		return c.Blob(200, "application/json", benchJSON)
	})
	req := httptest.NewRequest("POST", "/order", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "bench/1.0")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req.Body = io.NopCloser(bytes.NewReader(benchJSON))
		e.ServeHTTP(httptest.NewRecorder(), req)
	}
}
