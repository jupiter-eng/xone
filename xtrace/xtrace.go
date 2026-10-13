package xtrace

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/propagators/b3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	oteltrace "go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/jupiter-eng/xone/internal/logext"
	"github.com/jupiter-eng/xone/xapp"
	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xhook"
)

// Tracing 一份配置装配出来的链路设施。
//
// New 只构造、不安装；装进 OpenTelemetry 的全局变量是 Install 的事。
// 分开是为了让测试能拿到一套独立的链路设施，而不必动全局状态。
type Tracing struct {
	// TracerProvider 链路关闭时是 noop 实现，不是 nil
	TracerProvider oteltrace.TracerProvider

	// Propagator 跨进程传递链路标识与透传 Header
	Propagator propagation.TextMapPropagator
}

// Install 把这套设施装成进程级的。
//
// 之后业务代码用原生的 otel.Tracer("...") 开 Span 即可，不需要认识本包。
func (t *Tracing) Install() {
	otel.SetTracerProvider(t.TracerProvider)
	otel.SetTextMapPropagator(t.Propagator)
	// 让日志带上 TraceID。xlog 不依赖 OpenTelemetry，这个能力由本包注入；
	// 注入点在 internal/logext，本包不 import xlog，只用 xgorm 的程序不会因此被装上 xlog
	logext.SetTraceExtractor(traceIDsFromContext)
}

// New 按配置构造链路设施，不触碰任何全局变量。
//
// procs 是要挂上去的 SpanProcessor。框架不内置任何上报 exporter——
// OTLP 一个就带进上百个构建依赖，不该由所有使用者承担。
//
// 返回的 io.Closer 永不为 nil，关闭时会把 Span 冲刷出去、Shutdown 全部 procs，
// 等待上限由 cfg.ShutdownTimeout 控制。链路关着时也要关：procs 收不到 Span，
// 但它们持有的连接和协程照样要有人收。
func New(ctx context.Context, cfg Config, procs ...sdktrace.SpanProcessor) (*Tracing, io.Closer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, nil, xerror.Newf("xtrace", "config", "invalid config: %w", err)
	}

	prop, err := newPropagator(cfg)
	if err != nil {
		return nil, nil, err
	}

	if !cfg.Enable {
		if len(procs) > 0 {
			slog.Warn("xtrace is disabled, the registered SpanProcessors will receive no spans",
				"count", len(procs), "switch", ConfigKey+".Enable")
		}
		// 处理器照样挂到一个不对外的 provider 上：对外的是 noop，一个 Span 都不会有，
		// 但关闭时它们照样被 Shutdown。AddSpanProcessor 承诺过由本包关掉它们——
		// 原先这里交回的是空操作的 Closer，exporter 持有的连接和协程退出时没人收
		idle := sdktrace.NewTracerProvider(processors(procs)...)
		return &Tracing{TracerProvider: noop.NewTracerProvider(), Propagator: prop},
			&providerCloser{tp: idle, timeout: cfg.ShutdownTimeout}, nil
	}

	res := newResource(ctx)

	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithSampler(samplerOf(cfg.SampleRatio)),
		sdktrace.WithResource(res),
	}
	if cfg.Console {
		exp, err := stdouttrace.New(stdouttrace.WithPrettyPrint())
		if err != nil {
			return nil, nil, xerror.Newf("xtrace", "new", "create stdout exporter: %w", err)
		}
		// Simple 而非 Batch：本地调试要的是立刻看见，不是攒够了再刷
		opts = append(opts, sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exp)))
	}
	tp := sdktrace.NewTracerProvider(append(opts, processors(procs)...)...)
	return &Tracing{TracerProvider: tp, Propagator: prop}, &providerCloser{tp: tp, timeout: cfg.ShutdownTimeout}, nil
}

// processors 把 SpanProcessor 逐个换成 provider 的选项
func processors(procs []sdktrace.SpanProcessor) []sdktrace.TracerProviderOption {
	opts := make([]sdktrace.TracerProviderOption, 0, len(procs))
	for _, sp := range procs {
		opts = append(opts, sdktrace.WithSpanProcessor(sp))
	}
	return opts
}

// newPropagator 组装 Propagator。
//
// baggage 用的是 trustedBaggage，与透传 Header 同一条信任边界；
// traceparent、b3 只是链路标识，谁发来的都接。
//
// 链路关掉时仍然保留 Header 透传：X-Request-Id 该不该带给下游，
// 跟要不要采样 Span 是两个问题，关掉一个不该让另一个静默失效。
func newPropagator(cfg Config) (propagation.TextMapPropagator, error) {
	var list []propagation.TextMapPropagator
	if cfg.Enable {
		list = append(list, propagation.TraceContext{}, &trustedBaggage{}, b3.New())
	}
	if cfg.forwardEnabled() {
		hp, err := NewHeaderPropagator(cfg.ForwardHeaders, cfg.ForwardHeaderRules)
		if err != nil {
			return nil, err
		}
		list = append(list, hp)
	}
	return propagation.NewCompositeTextMapPropagator(list...), nil
}

