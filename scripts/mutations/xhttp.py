# xhttp 的变异：module xhttp 里的承诺。写法见 scripts/mutations/__init__.py
from . import cut, mutate, section, swap

section("客户端")
# 挂上重试条件，resty 自己「不重试」的判断就作废了：200 + 坏 JSON 会被同一个 GET 重发
mutate("只重试传输层的错", "xhttp/xhttp.go", "./xhttp", "TestRetry",
       swap('\tif !isTransportError(err) {\n', '\tif err == nil {\n'))
# 超时分不出请求到没到服务端，重发 POST 就是重复下单：打在挂条件的调用点和方法判断上
mutate("POST 不重试", "xhttp/xhttp.go", "./xhttp", "TestRetry",
       swap('\tif _, ok := idempotentMethods[method]; ok || !p.onlyIdempotent {', '\tif true {'))
# resty 的条件是「或」：使用者挂一个 err != nil 就重试的条件，POST 的重试就回来了。veto 挂在 RetryAfter 上兜住
mutate("使用者的重试条件放不回 POST 的重试", "xhttp/xhttp.go", "./xhttp", "TestRetry_UserCondition|TestRetry_UserStatusConditionWithConsumedReader",
       swap('\t\t\tAddRetryCondition(p.condition).\n\t\t\tSetRetryAfter(p.veto)\n', '\t\t\tAddRetryCondition(p.condition)\n'))
# 拿到了响应时否决，调用方拿到的就是我们编出来的错误，不是那个 503
mutate("拿到了响应不因为方法否决", "xhttp/xhttp.go", "./xhttp", "TestRetry_UserStatusConditionNotVetoedAndNoBogusError",
       swap('if err == nil || errors.Is(err, errNotIdempotent) && resp != nil && resp.RawResponse != nil {', 'if err == nil {'))
# resty 每次尝试都拿同一个 io.Reader 重建请求：第二次发出去 0 字节、服务端回 200
mutate("读过的 io.Reader 不重发", "xhttp/xhttp.go", "./xhttp", "TestRetry_ConsumedReaderBodyNotResent|TestRetry_UserStatusConditionWithConsumedReader",
       swap('\tif consumedReader(resp.Request) {', '\tif false {'))
mutate("没有 body 的 PUT 照旧重试", "xhttp/xhttp.go", "./xhttp", "TestRetry_RewindableBodyStillRetried",
       swap('\treturn ok && r.Body != http.NoBody\n', '\treturn ok\n'))
mutate("关闭时清掉空闲连接", "xhttp/xhttp.go", "./xhttp", "TestNew",
       swap('\treturn client, &clientCloser{pool: pool}, nil','\treturn client, &clientCloser{pool: traced(cfg, pool)}, nil'))
mutate("重试耗时算整次逻辑请求", "xhttp/metric.go", "./xhttp", "TestMetric",
       swap('elapsed(resp.Request, resp.Time())','resp.Time()'))
# 调用点：字段在 Config 里、Validate 里都有，没赋给 Transport 就是一句空话
mutate("MaxConnsPerHost 传给连接池", "xhttp/xhttp.go", "./xhttp", "TestNew_MaxConnsPerHostApplied",
       swap('\tt.MaxConnsPerHost = cfg.MaxConnsPerHost\n', ''))
mutate("MaxConnsPerHost 为负要被拦住", "xhttp/config.go", "./xhttp", "TestValidate",
       swap(' || c.MaxConnsPerHost < 0 {', ' {'))
mutate("XHttp 校验 TLS 块", "xhttp/config.go", "./xhttp", "TestValidate_TLSBlock",
       swap('\treturn c.TLS.Validate()\n}', '\treturn nil\n}'))
mutate("XHttp 的 TLS 块交给连接池", "xhttp/xhttp.go", "./xhttp", "TestNew_TLS",
       swap('\t\tt.TLSClientConfig = tlsCfg\n', ''))
# 一个减号换来的静默故障：DialTimeout 为负时每次请求当场 i/o timeout，
# Timeout 为负反而被标准库当成「不限时」，超时保护整个消失
mutate("负的时长要被拦住", "xhttp/config.go", "./xhttp", "TestInitXHttp|TestValidate",
       swap('\t\tif d.val < 0 {', '\t\tif false {'))
