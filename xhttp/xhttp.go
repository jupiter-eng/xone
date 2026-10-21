package xhttp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/go-resty/resty/v2"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xhook"
	"github.com/jupiter-eng/xone/xmetric"
	"github.com/jupiter-eng/xone/xtrace"
)

// fallbackTimeout is the timeout of the fallback client, used before initialization and after shutdown.
//
// A zero timeout means "never time out", not "there is some default": with an unresponsive peer the request hangs
// forever. The worst of it is during shutdown -- one component sending such a request while closing would leave the
// whole process stuck there.
const fallbackTimeout = 30 * time.Second

// New builds an HTTP client from the configuration and does not touch this package's global instance.
//
// One exception: with cfg.Metric on (which is the default), the duration histogram has to be registered on xmetric's
// global Registry -- there is only one set of metrics anyway, and registering elsewhere means they are never exported.
// To avoid that, turn cfg.Metric off. A failed registration (a metric of that name already registered as another
// type, say) does not fail New; one error log is recorded and that group of metrics is not exported.
//
// The io.Closer returned releases the idle connections in the pool.
func New(cfg Config) (*resty.Client, io.Closer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, nil, xerror.Newf("xhttp", "config", "invalid config: %w", err)
	}

	// The connection pool layer is held directly rather than hoping http.Client.CloseIdleConnections finds it.
	//
	// That method searches downwards by type assertion: with tracing on there is an otelhttp.Transport in between,
	// which does not implement the method, so the assertion stops there -- the whole call becomes a no-op,
	// and tracing is on by default. Held directly, it is simply closed when closing.
	tlsCfg, err := cfg.TLS.Build()
	if err != nil {
		return nil, nil, xerror.Newf("xhttp", "config", "invalid TLS config: %w", err)
	}
	pool := tunedTransport(cfg, tlsCfg)
	if _, ok := pool.(*http.Transport); tlsCfg != nil && !ok {
		// Rather than pretend it was configured: requests would still go out, on the system roots and without a client certificate.
		return nil, nil, xerror.Newf("xhttp", "config", "http.DefaultTransport has been replaced by %T, the TLS block cannot be applied", pool)
	}
	client := newResty(&http.Client{
		Transport: traced(cfg, pool),
		Timeout:   cfg.Timeout,
	}, cfg.Log)

	if cfg.RetryCount > 0 {
		p := retryPolicy{onlyIdempotent: cfg.RetryOnlyIdempotent}
		client.SetRetryCount(cfg.RetryCount).
			SetRetryWaitTime(cfg.RetryWaitTime).
			SetRetryMaxWaitTime(cfg.RetryMaxWaitTime).
			AddRetryCondition(p.condition).
			SetRetryAfter(p.veto)
	}

	if cfg.Metric {
		// Use RegisterAs's return value: on a duplicate registration it hands back the existing instance,
		// and recording into the newly built one would never be exported.
		//
		// A failed registration is only logged and the client is handed back as usual, as in xgin / xgorm / xredis:
		// metrics not being exported is an observability problem and must not keep every outbound call from working.
		// On an error RegisterAs hands back the newly built one, which records as usual and simply is not exported.
		hist, err := xmetric.RegisterAs(newDurationHistogram())
		if err != nil {
			slog.Error("xhttp failed to register the request duration metric, values recorded through it will not be exported", "error", err)
		}
		installMetrics(client, hist)
	}
	if cfg.Log {
		installLog(client, cfg.SlowThreshold)
	}

	return client, &clientCloser{pool: pool}, nil
}

// newResty builds a resty from the given http.Client and routes resty's own logs into slog.
//
// NewWithClient is always used rather than resty.New(): the latter brings a cookie jar, shared by every request this
// client sends -- a session cookie set by service A would be carried into every unrelated call afterwards.
// A configured instance never had a jar, while the fallback instance used to be a resty.New(), so the two behaved
// differently (measured: cookies did bleed across).
//
// quiet is Config.Log: with the request log on, resty's two kinds of retry-path logs are replaced by that one line (see restyLogger).
// It is set on the client's logger rather than per request: a SetLogger of your own replaces the whole thing and follows resty's rules.
func newResty(hc *http.Client, quiet bool) *resty.Client {
	return resty.NewWithClient(hc).SetLogger(restyLogger{quiet: quiet})
}

