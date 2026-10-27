# Mutations for xgin: the promises of the xgin module. The format is in scripts/mutations/__init__.py.
from . import cut, mutate, section, swap

section("HTTP server")
# How long it waits comes from the caller's ctx alone (xone.Run passes the server's share of the
# stop budget). Replacing it makes a stuck request keep Stop from returning, and "the whole
# shutdown has one budget" becomes an empty phrase.
mutate("the server does not outlive the caller's deadline", "internal/web/server.go", "./xgin", "TestStop",
       swap('shutCtx, cancel := shutdownCtx(ctx)', 'shutCtx, cancel := shutdownCtx(context.WithoutCancel(ctx))'))
mutate("in-flight connections are force-closed after the timeout", "internal/web/server.go", "./xgin", "TestStop",
       swap('\t\tif cerr := srv.Close(); cerr != nil {\n\t\t\tslog.Warn(module+" force close failed", "error", cerr)\n\t\t}\n',''))
# Close only drops connections and cancels the request ctx; the handler's goroutine keeps running.
# Returning right after Close leaves a handler that is finishing up still running while the
# framework goes on to close the database.
mutate("handlers are waited for after the forced disconnect", "internal/web/server.go", "./xgin", "TestStop_WaitsForHandlersAfterForceClose|TestStop_HandlerIgnoringCtxReportsRemainingCount",
       swap('if n := s.waitHandlers(ctx); n > 0 {', 'if n := int64(0); n > 0 {'))
mutate("every request is counted as in flight", "internal/web/server.go", "./xgin", "TestStop_WaitsForHandlersAfterForceClose",
       swap('Handler:           s.track(h),', 'Handler:           h,'))
# Call site: XGin's Start / Stop only forward to web.Server. Without the forward, the server keeps
# running after the stop signal arrives.
mutate("XGin.Stop forwards to web.Server", "xgin/xgin.go", "./xgin", "TestStop",
       swap('{ return g.server.Stop(ctx, "xgin") }', '{ return nil }'))
# With Shutdown using the whole budget, there is nothing left when Close lands and nobody waits for
# the handlers that are finishing up.
mutate("Shutdown leaves part of the budget for waiting on handlers", "internal/web/server.go", "./xgin", "TestStop_WaitsForHandlersAfterForceClose",
       swap('shutCtx, cancel := shutdownCtx(ctx)', 'shutCtx, cancel := context.WithCancel(ctx)'))
# Gin fixes a route's handler chain when the route goes on, so an endpoint registered before
# e.Use(g.extra...) walks past the shared auth every business route sits behind. (The 404 / 405
# handlers need no mutation of their own: engine.Use rebuilds them, which the test pins.)
mutate("the metrics endpoint goes through the user middleware too", "xgin/xgin.go", "./xgin", "TestBuild_MetricsEndpointRunsUserMiddleware",
       swap('\t\t\t} else {\n\t\t\t\te.GET(c.MetricPath, serveMetrics)\n\t\t\t}\n', '\t\t\t}\n'),
       swap('\t\te.Use(g.extra...)\n', '\t\tif c.Metric {\n\t\t\te.GET(c.MetricPath, serveMetrics)\n\t\t}\n\t\te.Use(g.extra...)\n'))
mutate("a public peer's X-Forwarded-For is not believed", "xgin/xgin.go", "./xgin", "TestBuild|TestLog",
       cut('\tif err := e.SetTrustedProxies(web.ExpandProxies(c.TrustedProxies)); err != nil {',
    '_ = e.SetTrustedProxies([]string{})\n\t\treturn nil\n\t}\n'))
# When the list cannot be installed, gin's client_ip falls back to the direct peer while the ranges
# the forwarded headers are judged against would keep the whole list: the two halves of "who counts
# as one of us" have to fall back together.
mutate("the trust fallback covers the forwarded headers too", "xgin/xgin.go", "./xgin", "TestApplyConfig_FallsBackToTrustNoneOnProxyError",
       swap('\t\t_ = e.SetTrustedProxies([]string{})\n\t\treturn nil\n', '\t\t_ = e.SetTrustedProxies([]string{})\n'))
# h2c used to rely on x/net's h2c.NewHandler: the connection was hijacked, Shutdown returned nil in
# about 100µs with in-flight requests still running, and the framework went on to close the
# database. Going back to that spelling loses this promise.
mutate("in-flight h2c requests are waited for as well", "internal/web/server.go", "./xgin", "TestStop_H2C",
       swap('\t"github.com/jupiter-eng/xone/xerror"\n', '\t"github.com/jupiter-eng/xone/xerror"\n\t"golang.org/x/net/http2"\n\t"golang.org/x/net/http2/h2c"\n'),
       swap('\t\tHandler:           s.track(h),\n\t\tProtocols:         protocols(c),\n',
     '\t\tHandler:           h2c.NewHandler(s.track(h), &http2.Server{}),\n'))
# Call site: UseH2C has to be copied from XGin's config into web.Server, or configuring it still
# yields HTTP/1.1.
mutate("UseH2C reaches web.Server", "xgin/config.go", "./xgin", "TestStop_H2C",
       swap('\t\tUseH2C:            c.UseH2C,\n', ''))