// newResource 采集进程与主机属性。走调用方的 ctx：主机探测会读文件、
// 查网卡，慢的时候不该让一个已经在退出的进程还在这儿等。
//
// 采集出错只记一条告警、用采到的那部分，不让启动失败。resource.New
// 在任何一个探测器出错时都照样交回其余探测器合并出的结果（service.name 在内，
// 见 SDK v1.46 的 resource/auto.go），出错的那一个只是被跳过：
//
//   - OTEL_RESOURCE_ATTRIBUTES 写错一个字符：ErrPartialResource，
//     写对的那几项照常生效（有测试钉着）
//   - 容器里以随机 UID 运行、user.Current 查不到：process.owner 探测器
//     返回的是一个普通错误，不是 ErrPartialResource——所以这里不按错误类型挑，
//     一律当作告警
//
// 这些都是描述性的属性，为它们让整个服务起不来，代价不对等。
//
// service.name 的优先级从低到高：OTel 自己的 unknown_service:<可执行文件名>、
// XApp.Name / XApp.Version、OTEL_RESOURCE_ATTRIBUTES、OTEL_SERVICE_NAME。
// resource.New 按选项顺序合并、同名的后者覆盖前者（实测），所以顺序就是优先级：
// 环境变量是部署方的最后一句话，应当压过打进镜像的配置文件。
// XApp.Name 没配时不写：原先无条件写进 service.name=""，
// 连 OTel 的兜底名和 OTEL_SERVICE_NAME 一起盖掉，看板上多出一个无名服务。
// 兜底名来自 WithService，它顺带给每个进程一个随机的 service.instance.id。
func newResource(ctx context.Context) *resource.Resource {
	res, err := resource.New(ctx,
		resource.WithService(),
		resource.WithAttributes(appAttributes()...),
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
	)
	if err != nil {
		slog.WarnContext(ctx, "xtrace some resource attributes could not be detected, continuing without them", "error", err)
	}
	return res
}

// appAttributes XApp 块里配了的服务名与版本，没配的那一项不写
func appAttributes() []attribute.KeyValue {
	var attrs []attribute.KeyValue
	if name := xapp.Name(); name != "" {
		attrs = append(attrs, semconv.ServiceName(name))
	}
	if version := xapp.Version(); version != "" {
		attrs = append(attrs, semconv.ServiceVersion(version))
	}
	return attrs
}

// samplerOf 按采样率构造 Sampler。
//
// 一律套 ParentBased：有上游时听上游的，只有根 Span 才按比例抽。
// 全采样原先是裸的 AlwaysSample，不看父 Span——上游传来 sampled=00，
// 我们照样采，再以 -01 往下游传（实测），上游的采样决定在这里被推翻。
// TraceIDRatioBased(1) 本身就是全采样，所以不需要为 1 单开一支。
func samplerOf(ratio float64) sdktrace.Sampler {
	return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
}

// traceIDsFromContext 从 ctx 的 Span 里取出链路标识，供 xlog 注入日志
func traceIDsFromContext(ctx context.Context) (traceID, spanID string) {
	sc := oteltrace.SpanFromContext(ctx).SpanContext()
	if !sc.IsValid() {
		return "", ""
	}
	return sc.TraceID().String(), sc.SpanID().String()
}

// ---- 关闭 ----

// providerCloser 关闭 TracerProvider，顺带关掉挂在它上面的全部 SpanProcessor
type providerCloser struct {
	tp      *sdktrace.TracerProvider
	timeout time.Duration
}

// Close 独立使用时的关闭，上限是 ShutdownTimeout
func (c *providerCloser) Close() error { return c.shutdown(context.Background()) }

// shutdown 在调用方的 ctx 上再收紧一层，而不是换掉它。
//
// 与 xgin 的 Stop 同一个道理：WithTimeout 取两者中更早的截止时间，
// 于是既不占满框架给的总预算，也不超出它。原先这里用的是
// Background + ShutdownTimeout，框架只剩 100ms 时照样等满 5s（实测）。
func (c *providerCloser) shutdown(parent context.Context) error {
	// 必须限时：导出端不可达时 Shutdown 会一直阻塞，没有 deadline 就是退出时挂死
	ctx, cancel := context.WithTimeout(parent, c.timeout)
	defer cancel()

	detach(c)
	if err := c.tp.Shutdown(ctx); err != nil {
		return xerror.Newf("xtrace", "close", "shutdown: %w", err)
	}
	return nil
}

// ---- Transport ----

// Transport 把目标请求的 Host 写进 context，让按域名透传的规则能生效。
//
// 没有它，ForwardHeaderRules 里的 header 一条都不会被注入——
// Inject 不知道这个请求要发给谁。
//
// 它自己不注入，注入是它下面那一层的事：otelhttp 无论 TracerProvider 是不是 noop，
// 都会调用全局 Propagator 注入——XTrace.Enable 关掉时它注入透传 header、
// 不注入 traceparent，正是想要的行为。这个前提由 xhttp 的测试钉住（它本来就依赖
// otelhttp，放在那里不额外增加依赖）。不要出站 Span 的（XHttp.Trace: false），
// xhttp 在这一层下面换上一个只注入、不开 Span 的，透传照常。
type Transport struct {
	// Next 实际执行请求的 RoundTripper，为 nil 时用 http.DefaultTransport
	Next http.RoundTripper
}

