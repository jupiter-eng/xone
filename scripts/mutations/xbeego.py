# Mutations for xbeego: the promises of the xbeego module. The format is in
# scripts/mutations/__init__.py.
#
# Starting and stopping the server, TLS, proxy ranges and the access log fields are implemented in
# internal/web, and their own mutations live in xgin.py and core.py; what is mutated here are
# xbeego's call sites plus the things specific to beego: the route template has to be fetched from
# inside the router, the status code and the bytes written have to be counted by a writer of our
# own, and beego's two globals (the recovery slot and the logger) have to be taken over.
from . import cut, mutate, section, swap

section("the route template")
# Beego is the only one that knows the route template, and a plain http.Handler cannot see it.
# Without the filter copying it onto the request, every request is recorded as unmatched and the
# route label is gone from the logs, the metrics and the span names alike
mutate("the route template reaches the middleware", "xbeego/xbeego.go", "./xbeego",
       "TestAccessLog_RouteIsTheTemplate|TestMetrics_RouteLabelFoldsTheIdsAway|TestTrace_SpanNameUsesTheRouteTemplate",
       swap('\t\ts.Route = pattern\n', '\t\t_ = pattern\n'))
# The position matters and cost a bug: a panic in a controller unwinds straight to beego's recovery
# and FinishRouter never runs, so every panicking request was recorded as unmatched -- losing
# exactly the requests worth finding
mutate("the route survives a panicking handler", "xbeego/xbeego.go", "./xbeego", "TestRecover_PanicStillCountedAndTraced",
       swap('beeweb.InsertFilter("/*", beeweb.BeforeExec, captureRoute, beeweb.WithReturnOnOutput(false))',
            'beeweb.InsertFilter("/*", beeweb.FinishRouter, captureRoute, beeweb.WithReturnOnOutput(false))'))
# A filter left at beego's default is skipped as soon as anything has been written, which a filter
# registered before this one may well have done
mutate("the route survives an earlier filter that wrote", "xbeego/xbeego.go", "./xbeego", "TestAccessLog_RouteSurvivesAnEarlierFilterThatWrote",
       swap(', captureRoute, beeweb.WithReturnOnOutput(false))', ', captureRoute)'))
# The fallback has to be the fixed value: the real path would put every id in a URL into the route
# label and grow the series count without bound
mutate("an unmatched route is recorded as unmatched", "xbeego/middleware/middleware.go", "./xbeego",
       "TestAccessLog_UnmatchedRouteUsesFixedValue|TestMetric_UnmatchedRouteUsesFixedValue|TestTrace_UnmatchedRouteUsesFixedValue",
       swap('\tif st != nil && st.Route != "" {\n', '\tif st != nil {\n'))

section("what went out")
# net/http tells nobody what went out, so the recorder in front of the writer is the only source
# for the status code and the byte count
mutate("the recorder is installed in front of the writer", "xbeego/middleware/middleware.go", "./xbeego",
       "TestCapture_InstallsTheStateAndTheRecorder|TestLog_Fields",
       swap('\t\t\tnext.ServeHTTP(rec, r.WithContext(ctx))\n', '\t\t\tnext.ServeHTTP(w, r.WithContext(ctx))\n'))
# A handler that wrote nothing gets a 200 from net/http; recording a 0 would read as a request that
# never answered
mutate("a handler that set no status is recorded as 200", "xbeego/internal/reqstate/reqstate.go", "./xbeego", "TestRecorder_StatusDefaultsTo200",
       swap('\tif r.status == 0 {\n\t\treturn http.StatusOK\n\t}\n', ''))
# net/http ignores the second WriteHeader; recording it would have the access log disagree with what
# the client received
mutate("only the status that went out is recorded", "xbeego/internal/reqstate/reqstate.go", "./xbeego", "TestRecorder_SecondWriteHeaderIsIgnored",
       swap('\tif r.started {\n\t\t// net/http logs a "superfluous WriteHeader" line for the second call and ignores it; beego\'s\n\t\t// own context.Response guards the same way. Recording the second code would have the access\n\t\t// log disagree with what the client received.\n\t\treturn\n\t}\n', ''))
