package xecho

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/labstack/echo/v4"
	gommonlog "github.com/labstack/gommon/log"

	"github.com/jupiter-eng/xone/internal/web"
	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xecho/internal/peer"
	"github.com/jupiter-eng/xone/xecho/internal/reqctx"
	"github.com/jupiter-eng/xone/xecho/middleware"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xhook"
	"github.com/jupiter-eng/xone/xmetric"

	// Using xecho means tracing is there: xtrace installs the global TracerProvider and
	// Propagator, and the middleware reaches them through otel's global API. To go without
	// tracing, set XTrace.Enable: false.
	_ "github.com/jupiter-eng/xone/xtrace"

	// Using a web framework means xlog is there: the access log and per-request fields
	// (xlog.AddKV) both rely on it. The root package does not pull it in, so a program using only
	// xhook and xgorm does not have its logging taken over.
	_ "github.com/jupiter-eng/xone/xlog"
)

// XEcho is an HTTP server waiting to be started.
//
// It satisfies xone.Runnable, so it goes straight to xone.Run:
//
//	xone.MustRun(xecho.New().WithRoutes(register))
//
// Note that the root package is not imported here — Go's interfaces are structural, matching
// methods are enough. That keeps "an integration does not depend on the framework" true at
// compile time.
type XEcho struct {
	override *Config // see WithConfig

	routes    []func(*echo.Echo)
	extra     []echo.MiddlewareFunc
	recover   func(echo.Context, any) error
	validator echo.Validator

	// The fields below are written once by build and read-only afterwards. Whoever reads them
	// either called build first or is a handler on the engine — and the engine is only reachable
	// through build. Once guarantees the write happens before the reads, so no lock is needed.
	buildOnce sync.Once
	engine    *echo.Echo
	conf      Config      // the config assembly used, and the one Start uses
	confErr   error       // the error when the config is invalid; conf is then the defaults
	trusted   web.Proxies // the ranges parsed out of TrustedProxies, see markTrustedPeer

	// server is the half that has nothing to do with echo: listening and graceful shutdown, see
	// web.Server. e.Start is not used: all four timeouts on its e.Server are 0, and it prints a
	// banner to stdout.
	server web.Server
}

// New creates an XEcho.
//
// Switches, port and timeouts live in the XEcho block of the config file; a second server in the
// same process uses WithConfig. Nothing is read here — the config is taken at assembly time
// (Engine or Start). The zero value &XEcho{} is equivalent to New().
func New() *XEcho { return &XEcho{} }

// WithConfig starts the server from this config instead of reading the XEcho block of the config
// file.
//
// Use it for a second server in one process — a public API on one port and an internal admin
// endpoint on another, say. Without it both instances read the same block and can only listen on
// the same port.
//
// What it takes is a complete config, not a patch: to change a couple of fields on top of the
// file's, start from CurrentConfig (or from DefaultConfig when writing one from scratch, since
// zero timeouts are invalid):
//
//	c := xecho.CurrentConfig()
//	c.Host, c.Port = "127.0.0.1", 9090
//	admin := xecho.New().WithConfig(c).WithRoutes(adminRoutes)
//
// It is validated during assembly; when it is invalid, Start returns the error and never listens.
func (x *XEcho) WithConfig(c Config) *XEcho {
	x.override = &c
	return x
}

// CurrentConfig returns a copy of the XEcho block of the config file, and may be called any time
// before Start.
//
// Use it together with WithConfig, see the example there. The config file is loaded the first
// time it is read, so even a call in main before xone.Run gets the final values.
//
// When that block is invalid it returns the defaults: the error is reported by xone.Run at
// startup, and is not reported a second time here.
func CurrentConfig() Config {
	c, _ := fileConfig()
	return c
}

// WithRoutes registers routes. It may be called several times and takes effect in call order.
//
// The callback gets the native *echo.Echo and may set anything. It runs after the config has been
// applied to echo, so settings in the callback win over the config: replacing e.IPExtractor,
// e.HTTPErrorHandler or e.Validator is taken from the callback.
//
// One thing does not follow: which peers are trusted for forwarded headers
// (XTrace.ForwardHeaders) comes only from TrustedProxies in the config, while an IPExtractor
// replaced in the callback only governs c.RealIP(). To move both, change the config.
func (x *XEcho) WithRoutes(f ...func(*echo.Echo)) *XEcho {
	x.routes = append(x.routes, f...)
	return x
}

