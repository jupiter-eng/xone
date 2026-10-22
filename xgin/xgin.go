package xgin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/jupiter-eng/xone/internal/web"
	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xgin/internal/peer"
	"github.com/jupiter-eng/xone/xgin/middleware"
	"github.com/jupiter-eng/xone/xgin/trans"
	"github.com/jupiter-eng/xone/xhook"
	"github.com/jupiter-eng/xone/xmetric"

	// Using xgin means tracing is there: xtrace installs the global TracerProvider and Propagator,
	// and the middleware reaches them through otel's global API. To go without tracing, set
	// XTrace.Enable: false.
	_ "github.com/jupiter-eng/xone/xtrace"

	// Using a web framework means xlog is there: the access log and per-request fields
	// (xlog.AddKV) both rely on it. The root package does not pull it in, so a program using only
	// xhook and xgorm does not have its logging taken over.
	_ "github.com/jupiter-eng/xone/xlog"
)

// XGin is an HTTP server waiting to be started.
//
// It satisfies xone.Runnable, so it goes straight to xone.Run:
//
//	xone.MustRun(xgin.New().WithRoutes(register))
//
// Note that the root package is not imported here — Go's interfaces are structural, matching
// methods are enough. That keeps "an integration does not depend on the framework" true at
// compile time.
type XGin struct {
	override *Config // see WithConfig

	routes  []func(*gin.Engine)
	extra   []gin.HandlerFunc
	recover gin.RecoveryFunc

	// The fields below are written once by build and read-only afterwards. Whoever reads them
	// either called build first or is a handler on the engine — and the engine is only reachable
	// through build. Once guarantees the write happens before the reads, so no lock is needed.
	buildOnce sync.Once
	engine    *gin.Engine
	conf      Config      // the config assembly used, and the one Start uses
	confErr   error       // the error when the config is invalid; conf is then the defaults
	trusted   web.Proxies // the ranges parsed out of TrustedProxies, see markTrustedPeer

	// server is the half that has nothing to do with gin: listening and graceful shutdown, see
	// web.Server.
	server web.Server
}

// New creates an XGin.
//
// Switches, port and timeouts live in the XGin block of the config file; a second server in the
// same process uses WithConfig. Nothing is read here — the config is taken at assembly time
// (Engine or Start). The zero value &XGin{} is equivalent to New().
func New() *XGin { return &XGin{} }

// WithConfig starts the server from this config instead of reading the XGin block of the config
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
//	c := xgin.CurrentConfig()
//	c.Host, c.Port = "127.0.0.1", 9090
//	admin := xgin.New().WithConfig(c).WithRoutes(adminRoutes)
//
// It is validated during assembly; when it is invalid, Start returns the error and never listens.
func (g *XGin) WithConfig(c Config) *XGin {
	g.override = &c
	return g
}

// CurrentConfig returns a copy of the XGin block of the config file, and may be called any time
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
// The callback gets the native engine and may set anything. It runs after the config has been
// applied to the engine, so settings in the callback win over the config: changing
// MaxMultipartMemory or calling SetTrustedProxies is taken from the callback.
//
// One thing does not follow: which peers are trusted for forwarded headers
// (XTrace.ForwardHeaders) comes only from TrustedProxies in the config. Gin does not expose its
// current proxy list, so a SetTrustedProxies in the callback only governs ClientIP. To move both,
// change the config.
func (g *XGin) WithRoutes(f ...func(*gin.Engine)) *XGin {
	g.routes = append(g.routes, f...)
	return g
}

// WithMiddleware appends custom middleware, after all the built-in middleware.
func (g *XGin) WithMiddleware(m ...gin.HandlerFunc) *XGin {
	g.extra = append(g.extra, m...)
	return g
}

// WithRecoverFunc customizes the response after a panic. The default is a 500.
func (g *XGin) WithRecoverFunc(f gin.RecoveryFunc) *XGin {
	g.recover = f
	return g
}

