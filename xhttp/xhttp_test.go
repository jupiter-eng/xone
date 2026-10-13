package xhttp

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xmetric"
	"github.com/jupiter-eng/xone/xonetest"
	"github.com/jupiter-eng/xone/xtrace"
)

// TestMain 关掉 resty 的内部日志：连不上的用例本来就会刷一屏重试告警
func TestMain(m *testing.M) {
	silent = true
	os.Exit(m.Run())
}

// silent 让测试里新建的 client 闭嘴
var silent bool

// echo 起一个记下收到的请求的服务端
func echo(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Pointer[http.Header]) {
	t.Helper()
	var last atomic.Pointer[http.Header]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := r.Header.Clone()
		last.Store(&hdr)
		if h != nil {
			h(w, r)
			return
		}
		w.WriteHeader(204)
	}))
	t.Cleanup(srv.Close)
	return srv, &last
}

func TestNew_ReturnsUsableResty(t *testing.T) {
	srv, _ := echo(t, nil)
	client, _ := newQuiet(t, DefaultConfig())

	resp, err := client.R().SetContext(context.Background()).Get(srv.URL)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	if resp.StatusCode() != 204 {
		t.Errorf("状态码不对，got=%d", resp.StatusCode())
	}
}

func TestNew_PoolSettingsPassedThrough(t *testing.T) {
	c := DefaultConfig()
	c.MaxIdleConnsPerHost, c.MaxIdleConns, c.IdleConnTimeout, c.MaxConnsPerHost = 33, 77, 11*time.Second, 5
	c.Trace = false // otelhttp.Transport 不导出内层，要看底层就别包它
	client, _ := newQuiet(t, c)

	// 标准库默认每主机只留 2 条空闲连接，对只调几个下游的服务太小
	tr := unwrapTransport(t, client)
	if tr.MaxIdleConnsPerHost != 33 || tr.MaxIdleConns != 77 || tr.IdleConnTimeout != 11*time.Second || tr.MaxConnsPerHost != 5 {
		t.Errorf("连接池参数没传下去，got=%+v", tr)
	}
	if tr.DialContext == nil {
		t.Error("应当装上带超时的 Dialer")
	}
	// 从 DefaultTransport 克隆而不是新建，代理和 TLS 这些默认设置才不会丢
	if tr.Proxy == nil {
		t.Error("应保留 DefaultTransport 的代理设置")
	}
}

// unwrapTransport 剥掉链路那几层包装，拿到底层的 *http.Transport
func unwrapTransport(t *testing.T, client *resty.Client) *http.Transport {
	t.Helper()
	rt := client.GetClient().Transport
	for i := 0; i < 5; i++ {
		switch v := rt.(type) {
		case *http.Transport:
			return v
		case *xtrace.Transport:
			rt = v.Next
		case propagateOnly:
			rt = v.next
		default:
			t.Fatalf("剥不开的 Transport 类型：%T（otelhttp 不导出内层，测这层时把 Trace 关掉）", rt)
		}
	}
	t.Fatal("包装层数太多")
	return nil
}

func TestNew_TraceOffOnlySkipsSpans_TraceIDAndForwardHeadersStillSent(t *testing.T) {
	// 回归用例。XHttp.Trace: false 原先把 xtrace.Transport 连同注入一起摘掉，
	// X-Request-Id 和上游的 traceparent 就断在这一跳，而文档说它只管 Span
	old := otel.GetTracerProvider()
	oldProp := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTracerProvider(old); otel.SetTextMapPropagator(oldProp) })

	exp := tracetest.NewInMemoryExporter()
	tc := xtrace.DefaultConfig()
	tc.ForwardHeaders = []string{"X-Request-Id"}
	tc.ForwardHeaderRules = []xtrace.ForwardHeaderRule{{Domains: []string{"127.0.0.1"}, Headers: []string{"X-Tenant-Id"}}}
	tr, tcloser, err := xtrace.New(context.Background(), tc, sdktrace.NewSimpleSpanProcessor(exp))
	if err != nil {
		t.Fatal(err)
	}
	defer tcloser.Close()
	tr.Install()

	srv, last := echo(t, nil)
	c := DefaultConfig()
	c.Trace = false
	client, _ := newQuiet(t, c)

	const upstream = "4bf92f3577b34da6a3ce929d0e0e4736"
	ctx := tr.Propagator.Extract(context.Background(), fromTrusted{propagation.HeaderCarrier(http.Header{
		"Traceparent":  {"00-" + upstream + "-00f067aa0ba902b7-01"},
		"Baggage":      {"tenant=acme"},
		"X-Request-Id": {"req-1"},
		"X-Tenant-Id":  {"t-1"},
	})})
	req := client.R().SetContext(ctx)
	if _, err := req.Get(srv.URL); err != nil {
		t.Fatal(err)
	}

	got := *last.Load()
	if got.Get("X-Request-Id") != "req-1" {
		t.Errorf("Trace 关着也该透传 X-Request-Id，下游收到=%v", got)
	}
	if got.Get("X-Tenant-Id") != "t-1" {
		t.Errorf("按域名的透传规则也要生效（目标 host 靠 xtrace.Transport 写进 ctx），下游收到=%v", got)
	}
	if tp := got.Get("Traceparent"); !strings.HasPrefix(tp, "00-"+upstream+"-") {
		t.Errorf("Trace 关着也该把上游的链路标识带给下游，got=%q", tp)
	}
	if got.Get("Baggage") != "tenant=acme" {
		t.Errorf("baggage 也该带给下游，got=%q", got.Get("Baggage"))
	}
	if spans := exp.GetSpans(); len(spans) != 0 {
		t.Errorf("Trace 关着不该开出站 Span，got=%d 个", len(spans))
	}
	if req.RawRequest.Header.Get("X-Request-Id") != "" {
		t.Error("注入要写在克隆出来的请求上，不该改调用方的请求")
	}
}

