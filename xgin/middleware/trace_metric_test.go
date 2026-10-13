package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/jupiter-eng/xone/internal/testkit"
	"github.com/jupiter-eng/xone/xgin/internal/peer"
	"github.com/jupiter-eng/xone/xmetric"
)

// recording 装一套独立的链路设施，返回取已结束 Span 的函数
func recording(t *testing.T) func() tracetest.SpanStubs {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exp)))

	oldTP, oldProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(oldTP)
		otel.SetTextMapPropagator(oldProp)
	})

	return exp.GetSpans
}

func TestTrace_StartsSpanAndReturnsTraceID(t *testing.T) {
	spans := recording(t)
	w := serve(t, get("/hello/42"), []gin.HandlerFunc{Trace()}, func(c *gin.Context) {
		c.String(200, "ok")
	})

	if w.Header().Get(TraceIDHeader) == "" {
		t.Error("应在响应头里回带 TraceID，否则从一次调用没法跳到链路")
	}
	got := spans()
	if len(got) != 1 {
		t.Fatalf("应产出一个 Span，got=%d", len(got))
	}
	// 用路由模板而不是真实路径：按真实路径命名会让 Span 名随用户数增长
	if got[0].Name != "GET /hello/:id" {
		t.Errorf("Span 名应用路由模板，got=%q", got[0].Name)
	}
	attrs := map[string]string{}
	for _, kv := range got[0].Attributes {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	if attrs["http.route"] != "/hello/:id" || attrs["url.path"] != "/hello/42" {
		t.Errorf("路由与路径属性不对，got=%v", attrs)
	}
	if attrs["http.response.status_code"] != "200" {
		t.Errorf("应记状态码，got=%v", attrs)
	}
}

func TestTrace_ContinuesUpstreamTrace(t *testing.T) {
	spans := recording(t)
	req := get("/hello")
	// 一条合法的 W3C traceparent
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")

	serve(t, req, []gin.HandlerFunc{Trace()}, func(c *gin.Context) { c.Status(200) })

	got := spans()
	if len(got) != 1 {
		t.Fatalf("应产出一个 Span，got=%d", len(got))
	}
	if id := got[0].SpanContext.TraceID().String(); id != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("应接上上游的 TraceID，got=%s", id)
	}
}

func TestTrace_OnlyServerErrorsMarkedAsError(t *testing.T) {
	// 4xx 是客户端传错了，标成错误会让链路里满屏是错，真故障反而看不出来
	for status, wantErr := range map[int]bool{200: false, 404: false, 400: false, 500: true, 503: true} {
		spans := recording(t)
		serve(t, get("/hello"), []gin.HandlerFunc{Trace()}, func(c *gin.Context) {
			c.Status(status)
		})
		got := spans()
		if len(got) != 1 {
			t.Fatalf("status=%d 应产出一个 Span", status)
		}
		isErr := got[0].Status.Code == codes.Error
		if isErr != wantErr {
			t.Errorf("status=%d 应当标错=%v，实际=%v", status, wantErr, isErr)
		}
	}
}

func TestTrace_UnmatchedRouteUsesFixedValue(t *testing.T) {
	// 用真实路径的话，扫描器随便打几个 URL 就能把链路和指标的基数撑爆
	spans := recording(t)
	e := gin.New()
	e.Use(Trace())
	e.ServeHTTP(httptest.NewRecorder(), get("/nope/whatever/123"))

	got := spans()
	if len(got) != 1 {
		t.Fatalf("应产出一个 Span，got=%d", len(got))
	}
	if strings.Contains(got[0].Name, "whatever") {
		t.Errorf("未匹配路由不该把真实路径写进 Span 名，got=%q", got[0].Name)
	}
}