mutate("the bytes written are counted", "xbeego/internal/reqstate/reqstate.go", "./xbeego", "TestRecorder_CountsStatusAndBytes|TestLog_Fields",
       swap('\tr.bytes += n\n\treturn n, err\n', '\treturn n, err\n'))
# xbeego does not set Access.Ctx -- the request it hands in is the one this layer was entered with,
# which a handler cannot swap out from under it -- so the access log has to fall back to the
# request's own ctx, where trace_id and the xlog.AddKV fields live. (xgin's own Request field is
# shared with the handler, so it sets Ctx explicitly instead.)
mutate("the access log falls back to the request's own ctx", "internal/web/accesslog.go", "./xbeego", "TestLogScope",
       swap('\tctx := a.Ctx\n\tif ctx == nil {', '\tctx := a.Ctx\n\tif false {'))
# Keeping the whole of a response in memory to write a log is not worth it, and a cap that does not
# hold is exactly that
mutate("the captured response body is capped", "xbeego/internal/reqstate/reqstate.go", "./xbeego", "TestLog_ResponseBodyIsCappedAndRedacted",
       swap('\tif n := r.max - len(r.buf); n > 0 {\n\t\tr.buf = append(r.buf, p[:min(len(p), n)]...)\n\t}\n',
            '\tif r.max > 0 {\n\t\tr.buf = append(r.buf, p...)\n\t}\n'))
# Beego's context.Response asks the writer it was handed for Hijack; without forwarding it, every
# WebSocket upgrade fails
mutate("Hijack reaches the writer underneath", "xbeego/internal/reqstate/reqstate.go", "./xbeego", "TestRecorder_ForwardsHijack",
       swap('\thj, ok := r.ResponseWriter.(http.Hijacker)\n\tif !ok {\n\t\treturn nil, nil, http.ErrNotSupported\n\t}\n',
            '\treturn nil, nil, http.ErrNotSupported\n\thj, ok := r.ResponseWriter.(http.Hijacker)\n\tif !ok {\n\t\treturn nil, nil, http.ErrNotSupported\n\t}\n'))
# ReadFrom hands the bytes straight to the writer underneath, which counts them but never sees
# them: the access log would record an empty response_body beside a correct bytes_out
mutate("the captured response body survives ReadFrom", "xbeego/internal/reqstate/reqstate.go", "./xbeego", "TestRecorder_ReadFromKeepsCapturing",
       swap('\tif !ok || r.max-len(r.buf) > 0 {\n', '\tif !ok {\n'))
# io.Copy finds ReadFrom on the recorder and would call it again without the guard, recursing until
# the stack runs out
mutate("ReadFrom without one underneath does not recurse", "xbeego/internal/reqstate/reqstate.go", "./xbeego", "TestRecorder_ReadFromWithoutOneUnderneath",
       swap('\t\treturn io.Copy(writeOnly{r}, src)\n', '\t\treturn io.Copy(r, src)\n'))

section("the status code recorded")
# An aborted request has usually written a 200 header already, and a request whose client left and
# that returned without writing anything is a 200 nobody will ever receive
mutate("a request whose client left is recorded as 499", "xbeego/middleware/middleware.go", "./xbeego", "TestStatus_ClientGoneIsRecordedAs499",
       swap('\tif st.Aborted || (!st.Rec.Started() && web.ClientGone(ctx)) {\n', '\tif st.Aborted {\n'))
mutate("an aborted request is recorded as 499", "xbeego/middleware/middleware.go", "./xbeego", "TestRecover_AbortIsRecordedAs499|TestRecover_AbortedRequestRecordedAs499",
       swap('\tif st.Aborted || (!st.Rec.Started()', '\tif false && st.Aborted || (!st.Rec.Started()'))

