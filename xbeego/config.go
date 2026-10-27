// Package xbeego serves a beego application from configuration; the caller keeps the native beego
// API.
//
//	func main() {
//		routers.Setup()                  // web.Router / web.InsertFilter, unchanged
//		xone.MustRun(xbeego.New())
//	}
//
// Access logging, tracing, metrics and panic recovery are installed as four middlewares whose
// order the framework owns (with access logging on, one more layer sits outermost: LogScope, which
// opens a per-request field scope for xlog.AddKV; with tracing off, the tracing layer becomes
// Propagate, which only forwards and opens no span). Switches, listen address, timeouts and TLS
// all live in the XBeego block of the config file; business code initializes nothing.
//
// The access log fields, metric names and span names are identical to xgin's and xecho's, and so
// are which client_ip is believed, what is masked in the log and whether shutdown waits for
// handlers: those live in internal/web, which all three share.
//
// # What this module does differently from xgin and xecho
//
// Beego's API is package level: web.Router, web.InsertFilter and the rest all register on the
// single global web.BeeApp, and an application's routers package registers into it from its own
// init. An integration that built a server of its own would see none of those routes, so this one
// binds to that global -- New stays pure, and the binding happens during assembly. Two XBeego
// instances in one process therefore serve the same routes on two ports; they do not have separate
// routers.
//
// The server is this module's own (internal/web.Server), the same one xgin and xecho use, with
// web.BeeApp.Handlers -- an http.Handler -- as the handler. web.Run is not called, which is what
// makes the graceful shutdown, the timeouts, h2c and TLS identical to the other two. The price is
// that beego's own initBeforeHTTPRun does not run either, so these are not available:
//
//   - sessions (web.BConfig.WebConfig.Session.SessionOn), HTML templates and the views directory,
//     static-file MIME types, gzip (web.BConfig.EnableGzip) and the admin server;
//   - beego's default error pages: with none registered, beego answers an unmatched route with the
//     status code and the code itself as a plain-text body. Register your own with
//     web.ErrorHandler to change that;
//   - callbacks registered with web.AddAPPStartHook: initBeforeHTTPRun is the only thing that runs
//     them, so such a callback never runs, with no error and no log. An application initialising
//     its ORM or its cache that way has to call it itself.
//
// For a JSON API -- which is what beego services in this organization are -- none of those is in
// use. A service that does need one of them should stay on web.Run and take logging alone, see
// "Troubleshooting" in xbeego/README.md.
//
// Importing beego also runs its own package init, with two effects a caller can see. It reads
// conf/app.conf from the working directory and panics when it cannot be parsed -- before xone
// reads anything, so such an error does not get the file and line the framework's own
// configuration errors get. And its grace package registers the command-line flags -graceful and
// -socketorder on flag.CommandLine: they do nothing here, web.Run being the only thing that reads
// them, but they appear in the program's -h output, and a caller defining a flag of either name
// panics at startup with "flag redefined". xone itself is unaffected, reading --config and
// --profile by scanning os.Args.
// The two files live side by side -- app.conf stays beego's, while xone reads application.yml from
// the conventional paths (conf/, config/, the working directory; see docs/config.md) -- and the
// listen address comes from XBeego.Port, beego's httpport being unused because web.Run is not
// called.
package xbeego

import (
	"fmt"
	"strings"
	"time"

	"github.com/jupiter-eng/xone/internal/web"
)

// ConfigKey is this module's top-level key in the config file.
const ConfigKey = "XBeego"

