// Package middleware holds xbeego's built-in middleware.
//
// Every one of them is plain net/http -- func(http.Handler) http.Handler, which is beego's own
// web.MiddleWare -- rather than a beego filter, because a filter runs inside beego's router: a
// request that never reaches the router would get no access log, no metric and no span. Mounted
// outside, they see every request net/http hands over.
//
// The onion, outermost first:
//
//	Capture -> LogScope -> Trace -> Log -> Metric -> Recover -> the caller's middleware -> beego
//
// Capture has to be outermost: it puts the recorder for the status code and the bytes written in
// front of the response, and the per-request state the others read. Recover has to be the
// innermost of the framework's middleware: a panic travels outwards, and whichever layer catches
// it, the code after next.ServeHTTP in every layer inside it never runs. Innermost, the outer
// layers still get to finish their work (record the metric, write the access log).
//
// The route template belongs to beego and a plain http.Handler cannot see it: xbeego registers a
// filter at web.BeforeExec that writes it into the request state, see xbeego.captureRoute. Used
// on their own, without xbeego, these middlewares record every request as unmatched.
//
// Each of them also works on its own, mounted with web.RunWithMiddleWares: whichever runs
// outermost installs the state the others share, so Capture is only needed to set which peers are
// trusted.
package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jupiter-eng/xone/internal/web"
	"github.com/jupiter-eng/xone/xbeego/internal/reqstate"
	"github.com/jupiter-eng/xone/xlog"
)

// Peers is who the peer sending a request is: whether it is one of ours, and which address counts
// as the client's. Both answers come from XBeego.TrustedProxies, and xbeego hands in the list it
// parsed.
type Peers = reqstate.Peers

// Capture installs the per-request state every other middleware in this package reads: the
// recorder in front of the ResponseWriter, and the room for the route template beego fills in
// later.
//
// peers decides whether the directly connected peer is one of ours -- which is what lets the
// tracing middleware accept forwarded headers and baggage -- and what client_ip reads. nil trusts
// nobody and records the peer's own address, the safe side on both counts.
//
// It is idempotent: finding a state already there, it changes nothing and passes the request on.
func Capture(peers Peers) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if reqstate.From(r.Context()) != nil {
				next.ServeHTTP(w, r)
				return
			}
			rec := reqstate.NewRecorder(w)
			ctx, _ := reqstate.New(r.Context(), r, peers, rec)
			next.ServeHTTP(rec, r.WithContext(ctx))
		})
	}
}

// ClientIP is the address the framework recorded as this request's client_ip, which is what the
// access log, the metrics and the span all go by.
//
// Business code should read this rather than beego's ctx.Input.IP(): the latter walks
// X-Forwarded-For from the left and believes whoever sent it, so a client reaching the service
// directly can make it say anything. In a beego controller the request to hand in is
// c.Ctx.Request.
//
// Without Capture in front of the request (the middleware used on its own, or a call from outside
// a request) it falls back to the directly connected peer, which cannot be forged.
func ClientIP(r *http.Request) string { return reqstate.From(r.Context()).ClientIP(r) }

// enter is what every middleware here starts with: the request state, installed on the spot when
// this layer happens to be the outermost one of the package. The writer and the request it returns
// are the ones to pass inwards.
func enter(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, *http.Request, *reqstate.State) {
	if s := reqstate.From(r.Context()); s != nil {
		return w, r, s
	}
	rec := reqstate.NewRecorder(w)
	ctx, s := reqstate.New(r.Context(), r, nil, rec)
	return rec, r.WithContext(ctx), s
}

// LogScope opens a log key-value scope for this request.
//
// With it installed, business code can call xlog.AddKV(ctx, ...) at any depth to add fields,
// without passing a new context back up layer by layer. In a beego controller the ctx to hand it
// is c.Ctx.Request.Context().
//
// It has to come before every other middleware: only after it is there a scope to write to.
func LogScope() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(xlog.CtxWithScope(r.Context())))
		})
	}
}

// routeOf resolves this request's route template, shared by the access log, the metrics and the
// span.
//
// It stays empty whenever beego never matched a route, or matched one and never reached the filter
// that copies the template over (xbeego's, at web.BeforeExec): an unmatched path, a method with no
// route of that name, a filter at BeforeRouter answering the request, or the middleware being used
// without xbeego. All of those are recorded as web.RouteUnmatched; the reasoning is there.
func routeOf(st *reqstate.State) string {
	if st != nil && st.Route != "" {
		return st.Route
	}
	return web.RouteUnmatched
}

// status is the status code that should be recorded for this request. ctx is the request's ctx as
// this layer saw it on the way in, used to tell whether the client is already gone (see
// web.ClientGone).
//
// The recorder's status cannot be taken as it is: an aborted request has usually written a 200
// header already, and a request whose client left and that returned without writing anything is a
// 200 nobody will ever receive. Both are recorded as 499, see web.StatusAborted.
func status(st *reqstate.State, ctx context.Context) int {
	if st == nil || st.Rec == nil {
		return 0
	}
	if st.Aborted || (!st.Rec.Started() && web.ClientGone(ctx)) {
		return web.StatusAborted
	}
	return st.Rec.Status()
}

// RecoverFunc answers the request after a panic. recovered is the value panic was called with.
//
// It is called only while the response has not started going out: once it has, changing the status
// code yields nothing but a truncated response.
type RecoverFunc func(w http.ResponseWriter, r *http.Request, recovered any)

