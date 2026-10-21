# The xhttp mutations: the promises of the xhttp module. The format is in
# scripts/mutations/__init__.py
from . import cut, mutate, section, swap

section("Clients")
# Adding a retry condition voids resty's own "do not retry" decision: a 200 with broken JSON would
# have the same GET sent again
mutate("only transport errors are retried", "xhttp/xhttp.go", "./xhttp", "TestRetry",
       swap('\tif !isTransportError(err) {\n', '\tif err == nil {\n'))
# A timeout cannot tell whether the request reached the server, and resending a POST places the
# order twice: aimed at the call site adding the condition and at the method check
mutate("a POST is not retried", "xhttp/xhttp.go", "./xhttp", "TestRetry",
       swap('\tif _, ok := idempotentMethods[method]; ok || !p.onlyIdempotent {', '\tif true {'))
# resty's conditions are ORed: a caller adding a condition that retries whenever err != nil brings
# the POST retries back. The veto on RetryAfter catches that
mutate("a caller's retry condition cannot bring POST retries back", "xhttp/xhttp.go", "./xhttp", "TestRetry_UserCondition|TestRetry_UserStatusConditionWithConsumedReader",
       swap('\t\t\tAddRetryCondition(p.condition).\n\t\t\tSetRetryAfter(p.veto)\n', '\t\t\tAddRetryCondition(p.condition)\n'))
# Vetoing once a response has arrived would hand the caller an error of our own making rather than
# that 503
mutate("no veto by method once a response has arrived", "xhttp/xhttp.go", "./xhttp", "TestRetry_UserStatusConditionNotVetoedAndNoBogusError",
       swap('if err == nil || errors.Is(err, errNotIdempotent) && resp != nil && resp.RawResponse != nil {', 'if err == nil {'))
# resty rebuilds the request from the same io.Reader on every attempt: the second one sends 0 bytes
# and the server answers 200
mutate("an io.Reader that has been read is not resent", "xhttp/xhttp.go", "./xhttp", "TestRetry_ConsumedReaderBodyNotResent|TestRetry_UserStatusConditionWithConsumedReader",
       swap('\tif consumedReader(resp.Request) {', '\tif false {'))
mutate("a PUT with no body is still retried", "xhttp/xhttp.go", "./xhttp", "TestRetry_RewindableBodyStillRetried",
       swap('\treturn ok && r.Body != http.NoBody\n', '\treturn ok\n'))
mutate("closing clears the idle connections", "xhttp/xhttp.go", "./xhttp", "TestNew",
       swap('\treturn client, &clientCloser{pool: pool}, nil','\treturn client, &clientCloser{pool: traced(cfg, pool)}, nil'))
mutate("the elapsed time covers the whole logical request including retries", "xhttp/metric.go", "./xhttp", "TestMetric",
       swap('elapsed(resp.Request, resp.Time())','resp.Time()'))
# The call site: the field is in Config and in Validate, and without being assigned to the
# Transport it is an empty promise
mutate("MaxConnsPerHost is passed to the connection pool", "xhttp/xhttp.go", "./xhttp", "TestNew_MaxConnsPerHostApplied",
       swap('\tt.MaxConnsPerHost = cfg.MaxConnsPerHost\n', ''))
mutate("a negative MaxConnsPerHost is caught", "xhttp/config.go", "./xhttp", "TestValidate",
       swap(' || c.MaxConnsPerHost < 0 {', ' {'))
mutate("XHttp validates the TLS block", "xhttp/config.go", "./xhttp", "TestValidate_TLSBlock",
       swap('\treturn c.TLS.Validate()\n}', '\treturn nil\n}'))
mutate("XHttp's TLS block is handed to the connection pool", "xhttp/xhttp.go", "./xhttp", "TestNew_TLS",
       swap('\t\tt.TLSClientConfig = tlsCfg\n', ''))
# A silent failure bought with one minus sign: a negative DialTimeout makes every request i/o
# timeout on the spot, while a negative Timeout is taken by the standard library as "unlimited" and
# the timeout protection disappears entirely
mutate("a negative duration is caught", "xhttp/config.go", "./xhttp", "TestInitXHttp|TestValidate",
       swap('\t\tif d.val < 0 {', '\t\tif false {'))
