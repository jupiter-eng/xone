package xecho

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/labstack/echo/v4"

	"github.com/jupiter-eng/xone/internal/web"
	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xecho/internal/peer"
	"github.com/jupiter-eng/xone/xecho/internal/reqctx"
	"github.com/jupiter-eng/xone/xecho/middleware"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xhook"
	"github.com/jupiter-eng/xone/xmetric"

	// 用了 xecho 就有链路：xtrace 装好全局的 TracerProvider 和 Propagator，
	// 中间件经 otel 的全局 API 用上它们。不要链路配 XTrace.Enable: false
	_ "github.com/jupiter-eng/xone/xtrace"

	// 用了 Web 框架就有 xlog：访问日志、请求级字段（xlog.AddKV）都靠它。
	// 根包不带它，只用 xhook、xgorm 的程序不会被它接管日志
	_ "github.com/jupiter-eng/xone/xlog"
)

// XEcho 一个待启动的 HTTP 服务。
//
// 它满足 xone.Runnable，直接交给 xone.Run 即可：
//
//	xone.MustRun(xecho.New().WithRoutes(register))
//
// 注意这里没有 import 根包——Go 的接口是结构化的，方法对得上就行。
// 这样「集成不依赖框架」这条在编译层面仍然成立。
type XEcho struct {
	override *Config // 见 WithConfig

	routes  []func(*echo.Echo)
	extra   []echo.MiddlewareFunc
	recover func(echo.Context, any) error

	// 下面这几项由 build 一次写定，之后只读。读它们的要么先调 build，要么是
	// engine 上的 handler——而 engine 只能经 build 拿到。Once 保证写在读之前，不需要锁
	buildOnce sync.Once
	engine    *echo.Echo
	conf      Config      // 装配用的那份配置，Start 用的也是它
	confErr   error       // 配置不合法时的错误，此时 conf 是默认值
	trusted   web.Proxies // TrustedProxies 解出来的网段，见 markTrustedPeer

	// server 监听和优雅关闭，与 echo 无关的那一半，见 web.Server。
	// 不用 e.Start：它用的 e.Server 四个超时全是 0，还会往 stdout 打一个 banner
	server web.Server
}

// New 创建一个 XEcho。
//
// 开关、端口、超时都在配置文件的 XEcho 块里；同一个进程里的第二个服务用 WithConfig。
// 这里什么都不读，配置在装配（Engine 或 Start）那一刻才取。零值 &XEcho{} 与 New() 等价。
func New() *XEcho { return &XEcho{} }

// WithConfig 用这份配置起服务，不再读配置文件里的 XEcho 块。
//
// 一个进程里起第二个服务时用它——比如对外的 API 一个端口、内部的管理端口
// 另一个。不给它的话两个实例读的是同一块配置，只能监听同一个端口。
//
// 传进来的是完整配置，不是补丁：想在文件配置的基础上改两项，先取
// CurrentConfig 再改（从头写的话从 DefaultConfig 开始，零值的超时是不合法的）：
//
//	c := xecho.CurrentConfig()
//	c.Host, c.Port = "127.0.0.1", 9090
//	admin := xecho.New().WithConfig(c).WithRoutes(adminRoutes)
//
// 它在装配时校验，不合法的话 Start 返回错误、不监听。
func (x *XEcho) WithConfig(c Config) *XEcho {
	x.override = &c
	return x
}

// CurrentConfig 返回配置文件里 XEcho 那一块（拷贝），在 Start 之前任何时候调都行。
//
// 配合 WithConfig 用，见那里的例子。配置文件第一次读的时候才加载，
// 所以在 main 里、xone.Run 之前拿到的也是最终值。
//
// 那一块写得不合法时返回默认值：错误由 xone.Run 在启动时报出来，这里不报第二遍。
func CurrentConfig() Config {
	c, _ := fileConfig()
	return c
}