func TestTrace_CustomMethodNormalizedToOTHER(t *testing.T) {
	// 与指标同一个道理：Span 名是链路后端建索引的那一维，方法是个自由 token，
	// 谁都能发 CUSTOM1、CUSTOM2。照抄的话每来一个新值就多一个 Span 名。
	// 原始方法留在 http.request.method_original 里，排查时看得到
	spans := recording(t)
	serve(t, httptest.NewRequest("CUSTOM1", "/hello", nil), []gin.HandlerFunc{Trace()},
		func(c *gin.Context) { c.Status(200) })

	got := spans()
	if len(got) != 1 {
		t.Fatalf("应产出一个 Span，got=%d", len(got))
	}
	if !strings.HasPrefix(got[0].Name, "OTHER ") {
		t.Errorf("自定义方法该收敛成 OTHER，got=%q", got[0].Name)
	}
	attrs := map[string]string{}
	for _, kv := range got[0].Attributes {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	if attrs["http.request.method"] != "OTHER" || attrs["http.request.method_original"] != "CUSTOM1" {
		t.Errorf("方法属性不对，got=%v", attrs)
	}
}

// trustProbe 只记录「交给 Extract 的 carrier 有没有声明对端可信」的 Propagator
type trustProbe struct{ got *bool }

func (p trustProbe) Extract(ctx context.Context, c propagation.TextMapCarrier) context.Context {
	t, ok := c.(interface{ TrustedPeer() bool })
	*p.got = ok && t.TrustedPeer()
	return ctx
}
func (trustProbe) Inject(context.Context, propagation.TextMapCarrier) {}
func (trustProbe) Fields() []string                                   { return nil }

func TestTrace_PeerTrustFollowsXginMarker(t *testing.T) {
	// 透传 Header 只收可信对端发来的值。可不可信由 xgin 按 TrustedProxies 判，
	// 这里只负责把判断结果交给 xtrace：没有记号（单独用本中间件）就是不可信
	old := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTextMapPropagator(old) })
	var got bool
	otel.SetTextMapPropagator(trustProbe{got: &got})

	markTrusted := func(c *gin.Context) { c.Set(peer.TrustedKey, true) }
	for name, c := range map[string]struct {
		mws  []gin.HandlerFunc
		want bool
	}{
		"单独用":                 {[]gin.HandlerFunc{Trace()}, false},
		"xgin 标了可信":           {[]gin.HandlerFunc{markTrusted, Trace()}, true},
		"Propagate 单独用":       {[]gin.HandlerFunc{Propagate()}, false},
		"Propagate xgin 标了可信": {[]gin.HandlerFunc{markTrusted, Propagate()}, true},
	} {
		got = !c.want
		serve(t, get("/hello"), c.mws, func(c *gin.Context) { c.Status(200) })
		if got != c.want {
			t.Errorf("%s：carrier 声明的可信=%v，want %v", name, got, c.want)
		}
	}
}

func TestPropagate_ContinuesUpstreamTraceWithoutSpan(t *testing.T) {
	spans := recording(t)
	req := get("/hello")
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")

	var seen string
	w := serve(t, req, []gin.HandlerFunc{Propagate()}, func(c *gin.Context) {
		seen = trace.SpanContextFromContext(c.Request.Context()).TraceID().String()
		c.Status(200)
	})
	if seen != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("handler 的 ctx 里该是上游的链路标识，got=%q", seen)
	}
	if n := len(spans()); n != 0 {
		t.Errorf("Propagate 不开 Span，got=%d 个", n)
	}
	if w.Header().Get(TraceIDHeader) != "" {
		t.Error("没开 Span 就不回带 X-Trace-Id")
	}
}

// ---- Metric ----

// withMetrics 装一套独立的指标设施
func withMetrics(t *testing.T) *xmetric.Metrics {
	t.Helper()
	m, closer, err := xmetric.New(xmetric.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closer.Close() })
	m.Install()
	return m
}

func TestMetric_RecordsCountAndDuration(t *testing.T) {
	m := withMetrics(t)
	serve(t, get("/hello/42"), []gin.HandlerFunc{Metric()}, func(c *gin.Context) {
		c.Status(201)
	})

	out := testkit.Scrape(m.Handler)
	if !strings.Contains(out, `http_requests_total{method="GET",route="/hello/:id",status="201"} 1`) {
		t.Errorf("应按方法、路由、状态码记请求数\n实际=\n%s", out)
	}
	if !strings.Contains(out, "http_request_duration_seconds_count") {
		t.Errorf("应记耗时\n实际=\n%s", out)
	}
}

func TestMetric_RouteUsesTemplate(t *testing.T) {
	// 按真实路径打标签会让时间序列随 URL 里的 id 无限增长，Prometheus 会被撑垮
	m := withMetrics(t)
	for _, p := range []string{"/hello/1", "/hello/2", "/hello/3"} {
		serve(t, get(p), []gin.HandlerFunc{Metric()}, func(c *gin.Context) { c.Status(200) })
	}

	out := testkit.Scrape(m.Handler)
	if !strings.Contains(out, `route="/hello/:id",status="200"} 3`) {
		t.Errorf("三个请求应聚合成一条序列\n实际=\n%s", out)
	}
	if strings.Contains(out, `route="/hello/1"`) {
		t.Errorf("不该把真实路径写进标签\n实际=\n%s", out)
	}
}

