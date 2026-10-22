// Package middleware holds xecho's built-in middleware.
//
// The onion, outermost first (xecho mounts them on e.Pre; the reasoning is under "Notes" in
// xecho/README.md):
//
//	LogScope -> Trace -> Log -> Metric -> Recover -> the caller's e.Pre -> router ->
//	the caller's middleware -> handler
//
// Mounting them on e.Use works too, it just cannot see requests that ended inside e.Pre.
//
// Recover has to be the innermost of the framework's middleware: a panic travels outwards, and
// whichever layer catches it, the code after next(c) in every layer inside it never runs.
// Innermost, the outer layers still get to finish their work (record the metric, write the
// access log).
//
// One difference from gin: in echo, an error returned by a handler is only rendered into a
// response by e.HTTPErrorHandler once the whole chain has returned. Trace, Log and Metric
// therefore hand the error to c.Error in their own layer and return nil outwards, since otherwise
// the status code they record is always 200; see finish. Each of them works on its own as well.
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

// LogScope opens a log key-value scope for this request.
//
// With it installed, business code can call xlog.AddKV(ctx, ...) at any depth to add fields,
// without passing a new context back up layer by layer — deep in the call stack there is no
// echo.Context anyway, so there was never a chance to pass it back. Fields written there are
// visible for the whole request, so the access log Log writes after next(c) carries them too.
//
// It has to come before every other middleware: only after it is there a scope to write to.
func LogScope() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			withContext(c, xlog.CtxWithScope(contextOf(c)))
			return next(c)
		}
	}
}

// contextOf is the ctx this request should be using so far: the one stored on echo.Context when
// xecho assembled the server (see reqctx), or the request's own when the middleware is used
// on its own.
func contextOf(c echo.Context) context.Context {
	if ctx, ok := c.Get(reqctx.Key).(context.Context); ok {
		return ctx
	}
	return c.Request().Context()
}

// withContext hands ctx to the layers after this one.
//
// Under xecho the built-in middleware sits on e.Pre, touches no request and only stores into
// echo.Context: xecho swaps it onto the request after routing, the reasoning is in reqctx. Used
// on its own (mounted on e.Use, where routing already happened) the middleware swaps the
// request's ctx as usual.
func withContext(c echo.Context, ctx context.Context) {
	if _, deferred := c.Get(reqctx.Key).(context.Context); deferred {
		c.Set(reqctx.Key, ctx)
		return
	}
	c.SetRequest(c.Request().WithContext(ctx))
}

// routes resolves a request's route template, one copy each for the access log, the metrics and
// the span.
//
// Anything unmatched is recorded as web.RouteUnmatched (the reasoning is there), as in xgin, and
// that includes the three cases where echo does hand out a template (measured on echo v4.16.0):
//
//   - no route matches the path at all: c.Path() is empty;
//   - the path matches and the method does not: c.Path() is that route's template and the handler
//     is echo's MethodNotAllowedHandler (405); for OPTIONS, echo answers 204 with Allow itself.
//     In both cases the router leaves the Allow value on c (echo.ContextKeyHeaderAllow), which it
//     does not do otherwise;
//   - the request landed on a fallback registered through RouteNotFound: c.Path() is the
//     fallback's template. These exist without the caller writing RouteNotFound — g.Use(...) and
//     e.Group(prefix, mw...) quietly register "<prefix>" and "<prefix>/*" for the group so that
//     group middleware applies to 404s under it too. An unmatched path under the group would
//     therefore be recorded as /api/v1/*; so would a wrong method, since the fallback wins over
//     405 and echo answers 404.
//
// Recording the 405 and the fallback as templates would make the same request look like two
// different things on a dashboard depending on whether the service runs xgin or xecho — and that
// route does not exist for that method in the first place.
//
// The table is taken from the echo the first request arrived on and kept, so one of these serves
// one *echo.Echo: xecho builds a middleware of its own per XEcho, and a caller mounting the same
// value on a second echo would have its requests labelled by the first one's routes. Taking it per
// request instead would walk every route on every request, which is what the table is there to
// avoid.
type routes struct {
	once sync.Once
	// fallback maps a template that has a fallback registered to the methods actually registered
	// on that same template. A fallback and a real route can share a template (a group with
	// GET("/*") proxying, plus the /proxy/* fallback echo registered for the group), so it is
	// kept per method: GET hits the route, every other method lands on the fallback.
	fallback map[string]map[string]bool
}

// of is this request's route template, or web.RouteUnmatched when nothing matched.
func (rs *routes) of(c echo.Context) string {
	p := c.Path()
	if p == "" || c.Get(echo.ContextKeyHeaderAllow) != nil {
		return web.RouteUnmatched
	}
	// The fallback table is taken from echo when the first request arrives: routes are registered
	// before requests are served (echo's router cannot take new routes while serving), so by then
	// registration is complete. WithRoutes only runs after xecho has assembled, and the caller
	// may add more through Engine(), so taking it at assembly time would miss those.
	rs.once.Do(func() { rs.fallback = fallbackRoutes(c.Echo()) })
	if methods, ok := rs.fallback[p]; ok && !methods[c.Request().Method] {
		return web.RouteUnmatched
	}
	return p
}

// fallbackRoutes collects every template registered on e through RouteNotFound (echo.RouteNotFound
// is their Method in Routes()), together with the methods actually registered on the same
// template. Host-specific routers are folded in: a template is only a label, and splitting it
// that finely serves nothing.
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

