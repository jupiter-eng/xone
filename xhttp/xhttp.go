package xhttp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/go-resty/resty/v2"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xhook"
	"github.com/jupiter-eng/xone/xmetric"
	"github.com/jupiter-eng/xone/xtrace"
)

// fallbackTimeout 初始化之前或关闭之后，兜底 client 的超时
//
// 零值超时是「永不超时」而不是「有个默认值」：对端不响应时请求会一直挂着。
// 最难受的是关闭阶段——某个组件在关闭时发一个这样的请求，
// 整个进程的退出流程就被卡在那里了。
const fallbackTimeout = 30 * time.Second

// New 按配置建一个 HTTP 客户端，不碰本包的全局实例。
//
// 一个例外：cfg.Metric 开着时（默认开着）耗时直方图要注册到 xmetric
// 的全局 Registry —— 指标本来就只有一份，注册到别处就导不出去。
// 不想碰它就把 cfg.Metric 关掉。注册失败（比如同名指标已被注册成别的类型）
// 不让 New 失败，只记一条错误日志，那组指标导不出去。
//
// 返回的 io.Closer 释放连接池里的空闲连接。
func New(cfg Config) (*resty.Client, io.Closer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, nil, xerror.Newf("xhttp", "config", "invalid config: %w", err)
	}

	// 自己抓住连接池那一层，不指望 http.Client.CloseIdleConnections 找得到它。
	//
	// 那个方法是靠类型断言往下找的：链路开着时中间隔着 otelhttp.Transport，
	// 而它没有实现这个方法，断言到那里就断了——整条调用变成空操作，
	// 而链路默认是开着的。自己持有，关的时候直接关它。
	tlsCfg, err := cfg.TLS.Build()
	if err != nil {
		return nil, nil, xerror.Newf("xhttp", "config", "invalid TLS config: %w", err)
	}
	pool := tunedTransport(cfg, tlsCfg)
	if _, ok := pool.(*http.Transport); tlsCfg != nil && !ok {
		// 交不进去就别假装配上了：照样发出去的请求用的是系统根证书、不带客户端证书
		return nil, nil, xerror.Newf("xhttp", "config", "http.DefaultTransport has been replaced by %T, the TLS block cannot be applied", pool)
	}
	client := newResty(&http.Client{
		Transport: traced(cfg, pool),
		Timeout:   cfg.Timeout,
	}, cfg.Log)

	if cfg.RetryCount > 0 {
		p := retryPolicy{onlyIdempotent: cfg.RetryOnlyIdempotent}
		client.SetRetryCount(cfg.RetryCount).
			SetRetryWaitTime(cfg.RetryWaitTime).
			SetRetryMaxWaitTime(cfg.RetryMaxWaitTime).
			AddRetryCondition(p.condition).
			SetRetryAfter(p.veto)
	}

	if cfg.Metric {
		// 用 RegisterAs 的返回值：重复注册时它给的是已有那个实例，
		// 记到新建的那个上会永远导不出去。
		//
		// 注册失败只记日志、照常交回客户端，与 xgin / xgorm / xredis 一致：
		// 指标导不出去是可观测性问题，不该让所有出站调用跟着起不来。
		// 出错时 RegisterAs 交回的是新建的那个，照常打点，只是导不出去
		hist, err := xmetric.RegisterAs(newDurationHistogram())
		if err != nil {
			slog.Error("xhttp failed to register the request duration metric, values recorded through it will not be exported", "error", err)
		}
		installMetrics(client, hist)
	}
	if cfg.Log {
		installLog(client, cfg.SlowThreshold)
	}

	return client, &clientCloser{pool: pool}, nil
}

