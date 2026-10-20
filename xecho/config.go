// Package xecho assembles an Echo server from configuration; the caller gets the native
// *echo.Echo.
//
//	func main() {
//		ex := xecho.New().WithRoutes(registerRoutes)
//		xone.MustRun(ex)
//	}
//
// Access logging, tracing, metrics and panic recovery are installed as four middlewares whose
// order the framework owns (with access logging on, one more layer sits outermost: LogScope,
// which opens a per-request field scope for xlog.AddKV; with tracing off, the tracing layer
// becomes Propagate, which only forwards and opens no span). Switches, listen address, timeouts
// and TLS all live in the XEcho block of the config file; business code initializes nothing.
//
// The access log fields, metric names and span names are identical to xgin's, and so are which
// client_ip is believed, what is masked in the log and whether shutdown waits for handlers:
// those live in internal/web, which both share.
package xecho

import (
	"fmt"
	"strings"
	"time"

	"github.com/jupiter-eng/xone/internal/web"
)

// ConfigKey is this module's top-level key in the config file.
const ConfigKey = "XEcho"

// Config is the server configuration.
type Config struct {
	// Host is the listen address. Defaults to 0.0.0.0.
	Host string `yaml:"Host"`

	// Port is the listen port. Defaults to 8080.
	Port int `yaml:"Port"`

	// UseH2C enables HTTP/2 without TLS. Off by default.
	//
	// Under TLS, HTTP/2 is automatic and this option is not needed.
	//
	// Prior knowledge only: the client opens with the HTTP/2 preface (gRPC and
	// curl --http2-prior-knowledge both do). The HTTP/1.1 Upgrade: h2c handshake is not
	// supported, and a client sending it gets an ordinary HTTP/1.1 response.
	UseH2C bool `yaml:"UseH2C"`

	// TLS holds the server certificate and mutual authentication. Off by default: setting both
	// CertFile and KeyFile makes it https.
	TLS TLSConfig `yaml:"TLS"`

	// ReadHeaderTimeout bounds reading the request headers. Defaults to 10s, must be > 0.
	//
	// This is the main defense against slow-connection attacks: without a limit, a slow client
	// holds a connection indefinitely, and once the connection count is exhausted the whole
	// service is unavailable. All four timeouts on echo's own e.Server are 0; this module does
	// not use it and builds an http.Server from these fields instead.
	//
	// 0 does not mean "use some default": net/http falls back to ReadTimeout, whose default is
	// also 0, so there is no limit at all (measured: a connection sending half a request header
	// is never closed). Both 0 and negative values are therefore rejected.
	ReadHeaderTimeout time.Duration `yaml:"ReadHeaderTimeout"`

	// ReadTimeout bounds reading the whole request, body included. Unlimited by default.
	//
	// Unlimited by default because a limit cuts off large uploads. A value matching the business
	// ceiling is better, but only the business knows that value. 0 means unlimited; a negative
	// value fails startup.
	ReadTimeout time.Duration `yaml:"ReadTimeout"`

	// WriteTimeout bounds writing the response. Unlimited by default.
	//
	// Unlimited by default because a limit cuts off SSE, long polling and large downloads.
	// 0 means unlimited; a negative value fails startup.
	WriteTimeout time.Duration `yaml:"WriteTimeout"`

	// IdleTimeout bounds how long a keep-alive connection may sit idle. Defaults to 60s, must be > 0.
	//
	// Same reasoning as ReadHeaderTimeout: 0 falls back to ReadTimeout (itself 0 by default),
	// which means idle connections are never reclaimed.
	IdleTimeout time.Duration `yaml:"IdleTimeout"`

	// TrustedProxies lists which directly connected peers are trusted: only their
	// X-Forwarded-For / X-Real-IP is used to compute client_ip (c.RealIP()), and only their
	// forwarded headers (XTrace.ForwardHeaders / ForwardHeaderRules) and baggage are accepted and
	// passed downstream. "Who counts as one of us" is stated here and nowhere else.
	//
	// The default is [private]: loopback, the private ranges, 100.64.0.0/10, and IPv6 ::1 and
	// fc00::/7. Load balancers, Kubernetes ingresses and pods, and sidecars all fall inside those
	// ranges, so however random pod IPs are, they need no listing. For a service exposed directly
	// to the internet the peer is a public address, none of which is trusted, so X-Forwarded-For
	// still cannot forge client_ip. Echo's own default (e.IPExtractor nil) believes everyone:
	// measured, a public peer 203.0.113.9 sending X-Forwarded-For: 1.2.3.4 makes c.RealIP()
	// return 1.2.3.4.
	//
	// The client_ip algorithm matches xgin's (walk right to left, take the first untrusted
	// address), so moving a service between frameworks does not change it.
	//
	// To add ranges on top of the default, write private alongside them (the list replaces the
	// default wholesale); to trust nobody, write []:
	//
	//	TrustedProxies: [private, 203.0.113.0/24]
	//	TrustedProxies: []
	//
	// When the internal network also holds untrusted clients (an office network or VPN users can
	// reach the service directly), do not use private: list the exact ranges. Write IPv4 as
	// 10.0.0.1 or 10.0.0.0/8; an IPv4-mapped IPv6 form such as ::ffff:10.0.0.1 fails startup.
	TrustedProxies []string `yaml:"TrustedProxies"`

	// Debug turns on echo's debug switch (e.Debug). Off by default, same as echo.New().
	// Keep it off in production.
	//
	// In echo v4.16.0 it governs two things the caller can see (those are all the places the
	// source reads e.Debug):
	//
	//   - error responses carry the error text: a 500 goes from {"message":"Internal Server Error"}
	//     to {"message":"Internal Server Error","error":"<err.Error()>"} (DefaultHTTPErrorHandler
	//     in echo.go, and only when Message is a string). Error text often holds a DSN, SQL or an
	//     internal address; those are masked on their way into the access log (see "Access log" in
	//     the README) and are not masked on their way into the response;
	//   - c.JSON / c.JSONP / c.XML indent by two spaces (c.echo.Debug || pretty in context.go),
	//     which is the same as every request carrying ?pretty: the body grows and the access log's
	//     bytes_out grows with it.
	//
	// One more thing echo does only inside e.Start / e.StartTLS: raise its own log level to DEBUG.
	// xecho does not use e.Start, so applyConfig does that part itself.
	//
	// This is not XGin.Mode: gin's Mode is process-wide (gin.SetMode), while this is per instance,
	// so two servers started with WithConfig in one process can differ.
	Debug bool `yaml:"Debug"`

	// Log enables the access log middleware. On by default.
	Log bool `yaml:"Log"`

	// LogSkipPaths lists paths that are not access-logged. Empty by default.
	//
	// A path ending in / matches by prefix, anything else matches exactly. With Metric on,
	// MetricPath is added automatically and needs no entry.
	LogSkipPaths []string `yaml:"LogSkipPaths"`

	// LogRequestBody records the request body in the access log. Off by default.
	//
	// Recording a body means buffering its first 256KB and masking every field, which costs
	// enough and risks enough to be a decision. Before turning it on, make sure the sensitive
	// words are complete (middleware.AddSensitiveFields; the rules are under "Access log" in
	// xecho/README.md).
	LogRequestBody bool `yaml:"LogRequestBody"`

	// LogResponseBody records the response body in the access log. Off by default.
	LogResponseBody bool `yaml:"LogResponseBody"`

	// LogQuery records the query string in the access log, as the query field. Off by default.
	//
	// Query strings often carry credentials: GET /login?token=..., signed links, an OAuth
	// callback's code. With it on, fields are masked by the same rules as a form body; a name
	// missing from the word list (code, for instance) still goes through in the clear, so add it
	// with middleware.AddSensitiveFields.
	LogQuery bool `yaml:"LogQuery"`

	// LogRequestHeaders records the request headers in the access log, as the request_headers
	// field. Off by default.
	//
	// With it on, credential-bearing values are masked: the listed ones such as Authorization,
	// Cookie and X-Api-Key, plus any name containing a sensitive word; values that are URLs
	// (Referer) lose their query string. Add business headers outside the list with
	// middleware.AddSensitiveHeaders.
	LogRequestHeaders bool `yaml:"LogRequestHeaders"`

	// LogResponseHeaders records the response headers in the access log, as the response_headers
	// field. Off by default.
	//
	// Masking works as it does for request headers: the listed ones such as Set-Cookie, plus any
	// name containing a sensitive word, have their values masked.
	LogResponseHeaders bool `yaml:"LogResponseHeaders"`

	// Trace opens a server span for inbound requests. On by default.
	//
	// It governs the span only. With it off, an incoming traceparent, baggage and forwarded
	// headers are still accepted (under the same trust rules as TrustedProxies), trace_id in the
	// log is the upstream one, and requests sent through xhttp still carry it downstream; the
	// response no longer carries X-Trace-Id back.
	Trace bool `yaml:"Trace"`

	// Metric enables the metrics middleware. On by default.
	//
	// When on, MetricPath (/metrics by default) is registered automatically and needs no route
	// of your own.
	Metric bool `yaml:"Metric"`

	// MetricPath is the path of the metrics endpoint. Defaults to /metrics, and must start with /
	// while Metric is on.
	//
	// Echo does not reject other spellings, it rewrites them (measured on v4.16.0): writing
	// metrics registers /metrics while the access log skips metrics, so the two no longer match;
	// leaving it empty registers on the root path /, and when the business later registers a home
	// page one of the two silently replaces the other — either the home page is gone or the
	// metrics are. Both spellings are therefore rejected at startup.
	MetricPath string `yaml:"MetricPath"`
}

