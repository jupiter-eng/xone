package xgin

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/jupiter-eng/xone/internal/web"
	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xgin/internal/peer"
	"github.com/jupiter-eng/xone/xgin/middleware"
	"github.com/jupiter-eng/xone/xgin/trans"
	"github.com/jupiter-eng/xone/xhook"
	"github.com/jupiter-eng/xone/xmetric"

	// 用了 xgin 就有链路：xtrace 装好全局的 TracerProvider 和 Propagator，
	// 中间件经 otel 的全局 API 用上它们。不要链路配 XTrace.Enable: false
	_ "github.com/jupiter-eng/xone/xtrace"

	// 用了 Web 框架就有 xlog：访问日志、请求级字段（xlog.AddKV）都靠它。
	// 根包不带它，只用 xhook、xgorm 的程序不会被它接管日志
	_ "github.com/jupiter-eng/xone/xlog"
)

// XGin 一个待启动的 HTTP 服务。
//
// 它满足 xone.Runnable，直接交给 xone.Run 即可：
//
//	xone.MustRun(xgin.New().WithRoutes(register))
//
// 注意这里没有 import 根包——Go 的接口是结构化的，方法对得上就行。
// 这样「集成不依赖框架」这条在编译层面仍然成立。
type XGin struct {
	override *Config // 见 WithConfig

	routes  []func(*gin.Engine)
	extra   []gin.HandlerFunc
	recover gin.RecoveryFunc

	// 下面这几项由 build 一次写定，之后只读。读它们的要么先调 build，要么是
	// engine 上的 handler——而 engine 只能经 build 拿到。Once 保证写在读之前，不需要锁
	buildOnce sync.Once
	engine    *gin.Engine
	conf      Config      // 装配用的那份配置，Start 用的也是它
	confErr   error       // 配置不合法时的错误，此时 conf 是默认值
	trusted   web.Proxies // TrustedProxies 解出来的网段，见 markTrustedPeer

	// server 监听和优雅关闭，与 gin 无关的那一半，见 web.Server
	server web.Server
}

// New 创建一个 XGin。
//
// 开关、端口、超时都在配置文件的 XGin 块里；同一个进程里的第二个服务用 WithConfig。
// 这里什么都不读，配置在装配（Engine 或 Start）那一刻才取。零值 &XGin{} 与 New() 等价。
func New() *XGin { return &XGin{} }

// WithConfig 用这份配置起服务，不再读配置文件里的 XGin 块。
//
// 一个进程里起第二个服务时用它——比如对外的 API 一个端口、内部的管理端口
// 另一个。不给它的话两个实例读的是同一块配置，只能监听同一个端口。
//
// 传进来的是完整配置，不是补丁：想在文件配置的基础上改两项，先取
// CurrentConfig 再改（从头写的话从 DefaultConfig 开始，零值的超时是不合法的）：
//
//	c := xgin.CurrentConfig()
//	c.Host, c.Port = "127.0.0.1", 9090
//	admin := xgin.New().WithConfig(c).WithRoutes(adminRoutes)
//
// 它在装配时校验，不合法的话 Start 返回错误、不监听。
func (g *XGin) WithConfig(c Config) *XGin {
	g.override = &c
	return g
}

// CurrentConfig 返回配置文件里 XGin 那一块（拷贝），在 Start 之前任何时候调都行。
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
// 回调拿到的是原生 engine，想怎么设都行。它在配置落到 engine 上之后才跑，
// 所以回调里的设置盖得过配置：改 MaxMultipartMemory、调 SetTrustedProxies 都以回调为准。
//
// 只有一处跟不过去：透传 Header（XTrace.ForwardHeaders）认的可信对端只看配置里的
// TrustedProxies。gin 不暴露它当前的代理列表，回调里的 SetTrustedProxies 只管得到
// ClientIP。两边要一起改就改配置。
func (g *XGin) WithRoutes(f ...func(*gin.Engine)) *XGin {
	g.routes = append(g.routes, f...)
	return g
}

// WithMiddleware 追加自定义中间件，排在所有内置中间件之后。
func (g *XGin) WithMiddleware(m ...gin.HandlerFunc) *XGin {
	g.extra = append(g.extra, m...)
	return g
}

