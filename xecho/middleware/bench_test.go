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

// 和 xgin/middleware/bench_test.go 同一套请求、同一条链，两边的数可以直接对照。
// 脱敏本身的基准在 xgin 那边（实现在 internal/web，两边共用）

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

// discardExporter 收下 Span 就扔：量的是链路在请求路径上的开销，不是导出
type discardExporter struct{}

func (discardExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return nil }
func (discardExporter) Shutdown(context.Context) error                             { return nil }

// sdkTracing 换上和 xtrace 开着链路时同一形状的全局 TracerProvider / Propagator，结束时还原。
// 理由同 xgin 的那一份：noop provider 下 Trace 几乎不花钱，量出来的数会低估好几倍
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

// runChain 同一条链各量一遍 noop 和真实 SDK 两种链路。
//
// 实测（-benchtime=100000x -count=3，go1.25 linux/amd64，同一台机器上和 xgin 的交替跑，仅作量级参考）：
//
//	                 noop                SDK采样              xgin 同一项（noop / SDK采样）
//	0_裸框架         0.96µs  10 allocs                        0.91µs  9 allocs
//	1_只LogScope     1.23µs  14 allocs                        1.34µs 12 allocs
//	2_加Trace        2.36µs  25 allocs   5.7µs  32 allocs     2.32µs 22 allocs / 5.8µs  29 allocs
//	5_全量           6.12µs  32 allocs   10.2µs 39 allocs     5.98µs 28 allocs / 10.3µs 35 allocs
//
// 耗时两边在误差之内；每请求多出的 1–4 次分配没有细查，数字没有到值得为它改代码的地步
func runChain(b *testing.B, mw ...echo.MiddlewareFunc) {
	b.Run("noop", func(b *testing.B) { runReqs(b, benchEcho(mw...)) })
	b.Run("SDK采样", func(b *testing.B) {
		testkit.QuietSlog(b)
		sdkTracing(b)
		e := benchEcho(mw...)
		// 确认量到的是写了 X-Trace-Id 的那一支，不是又一次空转
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

// benchJSON 和 xgin 那边同一份下单请求体
var benchJSON = []byte(`{"order_id":"20260923-000123","user_id":10086,"items":[{"sku":"A-1001","qty":2,"price":"19.90"},{"sku":"B-2002","qty":1,"price":"5.00"}],"address":"上海市浦东新区张江路 88 号","remark":"工作日送货"}`)

// BenchmarkLog_WithRequestAndResponseBody 打开 WithBody 之后的整条路径：
// 预读请求体、截响应、两次 body 脱敏、写一行日志
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
