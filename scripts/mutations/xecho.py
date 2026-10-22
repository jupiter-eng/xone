# Mutations for xecho: the promises of the xecho module. The format is in scripts/mutations/__init__.py.
#
# Starting and stopping the server, TLS, proxy ranges and the access log fields are implemented in
# internal/web, and their own mutations live in xgin.py and core.py; what is mutated here are
# xecho's call sites (whether the config was handed over, whether the middleware is wired) plus the
# few things specific to echo (errors rendered only after the chain returns, a template handed out
# for 405, c.RealIP() believing everyone, gommon writing to stdout).
from . import cut, mutate, section, swap

section("HTTP server")
# Call site: XEcho's Start / Stop only forward to web.Server. Without the forward, the server keeps
# running after the stop signal arrives.
mutate("XEcho.Stop forwards to web.Server", "xecho/xecho.go", "./xecho", "TestStop",
       swap('{ return x.server.Stop(ctx, "xecho") }', '{ return nil }'))
# The listen address, TLS, the middleware on echo and the trusted proxies must come from one config
mutate("XEcho.Start uses the config from assembly time", "xecho/xecho.go", "./xecho", "TestStart_UsesConfigFromBuildTime",
       swap('\treturn x.server.Start("xecho", x.conf.server(), x.engine)\n', '\tc, _ := x.cfg()\n\treturn x.server.Start("xecho", c.server(), x.engine)\n'))
# All four timeouts on echo's e.Server are 0: they have to be copied from the config into the
# http.Server web.Server builds
mutate("ReadHeaderTimeout reaches web.Server", "xecho/config.go", "./xecho", "TestStart_ServerTimeoutsFromConfig",
       swap('\t\tReadHeaderTimeout: c.ReadHeaderTimeout,\n', '\t\tReadHeaderTimeout: c.IdleTimeout,\n'))
mutate("XEcho UseH2C reaches web.Server", "xecho/config.go", "./xecho", "TestStop_H2C",
       swap('\t\tUseH2C:            c.UseH2C,\n', ''))
mutate("XEcho ClientCAFile reaches web.Server", "xecho/config.go", "./xecho", "TestStart_ClientCAFileEnablesMutualTLS",
       swap('\t\tClientCAFile:      c.TLS.ClientCAFile,\n', ''))
mutate("XEcho MinVersion reaches web.Server", "xecho/config.go", "./xecho", "TestStart_MinVersion",
       swap('\t\tMinVersion:        c.TLS.MinVersion,\n', '\t\tMinVersion:        "1.2",\n'))
# Call site: the rules live in internal/web, and XEcho's Validate has to actually reach them
mutate("XEcho Validate checks the port and TLS", "xecho/config.go", "./xecho", "TestValidate",
       swap('\tif err := s.ValidateListen(); err != nil {\n\t\treturn err\n\t}\n', ''))
mutate("XEcho Validate checks the timeouts", "xecho/config.go", "./xecho", "TestValidate",
       swap('\tif err := s.ValidateTimeouts(); err != nil {\n\t\treturn err\n\t}\n', ''))
mutate("XEcho Validate checks the proxy ranges", "xecho/config.go", "./xecho", "TestValidate",
       swap('\tif err := web.ValidateProxies(c.TrustedProxies); err != nil {\n\t\treturn err\n\t}\n', ''))
# Echo does not reject a path without a leading slash, it rewrites it quietly: empty mounts the
# metrics on the root path, where the home page replaces them
mutate("a metrics path without a leading slash fails startup", "xecho/config.go", "./xecho", "TestValidate|TestLoadConfig",
       swap('if c.Metric && !strings.HasPrefix(c.MetricPath, "/") {', 'if false && !strings.HasPrefix(c.MetricPath, "/") {'))
# The XEcho block of the config file is claimed and validated by the StageServer hook. Dropping that
# error makes a misconfigured value wait until Start
mutate("a misconfigured XEcho block fails during startup", "xecho/xecho.go", "./xecho", "TestLoadConfig",
       swap('\t_, err := fileConfig()\n\treturn err\n', '\t_, _ = fileConfig()\n\treturn nil\n'))
# A half-decoded invalid config may well have TrustedProxies as 0.0.0.0/0
mutate("an invalid XEcho block falls back to the defaults", "xecho/xecho.go", "./xecho", "TestCurrentConfig|TestEngine_InvalidConfigFallsBackToSafeDefaults",
       swap('\tif err := xconfig.Unmarshal(ConfigKey, &c); err != nil {\n\t\treturn DefaultConfig(), err\n',
            '\tif err := xconfig.Unmarshal(ConfigKey, &c); err != nil {\n\t\treturn c, err\n'))
mutate("the config given to WithConfig is validated too", "xecho/xecho.go", "./xecho", "TestStart_DoesNotListenOnInvalidConfig",
       swap('\tif err := x.override.Validate(); err != nil {', '\tif err := x.override.Validate(); false && err != nil {'))

section("errors rendered in the middleware")
# Echo renders the error only after the whole chain returns: the middleware sees Status 200 and
# Size 0. All three render in their own layer (finish), and every call site is mutated - whichever
# falls back to returning the error as is records nothing but 200s
mutate("the access log records the rendered status code", "xecho/middleware/log.go", "./xecho", "TestLog_ErrorStatus",
       swap('\t\t\tfinish(c, ctx, next(c))\n\t\t\treturn nil\n', '\t\t\t_ = ctx\n\t\t\treturn next(c)\n'))
