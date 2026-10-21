// Package xhttp sets up the outbound HTTP client from the configuration; the caller gets a native *resty.Client.
//
//	resp, err := xhttp.R(ctx).SetResult(&out).Get(url)
//
// Tracing, metrics, the connection pool and the retry policy are all decided by the configuration, with no initialization in business code.
package xhttp

import (
	"fmt"
	"time"

	"github.com/jupiter-eng/xone/xtls"
)

// ConfigKey is this module's top-level key in the config file.
const ConfigKey = "XHttp"

// Config is the outbound HTTP client configuration.
//
// Unlike xgorm / xredis there are no several instances here: an HTTP client connects to no external resource, works
// unconfigured, and one shared connection pool and timeout is enough. A scenario needing a second set of parameters
// builds one with New directly.
type Config struct {
	// Timeout is the timeout of one attempt. It defaults to 60s.
	//
	// 0 means "never time out", not "use some default": with an unresponsive peer the request hangs forever.
	//
	// Note that it governs one attempt, not one logical request: with RetryCount on, the worst case is
	// (RetryCount+1) x Timeout plus the backoff waits (measured at a full 1.24s with Timeout=300ms and
	// RetryCount=3).
	// To cap the whole logical request, use the caller's ctx -- both the retry backoff and each attempt honor it:
	//
	//	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	//	defer cancel()
	//	resp, err := xhttp.C().R().SetContext(ctx).Get(url)
	Timeout time.Duration `yaml:"Timeout"`

	// DialTimeout is the timeout for establishing a TCP connection. It defaults to 30s.
	DialTimeout time.Duration `yaml:"DialTimeout"`

	// DialKeepAlive is the TCP keep-alive probe interval. It defaults to 30s.
	//
	// It is the one duration where a negative value is meaningful: the standard library's net.Dialer takes a negative
	// value as "send no probes", which is why it is not in Validate's non-negative check.
	DialKeepAlive time.Duration `yaml:"DialKeepAlive"`

	// MaxIdleConns is the global maximum number of idle connections. It defaults to 100.
	MaxIdleConns int `yaml:"MaxIdleConns"`

	// MaxIdleConnsPerHost is the maximum number of idle connections per host. It defaults to 10.
	//
	// The standard library's default is 2, which is too small for a service calling only a few downstreams:
	// connections would be rebuilt over and over and the tail latency would be all handshakes.
	MaxIdleConnsPerHost int `yaml:"MaxIdleConnsPerHost"`

	// MaxConnsPerHost is the upper bound on connections per host (those in use included). It defaults to 0, unlimited, as in the standard library.
	//
	// Unlimited means as many as the concurrency asks for: measured against a downstream answering after 300ms with 200
	// concurrent requests, it received 200 new connections; another 200 right after reused only the 10 of
	// MaxIdleConnsPerHost and opened 190 more. As the downstream slows, the connection count rises with the concurrency
	// and may exhaust the peer's connections or this machine's ports. Set positive, excess requests queue for a connection, and that wait counts against Timeout and the caller's ctx.
	MaxConnsPerHost int `yaml:"MaxConnsPerHost"`

	// IdleConnTimeout is how long an idle connection is kept. It defaults to 90s, as in the standard library.
	//
	// When the downstream (or a load balancer in between) closes an idle connection first, a request reusing it at
	// exactly that moment fails: measured with a 200ms server-side idle timeout and request intervals around 200ms,
	// 27 of 300 POSTs failed (connection reset by peer / use of closed network connection); GETs are resent
	// automatically on a new connection and none of 200 failed. So it has to be shorter than the downstream's keep-alive timeout.
	IdleConnTimeout time.Duration `yaml:"IdleConnTimeout"`

	// RetryCount is the number of retries. It defaults to 0, meaning no retry.
	RetryCount int `yaml:"RetryCount"`

	// RetryWaitTime is the initial retry wait. It defaults to 100ms.
	RetryWaitTime time.Duration `yaml:"RetryWaitTime"`

	// RetryMaxWaitTime is the upper bound on the retry wait. It defaults to 2s.
	RetryMaxWaitTime time.Duration `yaml:"RetryMaxWaitTime"`

	// RetryOnlyIdempotent says whether only idempotent methods are retried. It defaults to on.
	//
	// A transport-layer timeout cannot tell "the request never reached the server" from "the server finished and the
	// response was lost", so resending a POST may mean a duplicate order or a duplicate charge.
	// Turn it off once the endpoint is confirmed idempotent (carrying an idempotency key, say).
	//
	// A condition added with AddRetryCondition cannot put a POST with no response back either (xhttp vetoes that in RetryAfter);
	// what it can put back is one retrying on a status code after a response arrived (a 5xx, say) -- vetoing there could only replace the response
	// with an invented error, so such a condition has to check the method itself. A SetRetryAfter of your own replaces this gate.
	// Either way, a request whose body is an io.Reader is never retried: the first attempt read it to the end and a resend would have an empty body.
	RetryOnlyIdempotent bool `yaml:"RetryOnlyIdempotent"`

	// Trace says whether a Span is opened for an outbound request. It defaults to on.
	//
	// It governs the Span only. With it off, traceparent, baggage and the forwarded headers from XTrace.ForwardHeaders
	// still travel downstream (the trace identifiers come from the upstream, or from a Span opened elsewhere in this
	// process): not recording a Span for this hop must not break the whole trace and the forwarding here.
	Trace bool `yaml:"Trace"`

	// Metric says whether the outbound request metrics are exported (by method, target and status code). It defaults to on.
	Metric bool `yaml:"Metric"`

	// Log says whether one log line is recorded per logical request (once, after every retry has finished). It defaults to off.
	//
	// It records the method, host, path, status code, duration and attempt count, plus the error on failure; the query string,
	// the fragment, the userinfo, the headers and the body are never recorded, and the query string is removed from the URLs in the error
	// text too. An error from resty (a transport error, a failing SetResult, a refused redirect) and a 5xx are recorded at warn.
	// With it on, resty's own lines on the retry path (one WARN per attempt, one ERROR once used up) are no longer printed and a request
	// is just this one line; a SetLogger of your own is unaffected. A request dump from SetDebug(true) is not covered, and its query
	// string and headers reach the log as they are. The fields and the measurements are in xhttp/README.md.
	Log bool `yaml:"Log"`

	// SlowThreshold records one warn line when the whole logical request (retries and backoff included) takes longer than this. It defaults to 1s, requires Log, and 0 records nothing.
	SlowThreshold time.Duration `yaml:"SlowThreshold"`

	// TLS is the TLS setup for outbound https requests: a self-signed CA, a client certificate for mutual authentication, the name to verify against.
	// It defaults to unconfigured, using the standard library's defaults (the system roots, no client certificate). The fields and rules are shared across modules, see xtls.Config.
	//
	// It governs https:// requests only and never upgrades http:// to TLS. It applies to **every** https request this
	// client sends: with CAFile configured only that CA is accepted, so an https downstream on the public internet no
	// longer verifies; and ServerName verifies every downstream against that one name. So this is for the client that
	// calls one kind of internal downstream only; to call the public internet as well, build another one with New.
	TLS xtls.Config `yaml:"TLS"`
}

