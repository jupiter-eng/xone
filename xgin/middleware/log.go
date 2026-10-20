package middleware

import (
	"bytes"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/jupiter-eng/xone/internal/web"
)

// maxResponseBody is the cap on how much response body is recorded, see web.MaxResponseBody.
const maxResponseBody = web.MaxResponseBody

// LogOption configures the log middleware.
type LogOption func(*logOptions)

// logOptions is what to record and what to skip. Which fields exist and how the log line is
// assembled has nothing to do with the framework and lives in internal/web.
type logOptions = web.AccessLog

// WithSkipPaths names the paths that are not access-logged.
//
// A path ending in / matches by prefix (/health/ matches /health/live), anything else matches
// exactly.
func WithSkipPaths(paths ...string) LogOption {
	return func(o *logOptions) { o.SkipPaths(paths...) }
}

// WithBody records the request body and the response body. Both off by default.
//
// Off by default because recording a body costs and risks enough to be a decision: the request
// body has to be buffered, the ResponseWriter has to be wrapped to capture the response, and
// every field has to be masked. Turn it on while debugging, and make sure the masked fields are
// complete.
func WithBody(request, response bool) LogOption {
	return func(o *logOptions) { o.ReqBody, o.RespBody = request, response }
}

// WithQuery records the query string. Off by default: query strings often carry credentials
// (?token=, signatures, an OAuth code). With it on, fields are masked by the same rules as a form
// body.
func WithQuery(on bool) LogOption {
	return func(o *logOptions) { o.Query = on }
}

// WithHeaders records the request headers and the response headers. Both off by default.
//
// With them on, credential-bearing values are masked: the listed ones such as Authorization,
// Cookie and Set-Cookie, any name containing a sensitive word (X-Csrf-Token), and values that are
// URLs lose their query string (Referer).
func WithHeaders(request, response bool) LogOption {
	return func(o *logOptions) { o.ReqHeader, o.RespHeader = request, response }
}

// writerPool reuses the writers used to capture responses.
var writerPool = sync.Pool{New: func() any { return &captureWriter{buf: &bytes.Buffer{}} }}

// captureWriter keeps a copy of the response while it is being written.
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

// Log writes the access log.
func Log(opts ...LogOption) gin.HandlerFunc {
	o := &logOptions{}
	for _, opt := range opts {
		opt(o)
	}

	return func(c *gin.Context) {
		// Skipped paths, and a disabled level, pass straight through; see web.AccessLog.Skip.
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

		// Finish in a defer: the access log is still written when a panic passes through this layer,
		// c.Writer is always restored, and the writer always goes back to the pool.
		defer func() {
			// How the fields are assembled and masked is in web.AccessLog.Log; only the values that can
			// be obtained from gin are read here.
			a := web.Access{
				Request:  c.Request,
				Elapsed:  time.Since(start),
				Route:    routeOf(c), // unmatched when nothing matched; the real path is in path
				Status:   status(c, ctx),
				ClientIP: c.ClientIP(),
				// gin hands out -1 when not one byte was written, which is recorded as 0
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
				// Restore the writer before returning it to the pool: an outer middleware may still need it
				c.Writer = orig
				cw.ResponseWriter, cw.capture = nil, false
				writerPool.Put(cw)
			}
		}()

		c.Next()
	}
}