// WithRecoverFunc 自定义 panic 之后的响应。默认返回 500。
func (g *XGin) WithRecoverFunc(f gin.RecoveryFunc) *XGin {
	g.recover = f
	return g
}

// Engine 返回原生的 *gin.Engine，需要做本包没覆盖的事情时用它。
//
// 调用它会触发装配，用的是 WithConfig 给的那份配置，或者配置文件里的 XGin 块——
// 配置文件第一次读的时候才加载，所以在 xone.Run 之前调也拿得到最终值。
// 装配只有一次：之后再 WithConfig / WithRoutes / WithMiddleware / WithRecoverFunc
// 就不生效了，Start 也沿用这一次的配置。
//
// 配置不合法时照样返回一个 engine，按默认值装配（只信私有网段的代理、8MB 的 multipart 阈值，
// 都是偏安全的那一侧），并记一条告警。那个错误由 Start 返回，xone.Run 在启动时就会报出来。
func (g *XGin) Engine() *gin.Engine {
	g.build()
	return g.engine
}

// build 装配 engine，一个实例只装配一次。
//
// 用 Once 而不是布尔标志：并发调用时后者会把中间件注册两遍，
// 表现是每个请求打两条日志、指标翻倍。
//
// 顺序有讲究：先定 Mode，再建 engine、落配置，然后挂中间件和内置路由，最后才跑
// 使用者的 WithRoutes——所以回调里对 engine 的设置盖得过配置，见 WithRoutes。
func (g *XGin) build() {
	g.buildOnce.Do(func() {
		// 配置不合法时照样装配，按默认值：Engine() 的调用方总得拿到一个 engine，
		// 而默认值是偏安全的那一侧。错误记下来由 Start 返回；告警是给只用 Engine()、
		// 从不调 Start 的使用者的，否则他们拿到一份默认值却毫无迹象
		c, err := g.cfg()
		if err != nil {
			slog.Warn("xgin invalid config, assembling the engine with defaults; Start will return the error", "error", err)
		}
		g.conf, g.confErr = c, err

		// 在 gin.New 之前设：debug 模式下 gin.New 会打一段「正在以 debug 模式运行，
		// 线上请切到 release」的警告，之后注册的每条路由再各打一行（实测 v1.12.0）。
		// 先建后设的话，配成 release 的服务照样会在启动时打出那段警告。
		//
		// 注意 gin.SetMode 是进程级的，不是这个实例的：同一进程里用 WithConfig
		// 起两个 Mode 不同的服务，后装配的那个说了算。gin 没有实例级的 Mode，
		// 这里没法替它隔离，所以多实例时请让它们的 Mode 一致
		gin.SetMode(c.Mode)
		e := gin.New()
		e.HandleMethodNotAllowed = true // 不开的话，方法不对会返回 404 而不是 405
		// *gin.Context 本身就是 context.Context，业务里常直接传它：xlog.AddKV(c, ...)、db.WithContext(c)、
		// otel.Tracer(...).Start(c, ...)。不开这一项时它的 Value 只查 c.Keys、Done 返回 nil、Err 永远是 nil
		// （v1.12.0 context.go 的 hasRequestContext）：日志作用域、父 Span 全丢，客户端走了也取消不下去。
		// 开了之后这几样都转到 c.Request.Context()，和传 c.Request.Context() 一样
		e.ContextWithFallback = true
		applyConfig(e, c)
		g.trusted = web.ParseProxies(c.TrustedProxies)

		// 洋葱模型，自外向内：
		//   LogScope → Trace → Log → Metric → Recover → 用户中间件 → handler
		//
		// Recover 必须是内置里最内层的：panic 在哪一层被兜住，
		// 比它更内层的中间件里 c.Next() 之后的代码就都不执行了。
		// 放最内层，外面几层的收尾（记指标、写访问日志）才还跑得到。
		if c.Log {
			e.Use(middleware.LogScope())
		}
		// 先判对端可不可信，Trace / Propagate 才知道收不收透传 Header 和 baggage。
		// Trace 只管 Span：关掉时照样接上游的链路标识和透传 Header，只是不开 Span
		if c.Trace {
			e.Use(g.markTrustedPeer, skipMetrics(c, middleware.Trace()))
		} else {
			e.Use(g.markTrustedPeer, middleware.Propagate())
		}
		if c.Log {
			skip := slices.Clone(c.LogSkipPaths)
			if c.Metric {
				// 指标端点会被抓取系统按秒轮询，记日志纯属刷屏
				skip = append(skip, c.MetricPath)
			}
			e.Use(middleware.Log(
				middleware.WithSkipPaths(skip...),
				middleware.WithBody(c.LogRequestBody, c.LogResponseBody),
				middleware.WithQuery(c.LogQuery),
				middleware.WithHeaders(c.LogRequestHeaders, c.LogResponseHeaders),
			))
		}
		if c.Metric {
			e.Use(middleware.Metric())
		}
		e.Use(middleware.Recover(g.recover))
		e.Use(g.extra...)

		// 路由一律注册在所有中间件之后。
		//
		// gin 在注册路由的那一刻就把处理链定死了：那之后再 Use 的中间件
		// 对它不生效。指标端点原先注册在 g.extra 之前，于是使用者用
		// WithMiddleware 挂的统一鉴权对业务路由生效、对 /metrics 不生效——
		// 一个以为被保护起来的端点其实是敞开的。
		if c.Metric {
			e.GET(c.MetricPath, serveMetrics)
		}
		// gin 默认的 404 / 405 响应体是整条中间件链跑完之后才写的（gin v1.12.0 gin.go 的 serveError），
		// 访问日志记 bytes_out 时它还没写，实测记成 0 而客户端收到 18 字节。在链里写同样的内容，
		// 响应一个字节不差，字节数也记得上。放在用户的路由之前：WithRoutes 里的 NoRoute 照样盖得过
		e.NoRoute(notFound)
		e.NoMethod(methodNotAllowed)
		for _, f := range g.routes {
			f(e)
		}

		if c.ZHTranslations {
			if err := trans.RegisterZH(); err != nil {
				slog.Warn("xgin failed to register the zh validation translator", "error", err)
			}
		}

		g.engine = e
	})
}

