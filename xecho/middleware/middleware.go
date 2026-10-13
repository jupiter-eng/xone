// Package middleware 提供 xecho 内置的中间件。
//
// 洋葱模型，自外向内的顺序是（xecho 把它们挂在 e.Pre 上，理由见 xecho/README.md「注意事项」）：
//
//	LogScope → Trace → Log → Metric → Recover → 用户的 e.Pre → router → 用户中间件 → handler
//
// 挂在 e.Use 上也成立，只是看不到在 e.Pre 里就结束了的请求。
//
// Recover 必须是框架中间件里最内层的一个：panic 一路向外抛，
// 在哪一层被兜住，比它更内层的中间件里 next(c) 之后的代码就都不执行。
// 放在最内层，外面几层的收尾（记指标、写访问日志）才都还跑得到。
//
// 和 gin 不同的一点：echo 里 handler 返回的错误要等整条链返回之后才由 e.HTTPErrorHandler
// 渲染成响应。Trace、Log、Metric 因此都在自己这一层就把错误交给 c.Error、再往外返回 nil，
// 否则它们记下的状态码全是 200，见 finish。单独用其中哪一个都成立。
package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/labstack/echo/v4"

	"github.com/jupiter-eng/xone/internal/web"
	"github.com/jupiter-eng/xone/xecho/internal/reqctx"
	"github.com/jupiter-eng/xone/xlog"
)

// LogScope 为本次请求开一个日志 KV 作用域。
//
// 装上之后，业务代码在任意调用层级都可以 xlog.AddKV(ctx, ...) 补字段，
// 不用把新 context 逐层回传——调用栈深处拿不到 echo.Context，本来也没机会回传。
// 写进去的字段对整条请求可见，所以 Log 在 next(c) 之后打的访问日志也带得上。
//
// 必须排在所有中间件最前面：在它之后才有作用域可写。
func LogScope() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			withContext(c, xlog.CtxWithScope(contextOf(c)))
			return next(c)
		}
	}
}

// contextOf 这个请求到目前为止该用的 ctx：xecho 装配时是存在 echo.Context 上的那个（见 reqctx），
// 单独用中间件时就是请求自己的
func contextOf(c echo.Context) context.Context {
	if ctx, ok := c.Get(reqctx.Key).(context.Context); ok {
		return ctx
	}
	return c.Request().Context()
}

// withContext 把 ctx 交给这个请求之后的各层。
//
// xecho 装配时内置中间件在 e.Pre 上，不动请求，只存进 echo.Context：路由之后 xecho 再把它换到请求上，
// 理由见 reqctx。单独用中间件（挂在 e.Use 上，已经路由过了）时照常换请求的 ctx
func withContext(c echo.Context, ctx context.Context) {
	if _, deferred := c.Get(reqctx.Key).(context.Context); deferred {
		c.Set(reqctx.Key, ctx)
		return
	}
	c.SetRequest(c.Request().WithContext(ctx))
}

// routes 取请求的路由模板，访问日志、指标、Span 各持有一份。
//
// 没匹配上的记 web.RouteUnmatched（理由见那里），和 xgin 一样，包括三种 echo 给了模板的情况
// （实测 echo v4.16.0）：
//
//   - 没有任何路由匹配这个路径：c.Path() 是空串；
//   - 路径对、方法不对：c.Path() 是那条路由的模板，handler 是 echo 的 MethodNotAllowedHandler（405）；
//     方法是 OPTIONS 时 echo 自己回 204 和 Allow。这两种 router 都会在 c 上留下 Allow 的值
//     （echo.ContextKeyHeaderAllow），别的情况不留；
//   - 落到了 RouteNotFound 注册的兜底上：c.Path() 是兜底的模板。使用者不写 RouteNotFound 也会有——
//     g.Use(...) 和 e.Group(prefix, mw...) 给分组悄悄注册了 "<prefix>" 和 "<prefix>/*" 两条兜底，
//     好让分组中间件对分组下的 404 也生效。于是分组下没匹配上的路径记成 /api/v1/*；
//     方法不对也一样，兜底比 405 优先，echo 回的是 404。
//
// 405 和兜底记成模板的话，xgin 服务和 xecho 服务的同一种请求在看板上是两个样子；
// 而且那条路由在这个方法上根本不存在
type routes struct {
	once sync.Once
	// fallback 注册过兜底的模板 → 同一模板上真正注册了的方法。兜底和真正的路由可以是同一个模板
	// （分组里 GET("/*") 做转发，echo 又给分组注册了 /proxy/* 的兜底），所以按方法分：
	// GET 命中的是那条路由，别的方法落到兜底上
	fallback map[string]map[string]bool
}