mutate("cleartext HTTP/2 is rejected without UseH2C", "internal/web/server.go", "./xgin", "TestStart_CleartextHTTP2RejectedWithoutH2C",
       swap('p.SetUnencryptedHTTP2(c.UseH2C && !c.tlsEnabled())', 'p.SetUnencryptedHTTP2(true)'))
# net/http treats a ReadHeaderTimeout of 0 as "fall back to ReadTimeout", whose default is also 0:
# the main defense against slow-connection attacks disappears entirely while the config file merely
# reads 0.
mutate("a read-header timeout of 0 is rejected", "internal/web/config.go", "./xgin", "TestValidate", swap('\t\tif d.val <= 0 {', '\t\tif d.val < 0 {'))
mutate("negative read and write timeouts are rejected", "internal/web/config.go", "./xgin", "TestValidate",
       swap('if c.ReadTimeout < 0 || c.WriteTimeout < 0 {', 'if false {'))
# Call site: the rules live in internal/web, and XGin's Validate has to actually reach them, in the
# same order.
mutate("Validate checks the port and TLS", "xgin/config.go", "./xgin", "TestValidate",
       swap('\tif err := s.ValidateListen(); err != nil {\n\t\treturn err\n\t}\n', ''))
mutate("Validate checks the timeouts", "xgin/config.go", "./xgin", "TestValidate",
       swap('\tif err := s.ValidateTimeouts(); err != nil {\n\t\treturn err\n\t}\n', ''))
mutate("Validate checks the proxy ranges", "xgin/config.go", "./xgin", "TestValidate",
       swap('\tif err := web.ValidateProxies(c.TrustedProxies); err != nil {\n\t\treturn err\n\t}\n', ''))
# Gin does not reject a path without a leading slash, it rewrites it quietly: empty mounts the
# metrics on the root path /.
mutate("a metrics path without a leading slash fails startup", "xgin/config.go", "./xgin", "TestValidate|TestLoadConfig",
       swap('if c.Metric && !strings.HasPrefix(c.MetricPath, "/") {', 'if false && !strings.HasPrefix(c.MetricPath, "/") {'))
# The XGin block of the config file is claimed and validated by the StageServer hook (Unmarshal
# calls Validate). Dropping that error makes a misconfigured value wait until the server starts, by
# which time every other component is already connected.
mutate("a misconfigured XGin block fails during startup", "xgin/xgin.go", "./xgin", "TestLoadConfig",
       swap('\t_, err := fileConfig()\n\treturn err\n', '\t_, _ = fileConfig()\n\treturn nil\n'))
# CurrentConfig and assembly both rely on this fallback: a half-decoded invalid config may well
# have TrustedProxies as 0.0.0.0/0.
mutate("an invalid XGin block falls back to the defaults", "xgin/xgin.go", "./xgin", "TestCurrentConfig|TestEngine_InvalidConfigFallsBackToSafeDefaults",
       swap('\tif err := xconfig.Unmarshal(ConfigKey, &c); err != nil {\n\t\treturn DefaultConfig(), err\n',
     '\tif err := xconfig.Unmarshal(ConfigKey, &c); err != nil {\n\t\treturn c, err\n'))
mutate("the config given to WithConfig is validated too", "xgin/xgin.go", "./xgin", "TestStart_DoesNotListenOnInvalidConfig",
       swap('\tif err := g.override.Validate(); err != nil {', '\tif err := g.override.Validate(); false && err != nil {'))
# The listen address, TLS and the middleware and trusted proxies on the engine must come from one
# config. Start reading it again makes a config changed after assembly take effect only halfway.
mutate("Start uses the config from assembly time", "xgin/xgin.go", "./xgin", "TestStart_UsesConfigFromBuildTime",
       swap('\tc := g.conf\n', '\tc, _ := g.cfg()\n'))
# gin.New prints a "switch to release" warning in debug mode. Creating first and setting after
# makes a server configured as release print it anyway (without GIN_MODE set in the caller's binary,
# the default is debug).
mutate("the Mode is set before the engine is created", "xgin/xgin.go", "./xgin", "TestBuild_Mode",
       swap('\t\tgin.SetMode(c.Mode)\n\t\te := gin.New()\n', '\t\te := gin.New()\n\t\tgin.SetMode(c.Mode)\n'))

section("middleware")
mutate("a malformed proxy range fails startup", "internal/web/proxy.go", "./xgin", "TestValidate", swap('if p != TrustPrivate && !isIPOrCIDR(p) {','if false {'))
# xecho has no gin available and computes client_ip through internal/web's Proxies.ClientIP; that it
# agrees with gin is compared entry by entry here. Break any part of the rule (this picks "take the
# leftmost when all are trusted") and the result no longer matches gin.
mutate("the shared client_ip rule matches gin", "internal/web/proxy.go", "./xgin", "TestClientIP_SharedRuleMatchesGin",
       swap('if i == 0 || !ps.contains(ip) {', 'if !ps.contains(ip) {'))
# There is one word list per process (internal/web): without these two exported names forwarding to
# it, words the caller adds are honored by nobody.
mutate("AddSensitiveFields writes to the shared word list", "xgin/middleware/redact.go", "./xgin", "TestAddSensitive",
       swap('{ web.AddSensitiveFields(fields...) }', '{}'))
mutate("AddSensitiveHeaders writes to the shared list", "xgin/middleware/redact.go", "./xgin", "TestAddSensitive",
       swap('{ web.AddSensitiveHeaders(headers...) }', '{}'))