// WithMiddleware appends custom middleware, after all the built-in middleware.
//
// It is mounted on e.Use and runs after the router, so c.Path() and c.Param() are available; the
// built-in middleware is mounted on e.Pre, outside the caller's own e.Pre, in the order build
// lays out. It applies to every request, including the framework's MetricPath and requests that
// matched no route (that is how echo's e.Use behaves).
func (x *XEcho) WithMiddleware(m ...echo.MiddlewareFunc) *XEcho {
	x.extra = append(x.extra, m...)
	return x
}

// WithRecoverFunc customizes the response after a panic.
//
// The error f returns goes to e.HTTPErrorHandler exactly as a handler's would; return nil after
// writing a response yourself. The default returns echo.ErrInternalServerError, that is, a 500
// {"message":"Internal Server Error"}. f is not called once the response has started going out:
// changing the status code then only yields a truncated response.
func (x *XEcho) WithRecoverFunc(f func(c echo.Context, recovered any) error) *XEcho {
	x.recover = f
	return x
}

// WithValidator installs a validator, after which a handler can call c.Validate(&req).
//
// Echo has no built-in validation the way gin does: without one, c.Validate returns
// echo.ErrValidatorNotRegistered rather than a validation result — a service that forgot to
// install a validator looks perfectly fine until someone sends an invalid body. That is a plain
// error (errors.New in echo.go), not an *echo.HTTPError: a handler returning it directly gives
// the client a 500 {"message":"Internal Server Error"}, with the text going only to the access
// log (measured on v4.16.0).
//
// Which validation library to use and what its errors look like is the business's call; the
// framework does not choose for you and only installs it on e.Validator:
//
//	// github.com/go-playground/validator/v10
//	type structValidator struct{ v *validator.Validate }
//
//	func (s structValidator) Validate(i any) error { return s.v.Struct(i) }
//
//	xecho.New().WithValidator(structValidator{v: validator.New()}).WithRoutes(register)
//
// c.Validate hands whatever Validate returns to the handler unchanged, and the handler decides
// what to do with it — to turn it into a 400, return
// echo.NewHTTPError(http.StatusBadRequest, err.Error()); doing nothing makes a plain error a 500.
// The error text reaches the access log's errors field (with sensitive words masked) but not the
// response, unless Debug is on.
//
// Passing nil is the same as not installing one. Setting e.Validator in a WithRoutes callback
// works too, and wins over this.
func (x *XEcho) WithValidator(v echo.Validator) *XEcho {
	x.validator = v
	return x
}

// Engine returns the native *echo.Echo, for anything this package does not cover.
//
// Calling it triggers assembly, from the config given to WithConfig or from the XEcho block of
// the config file — which is loaded the first time it is read, so even a call before xone.Run
// gets the final values. Assembly happens once: WithConfig / WithRoutes / WithMiddleware /
// WithRecoverFunc / WithValidator have no effect afterwards, and Start reuses that same config.
//
// Do not call e.Start / e.StartTLS on it: the server is started by Start, which owns the
// timeouts, TLS and graceful shutdown.
//
// When the config is invalid it still returns an echo, assembled from the defaults (trusting only
// private-range proxies, the safe side), and logs a warning. The error itself is returned by
// Start, so xone.Run reports it at startup.
func (x *XEcho) Engine() *echo.Echo {
	x.build()
	return x.engine
}