// WithRoutes 注册路由。可以调用多次，按调用顺序生效。
//
// 回调拿到的是原生的 *echo.Echo，想怎么设都行。它在配置落到 echo 上之后才跑，
// 所以回调里的设置盖得过配置：换 e.IPExtractor、e.HTTPErrorHandler 都以回调为准。
//
// 只有一处跟不过去：透传 Header（XTrace.ForwardHeaders）认的可信对端只看配置里的
// TrustedProxies，回调里换的 IPExtractor 只管得到 c.RealIP()。两边要一起改就改配置。
func (x *XEcho) WithRoutes(f ...func(*echo.Echo)) *XEcho {
	x.routes = append(x.routes, f...)
	return x
}

// WithMiddleware 追加自定义中间件，排在所有内置中间件之后。
//
// 它们挂在 e.Use 上，在 router 之后跑，取得到 c.Path()、c.Param()；内置的挂在 e.Pre 上，
// 在使用者自己的 e.Pre 外面，顺序见 build。
// 它们对每个请求都生效，包括框架挂的 MetricPath 和没匹配上路由的请求（echo 的 e.Use 如此）。
func (x *XEcho) WithMiddleware(m ...echo.MiddlewareFunc) *XEcho {
	x.extra = append(x.extra, m...)
	return x
}

// WithRecoverFunc 自定义 panic 之后的响应。
//
// f 返回的错误和 handler 返回的一样交给 e.HTTPErrorHandler；自己写了响应就返回 nil。
// 默认返回 echo.ErrInternalServerError，即 500 {"message":"Internal Server Error"}。
// 响应已经开始往外写了的话不调 f：再改状态码只会得到一个半截的响应。
func (x *XEcho) WithRecoverFunc(f func(c echo.Context, recovered any) error) *XEcho {
	x.recover = f
	return x
}

// Engine 返回原生的 *echo.Echo，需要做本包没覆盖的事情时用它。
//
// 调用它会触发装配，用的是 WithConfig 给的那份配置，或者配置文件里的 XEcho 块——
// 配置文件第一次读的时候才加载，所以在 xone.Run 之前调也拿得到最终值。
// 装配只有一次：之后再 WithConfig / WithRoutes / WithMiddleware / WithRecoverFunc
// 就不生效了，Start 也沿用这一次的配置。
//
// 不要调它的 e.Start / e.StartTLS：服务由 Start 起，超时、TLS、优雅退出都在那一边。
//
// 配置不合法时照样返回一个 echo，按默认值装配（只信私有网段的代理，偏安全的那一侧），
// 并记一条告警。那个错误由 Start 返回，xone.Run 在启动时就会报出来。
func (x *XEcho) Engine() *echo.Echo {
	x.build()
	return x.engine
}