// newResty 用给定的 http.Client 建 resty，并把 resty 自己的日志接到 slog 上。
//
// 一律走 NewWithClient 而不是 resty.New()：后者自带一个 cookie jar，
// 同一个 client 发出的所有请求共享它——A 服务种下的会话 cookie
// 会被带给之后每一次毫不相干的调用。配置出来的实例本来就没有 jar，
// 兜底实例曾经是 resty.New()，于是两者行为不一致（实测会串 cookie）。
//
// quiet 是 Config.Log：请求日志开着时，resty 重试路径上那两种日志由请求日志的一行代替（见 restyLogger）。
// 设在 client 的 logger 上而不是每个请求上：使用者自己 SetLogger 的话就整个换掉，照 resty 的规矩来。
func newResty(hc *http.Client, quiet bool) *resty.Client {
	return resty.NewWithClient(hc).SetLogger(restyLogger{quiet: quiet})
}

// traced 在连接池外面包上链路那几层
//
//	Trace 开着：client → xtrace.Transport → otelhttp.Transport → scrubURL → 调好参数的 http.Transport
//	Trace 关着：client → xtrace.Transport → propagateOnly → 调好参数的 http.Transport
//
// xtrace.Transport 把目标 host 写进 ctx，按域名透传 Header 的规则才能生效。
// otelhttp 无论链路是否采样都会调用全局 Propagator 注入（本包的测试钉着）。
//
// Trace 只管「开不开出站 Span」。traceparent、baggage、透传 Header 带不带给下游
// 是另一件事：原先关掉 Trace 连 xtrace.Transport 一起摘了，X-Request-Id 这类
// 透传头和上游的链路标识就悄悄断在这一跳。所以关着时换成只注入、不开 Span 的那一层。
func traced(cfg Config, pool http.RoundTripper) http.RoundTripper {
	next := http.RoundTripper(propagateOnly{next: pool})
	if cfg.Trace {
		next = otelhttp.NewTransport(scrubURL{next: pool}, otelhttp.WithSpanNameFormatter(spanName))
	}
	return &xtrace.Transport{Next: next}
}

// propagateOnly 用全局 Propagator 把 ctx 里的链路标识和透传 Header 写进请求头，
// 不开 Span。Trace 关着时顶替 otelhttp 做它注入的那一半。
type propagateOnly struct{ next http.RoundTripper }

// RoundTrip 按 RoundTripper 的约定不改入参：注入写在克隆出来的请求上
func (p propagateOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	otel.GetTextMapPropagator().Inject(r.Context(), propagation.HeaderCarrier(r.Header))
	return p.next.RoundTrip(r)
}