func TestNew_TraceOffCloseIdleConnectionsStillReachesPool(t *testing.T) {
	// RawClient 的使用者调 http.Client.CloseIdleConnections，它靠类型断言一层层往下找，
	// 中间哪一层不转发就是空操作。只断言方法存在测不出来，得看连接有没有真的被释放
	var idle atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		switch s {
		case http.StateIdle:
			idle.Add(1)
		case http.StateClosed, http.StateHijacked:
			idle.Add(-1)
		}
	}
	srv.Start()
	defer srv.Close()

	c := DefaultConfig()
	c.Trace = false
	client, _ := newQuiet(t, c)
	if _, err := client.R().Get(srv.URL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return idle.Load() > 0 }, "请求完成后该有一个空闲连接")
	client.GetClient().CloseIdleConnections()
	waitFor(t, func() bool { return idle.Load() == 0 }, "CloseIdleConnections 之后空闲连接该被释放")
}

func TestTransport_OtelhttpInjectsForwardHeadersUnderNoop(t *testing.T) {
	// 单 Transport 的简化依赖这个前提：otelhttp 无论 TracerProvider 是不是 noop，
	// 都会调用全局 Propagator 注入。前提不成立就得补回「链路关了自己注入」那一层。
	// 钉在这里而不是 xtrace：那边为此引 otelhttp 会让每个 xtrace 使用者
	// 的模块图凭空多四个模块。
	old := otel.GetTracerProvider()
	oldProp := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTracerProvider(old); otel.SetTextMapPropagator(oldProp) })
	otel.SetTracerProvider(noop.NewTracerProvider())

	tc := xtrace.DefaultConfig()
	tc.Enable = false // 链路关掉
	tc.ForwardHeaders = []string{"X-Request-Id"}
	tr, tcloser, err := xtrace.New(context.Background(), tc)
	if err != nil {
		t.Fatal(err)
	}
	defer tcloser.Close()
	tr.Install()

	srv, last := echo(t, nil)
	client, _ := newQuiet(t, DefaultConfig())

	// 透传只收可信对端发来的值，这里扮演 xgin 判过可信之后交来的请求头
	ctx := tr.Propagator.Extract(context.Background(),
		fromTrusted{propagation.HeaderCarrier(http.Header{"X-Request-Id": {"req-1"}})})
	if _, err := client.R().SetContext(ctx).Get(srv.URL); err != nil {
		t.Fatal(err)
	}

	got := *last.Load()
	if got.Get("X-Request-Id") != "req-1" {
		t.Fatalf("链路关着也该透传 header，实际收到的=%v", got)
	}
	if got.Get("traceparent") != "" {
		t.Errorf("链路关着就不该注入 traceparent，got=%q", got.Get("traceparent"))
	}
}

// fromTrusted 声明了「对端可信」的 carrier，约定见 xtrace.HeaderPropagator.Extract
type fromTrusted struct{ propagation.HeaderCarrier }

func (fromTrusted) TrustedPeer() bool { return true }