// traced wraps the tracing layers around the connection pool.
//
//	Trace on:  client → xtrace.Transport → otelhttp.Transport → scrubURL → the tuned http.Transport
//	Trace off: client → xtrace.Transport → propagateOnly → the tuned http.Transport
//
// xtrace.Transport writes the target host into the ctx, which is what lets the per-domain header forwarding rules match.
// otelhttp calls the global Propagator to inject whether or not the trace is sampled (pinned down by this package's tests).
//
// Trace only governs "is an outbound Span opened". Whether traceparent, baggage and the forwarded headers travel
// downstream is a different question: turning Trace off used to remove xtrace.Transport with it, and a forwarded
// header such as X-Request-Id and the upstream trace identifiers died quietly at this hop. So with it off, a layer that only injects and opens no Span takes its place.
func traced(cfg Config, pool http.RoundTripper) http.RoundTripper {
	next := http.RoundTripper(propagateOnly{next: pool})
	if cfg.Trace {
		next = otelhttp.NewTransport(scrubURL{next: pool}, otelhttp.WithSpanNameFormatter(spanName))
	}
	return &xtrace.Transport{Next: next}
}

// propagateOnly writes the trace identifiers and forwarded headers from the ctx into the request headers through the
// global Propagator, without opening a Span. With Trace off it does the injecting half that otelhttp would have done.
type propagateOnly struct{ next http.RoundTripper }

// RoundTrip does not modify its argument, by the RoundTripper contract: the injection is written on a clone.
func (p propagateOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	otel.GetTextMapPropagator().Inject(r.Context(), propagation.HeaderCarrier(r.Header))
	return p.next.RoundTrip(r)
}

// CloseIdleConnections passes through to the connection pool; the reasoning is on xtrace.Transport's method of the same name.
func (p propagateOnly) CloseIdleConnections() {
	if c, ok := p.next.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// scrubURL replaces the url.full otelhttp writes onto the Span with a version carrying no query string.
//
// otelhttp (v0.71) only removes user:password from the URL and writes the query string into url.full as it is --
// and a query string often holds a token or a signature, which would reach the tracing backend with the Span.
// otelhttp has no option for rewriting the attribute; it calls SetAttributes after opening the Span and before
// calling the next layer, so writing the same key again in the layer below it overwrites it (the SDK takes the later write for a duplicate attribute).
// The path is kept: it does not go into the Span name, it is only an attribute, and an investigation needs it.
//
// A Span that is not recorded (not sampled, tracing off) discards every attribute, so there is no need to assemble this URL at all.
type scrubURL struct{ next http.RoundTripper }

func (s scrubURL) RoundTrip(r *http.Request) (*http.Response, error) {
	if span := trace.SpanFromContext(r.Context()); span.IsRecording() {
		span.SetAttributes(attribute.String("url.full", bareURL(r.URL)))
	}
	return s.next.RoundTrip(r)
}

// tunedTransport is cloned from DefaultTransport and then adjusted.
//
// Whatever is not adjusted is an accepted standard library default, measured one by one (Go 1.25):
//
//   - Proxy: ProxyFromEnvironment. With HTTP_PROXY / HTTPS_PROXY set, every outbound request goes through the proxy
//     (measured: an http:// request is handed to the proxy with its query string as it is), loopback excepted, and NO_PROXY can exclude more.
//     The environment variables are read once on first use in the process and cached: changing them later (even unsetting) has no effect.
//   - TLSHandshakeTimeout: 10s. With the peer accepting the TCP connection and never handshaking, measured at 10.0s with TLS handshake timeout.
//   - ResponseHeaderTimeout: 0, so the transport does not bound the wait for response headers and Timeout governs the whole attempt;
//     with Timeout at 0 as well, a peer that accepts the request and never answers leaves it hanging forever.
//   - MaxConnsPerHost: 0, unlimited; configurable, see Config.MaxConnsPerHost.
//   - ForceAttemptHTTP2: true, so an https downstream negotiating HTTP/2 multiplexes over one connection.
//   - Redirects: resty v2.17 sets no CheckRedirect, so the standard library's default applies, following at most 10 and reporting
//     stopped after 10 redirects on the 11th. Across hosts the standard library only drops Authorization and Cookie,
//     while a custom credential header (measured with X-Api-Key) is still carried to the new host; a 302 turns a POST into a GET with no body,
//     and a 307 keeps the method and the body. To change that, SetRedirectPolicy on the native client.
//
// When tlsCfg is non-nil it replaces TLSClientConfig (the TLS block from the configuration). HTTP/2 is unaffected:
// ForceAttemptHTTP2 makes the standard library add h2 on a custom TLSClientConfig too (measured: HTTP/2.0 negotiated).
func tunedTransport(cfg Config, tlsCfg *tls.Config) http.RoundTripper {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		slog.Warn("xhttp cannot tune the connection pool", "default_transport_type", fmt.Sprintf("%T", http.DefaultTransport))
		return http.DefaultTransport
	}

	t = t.Clone()
	t.MaxIdleConns = cfg.MaxIdleConns
	t.MaxIdleConnsPerHost = cfg.MaxIdleConnsPerHost
	t.MaxConnsPerHost = cfg.MaxConnsPerHost
	t.IdleConnTimeout = cfg.IdleConnTimeout
	t.DialContext = (&net.Dialer{Timeout: cfg.DialTimeout, KeepAlive: cfg.DialKeepAlive}).DialContext
	if tlsCfg != nil {
		t.TLSClientConfig = tlsCfg
	}
	return t
}

