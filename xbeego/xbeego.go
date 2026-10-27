package xbeego

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync"

	beeweb "github.com/beego/beego/v2/server/web"
	beectx "github.com/beego/beego/v2/server/web/context"

	"github.com/jupiter-eng/xone/internal/web"
	"github.com/jupiter-eng/xone/xbeego/internal/reqstate"
	"github.com/jupiter-eng/xone/xbeego/middleware"
	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xhook"
	"github.com/jupiter-eng/xone/xmetric"

	// Using xbeego means tracing is there: xtrace installs the global TracerProvider and
	// Propagator, and the middleware reaches them through otel's global API. To go without
	// tracing, set XTrace.Enable: false.
	_ "github.com/jupiter-eng/xone/xtrace"

	// Using a web framework means xlog is there: the access log and per-request fields
	// (xlog.AddKV) both rely on it. The root package does not pull it in, so a program using only
	// xhook and xgorm does not have its logging taken over.
	_ "github.com/jupiter-eng/xone/xlog"
)

// XBeego is an HTTP server waiting to be started, serving the routes registered on beego's global
// web.BeeApp.
//
// It satisfies xone.Runnable, so it goes straight to xone.Run:
//
//	xone.MustRun(xbeego.New())
//
// Note that the root package is not imported here -- Go's interfaces are structural, matching
// methods are enough. That keeps "an integration does not depend on the framework" true at compile
// time.
type XBeego struct {
	override *Config // see WithConfig

	routes  []func(*beeweb.HttpServer)
	extra   []func(http.Handler) http.Handler
	recover middleware.RecoverFunc

	// The fields below are written once by build and read-only afterwards. Whoever reads them
	// either called build first or is a handler on the server -- and the handler is only reachable
	// through build. Once guarantees the write happens before the reads, so no lock is needed.
	buildOnce sync.Once
	handler   http.Handler
	conf      Config      // the config assembly used, and the one Start uses
	confErr   error       // the error when the config is invalid; conf is then the defaults
	trusted   web.Proxies // the ranges parsed out of TrustedProxies

	// server is the half that has nothing to do with beego: listening and graceful shutdown, see
	// web.Server. beego's web.Run is not used -- the reasoning is in this package's doc comment.
	server web.Server
}

// New creates an XBeego.
//
// Switches, port and timeouts live in the XBeego block of the config file; a second server in the
// same process uses WithConfig. Nothing is read here and no global is touched -- the config is
// taken, and beego bound to, at assembly time (Handler or Start). The zero value &XBeego{} is
// equivalent to New().
func New() *XBeego { return &XBeego{} }

// WithConfig starts the server from this config instead of reading the XBeego block of the config
// file.
//
// Use it for a second server in one process -- a public API on one port and an internal admin
// endpoint on another, say. Without it both instances read the same block and can only listen on
// the same port. Both still serve the same routes: beego has one global router, see this package's
// doc comment.
//
// What it takes is a complete config, not a patch: to change a couple of fields on top of the
// file's, start from CurrentConfig (or from DefaultConfig when writing one from scratch, since
// zero timeouts are invalid):
//
//	c := xbeego.CurrentConfig()
//	c.Host, c.Port = "127.0.0.1", 9090
//	admin := xbeego.New().WithConfig(c)
//
// It is validated during assembly; when it is invalid, Start returns the error and never listens.
func (x *XBeego) WithConfig(c Config) *XBeego {
	x.override = &c
	return x
}

// CurrentConfig returns a copy of the XBeego block of the config file, and may be called any time
// before Start.
//
// Use it together with WithConfig, see the example there. The config file is loaded the first time
// it is read, so even a call in main before xone.Run gets the final values.
//
// When that block is invalid it returns the defaults: the error is reported by xone.Run at
// startup, and is not reported a second time here.
func CurrentConfig() Config {
	c, _ := fileConfig()
	return c
}