mutate("the metrics method label is folded", "xgin/middleware/metric.go", "./xgin", "TestMetric",
       swap('web.NormalizeMethod(c.Request.Method)', 'c.Request.Method'),
       swap('\t\tlatency *prometheus.HistogramVec\n\t)\n', '\t\tlatency *prometheus.HistogramVec\n\t)\n\t_ = web.NormalizeMethod\n'))
mutate("the config lands on the engine during assembly", "xgin/xgin.go", "./xgin", "TestBuild",
       swap('\t\tg.trusted = applyConfig(e, c)\n', '\t\tg.trusted = web.ParseProxies(c.TrustedProxies)\n'))
# The callback runs after the config has landed on the engine, so what it sets explicitly wins.
# Two ways to break it: land the config after the callback, or apply it again in Start (which is
# what it used to do).
mutate("engine settings in the callback win over the config", "xgin/xgin.go", "./xgin", "TestBuild_CallbackEngineSettingsOverrideConfig",
       swap('\t\tg.trusted = applyConfig(e, c)\n', '\t\tg.trusted = web.ParseProxies(c.TrustedProxies)\n'),
       swap('\t\tfor _, f := range g.routes {\n\t\t\tf(e)\n\t\t}\n', '\t\tfor _, f := range g.routes {\n\t\t\tf(e)\n\t\t}\n\t\tapplyConfig(e, c)\n'))
mutate("Start does not apply the config a second time", "xgin/xgin.go", "./xgin", "TestBuild_CallbackEngineSettingsOverrideConfig",
       swap('\tc := g.conf\n', '\tc := g.conf\n\tapplyConfig(g.engine, c)\n'))
# The switches live in the config, and these places in assembly are what read them. A switch that
# is not wired is the hardest kind of bug to notice: the program runs as usual, that one option just
# never took effect.
mutate("the access log switch reads the config", "xgin/xgin.go", "./xgin", "TestLog_", swap('\t\tif c.Log {\n', '\t\tif true {\n', 2))
mutate("the tracing switch reads the config", "xgin/xgin.go", "./xgin", "TestTrace_", swap('\t\tif c.Trace {\n', '\t\tif true {\n'))
mutate("no metrics middleware is mounted with metrics off", "xgin/xgin.go", "./xgin", "TestMetric_",
       swap('\t\tif c.Metric {\n\t\t\te.Use(middleware.Metric())', '\t\tif true {\n\t\t\te.Use(middleware.Metric())'))
mutate("no endpoint is registered with metrics off", "xgin/xgin.go", "./xgin", "TestBuild_NoMetricsEndpointWhenDisabled",
       swap('\t\tif c.Metric {\n\t\t\tif metricPathTaken(', '\t\tif true {\n\t\t\tif metricPathTaken('))
mutate("the metrics endpoint uses the configured path", "xgin/xgin.go", "./xgin", "TestBuild_MetricsPathConfigurable",
       swap('e.GET(c.MetricPath, serveMetrics)', 'e.GET("/metrics", serveMetrics)'))
# Registered on a path the application already uses, gin panics on the duplicate -- out of the
# caller's own routing code, with nothing pointing at MetricPath.
mutate("a route of the application's own on MetricPath fails startup", "xgin/xgin.go", "./xgin", "TestBuild_MetricPathTaken",
       swap('if metricPathTaken(e, c.MetricPath) {', 'if false {'))
mutate("MetricPath is taken whatever method the route was registered for", "xgin/xgin.go", "./xgin", "TestBuild_MetricPathTakenCountsAnyMethod",
       swap('func(r gin.RouteInfo) bool { return r.Path == path }', 'func(r gin.RouteInfo) bool { return r.Path == path && r.Method == http.MethodGet }'))
mutate("the skipped log paths read the config", "xgin/xgin.go", "./xgin", "TestLogSkipPaths",
       swap('skip := slices.Clone(c.LogSkipPaths)', 'skip := slices.Clone([]string{})'))
# Crossed, the response body (which may carry a token) reaches the log of someone who enabled the
# request body only.
mutate("the request and response body switches are independent", "xgin/xgin.go", "./xgin", "TestLogBody",
       swap('middleware.WithBody(c.LogRequestBody, c.LogResponseBody)', 'middleware.WithBody(c.LogResponseBody, c.LogRequestBody)'))
# Each switch reaches its own option: wired to true, the query string, Authorization and Set-Cookie
# go into the log by default.
mutate("the query string switch reads the config", "xgin/xgin.go", "./xgin", "TestLogQueryAndHeaders",
       swap('middleware.WithQuery(c.LogQuery)', 'middleware.WithQuery(true)'))
mutate("the request and response header switches are independent", "xgin/xgin.go", "./xgin", "TestLogQueryAndHeaders",
       swap('middleware.WithHeaders(c.LogRequestHeaders, c.LogResponseHeaders)', 'middleware.WithHeaders(c.LogResponseHeaders, c.LogRequestHeaders)'))
mutate("the Chinese translation switch reads the config", "xgin/xgin.go", "./xgin", "TestZHTranslations",
       swap('\t\tif c.ZHTranslations {', '\t\tif false {'))
