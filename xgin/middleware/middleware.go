// Package middleware holds xgin's built-in middleware.
//
// The onion, outermost first:
//
//	LogScope -> Trace -> Log -> Metric -> Recover -> the caller's middleware -> handler
//
// Recover has to be the innermost of the framework's middleware: a panic travels outwards, and
// whichever layer catches it, the code after c.Next() in every layer inside it never runs.
// Innermost, the outer layers still get to finish their work (record the metric, write the
// access log).
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

// LogScope opens a log key-value scope for this request.
//
// With it installed, business code can call xlog.AddKV(ctx, ...) at any depth to add fields,
// without passing a new context back up layer by layer — deep in the call stack there is no
// *gin.Context anyway, so there was never a chance to pass it back. Fields written there are
// visible for the whole request, so the access log Log writes after c.Next() carries them too.
//
// It has to come before every other middleware: only after it is there a scope to write to.
func LogScope() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request = c.Request.WithContext(xlog.CtxWithScope(c.Request.Context()))
		c.Next()
	}
}

// routeOf resolves this request's route template, shared by the access log, the metrics and the
// span.
//
// With no route matching (404) and with a wrong method (405), gin hands out an empty string, which
// is recorded as web.RouteUnmatched; the reasoning is there.
func routeOf(c *gin.Context) string {
	if route := c.FullPath(); route != "" {
		return route
	}
	return web.RouteUnmatched
}

// Recover catches a panic and turns it into an error log line and a 500.
//
// When handle is nil, a 500 is returned. http.ErrAbortHandler is not caught and is re-panicked for
// net/http to break the connection.
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
			// http.ErrAbortHandler is the agreed way for a handler to ask for this connection to be
			// dropped (httputil.ReverseProxy does it when the upstream goes away). net/http receives
			// it, breaks the connection silently and prints no stack; catching it here would turn a
			// response that was meant to be aborted into a 500 going out, plus a stack trace that
			// means nothing. So it is re-panicked.
			//
			// It goes into c.Errors before being re-panicked: the outer layers (access log, metrics,
			// tracing) use that to record it as an abort, see status — what they read from
			// c.Writer.Status() is usually the 200 already sent, which would record a truncated
			// response as a success.
			if err == http.ErrAbortHandler {
				_ = c.Error(http.ErrAbortHandler) //nolint:errcheck // only recording it
				panic(err)
			}

			// A broken connection is not a failure: no stack, and WARN rather than ERROR — a client's
			// flaky network should not fire the service's alerts.
			broken := web.IsBrokenPipe(err)
			ctx := c.Request.Context()
			if broken {
				slog.WarnContext(ctx, "connection broken", "error", err)
				_ = c.Error(err.(error)) //nolint:errcheck // web.IsBrokenPipe guarantees it is an *net.OpError
				c.Abort()
				return
			}

			// A panic value carries credentials just as an error returned by a handler does (the whole
			// DSN inside panic(err)), so it goes through RedactText before being recorded. The stack
			// does not need it: runtime.Stack prints function names, file and line, and the raw
			// hexadecimal words of the arguments (pointers, lengths) — the contents of strings and
			// structs are not in there.
			slog.ErrorContext(ctx, "panic while handling request",
				"error", web.RedactText(fmt.Sprint(err)),
				"stack", web.Stack(),
				"path", c.Request.URL.Path,
				"method", c.Request.Method)

			if c.Writer.Written() {
				// The response has started going out; changing the status code now only yields a
				// truncated response.
				c.Abort()
				return
			}
			handle(c, err)
		}()
		c.Next()
	}
}

// statusAborted is the status code recorded for a request that did not end normally (499), see
// web.StatusAborted.
const statusAborted = web.StatusAborted

// status is the status code that should be recorded for this request. ctx is the request's ctx as
// this layer saw it on the way in, used to tell whether the client is already gone (see
// web.ClientGone).
//
// c.Writer.Status() cannot be read directly: an aborted request has usually written a 200 header
// already, and a request whose client left and that returned without writing anything is a 200
// nobody will ever receive. Both are recorded as 499, see web.StatusAborted.
func status(c *gin.Context, ctx context.Context) int {
	if aborted(c) || (!c.Writer.Written() && web.ClientGone(ctx)) {
		return statusAborted
	}
	return c.Writer.Status()
}

// aborted reports whether the handler aborted with panic(http.ErrAbortHandler), which Recover
// records in c.Errors.
func aborted(c *gin.Context) bool {
	for _, e := range c.Errors {
		if errors.Is(e.Err, http.ErrAbortHandler) {
			return true
		}
	}
	return false
}
