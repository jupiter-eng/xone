package middleware

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/jupiter-eng/xone/internal/testkit"
	"github.com/jupiter-eng/xone/xecho/internal/peer"
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

// attrsOf Span 的属性，值一律转成字符串
func attrsOf(s tracetest.SpanStub) map[string]string {
	attrs := map[string]string{}
	for _, kv := range s.Attributes {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	return attrs
}

func TestTrace_StartsSpanAndReturnsTraceID(t *testing.T) {
	spans := recording(t)
	var inHandler trace.SpanContext
	w := serve(t, get("/hello/42"), []echo.MiddlewareFunc{Trace()}, func(c echo.Context) error {
		inHandler = trace.SpanContextFromContext(c.Request().Context())
		return c.String(200, "ok")
	})

	if w.Header().Get(TraceIDHeader) == "" {
		t.Error("应在响应头里回带 TraceID，否则从一次调用没法跳到链路")
	}
	got := spans()
	if len(got) != 1 {
		t.Fatalf("应产出一个 Span，got=%d", len(got))
	}
	// 用路由模板而不是真实路径：按真实路径命名会让 Span 名随用户数增长
	// handler 的 ctx 里就是这个 Span：往下游传 c.Request().Context()，链路才接得上
	if inHandler.SpanID() != got[0].SpanContext.SpanID() || w.Header().Get(TraceIDHeader) != got[0].SpanContext.TraceID().String() {
		t.Errorf("handler 的 ctx 和 X-Trace-Id 该对应这个 Span，got handler=%v header=%q", inHandler.SpanID(), w.Header().Get(TraceIDHeader))
	}
	if got[0].Name != "GET /hello/:id" || got[0].SpanKind != trace.SpanKindServer {
		t.Errorf("Span 名应用路由模板、类型是服务端，got=%q %v", got[0].Name, got[0].SpanKind)
	}
	attrs := attrsOf(got[0])
	if attrs["http.route"] != "/hello/:id" || attrs["url.path"] != "/hello/42" || attrs["http.request.method"] != "GET" {
		t.Errorf("路由与路径属性不对，got=%v", attrs)
	}
	if attrs["http.response.status_code"] != "200" {
		t.Errorf("应记状态码，got=%v", attrs)
	}
	if _, ok := attrs["echo.errors"]; ok {
		t.Errorf("没出错就不写 echo.errors，got=%v", attrs)
	}
}

func TestTrace_ContinuesUpstreamTrace(t *testing.T) {
	spans := recording(t)
	req := get("/hello")
	// 一条合法的 W3C traceparent
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	serve(t, req, []echo.MiddlewareFunc{Trace()}, statusOnly(200))

	got := spans()
	if len(got) != 1 {
		t.Fatalf("应产出一个 Span，got=%d", len(got))
	}
	if id := got[0].SpanContext.TraceID().String(); id != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("应接上上游的 TraceID，got=%s", id)
	}
}

func TestTrace_OnlyServerErrorsMarkedAsError(t *testing.T) {
	// 4xx 是客户端传错了，标成错误会让链路里满屏是错，真故障反而看不出来。
	// 状态码写出来的和 handler 返回错误、由 echo 渲染的，两种都要对
	for status, wantErr := range map[int]bool{200: false, 404: false, 400: false, 500: true, 503: true} {
		handlers := map[string]echo.HandlerFunc{"写出状态码": statusOnly(status)}
		if status >= 400 {
			handlers["返回 HTTPError"] = func(echo.Context) error { return echo.NewHTTPError(status) }
		}
		for name, h := range handlers {
			spans := recording(t)
			serve(t, get("/hello"), []echo.MiddlewareFunc{Trace()}, h)
			got := spans()
			if len(got) != 1 {
				t.Fatalf("status=%d 应产出一个 Span", status)
			}
			if isErr := got[0].Status.Code == codes.Error; isErr != wantErr {
				t.Errorf("%s status=%d 应当标错=%v，实际=%v", name, status, wantErr, isErr)
			}
			if a := attrsOf(got[0]); a["http.response.status_code"] != strconv.Itoa(status) {
				t.Errorf("%s：Span 上的状态码该是渲染之后的 %d，got=%v", name, status, a["http.response.status_code"])
			}
		}
	}
}