// CloseIdleConnections 转给连接池，理由见 xtrace.Transport 的同名方法
func (p propagateOnly) CloseIdleConnections() {
	if c, ok := p.next.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// scrubURL 把 otelhttp 写在 Span 上的 url.full 换成不带查询串的版本。
//
// otelhttp（v0.71）只去掉 URL 里的 user:password，查询串原样写进 url.full——
// 而查询串里常有令牌和签名，它们会跟着 Span 进链路后端。otelhttp 没有
// 改写属性的选项；它是在开好 Span、调下一层之前 SetAttributes 的，
// 所以在它下面这一层用同一个 key 再写一次就覆盖掉了（SDK 对同名属性取后写的）。
// 路径保留：它不进 Span 名，只是一个属性，排查时要用。
//
// 不记录的 Span（没采样、链路关着）丢掉一切属性，就不必再拼这个 URL。
type scrubURL struct{ next http.RoundTripper }

func (s scrubURL) RoundTrip(r *http.Request) (*http.Response, error) {
	if span := trace.SpanFromContext(r.Context()); span.IsRecording() {
		span.SetAttributes(attribute.String("url.full", bareURL(r.URL)))
	}
	return s.next.RoundTrip(r)
}

// tunedTransport 从 DefaultTransport 克隆再改。
//
// 没改的那些就是接受了标准库的默认值，逐个量过（Go 1.25）：
//
//   - Proxy: ProxyFromEnvironment。设了 HTTP_PROXY / HTTPS_PROXY 就全部出站都走代理
//     （实测 http:// 的请求连同查询串原样交给代理），回环地址除外，NO_PROXY 可以排除。
//     环境变量在进程里第一次用到时读一次就缓存了：之后再改（哪怕 unset）不生效。
//   - TLSHandshakeTimeout: 10s。对端收下 TCP 连接不回握手，实测 10.0s 报 TLS handshake timeout。
//   - ResponseHeaderTimeout: 0，传输层不限等响应头的时间，由 Timeout 管住整次尝试；
//     Timeout 也配成 0 的话，一个收下请求不回话的对端会让请求永远挂着。
//   - MaxConnsPerHost: 0，不限；可配，见 Config.MaxConnsPerHost。
//   - ForceAttemptHTTP2: true，https 的下游协商成 HTTP/2 后一条连接多路复用。
//   - 重定向：resty v2.17 不设 CheckRedirect，于是是标准库的默认，最多跟 10 次，第 11 次报
//     stopped after 10 redirects。跨 host 跳转时标准库只去掉 Authorization、Cookie 这几个，
//     自定义的凭证头（实测 X-Api-Key）照样带给新 host；302 把 POST 变成不带 body 的 GET，
//     307 保留方法和 body。要改就在原生 client 上 SetRedirectPolicy。
//
// tlsCfg 不为 nil 时换掉 TLSClientConfig（配置里的 TLS 块）。换了之后 HTTP/2 照旧：
// ForceAttemptHTTP2 让标准库在自定义的 TLSClientConfig 上也补上 h2（实测协商出 HTTP/2.0）。
func tunedTransport(cfg Config, tlsCfg *tls.Config) http.RoundTripper {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		slog.Warn("xhttp cannot tune the connection pool", "default_transport_type", fmt.Sprintf("%T", http.DefaultTransport))
		return http.DefaultTransport
	}

	t = t.Clone()
	t.MaxIdleConns = cfg.MaxIdleConns
	t.MaxIdleConnsPerHost = cfg.MaxIdleConnsPerHost
	t.MaxConnsPerHost = cfg.MaxConnsPerHost
	t.IdleConnTimeout = cfg.IdleConnTimeout
	t.DialContext = (&net.Dialer{Timeout: cfg.DialTimeout, KeepAlive: cfg.DialKeepAlive}).DialContext
	if tlsCfg != nil {
		t.TLSClientConfig = tlsCfg
	}
	return t
}

// spanName 出站 Span 只用方法命名。
//
// 入站那边用的是路由模板（GET /users/:id），出站这边没有模板可用——
// 真实路径 /users/42、/users/43 各是一个 Span 名，基数随用户数增长，
// 链路后端按名字建的索引会被撑爆。OTel 的语义约定在没有模板时也是
// 只用方法。要看具体打到哪，看 url.full 和 server.address 属性。
func spanName(_ string, r *http.Request) string {
	return r.Method
}