mutate("the XHttp block is validated while the configuration is read", "xhttp/xhttp.go", "./xhttp", "TestInitXHttp",
       swap('xconfig.Unmarshal(ConfigKey, &c)', 'func() error { type raw Config; return xconfig.Unmarshal(ConfigKey, (*raw)(&c)) }()'))
mutate("an xhttp.New called directly validates too", "xhttp/xhttp.go", "./xhttp", "TestNew_DirectCallAlsoValidatesConfig",
       swap('\tif err := cfg.Validate(); err != nil {', '\tif err := cfg.Validate(); false && err != nil {'))
# XHttp.Trace governs Spans alone. Turning it off used to remove the injection as well, breaking
# the forwarded headers and traceparent at this hop
mutate("XHttp.Trace off still injects the trace identifiers and the forwarded headers", "xhttp/xhttp.go", "./xhttp", "TestNew_TraceOff",
       swap('next := http.RoundTripper(propagateOnly{next: pool})', 'next := pool'))
mutate("XHttp.Trace off still forwards by domain", "xhttp/xhttp.go", "./xhttp", "TestNew_TraceOff",
       swap('\treturn &xtrace.Transport{Next: next}\n', '\tif !cfg.Trace {\n\t\treturn next\n\t}\n\treturn &xtrace.Transport{Next: next}\n'))
mutate("the injection-only layer does not change the caller's request", "xhttp/xhttp.go", "./xhttp", "TestNew_TraceOff",
       swap('\tr = r.Clone(r.Context())\n', ''))
mutate("the injection-only layer forwards CloseIdleConnections", "xhttp/xhttp.go", "./xhttp", "TestNew_TraceOff",
       swap('func (p propagateOnly) CloseIdleConnections() {', 'func (p propagateOnly) closeIdleConnections() {'))
# The method is a free token, and copying it into a label lets anybody blow up the time series.
# Aimed at both call sites
mutate("the method label of the outbound metrics is bounded", "xhttp/metric.go", "./xhttp", "TestMetric",
       swap('normalizeMethod(raw.Method)', 'raw.Method', 2))
mutate("a failed outbound metric registration does not fail New", "xhttp/xhttp.go", "./xhttp", "TestNew_MetricRegisterFailureOnlyLogs_ClientStillUsable",
       swap('\t\t\tslog.Error("xhttp failed to register the request duration metric', '\t\t\treturn nil, nil, err\n\t\t\tslog.Error("xhttp failed to register the request duration metric'))
# resty's default logger goes around slog straight to stderr, printing the token in the query
# string along with a failed retry
mutate("resty's own logs go through slog", "xhttp/xhttp.go", "./xhttp", "TestNew_RestyLogsGoToSlogWithoutQuery",
       swap('return resty.NewWithClient(hc).SetLogger(restyLogger{quiet: quiet})', 'return resty.NewWithClient(hc)'))
mutate("resty's logs have the query string removed", "xhttp/xhttp.go", "./xhttp", "TestRestyLogger_TextScrubbed",
       swap('"detail", scrubText(fmt.Sprintf(format, args...))', '"detail", fmt.Sprintf(format, args...)'))
# When a redirect is refused the *url.Error's URL is the Location verbatim: with a space before
# the question mark, matching on the text does not recognize it
mutate("an error among resty's log arguments has its query string removed structurally", "xhttp/xhttp.go", "./xhttp", "TestNew_RestyLogsScrubErrorArgsStructurally",
       swap('\t\t\ta = scrubError(err)\n', '\t\t\ta = err\n'))
# otelhttp removes user:password alone and writes the query string into url.full as it is
mutate("an outbound Span's url.full carries no query string", "xhttp/xhttp.go", "./xhttp", "TestTransport_Span",
       swap('otelhttp.NewTransport(scrubURL{next: pool},', 'otelhttp.NewTransport(pool,'))
mutate("an outbound Span's url.full uses the URL with the query string removed", "xhttp/xhttp.go", "./xhttp", "TestTransport_Span",
       swap('attribute.String("url.full", bareURL(r.URL))', 'attribute.String("url.full", r.URL.String())'))