// build assembles echo, once per instance.
//
// Once rather than a boolean flag: with a flag, concurrent calls register the middleware twice,
// which shows up as two log lines per request and doubled metrics.
//
// The order matters: create echo and apply the config, then mount the middleware and the built-in
// routes, and only then run the caller's WithRoutes — which is why settings in the callback win
// over the config, see WithRoutes.
func (x *XEcho) build() {
	x.buildOnce.Do(func() {
		// Assemble even when the config is invalid, from the defaults: a caller of Engine() has
		// to get an echo one way or another, and the defaults are the safe side. The error is
		// kept for Start to return; the warning is for callers who only use Engine() and never
		// call Start, who would otherwise get the defaults with nothing to show for it.
		c, err := x.cfg()
		if err != nil {
			slog.Warn("xecho invalid config, assembling the engine with defaults; Start will return the error", "error", err)
		}
		x.conf, x.confErr = c, err

		e := echo.New()
		applyConfig(e, c)
		// The validator lands early, with the config: an e.Validator set in a WithRoutes callback
		// wins over it, as every other setting does. Without one, echo's default stands (nil, so
		// c.Validate returns ErrValidatorNotRegistered).
		if x.validator != nil {
			e.Validator = x.validator
		}
		x.trusted = web.ParseProxies(c.TrustedProxies)

		// The onion, outermost first:
		//   LogScope -> Trace -> Log -> Metric -> Recover -> the caller's e.Pre -> router ->
		//   the caller's middleware -> handler
		//
		// Recover has to be the innermost of the built-ins: whichever layer catches a panic, the
		// code after next(c) in every layer inside it never runs. Innermost, the outer layers
		// still get to finish their work (record the metric, write the access log).
		//
		// The built-ins use e.Pre rather than e.Use: the latter runs after the router, so
		// requests the caller rejects or redirects in e.Pre (auth, rate limiting, the trailing
		// slash 301 all tend to live there) never reach them — no access log, no metrics, no
		// trace — and a panic in Pre is caught by nobody (echo does not catch it, and the client
		// reads EOF). Mounting on Pre still yields the route template: the router runs innermost
		// in the Pre chain (Echo.ServeHTTP), so c.Path() is set by the time next(c) returns, and
		// a request that never reached the router has an empty one and is recorded as unmatched.
		// They are mounted before WithRoutes, which puts them outside the caller's e.Pre.
		//
		// Nothing in Pre touches the request: the log scope and the span are attached to a ctx
		// and stored in echo.Context, and attachContext swaps them onto the request after
		// routing — the reasoning is in reqctx. So the caller's e.Pre sees the request's original
		// ctx (logs written there carry no trace_id).
		e.Pre(x.enter)
		if c.Log {
			e.Pre(middleware.LogScope())
		}
		// Trace governs the span only: with it off, the upstream trace identifiers and forwarded
		// headers are still accepted, there is simply no span.
		if c.Trace {
			e.Pre(skipMetrics(c, middleware.Trace()))
		} else {
			e.Pre(middleware.Propagate())
		}
		if c.Log {
			skip := slices.Clone(c.LogSkipPaths)
			if c.Metric {
				// The metrics endpoint is polled every few seconds by the scraper; logging it is
				// pure noise.
				skip = append(skip, c.MetricPath)
			}
			e.Pre(middleware.Log(
				middleware.WithSkipPaths(skip...),
				middleware.WithBody(c.LogRequestBody, c.LogResponseBody),
				middleware.WithQuery(c.LogQuery),
				middleware.WithHeaders(c.LogRequestHeaders, c.LogResponseHeaders),
			))
		}
		if c.Metric {
			e.Pre(middleware.Metric())
		}
		e.Pre(middleware.Recover(x.recover))
		// The caller's middleware runs after the router: what it wants is c.Path() and c.Param(),
		// the same as the caller's own e.Use. Before it, swap the ctx built in Pre onto the
		// request, so it and the handler see the ctx carrying the span and the log scope.
		e.Use(attachContext)
		e.Use(x.extra...)

		// Echo wraps e.Use around each request as it arrives, regardless of when routes were
		// registered (gin fixes the chain at registration time): the metrics endpoint therefore
		// goes through the shared auth a caller mounted with WithMiddleware too. Written after
		// the middleware, in xgin's order.
		if c.Metric {
			e.GET(c.MetricPath, serveMetrics)
		}
		for _, f := range x.routes {
			f(e)
		}

		x.engine = e
	})
}

// serveMetrics is the metrics endpoint. The handler is fetched per request rather than fixed at
// assembly time: assembly can happen before xmetric is initialized (calling Engine() before
// xone.Run does exactly that), and the handler taken then belongs to the fallback registry, which
// would leave /metrics empty forever.
func serveMetrics(c echo.Context) error {
	xmetric.Handler().ServeHTTP(c.Response(), c.Request())
	return nil
}

// skipMetrics keeps the metrics endpoint out of m: the scraper polls it every few seconds, and a
// span per poll only floods the tracing backend — the access log skips it for the same reason.
// Middleware mounted on Pre has not routed yet, so the decision is made on the path, comparing
// URL.Path only, as the access log's SkipPaths does.
func skipMetrics(c Config, m echo.MiddlewareFunc) echo.MiddlewareFunc {
	if !c.Metric {
		return m
	}
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		h := m(next)
		return func(ctx echo.Context) error {
			if ctx.Request().URL.Path == c.MetricPath {
				return next(ctx)
			}
			return h(ctx)
		}
	}
}