// WithRoutes registers routes. It may be called several times and takes effect in call order.
//
// It is optional: a beego application registers its routes on the global web.BeeApp from its
// routers package, usually in an init, and those are served whether this is called or not. Use it
// for a registration that has to happen at a defined moment instead -- after the configuration has
// been read, and before the server starts:
//
//	xbeego.New().WithRoutes(func(*web.HttpServer) { routers.Setup() })
//
// The callback gets web.BeeApp itself, so web.Router, web.InsertFilter and the package-level
// functions that forward to it are all equivalent to using the argument.
//
// One registration does not belong here on a second instance: web.InsertFilterChain. Beego folds
// filter chains into its router in Handlers.Init(), which is not idempotent and so runs once per
// process, during the first assembly -- a chain registered from a later instance's callback is
// never folded in and silently does not run. Register those from an init instead.
func (x *XBeego) WithRoutes(f ...func(*beeweb.HttpServer)) *XBeego {
	x.routes = append(x.routes, f...)
	return x
}

// WithMiddleware appends custom middleware, after all the built-in middleware and outside beego.
//
// What it takes is net/http's own shape, which is beego's web.MiddleWare as well, so middleware
// written for web.RunWithMiddleWares moves over unchanged. It runs before beego's router, so the
// route template and beego's context are not available there yet; a filter registered with
// web.InsertFilter is the place for anything that needs them.
//
// It applies to every request, including the framework's MetricPath -- an endpoint a caller
// believes to be behind their shared authentication must not be the one exception.
func (x *XBeego) WithMiddleware(m ...func(http.Handler) http.Handler) *XBeego {
	x.extra = append(x.extra, m...)
	return x
}

// WithRecoverFunc customizes the response after a panic.
//
// The default is a bare 500 with no body. It is called only while the response has not started
// going out: changing the status code afterwards would yield nothing but a truncated response.
// It governs the response alone -- the log line, the masking and the stack are the framework's and
// are written either way.
//
// It applies to both places a panic is caught: beego's own recovery slot, which XBeego.RecoverPanic
// takes over, and the framework's backstop outside beego. It is per instance although that slot is
// one per process -- the answer travels on the request, see reqstate.State.Recover.
func (x *XBeego) WithRecoverFunc(f middleware.RecoverFunc) *XBeego {
	x.recover = f
	return x
}

// App returns beego's global web.HttpServer, for anything this package does not cover.
//
// Calling it triggers assembly, from the config given to WithConfig or from the XBeego block of
// the config file -- which is loaded the first time it is read, so even a call before xone.Run
// gets the final values. Assembly happens once: WithConfig / WithRoutes / WithMiddleware /
// WithRecoverFunc have no effect afterwards, and Start reuses that same config.
//
// Do not call its Run: the server is started by Start, which owns the timeouts, TLS and graceful
// shutdown.
func (x *XBeego) App() *beeweb.HttpServer {
	x.build()
	return beeweb.BeeApp
}

// Handler returns the whole request chain: the built-in middleware, the caller's, and beego's
// router innermost. It is what Start serves, and what a test can drive with httptest.
//
// Calling it triggers assembly, see App.
func (x *XBeego) Handler() http.Handler {
	x.build()
	return x.handler
}

// bindOnce guards the three things that are done to beego's globals once per process rather than
// once per XBeego: the filter that reports the route template, the adapter name beego's logs
// package knows slog by, and the recovery beego had before any of this.
//
// Per instance the first two would be done twice with two servers in one process -- the filter
// would run twice per request, and logs.Register panics outright on a repeated name.
var bindOnce sync.Once

// beegoRecover is whatever stood in beego's recovery slot before xbeego ever wrote it: beego's own
// defaultRecoverPanic, or one the application installed. It is the only code that can render an
// error handler registered under a name, see renderThroughBeego.
//
// It is taken inside bindOnce rather than where the slot is written so that it is never an earlier
// XBeego's: the slot is written once per process today, but a test resetting that is enough to
// chain one onto another. Written inside the Once and read after it, which orders the two.
var beegoRecover func(*beectx.Context, *beeweb.Config)

