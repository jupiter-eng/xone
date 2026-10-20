package middleware

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/jupiter-eng/xone/internal/testkit"
)

func benchEngine(mw ...gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	e := gin.New()
	e.Use(mw...)
	e.GET("/order/:id", func(c *gin.Context) { c.String(200, "ok") })
	return e
}

func runReqs(b *testing.B, e *gin.Engine) {
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
// with tracing on, and restores them afterwards.
//
// Without it the globals are OTel's defaults: a noop provider and an empty Propagator. Start in
// the Trace middleware then costs almost nothing, the SpanContext is invalid and the branch
// writing X-Trace-Id never runs — which is how the old benchmarks measured, putting tracing at
// +1µs per request while e2e measured +7-10µs in a real process, three to seven times more.
//
// This matches xtrace.New: an SDK provider, ParentBased(TraceIDRatioBased(1)) sampling
// everything, and a batch processor; the Propagator is TraceContext + Baggage. xtrace also
// installs b3 and forwarded headers, the former needing one more dependency inside xgin and the
// latter living in the xtrace module, so neither Extract is counted here.
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
// Measured (-benchtime=100000x -count=3, go1.25 linux/amd64, a 4-core Xeon at 2.1GHz; orders of
// magnitude only):
//
//	                      noop               SDK sampled
//	Chain_1_LogScopeOnly  1.06µs  12 allocs  (no Trace, the same number)
//	Chain_2_PlusTrace     2.0µs   22 allocs  4.7µs  29 allocs
//	Chain_5_Full          6.6µs   28 allocs  12.4µs 35 allocs
//
// The access log records host, proto, user_agent, bytes_in and bytes_out by default, measured at
// +1.2µs with no extra allocations.
// Tracing costs +0.9µs under noop and +3.6µs under the real SDK, about four times as much. A real
// process additionally does the b3 and forwarded-header Extract, batches spans for export and
// contends across cores, which is why e2e measures +7-10µs, higher still than here.
func runChain(b *testing.B, mw ...gin.HandlerFunc) {
	b.Run("noop", func(b *testing.B) { runReqs(b, benchEngine(mw...)) })
	b.Run("SDK_sampled", func(b *testing.B) {
		testkit.QuietSlog(b)
		sdkTracing(b)
		e := benchEngine(mw...)
		// Confirm what was measured is the branch that writes X-Trace-Id, not another no-op run.
		w := httptest.NewRecorder()
		e.ServeHTTP(w, httptest.NewRequest("GET", "/order/123", nil))
		if w.Header().Get(TraceIDHeader) == "" {
			b.Fatal("SDK tracing is installed but the response carries no " + TraceIDHeader)
		}
		runReqs(b, e)
	})
}

func BenchmarkChain_0_BareGin(b *testing.B)      { runReqs(b, benchEngine()) }
func BenchmarkChain_1_LogScopeOnly(b *testing.B) { runReqs(b, benchEngine(LogScope())) }
func BenchmarkChain_2_PlusTrace(b *testing.B)    { runChain(b, LogScope(), Trace()) }
func BenchmarkChain_3_PlusLog(b *testing.B)      { runChain(b, LogScope(), Trace(), Log()) }
func BenchmarkChain_4_PlusMetric(b *testing.B)   { runChain(b, LogScope(), Trace(), Log(), Metric()) }
func BenchmarkChain_5_Full(b *testing.B) {
	runChain(b, LogScope(), Trace(), Log(), Metric(), Recover(nil))
}

func BenchmarkRedactHeaders(b *testing.B) {
	h := http.Header{}
	h.Set("Authorization", "Bearer secret")
	h.Set("User-Agent", "bench/1.0")
	h.Set("X-Request-Id", "abc-123")
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = RedactHeaders(h)
	}
}

// BenchmarkRedactBody_PlainText is a body with no recognizable structure and no sensitive words:
// after the pre-check it is recorded as is, with newlines stripped.
func BenchmarkRedactBody_PlainText(b *testing.B) {
	body := []byte("order 20260923-000123 shipped to warehouse 7, eta 2 days")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = RedactBody(body, "text/plain")
	}
}

func benchHeader() http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer secret")
	h.Set("User-Agent", "bench/1.0")
	h.Set("X-Request-Id", "abc-123")
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	return h
}

// BenchmarkRedact_WholeLogEntry pins that the request headers are serialized exactly once.
// Handing over a serialized string would have slog escape it again, doubling both time and
// allocations.
func BenchmarkRedact_WholeLogEntry(b *testing.B) {
	h := benchHeader()
	l := slog.New(slog.NewJSONHandler(io.Discard, nil))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.Info("request completed", "request_headers", RedactHeaders(h))
	}
}

// benchJSON is a common order request body of about 230 bytes with no sensitive fields: after the
// pre-check it takes the fast path, which is the branch the vast majority of requests take once
// WithBody is on.
var benchJSON = []byte(`{"order_id":"20260923-000123","user_id":10086,"items":[{"sku":"A-1001","qty":2,"price":"19.90"},{"sku":"B-2002","qty":1,"price":"5.00"}],"address":"Friedrichstrasse 88, 10117 Berlin","remark":"weekday delivery"}`)

func BenchmarkRedactBody_JSONWithoutSensitiveFields(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = RedactBody(benchJSON, "application/json")
	}
}

func BenchmarkRedactBody_JSONWithSensitiveFields(b *testing.B) {
	body := []byte(`{"user":"alice","password":"hunter2","items":[{"sku":"A-1001","qty":2}]}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = RedactBody(body, "application/json")
	}
}

// BenchmarkLog_WithRequestAndResponseBody covers the whole path with WithBody on: buffering the
// request body, capturing the response, masking both bodies and writing one log line.
func BenchmarkLog_WithRequestAndResponseBody(b *testing.B) {
	testkit.QuietSlog(b)
	gin.SetMode(gin.ReleaseMode)
	e := gin.New()
	e.Use(Log(WithBody(true, true)))
	e.POST("/order", func(c *gin.Context) {
		_, _ = io.Copy(io.Discard, c.Request.Body)
		c.Data(200, "application/json", benchJSON)
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