func TestMetric_UnmatchedRouteUsesFixedValue(t *testing.T) {
	m := withMetrics(t)
	e := gin.New()
	e.Use(Metric())
	e.ServeHTTP(httptest.NewRecorder(), get("/nope/12345"))

	out := testkit.Scrape(m.Handler)
	if !strings.Contains(out, `route="unmatched"`) {
		t.Errorf("未匹配路由应记成固定值\n实际=\n%s", out)
	}
}

func TestMetric_PanicStillCounted(t *testing.T) {
	// 不计入的话，出问题的请求会在错误率指标里凭空消失
	m := withMetrics(t)
	func() {
		defer func() { recover() }()
		serve(t, get("/hello"), []gin.HandlerFunc{Metric()}, func(c *gin.Context) { panic("炸了") })
	}()

	if out := testkit.Scrape(m.Handler); !strings.Contains(out, "http_requests_total") {
		t.Errorf("panic 的请求也该计入\n实际=\n%s", out)
	}
}

var _ = http.StatusOK

func TestMetric_CustomMethodNormalizedToOTHER(t *testing.T) {
	// 路由已经用模板挡住了 URL 里的 id，方法这一维却是照抄请求的——
	// 而 HTTP 方法是个自由 token，谁都能发 CUSTOM1、CUSTOM2，
	// 每来一个新值就多一组时间序列，没有淘汰机制。
	//
	// 这一段走真实的中间件，而不是直接调 web.NormalizeMethod：
	// 变异测试发现只测那个函数的话，把调用点绕开（`web.NormalizeMethod(m)` → `m`）
	// 一样能过——函数本身是对的，只是没人用它，而那正是这个 bug 的形状
	m := withMetrics(t)
	for _, method := range []string{"CUSTOM1", "FOOBAR", "PROPFIND"} {
		serve(t, httptest.NewRequest(method, "/hello", nil), []gin.HandlerFunc{Metric()},
			func(c *gin.Context) { c.Status(200) })
	}
	out := testkit.Scrape(m.Handler)
	if !strings.Contains(out, `method="OTHER"`) {
		t.Errorf("自定义方法该收敛成 OTHER\n实际=\n%s", out)
	}
	for _, leaked := range []string{`method="CUSTOM1"`, `method="FOOBAR"`, `method="PROPFIND"`} {
		if strings.Contains(out, leaked) {
			t.Errorf("%s 进了标签，时间序列会被请求方撑爆\n实际=\n%s", leaked, out)
		}
	}

	// 映射表本身的用例在 internal/web（TestNormalizeMethod）
}

func TestTrace_ErrAbortHandlerAbortRecordedAsError(t *testing.T) {
	// 中止的请求带着 panic 穿过 Trace，写在 c.Next() 后面的收尾走不到；
	// 读到的状态码又是已经发出去的 200——Span 上看是一次成功
	spans := recording(t)
	serveAborted(t, Trace())

	got := spans()
	if len(got) != 1 {
		t.Fatalf("应产出一个 Span，got=%d", len(got))
	}
	if got[0].Status.Code != codes.Error {
		t.Errorf("中止的请求该标成错误，got=%v", got[0].Status)
	}
	found := false
	for _, a := range got[0].Attributes {
		if a.Key == "http.response.status_code" {
			found = true
			if a.Value.AsInt64() != statusAborted {
				t.Errorf("状态码该记成 %d，got=%d", statusAborted, a.Value.AsInt64())
			}
		}
	}
	if !found {
		t.Error("Span 上没有状态码")
	}
}

func TestMetric_ErrAbortHandlerAbortRecordedAs499(t *testing.T) {
	// 记成 200 的话，被截断的响应在错误率里凭空消失
	m := withMetrics(t)
	serveAborted(t, Metric())

	if out := testkit.Scrape(m.Handler); !strings.Contains(out, `status="499"`) {
		t.Errorf("中止的请求该记成 499\n实际=\n%s", out)
	}
}

func TestTrace_ErrorTextIsRedactedOnSpan(t *testing.T) {
	spans := recording(t)
	serve(t, get("/hello"), []gin.HandlerFunc{Trace()}, func(c *gin.Context) {
		_ = c.Error(errors.New("login failed password=hunter2"))
		c.Status(500)
	})
	got := "（没写）"
	for _, a := range spans()[0].Attributes {
		if a.Key == "gin.errors" {
			got = a.Value.AsString()
		}
	}
	if got != Redacted {
		t.Errorf("Span 上的 gin.errors 该被遮掉，got=%q", got)
	}
}

