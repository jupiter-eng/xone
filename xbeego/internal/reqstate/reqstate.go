// Package reqstate is the contract between xbeego and its built-in middleware: the per-request
// values beego does not hand to a plain http.Handler.
//
// xgin and xecho take the route template, the status code and the bytes written straight out of
// *gin.Context / echo.Context. xbeego's middleware is plain net/http (func(http.Handler)
// http.Handler) mounted outside beego's router, where none of the three is available:
//
//   - the route template is known only inside beego, after routing
//     (ctx.Input.GetData("RouterPattern")), so xbeego registers a filter at web.BeforeExec that
//     writes it into the State on the request ctx;
//   - the status code and the bytes written are nobody's business in net/http, so Recorder wraps
//     the ResponseWriter and counts them.
//
// It lives under internal because it is strictly between two packages of the framework: callers
// neither need it nor should reach for it.
package reqstate

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"

	"github.com/jupiter-eng/xone/internal/web"
)

// Peers is what the layer accepting a request has to know about the one that sent it: whether it
// is one of ours, and which address counts as the client's. internal/web.Proxies implements it
// from XBeego.TrustedProxies, which is the only place that is stated.
//
// It is an interface rather than internal/web.Proxies itself because xbeego/middleware is a
// caller-facing package: a name from internal/ in its signatures could not be spelled by anyone
// outside this repository.
type Peers interface {
	// Trusts reports whether the directly connected peer, an address without a port, is one of
	// ours.
	Trusts(ip string) bool

	// ClientIP is the address recorded as client_ip, walking the forwarded headers only when the
	// peer is trusted.
	ClientIP(r *http.Request) string
}

// State is what one request carries for the built-in middleware.
//
// Route is written by xbeego's BeforeExec filter and read once beego's handler has returned,
// both on the request's own goroutine, so the write happens before the read and no lock is
// needed. A handler that hands the ctx to a goroutine outliving the request must not read Route
// there.
type State struct {
	// Route is the route template beego matched. It stays empty when the filter never ran --
	// nothing matched, or a filter at BeforeRouter ended the request first -- and the middleware
	// records web.RouteUnmatched for it.
	Route string

	// Trusted says the directly connected peer is in XBeego.TrustedProxies. Only such a peer's
	// forwarded headers and baggage are accepted, see xbeego.markTrustedPeer.
	Trusted bool

	// Aborted says the handler asked for the connection to be dropped with
	// panic(http.ErrAbortHandler). The status code recorded for such a request is
	// web.StatusAborted rather than whatever header had already gone out.
	Aborted bool

	// Errors are what the framework recorded about this request, the panic text above all. Beego
	// has no error list of its own the way gin's c.Errors is, so the recovery middleware collects
	// them here and the access log writes them, masked, into its errors field.
	Errors []string

	// Peers is how client_ip is computed, nil when nobody said (the peer itself is then used).
	Peers Peers

	// Recover answers this request after a panic, as XBeego.WithRecoverFunc set it. It travels on
	// the request rather than being closed over by the recovery itself: beego has one global slot
	// for it (web.BConfig.RecoverFunc), so a handler captured there would be whichever XBeego
	// assembled last, and two instances in one process could not differ.
	Recover func(w http.ResponseWriter, r *http.Request, recovered any)

	// Rec counts the status code and the bytes written, see Recorder.
	Rec *Recorder
}

type key struct{}

// New puts a State on ctx and returns both. The State is mutable and the ctx holds the pointer:
// the filter writing Route later is seen by whoever reads it afterwards.
func New(ctx context.Context, r *http.Request, peers Peers, rec *Recorder) (context.Context, *State) {
	s := &State{Peers: peers, Rec: rec}
	if peers != nil {
		// Decided here, once, on the way in: what has to be judged is the directly connected peer,
		// not the client IP computed from the forgeable forwarded headers.
		s.Trusted = peers.Trusts(web.RemoteIP(r))
	}
	return context.WithValue(ctx, key{}, s), s
}

// ClientIP is the address recorded as client_ip for this request: what Peers says, or the directly
// connected peer when nobody configured any.
func (s *State) ClientIP(r *http.Request) string {
	if s != nil && s.Peers != nil {
		return s.Peers.ClientIP(r)
	}
	return web.RemoteIP(r)
}

// From is the State on ctx, nil when there is none (middleware used on its own, outside xbeego).
func From(ctx context.Context) *State {
	s, _ := ctx.Value(key{}).(*State)
	return s
}

