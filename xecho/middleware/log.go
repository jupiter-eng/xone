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
//
// What it replaces is the Writer inside echo.Response, not echo.Response itself: c.String,
// c.JSON and c.Stream all write through Response.Write onto it, and Size and Committed are still
// recorded by echo. echo.Response has no WriteString, so capturing Write alone misses nothing.
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

// Unwrap hands back the original writer. http.ResponseController uses it to walk down layer by
// layer: read and write deadlines (SetReadDeadline / SetWriteDeadline) and EnableFullDuplex all
// return http.ErrNotSupported without it.
func (w *captureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush and Hijack exist for code that type-asserts directly (c.Response().Writer.(http.Flusher),
// older SSE and WebSocket libraries): with Unwrap alone those assertions fail. Both go through
// http.ResponseController to the writer below, which unwraps down layer by layer; when the writer
// below supports neither, Flush does nothing and Hijack returns http.ErrNotSupported, as net/http
// does.
func (w *captureWriter) Flush() { _ = http.NewResponseController(w.ResponseWriter).Flush() }

func (w *captureWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

// Log writes the access log.
//
// An error returned by the handler is rendered into a response in this layer (see finish), so the
// status code and byte count recorded are the ones actually sent.
func Log(opts ...LogOption) echo.MiddlewareFunc {
	o := &logOptions{}
	for _, opt := range opts {
		opt(o)
	}
	var rs routes

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			// Skipped paths, and a disabled level, pass straight through; see web.AccessLog.Skip.
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

			// Finish in a defer: the access log is still written when a panic passes through this
			// layer, the Writer is always restored, and the writer always goes back to the pool.
			defer func() {
				// How the fields are assembled and masked is in web.AccessLog.Log; only the
				// values that can be obtained from echo are read here. Both the request and the
				// ctx are re-read: the layers inside (user middleware beyond Trace and LogScope)
				// may have replaced them. The ctx comes from contextOf: for a request that ended
				// inside e.Pre, the log scope and the span have not been swapped onto the request
				// yet.
				a := web.Access{
					Request:    c.Request(),
					Ctx:        contextOf(c),
					Elapsed:    time.Since(start),
					Route:      rs.of(c), // unmatched when nothing matched; the real path is in path
					Status:     status(c),
					ClientIP:   c.RealIP(), // computed by e.IPExtractor, which xecho set from TrustedProxies
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
					// Restore the writer before returning it to the pool: an outer middleware may
					// still need it.
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