section("panic recovery")
# Beego recovers inside its own router, which is where the stack still points at the controller.
# Without taking that slot over, the panic is written by beego's logs as plain text with no
# trace_id, no masking and no stack
mutate("the framework takes beego's recovery slot over", "xbeego/xbeego.go", "./xbeego", "TestRecover_BeegoHandlerPanic",
       swap('\tbeeweb.BConfig.RecoverFunc = func(ctx *beectx.Context, cfg *beeweb.Config) {', '\tif true {\n\t\treturn\n\t}\n\tbeeweb.BConfig.RecoverFunc = func(ctx *beectx.Context, cfg *beeweb.Config) {'))
# Controller.Abort / CustomAbort stop a handler by panicking with ErrAbort after writing the
# response. Logging that as a crash fills the error log with ordinary 401s
mutate("beego's own abort is not reported as a panic", "xbeego/xbeego.go", "./xbeego", "TestRecover_BeegoAbortIsNotAPanic",
       swap('\t\tif recovered == beeweb.ErrAbort {\n\t\t\treturn\n\t\t}\n', ''))
# c.Abort("404") is how a beego application answers a 404 when it registered a handler for it: the
# controller panics with the code as a string rather than with ErrAbort. Treated as a crash it
# answers 500 and writes an ERROR line per request
mutate("a registered error handler is rendered rather than crashed on", "xbeego/xbeego.go", "./xbeego", "TestRecover_RegisteredErrorHandlerIsRendered",
       swap('\t\t\t\t\tbeeweb.Exception(code, ctx)\n\t\t\t\t\treturn\n', '\t\t\t\t\t_ = code\n'))
# web.ErrorController registers every method it finds under a name rather than a status code
# (ErrorDb becomes "Db"), and that name cannot be rendered from outside beego. Reported as a crash
# it writes an ERROR line with a stack for a request the status, the metrics and the span all
# record as a success
mutate("an error code registered under a name is not a crash", "xbeego/xbeego.go", "./xbeego", "TestRecover_ErrorHandlerUnderANameIsNotACrash",
       swap('\t\t\t\tmiddleware.OnAbort(ctx.ResponseWriter, ctx.Request, recovered, recoverResponse(ctx))\n',
            '\t\t\t\tmiddleware.OnPanic(ctx.ResponseWriter, ctx.Request, recovered, recoverResponse(ctx))\n'))
# Context.Abort sets ctx.Output.Status and then panics with the body: a bare 500 would turn every
# such abort into a 500
mutate("an aborted request keeps the status the handler set", "xbeego/xbeego.go", "./xbeego", "TestRecover_ContextAbortKeepsItsStatus",
       swap('\t\tcode := ctx.Output.Status\n', '\t\tcode := 0\n'))
# The caller's answer is the caller's, whichever of the two recoveries caught the panic
mutate("WithRecoverFunc wins over beego's status", "xbeego/xbeego.go", "./xbeego", "TestRecover_WithRecoverFuncStillWinsOverBeegosStatus",
       swap('\tif st := reqstate.From(ctx.Request.Context()); st != nil && st.Recover != nil {\n\t\treturn st.Recover\n\t}\n', ''))

# net/http expects it and breaks the connection; answering a 500 sends a response the handler
# deliberately abandoned
mutate("http.ErrAbortHandler travels on to net/http", "xbeego/middleware/middleware.go", "./xbeego",
       "TestRecover_ErrAbortHandlerIsRePanicked|TestRecover_ErrAbortHandlerTravelsOn",
       swap('\t\tpanic(recovered)\n\t}\n\n\t// A broken connection', '\t\treturn\n\t}\n\n\t// A broken connection'))
# A client's flaky network should not fire the service's alerts
mutate("a broken connection is a warning, not an error", "xbeego/middleware/middleware.go", "./xbeego", "TestRecover_BrokenPipeIsAWarning",
       swap('\tif web.IsBrokenPipe(recovered) {\n', '\tif false && web.IsBrokenPipe(recovered) {\n'))
# A panic value carries credentials just as an error does: the whole DSN inside panic(err)
mutate("the panic value is masked", "xbeego/middleware/middleware.go", "./xbeego", "TestRecover_LogsAndAnswers500",
       swap('"error", web.RedactText(fmt.Sprint(recovered)),', '"error", fmt.Sprint(recovered),'))