// Recorder wraps an http.ResponseWriter and records what went out: the status code, how many bytes
// of body, and -- while capturing -- the first few KB of it.
//
// Gin and echo both hand their integration a writer that already does this; net/http does not, so
// a beego service has to have one put in front of it. It must be installed outermost, before
// anything that writes.
type Recorder struct {
	http.ResponseWriter

	status  int
	bytes   int
	started bool

	buf []byte // the captured response body, capped at max
	max int    // 0 means nothing is captured
}

// NewRecorder wraps w.
func NewRecorder(w http.ResponseWriter) *Recorder { return &Recorder{ResponseWriter: w} }

// Capture starts keeping the response body, max bytes of it at most. It has to be called before
// the first write.
func (r *Recorder) Capture(max int) { r.max = max }

// Body is the captured response body, empty unless Capture was called.
func (r *Recorder) Body() []byte { return r.buf }

// Status is the status code that went out, 200 when the handler never set one -- which is what
// net/http sends in that case.
func (r *Recorder) Status() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

// Bytes is how many bytes of response body were written, headers excluded.
func (r *Recorder) Bytes() int { return r.bytes }

// Started reports whether the response has begun going out, so that a panic handler knows whether
// the status code can still be changed.
func (r *Recorder) Started() bool { return r.started }

func (r *Recorder) WriteHeader(code int) {
	if r.started {
		// net/http logs a "superfluous WriteHeader" line for the second call and ignores it; beego's
		// own context.Response guards the same way. Recording the second code would have the access
		// log disagree with what the client received.
		return
	}
	r.status, r.started = code, true
	r.ResponseWriter.WriteHeader(code)
}

func (r *Recorder) Write(p []byte) (int, error) {
	// A Write with no WriteHeader before it makes net/http send a 200; record that rather than
	// leaving the status at 0.
	if !r.started {
		r.status, r.started = http.StatusOK, true
	}
	if n := r.max - len(r.buf); n > 0 {
		r.buf = append(r.buf, p[:min(len(p), n)]...)
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += n
	return n, err
}

// Unwrap hands the wrapped writer to http.ResponseController, so a handler calling
// SetReadDeadline / Flush through it reaches the real writer.
func (r *Recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// The four optional interfaces below are forwarded explicitly rather than left to a type
// assertion on the wrapper: beego's context.Response asks the writer it was handed for each of
// them (Hijack / Flush / CloseNotify / Pusher in context.go), and a wrapper not answering would
// break WebSocket upgrades, SSE and HTTP/2 push for every handler. Each falls back the way
// beego's own does when the writer underneath cannot do it.

func (r *Recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *Recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	// A hijacked connection is written to directly: the status line and the bytes go straight onto
	// it and neither can be recorded any more, so the access log reports the 200 Status falls back
	// to. What marking it started is for is the other side of that -- OnPanic and anything else
	// asking "has the response begun" must not try to write a header onto a connection that is no
	// longer the server's.
	r.started = true
	return hj.Hijack()
}

//nolint:staticcheck // SA1019: beego's context.Response exposes CloseNotify, so the writer under it has to answer
func (r *Recorder) CloseNotify() <-chan bool {
	if cn, ok := r.ResponseWriter.(http.CloseNotifier); ok {
		return cn.CloseNotify()
	}
	return nil
}

func (r *Recorder) Push(target string, opts *http.PushOptions) error {
	if p, ok := r.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

// ReadFrom keeps net/http's sendfile path reachable: without it, io.Copy into the writer (which is
// what http.ServeContent and http.ServeFile do) falls back to a 32KB buffer loop.
func (r *Recorder) ReadFrom(src io.Reader) (int64, error) {
	rf, ok := r.ResponseWriter.(io.ReaderFrom)
	// While the body is still being captured, the bytes have to travel through Write: ReadFrom
	// hands them straight to the writer underneath, which counts them but never sees them, and the
	// access log's response_body would be empty while bytes_out was right. Capturing is off for
	// every request the access log does not record a body for, so the fast path below is the
	// ordinary one.
	if !ok || r.max-len(r.buf) > 0 {
		// io.Copy would find ReadFrom on the Recorder and call it again; copying through a plain
		// Write avoids that and still counts the bytes.
		return io.Copy(writeOnly{r}, src)
	}
	if !r.started {
		r.status, r.started = http.StatusOK, true
	}
	n, err := rf.ReadFrom(src)
	r.bytes += int(n)
	return n, err
}

// writeOnly hides every method but Write, so io.Copy cannot find a ReadFrom to recurse into.
type writeOnly struct{ w io.Writer }

func (w writeOnly) Write(p []byte) (int, error) { return w.w.Write(p) }