mutate("the metrics record the rendered status code", "xecho/middleware/metric.go", "./xecho", "TestMetric_ReturnedErrorRecordedWithRenderedStatus",
       swap('\t\t\tfinish(c, ctx, next(c))\n\t\t\treturn nil\n', '\t\t\t_ = ctx\n\t\t\treturn next(c)\n'))
mutate("the span records the rendered status code", "xecho/middleware/trace.go", "./xecho", "TestTrace_OnlyServerErrorsMarkedAsError|TestTrace_ReturnedError",
       swap('\t\t\tfinish(c, ctx, next(c))\n\t\t\treturn nil\n', '\t\t\treturn next(c)\n'))
# Returning the error after rendering makes echo call HTTPErrorHandler again: a custom error handler
# then writes the error response twice
mutate("the error response is rendered once", "xecho/middleware/metric.go", "./xecho", "TestFinish_HTTPErrorHandlerRunsOnce",
       swap('\t\t\tfinish(c, ctx, next(c))\n\t\t\treturn nil\n', '\t\t\terr := next(c)\n\t\t\tfinish(c, ctx, err)\n\t\t\treturn err\n'))
# The outer layers receive nil: without recording the error on c, the access log's errors and the
# span's echo.errors are both empty
mutate("rendering records the error on the request", "xecho/middleware/middleware.go", "./xecho", "TestLog_ErrorStatus|TestTrace_ReturnedError",
       swap('\tc.Set(errKey, err)\n\tc.Error(err)\n', '\tc.Error(err)\n'))
mutate("the access log carries the error returned by the handler", "xecho/middleware/log.go", "./xecho", "TestLog_ErrorStatus",
       swap('\t\t\t\t\ta.Errors = err.Error()\n', ''))
mutate("the span carries the error returned by the handler", "xecho/middleware/trace.go", "./xecho", "TestTrace_ReturnedError",
       swap('span.SetAttributes(attribute.String("echo.errors", web.RedactText(err.Error())))', '_ = err'))

section("route labels")
# The value comes from routeOf, shared by the three middlewares; the first two break it directly and
# the last three mutate one call site each
mutate("XEcho records an unmatched route as unmatched", "xecho/middleware/middleware.go", "./xecho",
       "TestLog_UnmatchedRouteIsUnmatched|TestMetric_UnmatchedRouteUsesFixedValue|TestTrace_UnmatchedRouteUsesFixedValue",
       swap('\tif p == "" || c.Get(echo.ContextKeyHeaderAllow) != nil {\n\t\treturn web.RouteUnmatched\n',
            '\tif p == "" || c.Get(echo.ContextKeyHeaderAllow) != nil {\n\t\treturn c.Request().URL.Path\n'))
# Echo hands out that route's template on a 405: recording it would not match xgin (which records
# unmatched)
mutate("a wrong method (405) is recorded as unmatched", "xecho/middleware/middleware.go", "./xecho",
       "TestMetric_UnmatchedRouteUsesFixedValue|TestTrace_UnmatchedRouteUsesFixedValue|TestLog_ErrorStatus",
       swap(' || c.Get(echo.ContextKeyHeaderAllow) != nil', ''))
# For a group with middleware, echo quietly registers RouteNotFound fallbacks (<prefix>, <prefix>/*):
# anything landing on them used to be recorded as /api/v1/*
mutate("landing on a RouteNotFound fallback is recorded as unmatched", "xecho/middleware/middleware.go", "./xecho",
       "TestLog_ErrorStatus|TestMetric_UnmatchedRouteUsesFixedValue|TestTrace_UnmatchedRouteUsesFixedValue",
       swap('if methods, ok := rs.fallback[p]; ok && !methods[c.Request().Method] {', 'if methods, ok := rs.fallback[p]; false && ok && !methods[c.Request().Method] {'))
mutate("the fallback table matches on RouteNotFound", "xecho/middleware/middleware.go", "./xecho", "TestLog_ErrorStatus",
       swap('\t\tif r.Method == echo.RouteNotFound {\n', '\t\tif r.Method == "" {\n'))
# A fallback and a real route can share a template (GET("/*") proxying inside a group): the method
# hitting the real route still records the template
mutate("a method registered on a fallback template keeps the template", "xecho/middleware/middleware.go", "./xecho", "TestLog_FallbackTemplateKeptForMethodsRegisteredOnIt",
       swap('if methods, ok := rs.fallback[p]; ok && !methods[c.Request().Method] {', 'if _, ok := rs.fallback[p]; ok {'))
mutate("the XEcho access log takes its route from routeOf", "xecho/middleware/log.go", "./xecho", "TestLog_UnmatchedRouteIsUnmatched",
       swap('Route:      rs.of(c),', 'Route:      c.Path(),'), swap('\tvar rs routes\n', '\tvar _ routes\n'))
mutate("the XEcho metrics take their route from routeOf", "xecho/middleware/metric.go", "./xecho", "TestMetric_UnmatchedRouteUsesFixedValue",
       swap('route, method, code := rs.of(c),', 'route, method, code := c.Path(),'), swap('\t\trs      routes\n', '\t\t_       routes\n'))
mutate("the XEcho span takes its route from routeOf", "xecho/middleware/trace.go", "./xecho", "TestTrace_UnmatchedRouteUsesFixedValue",
       swap('route, st := rs.of(c), status(c)', 'route, st := c.Path(), status(c)'), swap('\tvar rs routes\n', '\tvar _ routes\n'))