// serveMetrics 指标端点。每次请求再取 handler，不在装配时定死：装配可能发生在
// xmetric 初始化之前（在 xone.Run 之前调 Engine() 就会），那时拿到的是兜底 registry，
// /metrics 会一直是空的
func serveMetrics(c *gin.Context) { xmetric.Handler().ServeHTTP(c.Writer, c.Request) }

// skipMetrics 指标端点不进 h：抓取系统按秒轮询它，每次开一个 Span 只是在链路后端刷屏，
// 访问日志也是同样的理由跳过它的。gin 的处理链在注册路由时定死、没法按路由摘掉一个中间件，
// 所以在中间件里按路径判断，和访问日志的 SkipPaths 一样只比 URL.Path
func skipMetrics(c Config, h gin.HandlerFunc) gin.HandlerFunc {
	if !c.Metric {
		return h
	}
	return func(ctx *gin.Context) {
		if ctx.Request.URL.Path == c.MetricPath {
			ctx.Next()
			return
		}
		h(ctx)
	}
}

// applyConfig 把配置里管 engine 的几项落到 e 上
func applyConfig(e *gin.Engine, c Config) {
	e.MaxMultipartMemory = c.MaxMultipartMemory

	// gin 默认开着 RedirectTrailingSlash：/users/ 对不上 /users 时直接回 301（GET）/ 307（其余方法），
	// 整条中间件链都不跑——不进访问日志、指标和链路（实测 v1.12.0，gin.go 的 handleHTTPRequest）。
	// 关掉之后它和别的对不上的路径一样走 NoRoute，记成 404 unmatched。
	// 要重定向的在 WithRoutes 里设回 e.RedirectTrailingSlash = true（重定向的请求照旧不经过中间件）。
	// RedirectFixedPath 在 gin.New 里默认就是 false，不用动
	e.RedirectTrailingSlash = false

	// 默认只信私有网段。gin 的默认是全都信，于是任何人发一个
	// X-Forwarded-For 就能决定访问日志里的 client_ip 是什么。
	// 网段写错在 Validate 里就拦下了，这里的错误兜底成「谁都不信」
	if err := e.SetTrustedProxies(web.ExpandProxies(c.TrustedProxies)); err != nil {
		slog.Warn("xgin invalid TrustedProxies, trusting none", "error", err)
		_ = e.SetTrustedProxies([]string{})
	}
}