# binding.Validator is an interface and gin.DisableBindValidation sets it to nil: asking a nil
# interface for its Engine panics, and this runs during assembly.
mutate("a disabled gin validator is reported, not panicked on", "xgin/trans/trans.go", "./xgin", "TestRegisterZH_DisabledValidator",
       cut('\tif binding.Validator == nil {', 'there is nothing to translate")\n\t}\n'))
mutate("the query string stays out of the access log", "internal/web/accesslog.go", "./xgin", "TestLog",
       swap('slog.String("path", r.URL.Path),', 'slog.String("path", r.URL.RequestURI()),'))
mutate("a recorded query string is masked", "internal/web/accesslog.go", "./xgin", "TestLog_Query",
       swap('slog.String("query", redactForm(r.URL.RawQuery))', 'slog.String("query", r.URL.RawQuery)'))
mutate("recorded request headers are masked", "internal/web/accesslog.go", "./xgin", "TestLog_RedactsRequestHeaders",
       swap('Value: RedactHeaders(r.Header)}', 'Value: slog.AnyValue(r.Header)}'))
mutate("recorded response headers are masked", "internal/web/accesslog.go", "./xgin", "TestLog_ResponseHeaders",
       swap('Value: RedactHeaders(a.RespHeader)}', 'Value: slog.AnyValue(a.RespHeader)}'))
# Call site: the response headers have to be taken from gin's writer and handed over, or what is
# recorded is always empty.
mutate("the access log gets the real response headers", "xgin/middleware/log.go", "./xgin", "TestLog_ResponseHeaders",
       swap('RespHeader: c.Writer.Header(),', 'RespHeader: map[string][]string{},'))
# The writer capturing the response body sits in front of gin's own, and http.ResponseController
# walks down to the connection through Unwrap. Without it, SetReadDeadline, SetWriteDeadline and
# EnableFullDuplex answer ErrNotSupported as soon as LogResponseBody is on, and SSE and long
# polling lose the deadlines they set per chunk.
mutate("recording the response body keeps http.ResponseController working", "xgin/middleware/log.go", "./xgin", "TestLog_ResponseCaptureKeepsResponseControllerFeatures",
       swap('func (w *captureWriter) Unwrap() http.ResponseWriter', 'func (w *captureWriter) unwrapGone() http.ResponseWriter'))
# With the level disabled the whole capture is skipped: by the time slog judges the level itself,
# buffering the body and capturing the response have already been paid for.
mutate("the access log does nothing while the level is disabled", "internal/web/accesslog.go", "./xgin", "TestLog_DoesNoWorkWhenLevelDisabled",
       swap(' || !slog.Default().Enabled(r.Context(), slog.LevelInfo)', ''))
mutate("the access log carries the errors gin recorded", "xgin/middleware/log.go", "./xgin", "TestLog_ErrAbortHandlerAbortLoggedAs499",
       swap('\t\t\t\ta.Errors = c.Errors.String()\n', ''))
# ErrAbortHandler is the agreed way to say "drop this connection". Catching it turns a response that
# was meant to be aborted into a 500 going out, plus a panic stack that means nothing.
mutate("ErrAbortHandler is re-panicked to net/http", "xgin/middleware/middleware.go", "./xgin", "TestRecover",
       cut('\t\t\tif err == http.ErrAbortHandler {\n', '\t\t\t\tpanic(err)\n\t\t\t}\n'))
# An aborted request has usually written a 200 header already: reading c.Writer.Status() as is makes
# a truncated response a success in the access log, the metrics and the trace alike.
mutate("a request aborted with ErrAbortHandler is recorded", "xgin/middleware/middleware.go", "./xgin", "TestLog_ErrAbortHandlerAbortLoggedAs499|TestMetric_ErrAbortHandlerAbortRecordedAs499|TestTrace_ErrAbortHandlerAbortRecordedAsError",
       swap('\t\t\t\t_ = c.Error(http.ErrAbortHandler) //nolint:errcheck // only recording it\n', ''))
mutate("the access log records an aborted request as 499", "xgin/middleware/log.go", "./xgin", "TestLog_ErrAbortHandler",
       swap('Status:   status(c, ctx),', 'Status:   c.Writer.Status(),'), swap('\t\tstart, ctx := time.Now(), c.Request.Context()\n', '\t\tstart, ctx := time.Now(), c.Request.Context()\n\t\t_ = ctx\n'))
mutate("the metrics record an aborted request as 499", "xgin/middleware/metric.go", "./xgin", "TestMetric_ErrAbortHandler",
       swap('strconv.Itoa(status(c, ctx))', 'strconv.Itoa(c.Writer.Status())'), swap('\t\tstart, ctx := time.Now(), c.Request.Context()\n', '\t\tstart, ctx := time.Now(), c.Request.Context()\n\t\t_ = ctx\n'))
mutate("tracing marks an aborted request as an error", "xgin/middleware/trace.go", "./xgin", "TestTrace_ErrAbortHandler",
       swap('st := status(c, ctx)', 'st := c.Writer.Status()'))
# The client left and the handler returned without writing: this used to be recorded as a 200 with
# bytes_out 0 while the client received nothing at all.
mutate("XGin records a client that left as 499", "xgin/middleware/middleware.go", "./xgin", "TestClientGoneBeforeAnythingSentRecordedAs499",
       swap('\tif aborted(c) || (!c.Writer.Written() && web.ClientGone(ctx)) {', '\tif aborted(c) {'))