// build 装配 echo，一个实例只装配一次。
//
// 用 Once 而不是布尔标志：并发调用时后者会把中间件注册两遍，
// 表现是每个请求打两条日志、指标翻倍。
//
// 顺序有讲究：先建 echo、落配置，然后挂中间件和内置路由，最后才跑
// 使用者的 WithRoutes——所以回调里对 echo 的设置盖得过配置，见 WithRoutes。
func (x *XEcho) build() {
	x.buildOnce.Do(func() {
		// 配置不合法时照样装配，按默认值：Engine() 的调用方总得拿到一个 echo，
		// 而默认值是偏安全的那一侧。错误记下来由 Start 返回；告警是给只用 Engine()、
		// 从不调 Start 的使用者的，否则他们拿到一份默认值却毫无迹象
		c, err := x.cfg()
		if err != nil {
			slog.Warn("xecho invalid config, assembling the engine with defaults; Start will return the error", "error", err)
		}
		x.conf, x.confErr = c, err

		e := echo.New()
		applyConfig(e, c)
		x.trusted = web.ParseProxies(c.TrustedProxies)

		// 洋葱模型，自外向内：
		//   LogScope → Trace → Log → Metric → Recover → 用户的 e.Pre → router → 用户中间件 → handler
		//
		// Recover 必须是内置里最内层的：panic 在哪一层被兜住，
		// 比它更内层的中间件里 next(c) 之后的代码就都不执行了。
		// 放最内层，外面几层的收尾（记指标、写访问日志）才还跑得到。
		//
		// 内置的用 e.Pre 而不是 e.Use：后者在 router 之后才跑，使用者在 e.Pre 里拒掉、重定向的请求
		// （鉴权、限流、末尾斜杠的 301 常写在那里）经过不了它们——不进访问日志、指标、链路，
		// Pre 里的 panic 也没人兜（echo 自己不兜，客户端读到 EOF）。挂在 Pre 上照样取得到路由模板：
		// router 在 Pre 链的最里面跑（Echo.ServeHTTP），next(c) 返回时 c.Path() 已经有了，
		// 没走到 router 的是空串，记 unmatched。它们在 WithRoutes 之前挂上，排在使用者的 e.Pre 外面。
		//
		// Pre 里不动请求：日志作用域、Span 只挂在 ctx 上、存进 echo.Context，路由之后由 attachContext
		// 换到请求上，理由见 reqctx。所以使用者的 e.Pre 看到的是请求原来的 ctx（那里打的日志不带 trace_id）
		e.Pre(x.enter)
		if c.Log {
			e.Pre(middleware.LogScope())
		}
		// Trace 只管 Span：关掉时照样接上游的链路标识和透传 Header，只是不开 Span
		if c.Trace {
			e.Pre(skipMetrics(c, middleware.Trace()))
		} else {
			e.Pre(middleware.Propagate())
		}
		if c.Log {
			skip := slices.Clone(c.LogSkipPaths)
			if c.Metric {
				// 指标端点会被抓取系统按秒轮询，记日志纯属刷屏
				skip = append(skip, c.MetricPath)
			}
			e.Pre(middleware.Log(
				middleware.WithSkipPaths(skip...),
				middleware.WithBody(c.LogRequestBody, c.LogResponseBody),
				middleware.WithQuery(c.LogQuery),
				middleware.WithHeaders(c.LogRequestHeaders, c.LogResponseHeaders),
			))
		}
		if c.Metric {
			e.Pre(middleware.Metric())
		}
		e.Pre(middleware.Recover(x.recover))
		// 用户中间件在 router 之后：它们要的是 c.Path()、c.Param()，和使用者自己 e.Use 的一样。
		// 在它们之前先把 Pre 里建好的 ctx 换到请求上，它们和 handler 看到的就是带着 Span 和日志作用域的 ctx
		e.Use(attachContext)
		e.Use(x.extra...)

		// echo 的 e.Use 在每个请求到来时才套上，和路由注册的先后无关（gin 是注册那一刻定死的）：
		// 指标端点同样走用户用 WithMiddleware 挂的统一鉴权。照 xgin 的顺序写在中间件之后
		if c.Metric {
			e.GET(c.MetricPath, serveMetrics)
		}
		for _, f := range x.routes {
			f(e)
		}

		x.engine = e
	})
}

// serveMetrics 指标端点。每次请求再取 handler，不在装配时定死：装配可能发生在
// xmetric 初始化之前（在 xone.Run 之前调 Engine() 就会），那时拿到的是兜底 registry，
// /metrics 会一直是空的
func serveMetrics(c echo.Context) error {
	xmetric.Handler().ServeHTTP(c.Response(), c.Request())
	return nil
}

// skipMetrics 指标端点不进 m：抓取系统按秒轮询它，每次开一个 Span 只是在链路后端刷屏，
// 访问日志也是同样的理由跳过它的。挂在 Pre 上的中间件这时还没路由，按路径判断，
// 和访问日志的 SkipPaths 一样只比 URL.Path
func skipMetrics(c Config, m echo.MiddlewareFunc) echo.MiddlewareFunc {
	if !c.Metric {
		return m
	}
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		h := m(next)
		return func(ctx echo.Context) error {
			if ctx.Request().URL.Path == c.MetricPath {
				return next(ctx)
			}
			return h(ctx)
		}
	}
}

