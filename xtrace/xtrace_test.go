package xtrace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/xapp"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xlog"
	"github.com/jupiter-eng/xone/xonetest"
)

// recorder 收集 Span 的 SpanProcessor，用来断言「Span 真的到了处理器手里」
type recorder struct {
	mu    sync.Mutex
	spans []string
	shut  bool
}

func (r *recorder) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (r *recorder) OnEnd(s sdktrace.ReadOnlySpan) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spans = append(r.spans, s.Name())
}
func (r *recorder) Shutdown(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.shut = true
	return nil
}
func (r *recorder) ForceFlush(context.Context) error { return nil }
func (r *recorder) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.spans...)
}
func (r *recorder) isShut() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.shut
}

func TestNew_DefaultConfigYieldsRealProvider(t *testing.T) {
	rec := &recorder{}
	tr, closer, err := New(context.Background(), DefaultConfig(), rec)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tr.TracerProvider.(*sdktrace.TracerProvider); !ok {
		t.Fatalf("开启时应是 SDK 实现，got=%T", tr.TracerProvider)
	}

	_, span := tr.TracerProvider.Tracer("t").Start(context.Background(), "干活")
	if !span.SpanContext().IsValid() {
		t.Error("默认全采样，SpanContext 应有效")
	}
	span.End()

	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	if got := rec.names(); len(got) != 1 || got[0] != "干活" {
		t.Errorf("Span 应送到注册的处理器，got=%v", got)
	}
	if !rec.isShut() {
		t.Error("关闭 provider 应一并关掉挂在它上面的处理器")
	}
}

func TestNew_NoopWhenTracingDisabled(t *testing.T) {
	c := DefaultConfig()
	c.Enable = false
	rec := &recorder{}
	tr, closer, err := New(context.Background(), c, rec)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	if _, ok := tr.TracerProvider.(*sdktrace.TracerProvider); ok {
		t.Error("关闭时不该建出 SDK provider")
	}
	_, span := tr.TracerProvider.Tracer("t").Start(context.Background(), "干活")
	span.End()
	if len(rec.names()) != 0 {
		t.Errorf("关闭时处理器不该收到 Span，got=%v", rec.names())
	}
}