# Changing the status code once the response is going out only truncates it
mutate("a response already going out is left alone", "xbeego/middleware/middleware.go", "./xbeego", "TestRecover_ResponseAlreadyStarted",
       swap('\tif st != nil && st.Rec != nil && st.Rec.Started() {\n', '\tif false && st != nil && st.Rec != nil && st.Rec.Started() {\n'))
# Beego has one global slot for the recovery, so the answer has to travel on the request: closed
# over, it would be whichever instance assembled last for both
mutate("the recovery answer travels on the request", "xbeego/middleware/middleware.go", "./xbeego", "TestWithRecoverFunc_IsPerInstance|TestOnPanic_UsesTheAnswerOnTheRequest",
       swap('\t\t\tif st != nil && handle != nil {\n\t\t\t\tst.Recover = handle\n\t\t\t}\n', '\t\t\t_ = st\n'))
# The backstop catches what never reaches beego's router: a panic in middleware mounted with
# WithMiddleware would otherwise reach net/http and kill the connection with no log at all
mutate("a panic outside beego is caught too", "xbeego/xbeego.go", "./xbeego", "TestRecover_CatchesWhatEscapesBeego",
       swap('\t\tchain = append(chain, middleware.Recover(x.recover))\n', ''))

section("who the peer is")
# Beego's own ctx.Input.IP() walks X-Forwarded-For from the left and believes whoever sent it: a
# client reaching the service directly can make client_ip say anything
mutate("client_ip comes from the trusted proxy list", "xbeego/xbeego.go", "./xbeego",
       "TestAccessLog_ClientIPIgnoresForgedHeadersFromAnUntrustedPeer|TestClientIP_IsWhatTheHandlerSeesToo",
       swap('chain := []func(http.Handler) http.Handler{middleware.Capture(x.trusted)}',
            'chain := []func(http.Handler) http.Handler{middleware.Capture(nil)}'))
# Forwarded headers accepted from everyone let a public client send X-Tenant-Id and have it treated
# as coming from one of us, carried into every internal call
mutate("forwarded headers are accepted from a trusted peer alone", "xbeego/middleware/trace.go", "./xbeego", "TestTrace_ForwardedHeadersOnlyFromATrustedPeer",
       swap('\tif st != nil && st.Trusted {\n', '\tif st != nil {\n'))
# What has to be judged is the directly connected peer, not the client IP computed from exactly the
# forgeable headers
mutate("trust is decided on the direct peer", "xbeego/internal/reqstate/reqstate.go", "./xbeego", "TestNew_DecidesTrustFromTheDirectPeer",
       swap('\t\ts.Trusted = peers.Trusts(web.RemoteIP(r))\n', '\t\ts.Trusted = true\n'))

section("assembly and the config")
# Call site: XBeego's Start / Stop only forward to web.Server. Without the forward, the server keeps
# running after the stop signal arrives
mutate("XBeego.Stop forwards to web.Server", "xbeego/xbeego.go", "./xbeego", "TestStop_WaitsForRequestsInFlight",
       swap('{ return x.server.Stop(ctx, "xbeego") }', '{ return nil }'))
# The listen address, TLS, the middleware and the trusted proxies must all come from one config
mutate("XBeego.Start uses the config from assembly time", "xbeego/xbeego.go", "./xbeego", "TestStart_UsesConfigFromAssemblyTime",
       swap('\treturn x.server.Start("xbeego", x.conf.server(), x.handler)\n',
            '\tc, _ := x.cfg()\n\treturn x.server.Start("xbeego", c.server(), x.handler)\n'))
# Beego sets only ReadTimeout and WriteTimeout, from one ServerTimeOut that is 0 by default: a
# timeout that never reaches the http.Server is no defense at all
mutate("ReadHeaderTimeout reaches web.Server", "xbeego/config.go", "./xbeego", "TestConfig_ServerMapping|TestStart_TimeoutsReachTheServer",
       swap('\t\tReadHeaderTimeout: c.ReadHeaderTimeout,\n', '\t\tReadHeaderTimeout: c.IdleTimeout,\n'))