# The built-in middleware sits on Pre, and the router has not run when the span starts: the name
# gains the route template on the way out
mutate("the XEcho span name gains the route template on the way out", "xecho/middleware/trace.go", "./xecho", "TestTrace_UnmatchedRouteUsesFixedValue|TestBuild_RouteTemplateKnownWithBuiltinsInPre",
       swap('\t\t\t\tspan.SetName(method + " " + route)\n', ''))
mutate("the XEcho metrics method label is folded", "xecho/middleware/metric.go", "./xecho", "TestMetric_CustomMethod",
       swap('web.NormalizeMethod(c.Request().Method)', 'c.Request().Method'), swap('\t\trs      routes\n\t)\n', '\t\trs      routes\n\t)\n\t_ = web.NormalizeMethod\n'))
mutate("the XEcho span method is folded", "xecho/middleware/trace.go", "./xecho", "TestTrace_CustomMethod",
       swap('method := web.NormalizeMethod(r.Method)', 'method := r.Method'))

section("panic recovery")
# Echo does not catch panics by default: the connection is simply broken and the stack goes to stderr
mutate("XEcho installs Recover", "xecho/xecho.go", "./xecho", "TestBuild_PanicRecovered",
       swap('\t\te.Pre(middleware.Recover(x.recover))\n', ''))
# With Recover further out, a panic passing through Log / Metric leaves them recording 200
mutate("Recover is the innermost built-in", "xecho/xecho.go", "./xecho", "TestBuild_PanicRecoveredAndStillLogged",
       swap('\t\te.Pre(middleware.Recover(x.recover))\n', ''),
       swap('\t\tif c.Log {\n\t\t\te.Pre(middleware.LogScope())', '\t\te.Pre(middleware.Recover(x.recover))\n\t\tif c.Log {\n\t\t\te.Pre(middleware.LogScope())'))
mutate("user middleware sits inside Recover", "xecho/xecho.go", "./xecho", "TestBuild_UserMiddlewareInsideRecover",
       swap('\t\te.Pre(middleware.Recover(x.recover))\n', '\t\te.Pre(x.extra...)\n\t\te.Pre(middleware.Recover(x.recover))\n'),
       swap('\t\te.Use(x.extra...)\n', ''))
mutate("WithRecoverFunc reaches Recover", "xecho/xecho.go", "./xecho", "TestWithRecoverFunc",
       swap('e.Pre(middleware.Recover(x.recover))', 'e.Pre(middleware.Recover(nil))'))
mutate("the default recover answers echo's 500", "xecho/middleware/middleware.go", "./xecho", "TestRecover_CatchesPanicAndReturns500|TestBuild_PanicRecovered",
       swap('return echo.ErrInternalServerError }', 'return nil }'))
# ErrAbortHandler is the agreed way to say "drop this connection". Catching it turns a response that
# was meant to be aborted into a 500 going out
mutate("XEcho re-panics ErrAbortHandler to net/http", "xecho/middleware/middleware.go", "./xecho", "TestRecover",
       swap('\t\t\t\tif r == http.ErrAbortHandler {\n', '\t\t\t\tif r == http.ErrAbortHandler && false {\n'))
mutate("XEcho records a request aborted with ErrAbortHandler", "xecho/middleware/middleware.go", "./xecho",
       "TestLog_ErrAbortHandlerAbortLoggedAs499|TestMetric_ErrAbortHandlerAbortRecordedAs499|TestTrace_ErrAbortHandlerAbortRecordedAsError",
       swap('\t\t\t\t\tc.Set(abortedKey, true)\n', ''))
mutate("the XEcho access log records an aborted request as 499", "xecho/middleware/log.go", "./xecho", "TestLog_ErrAbortHandler",
       swap('Status:     status(c),', 'Status:     c.Response().Status,'))
mutate("the XEcho metrics record an aborted request as 499", "xecho/middleware/metric.go", "./xecho", "TestMetric_ErrAbortHandler",
       swap('strconv.Itoa(status(c))', 'strconv.Itoa(c.Response().Status)'))
mutate("XEcho tracing marks an aborted request as an error", "xecho/middleware/trace.go", "./xecho", "TestTrace_ErrAbortHandler",
       swap('route, st := rs.of(c), status(c)', 'route, st := rs.of(c), c.Response().Status'))
mutate("XEcho prints no stack for a broken connection", "xecho/middleware/middleware.go", "./xecho", "TestRecover_BrokenPipe",
       swap('if web.IsBrokenPipe(r) {', 'if false {'))
# Once the response has started going out, calling the recover function only appends what it writes
# to a truncated response
mutate("the recover function is not called once the response started", "xecho/middleware/middleware.go", "./xecho", "TestRecover_CustomFuncNotCalledOnceResponseStarted",
       swap('\t\t\t\tif c.Response().Committed {', '\t\t\t\tif false {'))

section("client_ip and the forwarding trust boundary")
# Echo's default (IPExtractor nil) believes whoever sends X-Forwarded-For
mutate("client_ip is computed from TrustedProxies", "xecho/xecho.go", "./xecho", "TestBuild_TrustsOnlyPrivateProxiesByDefault",
       swap('\te.IPExtractor = web.ParseProxies(c.TrustedProxies).ClientIP\n', '\t_ = web.ParseProxies(c.TrustedProxies).ClientIP\n'))
mutate("the trusted ranges for client_ip come from the config", "xecho/xecho.go", "./xecho", "TestBuild_TrustedProxiesEmptyListTrustsNone|TestBuild_ForwardedHeadersOnlyWithProxiesConfigured|TestLoadConfig_EmptyTrustedProxies",
       swap('e.IPExtractor = web.ParseProxies(c.TrustedProxies).ClientIP', 'e.IPExtractor = web.ParseProxies([]string{web.TrustPrivate}).ClientIP'))