// Engine returns the native *gin.Engine, for anything this package does not cover.
//
// Calling it triggers assembly, from the config given to WithConfig or from the XGin block of the
// config file — which is loaded the first time it is read, so even a call before xone.Run gets
// the final values. Assembly happens once: WithConfig / WithRoutes / WithMiddleware /
// WithRecoverFunc have no effect afterwards, and Start reuses that same config.
//
// When the config is invalid it still returns an engine, assembled from the defaults (trusting
// only private-range proxies and an 8MB multipart threshold, both the safe side), and logs a
// warning. The error itself is returned by Start, so xone.Run reports it at startup.
func (g *XGin) Engine() *gin.Engine {
	g.build()
	return g.engine
}

// build assembles the engine, once per instance.
//
// Once rather than a boolean flag: with a flag, concurrent calls register the middleware twice,
// which shows up as two log lines per request and doubled metrics.
//
// The order matters: set the Mode, then create the engine and apply the config, then mount the
// middleware and the built-in routes, and only then run the caller's WithRoutes — which is why
// settings in the callback win over the config, see WithRoutes.
func (g *XGin) build() {
	g.buildOnce.Do(func() {
		// Assemble even when the config is invalid, from the defaults: a caller of Engine() has to
		// get an engine one way or another, and the defaults are the safe side. The error is kept for
		// Start to return; the warning is for callers who only use Engine() and never call Start, who
		// would otherwise get the defaults with nothing to show for it.
		c, err := g.cfg()
		if err != nil {
			slog.Warn("xgin invalid config, assembling the engine with defaults; Start will return the error", "error", err)
		}
		g.conf, g.confErr = c, err

		// Set before gin.New: in debug mode gin.New prints a warning about running in debug mode and
		// switching to release in production, and then one line per route registered afterwards
		// (measured on v1.12.0). Creating first and setting after would print that warning even for a
		// server configured as release.
		//
		// Note that gin.SetMode is process-wide rather than per instance: with two servers started
		// from WithConfig with different Modes in one process, whichever assembles last decides. Gin
		// has no per-instance Mode and nothing here can isolate it, so keep their Modes identical.
		gin.SetMode(c.Mode)
		e := gin.New()
		e.HandleMethodNotAllowed = true // without it, a wrong method answers 404 rather than 405
		// A *gin.Context is itself a context.Context and business code often passes it directly:
		// xlog.AddKV(c, ...), db.WithContext(c), otel.Tracer(...).Start(c, ...). Without this option
		// its Value only looks at c.Keys, Done returns nil and Err is always nil (hasRequestContext in
		// context.go, v1.12.0): the log scope and the parent span are both lost, and a client that
		// left cannot cancel anything. With it, all of those go to c.Request.Context(), exactly as
		// passing c.Request.Context() would.
		e.ContextWithFallback = true
		applyConfig(e, c)
		g.trusted = web.ParseProxies(c.TrustedProxies)

		// The onion, outermost first:
		//   LogScope -> Trace -> Log -> Metric -> Recover -> the caller's middleware -> handler
		//
		// Recover has to be the innermost of the built-ins: whichever layer catches a panic, the code
		// after c.Next() in every layer inside it never runs. Innermost, the outer layers still get to
		// finish their work (record the metric, write the access log).
		if c.Log {
			e.Use(middleware.LogScope())
		}
		// Decide whether the peer is trusted first, so Trace / Propagate know whether to accept
		// forwarded headers and baggage. Trace governs the span only: with it off, the upstream trace
		// identifiers and forwarded headers are still accepted, there is simply no span.
		if c.Trace {
			e.Use(g.markTrustedPeer, skipMetrics(c, middleware.Trace()))
		} else {
			e.Use(g.markTrustedPeer, middleware.Propagate())
		}
		if c.Log {
			skip := slices.Clone(c.LogSkipPaths)
			if c.Metric {
				// The metrics endpoint is polled every few seconds by the scraper; logging it is pure noise.
				skip = append(skip, c.MetricPath)
			}
			e.Use(middleware.Log(
				middleware.WithSkipPaths(skip...),
				middleware.WithBody(c.LogRequestBody, c.LogResponseBody),
				middleware.WithQuery(c.LogQuery),
				middleware.WithHeaders(c.LogRequestHeaders, c.LogResponseHeaders),
			))
		}
		if c.Metric {
			e.Use(middleware.Metric())
		}
		e.Use(middleware.Recover(g.recover))
		e.Use(g.extra...)

		// Routes are always registered after every middleware: gin fixes the handler chain at the
		// moment a route is registered, and middleware added with Use afterwards does not apply to
		// it.
		//
		// Gin writes its default 404 / 405 body only after the whole middleware chain has run
		// (serveError in gin.go, gin v1.12.0), so it is unwritten when the access log records
		// bytes_out: measured, 0 was recorded while the client received 18 bytes. Writing the same
		// content inside the chain keeps the response identical byte for byte and makes the count
		// recordable. Placed before the caller's routes: a NoRoute in WithRoutes still wins.
		e.NoRoute(notFound)
		e.NoMethod(methodNotAllowed)
		for _, f := range g.routes {
			f(e)
		}

		// The metrics endpoint goes on last, after the caller's routes, so that a caller's own
		// route on the same path is seen rather than run into: registered first, gin panics
		// ("handlers are already registered for path") from inside the caller's routing code, with
		// nothing in the message pointing at MetricPath. Here it becomes a config error that names
		// the field, the same one xecho and xbeego report (web.MetricPathTaken).
		//
		// Registering last costs nothing and keeps what matters: gin takes the Use chain as it
		// stands at that moment, and every Use above has already happened. The endpoint used to be
		// registered before g.extra, where the shared auth a caller mounted with WithMiddleware
		// applied to business routes and not to /metrics — an endpoint believed to be protected
		// was in fact open.
		if c.Metric {
			if metricPathTaken(e, c.MetricPath) {
				g.confErr = errors.Join(g.confErr, web.MetricPathTaken(ConfigKey, c.MetricPath))
				slog.Warn("xgin the application already has a route on MetricPath, the metrics endpoint was not registered; Start will return the error",
					"path", c.MetricPath)
			} else {
				e.GET(c.MetricPath, serveMetrics)
			}
		}

		if c.ZHTranslations {
			if err := trans.RegisterZH(); err != nil {
				slog.Warn("xgin failed to register the zh validation translator", "error", err)
			}
		}

		g.engine = e
	})
}