mutate("XGin records what was sent once the response went out", "xgin/middleware/middleware.go", "./xgin", "TestClientGoneAfterResponseWrittenKeepsSentStatus",
       swap('\tif aborted(c) || (!c.Writer.Written() && web.ClientGone(ctx)) {', '\tif aborted(c) || web.ClientGone(ctx) {'))
# What is judged is the ctx as this layer saw it on the way in: a ctx installed by an inner layer
# and cancelled on its way out does not mean the client left. One entry per call site.
mutate("the XGin access log judges the client by the ctx on the way in", "xgin/middleware/log.go", "./xgin", "TestClientGoneIgnoresContextsSetByInnerMiddleware",
       swap('Status:   status(c, ctx),', 'Status:   status(c, c.Request.Context()),'), swap('\t\tstart, ctx := time.Now(), c.Request.Context()\n', '\t\tstart, ctx := time.Now(), c.Request.Context()\n\t\t_ = ctx\n'))
mutate("the XGin metrics judge the client by the ctx on the way in", "xgin/middleware/metric.go", "./xgin", "TestClientGoneIgnoresContextsSetByInnerMiddleware",
       swap('strconv.Itoa(status(c, ctx))', 'strconv.Itoa(status(c, c.Request.Context()))'), swap('\t\tstart, ctx := time.Now(), c.Request.Context()\n', '\t\tstart, ctx := time.Now(), c.Request.Context()\n\t\t_ = ctx\n'))
mutate("XGin tracing judges the client by the ctx on the way in", "xgin/middleware/trace.go", "./xgin", "TestClientGoneIgnoresContextsSetByInnerMiddleware",
       swap('st := status(c, ctx)', 'st := status(c, c.Request.Context())'))
# A client leaving is not the server's fault; only a handler aborting itself is marked as an error.
mutate("the span of a client that left is not marked as an error", "xgin/middleware/trace.go", "./xgin", "TestClientGoneBeforeAnythingSentRecordedAs499",
       swap('\t\t\tcase aborted(c):\n', '\t\t\tcase st == statusAborted:\n'))
# The compression middleware sits inside the access log: what is captured is compressed bytes, which
# used to reach response_body as garbage.
mutate("a compressed response body records no content", "internal/web/accesslog.go", "./xgin", "TestLog_EncodedResponseBodyOmitted",
       swap('\t\t\tbody := encodedOmitted(a.RespHeader)\n', '\t\t\tbody := ""\n'))
# The access log is written with the ctx this layer was entered with, not with whatever the handler
# left on c.Request: gin's c.Request is one field every layer shares, so a handler swapping in a ctx
# of its own used to carry a derived xlog.CtxWithKV scope into the access log -- and take trace_id
# and the whole scope away when what it swapped in was not derived from the request's ctx at all.
mutate("the access log is written with the ctx from the way in", "xgin/middleware/log.go", "./xgin", "TestLogScope_CtxSwappedByTheHandler",
       swap('\t\t\t\tCtx:      ctx,\n', ''))
mutate("the tracing method is folded", "xgin/middleware/trace.go", "./xgin", "TestTrace",
       swap('method := web.NormalizeMethod(c.Request.Method)', 'method := c.Request.Method'),
       swap('\t\troute := routeOf(c)\n', '\t\troute := routeOf(c)\n\t\t_ = web.NormalizeMethod\n'))
# "Who is one of us" comes from TrustedProxies alone. Get the mark wrong once and either forged
# headers reach the internal network or forwarding stops working.
mutate("a peer is trusted only when it is in TrustedProxies", "xgin/xgin.go", "./xgin", "TestBuild_PassthroughHeadersOnlyFromTrustedProxies|TestBuild_PassthroughHeadersOnlyFromPrivateByDefault",
       swap('if g.trusted.Trusts(c.RemoteIP()) {', 'if len(g.trusted) > 0 {'))
mutate("the trusted ranges come from the config at assembly time", "xgin/xgin.go", "./xgin", "TestBuild_PassthroughHeadersOnlyFromTrustedProxies",
       swap('\treturn web.ParseProxies(c.TrustedProxies)\n', '\treturn web.ParseProxies(nil)\n'))
# The peer trust check has to expand private as well: without it nobody is trusted under the default
# config and forwarding stops working entirely.
mutate("the peer trust check expands private too", "internal/web/proxy.go", "./xgin", "TestBuild_PassthroughHeadersOnlyFromPrivateByDefault",
       swap('expanded := ExpandProxies(list)', 'expanded := list'))
# Private ranges only by default: pod IPs in Kubernetes are random, and what an ingress, a load
# balancer or a sidecar forwards is trusted out of the box.
mutate("TrustedProxies defaults to private", "xgin/config.go", "./xgin", "TestBuild_TrustsOnlyPrivateProxiesByDefault|TestBuild_PassthroughHeadersOnlyFromPrivateByDefault",
       swap('\t\tTrustedProxies:     []string{web.TrustPrivate},\n', ''))
mutate("private covers the carrier-grade NAT range", "internal/web/proxy.go", "./xgin", "TestBuild_PassthroughHeadersOnlyFromPrivateByDefault",
       swap('"192.168.0.0/16", "100.64.0.0/10",', '"192.168.0.0/16",'))
