package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/jupiter-eng/xone/internal/web"
	"github.com/jupiter-eng/xone/xgin/internal/peer"
)

const (
	tracerName = "github.com/jupiter-eng/xone/xgin"

	// TraceIDHeader 响应里回带的链路标识头，方便从一次调用直接跳到链路
	TraceIDHeader = "X-Trace-Id"
)

// Trace 为每个请求开一个服务端 Span，并接上上游传来的链路。
//
// 透传 Header（XTrace.ForwardHeaders）和 baggage 只收可信对端发来的值，可信与否由 xgin
// 按 XGin.TrustedProxies 判断直连的对端。单独用本中间件、没经过 xgin 装配时
// 没人做这个判断，一律当作不可信。链路标识（traceparent 等）不受影响。
func Trace() gin.HandlerFunc {
	return func(c *gin.Context) {
		route := routeOf(c)

		// 每次都取当前的全局 Propagator：构造中间件时链路可能还没初始化
		ctx := otel.GetTextMapPropagator().Extract(c.Request.Context(), inbound(c))

		// 方法和指标一样收敛到固定集合：它是个自由 token，照抄进 Span 名的话
		// 谁都能发 CUSTOM1、CUSTOM2 把链路后端的 Span 名撑爆。
		// 原始值放进 method_original（OTel 语义约定里的写法），排查时看得到
		method := web.NormalizeMethod(c.Request.Method)
		attrs := []attribute.KeyValue{
			attribute.String("http.request.method", method),
			attribute.String("http.route", route),
			attribute.String("url.path", c.Request.URL.Path),
		}
		if method != c.Request.Method {
			attrs = append(attrs, attribute.String("http.request.method_original", c.Request.Method))
		}

		ctx, span := otel.Tracer(tracerName).Start(ctx, method+" "+route,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attrs...),
		)
		// 状态码在 defer 里记：以 http.ErrAbortHandler 中止的请求是带着 panic
		// 穿过这一层的，写在 c.Next() 后面的代码根本走不到
		defer func() {
			st := status(c, ctx)
			span.SetAttributes(attribute.Int("http.response.status_code", st))
			// 只有 5xx 和中止算服务端的错。4xx 是客户端传错了，标成错误会让
			// 链路里满屏是「错误」，真正的故障反而看不出来；客户端走了的 499 同理
			switch {
			case aborted(c):
				span.SetStatus(codes.Error, "handler aborted")
			case st >= 500:
				span.SetStatus(codes.Error, http.StatusText(st))
			}
			if len(c.Errors) > 0 {
				span.SetAttributes(attribute.String("gin.errors", web.RedactText(c.Errors.String())))
			}
			span.End()
		}()

		c.Request = c.Request.WithContext(ctx)

		// 必须在 c.Next() 之前写：响应一旦开始发送，header 就改不动了
		if sc := span.SpanContext(); sc.IsValid() {
			c.Header(TraceIDHeader, sc.TraceID().String())
		}

		c.Next()
	}
}

// Propagate 只接上上游传来的链路标识、baggage 和透传 Header，不开 Span。
//
// XGin.Trace 关掉时 xgin 装的是它而不是 Trace：Trace 只管 Span，
// 上游的 traceparent、X-Request-Id 这些照样要能带给下游、进日志，
// 不该因为这一跳不记 Span 就断掉。可信对端的规则与 Trace 相同。
func Propagate() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request = c.Request.WithContext(otel.GetTextMapPropagator().Extract(c.Request.Context(), inbound(c)))
		c.Next()
	}
}

// inbound 取入站请求头作 carrier，对端可信时带上 TrustedPeer 这个记号。
//
// xtrace 靠 carrier 上的 TrustedPeer() 知道「发来透传 Header 的对端是不是自己人」——
// 它不认识 xgin，接请求的这一层才知道对端是谁。约定写在
// xtrace.HeaderPropagator.Extract 上，两边各有测试钉着，example 里还有一条端到端的。
func inbound(c *gin.Context) propagation.TextMapCarrier {
	h := propagation.HeaderCarrier(c.Request.Header)
	if c.GetBool(peer.TrustedKey) {
		return trustedCarrier{h}
	}
	return h
}

// trustedCarrier 可信对端发来的请求头。
//
// 只有一个 map 字段，装进接口时不分配——链路中间件每个请求都走这里
type trustedCarrier struct{ propagation.HeaderCarrier }

// TrustedPeer 见 inbound
func (trustedCarrier) TrustedPeer() bool { return true }