// restyLogger 把 resty 自己的日志接到 slog 上。
//
// resty 的默认 logger 在建 client 那一刻抓住 os.Stderr，绕开 slog 直接写。
// 实测（resty v2.17，RetryCount=2，目标端口没人监听）是三行 WARN 一行 ERROR：
//
//	WARN RESTY Get "http://host/x?token=…": dial tcp …: connection refused, Attempt 1
//	…Attempt 2、Attempt 3…
//	ERROR RESTY Get "http://host/x?token=…": dial tcp …: connection refused
//
// RetryCount=0 时这条路径不打日志。另一类是配置调用被忽略时的 ERROR，
// 比如链路开着时 transport 不是 *http.Transport，SetTLSClientConfig 就只打一行错误、
// 什么也不做——那是使用者唯一能看到的信号，所以级别照搬，不往下降。
// 不进 slog 就不进日志平台、格式和其余日志对不上；查询串里的令牌还原样落盘。
// 这里接到 slog，消息里的 URL 去掉查询串、片段和 userinfo（参数里的错误按结构去，见 scrubError）。
//
// 调试模式（SetDebug(true)）是例外：resty 把整个请求（请求行带着查询串、全部 Header）
// 拼成一段文本交给 Debugf，请求行里的路径不带 http://、也不在引号里，这里认不出来，照原样进日志。
//
// quiet（Config.Log 开着）时不打重试路径上的那两种，它们就是请求日志那一行的 error。
// resty v2.17.2 打的日志里，每次失败的尝试是 Warnf("%v, Attempt %v")，按格式串认；
// 重试用完是 (*Request).Execute 里的 Errorf("%v")，而配置调用被忽略时（比如 SetTLSClientConfig
// 碰上不是 *http.Transport 的 transport）的 Errorf 格式串也是 "%v"——那是使用者唯一能看到的信号，
// 不能一起吞掉，所以按调用方认：只挑掉 Execute 里打的那一行。
type restyLogger struct{ quiet bool }

func (l restyLogger) Errorf(format string, v ...any) {
	if l.quiet && calledFromExecute() {
		return
	}
	logResty(slog.LevelError, format, v)
}

func (l restyLogger) Warnf(format string, v ...any) {
	if l.quiet && format == "%v, Attempt %v" {
		return
	}
	logResty(slog.LevelWarn, format, v)
}

func (restyLogger) Debugf(format string, v ...any) { logResty(slog.LevelDebug, format, v) }

func logResty(level slog.Level, format string, v []any) {
	args := make([]any, len(v))
	for i, a := range v {
		if err, ok := a.(error); ok {
			a = scrubError(err)
		}
		args[i] = a
	}
	slog.Log(context.Background(), level, "xhttp resty log", "detail", scrubText(fmt.Sprintf(format, args...)))
}

// restyExecute resty 发请求、重试的那个方法（v2.17.2）。重试用完的那行 Errorf 就在它里面
const restyExecute = "github.com/go-resty/resty/v2.(*Request).Execute"

// calledFromExecute 报告调 restyLogger 的是不是 restyExecute。跳过本包自己的栈帧，看第一个外面的
func calledFromExecute() bool {
	pcs := make([]uintptr, 8)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		f, more := frames.Next()
		if !strings.HasPrefix(f.Function, "github.com/jupiter-eng/xone/xhttp.") {
			return f.Function == restyExecute
		}
		if !more {
			return false
		}
	}
}

// idempotentMethods 可以安全重试的方法（RFC 9110 的幂等方法）
var idempotentMethods = map[string]struct{}{
	http.MethodGet: {}, http.MethodHead: {}, http.MethodOptions: {},
	http.MethodTrace: {}, http.MethodPut: {}, http.MethodDelete: {},
}

// retryPolicy 哪些请求可以重试。两道关：
//
//   - condition：挂成重试条件。只有传输层的错才重试（理由见下），body 发得了第二次才重试，
//     onlyIdempotent 时还得是幂等方法。
//   - veto：挂成 RetryAfter，重试之前的最后一道关。resty 的条件是「或」（v2.17.2 retry.go Backoff：
//     一个返回 true 就不看后面的），使用者自己 AddRetryCondition 挂的条件会把 condition 的 false 盖掉；
//     RetryAfter 在决定重试之后、等待之前调用，返回错误就不再重试。
//
// veto 返回的错误只在「这一次没有错误」时才交到调用方手里（Backoff：if err == nil { err = err2 }），
// 有错误时调用方拿到的仍是原来那个。所以它只在两种情况下否决：
//
//   - 没拿到响应（RawResponse 为 nil，一定是传输层错误）而方法不幂等：那正是 RetryOnlyIdempotent 防的
//     「分不出请求到没到」，调用方拿到的是原来的传输层错误；
//   - body 是读过的 io.Reader：再发就是一个空 body，服务端回 200 就是一次静默的数据丢失，
//     拿到了响应也照样否决（这时调用方拿到 errBodyNotRewindable 和那个响应）。
//
// 拿到了响应、方法不幂等、body 发得了第二次的，否决的代价是把一个 503 换成我们编出来的错误，
// 所以不否决：使用者按状态码重试的条件照旧生效，自己判断方法。使用者 SetRetryAfter 会换掉 veto。
type retryPolicy struct{ onlyIdempotent bool }