mutate("private covers IPv6 ULA", "internal/web/proxy.go", "./xgin", "TestBuild_PassthroughHeadersOnlyFromPrivateByDefault",
       swap('"::1/128", "fc00::/7",', '"::1/128",'))
mutate("other ranges written alongside private still count", "internal/web/proxy.go", "./xgin", "TestBuild_TrustedProxiesMixesPrivateWithOtherCIDRs",
       swap('\t\tout = append(out, p)\n\t}\n\treturn out', '\t}\n\treturn out'))
# Call site: the client_ip side (gin) needs the expanded ranges too — handed "private" directly it
# fails to parse and falls back to trusting nobody.
mutate("gin receives the expanded ranges", "xgin/xgin.go", "./xgin", "TestBuild_TrustsOnlyPrivateProxiesByDefault",
       swap('e.SetTrustedProxies(web.ExpandProxies(c.TrustedProxies))', 'e.SetTrustedProxies(c.TrustedProxies)'))
mutate("peer trust is decided before tracing starts", "xgin/xgin.go", "./xgin", "TestBuild_PassthroughHeadersOnlyFromTrustedProxies",
       swap('e.Use(g.markTrustedPeer, skipMetrics(c, middleware.Trace()))', 'e.Use(skipMetrics(c, middleware.Trace()))'))
# XGin.Trace governs the span only. Turning it off used to drop the extraction with it, so neither
# the upstream trace identifiers nor the forwarded headers were accepted.
mutate("XGin.Trace off still joins upstream tracing and forwarding", "xgin/xgin.go", "./xgin", "TestBuild_TraceDisabledOnlySkipsSpan_StillPropagates",
       swap('e.Use(g.markTrustedPeer, middleware.Propagate())', 'e.Use(g.markTrustedPeer)'))
mutate("XGin.Trace off leaves the trust rules unchanged", "xgin/xgin.go", "./xgin", "TestBuild_TraceDisabledOnlySkipsSpan_StillPropagates",
       swap('e.Use(g.markTrustedPeer, middleware.Propagate())', 'e.Use(middleware.Propagate())'))
mutate("Propagate hands the trust mark to xtrace too", "xgin/middleware/trace.go", "./xgin", "TestTrace_PeerTrustFollowsXginMarker",
       swap('WithContext(otel.GetTextMapPropagator().Extract(c.Request.Context(), inbound(c)))', 'WithContext(otel.GetTextMapPropagator().Extract(c.Request.Context(), propagation.HeaderCarrier(c.Request.Header)))'))
mutate("the tracing middleware hands the trust mark to xtrace", "xgin/middleware/trace.go", "./xgin", "TestTrace_PeerTrustFollowsXginMarker",
       swap('\tif c.GetBool(peer.TrustedKey) {\n\t\treturn trustedCarrier{h}\n\t}\n', '\t_ = peer.TrustedKey\n'))

section("clients")
mutate("the XGin server TLS settings reach http.Server", "internal/web/server.go", "./xgin", "TestStart_ClientCAFile|TestStart_MinVersion",
       swap('\tsrv.TLSConfig = tlsCfg', '\t_ = tlsCfg'))
mutate("XGin ClientCAFile turns on mutual authentication", "internal/web/config.go", "./xgin", "TestStart_ClientCAFileEnablesMutualTLS",
       swap('\t\tcfg.ClientAuth = tls.RequireAndVerifyClientCert\n', ''))
mutate("XGin MinVersion takes effect", "internal/web/config.go", "./xgin", "TestStart_MinVersion",
       swap('cfg := &tls.Config{MinVersion: tlsVersions[c.MinVersion]}', 'cfg := &tls.Config{}'))
# Believed to be mutual authentication while in fact anyone can connect, in plaintext.
mutate("XGin ClientCAFile has to be set together with a certificate", "internal/web/config.go", "./xgin", "TestValidate_ServerTLSNewFields|TestConfig_ServerTLSLoadedFromFile",
       swap('if c.ClientCAFile != "" && !c.tlsEnabled() {', 'if false {'))
mutate("XGin MinVersion accepts 1.2 / 1.3 only", "internal/web/config.go", "./xgin", "TestValidate_ServerTLSNewFields",
       swap('if _, ok := tlsVersions[c.MinVersion]; c.tlsEnabled() && !ok {', 'if false {'))
# MinVersion does nothing on a plaintext server: it used to be validated anyway, so a value nobody
# used kept the server from starting.
mutate("MinVersion is not validated without TLS", "internal/web/config.go", "./xgin", "TestValidate_ServerTLSNewFields",
       swap('c.tlsEnabled() && !ok', '!ok'))
# Call site: the TLS fields are copied from XGin's config into web.Server, and missing one means
# believing it is on while it is not.
mutate("ClientCAFile reaches web.Server", "xgin/config.go", "./xgin", "TestStart_ClientCAFileEnablesMutualTLS",
       swap('\t\tClientCAFile:      c.TLS.ClientCAFile,\n', ''))
mutate("MinVersion reaches web.Server", "xgin/config.go", "./xgin", "TestStart_MinVersion",
       swap('\t\tMinVersion:        c.TLS.MinVersion,\n', '\t\tMinVersion:        "1.2",\n'))