// metricPathTaken reports whether the caller's own routes already hold path.
//
// The method is not compared, only the route template: the access log and the tracing leave
// MetricPath out by path alone (SkipPaths, skipMetrics), so a caller's POST on that path would be
// just as invisible as a GET, and gin would answer the scraper's GET with a 405 besides.
func metricPathTaken(e *gin.Engine, path string) bool {
	return slices.ContainsFunc(e.Routes(), func(r gin.RouteInfo) bool { return r.Path == path })
}

// serveMetrics is the metrics endpoint. The handler is fetched per request rather than fixed at
// assembly time: assembly can happen before xmetric is initialized (calling Engine() before
// xone.Run does exactly that), and the handler taken then belongs to the fallback registry, which
// would leave /metrics empty forever.
func serveMetrics(c *gin.Context) { xmetric.Handler().ServeHTTP(c.Writer, c.Request) }

// skipMetrics keeps the metrics endpoint out of h: the scraper polls it every few seconds, and a
// span per poll only floods the tracing backend — the access log skips it for the same reason.
// Gin fixes its handler chain when routes are registered and cannot drop one middleware for one
// route, so the decision is made inside the middleware on the path, comparing URL.Path only, as
// the access log's SkipPaths does.
func skipMetrics(c Config, h gin.HandlerFunc) gin.HandlerFunc {
	if !c.Metric {
		return h
	}
	return func(ctx *gin.Context) {
		if ctx.Request.URL.Path == c.MetricPath {
			ctx.Next()
			return
		}
		h(ctx)
	}
}