func TestNew_TracingDisabledKeepsHeaderForwarding(t *testing.T) {
	// X-Request-Id 该不该带给下游，跟要不要采样 Span 是两个问题
	c := DefaultConfig()
	c.Enable = false
	c.ForwardHeaders = []string{"X-Request-Id"}
	tr, closer, err := New(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	out := http.Header{}
	ctx := tr.Propagator.Extract(context.Background(), trusted(http.Header{"X-Request-Id": {"r1"}}))
	tr.Propagator.Inject(ctx, headerCarrier(out))
	if out.Get("X-Request-Id") != "r1" {
		t.Errorf("链路关了，透传还得在，got=%v", out)
	}
	if out.Get("traceparent") != "" {
		t.Errorf("链路关了就不该注入 traceparent，got=%v", out)
	}
}

func TestNew_InstalledPropagatorAcceptsHeadersOnlyFromTrustedPeers(t *testing.T) {
	// 与 HeaderPropagator 自己的用例互补：这里走的是 New 组装出来的那个组合
	// Propagator——carrier 要穿过 TraceContext、Baggage、B3 才到它手上，
	// 可信的记号在路上不能丢，不可信的也不能被别的 propagator 放行
	c := DefaultConfig()
	c.ForwardHeaders = []string{"X-Tenant-Id"}
	c.ForwardHeaderRules = []ForwardHeaderRule{{Domains: []string{"*.internal.com"}, Headers: []string{"X-Internal-Token"}}}
	tr, closer, err := New(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	in := http.Header{"X-Tenant-Id": {"forged"}, "X-Internal-Token": {"forged"}}
	for _, from := range []struct {
		name    string
		carrier propagation.TextMapCarrier
		want    bool
	}{
		{"不可信", headerCarrier(in), false},
		{"可信", trusted(in), true},
	} {
		ctx := tr.Propagator.Extract(context.Background(), from.carrier)
		out := http.Header{}
		tr.Propagator.Inject(WithTargetHost(ctx, "api.internal.com"), headerCarrier(out))
		for _, h := range []string{"X-Tenant-Id", "X-Internal-Token"} {
			if got := out.Get(h) != ""; got != from.want {
				t.Errorf("%s的对端：%s 透传=%v，want %v", from.name, h, got, from.want)
			}
		}
	}
}

func TestNew_InstalledPropagatorAcceptsBaggageOnlyFromTrustedPeers(t *testing.T) {
	// baggage 和透传 Header 是同一种东西：上游给的键值原样带进每一次调用。
	// 谁发来的都收的话，ForwardHeaders 挡在门外的 X-Tenant-Id 改写成
	// baggage: tenant=… 照样进了内网。traceparent 只是链路标识，谁发来的都接
	tr, closer, err := New(context.Background(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	in := http.Header{
		"Baggage":     {"tenant=forged"},
		"Traceparent": {"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
	}
	for _, from := range []struct {
		name    string
		carrier propagation.TextMapCarrier
		want    string
	}{
		{"不可信", headerCarrier(in), ""},
		{"可信", trusted(in), "tenant=forged"},
	} {
		ctx := tr.Propagator.Extract(context.Background(), from.carrier)
		out := http.Header{}
		tr.Propagator.Inject(ctx, headerCarrier(out))
		if got := out.Get("Baggage"); got != from.want {
			t.Errorf("%s的对端：下游收到 baggage=%q，want %q", from.name, got, from.want)
		}
		if !strings.Contains(out.Get("Traceparent"), "4bf92f3577b34da6a3ce929d0e0e4736") {
			t.Errorf("%s的对端：traceparent 不受信任边界影响，got=%q", from.name, out.Get("Traceparent"))
		}
	}
}

func TestTrustedBaggage_WarnsOnceForUntrustedPeerBaggage(t *testing.T) {
	var buf strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	b := &trustedBaggage{}
	b.Extract(context.Background(), headerCarrier(http.Header{"X-Other": {"x"}}))
	if buf.Len() != 0 {
		t.Fatalf("没带 baggage 就不该告警，got=%s", buf.String())
	}
	for range 3 {
		b.Extract(context.Background(), headerCarrier(http.Header{"Baggage": {"k=v"}}))
	}
	if n := strings.Count(buf.String(), "xtrace ignored baggage from an untrusted peer"); n != 1 {
		t.Errorf("该只告警一次，got=%d 次：%s", n, buf.String())
	}
	if want := "xtrace ignored baggage from an untrusted peer, only peers in the web server's TrustedProxies (XGin / XEcho) are trusted"; !strings.Contains(buf.String(), want) {
		t.Errorf("告警该写成 %q，got=%s", want, buf.String())
	}
}

func TestNew_InstallsW3CAndB3WhenEnabled(t *testing.T) {
	tr, closer, err := New(context.Background(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	ctx, span := tr.TracerProvider.Tracer("t").Start(context.Background(), "s")
	defer span.End()
	out := http.Header{}
	tr.Propagator.Inject(ctx, headerCarrier(out))

	for _, h := range []string{"Traceparent", "B3"} {
		if out.Get(h) == "" {
			t.Errorf("应注入 %s，got=%v", h, out)
		}
	}
}

func TestNew_ContradictoryForwardingConfigFails(t *testing.T) {
	c := DefaultConfig()
	c.ForwardHeaders = []string{"X-Internal-Token"}
	c.ForwardHeaderRules = []ForwardHeaderRule{{Domains: []string{"*.internal.com"}, Headers: []string{"X-Internal-Token"}}}

	if _, _, err := New(context.Background(), c); err == nil {
		t.Fatal("矛盾的配置应当在启动时就失败，而不是猜一个语义跑下去")
	}
}

func TestNew_ZeroStopBudgetFails(t *testing.T) {
	// 0 不是「不限时」而是「一点都不等」：Shutdown 拿到一个已经过期的 context，
	// 缓冲区里还没发出去的 Span 直接丢掉，而配置文件看上去只是没设上限
	c := DefaultConfig()
	c.ShutdownTimeout = 0
	if _, _, err := New(context.Background(), c); err == nil {
		t.Fatal("ShutdownTimeout=0 应当报错")
	}
}

func TestNew_ZeroSampleRateStillGeneratesAndPropagatesTraceID(t *testing.T) {
	// 「不采样」和「关掉链路」是两件事：写 0 时 Span 照常创建、TraceID 照常
	// 生成，只是不落地——下游拿得到 TraceID，本地不存 Span。
	// 要连 Span 都不产生请用 Enable: false
	c := DefaultConfig()
	c.SampleRatio = 0
	tr, closer, err := New(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	_, span := tr.TracerProvider.Tracer("t").Start(context.Background(), "s")
	defer span.End()
	sc := span.SpanContext()
	if !sc.IsValid() {
		t.Fatal("采样率为 0 时 SpanContext 仍应有效，否则 TraceID 传不下去")
	}
	if sc.IsSampled() {
		t.Error("采样率为 0 时不该被采样")
	}
}

func TestSamplerOf(t *testing.T) {
	// 每一档都要尊重上游的决定，全采样也不例外
	for _, r := range []float64{0, 0.1, 1} {
		if got := samplerOf(r).Description(); !strings.HasPrefix(got, "ParentBased{") {
			t.Errorf("SampleRatio=%v 该是 ParentBased，got=%s", r, got)
		}
	}
	// TraceIDRatioBased(1) 本身就是全采样，不需要为 1 单开一支——钉住这个前提
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(samplerOf(1)))
	defer tp.Shutdown(context.Background())
	for i := 0; i < 1000; i++ {
		_, span := tp.Tracer("t").Start(context.Background(), "s")
		if !span.SpanContext().IsSampled() {
			t.Fatal("SampleRatio=1 时根 Span 该全采样")
		}
		span.End()
	}
}

func TestNew_FullSamplingRespectsUpstreamNotSampled(t *testing.T) {
	// 回归用例。SampleRatio>=1 原先用的是 AlwaysSample，不看父 Span：
	// 上游传来 sampled=00，我们照样采，再以 -01 往下游传——
	// 上游的采样决定在我们这里被推翻，整条链路要么断成两截，要么把
	// 上游刻意丢掉的流量又捡回来塞给了链路后端
	for _, ratio := range []float64{1, 0.5} {
		c := DefaultConfig()
		c.SampleRatio = ratio
		tr, closer, err := New(context.Background(), c)
		if err != nil {
			t.Fatal(err)
		}

		h := http.Header{"Traceparent": {"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00"}}
		ctx := tr.Propagator.Extract(context.Background(), headerCarrier(h))
		ctx, span := tr.TracerProvider.Tracer("t").Start(ctx, "s")
		if span.SpanContext().IsSampled() {
			t.Errorf("SampleRatio=%v：上游说了不采，我们不该采", ratio)
		}
		out := http.Header{}
		tr.Propagator.Inject(ctx, headerCarrier(out))
		if tp := out.Get("Traceparent"); !strings.HasSuffix(tp, "-00") {
			t.Errorf("SampleRatio=%v：往下游传的也该是不采样，got=%s", ratio, tp)
		}
		span.End()
		closer.Close()
	}
}

func TestNew_OutOfRangeSampleRateFails(t *testing.T) {
	// 原先 >1 当作全采样、负数当作不采样、NaN 更是什么都不像——
	// 都是写错了的配置，却能静默跑起来
	for _, r := range []float64{-0.1, 1.5, math.NaN(), math.Inf(1)} {
		c := DefaultConfig()
		c.SampleRatio = r
		_, _, err := New(context.Background(), c)
		if err == nil {
			t.Errorf("SampleRatio=%v 应当报错", r)
			continue
		}
		// 一个模块边界一个 xerror：validate 自己返回普通 error，由 New 包一次
		if n := strings.Count(err.Error(), "xtrace"); n != 1 {
			t.Errorf("模块名该只出现一次，got=%v", err)
		}
	}
}

func TestNew_StartsWhenSomeResourceAttrsFail(t *testing.T) {
	// 回归用例。resource.New 在部分探测失败时返回 ErrPartialResource，
	// 同时给出采到的那部分。原先一律当作致命错误：OTEL_RESOURCE_ATTRIBUTES
	// 写错一个字符、或者容器里 user.Current 查不到随机 UID，服务就起不来——
	// 为了几个描述性的属性把整个进程挡在门外
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment=prod,this-is-not-key-value")
	rec := tracetest.NewSpanRecorder()
	tr, closer, err := New(context.Background(), DefaultConfig(), rec)
	if err != nil {
		t.Fatalf("部分属性采不到不该让启动失败：%v", err)
	}
	defer closer.Close()
	if _, ok := tr.TracerProvider.(*sdktrace.TracerProvider); !ok {
		t.Fatalf("该照常装上真的 provider，got=%T", tr.TracerProvider)
	}

	// 用的是采到的那部分，不是一个空的 resource：写对的那一项、
	// 其余探测器的属性、我们自己的 service.name 都还在
	_, span := tr.TracerProvider.Tracer("t").Start(context.Background(), "s")
	span.End()
	ended := rec.Ended()
	if len(ended) != 1 {
		t.Fatalf("应产出一个 Span，got=%d", len(ended))
	}
	got := map[string]string{}
	for _, kv := range ended[0].Resource().Attributes() {
		got[string(kv.Key)] = kv.Value.Emit()
	}
	if got["deployment.environment"] != "prod" {
		t.Errorf("写对的那一项该照常生效，got=%v", got)
	}
	for _, k := range []string{"service.name", "telemetry.sdk.language"} {
		if _, ok := got[k]; !ok {
			t.Errorf("其余属性该都还在，缺 %s，got=%v", k, got)
		}
	}
}

func TestCloseXTrace_HonorsFrameworkDeadline(t *testing.T) {
	// 回归用例。关闭原先用 Background + ShutdownTimeout，不看框架传进来的 ctx：
	// 框架的停止预算只剩 100ms 时，这里照样等满自己的 5s，
	// 后面还没关的组件被挤掉，K8s 的宽限期一到整个进程被 SIGKILL
	keepGlobals(t)
	resetRegistration(t)
	AddSpanProcessor(blockingProcessor{})
	c := DefaultConfig()
	c.ShutdownTimeout = 5 * time.Second
	if err := install(context.Background(), c); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := closeXTrace(ctx)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("该按框架给的截止时间收手，实际等了 %v", elapsed)
	}
	if err == nil {
		t.Error("导出端卡住时应如实报错")
	}
}

func TestNew_SampleRateTakesEffect(t *testing.T) {
	c := DefaultConfig()
	c.SampleRatio = 0.5
	tr, closer, err := New(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	// 采样一半时，一千次里既该有采到的也该有没采到的
	var sampled, dropped int
	for i := 0; i < 1000; i++ {
		_, span := tr.TracerProvider.Tracer("t").Start(context.Background(), "s")
		if span.SpanContext().IsSampled() {
			sampled++
		} else {
			dropped++
		}
		span.End()
	}
	if sampled == 0 || dropped == 0 {
		t.Errorf("采样率 0.5 应当有采有丢，sampled=%d dropped=%d", sampled, dropped)
	}
}

func TestClose_DoesNotHangOnTimeout(t *testing.T) {
	// 导出端不可达时 Shutdown 会一直阻塞，没有 deadline 就是退出时挂死
	c := DefaultConfig()
	c.ShutdownTimeout = 50 * time.Millisecond
	_, closer, err := New(context.Background(), c, blockingProcessor{})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- closer.Close() }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("导出端卡住时应返回超时错误，而不是假装关干净了")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close 没有在超时后返回")
	}
}

// blockingProcessor 模拟一个连不上导出端、Shutdown 一直等的处理器
type blockingProcessor struct{}

func (blockingProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (blockingProcessor) OnEnd(sdktrace.ReadOnlySpan)                     {}
func (blockingProcessor) ForceFlush(context.Context) error                { return nil }
func (blockingProcessor) Shutdown(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestInstall_LogsGetTraceID(t *testing.T) {
	// xlog 不依赖 OpenTelemetry，日志里的 trace_id 全靠本包注入这个提取器
	t.Cleanup(func() { xlog.SetTraceExtractor(nil) })

	tr, closer, err := New(context.Background(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	tr.Install()

	ctx, span := otel.Tracer("t").Start(context.Background(), "s")
	defer span.End()

	dir := t.TempDir()
	lc := xlog.DefaultConfig()
	lc.Console = false
	lc.File = xlog.FileConfig{Enable: true, Path: dir, Name: "app.log", RotateTime: time.Hour, Perm: "0644"}
	l, lcloser, err := xlog.New(lc)
	if err != nil {
		t.Fatal(err)
	}
	l.InfoContext(ctx, "带链路的日志")
	lcloser.Close()

	if got := readLog(t, dir); got[traceIDKey] != span.SpanContext().TraceID().String() {
		t.Errorf("日志里的 trace_id 应等于当前 Span 的，got=%v", got)
	}
}

func TestTransport_WritesTargetHostToCtx(t *testing.T) {
	// 没有它，ForwardHeaderRules 里的 header 一条都不会被注入
	var seen string
	tr := &Transport{Next: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		seen = TargetHostFromContext(r.Context())
		return &http.Response{StatusCode: 204, Body: io.NopCloser(nilReader{})}, nil
	})}

	req, _ := http.NewRequest("GET", "https://api.example.com:8443/x", nil)
	before := req.Context()
	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if seen != "api.example.com:8443" {
		t.Errorf("应写入带端口的 host，got=%q", seen)
	}
	if TargetHostFromContext(before) != "" {
		t.Error("按 RoundTripper 约定，不能改动入参请求")
	}
}

func TestTransport_NextNilUsesDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL, nil)
	resp, err := (&Transport{}).RoundTrip(req)
	if err != nil {
		t.Fatalf("Next 为空时应回落到 http.DefaultTransport，got=%v", err)
	}
	resp.Body.Close()
}

func TestAddSpanProcessor(t *testing.T) {
	t.Run("nil 直接 panic", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("传 nil 应当 panic，不留到运行时才空指针")
			}
		}()
		AddSpanProcessor(nil)
	})

	t.Run("初始化前注册的会被挂上", func(t *testing.T) {
		resetRegistration(t)
		rec := &recorder{}
		AddSpanProcessor(rec)

		initComponent(t)
		otel.Tracer("t").Start(context.Background(), "先注册")
		_, span := otel.Tracer("t").Start(context.Background(), "先注册")
		span.End()
		_ = closeXTrace(context.Background())

		if len(rec.names()) == 0 {
			t.Error("初始化前注册的处理器应在初始化时挂上")
		}
	})

	t.Run("初始化后注册的立即生效", func(t *testing.T) {
		resetRegistration(t)
		initComponent(t)

		rec := &recorder{}
		AddSpanProcessor(rec)
		_, span := otel.Tracer("t").Start(context.Background(), "后注册")
		span.End()
		_ = closeXTrace(context.Background())

		if got := rec.names(); len(got) != 1 || got[0] != "后注册" {
			t.Errorf("初始化后注册的应立即收到 Span，got=%v", got)
		}
	})

	t.Run("关闭之后注册的进待办，不挂到已关闭的实例上", func(t *testing.T) {
		resetRegistration(t)
		initComponent(t)
		_ = closeXTrace(context.Background())

		rec := &recorder{}
		AddSpanProcessor(rec)

		mu.Lock()
		n, l := len(pending), live
		mu.Unlock()
		if l != nil {
			t.Error("关闭后应清掉 provider")
		}
		if n != 1 {
			t.Errorf("关闭后注册的应进待办队列，got=%d", n)
		}
	})
}

func TestAddSpanProcessor_NotLostWhenConcurrentWithInit(t *testing.T) {
	// 注册分两支：初始化前进 pending 等着被取走，初始化后直接挂到 live 上。
	// 如果取 pending 和装 live 之间放开了锁，落在那个窗口里的注册两边都不占——
	// 它进了一个再也不会被读的 pending，然后被静默丢掉。
	// 这是一次完全无声的失败：Span 照常产生，只是永远到不了上报端。
	for i := 0; i < 50; i++ {
		resetRegistration(t)

		rec := &recorder{}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); initComponent(t) }()
		go func() { defer wg.Done(); AddSpanProcessor(rec) }()
		wg.Wait()

		// 无论两者谁先，注册完成之后处理器都该是挂上了的：
		// 要么被 Init 从 pending 里取走，要么直接挂到了 live 上
		mu.Lock()
		leftover := len(pending)
		mu.Unlock()
		if leftover > 0 {
			_ = closeXTrace(context.Background())
			t.Fatalf("第 %d 轮：处理器落在窗口里没人认领，pending 还剩 %d 个", i, leftover)
		}

		_, span := otel.Tracer("t").Start(context.Background(), "s")
		span.End()
		got := len(rec.names())
		_ = closeXTrace(context.Background())
		if got == 0 {
			t.Fatalf("第 %d 轮：处理器被吞了，一个 Span 都没收到", i)
		}
	}
}