# gin's ResponseWriter interface carries WriteString and handlers calling it directly is a common
# thing to do. With the wrapper capturing Write alone, a response written that way is always empty
# in the log.
mutate("a response written with WriteString is captured too", "xgin/middleware/log.go", "./xgin", "TestLog",
       swap('''\tif w.capture && w.buf.Len() < maxResponseBody {
\t\tw.buf.WriteString(s[:min(len(s), maxResponseBody-w.buf.Len())])
\t}''', '\tif false {\n\t}'))
# gin's default is to trust everyone, so anyone sending an X-Forwarded-For decides client_ip in the
# access log. When the setup fails it has to fall back to the safe side.
mutate("a failed proxy setup falls back to trusting nobody", "xgin/xgin.go", "./xgin", "TestApplyConfig|TestBuild_ForwardedHeadersOnlyWithProxiesConfigured",
       swap('\t\t_ = e.SetTrustedProxies([]string{})', ''))

section("tracing")
# Using xgin gives tracing, with no need for the caller to remember importing xtrace. Drop that line
# and the global TracerProvider stays noop: spans record nothing, logs carry no trace_id, and there
# is no error anywhere.
mutate("xgin gives tracing without importing xtrace", "xgin/xgin.go", "./xgin", "TestTracingWorksWithoutImportingXtrace",
       swap('\t_ "github.com/jupiter-eng/xone/xtrace"\n', ''))

section("access log fields")
# slog's JSON writes a Duration as an integer of nanoseconds: with the field called elapsed_ms and a
# nanosecond value, an alert configured in milliseconds is off by a factor of a million.
mutate("the access log duration is in milliseconds", "internal/web/accesslog.go", "./xgin", "TestLog_ElapsedIsMilliseconds",
       swap('slog.Float64("elapsed_ms", millis(a.Elapsed))', 'slog.Float64("elapsed_ms", float64(a.Elapsed))'))
# Filling in the real path for an unmatched route makes /nope indistinguishable between a route and
# a 404 in the log, and disagrees with the metrics and the span.
# gin returns -1 from Size() when no response body was written: recorded as is, a 204 looks like a
# failure and summing bytes_out undercounts.
# gin writes its default 404 / 405 body only after the middleware chain, so without writing it
# inside the chain bytes_out records 0.
mutate("the default 404 / 405 is written inside the chain", "xgin/xgin.go", "./xgin", "TestDefault404And405",
       swap('\t\te.NoRoute(notFound)\n\t\te.NoMethod(methodNotAllowed)\n', ''))
mutate("bytes_out records 0 when no response body was written", "xgin/middleware/log.go", "./xgin", "TestLog_BytesOut",
       swap('BytesOut:   max(c.Writer.Size(), 0),', 'BytesOut:   c.Writer.Size(),'))
# The value comes from routeOf, shared by the three middlewares; this entry breaks it directly and
# the three below mutate one call site each.
mutate("an unmatched route is recorded as unmatched", "xgin/middleware/middleware.go", "./xgin",
       "TestLog_UnmatchedRouteIsUnmatched|TestMetric_UnmatchedRouteUsesFixedValue|TestTrace_UnmatchedRouteUsesFixedValue",
       swap('\treturn web.RouteUnmatched\n', '\treturn c.Request.URL.Path\n'))
mutate("the access log records an unmatched route as unmatched", "xgin/middleware/log.go", "./xgin", "TestLog_UnmatchedRouteIsUnmatched",
       swap('Route:    routeOf(c),', 'Route:    c.FullPath(),'))
mutate("the metrics record an unmatched route as unmatched", "xgin/middleware/metric.go", "./xgin", "TestMetric_UnmatchedRouteUsesFixedValue",
       swap('route, method, code := routeOf(c),', 'route, method, code := c.Request.URL.Path,'))
mutate("the span records an unmatched route as unmatched", "xgin/middleware/trace.go", "./xgin", "TestTrace_UnmatchedRouteUsesFixedValue",
       swap('\t\troute := routeOf(c)\n', '\t\troute := c.Request.URL.Path\n'))

section("masking error text")
mutate("the access log errors field is masked (xgin)", "internal/web/accesslog.go", "./xgin", "TestLog_ErrorTextIsRedacted",
       swap('slog.String("errors", RedactText(a.Errors))', 'slog.String("errors", a.Errors)'))
mutate("gin.errors on the span is masked", "xgin/middleware/trace.go", "./xgin", "TestTrace_ErrorTextIsRedactedOnSpan",
       swap('web.RedactText(c.Errors.String())', 'c.Errors.String()'))

section("masking error text and panics")
# The whole DSN inside panic(err), or a "password=..." in it, reaching the error field as is writes
# the password to disk.
mutate("the panic value is masked", "xgin/middleware/middleware.go", "./xgin", "TestRecover_PanicValueIsRedacted",
       swap('"error", web.RedactText(fmt.Sprint(err)),', '"error", fmt.Sprint(err),'))
# The rules live in internal/web (with their own mutations in core.py); this checks that xgin's
# access log and span really look that way.
mutate("several errors stay separated (xgin)", "internal/web/redact.go", "./xgin", "TestLog_MultipleErrorsStaySeparated|TestTrace_ErrorTextKeepsSeparationAndMasksDSNOnSpan",
       swap('return textNewlines.Replace(strings.TrimRight(redactCredentials(s), "\\r\\n"))', 'return newlines.Replace(redactCredentials(s))'))