// spanName names an outbound Span by the method alone.
//
// The inbound side uses the route template (GET /users/:id), and the outbound side has no template to use --
// the real paths /users/42 and /users/43 would each be a Span name, the cardinality would grow with the number of
// users, and the tracing backend's index by name would burst. OTel's semantic conventions also use the method alone
// when there is no template. To see what was actually called, look at the url.full and server.address attributes.
func spanName(_ string, r *http.Request) string {
	return r.Method
}

// restyLogger routes resty's own logs into slog.
//
// resty's default logger grabs os.Stderr the moment the client is built and writes past slog.
// Measured (resty v2.17, RetryCount=2, nothing listening on the target port): three WARN lines and one ERROR:
//
//	WARN RESTY Get "http://host/x?token=…": dial tcp …: connection refused, Attempt 1
//	…Attempt 2, Attempt 3…
//	ERROR RESTY Get "http://host/x?token=…": dial tcp …: connection refused
//
// With RetryCount=0 this path logs nothing. The other kind is an ERROR when a configuration call is ignored:
// with tracing on the transport is not an *http.Transport, so SetTLSClientConfig only logs one error and
// does nothing -- which is the only signal the caller gets, so its level is kept rather than lowered.
// Not going into slog means not going into the logging platform, in a format that does not match the other logs; and a token in the query string would land on disk as it is.
// Routed into slog here, the URLs in the message have their query string, fragment and userinfo removed (an error among the arguments is handled by structure, see scrubError).
//
// Debug mode (SetDebug(true)) is the exception: resty assembles the whole request (the request line with its query string, every header)
// into one piece of text for Debugf, where the path in the request line carries no http:// and is not quoted, so it is not recognized here and reaches the log as it is.
//
// With quiet (Config.Log on) the two retry-path kinds are not printed; they are exactly the error on the request log line.
// In resty v2.17.2, each failed attempt is a Warnf("%v, Attempt %v"), recognized by format string;
// the one when retries are used up is an Errorf("%v") inside (*Request).Execute, while an ignored configuration call
// (SetTLSClientConfig meeting a transport that is not an *http.Transport, say) uses the format string "%v" too -- and that is the only signal the caller gets,
// so it must not be swallowed with it. Hence it is recognized by caller: only the line Execute logs is dropped.
type restyLogger struct{ quiet bool }

func (l restyLogger) Errorf(format string, v ...any) {
	if l.quiet && calledFromExecute() {
		return
	}
	logResty(slog.LevelError, format, v)
}

func (l restyLogger) Warnf(format string, v ...any) {
	if l.quiet && format == "%v, Attempt %v" {
		return
	}
	logResty(slog.LevelWarn, format, v)
}

func (restyLogger) Debugf(format string, v ...any) { logResty(slog.LevelDebug, format, v) }

func logResty(level slog.Level, format string, v []any) {
	args := make([]any, len(v))
	for i, a := range v {
		if err, ok := a.(error); ok {
			a = scrubError(err)
		}
		args[i] = a
	}
	slog.Log(context.Background(), level, "xhttp resty log", "detail", scrubText(fmt.Sprintf(format, args...)))
}

// restyExecute is resty's send-and-retry method (v2.17.2). The Errorf for used-up retries is inside it.
const restyExecute = "github.com/go-resty/resty/v2.(*Request).Execute"

// calledFromExecute reports whether restyLogger was called from restyExecute. It skips this package's own frames and looks at the first one outside.
func calledFromExecute() bool {
	pcs := make([]uintptr, 8)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		f, more := frames.Next()
		if !strings.HasPrefix(f.Function, "github.com/jupiter-eng/xone/xhttp.") {
			return f.Function == restyExecute
		}
		if !more {
			return false
		}
	}
}