func TestTrace_ReturnedErrorRecordedOnSpanAndResponseKeepsTraceID(t *testing.T) {
	// handler 返回的错误进 Span 的 echo.errors（这条带着 password=，整段遮掉）；echo 渲染的错误响应照样带着 X-Trace-Id
	spans := recording(t)
	w := httptest.NewRecorder()
	errorEcho(Trace()).ServeHTTP(w, httptest.NewRequest("GET", "/plain", nil))
	got := spans()
	if len(got) != 1 {
		t.Fatalf("应产出一个 Span，got=%d", len(got))
	}
	a := attrsOf(got[0])
	if a["echo.errors"] != Redacted || a["http.response.status_code"] != "500" || got[0].Status.Code != codes.Error {
		t.Errorf("Span 该记下错误、500 并标错，got=%v %v", a, got[0].Status)
	}
	if w.Code != 500 || w.Header().Get(TraceIDHeader) == "" {
		t.Errorf("错误响应也该带 X-Trace-Id，got=%d %q", w.Code, w.Header().Get(TraceIDHeader))
	}
}

func TestTrace_UnmatchedRouteUsesFixedValue(t *testing.T) {
	// 用真实路径的话，扫描器随便打几个 URL 就能把链路和指标的基数撑爆。
	// 方法不对（405）也记 unmatched：echo 给的是那条路由的模板，和 xgin 对不上
	for _, c := range errorCases {
		if c.route != "unmatched" {
			continue
		}
		spans := recording(t)
		errorEcho(Trace()).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(c.method, c.path, nil))
		got := spans()
		if len(got) != 1 {
			t.Fatalf("%s：应产出一个 Span，got=%d", c.name, len(got))
		}
		if want := c.method + " unmatched"; got[0].Name != want || attrsOf(got[0])["http.route"] != "unmatched" {
			t.Errorf("%s：Span 名该是 %q，got=%q %v", c.name, want, got[0].Name, attrsOf(got[0]))
		}
	}
}