# The rule itself is in internal/web (with its own mutations in core.py); this checks that xecho
# really uses it: a public peer cannot forge it
mutate("a public peer's X-Forwarded-For is not believed", "internal/web/proxy.go", "./xecho", "TestBuild_TrustsOnlyPrivateProxiesByDefault",
       swap('\tif ps.contains(remote) {\n', '\tif true {\n'))
mutate("TrustedProxies defaults to private", "xecho/config.go", "./xecho", "TestBuild_TrustsOnlyPrivateProxiesByDefault|TestBuild_PassthroughHeadersOnlyFromPrivateByDefault",
       swap('\t\tTrustedProxies:    []string{web.TrustPrivate},\n', ''))
# The callback runs after the config has landed on echo, so an IPExtractor replaced there wins.
# Two ways to break it: land the config after the callback, or apply it again in Start
mutate("echo settings in the callback win over the config", "xecho/xecho.go", "./xecho", "TestBuild_CallbackEngineSettingsOverrideConfig",
       swap('\t\tapplyConfig(e, c)\n', ''),
       swap('\t\tfor _, f := range x.routes {\n\t\t\tf(e)\n\t\t}\n', '\t\tfor _, f := range x.routes {\n\t\t\tf(e)\n\t\t}\n\t\tapplyConfig(e, c)\n'))
mutate("Start does not apply the config a second time", "xecho/xecho.go", "./xecho", "TestBuild_CallbackEngineSettingsOverrideConfig",
       swap('\treturn x.server.Start("xecho", x.conf.server(), x.engine)\n', '\tapplyConfig(x.engine, x.conf)\n\treturn x.server.Start("xecho", x.conf.server(), x.engine)\n'))
# "Who is one of us" comes from TrustedProxies alone and looks at the direct hop alone. Get the mark
# wrong once and either forged headers reach the internal network or forwarding stops working
mutate("XEcho trusts a peer only when it is in TrustedProxies", "xecho/xecho.go", "./xecho", "TestBuild_PassthroughHeadersOnlyFromTrustedProxies|TestBuild_PassthroughHeadersOnlyFromPrivateByDefault",
       swap('if x.trusted.Trusts(web.RemoteIP(c.Request())) {', 'if len(x.trusted) > 0 {'))
mutate("the trust check uses the direct peer, not c.RealIP()", "xecho/xecho.go", "./xecho", "TestBuild_PassthroughHeadersOnlyFromTrustedProxies|TestBuild_PassthroughHeadersOnlyFromPrivateByDefault",
       swap('if x.trusted.Trusts(web.RemoteIP(c.Request())) {', 'if x.trusted.Trusts(c.RealIP()) {'))
mutate("the XEcho trusted ranges come from the config at assembly time", "xecho/xecho.go", "./xecho", "TestBuild_PassthroughHeadersOnlyFromTrustedProxies",
       swap('x.trusted = web.ParseProxies(c.TrustedProxies)', 'x.trusted = web.ParseProxies(nil)'))
mutate("XEcho decides peer trust outermost in the Pre chain", "xecho/xecho.go", "./xecho", "TestBuild_PassthroughHeadersOnlyFromTrustedProxies|TestBuild_TraceDisabledOnlySkipsSpan_StillPropagates",
       swap('\t\tx.markTrustedPeer(c)\n', ''))
# XEcho.Trace governs the span only: turning it off must not drop the upstream trace identifiers and
# forwarded headers with it
mutate("XEcho.Trace off still joins upstream tracing and forwarding", "xecho/xecho.go", "./xecho", "TestBuild_TraceDisabledOnlySkipsSpan_StillPropagates",
       swap('\t\t\te.Pre(middleware.Propagate())\n', ''))
mutate("XEcho Propagate hands the trust mark to xtrace too", "xecho/middleware/trace.go", "./xecho", "TestTrace_PeerTrustFollowsXechoMarker",
       swap('withContext(c, otel.GetTextMapPropagator().Extract(contextOf(c), inbound(c)))',
            'withContext(c, otel.GetTextMapPropagator().Extract(contextOf(c), propagation.HeaderCarrier(c.Request().Header)))'))
mutate("the XEcho tracing middleware hands the trust mark to xtrace", "xecho/middleware/trace.go", "./xecho", "TestTrace_PeerTrustFollowsXechoMarker",
       swap('\tif trusted, _ := c.Get(peer.TrustedKey).(bool); trusted {\n\t\treturn trustedCarrier{h}\n\t}\n', '\t_ = peer.TrustedKey\n'))

section("middleware and switches")
mutate("the XEcho access log switch reads the config", "xecho/xecho.go", "./xecho", "TestLog_", swap('\t\tif c.Log {\n', '\t\tif true {\n', 2))
mutate("the XEcho tracing switch reads the config", "xecho/xecho.go", "./xecho", "TestTrace_", swap('\t\tif c.Trace {\n', '\t\tif true {\n'))
mutate("XEcho mounts no metrics middleware with metrics off", "xecho/xecho.go", "./xecho", "TestMetric_",
       swap('\t\tif c.Metric {\n\t\t\te.Pre(middleware.Metric())', '\t\tif true {\n\t\t\te.Pre(middleware.Metric())'))