// RoundTrip 实现 http.RoundTripper。
// 按约定不修改入参请求：WithContext 返回的是浅拷贝。
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.next().RoundTrip(req.WithContext(WithTargetHost(req.Context(), req.URL.Host)))
}

// CloseIdleConnections 把关闭空闲连接的请求转给底层 transport。
//
// 必须有这个方法。http.Client.CloseIdleConnections() 是靠类型断言找它的，
// 包一层却不转发，断言就不成立，整个调用变成一次空操作——
// 而链路默认是开着的，也就是默认情况下退出时那些空闲连接根本没被清掉。
func (t *Transport) CloseIdleConnections() {
	if c, ok := t.next().(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// next 实际执行请求的 RoundTripper
func (t *Transport) next() http.RoundTripper {
	if t.Next == nil {
		return http.DefaultTransport
	}
	return t.Next
}

// ---- 登记 ----

// 已登记的 SpanProcessor，以及装到全局、还没关掉的那一套链路设施。
// live 为 nil：还没装、或者已经关了。链路关着时它也在（处理器挂在一个不对外的
// provider 上，收不到 Span、但关闭时照样被 Shutdown）。
//
// 一把锁管两件事：注册与初始化必须互斥，否则并发注册可能落在
// 「已经取走 pending、还没装好 provider」的窗口里，那个处理器就被吞了。
var (
	mu      sync.Mutex
	pending []sdktrace.SpanProcessor
	live    *providerCloser
)

// AddSpanProcessor 挂一个 SpanProcessor，用于把 Span 上报到远端。
//
// 框架不内置任何 exporter：OTLP 一个就带进上百个构建依赖，
// 不需要上报的服务不该为此付钱。需要的服务自己引入并在此注册：
//
//	exp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpoint(ep), otlptracegrpc.WithInsecure())
//	if err != nil { return err }
//	xtrace.AddSpanProcessor(sdktrace.NewBatchSpanProcessor(exp))
//
// 在 xone.Run 之前调用即可。初始化之后注册的会立即挂上。
// 关闭时由本包统一 Shutdown，等待上限由 XTrace.ShutdownTimeout 控制。
func AddSpanProcessor(sp sdktrace.SpanProcessor) {
	if sp == nil {
		panic("xtrace: SpanProcessor must not be nil")
	}
	mu.Lock()
	defer mu.Unlock()

	if live != nil {
		live.tp.RegisterSpanProcessor(sp)
		return
	}
	pending = append(pending, sp)
}

// detach 关闭后清掉 provider，避免后来的注册挂到已关闭的实例上。
//
// 只清掉自己：New 出来的实例不一定是装到全局的那个——测试要一套干净的
// 链路设施、或者同时存在两套配置时，关掉其中一个曾经把全局那个也一起抹掉。
// 之后每一次 AddSpanProcessor 都会挂到 pending 上再也没人读，
// Span 照常产生、永远到不了上报端，而且没有任何迹象。
func detach(c *providerCloser) {
	mu.Lock()
	defer mu.Unlock()
	if live == c {
		live = nil
	}
}

// initXTrace 读配置、装好链路设施、挂上 provider。
func initXTrace(ctx context.Context) error {
	c := DefaultConfig()
	if err := xconfig.Unmarshal(ConfigKey, &c); err != nil {
		return err
	}
	return install(ctx, c)
}

func closeXTrace(ctx context.Context) error {
	mu.Lock()
	c := live
	mu.Unlock()
	if c == nil {
		return nil
	}
	return c.shutdown(ctx)
}

// install 取走待办的 SpanProcessor、装好链路设施、挂上 provider。
//
// 全程持锁。取待办和装 provider 之间一旦放开，落在那个窗口里的
// AddSpanProcessor 两边都不占：它看到 live 还是 nil，于是追加到一个
// 已经被取走、再也不会被读的 pending 上，然后被静默丢掉——
// Span 照常产生，只是永远到不了上报端，没有任何迹象。
// 代价只是并发的注册方要等初始化走完，那本来就是它该等的。
func install(ctx context.Context, c Config) error {
	mu.Lock()
	defer mu.Unlock()

	procs := pending
	pending = nil

	t, closer, err := New(ctx, c, procs...)
	if err != nil {
		// 没装起来，把待办还回去：调用方多半会让启动失败，
		// 但万一它选择继续，这些处理器不该凭空消失
		pending = procs
		return err
	}
	t.Install()
	live = closer.(*providerCloser) // 链路开关与否，New 交回的都是它
	return nil
}

// init 链路要早于各类客户端就绪，否则它们发出的 Span 挂不上
func init() {
	xhook.BeforeStart(initXTrace, xhook.At(xhook.StageTelemetry))
	xhook.BeforeStop(closeXTrace) // 档位跟着上面那个启动钩子
}