mutate("XHttp 块在读配置时就校验", "xhttp/xhttp.go", "./xhttp", "TestInitXHttp",
       swap('xconfig.Unmarshal(ConfigKey, &c)', 'func() error { type raw Config; return xconfig.Unmarshal(ConfigKey, (*raw)(&c)) }()'))
mutate("直接调 xhttp.New 也校验", "xhttp/xhttp.go", "./xhttp", "TestNew_DirectCallAlsoValidatesConfig",
       swap('\tif err := cfg.Validate(); err != nil {', '\tif err := cfg.Validate(); false && err != nil {'))
# XHttp.Trace 只管 Span。原先关掉它连注入一起摘了，透传头和 traceparent 断在这一跳
mutate("XHttp.Trace 关掉照样注入链路标识和透传头", "xhttp/xhttp.go", "./xhttp", "TestNew_TraceOff",
       swap('next := http.RoundTripper(propagateOnly{next: pool})', 'next := pool'))
mutate("XHttp.Trace 关掉照样按域名透传", "xhttp/xhttp.go", "./xhttp", "TestNew_TraceOff",
       swap('\treturn &xtrace.Transport{Next: next}\n', '\tif !cfg.Trace {\n\t\treturn next\n\t}\n\treturn &xtrace.Transport{Next: next}\n'))
mutate("只注入那一层不改调用方的请求", "xhttp/xhttp.go", "./xhttp", "TestNew_TraceOff",
       swap('\tr = r.Clone(r.Context())\n', ''))
mutate("只注入那一层转发 CloseIdleConnections", "xhttp/xhttp.go", "./xhttp", "TestNew_TraceOff",
       swap('func (p propagateOnly) CloseIdleConnections() {', 'func (p propagateOnly) closeIdleConnections() {'))
# 方法是自由 token，照抄进标签的话谁都能把时间序列撑爆。打在两个调用点上
mutate("出站指标的 method 标签收敛", "xhttp/metric.go", "./xhttp", "TestMetric",
       swap('normalizeMethod(raw.Method)', 'raw.Method', 2))
mutate("出站指标注册失败不让 New 失败", "xhttp/xhttp.go", "./xhttp", "TestNew_MetricRegisterFailureOnlyLogs_ClientStillUsable",
       swap('\t\t\tslog.Error("xhttp failed to register the request duration metric', '\t\t\treturn nil, nil, err\n\t\t\tslog.Error("xhttp failed to register the request duration metric'))
# resty 的默认 logger 绕开 slog 直写 stderr，重试失败时连查询串里的令牌一起打
mutate("resty 自己的日志走 slog", "xhttp/xhttp.go", "./xhttp", "TestNew_RestyLogsGoToSlogWithoutQuery",
       swap('return resty.NewWithClient(hc).SetLogger(restyLogger{quiet: quiet})', 'return resty.NewWithClient(hc)'))
mutate("resty 日志去掉查询串", "xhttp/xhttp.go", "./xhttp", "TestRestyLogger_TextScrubbed",
       swap('"detail", scrubText(fmt.Sprintf(format, args...))', '"detail", fmt.Sprintf(format, args...)'))
# 重定向被拒时 *url.Error 的 URL 是 Location 原样：问号前有空格的，按文本认不出来
mutate("resty 日志参数里的错误按结构去掉查询串", "xhttp/xhttp.go", "./xhttp", "TestNew_RestyLogsScrubErrorArgsStructurally",
       swap('\t\t\ta = scrubError(err)\n', '\t\t\ta = err\n'))
# otelhttp 只去掉 user:password，查询串原样写进 url.full
mutate("出站 Span 的 url.full 不带查询串", "xhttp/xhttp.go", "./xhttp", "TestTransport_Span",
       swap('otelhttp.NewTransport(scrubURL{next: pool},', 'otelhttp.NewTransport(pool,'))
mutate("出站 Span 的 url.full 用去掉查询串的 URL", "xhttp/xhttp.go", "./xhttp", "TestTransport_Span",
       swap('attribute.String("url.full", bareURL(r.URL))', 'attribute.String("url.full", r.URL.String())'))