// condition 挂上任何一个重试条件，resty 自己的判断就整个作废，只听条件的
// （resty v2.17.2 retry.go 的 Backoff）。它自己不重试的那些——请求前的中间件
// 失败、响应已经完整收到之后的解析失败——传进条件时已经剥掉了「不重试」
// 的标记，看上去和传输层错误一样。从前这里对幂等方法一律返回 true，实测
// SetResult 遇上 200 + 坏 JSON、RetryCount=3，同一个请求发了 4 次，
// 不挂条件时 resty 只发 1 次。所以这里先自己认一遍「是不是传输层的错」。
func (p retryPolicy) condition(resp *resty.Response, err error) bool {
	if !isTransportError(err) {
		return false // 拿到响应、或者错在拿到响应之后，都不重试，与 resty 的默认条件一致
	}
	return p.refusal(resp) == nil
}

// veto 见 retryPolicy。返回 0 是交给 resty 按退避算等多久
func (p retryPolicy) veto(_ *resty.Client, resp *resty.Response) (time.Duration, error) {
	err := p.refusal(resp)
	if err == nil || errors.Is(err, errNotIdempotent) && resp != nil && resp.RawResponse != nil {
		return 0, nil
	}
	return 0, err
}

var (
	// errNotIdempotent 不会交到调用方手里：只在没拿到响应时否决，那时调用方拿到的是原来的传输层错误。
	// 不是 xerror：它只在 resty 的重试判断里流转
	errNotIdempotent = errors.New("not retrying a non-idempotent method")

	errBodyNotRewindable = xerror.Newf("xhttp", "execute",
		"not retrying: the request body is an io.Reader that the first attempt already consumed "+
			"(pass []byte or string to make the request retryable)")
)

// refusal 这个请求为什么不能重试，能重试是 nil
func (p retryPolicy) refusal(resp *resty.Response) error {
	if resp == nil || resp.Request == nil {
		return errNotIdempotent // 认不出方法时保守地不重试
	}
	if consumedReader(resp.Request) {
		return errBodyNotRewindable
	}
	method := strings.ToUpper(resp.Request.Method)
	if _, ok := idempotentMethods[method]; ok || !p.onlyIdempotent {
		return nil
	}
	slog.Debug("xhttp skipped retrying a non-idempotent method",
		"method", method, "to_allow_set", ConfigKey+".RetryOnlyIdempotent=false")
	return errNotIdempotent
}

// consumedReader body 是不是一个读过就没了的 io.Reader。
//
// resty v2.17.2 每次尝试都拿 Request.Body 重新建 http.Request（middleware.go createHTTPRequest）：
// []byte、string、结构体每次重新序列化，是完整的；io.Reader 在第一次尝试就读到了头，
// 实测 PUT strings.NewReader(…)、第一次连接被掐断，第二次发出去 0 字节，服务端回 200。
// resty 不替它倒回去（RetryResetReaders 只管 multipart）。例外是 SetContentLength(true)：
// resty 把 io.Reader 读进缓冲、Body 置 nil，之后每次都发那份缓冲（实测照样完整）。
// http.NoBody 是 resty 给没有 body 的 PUT / POST 补的，空的，发几次都一样
func consumedReader(r *resty.Request) bool {
	_, ok := r.Body.(io.Reader)
	return ok && r.Body != http.NoBody
}

