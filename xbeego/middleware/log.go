package middleware

import (
	"net/http"
	"strings"
	"time"

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
// body has to be buffered, the response has to be captured on its way out, and every field has to
// be masked. Turn it on while debugging, and make sure the masked fields are complete.
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

// Log writes the access log.
func Log(opts ...LogOption) func(http.Handler) http.Handler {
	o := &logOptions{}
	for _, opt := range opts {
		opt(o)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w, r, st := enter(w, r)

			// Skipped paths, and a disabled level, pass straight through; see web.AccessLog.Skip.
			// The state above is installed first all the same: the layers inside read it.
			if o.Skip(r) {
				next.ServeHTTP(w, r)
				return
			}

			start, ctx := time.Now(), r.Context()
			var reqBody []byte
			if o.ReqBody {
				reqBody = web.SnapshotBody(r)
			}
			if o.RespBody {
				// Capturing starts here rather than in the recorder itself: the recorder is
				// installed for every request, while keeping a copy of the response is only worth
				// its memory for the ones that are logged.
				st.Rec.Capture(maxResponseBody)
			}

			// Finish in a defer: the access log is still written when a panic passes through this
			// layer.
			defer func() {
				// How the fields are assembled and masked is in web.AccessLog.Log; only the values
				// that have to come from outside are read here.
				a := web.Access{
					Request: r,
					Elapsed: time.Since(start),
					Route:   routeOf(st), // unmatched when nothing matched; the real path is in path
					Status:  status(st, ctx),
					// Beego has no client_ip of its own worth recording: ctx.Input.IP() believes
					// whoever sent X-Forwarded-For. What is recorded is the framework's, computed
					// from XBeego.TrustedProxies by the same algorithm xgin and xecho use.
					ClientIP:   st.ClientIP(r),
					BytesOut:   st.Rec.Bytes(),
					RespHeader: w.Header(),
					ReqBody:    reqBody,
				}
				if o.RespBody {
					a.RespBody = st.Rec.Body()
				}
				if len(st.Errors) > 0 {
					// The same separator gin's c.Errors.String() puts between two errors, so one
					// query over the errors field reads the same on all three integrations.
					a.Errors = strings.Join(st.Errors, "\n")
				}
				o.Log(&a)
			}()

			next.ServeHTTP(w, r)
		})
	}
}