mutate("XEcho registers no endpoint with metrics off", "xecho/xecho.go", "./xecho", "TestBuild_NoMetricsEndpointWhenDisabled",
       swap('\t\tif c.Metric {\n\t\t\tif metricPathTaken(', '\t\tif true {\n\t\t\tif metricPathTaken('))
mutate("the XEcho metrics endpoint uses the configured path", "xecho/xecho.go", "./xecho", "TestBuild_MetricsPathConfigurable",
       swap('e.GET(c.MetricPath, serveMetrics)', 'e.GET("/metrics", serveMetrics)'))
# Echo replaces a route registered earlier without a word: the endpoint disappeared, and the path
# kept being left out of the access log and the tracing, which both go by MetricPath.
mutate("a route of the application's own on MetricPath fails startup", "xecho/xecho.go", "./xecho", "TestBuild_MetricPathTakenByTheApplicationFailsStartup",
       swap('if metricPathTaken(e, c.MetricPath) {', 'if false {'))
# e.Group(prefix, mw...) registers a RouteNotFound at the bare prefix, which is echo's own
# bookkeeping and not a route of the caller's -- read as one, a group named after MetricPath would
# fail startup for nothing.
mutate("echo's own group fallback is not a MetricPath collision", "xecho/xecho.go", "./xecho", "TestBuild_MetricPathUnderAGroupIsNotACollision",
       swap('return r.Path == path && r.Method != echo.RouteNotFound', 'return r.Path == path'))
# The metrics endpoint takes the handler per request: fixed at assembly time, before xmetric is
# initialized, it pins a fallback registry that stays empty forever
mutate("the XEcho metrics endpoint takes the current registry per request", "xecho/xecho.go", "./xecho", "TestBuild_MetricsEndpointOKWhenEngineFetchedFirst",
       swap('func serveMetrics(c echo.Context) error {\n\txmetric.Handler().ServeHTTP(c.Response(), c.Request())\n\treturn nil\n}',
            'var metricsHandler = xmetric.Handler()\n\nfunc serveMetrics(c echo.Context) error {\n\tmetricsHandler.ServeHTTP(c.Response(), c.Request())\n\treturn nil\n}'))
mutate("XEcho mounts the user middleware", "xecho/xecho.go", "./xecho", "TestBuild_UserMiddlewareAfterBuiltin|TestBuild_MetricsEndpointRunsUserMiddleware",
       swap('\t\te.Use(x.extra...)\n', ''))
mutate("the XEcho skipped log paths read the config", "xecho/xecho.go", "./xecho", "TestLogSkipPaths",
       swap('skip := slices.Clone(c.LogSkipPaths)', 'skip := slices.Clone([]string{})'))
mutate("the XEcho metrics endpoint is access-log skipped automatically", "xecho/xecho.go", "./xecho", "TestLogSkipPaths_MetricsPathAddedAutomatically",
       swap('\t\t\t\tskip = append(skip, c.MetricPath)\n', ''))
# Crossed, the response body (which may carry a token) reaches the log of someone who enabled the
# request body only
mutate("the XEcho request and response body switches are independent", "xecho/xecho.go", "./xecho", "TestLogBody",
       swap('middleware.WithBody(c.LogRequestBody, c.LogResponseBody)', 'middleware.WithBody(c.LogResponseBody, c.LogRequestBody)'))
mutate("the XEcho query string switch reads the config", "xecho/xecho.go", "./xecho", "TestLogQueryAndHeaders",
       swap('middleware.WithQuery(c.LogQuery)', 'middleware.WithQuery(true)'))
mutate("the XEcho request and response header switches are independent", "xecho/xecho.go", "./xecho", "TestLogQueryAndHeaders",
       swap('middleware.WithHeaders(c.LogRequestHeaders, c.LogResponseHeaders)', 'middleware.WithHeaders(c.LogResponseHeaders, c.LogRequestHeaders)'))
mutate("XEcho LogScope opens a per-request field scope", "xecho/middleware/middleware.go", "./xecho", "TestLogScope",
       swap('\t\t\twithContext(c, xlog.CtxWithScope(contextOf(c)))\n', '\t\t\t_ = xlog.CtxWithScope\n'))
# There is one word list per process (internal/web): without these two exported names forwarding to
# it, words the caller adds are honored by nobody
mutate("XEcho AddSensitiveFields writes to the shared word list", "xecho/middleware/redact.go", "./xecho", "TestAddSensitive",
       swap('{ web.AddSensitiveFields(fields...) }', '{}'))
mutate("XEcho AddSensitiveHeaders writes to the shared list", "xecho/middleware/redact.go", "./xecho", "TestAddSensitive",
       swap('{ web.AddSensitiveHeaders(headers...) }', '{}'))

section("access log values")
mutate("the XEcho access log gets the real response headers", "xecho/middleware/log.go", "./xecho", "TestLog_ResponseHeaders",
       swap('RespHeader: resp.Header(),', 'RespHeader: http.Header{},'))
mutate("the XEcho access log bytes_out is the bytes written", "xecho/middleware/log.go", "./xecho", "TestLog_RecordsRequestAndResponseSizes|TestLog_ErrorStatus",
       swap('BytesOut:   int(resp.Size),', 'BytesOut:   0,'))
# client_ip is c.RealIP(): computed by e.IPExtractor, including one replaced in a callback
mutate("the XEcho access log client_ip follows the IPExtractor", "xecho/middleware/log.go", "./xecho", "TestLog_ClientIPFollowsIPExtractor|TestBuild_TrustsOnlyPrivateProxiesByDefault",
       swap('ClientIP:   c.RealIP(),', 'ClientIP:   web.RemoteIP(c.Request()),'))