// build assembles the handler chain, once per instance.
//
// Once rather than a boolean flag: with a flag, concurrent calls register the middleware twice,
// which shows up as two log lines per request and doubled metrics.
//
// The order matters: read the config, apply what is beego's own, let the caller register routes,
// and only then freeze beego's filter chain and wrap the middleware around it.
func (x *XBeego) build() {
	x.buildOnce.Do(func() {
		// Assemble even when the config is invalid, from the defaults: a caller of Handler() has to
		// get a handler one way or another, and the defaults are the safe side. The error is kept
		// for Start to return; the warning is for callers who only use Handler() and never call
		// Start, who would otherwise get the defaults with nothing to show for it.
		c, err := x.cfg()
		if err != nil {
			slog.Warn("xbeego invalid config, assembling the handler with defaults; Start will return the error", "error", err)
		}
		x.conf, x.confErr = c, err
		x.trusted = web.ParseProxies(c.TrustedProxies)

		bindOnce.Do(func() {
			beegoRecover = beeweb.BConfig.RecoverFunc
			registerBeeLogAdapter()
			// The route template is written during routing and read outside beego, so it travels
			// on the request state. The standard middleware chain wraps the router from outside
			// and runs before it, so a filter is the only way to reach the template at all.
			//
			// BeforeExec rather than FinishRouter, although both run after the template is known
			// (server/web/router.go writes it immediately before the BeforeExec filters): a panic
			// inside the handler unwinds straight to beego's recovery and the later positions never
			// run, so with FinishRouter every panicking request was recorded as unmatched -- route
			// and span would lose exactly the requests worth finding.
			//
			// WithReturnOnOutput(false) is required rather than tidy: a filter left at the default
			// is skipped as soon as anything has been written, which a filter earlier in the chain
			// may well have done.
			beeweb.InsertFilter("/*", beeweb.BeforeExec, captureRoute, beeweb.WithReturnOnOutput(false))
		})
		settleBeegoGlobals(c)

		for _, f := range x.routes {
			f(beeweb.BeeApp)
		}
		// What web.Run does right before it listens: fold the filter chains registered with
		// InsertFilterChain into the chain root. Without it they are silently not run. It is not
		// idempotent -- each call wraps the root once more -- so it goes inside bindOnce's
		// neighbourhood: one process, one call.
		initOnce.Do(beeweb.BeeApp.Handlers.Init)

		// The onion, outermost first:
		//   Capture -> LogScope -> Trace -> Log -> Metric -> Recover -> the caller's middleware ->
		//   MetricPath -> beego
		//
		// Recover has to be the innermost of the built-ins: whichever layer catches a panic, the
		// code after next.ServeHTTP in every layer inside it never runs. Innermost, the outer layers
		// still get to finish their work (record the metric, write the access log).
		//
		// Assembled inside out, so the slice below reads in the order a request travels.
		h := http.Handler(beeweb.BeeApp.Handlers)
		if c.Metric {
			// A route of the application's own on the same path is reported rather than shadowed:
			// the endpoint is answered in front of beego's router, so beego would never see that
			// request and nothing would say so. It becomes a config error naming the field, the
			// same one xgin and xecho report (web.MetricPathTaken).
			if metricPathTaken(c.MetricPath) {
				x.confErr = errors.Join(x.confErr, web.MetricPathTaken(ConfigKey, c.MetricPath))
				slog.Warn("xbeego the application already has a route on MetricPath, the metrics endpoint was not installed; Start will return the error",
					"path", c.MetricPath)
			} else {
				// Answered before beego's router sees it: the endpoint then exists whatever the
				// application's routes are, and no beego controller has to be written for it.
				h = serveMetrics(c.MetricPath, h)
			}
		}
		chain := []func(http.Handler) http.Handler{middleware.Capture(x.trusted)}
		if c.Log {
			chain = append(chain, middleware.LogScope())
		}
		// Trace governs the span only: with it off, the upstream trace identifiers and forwarded
		// headers are still accepted, there is simply no span.
		if c.Trace {
			chain = append(chain, skipMetrics(c, middleware.Trace()))
		} else {
			chain = append(chain, middleware.Propagate())
		}
		if c.Log {
			skip := slices.Clone(c.LogSkipPaths)
			if c.Metric {
				// The metrics endpoint is polled every few seconds by the scraper; logging it is
				// pure noise.
				skip = append(skip, c.MetricPath)
			}
			chain = append(chain, middleware.Log(
				middleware.WithSkipPaths(skip...),
				middleware.WithBody(c.LogRequestBody, c.LogResponseBody),
				middleware.WithQuery(c.LogQuery),
				middleware.WithHeaders(c.LogRequestHeaders, c.LogResponseHeaders),
			))
		}
		if c.Metric {
			chain = append(chain, middleware.Metric())
		}
		chain = append(chain, middleware.Recover(x.recover))
		chain = append(chain, x.extra...)

		for i := len(chain) - 1; i >= 0; i-- {
			h = chain[i](h)
		}
		x.handler = h
	})
}