// idempotentMethods are the methods that can safely be retried (RFC 9110's idempotent methods).
var idempotentMethods = map[string]struct{}{
	http.MethodGet: {}, http.MethodHead: {}, http.MethodOptions: {},
	http.MethodTrace: {}, http.MethodPut: {}, http.MethodDelete: {},
}

// retryPolicy decides which requests may be retried. Two gates:
//
//   - condition: attached as a retry condition. Only a transport-layer error is retried (the reasoning is below), only
//     when the body can be sent a second time, and with onlyIdempotent also only for an idempotent method.
//   - veto: attached as RetryAfter, the last gate before a retry. resty's conditions are an "or" (v2.17.2 retry.go Backoff:
//     one returning true means the rest are not consulted), so a condition the caller added with AddRetryCondition would override condition's false;
//     RetryAfter is called after the decision to retry and before the wait, and returning an error stops the retry.
//
// The error veto returns only reaches the caller when "this attempt had no error" (Backoff: if err == nil { err = err2 });
// with an error, the caller still gets the original one. So it vetoes in two cases only:
//
//   - no response arrived (RawResponse is nil, necessarily a transport-layer error) and the method is not idempotent: exactly what
//     RetryOnlyIdempotent guards against, "cannot tell whether the request arrived", and the caller gets the original transport error;
//   - the body is an io.Reader already read: resending it would send an empty body, and a 200 from the server would be a silent loss of data,
//     so it is vetoed even when a response did arrive (the caller then gets errBodyNotRewindable and that response).
//
// When a response did arrive, the method is not idempotent and the body can be sent again, vetoing would cost replacing a 503 with an error we invented,
// so it does not: a caller's condition retrying on status codes keeps working and checks the method itself. A caller's SetRetryAfter replaces veto.
type retryPolicy struct{ onlyIdempotent bool }

// condition: attaching any retry condition voids resty's own judgment entirely and only the conditions are consulted
// (resty v2.17.2 retry.go Backoff). The ones it would not retry itself -- a failing middleware before the request,
// a parse failure after the response was fully received -- arrive at the condition with the "do not retry" mark
// already stripped, looking just like a transport-layer error. This used to return true for any idempotent method, and measured,
// a SetResult meeting a 200 with broken JSON and RetryCount=3 sent the same request 4 times,
// while resty with no condition attached sends it once. So "is this a transport-layer error" is recognized here first.
func (p retryPolicy) condition(resp *resty.Response, err error) bool {
	if !isTransportError(err) {
		return false // a response arrived, or the error came after one did: not retried, matching resty's default condition
	}
	return p.refusal(resp) == nil
}

// veto: see retryPolicy. Returning 0 leaves the wait to resty's backoff.
func (p retryPolicy) veto(_ *resty.Client, resp *resty.Response) (time.Duration, error) {
	err := p.refusal(resp)
	if err == nil || errors.Is(err, errNotIdempotent) && resp != nil && resp.RawResponse != nil {
		return 0, nil
	}
	return 0, err
}

var (
	// errNotIdempotent never reaches the caller: it only vetoes when no response arrived, and the caller then gets the original transport error.
	// It is not an xerror: it only travels inside resty's retry decision.
	errNotIdempotent = errors.New("not retrying a non-idempotent method")

	errBodyNotRewindable = xerror.Newf("xhttp", "execute",
		"not retrying: the request body is an io.Reader that the first attempt already consumed "+
			"(pass []byte or string to make the request retryable)")
)

// refusal is why this request cannot be retried, or nil when it can.
func (p retryPolicy) refusal(resp *resty.Response) error {
	if resp == nil || resp.Request == nil {
		return errNotIdempotent // an unrecognized method is conservatively not retried
	}
	if consumedReader(resp.Request) {
		return errBodyNotRewindable
	}
	method := strings.ToUpper(resp.Request.Method)
	if _, ok := idempotentMethods[method]; ok || !p.onlyIdempotent {
		return nil
	}
	slog.Debug("xhttp skipped retrying a non-idempotent method",
		"method", method, "to_allow_set", ConfigKey+".RetryOnlyIdempotent=false")
	return errNotIdempotent
}