// applyConfig 把配置里管 echo 的两件事落到 e 上：client_ip 信谁、echo 自己的日志往哪写
func applyConfig(e *echo.Echo, c Config) {
	// 默认只信私有网段。echo 的默认（IPExtractor 为 nil）是谁发来的 X-Forwarded-For 都信：
	// 公网对端发一个 X-Forwarded-For: 1.2.3.4，c.RealIP() 就是 1.2.3.4（实测 v4.16.0）。
	// 算法和 xgin 用的 gin ClientIP 一样，见 web.Proxies.ClientIP。
	// 网段写错在 Validate 里就拦下了；解不出的那几项 ParseProxies 直接跳过，拿不准就不信
	e.IPExtractor = web.ParseProxies(c.TrustedProxies).ClientIP

	// echo 自己的日志（gommon）默认写 os.Stdout，是它自己的 JSON 格式，不经过 slog：
	// 实测 e.HTTPErrorHandler 写错误响应失败时（客户端已经断开）会在 stdout 留一行
	// {"time":…,"level":"ERROR","prefix":"echo",…,"message":"write: broken pipe"}。
	// 接到 slog，级别沿用 echo 的默认（ERROR 以上才写）
	e.Logger.SetHeader("${level}")
	e.Logger.SetOutput(echoLog{})
	// e.StdLogger 是 echo 给 http.Server.ErrorLog 准备的（e.Start 里就这么用），在 echo.New 里就绑定了
	// e.Logger 当时的输出，也就是 os.Stdout（实测 v4.16.0），上面换 Logger 的输出改不到它。
	// 和服务真正用的 ErrorLog 接到同一处
	e.StdLogger = web.ErrorLog("xecho")
}

// echoLog 把 echo 自己的日志一行一行转给 slog。
//
// 配合 SetHeader("${level}")：gommon 写进来的每一行是「级别 消息」，
// 级别换成 slog 的，消息放进 message 字段
type echoLog struct{}

func (echoLog) Write(p []byte) (int, error) {
	level, msg, _ := strings.Cut(strings.TrimSpace(string(p)), " ")
	slog.Log(context.Background(), echoLevel(level), "echo internal log", "message", msg)
	return len(p), nil
}

// echoLevel gommon 的级别名换成 slog 的级别。PANIC / FATAL 记成 ERROR：
// 它们之后 gommon 自己会 panic / 退出进程，slog 只管把这一行记下来
func echoLevel(s string) slog.Level {
	switch s {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN":
		return slog.LevelWarn
	case "ERROR", "PANIC", "FATAL":
		return slog.LevelError
	}
	return slog.LevelInfo // INFO，以及 Print 系列（级别是 -）
}

// enter 挂在 Pre 链的最外面，给内置中间件备好两样东西：请求原来的 ctx（内置中间件见到它就只往 echo.Context
// 里写、不动请求，见 reqctx），和直连的对端可不可信（Trace / Propagate 据此决定收不收透传 Header，见 markTrustedPeer）。
//
// c.Set 会建 echo.Context 的存储，一个请求一次，之后的 c.Set 都用它
func (x *XEcho) enter(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		c.Set(reqctx.Key, c.Request().Context())
		x.markTrustedPeer(c)
		return next(c)
	}
}

// attachContext 路由之后、使用者的中间件之前：把 Pre 里建好的 ctx 换到请求上，照常用 r.WithContext 的副本。
//
// 使用者在 e.Pre 里换过请求的 ctx 的话，那一层放进去的值到这里就丢了：两棵 ctx 没法合并，
// 这里保的是 Span 和日志作用域
func attachContext(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if ctx, ok := c.Get(reqctx.Key).(context.Context); ok && ctx != c.Request().Context() {
			c.SetRequest(c.Request().WithContext(ctx))
		}
		return next(c)
	}
}

// markTrustedPeer 直连的对端在 TrustedProxies 里时给链路中间件留个记号：
// 只有这样的对端发来的透传 Header（XTrace.ForwardHeaders）才会被收下。
//
// 透传是「把上游给的值原样带给下游」。照单全收的话，公网客户端发一个
// X-Tenant-Id / X-Internal-Token，就被当成自己人给的，带进内网的每一次调用。
// 「谁是自己人」用的是信任代理的那张表，不另设开关——同一件事只该有一个地方说。
//
// 判的是直连的对端（RemoteAddr），不是 c.RealIP()：后者正是从这些可伪造的头里推出来的。
func (x *XEcho) markTrustedPeer(c echo.Context) {
	if x.trusted.Trusts(web.RemoteIP(c.Request())) {
		c.Set(peer.TrustedKey, true)
	}
}