# http.ResponseController walks down through Unwrap: without the wrapper handing back the original
# writer, read and write deadlines and full duplex all return ErrNotSupported
mutate("the capturing writer does not block ResponseController", "xecho/middleware/log.go", "./xecho", "TestLog_ResponseCaptureKeepsResponseControllerFeatures",
       swap('func (w *captureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }', ''))
mutate("the capturing writer is swapped back before the request ends", "xecho/middleware/log.go", "./xecho", "TestLog_ResponseWriterRestoredAfterRequest",
       swap('\t\t\t\t\tresp.Writer = orig\n', ''))
mutate("XEcho captures only a prefix of the response body", "xecho/middleware/log.go", "./xecho", "TestLog_ResponseBodyTruncatedToPrefixOverLimit",
       swap('w.buf.Write(b[:min(len(b), maxResponseBody-w.buf.Len())])', 'w.buf.Write(b)'))

section("tracing")
# Using xecho gives tracing, with no need for the caller to remember importing xtrace
mutate("xecho gives tracing without importing xtrace", "xecho/xecho.go", "./xecho", "TestTracingWorksWithoutImportingXtrace",
       swap('\t_ "github.com/jupiter-eng/xone/xtrace"\n', ''))
mutate("the handler ctx carries the server span", "xecho/middleware/trace.go", "./xecho", "TestTrace_StartsSpanAndReturnsTraceID",
       swap('\t\t\twithContext(c, ctx)\n', '\t\t\t_ = ctx\n'))
mutate("XEcho echoes X-Trace-Id back", "xecho/middleware/trace.go", "./xecho", "TestTrace_StartsSpanAndReturnsTraceID|TestTrace_ReturnedError",
       swap('\t\t\t\tc.Response().Header().Set(TraceIDHeader, sc.TraceID().String())\n', ''))

section("echo's own log")
# Gommon writes to os.Stdout in its own JSON format by default: out of reach of the logging
# platform's search and alerts
mutate("echo's own log is routed into slog", "xecho/xecho.go", "./xecho", "TestEchoLogger_RoutedToSlog",
       swap('\te.Logger.SetOutput(echoLog{})\n', ''))
mutate("the echo log level is taken from the start of the line", "xecho/xecho.go", "./xecho", "TestEchoLogger_RoutedToSlog",
       swap('\te.Logger.SetHeader("${level}")\n', ''))

section("masking error text")
mutate("the access log errors field is masked (xecho)", "internal/web/accesslog.go", "./xecho", "TestLog_ErrorTextIsRedacted",
       swap('slog.String("errors", RedactText(a.Errors))', 'slog.String("errors", a.Errors)'))
mutate("echo.errors on the span is masked", "xecho/middleware/trace.go", "./xecho", "TestTrace_ErrorTextIsRedactedOnSpan",
       swap('web.RedactText(err.Error())', 'err.Error()'))

mutate("the panic value is masked (xecho)", "xecho/middleware/middleware.go", "./xecho", "TestRecover_PanicValueIsRedacted",
       swap('"error", web.RedactText(fmt.Sprint(r)),', '"error", fmt.Sprint(r),'))
# Echo's HTTPError folds Internal into Error(): code=502, message=..., internal=<the whole DSN the
# driver reported>
mutate("a DSN password in errors is masked (xecho)", "internal/web/redact.go", "./xecho", "TestLog_DSNPasswordInHTTPErrorIsMasked|TestTrace_DSNPasswordInErrorIsMaskedOnSpan",
       swap('\ts = urlUserinfo.ReplaceAllString(s, "$1:"+Redacted+"@")\n', ''))

section("the zero value and the startup log")
mutate("a zero value XEcho reports errors and logs under xecho", "xecho/xecho.go", "./xecho", "TestZeroValue_ReportsUnderModuleName",
       swap('x.server.Start("xecho", ', 'x.server.Start("", '))
mutate("listening is printed only after the port is bound (xecho)", "internal/web/server.go", "./xecho", "TestStart_NoListeningLogWhenListenFails",
       swap('\tln, err := listen(c, srv)\n', '\tslog.Info(module + " listening")\n\tln, err := listen(c, srv)\n'))
mutate("XEcho rejects an IPv4-mapped proxy spelling", "internal/web/proxy.go", "./xecho", "TestLoadConfig_IPv4MappedProxyFailsStartup",
       swap('\t\tif plain, ok := unmapped(p); ok {', '\t\tif plain, ok := unmapped(p); ok && false {'))

section("e.Pre")
# The built-in middleware used to sit on e.Use: requests rejected or redirected in Pre reached
# neither the access log, the metrics nor the trace, and a panic in Pre was caught by nobody
mutate("the access log is mounted on Pre", "xecho/xecho.go", "./xecho", "TestBuild_PreRejectionLoggedMeasuredAndTraced|TestBuild_PreRedirectLogged",
       swap('\t\t\te.Pre(middleware.Log(', '\t\t\te.Use(middleware.Log('))
mutate("the metrics are mounted on Pre", "xecho/xecho.go", "./xecho", "TestBuild_PreRejectionLoggedMeasuredAndTraced",
       swap('\t\t\te.Pre(middleware.Metric())', '\t\t\te.Use(middleware.Metric())'))
mutate("tracing is mounted on Pre", "xecho/xecho.go", "./xecho", "TestBuild_PreRejectionLoggedMeasuredAndTraced",
       swap('e.Pre(skipMetrics(c, middleware.Trace()))', 'e.Use(skipMetrics(c, middleware.Trace()))'))