mutate("XBeego UseH2C reaches web.Server", "xbeego/config.go", "./xbeego", "TestConfig_ServerMapping",
       swap('\t\tUseH2C:            c.UseH2C,\n', ''))
mutate("XBeego ClientCAFile reaches web.Server", "xbeego/config.go", "./xbeego", "TestConfig_ServerMapping",
       swap('\t\tClientCAFile:      c.TLS.ClientCAFile,\n', ''))
mutate("XBeego MinVersion reaches web.Server", "xbeego/config.go", "./xbeego", "TestConfig_ServerMapping",
       swap('\t\tMinVersion:        c.TLS.MinVersion,\n', '\t\tMinVersion:        "1.2",\n'))
# Call site: the rules live in internal/web, and XBeego's Validate has to actually reach them
mutate("XBeego Validate checks the port and TLS", "xbeego/config.go", "./xbeego", "TestValidate",
       swap('\tif err := s.ValidateListen(); err != nil {\n\t\treturn err\n\t}\n', ''))
mutate("XBeego Validate checks the timeouts", "xbeego/config.go", "./xbeego", "TestValidate",
       swap('\tif err := s.ValidateTimeouts(); err != nil {\n\t\treturn err\n\t}\n', ''))
mutate("XBeego Validate checks the proxy ranges", "xbeego/config.go", "./xbeego", "TestValidate",
       swap('\tif err := web.ValidateProxies(c.TrustedProxies); err != nil {\n\t\treturn err\n\t}\n', ''))
# A path without a leading slash is compared against URL.Path and matches nothing, leaving the
# endpoint unreachable without a word
mutate("a metrics path without a leading slash fails startup", "xbeego/config.go", "./xbeego", "TestValidate",
       swap('if c.Metric && !strings.HasPrefix(c.MetricPath, "/") {', 'if false && !strings.HasPrefix(c.MetricPath, "/") {'))
# The XBeego block is claimed and validated by the StageServer hook. Dropping that error makes a
# misconfigured value wait until Start, which is after the framework checks for unclaimed blocks
mutate("a misconfigured XBeego block fails during startup", "xbeego/xbeego.go", "./xbeego", "TestLoadConfig_ClaimsTheBlockAndReportsAMistake",
       swap('\t_, err := fileConfig()\n\treturn err\n', '\t_, _ = fileConfig()\n\treturn nil\n'))
# A half-decoded invalid config may well have TrustedProxies as 0.0.0.0/0
mutate("an invalid XBeego block falls back to the defaults", "xbeego/xbeego.go", "./xbeego", "TestCurrentConfig_InvalidBlockFallsBackToSafeDefaults",
       swap('\tif err := xconfig.Unmarshal(ConfigKey, &c); err != nil {\n\t\treturn DefaultConfig(), err\n',
            '\tif err := xconfig.Unmarshal(ConfigKey, &c); err != nil {\n\t\treturn c, err\n'))
mutate("the config given to WithConfig is validated too", "xbeego/xbeego.go", "./xbeego", "TestHandler_InvalidConfigAssemblesWithDefaults",
       swap('\tif err := x.override.Validate(); err != nil {', '\tif err := x.override.Validate(); false && err != nil {'))
# Assembling twice mounts the middleware twice: two access log lines and doubled metrics per request
mutate("assembly happens once", "xbeego/xbeego.go", "./xbeego", "TestHandler_AssemblesOnce",
       swap('\tx.buildOnce.Do(func() {\n', '\tfunc(f func()) { f(); f() }(func() {\n'))
# Beego reads BConfig.RecoverFunc on every request and its logs package walks its adapter list
# without a lock, so a second instance writing either one while the first is already serving is a
# data race -- which is what the race detector reported
mutate("beego's process-wide globals are settled once", "xbeego/xbeego.go", "./xbeego", "TestBeegoGlobals_SettledByTheFirstAssembly",
       swap('\tapplyOnce.Do(func() {\n\t\tapplyConfig(c)\n', '\tfunc(f func()) { f() }(func() {\n\t\tapplyConfig(c)\n'))