func TestTransport_TraceOnInjectsTraceparentAndStartsSpan(t *testing.T) {
	old := otel.GetTracerProvider()
	oldProp := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTracerProvider(old); otel.SetTextMapPropagator(oldProp) })

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exp)))
	defer tp.Shutdown(context.Background())
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	srv, last := echo(t, nil)
	client, _ := newQuiet(t, DefaultConfig())

	if _, err := client.R().SetContext(context.Background()).Get(srv.URL + "/api/orders"); err != nil {
		t.Fatal(err)
	}

	if got := (*last.Load()).Get("traceparent"); got == "" {
		t.Error("应当注入 traceparent")
	}
	spans := exp.GetSpans()
	if len(spans) == 0 {
		t.Fatal("应当产出 Span")
	}
	if name := spans[0].Name; name != "GET" {
		t.Errorf("Span 名应只有方法，got=%q", name)
	}
}

func TestTransport_TargetHostStoredInCtx(t *testing.T) {
	// 没有它，按域名透传的规则一条都不会命中
	var seen string
	tr := &xtrace.Transport{Next: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		seen = xtrace.TargetHostFromContext(r.Context())
		return &http.Response{StatusCode: 204, Body: http.NoBody}, nil
	})}
	req, _ := http.NewRequest("GET", "https://api.example.com/x", nil)
	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if seen != "api.example.com" {
		t.Errorf("应写入目标 host，got=%q", seen)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// ---- 重试 ----

func TestRetry_OnlyIdempotentMethods(t *testing.T) {
	// 超时分不出「请求没到」和「处理完了但响应丢了」，
	// 重发一个 POST 就可能变成重复下单
	if !onlyIdempotent.condition(respFor("GET"), timeoutErr) {
		t.Error("GET 应当允许重试")
	}
	for _, m := range []string{"POST", "PATCH"} {
		if onlyIdempotent.condition(respFor(m), timeoutErr) {
			t.Errorf("%s 不该重试", m)
		}
	}
	if onlyIdempotent.condition(respFor("GET"), nil) {
		t.Error("拿到响应就不该重试，与 resty 默认条件一致")
	}
	if onlyIdempotent.condition(nil, timeoutErr) {
		t.Error("认不出方法时应当保守地不重试")
	}
}

// onlyIdempotent RetryOnlyIdempotent 开着时挂上的那套策略
var onlyIdempotent = retryPolicy{onlyIdempotent: true}

// timeoutErr 一个传输层错误，形状与 http.Client.Do 超时时报的一样
var timeoutErr error = &url.Error{Op: "Get", URL: "http://h", Err: context.DeadlineExceeded}

func TestRetry_NoRetryOnResponseParseFailure(t *testing.T) {
	// 挂上重试条件之后 resty 自己的判断就作废了。从前幂等方法遇上任何错都重试，
	// 实测 200 + 坏 JSON、RetryCount=3 时同一个请求发了 4 次——响应早已完整收到，
	// 重发只会再拿到一模一样的坏 JSON。不挂条件时 resty 只发 1 次
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{bad`))
	}))
	defer srv.Close()

	c := DefaultConfig()
	c.RetryCount, c.RetryWaitTime, c.RetryMaxWaitTime = 3, time.Millisecond, 5*time.Millisecond
	client, _ := newQuiet(t, c)

	var out map[string]any
	if _, err := client.R().SetContext(context.Background()).SetResult(&out).Get(srv.URL); err == nil {
		t.Fatal("坏 JSON 应当报解析错误")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("解析失败不该重试，应当只请求 1 次，got=%d", got)
	}
}

func TestRetry_RetriesOnBodyCutMidway(t *testing.T) {
	// 与 resty 的默认条件一致：响应体没收全是传输层的问题，io.ErrUnexpectedEOF
	if !onlyIdempotent.condition(respFor("GET"), io.ErrUnexpectedEOF) {
		t.Error("响应体读到一半断开应当重试")
	}
}

func respFor(method string) *resty.Response {
	return &resty.Response{Request: &resty.Request{Method: method}}
}

func TestRetry_ActuallyResends(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// 直接断开连接，制造传输层错误
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer srv.Close()

	c := DefaultConfig()
	c.RetryCount, c.RetryWaitTime, c.RetryMaxWaitTime = 2, time.Millisecond, 5*time.Millisecond
	client, _ := newQuiet(t, c)

	client.R().SetContext(context.Background()).Get(srv.URL)
	if got := hits.Load(); got != 3 {
		t.Errorf("重试 2 次应当一共请求 3 次，got=%d", got)
	}

	hits.Store(0)
	client.R().SetContext(context.Background()).Post(srv.URL)
	if got := hits.Load(); got != 1 {
		t.Errorf("POST 不该重试，应当只请求 1 次，got=%d", got)
	}
}

func TestRetry_POSTRetriedWhenIdempotencyCheckOff(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer srv.Close()

	c := DefaultConfig()
	c.RetryCount, c.RetryWaitTime, c.RetryMaxWaitTime = 2, time.Millisecond, 5*time.Millisecond
	c.RetryOnlyIdempotent = false
	client, _ := newQuiet(t, c)

	client.R().SetContext(context.Background()).Post(srv.URL)
	if got := hits.Load(); got != 3 {
		t.Errorf("关掉限制后 POST 也该重试，got=%d", got)
	}
}

// ---- 全局实例 ----

func TestC_FallbackWithTimeoutBeforeInit(t *testing.T) {
	// 零值超时是「永不超时」：关闭阶段发一个这样的请求，整个退出流程就卡住了
	emptyCell(t)

	if C() == nil {
		t.Fatal("任何时候都该返回可用实例")
	}
	if got := C().GetClient().Timeout; got != fallbackTimeout {
		t.Errorf("兜底实例必须带超时，got=%v", got)
	}
	if got := RawClient().Timeout; got != fallbackTimeout {
		t.Errorf("兜底的原生 client 也必须带超时，got=%v", got)
	}
}

func TestR_BindsCtx(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "v")
	if got := R(ctx).Context().Value(key{}); got != "v" {
		t.Errorf("R 应当绑定传进来的 ctx，got=%v", got)
	}
}

func TestRegister_RegistrationMatchesFramework(t *testing.T) {
	// 这是本包和框架之间唯一的一根线：钩子漏登记、档位挂错，
	// 表现是「配置不生效」或者「比用它的东西晚就绪」，别处都测不出来
	var got *hook.Entry
	for _, e := range hook.Start() {
		if e.Pkg == "github.com/jupiter-eng/xone/xhttp" {
			got = &e
			break
		}
	}
	if got == nil {
		t.Fatal("没有登记启动钩子")
	}
	if got.Stage != hook.StageClient {
		t.Errorf("出站客户端要在业务之前就绪（StageClient），got=%v", got.Stage)
	}

	var stopped bool
	for _, e := range hook.Stop() {
		if e.Pkg == "github.com/jupiter-eng/xone/xhttp" {
			stopped = true
			if e.Stage != hook.StageClient {
				// 启动和停止不在同一档的话，业务还在用的时候它就被关了
				t.Errorf("停止钩子也该在 StageClient，got=%v", e.Stage)
			}
		}
	}
	if stopped != true {
		t.Errorf("停止钩子登记情况不对，got=%v", stopped)
	}
}

func TestInit_WorksWithoutConfig(t *testing.T) {
	// 与 xgorm / xredis 不同：HTTP 客户端不连任何外部资源，没配也该给一个能用的
	withMetrics(t)
	initComponent(t, DefaultConfig())

	if C().GetClient().Timeout != DefaultConfig().Timeout {
		t.Errorf("应当用默认超时，got=%v", C().GetClient().Timeout)
	}
	if RawClient() == nil {
		t.Error("原生 client 也该可用")
	}

	if err := closeXHttp(context.Background()); err != nil {
		t.Errorf("关闭不该报错：%v", err)
	}
	// 关闭后退回兜底实例而不是置空：关闭阶段仍可能有组件发请求，
	// 让它带着超时失败，好过拿到一个已经关掉的客户端
	if got := C().GetClient().Timeout; got != fallbackTimeout {
		t.Errorf("关闭后应退回兜底实例，got=%v", got)
	}
}

// emptyCell 把全局实例清空，模拟「框架还没初始化过」
func emptyCell(t *testing.T) {
	t.Helper()
	mu.Lock()
	old := current
	current = nil
	mu.Unlock()
	t.Cleanup(func() { mu.Lock(); current = old; mu.Unlock() })
}

// initComponent 走一遍框架真正会走的路径：按这份配置装好本模块。
func initComponent(t *testing.T, c Config) {
	t.Helper()
	if err := install(c); err != nil {
		t.Fatalf("初始化失败：%v", err)
	}
}

func TestValidate(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Errorf("默认配置应当合法：%v", err)
	}
	bad := DefaultConfig()
	bad.RetryCount = -1
	if err := bad.Validate(); err == nil {
		t.Error("重试次数为负应当报错")
	}
	bad = DefaultConfig()
	bad.MaxIdleConns = -1
	if err := bad.Validate(); err == nil {
		t.Error("连接数为负应当报错")
	}
	bad = DefaultConfig()
	bad.MaxConnsPerHost = -1
	if err := bad.Validate(); err == nil {
		t.Error("每 host 连接数上限为负应当报错")
	}
}

func TestTunedTransport_UntouchedDefaultsMatchDocs(t *testing.T) {
	// 注释和 xhttp/README.md 里写的是这几个数（Go 1.25、resty v2.17.2）。
	// 升级之后变了，这里先红，文档跟着改
	tr := tunedTransport(DefaultConfig(), nil).(*http.Transport)
	if tr.Proxy == nil || tr.TLSHandshakeTimeout != 10*time.Second || tr.ResponseHeaderTimeout != 0 ||
		tr.MaxConnsPerHost != 0 || !tr.ForceAttemptHTTP2 {
		t.Errorf("标准库的默认值变了：proxy=%v tls=%v header=%v maxConns=%d h2=%v",
			tr.Proxy != nil, tr.TLSHandshakeTimeout, tr.ResponseHeaderTimeout, tr.MaxConnsPerHost, tr.ForceAttemptHTTP2)
	}
	client, _ := newQuiet(t, DefaultConfig())
	if client.GetClient().CheckRedirect != nil {
		t.Error("resty 自己设了重定向策略，文档里「标准库默认，最多 10 次」不再成立")
	}
}

func TestNew_MaxConnsPerHostApplied(t *testing.T) {
	// 不限的话并发多少就开多少条连接；配了上限，超出的请求排队等连接
	var mu sync.Mutex
	conns := 0
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			mu.Lock()
			conns++
			mu.Unlock()
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	c := DefaultConfig()
	c.MaxConnsPerHost = 2
	client, _ := newQuiet(t, c)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := client.R().Get(srv.URL); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if conns > 2 {
		t.Errorf("MaxConnsPerHost=2 时下游最多该看到 2 条连接，got=%d", conns)
	}
}

func TestNew_DirectCallAlsoValidatesConfig(t *testing.T) {
	// 不经过配置文件、直接 New 的使用者没有 xconfig.Unmarshal 替他调 Validate
	bad := DefaultConfig()
	bad.DialTimeout = -time.Second
	_, _, err := New(bad)
	if err == nil {
		t.Fatal("非法配置直接 New 也该失败")
	}
	if xerror.Module(err) != "xhttp" || strings.Count(err.Error(), "xhttp") != 1 {
		t.Errorf("错误由 xhttp 包一次，got=%v", err)
	}
}

func TestSpanName_MethodOnly(t *testing.T) {
	// 路径里有 id，query 里常有 id 和令牌：放进 Span 名会撑爆基数，也会把敏感值带出去
	req, _ := http.NewRequest("GET", "https://h/api/orders/42?token=hunter2&id=1", nil)
	if got := spanName("", req); got != "GET" {
		t.Errorf("Span 名该只有方法，got=%q", got)
	}
	if strings.Contains(spanName("", req), "hunter2") {
		t.Error("Span 名里出现了令牌")
	}
}

// newQuiet 建一个不打日志的 client，同时给它装一套独立的指标设施。
//
// 每个用例都换一套 registry：指标是进程级的全局状态，一个用例注册出的冲突
// 会留在那里，让后面所有用例的 New 都失败——上一版就是这么串味的。
func newQuiet(t *testing.T, c Config) (*resty.Client, *xmetric.Metrics) {
	t.Helper()
	m := withMetrics(t)
	client, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	if silent {
		client.SetLogger(discardLogger{})
	}
	t.Cleanup(func() { closer.Close() })
	return client, m
}

type discardLogger struct{}

func (discardLogger) Errorf(string, ...any) {}
func (discardLogger) Warnf(string, ...any)  {}
func (discardLogger) Debugf(string, ...any) {}

func TestNew_CloseReallyClearsIdleConns(t *testing.T) {
	// 回归用例。上一版让 Closer 去调 http.Client.CloseIdleConnections()，
	// 那个方法靠类型断言往下找；链路开着时中间隔着 otelhttp.Transport，
	// 而它没实现这个方法，断言到那里就断了——整条调用是空操作，
	// 而链路默认就是开着的。只断言「包装层实现了这个方法」测不出来，
	// 得看连接有没有真的被释放
	for _, trace := range []bool{false, true} {
		t.Run(fmt.Sprintf("Trace=%v", trace), func(t *testing.T) {
			var idle atomic.Int64
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
			}))
			// 必须在 Start 之前设：起来之后再改，服务端协程已经在读它了
			srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
				switch s {
				case http.StateIdle:
					idle.Add(1)
				case http.StateClosed, http.StateHijacked:
					idle.Add(-1)
				}
			}
			srv.Start()
			defer srv.Close()

			c := DefaultConfig()
			c.Trace = trace
			client, closer, err := New(c)
			if err != nil {
				t.Fatal(err)
			}

			resp, err := client.R().Get(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp

			waitFor(t, func() bool { return idle.Load() > 0 }, "请求完成后该有一个空闲连接")
			if err := closer.Close(); err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool { return idle.Load() == 0 }, "关闭之后空闲连接该被释放")
		})
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(what)
}

func TestMetric_RetryDurationCoversWholeLogicalRequest(t *testing.T) {
	// resty 每次尝试都会重置 Request.Time，resp.Time() 只是最后一次尝试的耗时。
	// 计数是按「一次逻辑请求」记的，耗时也必须是——否则故障时请求数照涨、
	// 耗时却纹丝不动，监控看上去异常地健康
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError) // 第一次失败，触发重试
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	const backoff = 150 * time.Millisecond
	c := DefaultConfig()
	c.Trace, c.Metric = false, false
	c.RetryCount, c.RetryWaitTime, c.RetryMaxWaitTime = 2, backoff, backoff
	client, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	hist := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "probe_duration_seconds", Buckets: []float64{0.001, 10}},
		[]string{"method", "host", "status"})
	installMetrics(client, hist)
	client.AddRetryCondition(func(r *resty.Response, _ error) bool { return r.StatusCode() >= 500 })

	if _, err := client.R().Get(srv.URL); err != nil {
		t.Fatal(err)
	}
	if hits.Load() < 2 {
		t.Fatalf("应当重试过，服务端只收到 %d 次", hits.Load())
	}

	got := histogramSum(t, hist)
	if got < backoff.Seconds() {
		t.Errorf("耗时该覆盖整次逻辑请求（含退避 %v），got=%.3fs", backoff, got)
	}
}

func histogramSum(t *testing.T, h *prometheus.HistogramVec) float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(h); err != nil {
		t.Fatal(err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			if m.GetHistogram() != nil {
				return m.GetHistogram().GetSampleSum()
			}
		}
	}
	t.Fatal("没采到直方图样本")
	return 0
}

func TestNew_CtxCapsWholeLogicalRequest(t *testing.T) {
	// Timeout 管的是一次尝试。开了 RetryCount 之后，最坏情况是
	// (RetryCount+1) × Timeout 再加退避——配 300ms 实际能跑到 1.2s。
	// 唯一能给整次逻辑请求封顶的是调用方的 ctx，这里把它钉住。
	// 这里 Timeout 给 1s：不听 ctx 的话是 4s 起步，上界 2s 离 400ms 的预算和 4s 都远
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select { // 客户端断开就收手：srv.Close 要等在途请求跑完
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()

	c := DefaultConfig()
	c.Timeout = time.Second
	c.RetryCount = 3
	c.RetryWaitTime, c.RetryMaxWaitTime = 10*time.Millisecond, 20*time.Millisecond
	c.Trace, c.Metric = false, false
	cli, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := cli.R().SetContext(ctx).Get(srv.URL); err == nil {
		t.Fatal("该超时的")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("ctx 给了 400ms 的预算，重试不该把它撑到 %v", elapsed.Round(10*time.Millisecond))
	}
}

// keepGlobals 记下会被 install 改掉的那些全局值，测试结束还原
func keepGlobals(t *testing.T) {
	t.Helper()
	mu.Lock()
	oldCurrent, oldCloser := current, liveCloser
	mu.Unlock()
	t.Cleanup(func() {
		_ = closeXHttp(context.Background())
		mu.Lock()
		current, liveCloser = oldCurrent, oldCloser
		mu.Unlock()
	})
}

func TestInitXHttp_NoConfigInstallsUsableDefaultClient(t *testing.T) {
	// HTTP 客户端没配就不装的话，C() 每次退回兜底实例，
	// 超时和重试全是另一套值——而使用者没配本来就该是「用默认的」
	keepGlobals(t)
	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")

	if err := initXHttp(context.Background()); err != nil {
		t.Fatalf("没配不该报错：%v", err)
	}
	mu.Lock()
	got := current
	mu.Unlock()
	if got == nil {
		t.Fatal("没装上全局客户端")
	}
}

func TestInitXHttp_BadConfigFailsStartup(t *testing.T) {
	keepGlobals(t)
	xonetest.UseConfigYAML(t, "XHttp:\n  TimeOut: 3s\n")

	if err := initXHttp(context.Background()); err == nil {
		t.Fatal("字段拼错应当让启动失败")
	}
}

func TestInitXHttp_InvalidValueFailsStartup(t *testing.T) {
	// 负的时长是一个减号换来的静默故障：DialTimeout 写成负数时每一次请求
	// 当场 i/o timeout，Timeout 写成负数反而被标准库当成「不限时」，
	// 超时保护整个消失——两种都不会有任何迹象
	keepGlobals(t)
	for _, field := range []string{"Timeout", "DialTimeout", "IdleConnTimeout", "RetryWaitTime", "RetryMaxWaitTime"} {
		t.Run(field, func(t *testing.T) {
			xonetest.UseConfigYAML(t, "XHttp:\n  "+field+": -1s\n")
			if err := initXHttp(context.Background()); err == nil {
				t.Fatalf("%s 配成负数应当让启动失败", field)
			} else if !strings.Contains(err.Error(), field) {
				t.Errorf("错误里要点名是哪个字段，got=%v", err)
			} else if !xerror.Is(err, "xconfig") {
				// 在读配置时就拦下（xconfig.Unmarshal 调 Validate），不是等到 New
				t.Errorf("错误该出自读配置那一步，got=%v", err)
			}
		})
	}
}

func TestValidate_KeepAlive_AllowsNegative(t *testing.T) {
	// 这是唯一一个负值有意义的时长：标准库用它表示「不发探测」
	c := DefaultConfig()
	c.DialKeepAlive = -1
	if err := c.Validate(); err != nil {
		t.Errorf("DialKeepAlive 负值是合法的，got=%v", err)
	}
}

func TestInitXHttp_ConfigAppliedToGlobalClient(t *testing.T) {
	keepGlobals(t)
	xonetest.UseConfigYAML(t, "XHttp:\n  Timeout: 7s\n  RetryCount: 4\n")

	if err := initXHttp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := C().GetClient().Timeout; got != 7*time.Second {
		t.Errorf("超时没传到客户端上，got=%v", got)
	}
	if got := C().RetryCount; got != 4 {
		t.Errorf("重试次数没传到客户端上，got=%v", got)
	}
}

func TestCloseXHttp_FallsBackToDefaultAfterClose(t *testing.T) {
	// 摘掉之后 C() 还得能用：退出阶段里其它组件的关闭逻辑可能还要发请求
	keepGlobals(t)
	xonetest.UseConfigYAML(t, "XHttp:\n  Timeout: 7s\n")
	if err := initXHttp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := closeXHttp(context.Background()); err != nil {
		t.Fatalf("关闭不该报错：%v", err)
	}
	if C() == nil {
		t.Fatal("关完之后 C() 返回了 nil，调用方会当场空指针")
	}
	if got := C().GetClient().Timeout; got == 7*time.Second {
		t.Error("关完还拿得到已经关掉的那个客户端")
	}
}

func TestCloseXHttp_CloseWithoutInstallIsSafe(t *testing.T) {
	keepGlobals(t)
	mu.Lock()
	current, liveCloser = nil, nil
	mu.Unlock()
	if err := closeXHttp(context.Background()); err != nil {
		t.Errorf("want nil, got %v", err)
	}
}

// ---- 日志、链路属性、cookie ----

func TestNew_RestyLogsGoToSlogWithoutQuery(t *testing.T) {
	// 回归用例。resty 的 logger 默认写 os.Stderr（建 client 那一刻抓住的），
	// 开了重试之后每次失败都打一行 `WARN RESTY Get "http://…?token=…": …, Attempt 1`，
	// 最后再打一行 ERROR——绕开 slog，查询串里的令牌原样落盘
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = w
	var buf strings.Builder
	oldLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&lockedWriter{w: &buf}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(oldLog) })

	c := DefaultConfig()
	c.RetryCount, c.RetryWaitTime, c.RetryMaxWaitTime = 1, time.Millisecond, time.Millisecond
	c.Metric, c.Trace = false, false
	client, closer, err := New(c)
	if err != nil {
		os.Stderr = oldStderr
		t.Fatal(err)
	}
	defer closer.Close()
	_, reqErr := client.R().Get("http://127.0.0.1:1/x?token=hunter2")

	os.Stderr = oldStderr
	w.Close()
	stderr, _ := io.ReadAll(r)

	if reqErr == nil {
		t.Fatal("连一个没人监听的端口，该失败")
	}
	if len(stderr) > 0 {
		t.Errorf("resty 不该直接写 stderr，got=%s", stderr)
	}
	got := buf.String()
	if !strings.Contains(got, "Attempt") {
		t.Errorf("重试告警该进 slog，got=%s", got)
	}
	if strings.Contains(got, "hunter2") {
		t.Errorf("查询串里的令牌进了日志：%s", got)
	}
}

func TestNew_RestyLogsScrubErrorArgsStructurally(t *testing.T) {
	// Log 关着、resty 重试路径上的日志照打：参数里的 *url.Error 按结构去掉查询串。
	// Location 里问号前有空格，按文本认不出来
	srv, _ := echo(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/a b?token=hunter2")
		w.WriteHeader(http.StatusFound)
	})
	var buf strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&lockedWriter{w: &buf}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	c := DefaultConfig()
	c.RetryCount, c.RetryWaitTime, c.RetryMaxWaitTime = 1, time.Millisecond, time.Millisecond
	c.Metric, c.Trace = false, false
	client, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	client.SetRedirectPolicy(resty.NoRedirectPolicy())
	client.R().Get(srv.URL)
	got := buf.String()
	if !strings.Contains(got, "Attempt") || !strings.Contains(got, "/a%20b") {
		t.Fatalf("前提：重定向被拒也会重试，resty 打出每次尝试的那行，got=%s", got)
	}
	if strings.Contains(got, "hunter2") {
		t.Errorf("查询串里的令牌进了日志：%s", got)
	}
}

func TestRestyLogger_TextScrubbed(t *testing.T) {
	// 不是错误的参数、格式串本身里的 URL 按文本去掉查询串和 userinfo
	var buf strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&lockedWriter{w: &buf}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	restyLogger{}.Warnf("see %s and https://u:p@h/y#frag", "http://h/x?token=hunter2")
	if got := buf.String(); !strings.Contains(got, `see http://h/x and https://h/y`) {
		t.Errorf("resty 日志里的 URL 该去掉查询串、片段和 userinfo，got=%s", got)
	}
}

// lockedWriter resty 在自己的协程里打日志，测试这边同时在读
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestScrubText_Query(t *testing.T) {
	for in, want := range map[string]string{
		`Get "http://h/x?token=a&b=c": dial tcp: refused, Attempt 1`: `Get "http://h/x": dial tcp: refused, Attempt 1`,
		`Post "https://h:8443/a/b#frag": EOF`:                        `Post "https://h:8443/a/b": EOF`,
		`two http://a/x?k=1 and https://b/y?k=2 end`:                 `two http://a/x and https://b/y end`,
		`no url here?token=a`:                                        `no url here?token=a`,
	} {
		if got := scrubText(in); got != want {
			t.Errorf("scrubText(%q)\n got=%q\nwant=%q", in, got, want)
		}
	}
}

func TestTransport_SpanHasNoQueryAndIsNotNamedByPath(t *testing.T) {
	// 回归用例。otelhttp 往 url.full 里写的是完整 URL，查询串里的令牌跟着进了
	// 链路后端（它只去掉 user:password）；Span 名按真实路径起，
	// /users/42、/users/43 各是一个名字，基数随用户数增长
	old := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(old) })
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exp)))
	defer tp.Shutdown(context.Background())
	otel.SetTracerProvider(tp)

	srv, _ := echo(t, nil)
	client, _ := newQuiet(t, DefaultConfig())
	if _, err := client.R().Get(srv.URL + "/users/42?token=hunter2#frag"); err != nil {
		t.Fatal(err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("应产出一个 Span，got=%d", len(spans))
	}
	if spans[0].Name != "GET" {
		t.Errorf("出站 Span 名该只有方法，got=%q", spans[0].Name)
	}
	var full string
	for _, kv := range spans[0].Attributes {
		if strings.Contains(kv.Value.Emit(), "hunter2") {
			t.Errorf("属性 %s 里带出了令牌：%s", kv.Key, kv.Value.Emit())
		}
		if kv.Key == "url.full" {
			full = kv.Value.Emit()
		}
	}
	if full != srv.URL+"/users/42" {
		t.Errorf("url.full 该去掉查询串、保留其余部分，got=%q", full)
	}
}

func TestC_FallbackHasNoCookieJar(t *testing.T) {
	// 回归用例。兜底实例原先是 resty.New()，它自带一个 cookie jar，
	// 而配置出来的实例（NewWithClient）没有。于是初始化之前、关闭之后
	// 发的请求会把 A 服务种下的 cookie 带给之后的每一次调用——
	// 同一个进程里毫不相干的两次请求共享了会话
	emptyCell(t)
	var cookie atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/set" {
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "hunter2", Path: "/"})
			return
		}
		cookie.Store(r.Header.Get("Cookie"))
	}))
	t.Cleanup(srv.Close)

	if _, err := C().R().Get(srv.URL + "/set"); err != nil {
		t.Fatal(err)
	}
	if _, err := C().R().Get(srv.URL + "/get"); err != nil {
		t.Fatal(err)
	}
	if got, _ := cookie.Load().(string); got != "" {
		t.Errorf("兜底实例不该记住别的请求种下的 cookie，got=%q", got)
	}
	if RawClient().Jar != nil {
		t.Error("兜底实例的原生 client 不该带 cookie jar，要与配置出来的实例一致")
	}
}