func TestTrace_ErrorTextKeepsSeparationAndMasksDSNOnSpan(t *testing.T) {
	spans := recording(t)
	serve(t, get("/hello"), []gin.HandlerFunc{Trace()}, func(c *gin.Context) {
		_ = c.Error(errors.New("dial postgres://app:hunter2@db:5432/prod failed"))
		_ = c.Error(errors.New("retry gave up"))
		c.Status(500)
	})
	got := "（没写）"
	for _, a := range spans()[0].Attributes {
		if a.Key == "gin.errors" {
			got = a.Value.AsString()
		}
	}
	if want := "Error #01: dial postgres://app:" + Redacted + "@db:5432/prod failed; Error #02: retry gave up"; got != want {
		t.Errorf("gin.errors=%q, want %q", got, want)
	}
}

// ---- 499 的规矩：只记真的没发出去的 ----

// recordedAs 跑一次请求，返回访问日志、指标、Span 各自记下的状态码和 Span 是否标错
func recordedAs(t *testing.T, req *http.Request, h gin.HandlerFunc) (logged any, metric string, span string, spanErr bool) {
	t.Helper()
	lines := capture(t)
	spans := recording(t)
	m := withMetrics(t)
	serve(t, req, []gin.HandlerFunc{Trace(), Log(), Metric(), Recover(nil)}, h)

	var access []map[string]any
	for _, l := range lines() {
		if l["msg"] == "request completed" {
			access = append(access, l)
		}
	}
	logged = access[0]["status"]
	out := testkit.Scrape(m.Handler)
	const prefix = `http_requests_total{method="GET",route="/hello",status="`
	if i := strings.Index(out, prefix); i >= 0 {
		rest := out[i+len(prefix):]
		metric = rest[:strings.Index(rest, `"`)]
	}
	got := spans()
	if len(got) != 1 {
		t.Fatalf("应产出一个 Span，got=%d", len(got))
	}
	var code string
	for _, kv := range got[0].Attributes {
		if kv.Key == "http.response.status_code" {
			code = kv.Value.Emit()
		}
	}
	return logged, metric, code, got[0].Status.Code == codes.Error
}

// goneRequest 客户端已经走了的请求：net/http 在连接断开（HTTP/2 是流被重置）、强制断连时取消请求的 ctx
func goneRequest() *http.Request {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return get("/hello").WithContext(ctx)
}

func TestClientGoneBeforeAnythingSentRecordedAs499(t *testing.T) {
	// 回归用例：客户端断开之后 handler 什么都没写就返回，原先记成 bytes_out 为 0 的 200——
	// 客户端其实什么都没收到。和 xecho 一个规矩
	logged, metric, span, spanErr := recordedAs(t, goneRequest(), func(c *gin.Context) {
		_ = c.Error(c.Request.Context().Err()) //nolint:errcheck // 只是登记
	})
	if logged != float64(499) || metric != "499" || span != "499" || spanErr {
		t.Errorf("该记 499、Span 不标错（不是服务端的错），got log=%v metric=%s span=%s spanErr=%v", logged, metric, span, spanErr)
	}
}

func TestClientGoneAfterResponseWrittenKeepsSentStatus(t *testing.T) {
	// 响应已经开始发了：客户端收到了（至少是一部分），记已经发出去的那个状态码
	logged, metric, span, _ := recordedAs(t, goneRequest(), func(c *gin.Context) { c.String(201, "partial") })
	if logged != float64(201) || metric != "201" || span != "201" {
		t.Errorf("该记已经发出去的 201，got log=%v metric=%s span=%s", logged, metric, span)
	}
}

func TestClientGoneIgnoresContextsSetByInnerMiddleware(t *testing.T) {
	// 里面几层换上的 ctx 是业务自己的：套了超时、返回时 cancel 掉，出了那一层就是 Canceled，客户端却还在等
	logged, metric, span, _ := recordedAs(t, get("/hello"), func(c *gin.Context) {
		ctx, cancel := context.WithCancel(c.Request.Context())
		cancel()
		c.Request = c.Request.WithContext(ctx)
	})
	if logged != float64(200) || metric != "200" || span != "200" {
		t.Errorf("客户端还在，该记 200，got log=%v metric=%s span=%s", logged, metric, span)
	}
}