// cfg 这个实例该用的配置：WithConfig 给的那份，不给就是配置文件里的 XEcho 块。
// 不合法时返回默认值和那个错误，见 build
func (x *XEcho) cfg() (Config, error) {
	if x.override == nil {
		return fileConfig()
	}
	if err := x.override.Validate(); err != nil {
		return DefaultConfig(), fmt.Errorf("invalid config passed to WithConfig: %w", err)
	}
	return *x.override, nil
}

// fileConfig 配置文件里的 XEcho 块，没写的字段是默认值。
//
// 在 Start 之前任何时候调都行：配置文件第一次读的时候才加载，读到的永远是最终值。
// xconfig.Unmarshal 解完会调 Validate；解不出来或者不合法时返回默认值和那个错误
func fileConfig() (Config, error) {
	c := DefaultConfig()
	if err := xconfig.Unmarshal(ConfigKey, &c); err != nil {
		return DefaultConfig(), err
	}
	return c, nil
}

// Start 启动服务并阻塞到它停止。由 xone.Run 调用。
//
// 还没装配的话先装配。监听地址、超时、TLS 和 echo 上的中间件、信任的代理
// 出自同一份配置——装配时取的那份，这里不再重读。它不合法时直接返回错误，不监听。
func (x *XEcho) Start(ctx context.Context) error {
	x.build()
	if x.confErr != nil {
		return xerror.New("xecho", "config", x.confErr)
	}
	return x.server.Start("xecho", x.conf.server(), x.engine)
}

// Stop 优雅关闭服务：等在途请求做完，最多等到 ctx 的截止时间。由 xone.Run 调用。
//
// 等多久只看 ctx，这里不另设上限。在 xone.Run 里它带的是服务那一段停止预算
// （WithStopTimeout 的 2/3，默认 10s）：整个退出流程只有这一份预算，
// 服务能占多少由框架分，不需要再配一个超时去和它对齐。
//
// 单独使用、不经过 xone.Run 时，截止时间由你传进来的 ctx 定：
//
//	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
//	defer cancel()
//	err := x.Stop(ctx)
//
// 不带截止时间的 ctx 就一直等到在途请求全部做完——挂住的请求会让 Stop 一直不返回。
//
// 到点还有请求没做完时强制断开所有连接，再等 handler 返回，并返回错误。
//
// 框架能保证的是：返回 nil 时所有 handler 都已经返回；到点还有 handler
// 没返回时，返回的错误里写明还剩几个。它保证不了的是让一个不看请求 ctx 的
// handler 停下来——Go 没有从外面终止一个协程的办法，断开连接、取消
// 请求的 ctx 已经是能做的全部。handler 里的慢操作（查库、调下游）
// 要传 c.Request().Context()，断连之后才停得下来。
func (x *XEcho) Stop(ctx context.Context) error { return x.server.Stop(ctx, "xecho") }

// ---- 登记 ----

// 本包不登记停止钩子：服务本身由使用者交给 xone.Run 启停，
// 框架的 Runnable 只有一个，那个位置是使用者的。
func init() {
	xhook.BeforeStart(loadConfig, xhook.At(xhook.StageServer))
}

// loadConfig 在启动阶段把 XEcho 块读一遍：认领它，并让配错的值在这里就失败。
//
// 读出来不存：XEcho 在装配时自己去读（CurrentConfig 也是），读到的是同一份最终值。
// 少了这一步，这一块要到服务 Start 时才有人读——那已经在框架检查「没人认领的块」
// 之后了，它会被当成拼错的 key 让启动失败；进程里的服务都用 WithConfig 时更是
// 永远没人读。配错的值也要拖到 Start 才报出来。
func loadConfig(context.Context) error {
	_, err := fileConfig()
	return err
}