func TestRegister_RegistrationMatchesFramework(t *testing.T) {
	// 这是本包和框架之间唯一的一根线：钩子漏登记、档位挂错，
	// 表现是「配置不生效」或者「比用它的东西晚就绪」，别处都测不出来
	var got *hook.Entry
	for _, e := range hook.Start() {
		if e.Pkg == "github.com/jupiter-eng/xone/xtrace" {
			got = &e
			break
		}
	}
	if got == nil {
		t.Fatal("没有登记启动钩子")
	}
	if got.Stage != hook.StageTelemetry {
		t.Errorf("链路要早于各类客户端就绪，否则它们的 Span 挂不上，got=%v", got.Stage)
	}

	var stopped bool
	for _, e := range hook.Stop() {
		if e.Pkg == "github.com/jupiter-eng/xone/xtrace" {
			stopped = true
		}
	}
	if stopped != true {
		t.Errorf("停止钩子登记情况不对，got=%v", stopped)
	}
}

func TestInit_UsesAppNameAsServiceName(t *testing.T) {
	// 服务名放在共用的 App 块里，链路和指标读同一份，不会各配一遍再对不上。
	// 这里走完整路径：配置文件 → config.Load → 组件 Init → OTel resource
	resetRegistration(t)
	loadConfig(t, "XApp:\n  Name: xone.demo.app\n  Version: v1.2.0\nXTrace:\n  SampleRatio: 1\n")

	if xapp.Name() != "xone.demo.app" {
		t.Fatalf("配置没进到 xapp，got=%q", xapp.Name())
	}

	exp := tracetest.NewInMemoryExporter()
	AddSpanProcessor(sdktrace.NewSimpleSpanProcessor(exp))

	initComponent(t)
	_, span := otel.Tracer("t").Start(context.Background(), "s")
	span.End()
	// 先取再关：InMemoryExporter 的 Shutdown 会清空已收集的 Span
	spans := exp.GetSpans()
	_ = closeXTrace(context.Background())

	if len(spans) == 0 {
		t.Fatal("没收到 Span")
	}
	attrs := map[string]string{}
	for _, kv := range spans[0].Resource.Attributes() {
		attrs[string(kv.Key)] = kv.Value.AsString()
	}
	if attrs["service.name"] != "xone.demo.app" {
		t.Errorf("service.name 应取自 App.Name，got=%q", attrs["service.name"])
	}
	if attrs["service.version"] != "v1.2.0" {
		t.Errorf("service.version 应取自 App.Version，got=%q", attrs["service.version"])
	}
}