mutate("Recover is mounted on Pre", "xecho/xecho.go", "./xecho", "TestBuild_PrePanicRecovered",
       swap('\t\te.Pre(middleware.Recover(x.recover))\n', '\t\te.Use(middleware.Recover(x.recover))\n'))
# The *http.Request handed in is never modified: echo matches on it, and so do the caller's Pre
# middlewares (MethodOverride, RemoveTrailingSlash, Rewrite). Pre only stores the ctx in
# echo.Context and it is swapped onto the request after routing, see reqctx
mutate("the request ctx is stored outermost in the Pre chain", "xecho/xecho.go", "./xecho", "TestBuild_PreRewritingMiddlewaresStillRoute",
       swap('\t\tc.Set(reqctx.Key, c.Request().Context())\n', ''))
mutate("Pre only stores the ctx and leaves the request alone", "xecho/middleware/middleware.go", "./xecho", "TestBuild_PreRewritingMiddlewaresStillRoute",
       swap('\tif _, deferred := c.Get(reqctx.Key).(context.Context); deferred {', '\tif false {'))
mutate("the ctx is swapped onto the request after routing", "xecho/xecho.go", "./xecho", "TestBuild_HandlerSeesSpanAndLogScope",
       swap('\t\te.Use(attachContext)\n', ''))
mutate("the ctx swap runs before the user middleware", "xecho/xecho.go", "./xecho", "TestBuild_HandlerSeesSpanAndLogScope",
       swap('\t\te.Use(attachContext)\n\t\te.Use(x.extra...)\n', '\t\te.Use(x.extra...)\n\t\te.Use(attachContext)\n'))
# For a request that ended inside Pre the ctx has not been swapped onto the request yet: the access
# log has to use the stored one to carry trace_id
mutate("the access log uses the stored ctx", "xecho/middleware/log.go", "./xecho", "TestBuild_PreRejectionAccessLogCarriesTraceID",
       swap('\t\t\t\t\tCtx:        contextOf(c),\n', ''))
# Used on its own, the middleware swaps in an r.WithContext copy: mutating in place would hand the
# caller of e.ServeHTTP back a modified request, and race with a handler passing it to another
# goroutine
mutate("middleware used on its own does not modify the request handed in", "xecho/middleware/middleware.go", "./xecho", "TestRequestUnchangedAfterServeHTTP",
       swap('\tc.SetRequest(c.Request().WithContext(ctx))\n}', '\tr := c.Request()\n\t*r = *r.WithContext(ctx)\n}'))

section("the 499 rule")
# Only panic(http.ErrAbortHandler) makes net/http break the connection; return an error wrapping it
# and the client receives a 500
mutate("a returned ErrAbortHandler is recorded as what was sent", "xecho/middleware/middleware.go", "./xecho", "TestReturnedErrAbortHandlerRecordsWhatWasSent",
       swap('\tv, _ := c.Get(abortedKey).(bool)\n\treturn v\n',
            '\tfor err := errorOf(c); err != nil; {\n\t\tif err == http.ErrAbortHandler {\n\t\t\treturn true\n\t\t}\n'
            '\t\tu, ok := err.(interface{ Unwrap() error })\n\t\tif !ok {\n\t\t\treturn false\n\t\t}\n\t\terr = u.Unwrap()\n\t}\n\treturn false\n'))
# The client left with nothing sent: return ctx.Err() used to be recorded as 500 with the span marked
mutate("XEcho records a client that left as 499", "xecho/middleware/middleware.go", "./xecho", "TestClientGoneBeforeAnythingSentRecordedAs499",
       swap('\tif !c.Response().Committed && web.ClientGone(ctx) {', '\tif false {'))
mutate("XEcho records what was sent once the response went out", "xecho/middleware/middleware.go", "./xecho", "TestClientGoneAfterResponseCommittedKeepsSentStatus",
       swap('\tif !c.Response().Committed && web.ClientGone(ctx) {', '\tif web.ClientGone(ctx) {'))
# c.Error writing to a broken connection still sets Committed: deciding after it records
# return ctx.Err() as a 500
mutate("the client-gone check runs before c.Error", "xecho/middleware/middleware.go", "./xecho", "TestClientGoneBeforeAnythingSentRecordedAs499",
       swap('\tif !c.Response().Committed && web.ClientGone(ctx) {\n\t\tc.Set(goneKey, true)\n\t}\n\tif err == nil {\n\t\treturn\n\t}\n\tc.Set(errKey, err)\n\tc.Error(err)\n',
            '\tif err != nil {\n\t\tc.Set(errKey, err)\n\t\tc.Error(err)\n\t}\n\tif !c.Response().Committed && web.ClientGone(ctx) {\n\t\tc.Set(goneKey, true)\n\t}\n'))
# What is judged is the ctx as this layer saw it on the way in: a ctx installed by an inner layer and
# cancelled on its way out reads as Canceled outside it while the client is still waiting
mutate("the XEcho metrics judge the client by the ctx on the way in", "xecho/middleware/metric.go", "./xecho", "TestClientGoneIgnoresContextsSetByInnerLayers|TestBuild_ContextTimeoutRecordedAsSent",
       swap('\t\t\tfinish(c, ctx, next(c))\n', '\t\t\terr := next(c)\n\t\t\tfinish(c, c.Request().Context(), err)\n\t\t\t_ = ctx\n'))
