// Package reqctx 是 xecho 和它的内置中间件之间传「这个请求该用的 ctx」用的约定。
//
// 内置中间件挂在 e.Pre 上，那时还不能动请求：echo 的 router 按请求刚进来时的那个 *http.Request 找路由，
// 使用者的 e.Pre(echomw.MethodOverride()) 改的也是它，换成 r.WithContext 的副本的话路由就找错了；
// 原地改又违反 net/http 的约定（handler 不该改交进来的 Request），handler 把请求交给
// 活得比它久的协程时还是数据竞争。所以 Pre 里只把日志作用域、Span 挂在 ctx 上、存进 echo.Context，
// 路由之后 xecho 挂的第一个 e.Use 中间件再照常 c.SetRequest(r.WithContext(ctx))。
//
// 放在 internal 里：这是框架内部两个包之间的事，使用者不需要、也不该碰它。
package reqctx

// Key echo.Context 里的 key，值是 context.Context：这个请求到目前为止该用的 ctx。
//
// xecho 在 Pre 链的最外面写进请求原来的 ctx，内置中间件见到它就只往这里写、不动请求；
// 没有它（单独用中间件，挂在 e.Use 上）时中间件照老样子换请求的 ctx
const Key = "xone/xecho.ctx"