// consumedReader reports whether the body is an io.Reader that is gone once read.
//
// resty v2.17.2 rebuilds the http.Request from Request.Body on every attempt (middleware.go createHTTPRequest):
// a []byte, a string and a struct are re-serialized each time and are complete; an io.Reader was read to the end on the first attempt,
// and measured, a PUT strings.NewReader(…) whose first connection was cut sent 0 bytes the second time and the server answered 200.
// resty does not rewind it (RetryResetReaders only covers multipart). The exception is SetContentLength(true):
// resty reads the io.Reader into a buffer and sets Body to nil, sending that buffer every time afterwards (measured complete).
// http.NoBody is what resty fills in for a PUT / POST with no body; it is empty and sending it again changes nothing.
func consumedReader(r *resty.Request) bool {
	_, ok := r.Body.(io.Reader)
	return ok && r.Body != http.NoBody
}

// isTransportError reports whether err is the kind of transport-layer error resty retries by default.
//
// An error while connecting, sending or waiting for the response is always wrapped by http.Client.Do into a *url.Error,
// which implements net.Error; a connection reset or a timeout while reading the response body is a net.Error too.
// A connection dropped halfway through the response body is io.ErrUnexpectedEOF, which resty likewise retries
// (measured: a Content-Length of 100 with only 10 bytes sent before the drop was sent 4 times with no condition attached).
// A JSON parse failure and an error returned by middleware are not among them.
func isTransportError(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, io.ErrUnexpectedEOF)
}

// clientCloser holds the connection pool itself rather than the http.Client around it.
type clientCloser struct{ pool http.RoundTripper }

func (c *clientCloser) Close() error {
	if p, ok := c.pool.(interface{ CloseIdleConnections() }); ok {
		p.CloseIdleConnections()
	}
	return nil
}

// ---- global instance ----

// fallback is the client used before initialization and after shutdown, with a timeout. It is built once and never changes.
// Like a configured instance it carries no cookie jar; the reasoning is in newResty.
var fallback = newResty(&http.Client{Timeout: fallbackTimeout}, false)

var (
	mu      sync.RWMutex
	current *resty.Client // nil means not up yet, or already closed
)

// C fetches the resty client.
//
// Unlike xgorm / xredis it does not panic when there is none: an HTTP client connects to no external resource and
// works unconfigured, so this always returns a usable instance.
// Before initialization and after shutdown what comes back is the fallback instance with its 30s timeout -- a
// component may still send a request while shutting down, and failing with a timeout beats getting a closed client.
func C() *resty.Client {
	mu.RLock()
	defer mu.RUnlock()
	if current != nil {
		return current
	}
	return fallback
}

// R opens a request bound to a ctx, which is what carries the trace and the timeout downstream.
//
//	resp, err := xhttp.R(ctx).SetResult(&out).Get(url)
func R(ctx context.Context) *resty.Request { return C().R().SetContext(ctx) }

// RawClient fetches the underlying native *http.Client.
//
// It is for cases that handle the response body themselves, a streaming request such as SSE for example --
// resty reads the whole response into memory, which is wrong for a streaming endpoint.
//
// Before initialization it returns the one inside the fallback instance rather than http.DefaultClient:
// the latter has a timeout of 0 and a request can hang forever.
//
// It is taken from C() rather than stored separately: two pieces of related state would have to be kept in step,
// while resty's GetClient returns the very one it has been using, consistent in value and in lifetime.
func RawClient() *http.Client { return C().GetClient() }

// ---- registration ----

func init() {
	xhook.BeforeStart(initXHttp, xhook.At(xhook.StageClient))
	xhook.BeforeStop(closeXHttp) // the stage follows the start hook above
}

// liveCloser is filled in by initXHttp and used by closeXHttp to clean up.
var liveCloser io.Closer

func initXHttp(context.Context) error {
	c := DefaultConfig()
	if err := xconfig.Unmarshal(ConfigKey, &c); err != nil {
		return err
	}
	return install(c)
}

// install builds the client from the configuration and makes it the global default instance.
func install(c Config) error {
	client, cl, err := New(c)
	if err != nil {
		return err
	}

	mu.Lock()
	current, liveCloser = client, cl
	mu.Unlock()

	slog.Info("xhttp ready", "timeout", c.Timeout, "max_idle_conns_per_host", c.MaxIdleConnsPerHost, "retries", c.RetryCount)
	return nil
}

func closeXHttp(context.Context) error {
	mu.Lock()
	cl := liveCloser
	current, liveCloser = nil, nil // detached before closing, so C() falls back right away
	mu.Unlock()

	if cl == nil {
		return nil
	}
	return cl.Close()
}