mutate("the XEcho access log judges the client by the ctx on the way in", "xecho/middleware/log.go", "./xecho", "TestClientGoneIgnoresContextsSetByInnerLayers|TestBuild_ContextTimeoutRecordedAsSent",
       swap('\t\t\tfinish(c, ctx, next(c))\n', '\t\t\terr := next(c)\n\t\t\tfinish(c, c.Request().Context(), err)\n\t\t\t_ = ctx\n'))
mutate("XEcho tracing judges the client by the ctx on the way in", "xecho/middleware/trace.go", "./xecho", "TestClientGoneIgnoresContextsSetByInnerLayers",
       swap('\t\t\tfinish(c, ctx, next(c))\n', '\t\t\terr := next(c)\n\t\t\tfinish(c, c.Request().Context(), err)\n'))
mutate("the span of a client that left is not marked as an error", "xecho/middleware/trace.go", "./xecho", "TestClientGoneBeforeAnythingSentRecordedAs499",
       swap('\t\t\t\tcase aborted(c):\n', '\t\t\t\tcase st == web.StatusAborted:\n'))

section("the capturing writer")
# The compression middleware sits inside the access log: what is captured is compressed bytes, which
# used to reach response_body as garbage
mutate("a compressed response body records no content (xecho)", "internal/web/accesslog.go", "./xecho", "TestLog_EncodedResponseBodyOmitted",
       swap('\t\t\tbody := encodedOmitted(a.RespHeader)\n', '\t\t\tbody := ""\n'))
# Code that type-asserts directly (older SSE and WebSocket libraries): with Unwrap alone the
# assertion fails
mutate("the capturing writer satisfies http.Flusher", "xecho/middleware/log.go", "./xecho", "TestLog_ResponseCaptureSatisfiesFlusherAndHijacker",
       swap('func (w *captureWriter) Flush() { _ = http.NewResponseController(w.ResponseWriter).Flush() }\n', ''))
mutate("the capturing writer's Flush reaches the connection", "xecho/middleware/log.go", "./xecho", "TestLog_ResponseCaptureFlushReachesClient",
       swap('func (w *captureWriter) Flush() { _ = http.NewResponseController(w.ResponseWriter).Flush() }\n', 'func (w *captureWriter) Flush() {}\n'))
mutate("the capturing writer's Hijack yields the raw connection", "xecho/middleware/log.go", "./xecho", "TestLog_ResponseCaptureHijack",
       swap('\treturn http.NewResponseController(w.ResponseWriter).Hijack()\n', '\treturn nil, nil, http.ErrNotSupported\n'))

section("the debug switch and the validator")
# The Debug from the config has to actually land on e.Debug: without it production stays on the safe
# side, but a test environment gains nothing from turning it on - "I turned Debug on and still see no
# error text" gets investigated as an echo problem for a long time
mutate("XEcho Debug lands on e.Debug", "xecho/xecho.go", "./xecho", "TestDebug_PutsErrorDetailInResponse|TestDebug_PrettyPrintsJSON",
       swap('\te.Debug = c.Debug\n', ''))
# Echo raises gommon's level from e.Debug only inside e.Start, which xecho does not use, so this step
# has to be done here. Swapping in ERROR (echo.New's default) rather than deleting the line: deleting
# it leaves the gommonlog import unused and the package does not compile
mutate("with Debug on, echo's own log is raised to DEBUG", "xecho/xecho.go", "./xecho", "TestDebug_RaisesEchoLoggerLevel",
       swap('e.Logger.SetLevel(gommonlog.DEBUG)', 'e.Logger.SetLevel(gommonlog.ERROR)'))
# Call site: the validator WithValidator accepted has to be installed on e.Validator. Without it
# c.Validate returns ErrValidatorNotRegistered - a service that "installed a validator" never
# validated a single body
mutate("the WithValidator validator is installed on e.Validator", "xecho/xecho.go", "./xecho", "TestWithValidator_HandlerCanValidate",
       swap('\t\t\te.Validator = x.validator\n', ''))

section("the standard library log")
# e.StdLogger is bound to os.Stdout back in echo.New, and replacing e.Logger's output does not reach it
mutate("e.StdLogger is routed into slog", "xecho/xecho.go", "./xecho", "TestEchoStdLogger_RoutedToSlog",
       swap('\te.StdLogger = web.ErrorLog("xecho")\n', ''))

section("tracing, Recover and stopping")
# Call site: scraping /metrics opens a span every time
mutate("a metrics scrape opens no span (xecho)", "xecho/xecho.go", "./xecho", "TestTrace_MetricsScrapeNotTraced",
       swap('skipMetrics(c, middleware.Trace())', 'middleware.Trace()'))
mutate("a broken connection is logged as WARN (xecho)", "xecho/middleware/middleware.go", "./xecho", "TestRecover_BrokenPipe",
       swap('slog.WarnContext(ctx, "connection broken"', 'slog.ErrorContext(ctx, "connection broken"'))
# Starting and stopping is shared with xgin through web.Server; this checks xecho goes through it too:
# a handler holding a hijacked connection sees the cancel, and Start works again after a failed listen
mutate("Stop cancels the ctx of a hijacked connection (xecho)", "internal/web/server.go", "./xecho", "TestStop_HijackedConnGetsCancelled",
       swap('\tcancelBase()\n', '\t_ = cancelBase\n'))
mutate("Start works again after a failed listen (xecho)", "internal/web/server.go", "./xecho", "TestStart_RetryAfterListenFailure",
       swap('\t\t\ts.srv, s.cancel = nil, nil\n', ''))