// initOnce guards beego's Handlers.Init, see build.
var initOnce sync.Once

// applyOnce guards the two beego globals this module's configuration writes, see
// settleBeegoGlobals. It is kept apart from bindOnce because the reasons differ: what bindOnce
// holds cannot be done twice at all (logs.Register panics on a repeated name), while these two are
// simply settled by whichever assembly gets there first.
var applyOnce sync.Once

// beegoGlobals is the pair of switches that land on beego's process-wide globals.
type beegoGlobals struct {
	recoverPanic bool
	beegoLogs    bool
}

// settled is what the first assembly wrote onto beego's globals, for a later one to compare
// against. It is written inside applyOnce and read after it, so the Once orders the two.
var settled beegoGlobals

// settleBeegoGlobals lands RecoverPanic and BeegoLogs on beego's process-wide globals, once per
// process.
//
// Once rather than per instance because both are read while requests are being served: beego reads
// BConfig.RecoverFunc on every request (p.cfg.RecoverFunc in server/web/router.go) and its logs
// package walks its adapter list without holding a lock (BeeLogger.writeToLoggers against
// BeeLogger.Reset, core/logs/log.go). A second instance assembling while the first one already
// serves would write both from another goroutine, which the race detector reports as what it is.
//
// So the first assembly settles them, and a later instance that disagrees is told its own values
// were ignored rather than having them taken silently. In practice the two agree: the recipe for a
// second server starts from CurrentConfig(), which is a copy of the same block.
func settleBeegoGlobals(c Config) {
	applyOnce.Do(func() {
		applyConfig(c)
		if c.BeegoLogs {
			useBeeLogAdapter()
		}
		settled = beegoGlobals{recoverPanic: c.RecoverPanic, beegoLogs: c.BeegoLogs}
	})
	if settled.recoverPanic != c.RecoverPanic || settled.beegoLogs != c.BeegoLogs {
		slog.Warn("xbeego RecoverPanic and BeegoLogs are process-wide and were settled by the first assembly; this instance's own values are ignored",
			"recover_panic", settled.recoverPanic, "beego_logs", settled.beegoLogs,
			"ignored_recover_panic", c.RecoverPanic, "ignored_beego_logs", c.BeegoLogs)
	}
}

// captureRoute is the filter at web.BeforeExec: it copies the route template beego matched onto
// the request state, where the middleware outside beego reads it.
//
// beego writes it into the request's data store during routing
// (ctx.Input.SetData("RouterPattern", ...) in server/web/router.go). The request it is written on
// is the one the middleware handed in, ctx carrying the state, so the pointer found here is the
// one those layers hold.
//
// What it cannot reach: a request that ended before beego matched a route (404, 405, a filter at
// BeforeRouter answering it) has no template to copy and is recorded as unmatched, which is
// right -- and one that another BeforeExec filter registered earlier answered on the spot, which
// is recorded as unmatched although a route had been matched. Filters that answer requests
// normally sit at BeforeRouter, where no template exists yet either way.
//
// Nothing happens when there is no state: the filter stays registered for the life of the process
// while an XBeego may not be the one serving this request.
func captureRoute(ctx *beectx.Context) {
	s := reqstate.From(ctx.Request.Context())
	if s == nil {
		return
	}
	if pattern, ok := ctx.Input.GetData("RouterPattern").(string); ok {
		s.Route = pattern
	}
}