// of 这个请求的路由模板，没匹配上是 web.RouteUnmatched
func (rs *routes) of(c echo.Context) string {
	p := c.Path()
	if p == "" || c.Get(echo.ContextKeyHeaderAllow) != nil {
		return web.RouteUnmatched
	}
	// 兜底的表在第一个请求到来时才从 echo 里取：路由注册在前、服务请求在后（echo 的 router
	// 不能边服务边加路由），这时候注册的已经全了。xecho 装配之后才跑 WithRoutes，
	// 使用者还可能拿 Engine() 再加，装配时取就漏了这些
	rs.once.Do(func() { rs.fallback = fallbackRoutes(c.Echo()) })
	if methods, ok := rs.fallback[p]; ok && !methods[c.Request().Method] {
		return web.RouteUnmatched
	}
	return p
}

// fallbackRoutes e 上所有 RouteNotFound 注册的模板（echo.RouteNotFound 是它们在 Routes() 里的 Method），
// 以及同一模板上真正注册了的方法。按 Host 分的 router 一起算：模板只是标签，不必分得那么细
func fallbackRoutes(e *echo.Echo) map[string]map[string]bool {
	all := e.Routes()
	for _, r := range e.Routers() {
		all = append(all, r.Routes()...)
	}
	fallback := map[string]map[string]bool{}
	for _, r := range all {
		if r.Method == echo.RouteNotFound {
			fallback[r.Path] = map[string]bool{}
		}
	}
	for _, r := range all {
		if methods, ok := fallback[r.Path]; ok && r.Method != echo.RouteNotFound {
			methods[r.Method] = true
		}
	}
	return fallback
}

// 这几样记在 echo.Context 上，见 finish、Recover、status
const (
	errKey     = "xone/xecho.error"   // 这个请求的错误：handler 返回的，或者 Recover 登记的中止和断连
	abortedKey = "xone/xecho.aborted" // handler 以 panic(http.ErrAbortHandler) 中止，由 Recover 登记
	goneKey    = "xone/xecho.gone"    // 客户端已经走了、响应一个字节都没发，由 finish 登记
)

// finish 在这一层就把 handler 返回的错误渲染成响应，并把它记在 c 上。ctx 是这一层一进来时请求的 ctx，
// 用来判客户端是不是已经走了（见 web.ClientGone）。
//
// echo 在整条中间件链返回之后才调 e.HTTPErrorHandler 把错误写成响应（echo v4.16.0 的
// Echo.ServeHTTP）。链里的中间件拿到错误的那一刻响应还一个字节都没写：实测 404、405、500
// 都是 Status 200、Committed false、Size 0。照读的话每个出错的请求在访问日志、指标、链路里
// 都是一次 bytes_out 为 0 的 200。
//
// 所以在这里就交给 c.Error（它调的是 e.HTTPErrorHandler，WithRoutes 里换掉的也算），
// 再往外返回 nil：外面几层读到的是已经发出去的状态码和字节数，HTTPErrorHandler 也只跑这一次——
// 照样返回错误的话 echo 会再调它一遍，不查 Committed 的自定义错误处理会把错误响应写两遍。
//
// 错误本身记在 c 上：外面几层拿到的是 nil，访问日志的 errors、Span 的 echo.errors 靠 errorOf 取到它
func finish(c echo.Context, ctx context.Context, err error) {
	// 客户端走了、还什么都没发：记 499（见 web.StatusAborted）。要在 c.Error 之前判——
	// 它往断开的连接上写错误响应照样置上 Committed、把 Status 记成 500（实测 echo v4.16.0，写失败也一样）
	if !c.Response().Committed && web.ClientGone(ctx) {
		c.Set(goneKey, true)
	}
	if err == nil {
		return
	}
	c.Set(errKey, err)
	c.Error(err)
}

