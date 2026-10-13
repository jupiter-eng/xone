package middleware

import (
	"bytes"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

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

// captureWriter 在写响应的同时截一份副本
type captureWriter struct {
	gin.ResponseWriter
	buf     *bytes.Buffer
	capture bool
}

func (w *captureWriter) Write(b []byte) (int, error) {
	if w.capture && w.buf.Len() < maxResponseBody {
		w.buf.Write(b[:min(len(b), maxResponseBody-w.buf.Len())])
	}
	return w.ResponseWriter.Write(b)
}

func (w *captureWriter) WriteString(s string) (int, error) {
	if w.capture && w.buf.Len() < maxResponseBody {
		w.buf.WriteString(s[:min(len(s), maxResponseBody-w.buf.Len())])
	}
	return w.ResponseWriter.WriteString(s)
}

// Log 记访问日志。
func Log(opts ...LogOption) gin.HandlerFunc {
	o := &logOptions{}
	for _, opt := range opts {
		opt(o)
	}

	return func(c *gin.Context) {
		// 跳过的路径、级别关着时直接放行，见 web.AccessLog.Skip
		if o.Skip(c.Request) {
			c.Next()
			return
		}

		start, ctx := time.Now(), c.Request.Context()
		var reqBody []byte
		if o.ReqBody {
			reqBody = web.SnapshotBody(c.Request)
		}

		orig := c.Writer
		var cw *captureWriter
		if o.RespBody {
			cw = writerPool.Get().(*captureWriter)
			cw.ResponseWriter, cw.capture = orig, true
			cw.buf.Reset()
			c.Writer = cw
		}

		// 用 defer 收尾：即使 panic 穿过本层，访问日志仍然写得出去，
		// c.Writer 也一定会还原、writer 一定会还回池子
		defer func() {
			// 字段怎么拼、怎么脱敏在 web.AccessLog.Log；这里只取 gin 里才取得到的那几个值
			a := web.Access{
				Request:  c.Request,
				Elapsed:  time.Since(start),
				Route:    routeOf(c), // 没匹配上时记 unmatched，真实路径在 path 里
				Status:   status(c, ctx),
				ClientIP: c.ClientIP(),
				// 一个字节都没写时 gin 给的是 -1，记成 0
				BytesOut:   max(c.Writer.Size(), 0),
				RespHeader: c.Writer.Header(),
				ReqBody:    reqBody,
			}
			if cw != nil {
				a.RespBody = cw.buf.Bytes()
			}
			if len(c.Errors) > 0 {
				a.Errors = c.Errors.String()
			}
			o.Log(&a)

			if cw != nil {
				// 先还原 writer，再还回池子：外层中间件可能还要用它
				c.Writer = orig
				cw.ResponseWriter, cw.capture = nil, false
				writerPool.Put(cw)
			}
		}()

		c.Next()
	}
}