// metricPathTaken reports whether the application's own routes already hold path.
//
// It is asked of beego's global web.BeeApp rather than of anything this instance owns: that is
// where an application registers from its own init, and where this module's routes live too (see
// this package's doc comment).
//
// What is compared is the route template, not whether a request for path would match one: a
// pattern such as /:page does match MetricPath, and failing startup for it would be failing for a
// route that was never meant to be this endpoint. Such a route stays shadowed, as it was before.
func metricPathTaken(path string) bool {
	return slices.ContainsFunc(beeweb.BeeApp.Handlers.GetAllControllerInfo(),
		func(i *beeweb.ControllerInfo) bool { return i.GetPattern() == path })
}

// serveMetrics answers MetricPath and hands everything else to next.
//
// The handler is fetched per request rather than fixed at assembly time: assembly can happen before
// xmetric is initialized (calling Handler() before xone.Run does exactly that), and the handler
// taken then belongs to the fallback registry, which would leave /metrics empty forever.
func serveMetrics(path string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == path {
			// Its own route template, which nothing else can fill in: beego never sees this
			// request, so without it every scrape is recorded as unmatched -- and that is the
			// bucket a dashboard watches for scanners and typos, at scrape frequency. xgin and
			// xecho register the endpoint as an ordinary route and get the same label from their
			// router.
			if st := reqstate.From(r.Context()); st != nil {
				st.Route = path
			}
			xmetric.Handler().ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// skipMetrics keeps the metrics endpoint out of m: the scraper polls it every few seconds, and a
// span per poll only floods the tracing backend -- the access log skips it for the same reason.
// The decision is made on the path, comparing URL.Path only, as the access log's SkipPaths does.
func skipMetrics(c Config, m func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	if !c.Metric {
		return m
	}
	return func(next http.Handler) http.Handler {
		h := m(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == c.MetricPath {
				next.ServeHTTP(w, r)
				return
			}
			h.ServeHTTP(w, r)
		})
	}
}

// applyConfig lands the config items that govern beego itself onto its globals.
//
// Beego keeps its settings in one process-wide web.BConfig, so these apply to every XBeego in the
// process; that is the same global the router is on, see this package's doc comment.
func applyConfig(c Config) {
	if !c.RecoverPanic {
		return
	}
	// Beego recovers panics inside its own router, which is where the stack still points at the
	// handler that panicked -- outside it, after the stack has unwound into the middleware, the
	// trace is worth far less. So the framework's recovery goes into that slot rather than relying
	// on the backstop in the middleware chain.
	//
	// The recovery that stood here is not thrown away: one case is still its own to answer, see
	// renderThroughBeego.
	prev := beegoRecover
	beeweb.BConfig.RecoverFunc = func(ctx *beectx.Context, cfg *beeweb.Config) {
		recovered := recover()
		if recovered == nil {
			return
		}
		// Beego stops a handler by panicking, and two of its three ways through that are control
		// flow rather than a failure. Both have to keep behaving as they did, or taking this slot
		// over would change what ordinary beego code does.
		//
		// The first: Controller.Abort / CustomAbort panic with ErrAbort once the response has been
		// written.
		if recovered == beeweb.ErrAbort {
			return
		}
		// The second: when an error handler is registered for the code (web.ErrorHandler), those
		// same two -- and Context.Abort always -- panic with the code as a string instead, for the
		// recovery to render that handler. c.Abort("404") is how a beego application answers a 404.
		// Treated as a crash it would answer 500 and write an ERROR line per request.
		//
		// Rendering it is the work of the recovery that stood in this slot, so the request goes
		// back to it: for a numeric code it writes what web.Exception would, the two sharing
		// beego's unexported exception(), and it is the only code that reaches a handler
		// registered under a name. What is exported answers the request only when it declines.
		if s, ok := recovered.(string); ok && cfg.EnableErrorsShow {
			if _, registered := beeweb.ErrorMaps[s]; registered {
				if renderThroughBeego(prev, ctx, cfg, recovered) {
					return
				}
				// It let the request through, which beego's own does when
				// web.BConfig.RecoverPanic is off. A numeric code is still rendered from out here.
				if code, err := strconv.ParseUint(s, 10, 64); err == nil {
					beeweb.Exception(code, ctx)
					return
				}
				// A name is not: web.Exception takes a numeric code and errorInfo's fields are
				// unexported, and web.ErrorController registers every method it finds that way
				// (ErrorDb becomes "Db"). So the handler is not rendered and the status beego
				// would answer goes out without its body. One WARN says so: it is beego control
				// flow all the same, and an ERROR with a stack would say a request had failed
				// while everything else said it had succeeded. A name is not a code, so
				// c.Abort("Db") leaves the status at 200 and the access log, the metrics and the
				// span all record a success.
				slog.WarnContext(ctx.Request.Context(),
					"xbeego an error handler registered under a name was not rendered, so the response carries no body; only beego's own recovery can reach it and it let this request through (recoverpanic off, say) -- register the handler under its status code instead",
					"code", s, "path", ctx.Request.URL.Path)
				// beegoStatus rather than the response the caller asked for through
				// WithRecoverFunc: that answers panics, and this is control flow. Answering it
				// with the failure response would put a 500 on the wire for a request nothing
				// else records as a failure.
				middleware.OnAbort(ctx.ResponseWriter, ctx.Request, recovered, beegoStatus(ctx))
				return
			}
		}
		middleware.OnPanic(ctx.ResponseWriter, ctx.Request, recovered, recoverResponse(ctx))
	}
}

// renderThroughBeego hands a panic back to the recovery that stood in beego's slot before xbeego
// took it over, and reports whether that recovery answered it.
//
// It is how a registered error handler is rendered, and the only way one registered under a name
// can be. Beego keeps them in web.ErrorMaps behind an unexported errorInfo and renders them
// through an unexported exception(), which does take a name; web.Exception, the exported door,
// takes a numeric code. So such a handler is out of reach from here, and in reach of the recovery
// beego calls, which is the one this hands the request back to.
//
// The value is re-panicked rather than passed, because a recovery in that slot takes it from a
// recover() of its own: beego calls the slot deferred (defer p.cfg.RecoverFunc(ctx, p.cfg) in
// server/web/router.go). One that does not take it -- beego's own re-panics when
// web.BConfig.RecoverPanic is off -- lets the value through to the recover() below, and false says
// so, for the caller to answer the request itself. Nothing is written in that case: beego's
// re-panic is the first thing it does.
func renderThroughBeego(recovery func(*beectx.Context, *beeweb.Config), ctx *beectx.Context, cfg *beeweb.Config, recovered any) (rendered bool) {
	if recovery == nil {
		return false
	}
	defer func() {
		if recover() != nil {
			rendered = false
		}
	}()
	rethrow(recovery, ctx, cfg, recovered)
	return true
}

// rethrow panics with recovered so that recovery, deferred here, finds it in its own recover().
func rethrow(recovery func(*beectx.Context, *beeweb.Config), ctx *beectx.Context, cfg *beeweb.Config, recovered any) {
	defer recovery(ctx, cfg)
	panic(recovered)
}

// recoverResponse is how a panic inside beego is answered: what the caller asked for through
// WithRecoverFunc, or what beego's own recovery would write.
func recoverResponse(ctx *beectx.Context) middleware.RecoverFunc {
	if st := reqstate.From(ctx.Request.Context()); st != nil && st.Recover != nil {
		return st.Recover
	}
	return beegoStatus(ctx)
}

// beegoStatus answers the way beego's own recovery would: with ctx.Output.Status when a handler
// set one and 500 otherwise (the tail of defaultRecoverPanic in server/web/config.go).
// Context.Abort(404, body) sets it and then panics, so a bare 500 here would turn every such abort
// into a 500 -- which is why this is not simply the package default.
func beegoStatus(ctx *beectx.Context) middleware.RecoverFunc {
	return func(w http.ResponseWriter, _ *http.Request, _ any) {
		code := ctx.Output.Status
		if code == 0 {
			code = http.StatusInternalServerError
		}
		w.WriteHeader(code)
	}
}

// cfg is the config this instance should use: the one given to WithConfig, or the XBeego block of
// the config file. When it is invalid it returns the defaults and that error, see build.
func (x *XBeego) cfg() (Config, error) {
	if x.override == nil {
		return fileConfig()
	}
	if err := x.override.Validate(); err != nil {
		return DefaultConfig(), fmt.Errorf("invalid config passed to WithConfig: %w", err)
	}
	return *x.override, nil
}

// fileConfig is the XBeego block of the config file, with unset fields left at their defaults.
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
// middleware and trusted proxies all come from one config -- the one taken at assembly time, which
// is not re-read here. When it is invalid, Start returns the error immediately and never listens.
//
// beego's own web.Run is not called: the server here is internal/web.Server, the one xgin and xecho
// use, so the timeouts, h2c, TLS and the graceful shutdown behave identically on all three. What
// that leaves out is in this package's doc comment.
func (x *XBeego) Start(ctx context.Context) error {
	x.build()
	if x.confErr != nil {
		return xerror.New("xbeego", "config", x.confErr)
	}
	return x.server.Start("xbeego", x.conf.server(), x.handler)
}

// Stop shuts the server down gracefully: it waits for in-flight requests, up to the ctx's
// deadline. Called by xone.Run.
//
// How long it waits comes from the ctx alone; there is no separate limit here. Inside xone.Run the
// ctx carries the server's share of the stop budget (2/3 of WithStopTimeout, 10s by default): the
// whole shutdown has that one budget and the framework decides how much the server gets, so there
// is no second timeout to keep aligned with it.
//
// Used on its own, outside xone.Run, the deadline is whatever ctx you pass:
//
//	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
//	defer cancel()
//	err := x.Stop(ctx)
//
// A ctx with no deadline waits until every in-flight request is done -- a stuck request then keeps
// Stop from returning at all.
//
// When the deadline passes with requests still running, it force-closes every connection, waits for
// the handlers to return, and returns an error.
//
// What the framework guarantees: when it returns nil, every handler has returned; when the deadline
// passes with handlers still running, the error says how many are left. What it cannot guarantee is
// stopping a handler that ignores the request ctx -- Go has no way to terminate a goroutine from
// outside, and closing the connection and cancelling the request's ctx is already everything
// available. Slow work inside a beego controller has to be given c.Ctx.Request.Context() to be
// stoppable once the client is gone.
func (x *XBeego) Stop(ctx context.Context) error { return x.server.Stop(ctx, "xbeego") }

// ClientIP is the address the framework recorded as this request's client_ip.
//
// Business code should read this rather than beego's ctx.Input.IP(): the latter walks
// X-Forwarded-For from the left and believes whoever sent it, so a client reaching the service
// directly can make it say anything. In a controller:
//
//	ip := xbeego.ClientIP(c.Ctx.Request)
//
// It is the same value the access log, the metrics and the span go by, computed from
// XBeego.TrustedProxies by the algorithm xgin and xecho use.
func ClientIP(r *http.Request) string { return middleware.ClientIP(r) }

// ---- Registration ----

// This package registers no stop hook: the server itself is started and stopped by the caller
// through xone.Run, and the framework has exactly one Runnable -- that slot belongs to the caller.
func init() {
	xhook.BeforeStart(loadConfig, xhook.At(xhook.StageServer))
}

// loadConfig reads the XBeego block once during startup: it claims the block, and makes a
// misconfigured value fail right there.
//
// What it reads is not stored: XBeego reads it again during assembly (so does CurrentConfig), and
// gets the same final values. Without this step nobody would read the block until the server
// starts -- which is after the framework checks for unclaimed blocks, so it would be taken for a
// misspelled key and fail startup; and when every server in the process uses WithConfig, nobody
// would ever read it. A misconfigured value would also wait until Start to be reported.
func loadConfig(context.Context) error {
	_, err := fileConfig()
	return err
}