// errorOf 这个请求登记下的错误：handler 返回的（见 finish），或者 Recover 登记的中止和断连
func errorOf(c echo.Context) error {
	err, _ := c.Get(errKey).(error)
	return err
}

// aborted handler 是不是以 panic(http.ErrAbortHandler) 中止的。
//
// 看的是 Recover 登记的记号，不看错误本身：handler 返回（不是 panic）一个包着 http.ErrAbortHandler
// 的错误时连接并没有断，客户端收到的是 HTTPErrorHandler 渲染的 500，该记 500
func aborted(c echo.Context) bool {
	v, _ := c.Get(abortedKey).(bool)
	return v
}

// Recover 兜住 panic，把它变成一条错误日志和一个错误响应。
//
// handle 决定 panic 之后回什么：它返回的错误和 handler 返回的一样交给 e.HTTPErrorHandler。
// 为 nil 时返回 echo.ErrInternalServerError，也就是 echo 默认的
// 500 {"message":"Internal Server Error"}，和 handler 返回一个普通 error 时一个样子。
//
// echo 自己默认不兜 panic：实测连接被直接断掉（客户端读到 EOF），栈由 net/http 的日志写进 stderr，
// 不进 slog。http.ErrAbortHandler 不兜，原样抛给 net/http 去断开连接。
func Recover(handle func(c echo.Context, recovered any) error) echo.MiddlewareFunc {
	if handle == nil {
		handle = func(echo.Context, any) error { return echo.ErrInternalServerError }
	}

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) (err error) {
			defer func() {
				r := recover()
				if r == nil {
					return
				}
				// http.ErrAbortHandler 是 handler 主动要求「断掉这个连接」的约定写法
				// （httputil.ReverseProxy 在上游断开时也这么做）。net/http 接到它会
				// 静默断连、不打栈；在这里兜住的话，本该中止的响应会被写成 500 发出去，
				// 还多一份毫无意义的栈。所以原样抛回给它。
				//
				// 抛回之前登记下来：外面几层（访问日志、指标、链路）的收尾据此
				// 把它记成中止，见 status——它们读到的 Response().Status 往往是
				// 已经发出去的 200，一个被截断的响应就全都记成了成功
				if r == http.ErrAbortHandler {
					c.Set(abortedKey, true)
					c.Set(errKey, http.ErrAbortHandler)
					panic(r)
				}

				// 连接断了不算故障：不打栈，记 WARN 而不是 ERROR——客户端的网络抖动不该触发服务的告警
				ctx := contextOf(c)
				if web.IsBrokenPipe(r) {
					slog.WarnContext(ctx, "connection broken", "error", r)
					c.Set(errKey, r.(error)) // web.IsBrokenPipe 保证它是 *net.OpError
					err = nil
					return
				}

				// panic 的值和 handler 返回的错误一样会夹带凭证（panic(err) 里的整串 DSN），
				// 过一遍 RedactText 再记。栈不用过：runtime.Stack 只打函数名、文件行号和参数的十六进制原始字
				// （指针、长度），字符串和结构体的内容不在里面
				slog.ErrorContext(ctx, "panic while handling request",
					"error", web.RedactText(fmt.Sprint(r)),
					"stack", web.Stack(),
					"path", c.Request().URL.Path,
					"method", c.Request().Method)

				if c.Response().Committed {
					// 响应已经开始往外写了，再改状态码只会得到一个半截的响应
					err = nil
					return
				}
				err = handle(c, r)
			}()
			return next(c)
		}
	}
}

// status 本次请求该记下的状态码。
//
// 不能直接读 Response().Status：中止的请求往往已经写出了 200 的响应头，客户端走了的请求
// 可能被渲染成了一个谁也收不到的 500。这两种记 499，见 web.StatusAborted
func status(c echo.Context) int {
	if gone, _ := c.Get(goneKey).(bool); gone || aborted(c) {
		return web.StatusAborted
	}
	return c.Response().Status
}
