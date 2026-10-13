package web

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
)

// 访问日志、指标、Span 三处共用的几个取值约定。框架各自取出原始值（路由模板、方法、状态码），
// 按这里的规矩收敛之后再记：同一个服务换一个框架，看板和告警上的标签值不该变。

// RouteUnmatched 没匹配上任何路由时，访问日志的 route、指标的 route 标签、Span 名里记的值。
//
// 用路由模板而不是真实路径：/user/123 和 /user/456 是同一个接口，按真实路径记的话
// 指标标签和 Span 名的基数随 URL 里的 id 无限增长，Prometheus 会被撑垮。
// 没匹配上时填真实路径同样撑爆基数，日志里也分不出 /nope 是一个路由还是一次 404。
// 真实路径在访问日志的 path 里
const RouteUnmatched = "unmatched"

// StatusAborted 没有正常结束的请求在访问日志、指标、链路里记成这个状态码。两种情况：
//
//   - handler 以 panic(http.ErrAbortHandler) 中止：net/http 直接断开连接。中止的请求往往已经写出了
//     200 的响应头，照读框架记下的状态码的话，一个被截断的响应在三处都记成成功；
//   - 客户端已经走了（见 ClientGone），而响应一个字节都还没发：handler 照惯例 return ctx.Err()
//     会被渲染成 500，什么都不写就返回会记成 200——客户端其实什么都没收到。
//
// 借用的是 nginx 的 499（client closed request）：这个码不会真的发给客户端，只是给
// 「没有正常结束的请求」一个固定的、查得到的值，不和任何真实的响应混在一起。
// 响应已经开始发了的，记已经发出去的那个状态码：客户端至少收到了一部分
const StatusAborted = 499

// ClientGone 请求的 ctx 是不是被 net/http 取消了：客户端断开了连接（HTTP/2 是流被重置），
// 或者停止时到点强制断连。
//
// ctx 要取中间件一进来时的那个：里面几层换上的 ctx 可能是业务自己的——echo 的 ContextTimeout
// 返回时 defer cancel() 了它，出了那一层就是 Canceled，客户端却还在等响应。
// 只认 Canceled 不认 DeadlineExceeded 是同一个道理：截止时间是业务自己设的，net/http 不设
func ClientGone(ctx context.Context) bool { return errors.Is(ctx.Err(), context.Canceled) }

// knownMethods RFC 9110 定的那几个方法，加上 PATCH
var knownMethods = map[string]struct{}{
	http.MethodGet: {}, http.MethodHead: {}, http.MethodPost: {}, http.MethodPut: {},
	http.MethodPatch: {}, http.MethodDelete: {}, http.MethodConnect: {},
	http.MethodOptions: {}, http.MethodTrace: {},
}

// MethodOther 不认识的方法统一记成这个
const MethodOther = "OTHER"

// NormalizeMethod 把方法收敛到一个固定集合，指标的 method 标签和 Span 名用它。
//
// 路由已经用模板挡住了 URL 里的 id，方法这一维却是照抄请求的——而 HTTP 的
// 方法是一个自由 token，谁都可以发 CUSTOM1、CUSTOM2。每来一个新值就多一组
// 时间序列，没有淘汰机制：指标内存、抓取响应、监控存储一起涨。
// 就算最后返回 404 / 405 也已经记进去了。
func NormalizeMethod(m string) string {
	if _, ok := knownMethods[m]; ok {
		return m
	}
	return MethodOther
}

// IsBrokenPipe 判断 panic 出来的值是不是客户端提前断开连接（broken pipe / connection reset）。
// 连接断了不算故障，Recover 中间件据此不打栈
func IsBrokenPipe(err any) bool {
	ne, ok := err.(*net.OpError)
	if !ok {
		return false
	}
	var se *os.SyscallError
	if !errors.As(ne, &se) {
		return false
	}
	msg := strings.ToLower(se.Error())
	return strings.Contains(msg, "broken pipe") || strings.Contains(msg, "connection reset by peer")
}

// maxStack panic 栈信息的上限
const maxStack = 16 * 1024

// Stack 当前协程的栈，最多 16KB。
//
// 用 runtime.Stack 而不是逐帧读源码文件：panic 恢复期间去做文件 I/O，
// 磁盘一慢就把这条错误日志也拖住了。
func Stack() string {
	buf := make([]byte, maxStack)
	return string(buf[:runtime.Stack(buf, false)])
}