// These are recorded on echo.Context; see finish, Recover and status.
const (
	errKey     = "xone/xecho.error"   // this request's error: returned by the handler, or recorded by Recover for an abort or a broken connection
	abortedKey = "xone/xecho.aborted" // the handler aborted with panic(http.ErrAbortHandler), recorded by Recover
	goneKey    = "xone/xecho.gone"    // the client is already gone and not one byte of response was sent, recorded by finish
)

// finish renders the error a handler returned into a response in this layer, and records it on c.
// ctx is the request's ctx as this layer saw it on the way in, used to tell whether the client is
// already gone (see web.ClientGone).
//
// Echo only calls e.HTTPErrorHandler to write the error as a response once the whole middleware
// chain has returned (Echo.ServeHTTP in echo v4.16.0). At the moment a middleware in the chain
// receives the error, not one byte of the response has been written: measured, 404, 405 and 500
// all read as Status 200, Committed false, Size 0. Taken at face value, every failing request
// would appear in the access log, the metrics and the trace as a 200 with bytes_out 0.
//
// So the error goes to c.Error here (which calls e.HTTPErrorHandler, including one replaced in
// WithRoutes) and nil is returned outwards: the outer layers read the status code and byte count
// actually sent, and HTTPErrorHandler runs exactly once — returning the error as well would make
// echo call it again, and a custom error handler that does not check Committed would write the
// error response twice.
//
// The error itself is recorded on c: the outer layers receive nil, and the access log's errors
// field and the span's echo.errors reach it through errorOf.
func finish(c echo.Context, ctx context.Context, err error) {
	// The client is gone and nothing was sent: record 499 (see web.StatusAborted). This has to be
	// decided before c.Error — writing an error response to a closed connection still sets
	// Committed and records Status 500 (measured on echo v4.16.0, including when the write fails).
	if !c.Response().Committed && web.ClientGone(ctx) {
		c.Set(goneKey, true)
	}
	if err == nil {
		return
	}
	c.Set(errKey, err)
	c.Error(err)
}

// errorOf is the error recorded for this request: returned by the handler (see finish), or
// recorded by Recover for an abort or a broken connection.
func errorOf(c echo.Context) error {
	err, _ := c.Get(errKey).(error)
	return err
}

// aborted reports whether the handler aborted with panic(http.ErrAbortHandler).
//
// It reads the mark Recover left rather than the error itself: when a handler returns (rather
// than panics with) an error wrapping http.ErrAbortHandler, the connection was never broken and
// the client received the 500 HTTPErrorHandler rendered, which is what should be recorded.
func aborted(c echo.Context) bool {
	v, _ := c.Get(abortedKey).(bool)
	return v
}

// Recover catches a panic and turns it into an error log line and an error response.
//
// handle decides what to answer after a panic: the error it returns goes to e.HTTPErrorHandler
// exactly as a handler's would. When it is nil, echo.ErrInternalServerError is returned, that is,
// echo's default 500 {"message":"Internal Server Error"}, the same shape a plain error returned
// by a handler produces.
//
// Echo itself does not catch panics by default: measured, the connection is simply broken (the
// client reads EOF) and net/http's logger writes the stack to stderr, bypassing slog.
// http.ErrAbortHandler is not caught and is re-panicked for net/http to break the connection.
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
				// http.ErrAbortHandler is the agreed way for a handler to ask for this connection
				// to be dropped (httputil.ReverseProxy does it when the upstream goes away).
				// net/http receives it, breaks the connection silently and prints no stack;
				// catching it here would turn a response that was meant to be aborted into a 500
				// going out, plus a stack trace that means nothing. So it is re-panicked.
				//
				// It is recorded before being re-panicked: the outer layers (access log, metrics,
				// tracing) use that to record it as an abort, see status — what they read from
				// Response().Status is usually the 200 already sent, which would record a
				// truncated response as a success.
				if r == http.ErrAbortHandler {
					c.Set(abortedKey, true)
					c.Set(errKey, http.ErrAbortHandler)
					panic(r)
				}

				// A broken connection is not a failure: no stack, and WARN rather than ERROR — a
				// client's flaky network should not fire the service's alerts.
				ctx := contextOf(c)
				if web.IsBrokenPipe(r) {
					slog.WarnContext(ctx, "connection broken", "error", r)
					c.Set(errKey, r.(error)) // web.IsBrokenPipe guarantees it is an *net.OpError
					err = nil
					return
				}

				// A panic value carries credentials just as an error returned by a handler does
				// (the whole DSN inside panic(err)), so it goes through RedactText before being
				// recorded. The stack does not need it: runtime.Stack prints function names, file
				// and line, and the raw hexadecimal words of the arguments (pointers, lengths) —
				// the contents of strings and structs are not in there.
				slog.ErrorContext(ctx, "panic while handling request",
					"error", web.RedactText(fmt.Sprint(r)),
					"stack", web.Stack(),
					"path", c.Request().URL.Path,
					"method", c.Request().Method)

				if c.Response().Committed {
					// The response has started going out; changing the status code now only
					// yields a truncated response.
					err = nil
					return
				}
				err = handle(c, r)
			}()
			return next(c)
		}
	}
}

// status is the status code that should be recorded for this request.
//
// Response().Status cannot be read directly: an aborted request has usually written a 200 header
// already, and a request whose client left may have been rendered into a 500 nobody will ever
// receive. Both are recorded as 499, see web.StatusAborted.
func status(c echo.Context) int {
	if gone, _ := c.Get(goneKey).(bool); gone || aborted(c) {
		return web.StatusAborted
	}
	return c.Response().Status
}