// applyConfig lands the config fields that govern the engine onto e.
func applyConfig(e *gin.Engine, c Config) {
	e.MaxMultipartMemory = c.MaxMultipartMemory

	// Gin enables RedirectTrailingSlash by default: when /users/ does not match /users it answers
	// 301 (GET) or 307 (other methods) outright, without running the middleware chain at all — no
	// access log, no metrics, no trace (measured on v1.12.0, handleHTTPRequest in gin.go). With it
	// off, such a path goes to NoRoute like any other unmatched path and is recorded as a 404
	// unmatched. To redirect, set e.RedirectTrailingSlash = true back inside WithRoutes (a
	// redirected request still bypasses the middleware). RedirectFixedPath is already false in
	// gin.New and needs nothing.
	e.RedirectTrailingSlash = false

	// Trust only private ranges by default. Gin's default is to trust everyone, so anyone sending
	// an X-Forwarded-For decides what client_ip reads in the access log. A malformed range is
	// already rejected in Validate, and an error here falls back to trusting nobody.
	if err := e.SetTrustedProxies(web.ExpandProxies(c.TrustedProxies)); err != nil {
		slog.Warn("xgin invalid TrustedProxies, trusting none", "error", err)
		_ = e.SetTrustedProxies([]string{})
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
// What is judged is the directly connected peer (RemoteAddr), not ClientIP: the latter is
// precisely what those forgeable headers produce.
func (g *XGin) markTrustedPeer(c *gin.Context) {
	if g.trusted.Trusts(c.RemoteIP()) {
		c.Set(peer.TrustedKey, true)
	}
	c.Next()
}

// cfg is the config this instance should use: the one given to WithConfig, or the XGin block of
// the config file. When it is invalid it returns the defaults and that error, see build.
func (g *XGin) cfg() (Config, error) {
	if g.override == nil {
		return fileConfig()
	}
	if err := g.override.Validate(); err != nil {
		return DefaultConfig(), fmt.Errorf("invalid config passed to WithConfig: %w", err)
	}
	return *g.override, nil
}

// fileConfig is the XGin block of the config file, with unset fields left at their defaults.
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
// middleware and trusted proxies on the engine all come from one config — the one taken at
// assembly time, which is not re-read here. When it is invalid, Start returns the error
// immediately and never listens.
func (g *XGin) Start(ctx context.Context) error {
	g.build()
	if g.confErr != nil {
		return xerror.New("xgin", "config", g.confErr)
	}
	c := g.conf
	// The engine itself is handed over, not g.engine.Handler(): when the caller turned on gin's own
	// UseH2C in a callback, the latter wraps x/net's h2c.NewHandler, the connection is hijacked and
	// graceful shutdown never waits for it (the reasoning is under protocols in web.Server). To use
	// h2c, turn on XGin.UseH2C, which goes through the standard library.
	return g.server.Start("xgin", c.server(), g.engine)
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
//	err := g.Stop(ctx)
//
// A ctx with no deadline waits until every in-flight request is done (measured, a 1.5s request
// made Shutdown return after 1.57s) — a stuck request then keeps Stop from returning at all.
//
// When the deadline passes with requests still running, it force-closes every connection, waits
// for the handlers to return, and returns an error.
//
// What the framework guarantees: when it returns nil, every handler has returned; when the
// deadline passes with handlers still running, the error says how many are left. What it cannot
// guarantee is stopping a handler that ignores the request ctx — Go has no way to terminate a
// goroutine from outside, and closing the connection and cancelling the request's ctx is already
// everything available. Slow work inside a handler (a query, a downstream call) has to be given
// c.Request.Context() to be stoppable once the client is gone.
func (g *XGin) Stop(ctx context.Context) error { return g.server.Stop(ctx, "xgin") }

// ---- Registration ----

// This package registers no stop hook: the server itself is started and stopped by the caller
// through xone.Run, and the framework has exactly one Runnable — that slot belongs to the caller.
func init() {
	xhook.BeforeStart(loadConfig, xhook.At(xhook.StageServer))
}

// loadConfig reads the XGin block once during startup: it claims the block, and makes a
// misconfigured value fail right there.
//
// What it reads is not stored: XGin reads it again during assembly (so does CurrentConfig), and
// gets the same final values. Without this step nobody would read the block until the server
// starts — which is after the framework checks for unclaimed blocks, so it would be taken for a
// misspelled key and fail startup; and when every server in the process uses WithConfig, nobody
// would ever read it. A misconfigured value would also wait until Start to be reported.
func loadConfig(context.Context) error {
	_, err := fileConfig()
	return err
}

// notFound / methodNotAllowed write exactly what gin's default responses do: Content-Type
// text/plain, with the body identical byte for byte.
func notFound(c *gin.Context) {
	c.Data(http.StatusNotFound, gin.MIMEPlain, []byte("404 page not found"))
}

func methodNotAllowed(c *gin.Context) {
	c.Data(http.StatusMethodNotAllowed, gin.MIMEPlain, []byte("405 method not allowed"))
}