mutate("a DSN password in errors is masked (xgin)", "internal/web/redact.go", "./xgin", "TestLog_DSNPasswordInErrorIsMasked|TestTrace_ErrorTextKeepsSeparationAndMasksDSNOnSpan",
       swap('\ts = urlUserinfo.ReplaceAllString(s, "$1:"+Redacted+"@")\n', ''))

section("the zero value and the startup log")
# The module name used to be filled in by New alone: the errors of a &xgin.XGin{} were recorded
# under an empty module name and xerror.Is(err, "xgin") did not hold.
mutate("a zero value XGin reports errors and logs under xgin", "xgin/xgin.go", "./xgin", "TestZeroValue_ReportsUnderModuleName",
       swap('g.server.Start("xgin", ', 'g.server.Start("", '))
# listening used to be printed before listening: with an unreadable certificate or a taken port it
# said listening first and reported listen failed afterwards.
mutate("listening is printed only after the port is bound", "internal/web/server.go", "./xgin", "TestStart_NoListeningLogWhenListenFails",
       swap('\tln, err := listen(c, srv)\n', '\tslog.Info(module + " listening")\n\tln, err := listen(c, srv)\n'))
mutate("listening is printed only after the certificate is read", "internal/web/server.go", "./xgin", "TestStart_NoListeningLogWhenListenFails",
       swap('\t\tcert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)\n\t\tif err != nil {\n\t\t\treturn nil, err\n\t\t}\n\t\tsrv.TLSConfig.Certificates = append(srv.TLSConfig.Certificates, cert)\n',
            '\t\t_ = tls.Certificate{}\n'),
       swap('srv.ServeTLS(ln, "", "")', 'srv.ServeTLS(ln, c.CertFile, c.KeyFile)'))
# Call site: the rule lives in internal/web, and reading the XGin block really has to reject the
# mapped spelling.
mutate("XGin rejects an IPv4-mapped proxy spelling", "internal/web/proxy.go", "./xgin", "TestLoadConfig_IPv4MappedProxyFailsStartup",
       swap('\t\tif plain, ok := unmapped(p); ok {', '\t\tif plain, ok := unmapped(p); ok && false {'))

section("stopping, routing and the request context")
# Shutdown does not wait for a hijacked connection and Close cannot drop it: without cancelling the
# root ctx, a WebSocket handler waiting on the request ctx makes Stop burn the whole budget before
# reporting still running.
mutate("Stop cancels the ctx of a hijacked connection", "internal/web/server.go", "./xgin", "TestStop_HijackedConnGetsCancelled",
       swap('\tcancelBase()\n', '\t_ = cancelBase\n'))
mutate("the request ctx hangs off a cancellable root", "internal/web/server.go", "./xgin", "TestStop_HijackedConnGetsCancelled",
       swap('{ return base }', '{ return context.WithoutCancel(base) }'))
# Still considering itself running after a failed listen makes every later Start say already
# running once the port is free.
mutate("Start works again after a failed listen", "internal/web/server.go", "./xgin", "TestStart_RetryAfterListenFailure",
       swap('\t\t\ts.srv, s.cancel = nil, nil\n', ''))
# gin's default 301 / 307 runs no middleware: /users/ reaches neither the access log, the metrics
# nor the trace.
mutate("trailing slashes are not redirected", "xgin/xgin.go", "./xgin", "TestBuild_TrailingSlashGoesThroughMiddlewareAs404",
       swap('\te.RedirectTrailingSlash = false\n', ''))
# Without it, xlog.AddKV(c, ...) and Start(c, ...) lose the log scope and the parent span.
mutate("a *gin.Context passed as a ctx reaches the request ctx", "xgin/xgin.go", "./xgin", "TestBuild_GinContextAsContext",
       swap('\t\te.ContextWithFallback = true\n', ''))
# Call site: scraping /metrics opens a span every time.
mutate("a metrics scrape opens no span", "xgin/xgin.go", "./xgin", "TestTrace_MetricsScrapeNotTraced",
       swap('skipMetrics(c, middleware.Trace())', 'middleware.Trace()'))
# engine.Handler() wraps x/net's h2c.NewHandler when gin's UseH2C is on, and the connection is
# hijacked.
mutate("what is handed to net/http is the engine itself", "xgin/xgin.go", "./xgin", "TestStart_GinUseH2CDoesNotBypassShutdown",
       swap('c.server(), g.engine)', 'c.server(), g.engine.Handler())'))
# gin's Next() goes on with the handler after the one that panicked, so a RecoverFunc that only
# writes a response lets the request carry on past the layer that panicked -- middleware that
# panicked while turning a request away is followed by the handler it was meant to protect.
mutate("a panic ends the handler chain", "xgin/middleware/middleware.go", "./xgin", "TestRecover_CustomResponseEndsTheChain",
       swap('\t\t\t// handle goes through AbortWithStatus and already stops the chain itself.\n\t\t\tc.Abort()\n',
     '\t\t\t// handle goes through AbortWithStatus and already stops the chain itself.\n'))
# A client leaving is not the service's failure: ERROR would make the alerts follow the client's
# flaky network.
mutate("a broken connection is logged as WARN", "xgin/middleware/middleware.go", "./xgin", "TestRecover_BrokenPipe",
       swap('slog.WarnContext(ctx, "connection broken"', 'slog.ErrorContext(ctx, "connection broken"'))
