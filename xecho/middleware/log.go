package middleware

import (
	"bufio"
	"bytes"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/jupiter-eng/xone/internal/web"
)

// maxResponseBody 响应体记录上限，见 web.MaxResponseBody
const maxResponseBody = web.MaxResponseBody

// LogOption 日志中间件的配置项
type LogOption func(*logOptions)

// logOptions 记什么、跳过什么。字段的取舍和日志的拼法与框架无关，在 internal/web
type logOptions = web.AccessLog

// WithSkipPaths 指定不记访问日志的路径。
//
// 以 / 结尾的按前缀匹配（/health/ 命中 /health/live），其余精确匹配。
func WithSkipPaths(paths ...string) LogOption {
	return func(o *logOptions) { o.SkipPaths(paths...) }
}

// WithBody 是否记录请求体和响应体。默认都不记。
//
// 默认关掉是因为记 body 的代价和风险都不小：要把请求体缓存一份、
// 要包一层 ResponseWriter 截响应，还要对每个字段做脱敏。
// 需要排查时再打开，并确认脱敏字段配全了。
func WithBody(request, response bool) LogOption {
	return func(o *logOptions) { o.ReqBody, o.RespBody = request, response }
}

// WithQuery 是否记录查询串。默认不记：查询串里常有凭证（?token=、签名、OAuth 的 code）。
// 打开后按字段脱敏，规则同表单 body。
func WithQuery(on bool) LogOption {
	return func(o *logOptions) { o.Query = on }
}

// WithHeaders 是否记录请求头和响应头。默认都不记。
//
// 打开后凭证类的值遮掉：Authorization、Cookie、Set-Cookie 等名单里的，
// 名字带敏感词的（X-Csrf-Token），值是 URL 的去掉查询串（Referer）。
func WithHeaders(request, response bool) LogOption {
	return func(o *logOptions) { o.ReqHeader, o.RespHeader = request, response }
}

// writerPool 复用截响应用的 writer
var writerPool = sync.Pool{New: func() any { return &captureWriter{buf: &bytes.Buffer{}} }}

// captureWriter 在写响应的同时截一份副本。
//
// 换掉的是 echo.Response 里面那个 Writer，不是 echo.Response 本身：c.String、c.JSON、
// c.Stream 都经 Response.Write 写到它上面，Size、Committed 照旧由 echo 记。
// echo.Response 没有 WriteString，所以只截 Write 就不会漏。
type captureWriter struct {
	http.ResponseWriter
	buf *bytes.Buffer
}

func (w *captureWriter) Write(b []byte) (int, error) {
	if w.buf.Len() < maxResponseBody {
		w.buf.Write(b[:min(len(b), maxResponseBody-w.buf.Len())])
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap 交出原来的 writer。http.ResponseController 靠它一层层找下去：读写超时
// （SetReadDeadline / SetWriteDeadline）、EnableFullDuplex，少了它一律 http.ErrNotSupported
func (w *captureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush 和 Hijack 给直接做类型断言的代码（c.Response().Writer.(http.Flusher)，老的 SSE、WebSocket 库）：
// 只有 Unwrap 的话它们断言失败。两个都经 http.ResponseController 转给下面的 writer，
// 它一层层 Unwrap 下去；下面不支持时 Flush 什么都不做，Hijack 返回 http.ErrNotSupported，和 net/http 一样
func (w *captureWriter) Flush() { _ = http.NewResponseController(w.ResponseWriter).Flush() }

func (w *captureWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

// Log 记访问日志。
//
// handler 返回的错误在这一层渲染成响应（见 finish），记下的状态码、字节数是真正发出去的那些。
func Log(opts ...LogOption) echo.MiddlewareFunc {
	o := &logOptions{}
	for _, opt := range opts {
		opt(o)
	}
	var rs routes

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			// 跳过的路径、级别关着时直接放行，见 web.AccessLog.Skip
			if o.Skip(c.Request()) {
				return next(c)
			}

			start, ctx := time.Now(), contextOf(c)
			var reqBody []byte
			if o.ReqBody {
				reqBody = web.SnapshotBody(c.Request())
			}

			resp := c.Response()
			orig := resp.Writer
			var cw *captureWriter
			if o.RespBody {
				cw = writerPool.Get().(*captureWriter)
				cw.ResponseWriter = orig
				cw.buf.Reset()
				resp.Writer = cw
			}

			// 用 defer 收尾：即使 panic 穿过本层，访问日志仍然写得出去，
			// Writer 也一定会还原、writer 一定会还回池子
			defer func() {
				// 字段怎么拼、怎么脱敏在 web.AccessLog.Log；这里只取 echo 里才取得到的那几个值。
				// 请求和 ctx 都要重新取：里面几层（Trace、LogScope 之外的用户中间件）可能换过它们。
				// ctx 取 contextOf：在 e.Pre 里就结束了的请求，日志作用域和 Span 还没换到请求上
				a := web.Access{
					Request:    c.Request(),
					Ctx:        contextOf(c),
					Elapsed:    time.Since(start),
					Route:      rs.of(c), // 没匹配上时记 unmatched，真实路径在 path 里
					Status:     status(c),
					ClientIP:   c.RealIP(), // 按 e.IPExtractor 算，xecho 按 TrustedProxies 装好了它
					BytesOut:   int(resp.Size),
					RespHeader: resp.Header(),
					ReqBody:    reqBody,
				}
				if cw != nil {
					a.RespBody = cw.buf.Bytes()
				}
				if err := errorOf(c); err != nil {
					a.Errors = err.Error()
				}
				o.Log(&a)

				if cw != nil {
					// 先还原 writer，再还回池子：外层中间件可能还要用它
					resp.Writer = orig
					cw.ResponseWriter = nil
					writerPool.Put(cw)
				}
			}()

			finish(c, ctx, next(c))
			return nil
		}
	}
}