# web.Run calls Handlers.Init right before it listens; a filter chain registered with
# InsertFilterChain is silently never run without it
mutate("beego's filter chains are folded in", "xbeego/xbeego.go", "./xbeego", "TestHandler_FilterChainsAreFoldedIn",
       swap('\t\tinitOnce.Do(beeweb.BeeApp.Handlers.Init)\n', ''))

section("the metrics endpoint")
# Answered before beego's router, so the endpoint exists whatever the application's routes are
mutate("the metrics endpoint is served", "xbeego/xbeego.go", "./xbeego", "TestMetrics_EndpointIsServedWithoutARoute",
       swap('\t\t\t\th = serveMetrics(c.MetricPath, h)\n', ''))
# The endpoint is answered in front of beego's router, so an application route on the same path is
# never reached and nothing says so.
mutate("a route of the application's own on MetricPath fails startup", "xbeego/xbeego.go", "./xbeego", "TestMetrics_PathTakenByTheApplicationFailsStartup",
       swap('if metricPathTaken(c.MetricPath) {', 'if false {'))
# An endpoint a caller believes to be behind their shared authentication must not be the one
# exception
mutate("the metrics endpoint goes through the caller's middleware", "xbeego/xbeego.go", "./xbeego", "TestWithMiddleware_RunsOutsideBeegoAndCoversMetrics",
       swap('\t\tfor i := len(chain) - 1; i >= 0; i-- {\n\t\t\th = chain[i](h)\n\t\t}\n\t\tx.handler = h\n',
            '\t\tfor i := len(chain) - 1; i >= 0; i-- {\n\t\t\th = chain[i](h)\n\t\t}\n\t\tif c.Metric {\n\t\t\th = serveMetrics(c.MetricPath, h)\n\t\t}\n\t\tx.handler = h\n'))
# The scraper polls it every few seconds: a line of access log and a span per poll are pure noise
# Nothing else can fill the template in for this one, and the unmatched bucket is what a dashboard
# watches for scanners -- at scrape frequency it is no longer watchable
mutate("a metrics scrape has its own route label", "xbeego/xbeego.go", "./xbeego", "TestMetrics_ScrapeHasItsOwnRouteLabel",
       swap('\t\t\t\tst.Route = path\n', ''))
mutate("the metrics endpoint is not access-logged", "xbeego/xbeego.go", "./xbeego", "TestAccessLog_MetricPathSkippedWithoutBeingListed",
       swap('\t\t\t\tskip = append(skip, c.MetricPath)\n', ''))
mutate("the metrics endpoint opens no span", "xbeego/xbeego.go", "./xbeego", "TestTrace_MetricsEndpointOpensNoSpan",
       swap('\t\t\tchain = append(chain, skipMetrics(c, middleware.Trace()))\n', '\t\t\tchain = append(chain, middleware.Trace())\n'))

section("beego's own logs")
# Beego hands the format string over in Msg with the arguments beside it in Args: writing Msg out as
# it is logs "cannot reach %s"
mutate("beego's log messages are formatted", "xbeego/log.go", "./xbeego", "TestBeegoLogs_RoutedIntoSlog",
       swap('\t\tmsg = fmt.Sprintf(lm.Msg, lm.Args...)\n', '\t\tmsg = fmt.Sprint(lm.Msg)\n'))
# Beego's logger attaches a console adapter of its own the first time anything is written to it:
# without the Reset, every line is written twice, once as JSON and once as beego's text on stderr
mutate("beego's own logger is replaced, not added to", "xbeego/log.go", "./xbeego", "TestBeegoLogs_ReplacesBeegosOwnLoggerRatherThanAddingToIt",
       swap('\tlogs.Reset()\n', ''))
mutate("beego's logs reach slog", "xbeego/xbeego.go", "./xbeego", "TestBeegoLogs_RoutedIntoSlog",
       swap('\t\tif c.BeegoLogs {\n\t\t\tuseBeeLogAdapter()\n\t\t}\n', ''))