// markTrustedPeer 直连的对端在 TrustedProxies 里时给链路中间件留个记号：
// 只有这样的对端发来的透传 Header（XTrace.ForwardHeaders）才会被收下。
//
// 透传是「把上游给的值原样带给下游」。照单全收的话，公网客户端发一个
// X-Tenant-Id / X-Internal-Token，就被当成自己人给的，带进内网的每一次调用。
// 「谁是自己人」用的是信任代理的那张表，不另设开关——同一件事只该有一个地方说。
//
// 判的是直连的对端（RemoteAddr），不是 ClientIP：后者正是从这些可伪造的头里推出来的。
func (g *XGin) markTrustedPeer(c *gin.Context) {
	if g.trusted.Trusts(c.RemoteIP()) {
		c.Set(peer.TrustedKey, true)
	}
	c.Next()
}

// cfg 这个实例该用的配置：WithConfig 给的那份，不给就是配置文件里的 XGin 块。
// 不合法时返回默认值和那个错误，见 build
func (g *XGin) cfg() (Config, error) {
	if g.override == nil {
		return fileConfig()
	}
	if err := g.override.Validate(); err != nil {
		return DefaultConfig(), fmt.Errorf("invalid config passed to WithConfig: %w", err)
	}
	return *g.override, nil
}

// fileConfig 配置文件里的 XGin 块，没写的字段是默认值。
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
// 还没装配的话先装配。监听地址、超时、TLS 和 engine 上的中间件、信任的代理
// 出自同一份配置——装配时取的那份，这里不再重读。它不合法时直接返回错误，不监听。
func (g *XGin) Start(ctx context.Context) error {
	g.build()
	if g.confErr != nil {
		return xerror.New("xgin", "config", g.confErr)
	}
	c := g.conf
	// 交 engine 本身，不交 g.engine.Handler()：使用者在回调里开了 gin 自己的 UseH2C 的话，
	// 后者包一层 x/net 的 h2c.NewHandler，连接被劫持走，优雅关闭等不到它们（理由见 web.Server 的 protocols）。
	// h2c 要开就开 XGin.UseH2C，走标准库
	return g.server.Start("xgin", c.server(), g.engine)
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
//	err := g.Stop(ctx)
//
// 不带截止时间的 ctx 就一直等到在途请求全部做完（实测一个 1.5s 的请求，
// Shutdown 等了 1.57s 才返回）——挂住的请求会让 Stop 一直不返回。
//
// 到点还有请求没做完时强制断开所有连接，再等 handler 返回，并返回错误。
//
// 框架能保证的是：返回 nil 时所有 handler 都已经返回；到点还有 handler
// 没返回时，返回的错误里写明还剩几个。它保证不了的是让一个不看请求 ctx 的
// handler 停下来——Go 没有从外面终止一个协程的办法，断开连接、取消
// 请求的 ctx 已经是能做的全部。handler 里的慢操作（查库、调下游）
// 要传 c.Request.Context()，断连之后才停得下来。
func (g *XGin) Stop(ctx context.Context) error { return g.server.Stop(ctx, "xgin") }

// ---- 登记 ----

// 本包不登记停止钩子：服务本身由使用者交给 xone.Run 启停，
// 框架的 Runnable 只有一个，那个位置是使用者的。
func init() {
	xhook.BeforeStart(loadConfig, xhook.At(xhook.StageServer))
}

// loadConfig 在启动阶段把 XGin 块读一遍：认领它，并让配错的值在这里就失败。
//
// 读出来不存：XGin 在装配时自己去读（CurrentConfig 也是），读到的是同一份最终值。
// 少了这一步，这一块要到服务 Start 时才有人读——那已经在框架检查「没人认领的块」
// 之后了，它会被当成拼错的 key 让启动失败；进程里的服务都用 WithConfig 时更是
// 永远没人读。配错的值也要拖到 Start 才报出来。
func loadConfig(context.Context) error {
	_, err := fileConfig()
	return err
}

// notFound / methodNotAllowed 写的和 gin 的默认响应一样：Content-Type 是 text/plain，正文一字不差
func notFound(c *gin.Context) {
	c.Data(http.StatusNotFound, gin.MIMEPlain, []byte("404 page not found"))
}

func methodNotAllowed(c *gin.Context) {
	c.Data(http.StatusMethodNotAllowed, gin.MIMEPlain, []byte("405 method not allowed"))
}