// defaultRecover answers a bare 500, as gin's and echo's defaults do. No body: what a panic is
// about belongs in the log, not in a response going to whoever sent the request.
func defaultRecover(w http.ResponseWriter, _ *http.Request, _ any) {
	w.WriteHeader(http.StatusInternalServerError)
}

// Recover catches a panic and turns it into an error log line and a 500.
//
// It is the framework's backstop, not its only recovery: a panic inside a beego controller or
// filter is caught by beego's router first, where xbeego installs OnPanic in web.BConfig.RecoverFunc
// (XBeego.RecoverPanic) so the stack still points at the handler that panicked. What reaches this
// layer is what escaped beego -- a panic in middleware mounted with WithMiddleware, or in beego's
// own request setup -- which would otherwise reach net/http and kill the connection with no log at
// all.
//
// When handle is nil, a bare 500 is returned.
func Recover(handle RecoverFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w, r, st := enter(w, r)
			// Recorded on the request so that the recovery inside beego answers the same way, see
			// State.Recover: the slot it sits in is beego's one global.
			if st != nil && handle != nil {
				st.Recover = handle
			}
			defer func() {
				if rec := recover(); rec != nil {
					OnPanic(w, r, rec, handle)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// OnPanic records a recovered panic and answers the request: one log line, the panic noted on the
// request state for the access log's errors field, and the response handle writes.
//
// It is exported because it is called from two places that both have to behave identically: this
// package's Recover, and the slot xbeego installs in beego's own router (web.BConfig.RecoverFunc),
// where the stack still points at the handler that panicked rather than at the layer outside
// beego. Call it from a deferred function, with the value recover() returned.
//
// It re-panics http.ErrAbortHandler rather than answering it, so net/http goes on to break the
// connection the way the handler asked.
//
// A nil handle takes the one the request carries (what XBeego.WithRecoverFunc set), and falls back
// to a bare 500. That is how the one slot beego has for this serves every XBeego in the process
// with the response its own caller asked for.
func OnPanic(w http.ResponseWriter, r *http.Request, recovered any, handle RecoverFunc) {
	st, ctx := reqstate.From(r.Context()), r.Context()

	// http.ErrAbortHandler is the agreed way for a handler to ask for this connection to be
	// dropped (httputil.ReverseProxy does it when the upstream goes away). net/http receives it,
	// breaks the connection silently and prints no stack; answering it here would turn a response
	// that was meant to be aborted into a 500 going out, plus a stack trace that means nothing.
	//
	// It is marked on the state before being re-panicked: the outer layers (access log, metrics,
	// tracing) record it as an abort, see status -- what the recorder holds is usually the 200
	// already sent, which would record a truncated response as a success.
	if recovered == http.ErrAbortHandler {
		// Both recoveries see it: beego's, where the re-panic starts, and this package's backstop
		// outside beego, which catches it on its way to net/http. Recording it once keeps the
		// access log's errors field from saying it twice.
		if st != nil && !st.Aborted {
			st.Aborted = true
			st.Errors = append(st.Errors, "handler aborted")
		}
		panic(recovered)
	}

	// A broken connection is not a failure: no stack, and WARN rather than ERROR -- a client's
	// flaky network should not fire the service's alerts.
	if web.IsBrokenPipe(recovered) {
		slog.WarnContext(ctx, "connection broken", "error", recovered)
		note(st, recovered)
		return
	}

	// A panic value carries credentials just as an error returned by a handler does (the whole DSN
	// inside panic(err)), so it goes through RedactText before being recorded. The stack does not
	// need it: runtime.Stack prints function names, file and line, and the raw hexadecimal words of
	// the arguments (pointers, lengths) -- the contents of strings and structs are not in there.
	slog.ErrorContext(ctx, "panic while handling request",
		"error", web.RedactText(fmt.Sprint(recovered)),
		"stack", web.Stack(),
		"path", r.URL.Path,
		"method", r.Method)
	note(st, recovered)

	answer(w, r, recovered, handle)
}

// OnAbort answers a request a beego handler ended through beego's own control flow -- a panic
// carrying a registered error code rather than a failure. It writes the response and nothing else:
// no stack, no ERROR line, and no entry in the access log's errors field, because nothing failed.
//
// It is exported for the reason OnPanic is: xbeego calls it from the slot it installs in beego's
// router, which is the only place such a panic can be seen. Call it from a deferred function, with
// the value recover() returned.
func OnAbort(w http.ResponseWriter, r *http.Request, recovered any, handle RecoverFunc) {
	answer(w, r, recovered, handle)
}

// answer writes the response after a recovered panic: what handle says, what the request carries
// (XBeego.WithRecoverFunc, see State.Recover), or a bare 500.
//
// It writes nothing at all once the response has started going out, which is why both callers go
// through it rather than calling handle themselves.
func answer(w http.ResponseWriter, r *http.Request, recovered any, handle RecoverFunc) {
	st := reqstate.From(r.Context())
	if handle == nil && st != nil {
		handle = st.Recover
	}
	if handle == nil {
		handle = defaultRecover
	}

	if st != nil && st.Rec != nil && st.Rec.Started() {
		// The response has started going out; changing the status code now only yields a truncated
		// response.
		return
	}
	handle(w, r, recovered)
}

// note records the panic on the request state, which is where the access log takes its errors
// field from. Beego has no error list of its own the way gin's c.Errors is.
func note(st *reqstate.State, recovered any) {
	if st != nil {
		st.Errors = append(st.Errors, fmt.Sprint(recovered))
	}
}