// applyConfig lands the three config items that govern echo onto e: which client_ip is believed,
// where echo's own log goes, and whether the debug switch is on.
func applyConfig(e *echo.Echo, c Config) {
	// e.Debug governs two things the caller can see: whether error responses carry the error text
	// (DefaultHTTPErrorHandler in echo.go) and whether c.JSON / c.JSONP / c.XML indent
	// (context.go, the same as every request carrying ?pretty). Measured on echo v4.16.0; off by
	// default, the reasoning is under Config.Debug.
	e.Debug = c.Debug

	// Trust only private ranges by default. Echo's default (IPExtractor nil) believes whoever
	// sends X-Forwarded-For: a public peer sending X-Forwarded-For: 1.2.3.4 makes c.RealIP()
	// return 1.2.3.4 (measured on v4.16.0). The algorithm matches gin's ClientIP as used by xgin,
	// see web.Proxies.ClientIP. A malformed range is already rejected in Validate; ParseProxies
	// skips the entries it cannot parse, trusting nothing it is unsure about.
	e.IPExtractor = web.ParseProxies(c.TrustedProxies).ClientIP

	// Echo's own log (gommon) writes to os.Stdout by default, in its own JSON format, bypassing
	// slog: measured, when e.HTTPErrorHandler fails to write the error response (the client is
	// already gone) it leaves a line on stdout reading
	// {"time":…,"level":"ERROR","prefix":"echo",…,"message":"write: broken pipe"}.
	// Route it into slog, keeping echo's default level (ERROR and above), raised to DEBUG while
	// Debug is on.
	e.Logger.SetHeader("${level}")
	e.Logger.SetOutput(echoLog{})
	// Echo itself raises gommon's level to DEBUG from e.Debug only inside e.Start / e.StartTLS
	// (configureServer / configureTLSServer in echo.go, v4.16.0). e.Start is not used here, so
	// set it directly; otherwise Debug still shows ERROR only, and echo's one log line below
	// ERROR ("response already committed" in response.go, WARN) stays invisible.
	if c.Debug {
		e.Logger.SetLevel(gommonlog.DEBUG)
	}
	// e.StdLogger is what echo prepares for http.Server.ErrorLog (e.Start uses it that way), and
	// echo.New binds it to e.Logger's output at that moment, which is os.Stdout (measured on
	// v4.16.0); replacing the Logger's output above does not reach it. Route it where the
	// server's real ErrorLog goes.
	e.StdLogger = web.ErrorLog("xecho")
}

// echoLog forwards echo's own log to slog, one line at a time.
//
// It goes with SetHeader("${level}"): every line gommon writes is "level message", the level is
// mapped to slog's, and the message goes into the message field.
type echoLog struct{}

func (echoLog) Write(p []byte) (int, error) {
	level, msg, _ := strings.Cut(strings.TrimSpace(string(p)), " ")
	slog.Log(context.Background(), echoLevel(level), "echo internal log", "message", msg)
	return len(p), nil
}

// echoLevel maps gommon's level names to slog's levels. PANIC / FATAL are recorded as ERROR:
// gommon panics or exits the process right after them anyway, and slog's job is only to record
// the line.
func echoLevel(s string) slog.Level {
	switch s {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN":
		return slog.LevelWarn
	case "ERROR", "PANIC", "FATAL":
		return slog.LevelError
	}
	return slog.LevelInfo // INFO, and the Print family (whose level is -)
}

// enter sits outermost in the Pre chain and prepares two things for the built-in middleware: the
// request's original ctx (middleware that finds it only writes into echo.Context and leaves the
// request alone, see reqctx), and whether the directly connected peer is trusted (Trace /
// Propagate decide from it whether to accept forwarded headers, see markTrustedPeer).
//
// c.Set creates echo.Context's store, once per request; every later c.Set uses it.
func (x *XEcho) enter(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		c.Set(reqctx.Key, c.Request().Context())
		x.markTrustedPeer(c)
		return next(c)
	}
}

// attachContext runs after routing and before the caller's middleware: it swaps the ctx built in
// Pre onto the request, through the usual r.WithContext copy.
//
// When the caller replaced the request's ctx in e.Pre, the values that layer put there are lost
// here: two ctx trees cannot be merged, and what is preserved here is the span and the log scope.
func attachContext(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if ctx, ok := c.Get(reqctx.Key).(context.Context); ok && ctx != c.Request().Context() {
			c.SetRequest(c.Request().WithContext(ctx))
		}
		return next(c)
	}
}