// Config is the server configuration.
type Config struct {
	// Host is the listen address. Defaults to 0.0.0.0.
	Host string `yaml:"Host"`

	// Port is the listen port. Defaults to 8080.
	//
	// This is the only listen setting that counts: web.Run is not called, so beego's own httpaddr
	// and httpport in conf/app.conf are read by beego and used by nobody.
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
	// service is unavailable. Beego sets ReadTimeout and WriteTimeout from a single
	// ServerTimeOut in conf/app.conf (0 by default, that is, no limit at all) and has no setting
	// for the other two; this module does not use beego's server and builds an http.Server from
	// these fields instead.
	//
	// 0 does not mean "use some default": net/http falls back to ReadTimeout, whose default is
	// also 0, so there is no limit at all. Both 0 and negative values are therefore rejected.
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
	// X-Forwarded-For / X-Real-IP is used to compute client_ip, and only their forwarded headers
	// (XTrace.ForwardHeaders / ForwardHeaderRules) and baggage are accepted and passed downstream.
	// "Who counts as one of us" is stated here and nowhere else.
	//
	// The default is [private]: loopback, the private ranges, 100.64.0.0/10, and IPv6 ::1 and
	// fc00::/7. Load balancers, Kubernetes ingresses and pods, and sidecars all fall inside those
	// ranges, so however random pod IPs are, they need no listing. For a service exposed directly
	// to the internet the peer is a public address, none of which is trusted, so X-Forwarded-For
	// still cannot forge client_ip.
	//
	// Beego's own ctx.Input.IP() is not used and is not governed by this: it walks
	// X-Forwarded-For from the left and believes whoever sent it (Proxy / IP in
	// server/web/context/input.go). The client_ip in the access log, in the metrics and on the
	// span is computed here instead, by the same algorithm xgin and xecho use, so moving a service
	// between frameworks does not change it. Business code reading ctx.Input.IP() keeps beego's
	// answer; to get the framework's, read the client_ip field of the access log or call
	// xbeego.ClientIP(r).
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

	// Log enables the access log middleware. On by default.
	//
	// It replaces whatever access logging the application had of its own (a filter pair at
	// BeforeRouter / AfterExec is the usual shape). Leaving both in place writes every request
	// twice, with two different sets of field names; remove the application's.
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
	// xbeego/README.md).
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
	// When on, MetricPath (/metrics by default) is served by the framework and needs no route of
	// your own. It is answered before the request reaches beego, so beego's router never sees it:
	// an application that registers a route on that path has to drop it or move this one, and
	// until it does, startup fails with an error naming MetricPath rather than the route being
	// shadowed without a word.
	Metric bool `yaml:"Metric"`

	// MetricPath is the path of the metrics endpoint. Defaults to /metrics, and must start with /
	// while Metric is on.
	//
	// It is matched on URL.Path exactly, as the access log's SkipPaths does, so a spelling without
	// the leading / would match nothing and leave the endpoint unreachable without a word. That is
	// why it is rejected at startup instead.
	//
	// The framework answers this path in front of beego's router, so a beego route registered on
	// exactly this template fails startup with an error naming this field rather than never being
	// reached, see Metric. A pattern that merely happens to match it (/:page) is left alone: it
	// was never meant to be this endpoint.
	MetricPath string `yaml:"MetricPath"`

	// RecoverPanic takes beego's panic recovery over (web.BConfig.RecoverFunc). On by default.
	//
	// Beego recovers panics inside its own router and writes them with its logs package: plain
	// text on stderr, no trace_id, no masking, and the whole thing invisible to the access log and
	// the metrics, which see a 500 with no reason attached. With this on, the framework's own
	// recovery is installed in its place (web.BConfig.RecoverFunc): one ERROR line through slog
	// carrying trace_id, the panic value masked by the same rules as the access log's, and a
	// stack. The response is a bare 500 unless WithRecoverFunc says otherwise.
	//
	// Turning it off leaves beego's recovery as it is. The framework then still catches whatever
	// escapes beego (a panic in middleware mounted with WithMiddleware, for one), since a panic
	// reaching net/http kills the connection with no log at all.
	//
	// What is replaced is the slot beego calls (web.BConfig.RecoverFunc), not its switch, so
	// beego's own web.BConfig.RecoverPanic -- recoverpanic in conf/app.conf -- decides nothing
	// while this is on. A panic is caught either way: with this off, what beego's recovery lets
	// through is still caught outside beego.
	//
	// This is a process-wide setting of beego's, not a per-instance one: two XBeego instances in
	// one process share it, as they share the router, and the first assembly settles it for the
	// process. An instance assembling later with a different value is told that value was ignored.
	RecoverPanic bool `yaml:"RecoverPanic"`

	// BeegoLogs routes beego's own logs package into slog. On by default.
	//
	// Beego's logs writes plain text to the console: a service whose logs are collected as JSON
	// has those lines fall out of every field-based query, and an application using beego's logs
	// as its own logger has all of its logging fall out. With this on, every line beego writes
	// becomes one slog record (the message under message, beego's eight levels folded onto slog's
	// four), so XLog governs the format and the level for the whole process.
	//
	// It replaces whatever the application configured on beego's logger, so turn it off to keep a
	// setup of your own -- a file adapter with rotation, say. beego's logger is process-wide, as
	// its router is, and the first assembly settles this for the process; an instance assembling
	// later with a different value is told that value was ignored.
	//
	// These lines carry no trace_id: beego's logs API has no ctx to take one from. A handler that
	// wants one writes through slog with the request's ctx (c.Ctx.Request.Context()).
	BeegoLogs bool `yaml:"BeegoLogs"`
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
	// present a certificate signed by this CA, and without one -- or with one signed by someone
	// else -- the handshake fails and the request reaches no handler, including framework routes
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
		RecoverPanic:      true,
		BeegoLogs:         true,
		TLS:               TLSConfig{MinVersion: "1.2"},
		TrustedProxies:    []string{web.TrustPrivate},
	}
}

// Validate reports the ways this configuration contradicts itself.
//
// xconfig.Unmarshal calls it after decoding the XBeego block of the config file, so a misconfigured
// value fails during startup rather than when the server starts listening; the config handed to
// WithConfig is validated during assembly, see XBeego.cfg.
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
	// The reason is under MetricPath. With metrics off the field is unused and may say anything.
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