// isTransportError 报告 err 是不是 resty 默认会重试的那种传输层错误
//
// 建连、发送、等响应时出的错，http.Client.Do 一律包成 *url.Error，
// 它实现了 net.Error；读响应体时连接被重置、超时也都是 net.Error。
// 响应体读到一半连接断了是 io.ErrUnexpectedEOF，resty 同样重试
// （实测 Content-Length 100 只发 10 字节就断开，不挂条件时共发 4 次）。
// JSON 解析失败、中间件返回的错误都不在其中。
func isTransportError(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, io.ErrUnexpectedEOF)
}

// clientCloser 持有连接池本身，而不是外面那个 http.Client
type clientCloser struct{ pool http.RoundTripper }

func (c *clientCloser) Close() error {
	if p, ok := c.pool.(interface{ CloseIdleConnections() }); ok {
		p.CloseIdleConnections()
	}
	return nil
}

// ---- 全局实例 ----

// fallback 初始化之前、以及关闭之后用的兜底 client，带超时。建一次就不再变。
// 与配置出来的实例一样不带 cookie jar，理由见 newResty
var fallback = newResty(&http.Client{Timeout: fallbackTimeout}, false)

var (
	mu      sync.RWMutex
	current *resty.Client // nil 表示还没起来、或者已经关掉了
)

// C 取 resty client。
//
// 不像 xgorm / xredis 那样取不到就 panic：HTTP 客户端不连任何外部资源，
// 没配也能用，所以这里任何时候都返回一个可用的实例。
// 初始化之前、关闭之后拿到的都是带 30s 超时的兜底实例——关闭阶段仍可能有
// 组件发请求，让它带着超时失败，好过拿到一个已经关掉的客户端。
func C() *resty.Client {
	mu.RLock()
	defer mu.RUnlock()
	if current != nil {
		return current
	}
	return fallback
}

// R 开一个绑定了 ctx 的请求，链路和超时才能传到下游。
//
//	resp, err := xhttp.R(ctx).SetResult(&out).Get(url)
func R(ctx context.Context) *resty.Request { return C().R().SetContext(ctx) }

// RawClient 取底层的原生 *http.Client。
//
// 用于需要自己处理响应体的场景，比如 SSE 这类流式请求——
// resty 会把响应整个读进内存，那对流式接口是不对的。
//
// 初始化之前返回兜底实例内部的那个，而不是 http.DefaultClient：
// 后者的超时是 0，请求可以永久挂住。
//
// 直接从 C() 取而不是另存一份：两份关联状态要同步维护，
// 而 resty 的 GetClient 返回的就是它一直在用的那个，取值一致、生命周期一致。
func RawClient() *http.Client { return C().GetClient() }

// ---- 登记 ----

func init() {
	xhook.BeforeStart(initXHttp, xhook.At(xhook.StageClient))
	xhook.BeforeStop(closeXHttp) // 档位跟着上面那个启动钩子
}

// liveCloser 由 initXHttp 填好，closeXHttp 用它收尾
var liveCloser io.Closer

func initXHttp(context.Context) error {
	c := DefaultConfig()
	if err := xconfig.Unmarshal(ConfigKey, &c); err != nil {
		return err
	}
	return install(c)
}

// install 按配置建好客户端并装成全局默认实例
func install(c Config) error {
	client, cl, err := New(c)
	if err != nil {
		return err
	}

	mu.Lock()
	current, liveCloser = client, cl
	mu.Unlock()

	slog.Info("xhttp ready", "timeout", c.Timeout, "max_idle_conns_per_host", c.MaxIdleConnsPerHost, "retries", c.RetryCount)
	return nil
}

func closeXHttp(context.Context) error {
	mu.Lock()
	cl := liveCloser
	current, liveCloser = nil, nil // 先摘掉再关，C() 随即退回兜底实例
	mu.Unlock()

	if cl == nil {
		return nil
	}
	return cl.Close()
}