mutate("出站 Span 名只用方法", "xhttp/xhttp.go", "./xhttp", "TestTransport|TestSpanName",
       swap('\treturn r.Method\n}', '\treturn r.Method + " " + r.URL.Path\n}'))
# resty.New() 自带 cookie jar：初始化前、关闭后的请求会共享别人种下的会话
mutate("兜底实例不带 cookie jar", "xhttp/xhttp.go", "./xhttp", "TestC_",
       swap('var fallback = newResty(&http.Client{Timeout: fallbackTimeout}, false)', 'var fallback = resty.New().SetTimeout(fallbackTimeout)'))

section("请求日志")
mutate("出站请求日志只在 Log 开着时挂", "xhttp/xhttp.go", "./xhttp", "TestLog_OffMeansNoRequestLinesAndRestyLogsUnchanged",
       swap('\tif cfg.Log {\n\t\tinstallLog(', '\tif true {\n\t\tinstallLog('))
mutate("出站慢请求阈值交给日志", "xhttp/xhttp.go", "./xhttp", "TestLog_SlowThreshold",
       swap('installLog(client, cfg.SlowThreshold)', 'installLog(client, 0)'))
# *url.Error 的原文是 Get "http://host/x?token=…": …，查询串里的令牌跟着错误进了日志
mutate("出站请求日志的错误去掉查询串", "xhttp/log.go", "./xhttp", "TestLog_TransportError",
       swap('"error", scrubError(err)', '"error", err.Error()'))
mutate("出站请求日志的错误去掉 userinfo", "xhttp/log.go", "./xhttp", "TestScrubText",
       swap('\treturn urlUserinfo.ReplaceAllString(s, "$1")\n', '\treturn s\n'))
# 重定向策略拒绝时 URL 是相对的 Location、查询串里有没转义的空格：按文本找 URL 都认不全
mutate("错误里的 URL 按结构去掉查询串", "xhttp/log.go", "./xhttp", "TestScrubError|TestLog_RedirectPolicyErrorsScrubbed",
       swap('if clean := cleanURL(ue.URL); clean != ue.URL {', 'if clean := ue.URL; false {'))
mutate("errors.Join 的每一支都去掉查询串", "xhttp/log.go", "./xhttp", "TestScrubError",
       swap('\t\tcase interface{ Unwrap() []error }:\n', '\t\tcase interface {\n\t\t\tUnwrap() []error\n\t\t\tneverImplemented()\n\t\t}:\n'))
mutate("解析不了的 URL 切在第一个问号上", "xhttp/log.go", "./xhttp", "TestScrubError",
       swap('if i := strings.IndexAny(raw, "?#"); i >= 0 {', 'if i := strings.IndexAny(raw, "?#"); false && i >= 0 {'))
mutate("解析不了的 URL 也去掉 userinfo", "xhttp/log.go", "./xhttp", "TestScrubError",
       swap("if at := strings.LastIndexByte(raw[start:end], '@'); at >= 0 {", "if at := strings.LastIndexByte(raw[start:end], '@'); false && at >= 0 {"))
mutate("引号里的 URL 查询串到右引号为止", "xhttp/log.go", "./xhttp", "TestScrubError",
       swap("\ts = quotedQuery.ReplaceAllString(s, `\"$1\"`)\n", ''))
# SRV 查不到时一次都没发出去，Request.Time 是零值：elapsed_ms 9223372036854.775
mutate("一次都没发出去的请求耗时记 0", "xhttp/log.go", "./xhttp", "TestLog_FailureBeforeAnyAttemptElapsedZero",
       swap('status, fallback := 0, sinceAttempt(req)', 'status, fallback := 0, time.Since(req.Time)'))
mutate("出站请求日志的路径记转义过的形式", "xhttp/log.go", "./xhttp", "TestLog_PathIsEscaped",
       swap('return u.Host, u.EscapedPath()', 'return u.Host, u.Path'))
mutate("又慢又失败的请求记失败", "xhttp/log.go", "./xhttp", "TestLog_SlowAndFailedLogsFailed",
       swap('\tcase failed:\n', '\tcase failed && !isSlow:\n'))