// TLSConfig is the server-side TLS configuration, the TLS block in the config file.
//
// There is no Enable switch: setting both CertFile and KeyFile turns it on, leaving both empty
// serves plaintext.
type TLSConfig struct {
	// CertFile is the path to the TLS certificate. It and KeyFile must both be set or both be empty.
	CertFile string `yaml:"CertFile"`

	// KeyFile is the path to the TLS private key.
	KeyFile string `yaml:"KeyFile"`

	// ClientCAFile is the CA used to verify client certificates (PEM, several are allowed).
	// Empty by default, meaning no client certificate is requested.
	//
	// Setting it turns on mutual authentication (tls.RequireAndVerifyClientCert): the client must
	// present a certificate signed by this CA, and without one — or with one signed by someone
	// else — the handshake fails and the request reaches no handler, including framework routes
	// such as /metrics. It can only be set together with CertFile and KeyFile.
	ClientCAFile string `yaml:"ClientCAFile"`

	// MinVersion is the lowest TLS version accepted: "1.2" or "1.3", and empty means "1.2".
	// Defaults to "1.2". It applies, and is validated, only when a certificate is configured.
	//
	// Lower versions are not accepted: TLS 1.0 and 1.1 have long been deprecated (RFC 8996).
	MinVersion string `yaml:"MinVersion"`
}