// ---- 助手 ----

// resetRegistration 把包级登记状态清干净，让每个子测试从同一起点开始
func resetRegistration(t *testing.T) {
	t.Helper()
	reset := func() {
		mu.Lock()
		pending, live = nil, nil
		mu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

// initComponent 走一遍框架真正会走的路径：按默认配置装好链路设施。
//
// 直接调 install 而不是 initXTrace：后者要先从已加载的配置里读，
// 而这些用例关心的是装配本身，不是配置怎么来的
func initComponent(t *testing.T) {
	t.Helper()
	if err := install(context.Background(), DefaultConfig()); err != nil {
		t.Errorf("初始化失败：%v", err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type nilReader struct{}

func (nilReader) Read([]byte) (int, error) { return 0, errors.New("EOF") }

var _ oteltrace.TracerProvider = (*sdktrace.TracerProvider)(nil)

// headerCarrier 让测试少写一个 import
func headerCarrier(h http.Header) propagation.HeaderCarrier { return propagation.HeaderCarrier(h) }

// traceIDKey 日志里链路字段的名字，由 xlog 的 handler 决定
const traceIDKey = "trace_id"

// readLog 读出目录下唯一一条日志，解析成 map
func readLog(t *testing.T, dir string) map[string]any {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "app.log.*"))
	if err != nil || len(files) == 0 {
		t.Fatalf("没找到日志文件：%v", err)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(b))
	out := map[string]any{}
	if err := json.Unmarshal([]byte(line), &out); err != nil {
		t.Fatalf("日志不是 JSON：%v，内容=%q", err, line)
	}
	return out
}

// loadConfig 把一段 YAML 走真实的加载路径灌进来
func loadConfig(t *testing.T, yml string) {
	t.Helper()
	xonetest.UseConfigYAML(t, yml)

	// 配置是各包自己在启动钩子里读的，所以要把钩子也走一遍——
	// 这个用例关心的正是 xapp 读到的服务名会进到链路里
	for _, e := range hook.Start() {
		if e.Pkg == "github.com/jupiter-eng/xone/xapp" {
			if err := e.Run(context.Background()); err != nil {
				t.Fatalf("%s 失败：%v", e.Name, err)
			}
		}
	}
}

func TestClose_ClosingStandaloneInstanceLeavesGlobalAlone(t *testing.T) {
	// New 出来的实例不一定是装到全局的那个——测试要一套干净的链路设施、
	// 或者同时存在两套配置时都会这样。关掉其中一个曾经把全局那个也抹掉，
	// 之后每一次 AddSpanProcessor 都挂到 pending 上再也没人读：
	// Span 照常产生、永远到不了上报端，而且没有任何迹象
	reset := func() { mu.Lock(); live, pending = nil, nil; mu.Unlock() }
	reset()
	t.Cleanup(reset)

	c := DefaultConfig()
	c.Enable = true
	if err := install(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeXTrace(context.Background()) })

	mu.Lock()
	installed := live
	mu.Unlock()
	if installed == nil {
		t.Fatal("start 之后 live 该有值")
	}

	// 另造一个，从没 Install 过，只把它关掉
	_, standalone, err := New(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if err := standalone.Close(); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	after := live
	mu.Unlock()
	if after != installed {
		t.Errorf("关掉一个从没装过的实例，把全局装着的那个清掉了：前 %p 后 %p", installed, after)
	}

	// 全局还活着的话，后来的 SpanProcessor 该挂到它上面而不是 pending
	AddSpanProcessor(noopProcessor{})
	mu.Lock()
	n := len(pending)
	mu.Unlock()
	if n != 0 {
		t.Errorf("SpanProcessor 落到了没人读的 pending 上，pending=%d", n)
	}
}

// noopProcessor 什么都不做的 SpanProcessor
type noopProcessor struct{}

func (noopProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (noopProcessor) OnEnd(sdktrace.ReadOnlySpan)                     {}
func (noopProcessor) Shutdown(context.Context) error                  { return nil }
func (noopProcessor) ForceFlush(context.Context) error                { return nil }

// keepGlobals 记下会被 install 改掉的那些全局值，测试结束还原
func keepGlobals(t *testing.T) {
	t.Helper()
	oldTP := otel.GetTracerProvider()
	oldProp := otel.GetTextMapPropagator()
	mu.Lock()
	oldPending, oldLive := pending, live
	mu.Unlock()
	t.Cleanup(func() {
		_ = closeXTrace(context.Background())
		otel.SetTracerProvider(oldTP)
		otel.SetTextMapPropagator(oldProp)
		mu.Lock()
		pending, live = oldPending, oldLive
		mu.Unlock()
	})
}

func TestInitXTrace_InstallsDefaultTracingWhenUnconfigured(t *testing.T) {
	// 链路没配就不装的话，AddSpanProcessor 登记的处理器会一直挂在 pending 上
	// 再也没人读，Span 照常产生却永远到不了上报端
	keepGlobals(t)
	loadConfig(t, "XApp:\n  Name: demo\n")

	if err := initXTrace(context.Background()); err != nil {
		t.Fatalf("没配不该报错：%v", err)
	}
	mu.Lock()
	got := live
	mu.Unlock()
	if got == nil {
		t.Fatal("没装上全局 provider")
	}
}

func TestInitXTrace_ConfigTypoFailsStartup(t *testing.T) {
	keepGlobals(t)
	loadConfig(t, "XTrace:\n  Sample: 0.5\n")

	if err := initXTrace(context.Background()); err == nil {
		t.Fatal("字段拼错应当让启动失败")
	}
}

func TestInitXTrace_InvalidValueFailsStartup(t *testing.T) {
	// 0 不是「不限时」而是「一点都不等」：Shutdown 拿到一个已经过期的
	// context，缓冲区里还没发出去的 Span 会被直接丢掉
	keepGlobals(t)
	loadConfig(t, "XTrace:\n  ShutdownTimeout: 0\n")

	if err := initXTrace(context.Background()); err == nil {
		t.Fatal("ShutdownTimeout 配成 0 应当让启动失败")
	}
}

func TestInitXTrace_BadForwardingRulesFailAtConfigRead(t *testing.T) {
	// Validate 连透传规则一起查：xconfig.Unmarshal 读这一块时就拦下，
	// 错误出自 xconfig、点名是哪一块，不必等到装配 Propagator
	for name, conf := range map[string]string{
		"通配写错":    "XTrace:\n  ForwardHeaderRules:\n    - Domains: [\"*trusted.com\"]\n      Headers: [X-Internal-Token]\n",
		"一个头两边都写": "XTrace:\n  ForwardHeaders: [X-Token]\n  ForwardHeaderRules:\n    - Domains: [api.internal.com]\n      Headers: [x-token]\n",
		"采样率越界":   "XTrace:\n  SampleRatio: 2\n",
	} {
		t.Run(name, func(t *testing.T) {
			keepGlobals(t)
			loadConfig(t, conf)
			err := initXTrace(context.Background())
			if err == nil {
				t.Fatal("配错的 XTrace 块应当在读配置时就失败")
			}
			if !xerror.Is(err, "xconfig") || !strings.Contains(err.Error(), ConfigKey) {
				t.Errorf("错误该出自 xconfig 并点名 %s，got=%v", ConfigKey, err)
			}
		})
	}
}

func TestInitXTrace_ConfigAppliedToGlobalProvider(t *testing.T) {
	keepGlobals(t)
	loadConfig(t, "XApp:\n  Name: xone.demo.app\n  Version: v9.9.9\nXTrace:\n  Enable: true\n")

	if err := initXTrace(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); !ok {
		t.Errorf("全局 provider 没换成本模块建的那个，got=%T", otel.GetTracerProvider())
	}
	if err := closeXTrace(context.Background()); err != nil {
		t.Errorf("关闭不该报错：%v", err)
	}
}

func TestCloseXTrace_ClosesWithoutInit(t *testing.T) {
	keepGlobals(t)
	mu.Lock()
	live = nil
	mu.Unlock()
	if err := closeXTrace(context.Background()); err != nil {
		t.Errorf("want nil, got %v", err)
	}
}

// idleCloser 一个会记账的 RoundTripper
type idleCloser struct {
	roundTripperFunc
	closed int
}

func (c *idleCloser) CloseIdleConnections() { c.closed++ }

func TestTransport_CloseIdleConnections_ForwardsToUnderlying(t *testing.T) {
	// http.Client.CloseIdleConnections() 是靠类型断言找这个方法的：
	// 包一层却不转发，断言仍然成立（本类型有这个方法），但调用变成空操作——
	// 而链路默认开着，也就是默认情况下退出时空闲连接根本没被清掉
	inner := &idleCloser{roundTripperFunc: func(*http.Request) (*http.Response, error) { return nil, nil }}
	tr := &Transport{Next: inner}

	tr.CloseIdleConnections()
	if inner.closed != 1 {
		t.Errorf("底层的 CloseIdleConnections 没被调到，got=%d", inner.closed)
	}

	// 走 http.Client 那条真实路径再确认一次：它断言的是本类型
	(&http.Client{Transport: tr}).CloseIdleConnections()
	if inner.closed != 2 {
		t.Errorf("http.Client 那条路径没转下去，got=%d", inner.closed)
	}
}

func TestTransport_CloseIdleConnections_NoPanicWhenUnderlyingLacksIt(t *testing.T) {
	tr := &Transport{Next: roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, nil })}
	tr.CloseIdleConnections() // 不 panic 就是通过
}

func TestTransport_CloseIdleConnections_ForwardsToDefaultWithoutNext(t *testing.T) {
	(&Transport{}).CloseIdleConnections() // 不 panic 就是通过
}

// resourceAttrs 按默认配置建一套链路设施，取出 Span 上带的 resource 属性
func resourceAttrs(t *testing.T) map[string]string {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tr, closer, err := New(context.Background(), DefaultConfig(), rec)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	_, span := tr.TracerProvider.Tracer("t").Start(context.Background(), "s")
	span.End()
	ended := rec.Ended()
	if len(ended) != 1 {
		t.Fatalf("应产出一个 Span，got=%d", len(ended))
	}
	got := map[string]string{}
	for _, kv := range ended[0].Resource().Attributes() {
		got[string(kv.Key)] = kv.Value.Emit()
	}
	return got
}

// useApp 经真实的配置路径设好 App 块，测试结束恢复成没配
func useApp(t *testing.T, name, version string) {
	t.Helper()
	set := func(n, v string) {
		loadConfig(t, fmt.Sprintf("XApp:\n  Name: %q\n  Version: %q\n", n, v))
	}
	set(name, version)
	t.Cleanup(func() { set("", "") })
}

func TestNew_Resource_NoEmptyServiceNameWithoutAppName(t *testing.T) {
	// 回归用例。App.Name 没配时原先照样写进 service.name=""，
	// OTel 自己的 unknown_service 兜底名被盖掉，看板上多出一个无名服务
	useApp(t, "", "")
	t.Setenv("OTEL_SERVICE_NAME", "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")

	got := resourceAttrs(t)
	if !strings.HasPrefix(got["service.name"], "unknown_service:") {
		t.Errorf("没配应用名时该落到 OTel 的兜底名，got=%q", got["service.name"])
	}
	if v, ok := got["service.version"]; ok {
		t.Errorf("没配版本就不该写 service.version，got=%q", v)
	}
}

func TestNew_Resource_UsesOTEL_SERVICE_NAMEWithoutAppName(t *testing.T) {
	useApp(t, "", "")
	t.Setenv("OTEL_SERVICE_NAME", "from-env")

	if got := resourceAttrs(t); got["service.name"] != "from-env" {
		t.Errorf("service.name 该取自 OTEL_SERVICE_NAME，got=%q", got["service.name"])
	}
}

func TestNew_Resource_OTelEnvOverridesAppConfig(t *testing.T) {
	// 环境变量是部署方的最后一句话，应当压过打进镜像的配置文件
	useApp(t, "from-config", "v1.0.0")
	t.Setenv("OTEL_SERVICE_NAME", "from-env")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.version=v2.0.0")

	got := resourceAttrs(t)
	if got["service.name"] != "from-env" {
		t.Errorf("OTEL_SERVICE_NAME 该压过 App.Name，got=%q", got["service.name"])
	}
	if got["service.version"] != "v2.0.0" {
		t.Errorf("OTEL_RESOURCE_ATTRIBUTES 该压过 App.Version，got=%q", got["service.version"])
	}
}

func TestNew_TracingDisabledStillShutsDownProcessors(t *testing.T) {
	// 回归用例。链路关着时原先交回空操作的 Closer，传进来的处理器
	// 从来没被 Shutdown：exporter 持有的连接和协程退出时没人收
	c := DefaultConfig()
	c.Enable = false
	rec := &recorder{}
	_, closer, err := New(context.Background(), c, rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	if !rec.isShut() {
		t.Error("关闭时该 Shutdown 传进来的处理器")
	}
}

func TestCloseXTrace_TracingDisabledStillClosesRegisteredProcessors(t *testing.T) {
	// AddSpanProcessor 承诺过由本包关掉它们，链路开关与否都一样
	keepGlobals(t)
	resetRegistration(t)
	before, after := &recorder{}, &recorder{}
	AddSpanProcessor(before)
	c := DefaultConfig()
	c.Enable = false
	if err := install(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	AddSpanProcessor(after)

	if err := closeXTrace(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !before.isShut() {
		t.Error("初始化前登记的处理器该在关闭时被 Shutdown")
	}
	if !after.isShut() {
		t.Error("初始化后登记的处理器该在关闭时被 Shutdown")
	}
}
