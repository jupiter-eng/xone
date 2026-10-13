// Package middleware 提供 xgin 内置的中间件。
//
// 洋葱模型，自外向内的顺序是：
//
//	LogScope → Trace → Log → Metric → Recover → 用户中间件 → handler
//
// Recover 必须是框架中间件里最内层的一个：panic 一路向外抛，
// 在哪一层被兜住，比它更内层的中间件里 c.Next() 之后的代码就都不执行。
// 放在最内层，外面几层的收尾（记指标、写访问日志）才都还跑得到。
package middleware

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/jupiter-eng/xone/internal/web"
	"github.com/jupiter-eng/xone/xlog"
)

// LogScope 为本次请求开一个日志 KV 作用域。
//
// 装上之后，业务代码在任意调用层级都可以 xlog.AddKV(ctx, ...) 补字段，
// 不用把新 context 逐层回传——调用栈深处拿不到 *gin.Context，本来也没机会回传。
// 写进去的字段对整条请求可见，所以 Log 在 c.Next() 之后打的访问日志也带得上。
//
// 必须排在所有中间件最前面：在它之后才有作用域可写。
func LogScope() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request = c.Request.WithContext(xlog.CtxWithScope(c.Request.Context()))
		c.Next()
	}
}

// routeOf 取这个请求的路由模板，访问日志、指标、Span 共用。
//
// 没匹配上任何路由（404）、方法不对（405）时 gin 给的都是空串，记成 web.RouteUnmatched，
// 理由见那里
func routeOf(c *gin.Context) string {
	if route := c.FullPath(); route != "" {
		return route
	}
	return web.RouteUnmatched
}

// Recover 兜住 panic，把它变成一条错误日志和一个 500。
//
// handle 为 nil 时返回 500。http.ErrAbortHandler 不兜，原样抛给 net/http 去断开连接。
func Recover(handle gin.RecoveryFunc) gin.HandlerFunc {
	if handle == nil {
		handle = func(c *gin.Context, _ any) { c.AbortWithStatus(http.StatusInternalServerError) }
	}

	return func(c *gin.Context) {
		defer func() {
			err := recover()
			if err == nil {
				return
			}
			// http.ErrAbortHandler 是 handler 主动要求「断掉这个连接」的约定写法
			// （httputil.ReverseProxy 在上游断开时也这么做）。net/http 接到它会
			// 静默断连、不打栈；在这里兜住的话，本该中止的响应会被写成 500 发出去，
			// 还多一份毫无意义的栈。所以原样抛回给它。
			//
			// 抛回之前记进 c.Errors：外面几层（访问日志、指标、链路）的收尾据此
			// 把它记成中止，见 status——它们读到的 c.Writer.Status() 往往是
			// 已经发出去的 200，一个被截断的响应就全都记成了成功
			if err == http.ErrAbortHandler {
				_ = c.Error(http.ErrAbortHandler) //nolint:errcheck // 只是登记
				panic(err)
			}

			// 连接断了不算故障：不打栈，记 WARN 而不是 ERROR——客户端的网络抖动不该触发服务的告警
			broken := web.IsBrokenPipe(err)
			ctx := c.Request.Context()
			if broken {
				slog.WarnContext(ctx, "connection broken", "error", err)
				_ = c.Error(err.(error)) //nolint:errcheck // web.IsBrokenPipe 保证它是 *net.OpError
				c.Abort()
				return
			}

			// panic 的值和 handler 返回的错误一样会夹带凭证（panic(err) 里的整串 DSN），
			// 过一遍 RedactText 再记。栈不用过：runtime.Stack 只打函数名、文件行号和参数的十六进制原始字
			// （指针、长度），字符串和结构体的内容不在里面
			slog.ErrorContext(ctx, "panic while handling request",
				"error", web.RedactText(fmt.Sprint(err)),
				"stack", web.Stack(),
				"path", c.Request.URL.Path,
				"method", c.Request.Method)

			if c.Writer.Written() {
				// 响应已经开始往外写了，再改状态码只会得到一个半截的响应
				c.Abort()
				return
			}
			handle(c, err)
		}()
		c.Next()
	}
}

// statusAborted 没有正常结束的请求记成的状态码（499），见 web.StatusAborted
const statusAborted = web.StatusAborted

// status 本次请求该记下的状态码。ctx 是这一层一进来时请求的 ctx，用来判客户端是不是已经走了
// （见 web.ClientGone）。
//
// 不能直接读 c.Writer.Status()：中止的请求往往已经写出了 200 的响应头，客户端走了、
// 什么都没写就返回的请求是一个谁也收不到的 200。这两种记 499，见 web.StatusAborted
func status(c *gin.Context, ctx context.Context) int {
	if aborted(c) || (!c.Writer.Written() && web.ClientGone(ctx)) {
		return statusAborted
	}
	return c.Writer.Status()
}

// aborted handler 是不是以 panic(http.ErrAbortHandler) 中止的，由 Recover 登记在 c.Errors 里
func aborted(c *gin.Context) bool {
	for _, e := range c.Errors {
		if errors.Is(e.Err, http.ErrAbortHandler) {
			return true
		}
	}
	return false
}