mutate("an outbound Span name uses the method alone", "xhttp/xhttp.go", "./xhttp", "TestTransport|TestSpanName",
       swap('\treturn r.Method\n}', '\treturn r.Method + " " + r.URL.Path\n}'))
# resty.New() brings a cookie jar along: requests before initialization and after closing would
# share a session somebody else set
mutate("the fallback instance carries no cookie jar", "xhttp/xhttp.go", "./xhttp", "TestC_",
       swap('var fallback = newResty(&http.Client{Timeout: fallbackTimeout}, false)', 'var fallback = resty.New().SetTimeout(fallbackTimeout)'))

section("The request log")
mutate("the outbound request log is installed only while Log is on", "xhttp/xhttp.go", "./xhttp", "TestLog_OffMeansNoRequestLinesAndRestyLogsUnchanged",
       swap('\tif cfg.Log {\n\t\tinstallLog(', '\tif true {\n\t\tinstallLog('))
mutate("the outbound slow threshold is handed to the log", "xhttp/xhttp.go", "./xhttp", "TestLog_SlowThreshold",
       swap('installLog(client, cfg.SlowThreshold)', 'installLog(client, 0)'))
# A *url.Error's text is Get "http://host/x?token=…": …, and the token in the query string lands
# in the log along with the error
mutate("the error in the outbound request log has its query string removed", "xhttp/log.go", "./xhttp", "TestLog_TransportError",
       swap('"error", scrubError(err)', '"error", err.Error()'))
mutate("the error in the outbound request log has its userinfo removed", "xhttp/log.go", "./xhttp", "TestScrubText",
       swap('\treturn urlUserinfo.ReplaceAllString(s, "$1")\n', '\treturn s\n'))
# When the redirect policy refuses, the URL is a relative Location whose query string holds an
# unescaped space: matching URLs on the text does not catch them all
mutate("a URL inside an error has its query string removed structurally", "xhttp/log.go", "./xhttp", "TestScrubError|TestLog_RedirectPolicyErrorsScrubbed",
       swap('if clean := cleanURL(ue.URL); clean != ue.URL {', 'if clean := ue.URL; false {'))
mutate("every branch of an errors.Join has its query string removed", "xhttp/log.go", "./xhttp", "TestScrubError",
       swap('\t\tcase interface{ Unwrap() []error }:\n', '\t\tcase interface {\n\t\t\tUnwrap() []error\n\t\t\tneverImplemented()\n\t\t}:\n'))
mutate("a URL that cannot be parsed is cut at the first question mark", "xhttp/log.go", "./xhttp", "TestScrubError",
       swap('if i := strings.IndexAny(raw, "?#"); i >= 0 {', 'if i := strings.IndexAny(raw, "?#"); false && i >= 0 {'))
mutate("a URL that cannot be parsed has its userinfo removed too", "xhttp/log.go", "./xhttp", "TestScrubError",
       swap("if at := strings.LastIndexByte(raw[start:end], '@'); at >= 0 {", "if at := strings.LastIndexByte(raw[start:end], '@'); false && at >= 0 {"))
mutate("a quoted URL's query string ends at the closing quote", "xhttp/log.go", "./xhttp", "TestScrubError",
       swap("\ts = quotedQuery.ReplaceAllString(s, `\"$1\"`)\n", ''))
# With the SRV lookup failing nothing is ever sent and Request.Time is the zero value:
# elapsed_ms 9223372036854.775
mutate("a request that was never sent logs an elapsed time of 0", "xhttp/log.go", "./xhttp", "TestLog_FailureBeforeAnyAttemptElapsedZero",
       swap('status, fallback := 0, sinceAttempt(req)', 'status, fallback := 0, time.Since(req.Time)'))
mutate("the outbound request log records the escaped form of the path", "xhttp/log.go", "./xhttp", "TestLog_PathIsEscaped",
       swap('return u.Host, u.EscapedPath()', 'return u.Host, u.Path'))
mutate("a request that is both slow and failed is logged as failed", "xhttp/log.go", "./xhttp", "TestLog_SlowAndFailedLogsFailed",
       swap('\tcase failed:\n', '\tcase failed && !isSlow:\n'))