func TestTrace_CustomMethodNormalizedToOTHER(t *testing.T) {
	// 与指标同一个道理：Span 名是链路后端建索引的那一维，方法是个自由 token，
	// 谁都能发 CUSTOM1、CUSTOM2。照抄的话每来一个新值就多一个 Span 名。
	// 原始方法留在 http.request.method_original 里，排查时看得到
	spans := recording(t)
	serve(t, httptest.NewRequest("CUSTOM1", "/hello", nil), []echo.MiddlewareFunc{Trace()}, statusOnly(200))

	got := spans()
	if len(got) != 1 {
		t.Fatalf("应产出一个 Span，got=%d", len(got))
	}
	if got[0].Name != "OTHER /hello" {
		t.Errorf("自定义方法该收敛成 OTHER，got=%q", got[0].Name)
	}
	attrs := attrsOf(got[0])
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

func TestTrace_PeerTrustFollowsXechoMarker(t *testing.T) {
	// 透传 Header 只收可信对端发来的值。可不可信由 xecho 按 TrustedProxies 判，
	// 这里只负责把判断结果交给 xtrace：没有记号（单独用本中间件）就是不可信
	old := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTextMapPropagator(old) })
	var got bool
	otel.SetTextMapPropagator(trustProbe{got: &got})

	markTrusted := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error { c.Set(peer.TrustedKey, true); return next(c) }
	}
	for name, c := range map[string]struct {
		mws  []echo.MiddlewareFunc
		want bool
	}{
		"单独用":                  {[]echo.MiddlewareFunc{Trace()}, false},
		"xecho 标了可信":           {[]echo.MiddlewareFunc{markTrusted, Trace()}, true},
		"Propagate 单独用":        {[]echo.MiddlewareFunc{Propagate()}, false},
		"Propagate xecho 标了可信": {[]echo.MiddlewareFunc{markTrusted, Propagate()}, true},
	} {
		got = !c.want
		serve(t, get("/hello"), c.mws, statusOnly(200))
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
	w := serve(t, req, []echo.MiddlewareFunc{Propagate()}, func(c echo.Context) error {
		seen = trace.SpanContextFromContext(c.Request().Context()).TraceID().String()
		return c.NoContent(200)
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

func TestTrace_ErrAbortHandlerAbortRecordedAsError(t *testing.T) {
	// 中止的请求带着 panic 穿过 Trace，写在 next(c) 后面的收尾走不到；
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
	if a := attrsOf(got[0]); a["http.response.status_code"] != "499" {
		t.Errorf("状态码该记成 499，got=%v", a["http.response.status_code"])
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
	serve(t, get("/hello/42"), []echo.MiddlewareFunc{Metric()}, statusOnly(201))

	out := testkit.Scrape(m.Handler)
	if !strings.Contains(out, `http_requests_total{method="GET",route="/hello/:id",status="201"} 1`) {
		t.Errorf("应按方法、路由、状态码记请求数\n实际=\n%s", out)
	}
	if !strings.Contains(out, `http_request_duration_seconds_count{method="GET",route="/hello/:id",status="201"} 1`) {
		t.Errorf("应记耗时\n实际=\n%s", out)
	}
}

func TestMetric_RouteUsesTemplate(t *testing.T) {
	// 按真实路径打标签会让时间序列随 URL 里的 id 无限增长，Prometheus 会被撑垮
	m := withMetrics(t)
	for _, p := range []string{"/hello/1", "/hello/2", "/hello/3"} {
		serve(t, get(p), []echo.MiddlewareFunc{Metric()}, statusOnly(200))
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
	e := errorEcho(Metric())
	e.ServeHTTP(httptest.NewRecorder(), get("/nope/12345"))
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/only-get", nil))
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("OPTIONS", "/only-get", nil))
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/v1/users", nil)) // 落到分组的兜底上

	out := testkit.Scrape(m.Handler)
	for _, want := range []string{
		`http_requests_total{method="GET",route="unmatched",status="404"} 1`,
		`http_requests_total{method="POST",route="unmatched",status="405"} 1`,
		// OPTIONS 由 echo 自己回 204 和 Allow，同样不是注册过的路由
		`http_requests_total{method="OPTIONS",route="unmatched",status="204"} 1`,
		// 分组下方法不对：兜底比 405 优先，echo 回 404
		`http_requests_total{method="POST",route="unmatched",status="404"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("该有 %s\n实际=\n%s", want, out)
		}
	}
	if strings.Contains(out, `route="/api/v1/*"`) {
		t.Errorf("落到分组兜底上的请求不该记成兜底的模板\n实际=\n%s", out)
	}
	if strings.Contains(out, `route="/only-get"`) {
		t.Errorf("方法不对的请求不该记成那条路由\n实际=\n%s", out)
	}
}

func TestMetric_ReturnedErrorRecordedWithRenderedStatus(t *testing.T) {
	// 不在这一层渲染的话，这几个请求在错误率里全是 200
	m := withMetrics(t)
	e := errorEcho(Metric())
	for _, c := range errorCases {
		e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(c.method, c.path, nil))
	}
	out := testkit.Scrape(m.Handler)
	for _, want := range []string{`route="/plain",status="500"} 1`, `route="/teapot",status="418"} 1`} {
		if !strings.Contains(out, want) {
			t.Errorf("该有 %s\n实际=\n%s", want, out)
		}
	}
	if strings.Contains(out, `status="200"`) {
		t.Errorf("出错的请求不该记成 200\n实际=\n%s", out)
	}
}

func TestMetric_PanicStillCounted(t *testing.T) {
	// 不计入的话，出问题的请求会在错误率指标里凭空消失
	m := withMetrics(t)
	func() {
		defer func() { recover() }()
		serve(t, get("/hello"), []echo.MiddlewareFunc{Metric()}, func(c echo.Context) error { panic("炸了") })
	}()
	if out := testkit.Scrape(m.Handler); !strings.Contains(out, "http_requests_total") {
		t.Errorf("panic 的请求也该计入\n实际=\n%s", out)
	}
}

func TestMetric_CustomMethodNormalizedToOTHER(t *testing.T) {
	// 路由已经用模板挡住了 URL 里的 id，方法这一维却是照抄请求的——
	// 而 HTTP 方法是个自由 token，谁都能发 CUSTOM1、CUSTOM2，
	// 每来一个新值就多一组时间序列，没有淘汰机制。
	//
	// 走真实的中间件而不是直接调 web.NormalizeMethod：只测那个函数的话，
	// 把调用点绕开一样能过（映射表本身的用例在 internal/web）
	m := withMetrics(t)
	for _, method := range []string{"CUSTOM1", "FOOBAR", "PROPFIND"} {
		serve(t, httptest.NewRequest(method, "/hello", nil), []echo.MiddlewareFunc{Metric()}, statusOnly(200))
	}
	out := testkit.Scrape(m.Handler)
	if !strings.Contains(out, `method="OTHER",route="/hello",status="200"} 3`) {
		t.Errorf("自定义方法该收敛成 OTHER\n实际=\n%s", out)
	}
	for _, leaked := range []string{`method="CUSTOM1"`, `method="FOOBAR"`, `method="PROPFIND"`} {
		if strings.Contains(out, leaked) {
			t.Errorf("%s 进了标签，时间序列会被请求方撑爆\n实际=\n%s", leaked, out)
		}
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

func TestMetric_TwoInstancesShareTheSameCollectors(t *testing.T) {
	// 同名同标签的指标重复注册时 xmetric 返回已有的那个：两个服务（或者 xgin 和 xecho）
	// 记在同一组指标上，不报注册冲突
	m := withMetrics(t)
	serve(t, get("/hello"), []echo.MiddlewareFunc{Metric()}, statusOnly(200))
	serve(t, get("/hello"), []echo.MiddlewareFunc{Metric()}, statusOnly(200))
	if out := testkit.Scrape(m.Handler); !strings.Contains(out, `http_requests_total{method="GET",route="/hello",status="200"} 2`) {
		t.Errorf("两个中间件实例该记在同一组指标上\n实际=\n%s", out)
	}
}

func TestTrace_ErrorTextIsRedactedOnSpan(t *testing.T) {
	spans := recording(t)
	serve(t, get("/hello"), []echo.MiddlewareFunc{Trace()}, func(c echo.Context) error {
		return errors.New("login failed password=hunter2")
	})
	if got := attrsOf(spans()[0])["echo.errors"]; got != Redacted {
		t.Errorf("Span 上的 echo.errors 该被遮掉，got=%q", got)
	}
}

func TestTrace_DSNPasswordInErrorIsMaskedOnSpan(t *testing.T) {
	spans := recording(t)
	serve(t, get("/hello"), []echo.MiddlewareFunc{Trace()}, func(c echo.Context) error {
		return errors.New("dial postgres://app:hunter2@db:5432/prod failed")
	})
	if got, want := attrsOf(spans()[0])["echo.errors"], "dial postgres://app:"+Redacted+"@db:5432/prod failed"; got != want {
		t.Errorf("echo.errors=%q, want %q", got, want)
	}
}

// ---- 499 的规矩：只记真的没发出去的 ----

// recordedAs 跑一次请求，返回客户端收到的状态码，以及访问日志、指标、Span 各自记下的状态码和 Span 是否标错
func recordedAs(t *testing.T, req *http.Request, h echo.HandlerFunc) (sent int, logged any, metric string, span string, spanErr bool) {
	t.Helper()
	lines := capture(t)
	spans := recording(t)
	m := withMetrics(t)
	w := serve(t, req, []echo.MiddlewareFunc{Trace(), Log(), Metric(), Recover(nil)}, h)

	logged = accessLogs(lines())[0]["status"]
	out := testkit.Scrape(m.Handler)
	if i := strings.Index(out, `http_requests_total{method="GET",route="/hello",status="`); i >= 0 {
		rest := out[i+len(`http_requests_total{method="GET",route="/hello",status="`):]
		metric = rest[:strings.Index(rest, `"`)]
	}
	got := spans()
	if len(got) != 1 {
		t.Fatalf("应产出一个 Span，got=%d", len(got))
	}
	return w.Code, logged, metric, attrsOf(got[0])["http.response.status_code"], got[0].Status.Code == codes.Error
}

func TestReturnedErrAbortHandlerRecordsWhatWasSent(t *testing.T) {
	// 回归用例：handler 返回（不是 panic）一个包着 http.ErrAbortHandler 的错误，连接并没有断，
	// 客户端收到的是 HTTPErrorHandler 渲染的 500，三处却都记成了 499。只有 panic 才会让 net/http 断开连接
	sent, logged, metric, span, spanErr := recordedAs(t, get("/hello"), func(echo.Context) error {
		return fmt.Errorf("proxy: %w", http.ErrAbortHandler)
	})
	if sent != 500 || logged != float64(500) || metric != "500" || span != "500" || !spanErr {
		t.Errorf("返回的错误该按发出去的 500 记，got sent=%d log=%v metric=%s span=%s spanErr=%v", sent, logged, metric, span, spanErr)
	}
}

// goneRequest 客户端已经走了的请求：net/http 在连接断开（HTTP/2 是流被重置）、强制断连时取消请求的 ctx
func goneRequest() *http.Request {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return get("/hello").WithContext(ctx)
}

func TestClientGoneBeforeAnythingSentRecordedAs499(t *testing.T) {
	// 回归用例：客户端断开之后 handler 照惯例 return ctx.Err()，原先记成 500、Span 标错、进 5xx 的指标——
	// 客户端其实什么都没收到。c.Error 往断开的连接上写错误响应照样置上 Committed、Status 记 500（实测），
	// 所以要在交给它之前判
	for name, h := range map[string]echo.HandlerFunc{
		"return ctx.Err()": func(c echo.Context) error { return c.Request().Context().Err() },
		"什么都不写就返回":         func(echo.Context) error { return nil },
	} {
		_, logged, metric, span, spanErr := recordedAs(t, goneRequest(), h)
		if logged != float64(499) || metric != "499" || span != "499" || spanErr {
			t.Errorf("%s：该记 499、Span 不标错（不是服务端的错），got log=%v metric=%s span=%s spanErr=%v", name, logged, metric, span, spanErr)
		}
	}
}

func TestClientGoneAfterResponseCommittedKeepsSentStatus(t *testing.T) {
	// 响应已经开始发了：客户端收到了（至少是一部分），记已经发出去的那个状态码
	_, logged, metric, span, _ := recordedAs(t, goneRequest(), func(c echo.Context) error {
		_ = c.String(201, "partial")
		return c.Request().Context().Err()
	})
	if logged != float64(201) || metric != "201" || span != "201" {
		t.Errorf("该记已经发出去的 201，got log=%v metric=%s span=%s", logged, metric, span)
	}
}

func TestClientDeadlineIsNotClientGone(t *testing.T) {
	// ContextTimeout 这类中间件给请求套的是截止时间：到点了客户端还在等，收到的是 503，不是 499
	_, logged, metric, _, _ := recordedAs(t, get("/hello"), func(c echo.Context) error {
		ctx, cancel := context.WithTimeout(c.Request().Context(), 0)
		defer cancel()
		<-ctx.Done()
		return echo.ErrServiceUnavailable.WithInternal(ctx.Err())
	})
	if logged != float64(503) || metric != "503" {
		t.Errorf("该记 503，got log=%v metric=%s", logged, metric)
	}
}

func TestClientGoneIgnoresContextsSetByInnerLayers(t *testing.T) {
	// 里面几层换上的 ctx 是业务自己的：套了一层、返回时 cancel 掉，出了那一层就是 Canceled，客户端却还在等
	_, logged, metric, span, _ := recordedAs(t, get("/hello"), func(c echo.Context) error {
		ctx, cancel := context.WithCancel(c.Request().Context())
		cancel()
		c.SetRequest(c.Request().WithContext(ctx))
		return nil
	})
	if logged != float64(200) || metric != "200" || span != "200" {
		t.Errorf("客户端还在，该记 200，got log=%v metric=%s span=%s", logged, metric, span)
	}
}
