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

// discardExporter 收下 Span 就扔：量的是链路在请求路径上的开销，不是导出
type discardExporter struct{}

func (discardExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return nil }
func (discardExporter) Shutdown(context.Context) error                             { return nil }

// sdkTracing 换上和 xtrace 开着链路时同一形状的全局 TracerProvider / Propagator，结束时还原。
//
// 不装的话全局是 OTel 的默认：noop provider、空的 Propagator。那时 Trace 中间件里
// Start 几乎不花钱、SpanContext 无效，写 X-Trace-Id 的那一支根本不走——从前的基准
// 就是这么量的，链路一项只有 +1µs/请求，e2e 在真实进程里量到的是 +7–10µs，差了 3–7 倍。
//
// 这里和 xtrace.New 对齐：SDK provider、ParentBased(TraceIDRatioBased(1)) 全量采样、
// 批处理器；Propagator 是 TraceContext + Baggage。xtrace 另外还挂着 b3 和透传 Header，
// 前者在 xgin 里要多引一个依赖，后者在 xtrace 模块里，这两项的 Extract 不计在内。
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
// noop 那一组留着作对照：两组之差就是链路本身的开销，只看 noop 会把它低估掉。
// 实测（-benchtime=100000x -count=3，go1.25 linux/amd64，4 核 Xeon 2.1GHz，仅作量级参考）：
//
//	                 noop               SDK采样
//	1_只LogScope     1.06µs  12 allocs  （不含 Trace，同一个数）
//	2_加Trace        2.0µs   22 allocs  4.7µs  29 allocs
//	5_全量           6.6µs   28 allocs  12.4µs 35 allocs
//
// 访问日志默认记 host、proto、user_agent、bytes_in、bytes_out 这五项，实测 +1.2µs、不多分配。
// Trace 这一项 noop 下 +0.9µs，真实 SDK 下 +3.6µs，约 4 倍。真实进程里还有
// b3 / 透传 Header 的 Extract、Span 攒批导出和多核争用，e2e 量到的 +7–10µs 比这里再高一些
func runChain(b *testing.B, mw ...gin.HandlerFunc) {
	b.Run("noop", func(b *testing.B) { runReqs(b, benchEngine(mw...)) })
	b.Run("SDK采样", func(b *testing.B) {
		testkit.QuietSlog(b)
		sdkTracing(b)
		e := benchEngine(mw...)
		// 确认量到的是写了 X-Trace-Id 的那一支，不是又一次空转
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

// BenchmarkRedactBody_PlainText 认不出结构、也没有敏感词的 body：预检之后去掉换行原样记
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

// BenchmarkRedact_WholeLogEntry 盯住「请求头只被序列化一次」这件事。
// 交出序列化好的字符串会让 slog 再转义一遍，时间和分配都翻倍。
func BenchmarkRedact_WholeLogEntry(b *testing.B) {
	h := benchHeader()
	l := slog.New(slog.NewJSONHandler(io.Discard, nil))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.Info("请求完成", "请求头", RedactHeaders(h))
	}
}

// benchJSON 一份常见的下单请求体，约 230 字节，里面没有敏感字段：
// 预检之后走快路径，这是打开 WithBody 之后绝大多数请求走的那一支
var benchJSON = []byte(`{"order_id":"20260923-000123","user_id":10086,"items":[{"sku":"A-1001","qty":2,"price":"19.90"},{"sku":"B-2002","qty":1,"price":"5.00"}],"address":"上海市浦东新区张江路 88 号","remark":"工作日送货"}`)

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

// BenchmarkLog_WithRequestAndResponseBody 打开 WithBody 之后的整条路径：
// 预读请求体、截响应、两次 body 脱敏、写一行日志
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