mutate("slog 只收 WARN 时照样记失败和慢请求", "xhttp/log.go", "./xhttp", "TestLog_WarnLevelHandlerStillGetsProblems",
       swap('if !failed && !isSlow && !slog.Default().Enabled(ctx, slog.LevelInfo) {', 'if !slog.Default().Enabled(ctx, slog.LevelInfo) {'))
mutate("出站请求日志的耗时保留到微秒", "xhttp/log.go", "./xhttp", "TestMs_KeepsSubMillisecond",
       swap('func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }', 'func ms(d time.Duration) float64 { return float64(d.Milliseconds()) }'))
mutate("5xx 记成失败", "xhttp/log.go", "./xhttp", "TestLog_5xxWithRetriesConfiguredIsOneWarnLine",
       swap('failed := err != nil || status >= 500', 'failed := err != nil'))
# 起点只在 Metric 开着时记的话，Metric: false 的客户端日志里的耗时只是最后一次尝试
mutate("出站请求日志的耗时算整次逻辑请求", "xhttp/log.go", "./xhttp", "TestLog_ElapsedCoversRetriesWithMetricOff",
       swap('func installLog(client *resty.Client, slow time.Duration) {\n\tmarkStart(client)\n', 'func installLog(client *resty.Client, slow time.Duration) {\n'))
mutate("出站请求日志用调用方的 ctx", "xhttp/log.go", "./xhttp", "TestLog_UsesCallerCtx",
       swap('\tctx := req.Context()\n', '\tctx := resty.New().R().Context()\n'))
# 同一件事不说两遍：resty 在重试路径上每次尝试一行 WARN、用完一行 ERROR。打在调用点上
mutate("Log 开着时不再打 resty 的重试日志", "xhttp/xhttp.go", "./xhttp", "TestLog_TransportErrorAfterRetriesOneLineNoQuery",
       swap('\t}, cfg.Log)\n', '\t}, false)\n'))
mutate("Log 关着时 resty 的重试日志照旧", "xhttp/xhttp.go", "./xhttp", "TestLog_OffMeansNoRequestLinesAndRestyLogsUnchanged",
       swap('\t}, cfg.Log)\n', '\t}, true)\n'))
mutate("Log 开着时不打 resty 每次尝试的 WARN", "xhttp/xhttp.go", "./xhttp", "TestLog_TransportErrorAfterRetriesOneLineNoQuery",
       swap('\tif l.quiet && format == "%v, Attempt %v" {\n', '\tif false {\n'))
mutate("Log 开着时不打 resty 重试用完的 ERROR", "xhttp/xhttp.go", "./xhttp", "TestLog_TransportErrorAfterRetriesOneLineNoQuery",
       swap('\tif l.quiet && calledFromExecute() {\n', '\tif false {\n'))
# 配置调用被忽略时的 ERROR 格式串也是 "%v"：按格式串认就把使用者唯一能看到的信号一起吞了
mutate("Log 开着时 resty 配置出错的 ERROR 照旧", "xhttp/xhttp.go", "./xhttp", "TestLog_RestyConfigErrorsStillLogged",
       swap('\tif l.quiet && calledFromExecute() {\n', '\tif l.quiet {\n'))
mutate("Log 开着时 resty 的其余提醒照旧", "xhttp/xhttp.go", "./xhttp", "TestLog_OtherRestyWarningsStillLogged",
       swap('\tif l.quiet && format == "%v, Attempt %v" {\n', '\tif l.quiet {\n'))
# 原先在 OnBeforeRequest 里给每个请求换 logger：使用者自己 SetLogger 的也被换掉，Basic Auth 的提醒到不了他那里
mutate("Log 开着不换掉使用者自己的 logger", "xhttp/log.go", "./xhttp", "TestLog_UserLoggerStillGetsRestyWarnings",
       swap('\tmarkStart(client)\n\tclient.OnSuccess(', '\tmarkStart(client)\n\tclient.OnBeforeRequest(func(_ *resty.Client, req *resty.Request) error {\n\t\treq.SetLogger(restyLogger{quiet: true})\n\t\treturn nil\n\t})\n\tclient.OnSuccess('))
mutate("XHttp SlowThreshold 为负要被拦住", "xhttp/config.go", "./xhttp", "TestConfig_LogDefaults",
       swap('\t\t{"SlowThreshold", c.SlowThreshold},\n', ''))
