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

// A few conventions about values that the access log, the metrics and the Spans share. Each
// framework takes the raw values out (the route template, the method, the status code) and they are
// bounded by the rules here before being recorded: the same service on another framework must not
// change the label values on the dashboards and the alerts.

// RouteUnmatched is what the access log's route, the metrics' route label and the Span name hold
// when no route matched.
//
// The route template rather than the real path: /user/123 and /user/456 are the same endpoint, and
// recording the real path would grow the cardinality of the metric labels and the Span names
// without bound with the ids in the URLs, which breaks Prometheus.
// Filling in the real path when nothing matched blows the cardinality up just the same and leaves
// the logs unable to tell whether /nope is a route or a 404.
// The real path is in the access log's path
const RouteUnmatched = "unmatched"

// StatusAborted is the status code a request that did not end normally is recorded with in the
// access log, the metrics and the trace. There are two cases:
//
//   - a handler aborting with panic(http.ErrAbortHandler): net/http breaks the connection outright.
//     An aborted request has usually written a 200 response header already, and reading the status
//     code the framework recorded would log a truncated response as a success in all three places;
//   - the client has gone (see ClientGone) with not one byte of the response sent: a handler
//     returning ctx.Err() as usual renders as a 500, and returning without writing anything records
//     a 200 -- while the client received nothing at all.
//
// It is borrowed from nginx's 499 (client closed request): the code is never really sent to a
// client and only gives "a request that did not end normally" a fixed value that can be searched
// for and does not blend into any real response.
// A response that has begun being sent records the status code that went out: the client received
// at least part of it
const StatusAborted = 499

// ClientGone reports whether the request's ctx was cancelled by net/http: the client broke the
// connection (under HTTP/2 the stream was reset), or the connections were force-closed at the
// deadline during shutdown.
//
// The ctx has to be the one the middleware was entered with: a ctx swapped in by a layer inside may
// be the business's own -- echo's ContextTimeout defers a cancel() of it on return, so outside that
// layer it is Canceled while the client is still waiting for a response.
// Only Canceled and not DeadlineExceeded, for the same reason: a deadline was set by the business
// and net/http sets none
func ClientGone(ctx context.Context) bool { return errors.Is(ctx.Err(), context.Canceled) }

// knownMethods are the methods RFC 9110 defines, plus PATCH
var knownMethods = map[string]struct{}{
	http.MethodGet: {}, http.MethodHead: {}, http.MethodPost: {}, http.MethodPut: {},
	http.MethodPatch: {}, http.MethodDelete: {}, http.MethodConnect: {},
	http.MethodOptions: {}, http.MethodTrace: {},
}

// MethodOther is what every method that is not recognized is recorded as
const MethodOther = "OTHER"

// NormalizeMethod bounds the method to a fixed set, which the metrics' method label and the Span
// names use.
//
// The route already keeps the ids in the URLs out through its template, while the method dimension
// is copied from the request -- and an HTTP method is a free token anybody may send as CUSTOM1 or
// CUSTOM2. Every new value is another set of time series, with nothing to evict them: the metrics'
// memory, the scrape responses and the monitoring storage all grow together.
// It is recorded even when the request ends in a 404 / 405.
func NormalizeMethod(m string) string {
	if _, ok := knownMethods[m]; ok {
		return m
	}
	return MethodOther
}

// IsBrokenPipe reports whether a panicked value is the client having broken the connection early
// (broken pipe / connection reset).
// A broken connection is not a failure, and the Recover middleware prints no stack because of it
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

// maxStack is the upper bound on a panic's stack
const maxStack = 16 * 1024

// Stack is the current goroutine's stack, 16KB at most.
//
// runtime.Stack rather than reading the source files frame by frame: doing file I/O while
// recovering from a panic holds this very error log up as soon as the disk is slow.
func Stack() string {
	buf := make([]byte, maxStack)
	return string(buf[:runtime.Stack(buf, false)])
}