mutate("failures and slow requests are still logged when slog only takes WARN", "xhttp/log.go", "./xhttp", "TestLog_WarnLevelHandlerStillGetsProblems",
       swap('if !failed && !isSlow && !slog.Default().Enabled(ctx, slog.LevelInfo) {', 'if !slog.Default().Enabled(ctx, slog.LevelInfo) {'))
mutate("the outbound request log keeps the elapsed time to the microsecond", "xhttp/log.go", "./xhttp", "TestMs_KeepsSubMillisecond",
       swap('func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }', 'func ms(d time.Duration) float64 { return float64(d.Milliseconds()) }'))
mutate("a 5xx is logged as a failure", "xhttp/log.go", "./xhttp", "TestLog_5xxWithRetriesConfiguredIsOneWarnLine",
       swap('failed := err != nil || status >= 500', 'failed := err != nil'))
# With the start recorded only while Metric is on, the elapsed time in a Metric: false client's log
# would be the last attempt alone
mutate("the elapsed time in the outbound request log covers the whole logical request", "xhttp/log.go", "./xhttp", "TestLog_ElapsedCoversRetriesWithMetricOff",
       swap('func installLog(client *resty.Client, slow time.Duration) {\n\tmarkStart(client)\n', 'func installLog(client *resty.Client, slow time.Duration) {\n'))
mutate("the outbound request log uses the caller's ctx", "xhttp/log.go", "./xhttp", "TestLog_UsesCallerCtx",
       swap('\tctx := req.Context()\n', '\tctx := resty.New().R().Context()\n'))
# The same thing is not said twice: on the retry path resty writes one WARN per attempt and one
# ERROR when the retries run out. Aimed at the call site
mutate("with Log on, resty's retry logs are no longer printed", "xhttp/xhttp.go", "./xhttp", "TestLog_TransportErrorAfterRetriesOneLineNoQuery",
       swap('\t}, cfg.Log)\n', '\t}, false)\n'))
mutate("with Log off, resty's retry logs are unchanged", "xhttp/xhttp.go", "./xhttp", "TestLog_OffMeansNoRequestLinesAndRestyLogsUnchanged",
       swap('\t}, cfg.Log)\n', '\t}, true)\n'))
mutate("with Log on, resty's per-attempt WARN is not printed", "xhttp/xhttp.go", "./xhttp", "TestLog_TransportErrorAfterRetriesOneLineNoQuery",
       swap('\tif l.quiet && format == "%v, Attempt %v" {\n', '\tif false {\n'))
mutate("with Log on, resty's out-of-retries ERROR is not printed", "xhttp/xhttp.go", "./xhttp", "TestLog_TransportErrorAfterRetriesOneLineNoQuery",
       swap('\tif l.quiet && calledFromExecute() {\n', '\tif false {\n'))
# The ERROR for an ignored configuration call has the format string "%v" too: going by the format
# string would swallow the one signal the caller can see
mutate("with Log on, resty's configuration-error ERROR is unchanged", "xhttp/xhttp.go", "./xhttp", "TestLog_RestyConfigErrorsStillLogged",
       swap('\tif l.quiet && calledFromExecute() {\n', '\tif l.quiet {\n'))
mutate("with Log on, resty's other warnings are unchanged", "xhttp/xhttp.go", "./xhttp", "TestLog_OtherRestyWarningsStillLogged",
       swap('\tif l.quiet && format == "%v, Attempt %v" {\n', '\tif l.quiet {\n'))
# The logger used to be swapped per request in OnBeforeRequest: a logger the caller set with
# SetLogger was swapped out too, and the Basic Auth warning never reached them
mutate("Log on does not swap out the caller's own logger", "xhttp/log.go", "./xhttp", "TestLog_UserLoggerStillGetsRestyWarnings",
       swap('\tmarkStart(client)\n\tclient.OnSuccess(', '\tmarkStart(client)\n\tclient.OnBeforeRequest(func(_ *resty.Client, req *resty.Request) error {\n\t\treq.SetLogger(restyLogger{quiet: true})\n\t\treturn nil\n\t})\n\tclient.OnSuccess('))
mutate("a negative XHttp SlowThreshold is caught", "xhttp/config.go", "./xhttp", "TestConfig_LogDefaults",
       swap('\t\t{"SlowThreshold", c.SlowThreshold},\n', ''))