// DefaultConfig gathers every default value in one place.
func DefaultConfig() Config {
	return Config{
		Host:              "0.0.0.0",
		Port:              8080,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		Log:               true,
		Trace:             true,
		Metric:            true,
		MetricPath:        "/metrics",
		TLS:               TLSConfig{MinVersion: "1.2"},
		TrustedProxies:    []string{web.TrustPrivate},
	}
}

// Validate reports the ways this configuration contradicts itself.
//
// xconfig.Unmarshal calls it after decoding the XEcho block of the config file, so a misconfigured
// value fails during startup rather than when the server starts listening; the config handed to
// WithConfig is validated during assembly, see XEcho.cfg.
func (c Config) Validate() error {
	// The rules for ports, TLS, timeouts and proxy ranges are shared by the web integrations and
	// live in internal/web, so the messages match xgin's word for word.
	s := c.server()
	if err := s.ValidateListen(); err != nil {
		return err
	}
	if err := s.ValidateTimeouts(); err != nil {
		return err
	}
	if err := web.ValidateProxies(c.TrustedProxies); err != nil {
		return err
	}
	// The reason is under MetricPath: echo does not reject it, it quietly rewrites it into a
	// different path. With metrics off the field is unused and may say anything.
	if c.Metric && !strings.HasPrefix(c.MetricPath, "/") {
		return fmt.Errorf("MetricPath must start with /, got=%q", c.MetricPath)
	}
	return nil
}

// server extracts the fields that listening and graceful shutdown need, for web.Server.
func (c Config) server() web.ServerConfig {
	return web.ServerConfig{
		Host:              c.Host,
		Port:              c.Port,
		UseH2C:            c.UseH2C,
		CertFile:          c.TLS.CertFile,
		KeyFile:           c.TLS.KeyFile,
		ClientCAFile:      c.TLS.ClientCAFile,
		MinVersion:        c.TLS.MinVersion,
		ReadHeaderTimeout: c.ReadHeaderTimeout,
		ReadTimeout:       c.ReadTimeout,
		WriteTimeout:      c.WriteTimeout,
		IdleTimeout:       c.IdleTimeout,
	}
}
