package middleware

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/jupiter-eng/xone/internal/testkit"
)

// benchHandler is the chain under test, ending in a handler that answers like a beego controller
// and fills the route template in as xbeego's filter does for real.
func benchHandler(mws ...func(http.Handler) http.Handler) http.Handler {
	return chain(routed("/order/:id", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}), mws...)
}

func runReqs(b *testing.B, h http.Handler) {
	testkit.QuietSlog(b)
	req := httptest.NewRequest("GET", "/order/123", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("User-Agent", "bench/1.0")
	req.Header.Set("X-Request-Id", "abc-123")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}

// discardExporter accepts spans and drops them: what is measured is what tracing costs on the
// request path, not exporting.
type discardExporter struct{}

func (discardExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return nil }
func (discardExporter) Shutdown(context.Context) error                             { return nil }

// sdkTracing installs a global TracerProvider / Propagator shaped like the one xtrace installs
// with tracing on, and restores them afterwards.
//
// Without it the globals are OTel's defaults: a noop provider and an empty Propagator. Start in
// the Trace middleware then costs almost nothing, the SpanContext is invalid and the branch
// writing X-Trace-Id never runs -- which measures tracing at a fraction of what it really costs.
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
// The noop group stays as the baseline: the difference between the two is what tracing itself
// costs, and looking at noop alone understates it.
//
// Measured (-benchtime=100000x -count=3, go1.27 darwin/arm64 on an Apple M5; orders of magnitude
// only, and not comparable with the numbers in xgin's benchmarks, which were taken on a 4-core
// Xeon):
//
//	                        noop               SDK sampled
//	Chain_0_Bare            0.30µs  10 allocs  (no middleware at all)
//	Chain_1_CaptureOnly     0.37µs  14 allocs  (the recorder and the request state)
//	Chain_2_PlusLogScope    0.43µs  17 allocs
//	Chain_3_PlusTrace       0.69µs  27 allocs  1.7µs  34 allocs
//	Chain_4_PlusLog         1.60µs  29 allocs  3.1µs  36 allocs
//	Chain_5_PlusMetric      1.77µs  30 allocs  3.3µs  37 allocs
//	Chain_6_Full            1.78µs  30 allocs  3.4µs  37 allocs
//
// What stands out, and is the one thing this chain pays that gin's and echo's do not: Capture
// costs 0.07µs and 4 allocations, for the writer that counts the status and the bytes. The access
// log is the expensive layer at +0.9µs, tracing costs +0.26µs under noop and +1.0µs under the real
// SDK, and Recover and Metric are close to free.
func runChain(b *testing.B, mws ...func(http.Handler) http.Handler) {
	b.Run("noop", func(b *testing.B) { runReqs(b, benchHandler(mws...)) })
	b.Run("SDK_sampled", func(b *testing.B) {
		testkit.QuietSlog(b)
		sdkTracing(b)
		h := benchHandler(mws...)
		// Confirm what was measured is the branch that writes X-Trace-Id, not another no-op run.
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/order/123", nil))
		if w.Header().Get(TraceIDHeader) == "" {
			b.Fatal("SDK tracing is installed but the response carries no " + TraceIDHeader)
		}
		runReqs(b, h)
	})
}

func BenchmarkChain_0_Bare(b *testing.B)        { runReqs(b, benchHandler()) }
func BenchmarkChain_1_CaptureOnly(b *testing.B) { runReqs(b, benchHandler(Capture(nil))) }
func BenchmarkChain_2_PlusLogScope(b *testing.B) {
	runReqs(b, benchHandler(Capture(nil), LogScope()))
}
func BenchmarkChain_3_PlusTrace(b *testing.B) { runChain(b, Capture(nil), LogScope(), Trace()) }
func BenchmarkChain_4_PlusLog(b *testing.B)   { runChain(b, Capture(nil), LogScope(), Trace(), Log()) }
func BenchmarkChain_5_PlusMetric(b *testing.B) {
	runChain(b, Capture(nil), LogScope(), Trace(), Log(), Metric())
}
func BenchmarkChain_6_Full(b *testing.B) {
	runChain(b, Capture(nil), LogScope(), Trace(), Log(), Metric(), Recover(nil))
}

// BenchmarkLog_WithRequestAndResponseBody covers the whole path with WithBody on: buffering the
// request body, capturing the response, masking both bodies and writing one log line.
func BenchmarkLog_WithRequestAndResponseBody(b *testing.B) {
	testkit.QuietSlog(b)
	h := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(benchJSON)
	}), Capture(nil), Log(WithBody(true, true)))

	req := httptest.NewRequest("POST", "/order", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "bench/1.0")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req.Body = io.NopCloser(bytes.NewReader(benchJSON))
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}

// benchJSON is a common order request body of about 230 bytes with no sensitive fields.
var benchJSON = []byte(`{"order_id":"20260923-000123","user_id":10086,"items":[{"sku":"A-1001","qty":2,"price":"19.90"},{"sku":"B-2002","qty":1,"price":"5.00"}],"address":"Friedrichstrasse 88, 10117 Berlin","remark":"weekday delivery"}`)

// BenchmarkRecorder_Write is the one thing beego's chain pays that gin's and echo's do not: their
// frameworks hand the integration a writer that already counts the status and the bytes, while a
// plain http.Handler has to have one put in front of it.
func BenchmarkRecorder_Write(b *testing.B) {
	body := []byte("hello world")
	h := chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}), Capture(nil))
	req := httptest.NewRequest("GET", "/x", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}