// DefaultConfig holds every default in one place.
func DefaultConfig() Config {
	return Config{
		Timeout:             60 * time.Second,
		DialTimeout:         30 * time.Second,
		DialKeepAlive:       30 * time.Second,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		RetryWaitTime:       100 * time.Millisecond,
		RetryMaxWaitTime:    2 * time.Second,
		RetryOnlyIdempotent: true,
		SlowThreshold:       time.Second,
		Trace:               true,
		Metric:              true,
	}
}

// Validate checks what does not make sense in the configuration itself.
//
// xconfig.Unmarshal calls it after decoding the XHttp block of the config file, so a misconfigured value fails while
// the config is read, carrying which file and which block; a direct call to New calls it once as well.
// It returns a plain error, which the layer calling it wraps in an xerror once.
func (c Config) Validate() error {
	if c.RetryCount < 0 {
		return fmt.Errorf("RetryCount must not be negative, got=%d", c.RetryCount)
	}
	if c.MaxIdleConns < 0 || c.MaxIdleConnsPerHost < 0 || c.MaxConnsPerHost < 0 {
		return fmt.Errorf("connection counts must not be negative, MaxIdleConns=%d MaxIdleConnsPerHost=%d MaxConnsPerHost=%d",
			c.MaxIdleConns, c.MaxIdleConnsPerHost, c.MaxConnsPerHost)
	}
	// A negative duration has no sensible meaning, and every field below it reacts differently:
	// a negative DialTimeout makes the dial deadline a moment in the past from the start, so every request fails with
	// i/o timeout on the spot; a negative Timeout is taken by the standard library as "no limit" instead (it only sets
	// a deadline when > 0), so the timeout protection disappears entirely. Both are a silent failure bought with one
	// minus sign, from a config file that looks perfectly fine.
	for _, d := range []struct {
		name string
		val  time.Duration
	}{
		{"Timeout", c.Timeout},
		{"DialTimeout", c.DialTimeout},
		{"IdleConnTimeout", c.IdleConnTimeout},
		{"RetryWaitTime", c.RetryWaitTime},
		{"RetryMaxWaitTime", c.RetryMaxWaitTime},
		{"SlowThreshold", c.SlowThreshold},
	} {
		if d.val < 0 {
			return fmt.Errorf("%s must not be negative, got=%v", d.name, d.val)
		}
	}
	return c.TLS.Validate()
}