// markTrustedPeer leaves a mark for the tracing middleware when the directly connected peer is in
// TrustedProxies: only such a peer's forwarded headers (XTrace.ForwardHeaders) are accepted.
//
// Forwarding means "pass what upstream gave us on to downstream". Accepting it from everyone lets
// a public client send X-Tenant-Id / X-Internal-Token and have it treated as coming from one of
// us, carried into every internal call. Who counts as one of us comes from the trusted-proxy
// table, with no switch of its own — one thing should be stated in one place.
//
// What is judged is the directly connected peer (RemoteAddr), not c.RealIP(): the latter is
// precisely what those forgeable headers produce.
func (x *XEcho) markTrustedPeer(c echo.Context) {
	if x.trusted.Trusts(web.RemoteIP(c.Request())) {
		c.Set(peer.TrustedKey, true)
	}
}

// cfg is the config this instance should use: the one given to WithConfig, or the XEcho block of
// the config file. When it is invalid it returns the defaults and that error, see build.
func (x *XEcho) cfg() (Config, error) {
	if x.override == nil {
		return fileConfig()
	}
	if err := x.override.Validate(); err != nil {
		return DefaultConfig(), fmt.Errorf("invalid config passed to WithConfig: %w", err)
	}
	return *x.override, nil
}

// fileConfig is the XEcho block of the config file, with unset fields left at their defaults.
//
// It may be called any time before Start: the config file is loaded the first time it is read, so
// what comes back is always the final values. xconfig.Unmarshal calls Validate after decoding;
// when it cannot decode, or the result is invalid, it returns the defaults and that error.
func fileConfig() (Config, error) {
	c := DefaultConfig()
	if err := xconfig.Unmarshal(ConfigKey, &c); err != nil {
		return DefaultConfig(), err
	}
	return c, nil
}

// Start starts the server and blocks until it stops. Called by xone.Run.
//
// It assembles first if that has not happened yet. The listen address, the timeouts, TLS and the
// middleware and trusted proxies on echo all come from one config — the one taken at assembly
// time, which is not re-read here. When it is invalid, Start returns the error immediately and
// never listens.
func (x *XEcho) Start(ctx context.Context) error {
	x.build()
	if x.confErr != nil {
		return xerror.New("xecho", "config", x.confErr)
	}
	return x.server.Start("xecho", x.conf.server(), x.engine)
}

// Stop shuts the server down gracefully: it waits for in-flight requests, up to the ctx's
// deadline. Called by xone.Run.
//
// How long it waits comes from the ctx alone; there is no separate limit here. Inside xone.Run
// the ctx carries the server's share of the stop budget (2/3 of WithStopTimeout, 10s by default):
// the whole shutdown has that one budget and the framework decides how much the server gets, so
// there is no second timeout to keep aligned with it.
//
// Used on its own, outside xone.Run, the deadline is whatever ctx you pass:
//
//	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
//	defer cancel()
//	err := x.Stop(ctx)
//
// A ctx with no deadline waits until every in-flight request is done — a stuck request then keeps
// Stop from returning at all.
//
// When the deadline passes with requests still running, it force-closes every connection, waits
// for the handlers to return, and returns an error.
//
// What the framework guarantees: when it returns nil, every handler has returned; when the
// deadline passes with handlers still running, the error says how many are left. What it cannot
// guarantee is stopping a handler that ignores the request ctx — Go has no way to terminate a
// goroutine from outside, and closing the connection and cancelling the request's ctx is already
// everything available. Slow work inside a handler (a query, a downstream call) has to be given
// c.Request().Context() to be stoppable once the client is gone.
func (x *XEcho) Stop(ctx context.Context) error { return x.server.Stop(ctx, "xecho") }

// ---- Registration ----

// This package registers no stop hook: the server itself is started and stopped by the caller
// through xone.Run, and the framework has exactly one Runnable — that slot belongs to the caller.
func init() {
	xhook.BeforeStart(loadConfig, xhook.At(xhook.StageServer))
}

// loadConfig reads the XEcho block once during startup: it claims the block, and makes a
// misconfigured value fail right there.
//
// What it reads is not stored: XEcho reads it again during assembly (so does CurrentConfig), and
// gets the same final values. Without this step nobody would read the block until the server
// starts — which is after the framework checks for unclaimed blocks, so it would be taken for a
// misspelled key and fail startup; and when every server in the process uses WithConfig, nobody
// would ever read it. A misconfigured value would also wait until Start to be reported.
func loadConfig(context.Context) error {
	_, err := fileConfig()
	return err
}
