# xgin 的变异：module xgin 里的承诺。写法见 scripts/mutations/__init__.py
from . import cut, mutate, section, swap

section("HTTP 服务")
# 等多久只看调用方的 ctx（xone.Run 给的是服务那一段停止预算）。换掉它的话
# 挂住的请求让 Stop 一直不返回，「整个退出流程只有一份预算」就成了空话
mutate("服务不超过调用方给的截止时间", "internal/web/server.go", "./xgin", "TestStop",
       swap('shutCtx, cancel := shutdownCtx(ctx)', 'shutCtx, cancel := shutdownCtx(context.WithoutCancel(ctx))'))
mutate("超时后强制断掉在途连接", "internal/web/server.go", "./xgin", "TestStop",
       swap('\t\tif cerr := srv.Close(); cerr != nil {\n\t\t\tslog.Warn(module+" force close failed", "error", cerr)\n\t\t}\n',''))
# Close 只关连接、取消请求的 ctx，handler 的协程照跑。Close 完就返回的话，
# 正在收尾的 handler 还没返回，框架就去关数据库了
mutate("断连之后等 handler 真正返回", "internal/web/server.go", "./xgin", "TestStop_WaitsForHandlersAfterForceClose|TestStop_HandlerIgnoringCtxReportsRemainingCount",
       swap('if n := s.waitHandlers(ctx); n > 0 {', 'if n := int64(0); n > 0 {'))
mutate("每个请求都记进在途计数", "internal/web/server.go", "./xgin", "TestStop_WaitsForHandlersAfterForceClose",
       swap('Handler:           s.track(h),', 'Handler:           h,'))
# 调用点：XGin 的 Start / Stop 只是转给 web.Server。Stop 不转的话，退出信号到了服务照跑
mutate("XGin.Stop 转给 web.Server", "xgin/xgin.go", "./xgin", "TestStop",
       swap('{ return g.server.Stop(ctx, "xgin") }', '{ return nil }'))
# Shutdown 用满全部时间的话，Close 落下时预算已经花完，收尾的 handler 没人等
mutate("Shutdown 给等 handler 留出一截", "internal/web/server.go", "./xgin", "TestStop_WaitsForHandlersAfterForceClose",
       swap('shutCtx, cancel := shutdownCtx(ctx)', 'shutCtx, cancel := context.WithCancel(ctx)'))
mutate("内置路由也走用户中间件", "xgin/xgin.go", "./xgin", "TestBuild",
       swap('\t\te.Use(middleware.Recover(g.recover))\n\t\te.Use(g.extra...)\n', '\t\te.Use(middleware.Recover(g.recover))\n'),
       swap('\t\tfor _, f := range g.routes {', '\t\te.Use(g.extra...)\n\t\tfor _, f := range g.routes {'))
mutate("公网对端的 X-Forwarded-For 不认", "xgin/xgin.go", "./xgin", "TestBuild|TestLog",
       cut('\tif err := e.SetTrustedProxies(web.ExpandProxies(c.TrustedProxies)); err != nil {',
    '_ = e.SetTrustedProxies([]string{})\n\t}\n'))
# h2c 原先靠 x/net 的 h2c.NewHandler：连接被劫持，Shutdown 约 100µs 就返回 nil、
# 在途请求照跑，框架紧接着去关数据库。换回那个写法，这条承诺就没了
mutate("h2c 的在途请求也等它做完", "internal/web/server.go", "./xgin", "TestStop_H2C",
       swap('\t"github.com/jupiter-eng/xone/xerror"\n', '\t"github.com/jupiter-eng/xone/xerror"\n\t"golang.org/x/net/http2"\n\t"golang.org/x/net/http2/h2c"\n'),
       swap('\t\tHandler:           s.track(h),\n\t\tProtocols:         protocols(c),\n',
     '\t\tHandler:           h2c.NewHandler(s.track(h), &http2.Server{}),\n'))
# 调用点：UseH2C 要从 XGin 的配置抄给 web.Server，漏抄的话配了也是 HTTP/1.1
mutate("UseH2C 交给了 web.Server", "xgin/config.go", "./xgin", "TestStop_H2C",
       swap('\t\tUseH2C:            c.UseH2C,\n', ''))
mutate("不开 UseH2C 时不接受明文 HTTP/2", "internal/web/server.go", "./xgin", "TestStart_CleartextHTTP2RejectedWithoutH2C",
       swap('p.SetUnencryptedHTTP2(c.UseH2C && !c.tlsEnabled())', 'p.SetUnencryptedHTTP2(true)'))
# net/http 把 ReadHeaderTimeout 的 0 当成「退到 ReadTimeout」，而后者默认也是 0：
# 慢连接攻击的主要防线整个消失，配置文件看上去只是写了个 0
mutate("读请求头超时写 0 要拦住", "internal/web/config.go", "./xgin", "TestValidate", swap('\t\tif d.val <= 0 {', '\t\tif d.val < 0 {'))
mutate("负的读写超时要拦住", "internal/web/config.go", "./xgin", "TestValidate",
       swap('if c.ReadTimeout < 0 || c.WriteTimeout < 0 {', 'if false {'))
# 调用点：规矩写在 internal/web，XGin 的 Validate 得真的调到它们（先后也不变）
mutate("Validate 查了端口和 TLS", "xgin/config.go", "./xgin", "TestValidate",
       swap('\tif err := s.ValidateListen(); err != nil {\n\t\treturn err\n\t}\n', ''))
mutate("Validate 查了超时", "xgin/config.go", "./xgin", "TestValidate",
       swap('\tif err := s.ValidateTimeouts(); err != nil {\n\t\treturn err\n\t}\n', ''))
mutate("Validate 查了代理网段", "xgin/config.go", "./xgin", "TestValidate",
       swap('\tif err := web.ValidateProxies(c.TrustedProxies); err != nil {\n\t\treturn err\n\t}\n', ''))
# gin 不拒绝不以 / 开头的路径，而是悄悄改写：留空就把指标挂到了根路径 / 上
mutate("指标路径不以 / 开头要启动失败", "xgin/config.go", "./xgin", "TestValidate|TestLoadConfig",
       swap('if c.Metric && !strings.HasPrefix(c.MetricPath, "/") {', 'if false && !strings.HasPrefix(c.MetricPath, "/") {'))
# 配置文件里的 XGin 块由 StageServer 的钩子认领并校验（Unmarshal 调 Validate）。
# 丢掉这个错误的话，配错的值要拖到服务 Start 才报出来，那时别的组件都已经连好了
mutate("配错的 XGin 块在启动阶段就失败", "xgin/xgin.go", "./xgin", "TestLoadConfig",
       swap('\t_, err := fileConfig()\n\treturn err\n', '\t_, _ = fileConfig()\n\treturn nil\n'))
# CurrentConfig 和装配都靠它兜底：解到一半的非法配置里 TrustedProxies 可能正是 0.0.0.0/0
mutate("XGin 块不合法时退回默认值", "xgin/xgin.go", "./xgin", "TestCurrentConfig|TestEngine_InvalidConfigFallsBackToSafeDefaults",
       swap('\tif err := xconfig.Unmarshal(ConfigKey, &c); err != nil {\n\t\treturn DefaultConfig(), err\n',
     '\tif err := xconfig.Unmarshal(ConfigKey, &c); err != nil {\n\t\treturn c, err\n'))
mutate("WithConfig 给的那份也要校验", "xgin/xgin.go", "./xgin", "TestStart_DoesNotListenOnInvalidConfig",
       swap('\tif err := g.override.Validate(); err != nil {', '\tif err := g.override.Validate(); false && err != nil {'))
# 监听地址、TLS 和 engine 上的中间件、信任的代理必须出自同一份配置。
# Start 自己再读一遍的话，装配之后换过的配置只生效一半
mutate("Start 用的是装配时的那份配置", "xgin/xgin.go", "./xgin", "TestStart_UsesConfigFromBuildTime",
       swap('\tc := g.conf\n', '\tc, _ := g.cfg()\n'))
# gin.New 在 debug 模式下会打一段「切到 release」的警告。先建后设的话，
# 配成 release 的服务照样打出它（使用者的二进制里没设 GIN_MODE 时默认就是 debug）
mutate("Mode 在建 engine 之前设", "xgin/xgin.go", "./xgin", "TestBuild_Mode",
       swap('\t\tgin.SetMode(c.Mode)\n\t\te := gin.New()\n', '\t\te := gin.New()\n\t\tgin.SetMode(c.Mode)\n'))

section("中间件")
mutate("代理网段写错要启动失败", "internal/web/proxy.go", "./xgin", "TestValidate", swap('if p != TrustPrivate && !isIPOrCIDR(p) {','if false {'))
# xecho 没有 gin 可用，client_ip 按 internal/web 的 Proxies.ClientIP 算；它和 gin 的规则一致由这里逐条比对着。
# 改坏规则里的任何一处（这里挑「全都可信时取最左边」），和 gin 的结果就对不上
mutate("共用的 client_ip 规则和 gin 一致", "internal/web/proxy.go", "./xgin", "TestClientIP_SharedRuleMatchesGin",
       swap('if i == 0 || !ps.contains(ip) {', 'if !ps.contains(ip) {'))
# 词表进程里只有一张（internal/web）：公开的这两个名字不转过去的话，使用者补的词谁都不认
mutate("AddSensitiveFields 写进共用的词表", "xgin/middleware/redact.go", "./xgin", "TestAddSensitive",
       swap('{ web.AddSensitiveFields(fields...) }', '{}'))
mutate("AddSensitiveHeaders 写进共用的名单", "xgin/middleware/redact.go", "./xgin", "TestAddSensitive",
       swap('{ web.AddSensitiveHeaders(headers...) }', '{}'))
mutate("指标的 method 标签收敛", "xgin/middleware/metric.go", "./xgin", "TestMetric",
       swap('web.NormalizeMethod(c.Request.Method)', 'c.Request.Method'),
       swap('\t\tlatency *prometheus.HistogramVec\n\t)\n', '\t\tlatency *prometheus.HistogramVec\n\t)\n\t_ = web.NormalizeMethod\n'))
mutate("配置在装配时落到 engine 上", "xgin/xgin.go", "./xgin", "TestBuild", swap('\t\tapplyConfig(e, c)\n', ''))
# 回调在配置落到 engine 上之后才跑，所以回调里明确设了的以回调为准。
# 两种改坏的写法：配置挪到回调之后落，或者 Start 时再落一遍（原先就是这样）
mutate("回调里的 engine 设置盖得过配置", "xgin/xgin.go", "./xgin", "TestBuild_CallbackEngineSettingsOverrideConfig",
       swap('\t\tapplyConfig(e, c)\n', ''),
       swap('\t\tfor _, f := range g.routes {\n\t\t\tf(e)\n\t\t}\n', '\t\tfor _, f := range g.routes {\n\t\t\tf(e)\n\t\t}\n\t\tapplyConfig(e, c)\n'))
mutate("Start 不再把配置落一遍", "xgin/xgin.go", "./xgin", "TestBuild_CallbackEngineSettingsOverrideConfig",
       swap('\tc := g.conf\n', '\tc := g.conf\n\tapplyConfig(g.engine, c)\n'))
# 开关写在配置里，读它们的是装配里的这几处。开关接不上是最难发现的一类 bug：
# 程序照常跑，只是那一项从来没生效
mutate("访问日志的开关读的是配置", "xgin/xgin.go", "./xgin", "TestLog_", swap('\t\tif c.Log {\n', '\t\tif true {\n', 2))
mutate("链路的开关读的是配置", "xgin/xgin.go", "./xgin", "TestTrace_", swap('\t\tif c.Trace {\n', '\t\tif true {\n'))
mutate("关掉指标就不挂指标中间件", "xgin/xgin.go", "./xgin", "TestMetric_",
       swap('\t\tif c.Metric {\n\t\t\te.Use(middleware.Metric())', '\t\tif true {\n\t\t\te.Use(middleware.Metric())'))
mutate("关掉指标就不注册端点", "xgin/xgin.go", "./xgin", "TestBuild_NoMetricsEndpointWhenDisabled",
       swap('\t\tif c.Metric {\n\t\t\te.GET(c.MetricPath, serveMetrics)', '\t\tif true {\n\t\t\te.GET(c.MetricPath, serveMetrics)'))
mutate("指标端点挂在配置的路径上", "xgin/xgin.go", "./xgin", "TestBuild_MetricsPathConfigurable",
       swap('e.GET(c.MetricPath, serveMetrics)', 'e.GET("/metrics", serveMetrics)'))
mutate("跳过日志的路径读的是配置", "xgin/xgin.go", "./xgin", "TestLogSkipPaths",
       swap('skip := slices.Clone(c.LogSkipPaths)', 'skip := slices.Clone([]string{})'))
# 接反了的话，只开了请求体的人，响应体（可能带着令牌）进了日志
mutate("请求体和响应体的开关各管各的", "xgin/xgin.go", "./xgin", "TestLogBody",
       swap('middleware.WithBody(c.LogRequestBody, c.LogResponseBody)', 'middleware.WithBody(c.LogResponseBody, c.LogRequestBody)'))
# 开关各自接到对应的选项上：接成 true 就是默认把查询串、Authorization、Set-Cookie 写进日志
mutate("查询串的开关读的是配置", "xgin/xgin.go", "./xgin", "TestLogQueryAndHeaders",
       swap('middleware.WithQuery(c.LogQuery)', 'middleware.WithQuery(true)'))
mutate("请求头和响应头的开关各管各的", "xgin/xgin.go", "./xgin", "TestLogQueryAndHeaders",
       swap('middleware.WithHeaders(c.LogRequestHeaders, c.LogResponseHeaders)', 'middleware.WithHeaders(c.LogResponseHeaders, c.LogRequestHeaders)'))
mutate("中文翻译的开关读的是配置", "xgin/xgin.go", "./xgin", "TestZHTranslations",
       swap('\t\tif c.ZHTranslations {', '\t\tif false {'))
mutate("查询串不进访问日志", "internal/web/accesslog.go", "./xgin", "TestLog",
       swap('slog.String("path", r.URL.Path),', 'slog.String("path", r.URL.RequestURI()),'))
mutate("记下的查询串脱过敏", "internal/web/accesslog.go", "./xgin", "TestLog_Query",
       swap('slog.String("query", redactForm(r.URL.RawQuery))', 'slog.String("query", r.URL.RawQuery)'))
mutate("记下的请求头脱过敏", "internal/web/accesslog.go", "./xgin", "TestLog_RedactsRequestHeaders",
       swap('Value: RedactHeaders(r.Header)}', 'Value: slog.AnyValue(r.Header)}'))
mutate("记下的响应头脱过敏", "internal/web/accesslog.go", "./xgin", "TestLog_ResponseHeaders",
       swap('Value: RedactHeaders(a.RespHeader)}', 'Value: slog.AnyValue(a.RespHeader)}'))
# 调用点：响应头要从 gin 的 writer 取来交过去，不交的话记下的永远是空的
mutate("访问日志拿到的是真实的响应头", "xgin/middleware/log.go", "./xgin", "TestLog_ResponseHeaders",
       swap('RespHeader: c.Writer.Header(),', 'RespHeader: map[string][]string{},'))
# 级别关着时整套捕获都不做：等 slog 自己判级别时，缓存请求体、截响应的代价已经付完了
mutate("日志级别关着时访问日志什么都不做", "internal/web/accesslog.go", "./xgin", "TestLog_DoesNoWorkWhenLevelDisabled",
       swap(' || !slog.Default().Enabled(r.Context(), slog.LevelInfo)', ''))
mutate("访问日志带上 gin 登记的错误", "xgin/middleware/log.go", "./xgin", "TestLog_ErrAbortHandlerAbortLoggedAs499",
       swap('\t\t\t\ta.Errors = c.Errors.String()\n', ''))
# ErrAbortHandler 是「断掉这个连接」的约定写法。兜住它的话本该中止的响应
# 被写成 500 发出去，还多一份毫无意义的 panic 栈
mutate("ErrAbortHandler 原样抛给 net/http", "xgin/middleware/middleware.go", "./xgin", "TestRecover",
       cut('\t\t\tif err == http.ErrAbortHandler {\n', '\t\t\t\tpanic(err)\n\t\t\t}\n'))
# 中止的请求往往已经写出了 200 的响应头：照读 c.Writer.Status() 的话，
# 被截断的响应在访问日志、指标、链路里都是一次成功
mutate("ErrAbortHandler 中止的请求登记下来", "xgin/middleware/middleware.go", "./xgin", "TestLog_ErrAbortHandlerAbortLoggedAs499|TestMetric_ErrAbortHandlerAbortRecordedAs499|TestTrace_ErrAbortHandlerAbortRecordedAsError",
       swap('\t\t\t\t_ = c.Error(http.ErrAbortHandler) //nolint:errcheck // 只是登记\n', ''))
mutate("访问日志把中止的请求记成 499", "xgin/middleware/log.go", "./xgin", "TestLog_ErrAbortHandler",
       swap('Status:   status(c, ctx),', 'Status:   c.Writer.Status(),'), swap('\t\tstart, ctx := time.Now(), c.Request.Context()\n', '\t\tstart, ctx := time.Now(), c.Request.Context()\n\t\t_ = ctx\n'))
mutate("指标把中止的请求记成 499", "xgin/middleware/metric.go", "./xgin", "TestMetric_ErrAbortHandler",
       swap('strconv.Itoa(status(c, ctx))', 'strconv.Itoa(c.Writer.Status())'), swap('\t\tstart, ctx := time.Now(), c.Request.Context()\n', '\t\tstart, ctx := time.Now(), c.Request.Context()\n\t\t_ = ctx\n'))
mutate("链路把中止的请求记成错误", "xgin/middleware/trace.go", "./xgin", "TestTrace_ErrAbortHandler",
       swap('st := status(c, ctx)', 'st := c.Writer.Status()'))
# 客户端走了、什么都没写就返回：原先记成 bytes_out 为 0 的 200，客户端其实什么都没收到
mutate("XGin 客户端走了记 499", "xgin/middleware/middleware.go", "./xgin", "TestClientGoneBeforeAnythingSentRecordedAs499",
       swap('\tif aborted(c) || (!c.Writer.Written() && web.ClientGone(ctx)) {', '\tif aborted(c) {'))
mutate("XGin 响应写出去了就记写出去的", "xgin/middleware/middleware.go", "./xgin", "TestClientGoneAfterResponseWrittenKeepsSentStatus",
       swap('\tif aborted(c) || (!c.Writer.Written() && web.ClientGone(ctx)) {', '\tif aborted(c) || web.ClientGone(ctx) {'))
# 判的是这一层一进来时的 ctx：里面几层换上的、返回时 cancel 掉的 ctx 不算客户端走了。三个调用点各一条
mutate("XGin 访问日志按一进来时的 ctx 判客户端", "xgin/middleware/log.go", "./xgin", "TestClientGoneIgnoresContextsSetByInnerMiddleware",
       swap('Status:   status(c, ctx),', 'Status:   status(c, c.Request.Context()),'), swap('\t\tstart, ctx := time.Now(), c.Request.Context()\n', '\t\tstart, ctx := time.Now(), c.Request.Context()\n\t\t_ = ctx\n'))
mutate("XGin 指标按一进来时的 ctx 判客户端", "xgin/middleware/metric.go", "./xgin", "TestClientGoneIgnoresContextsSetByInnerMiddleware",
       swap('strconv.Itoa(status(c, ctx))', 'strconv.Itoa(status(c, c.Request.Context()))'), swap('\t\tstart, ctx := time.Now(), c.Request.Context()\n', '\t\tstart, ctx := time.Now(), c.Request.Context()\n\t\t_ = ctx\n'))
mutate("XGin 链路按一进来时的 ctx 判客户端", "xgin/middleware/trace.go", "./xgin", "TestClientGoneIgnoresContextsSetByInnerMiddleware",
       swap('st := status(c, ctx)', 'st := status(c, c.Request.Context())'))
# 客户端走了不是服务端的错，只有 handler 自己中止的才标错
mutate("XGin 客户端走了的 Span 不标错", "xgin/middleware/trace.go", "./xgin", "TestClientGoneBeforeAnythingSentRecordedAs499",
       swap('\t\t\tcase aborted(c):\n', '\t\t\tcase st == statusAborted:\n'))
# 压缩中间件排在访问日志里面：截下来的是压缩过的字节，原先整段乱码进了 response_body
mutate("压缩过的响应体不记内容", "internal/web/accesslog.go", "./xgin", "TestLog_EncodedResponseBodyOmitted",
       swap('\t\t\tbody := encodedOmitted(a.RespHeader)\n', '\t\t\tbody := ""\n'))
# xgin 不给 Access.Ctx：访问日志得退回请求自己的 ctx，trace_id、xlog.AddKV 的字段在那里
mutate("访问日志的 ctx 缺省用请求的", "internal/web/accesslog.go", "./xgin", "TestLogScope",
       swap('\tctx := a.Ctx\n\tif ctx == nil {', '\tctx := a.Ctx\n\tif false {'))
mutate("链路的 method 收敛", "xgin/middleware/trace.go", "./xgin", "TestTrace",
       swap('method := web.NormalizeMethod(c.Request.Method)', 'method := c.Request.Method'),
       swap('\t\troute := routeOf(c)\n', '\t\troute := routeOf(c)\n\t\t_ = web.NormalizeMethod\n'))
# 「谁是自己人」只看 TrustedProxies。记号打错一次，要么伪造的头被带进内网，要么透传整个失效
mutate("对端在 TrustedProxies 里才算可信", "xgin/xgin.go", "./xgin", "TestBuild_PassthroughHeadersOnlyFromTrustedProxies|TestBuild_PassthroughHeadersOnlyFromPrivateByDefault",
       swap('if g.trusted.Trusts(c.RemoteIP()) {', 'if len(g.trusted) > 0 {'))
mutate("可信网段来自装配时的配置", "xgin/xgin.go", "./xgin", "TestBuild_PassthroughHeadersOnlyFromTrustedProxies",
       swap('g.trusted = web.ParseProxies(c.TrustedProxies)', 'g.trusted = web.ParseProxies(nil)'))
# 透传 Header 的可信判断也得认 private：不展开的话默认配置下谁都不信，透传整个失效
mutate("对端可信的判断也展开 private", "internal/web/proxy.go", "./xgin", "TestBuild_PassthroughHeadersOnlyFromPrivateByDefault",
       swap('expanded := ExpandProxies(list)', 'expanded := list'))
# 默认只信私有网段：K8s 里 Pod IP 随机，Ingress、负载均衡、sidecar 转发来的默认就认
mutate("TrustedProxies 默认是 private", "xgin/config.go", "./xgin", "TestBuild_TrustsOnlyPrivateProxiesByDefault|TestBuild_PassthroughHeadersOnlyFromPrivateByDefault",
       swap('\t\tTrustedProxies:     []string{web.TrustPrivate},\n', ''))
mutate("private 包含运营商级 NAT 网段", "internal/web/proxy.go", "./xgin", "TestBuild_PassthroughHeadersOnlyFromPrivateByDefault",
       swap('"192.168.0.0/16", "100.64.0.0/10",', '"192.168.0.0/16",'))
mutate("private 包含 IPv6 ULA", "internal/web/proxy.go", "./xgin", "TestBuild_PassthroughHeadersOnlyFromPrivateByDefault",
       swap('"::1/128", "fc00::/7",', '"::1/128",'))
mutate("private 和别的网段一起写时别的网段也算", "internal/web/proxy.go", "./xgin", "TestBuild_TrustedProxiesMixesPrivateWithOtherCIDRs",
       swap('\t\tout = append(out, p)\n\t}\n\treturn out', '\t}\n\treturn out'))
# 调用点：client_ip 那边（gin）也得拿展开后的网段，直接给 "private" 它解析失败、退回谁都不信
mutate("gin 拿到的是展开后的网段", "xgin/xgin.go", "./xgin", "TestBuild_TrustsOnlyPrivateProxiesByDefault",
       swap('e.SetTrustedProxies(web.ExpandProxies(c.TrustedProxies))', 'e.SetTrustedProxies(c.TrustedProxies)'))
mutate("装配时先判对端再开链路", "xgin/xgin.go", "./xgin", "TestBuild_PassthroughHeadersOnlyFromTrustedProxies",
       swap('e.Use(g.markTrustedPeer, skipMetrics(c, middleware.Trace()))', 'e.Use(skipMetrics(c, middleware.Trace()))'))
# XGin.Trace 只管 Span。原先关掉它连提取一起摘了，上游的链路标识和透传头都不收
mutate("XGin.Trace 关掉照样接上游的链路和透传", "xgin/xgin.go", "./xgin", "TestBuild_TraceDisabledOnlySkipsSpan_StillPropagates",
       swap('e.Use(g.markTrustedPeer, middleware.Propagate())', 'e.Use(g.markTrustedPeer)'))
mutate("XGin.Trace 关掉时可信规则不变", "xgin/xgin.go", "./xgin", "TestBuild_TraceDisabledOnlySkipsSpan_StillPropagates",
       swap('e.Use(g.markTrustedPeer, middleware.Propagate())', 'e.Use(middleware.Propagate())'))
mutate("Propagate 也把可信记号交给 xtrace", "xgin/middleware/trace.go", "./xgin", "TestTrace_PeerTrustFollowsXginMarker",
       swap('WithContext(otel.GetTextMapPropagator().Extract(c.Request.Context(), inbound(c)))', 'WithContext(otel.GetTextMapPropagator().Extract(c.Request.Context(), propagation.HeaderCarrier(c.Request.Header)))'))
mutate("链路中间件把可信记号交给 xtrace", "xgin/middleware/trace.go", "./xgin", "TestTrace_PeerTrustFollowsXginMarker",
       swap('\tif c.GetBool(peer.TrustedKey) {\n\t\treturn trustedCarrier{h}\n\t}\n', '\t_ = peer.TrustedKey\n'))

section("客户端")
mutate("XGin 服务端 TLS 设置交给 http.Server", "internal/web/server.go", "./xgin", "TestStart_ClientCAFile|TestStart_MinVersion",
       swap('\tsrv.TLSConfig = tlsCfg', '\t_ = tlsCfg'))
mutate("XGin ClientCAFile 开双向认证", "internal/web/config.go", "./xgin", "TestStart_ClientCAFileEnablesMutualTLS",
       swap('\t\tcfg.ClientAuth = tls.RequireAndVerifyClientCert\n', ''))
mutate("XGin MinVersion 生效", "internal/web/config.go", "./xgin", "TestStart_MinVersion",
       swap('cfg := &tls.Config{MinVersion: tlsVersions[c.MinVersion]}', 'cfg := &tls.Config{}'))
# 以为开了双向认证，实际是谁都能连的明文
mutate("XGin ClientCAFile 要和证书一起配", "internal/web/config.go", "./xgin", "TestValidate_ServerTLSNewFields|TestConfig_ServerTLSLoadedFromFile",
       swap('if c.ClientCAFile != "" && !c.tlsEnabled() {', 'if false {'))
mutate("XGin MinVersion 只收 1.2 / 1.3", "internal/web/config.go", "./xgin", "TestValidate_ServerTLSNewFields",
       swap('if _, ok := tlsVersions[c.MinVersion]; c.tlsEnabled() && !ok {', 'if false {'))
# 明文服务上 MinVersion 不起作用：原先照样校验，一个用不上的值让服务起不来
mutate("没开 TLS 不校验 MinVersion", "internal/web/config.go", "./xgin", "TestValidate_ServerTLSNewFields",
       swap('c.tlsEnabled() && !ok', '!ok'))
# 调用点：TLS 这几项从 XGin 的配置抄给 web.Server，漏抄一项就是以为开了、实际没开
mutate("ClientCAFile 交给了 web.Server", "xgin/config.go", "./xgin", "TestStart_ClientCAFileEnablesMutualTLS",
       swap('\t\tClientCAFile:      c.TLS.ClientCAFile,\n', ''))
mutate("MinVersion 交给了 web.Server", "xgin/config.go", "./xgin", "TestStart_MinVersion",
       swap('\t\tMinVersion:        c.TLS.MinVersion,\n', '\t\tMinVersion:        "1.2",\n'))
# gin 的 ResponseWriter 接口带 WriteString，handler 直接调它是常见写法。
# 包装层只包 Write 的话，这条路写出去的响应在日志里永远是空的
mutate("WriteString 写的响应也截得下来", "xgin/middleware/log.go", "./xgin", "TestLog",
       swap('''\tif w.capture && w.buf.Len() < maxResponseBody {
\t\tw.buf.WriteString(s[:min(len(s), maxResponseBody-w.buf.Len())])
\t}''', '\tif false {\n\t}'))
# gin 的默认是全都信，于是任何人发一个 X-Forwarded-For 就能决定
# 访问日志里的 client_ip。设置出错时必须退到安全的那一侧
mutate("代理网段设不上时退回谁都不信", "xgin/xgin.go", "./xgin", "TestApplyConfig|TestBuild_ForwardedHeadersOnlyWithProxiesConfigured",
       swap('\t\t_ = e.SetTrustedProxies([]string{})', ''))

section("链路")
# 用了 xgin 就有链路，使用者不用记得另外 import xtrace。摘掉这一行，
# 全局的 TracerProvider 就一直是 noop：Span 什么都不记、日志没有 trace_id，而且没有任何报错
mutate("用了 xgin 不另外 import xtrace 也有链路", "xgin/xgin.go", "./xgin", "TestTracingWorksWithoutImportingXtrace",
       swap('\t_ "github.com/jupiter-eng/xone/xtrace"\n', ''))

section("访问日志的字段")
# slog 的 JSON 把 Duration 写成纳秒整数：字段叫 elapsed_ms、值却是纳秒的话，照毫秒配的告警差出一百万倍
mutate("访问日志的耗时是毫秒", "internal/web/accesslog.go", "./xgin", "TestLog_ElapsedIsMilliseconds",
       swap('slog.Float64("elapsed_ms", millis(a.Elapsed))', 'slog.Float64("elapsed_ms", float64(a.Elapsed))'))
# 没匹配上路由时填真实路径，日志里分不出 /nope 是路由还是 404，也和指标、Span 对不上
# gin 在没写响应体时 Size() 返回 -1：原样记下来，204 看着像出了错，按 bytes_out 求和还会少算
# gin 默认的 404 / 405 正文在中间件链之后才写，不在链里写的话 bytes_out 记成 0
mutate("默认的 404 / 405 在链里写", "xgin/xgin.go", "./xgin", "TestDefault404And405",
       swap('\t\te.NoRoute(notFound)\n\t\te.NoMethod(methodNotAllowed)\n', ''))
mutate("没写响应体时 bytes_out 记 0", "xgin/middleware/log.go", "./xgin", "TestLog_BytesOut",
       swap('BytesOut:   max(c.Writer.Size(), 0),', 'BytesOut:   c.Writer.Size(),'))
# 取值写在 routeOf 里，三个中间件共用；这一条改坏它本身，下面三条各打一个调用点
mutate("没匹配上的路由记 unmatched", "xgin/middleware/middleware.go", "./xgin",
       "TestLog_UnmatchedRouteIsUnmatched|TestMetric_UnmatchedRouteUsesFixedValue|TestTrace_UnmatchedRouteUsesFixedValue",
       swap('\treturn web.RouteUnmatched\n', '\treturn c.Request.URL.Path\n'))
mutate("访问日志里没匹配上的路由记 unmatched", "xgin/middleware/log.go", "./xgin", "TestLog_UnmatchedRouteIsUnmatched",
       swap('Route:    routeOf(c),', 'Route:    c.FullPath(),'))
mutate("指标里没匹配上的路由记 unmatched", "xgin/middleware/metric.go", "./xgin", "TestMetric_UnmatchedRouteUsesFixedValue",
       swap('route, method, code := routeOf(c),', 'route, method, code := c.Request.URL.Path,'))
mutate("Span 里没匹配上的路由记 unmatched", "xgin/middleware/trace.go", "./xgin", "TestTrace_UnmatchedRouteUsesFixedValue",
       swap('\t\troute := routeOf(c)\n', '\t\troute := c.Request.URL.Path\n'))

section("错误文本脱敏")
mutate("访问日志的 errors 字段脱过敏（xgin）", "internal/web/accesslog.go", "./xgin", "TestLog_ErrorTextIsRedacted",
       swap('slog.String("errors", RedactText(a.Errors))', 'slog.String("errors", a.Errors)'))
mutate("Span 上的 gin.errors 脱过敏", "xgin/middleware/trace.go", "./xgin", "TestTrace_ErrorTextIsRedactedOnSpan",
       swap('web.RedactText(c.Errors.String())', 'c.Errors.String()'))

section("错误文本与 panic 脱敏")
# panic(err) 里的整串 DSN、"password=..." 原样进 error 字段就是密码落盘
mutate("panic 的值脱过敏", "xgin/middleware/middleware.go", "./xgin", "TestRecover_PanicValueIsRedacted",
       swap('"error", web.RedactText(fmt.Sprint(err)),', '"error", fmt.Sprint(err),'))
# 规则在 internal/web（core.py 有它自己的变异），这里验 xgin 的访问日志和 Span 真的是那个样子
mutate("多条错误用分号隔开（xgin）", "internal/web/redact.go", "./xgin", "TestLog_MultipleErrorsStaySeparated|TestTrace_ErrorTextKeepsSeparationAndMasksDSNOnSpan",
       swap('return textNewlines.Replace(strings.TrimRight(redactCredentials(s), "\\r\\n"))', 'return newlines.Replace(redactCredentials(s))'))
mutate("errors 里 DSN 的密码被遮掉（xgin）", "internal/web/redact.go", "./xgin", "TestLog_DSNPasswordInErrorIsMasked|TestTrace_ErrorTextKeepsSeparationAndMasksDSNOnSpan",
       swap('\ts = urlUserinfo.ReplaceAllString(s, "$1:"+Redacted+"@")\n', ''))

section("零值与启动日志")
# 模块名原先只在 New 里填：&xgin.XGin{} 的错误记在空模块名下，xerror.Is(err, "xgin") 不成立
mutate("零值 XGin 的错误和日志记在 xgin 名下", "xgin/xgin.go", "./xgin", "TestZeroValue_ReportsUnderModuleName",
       swap('g.server.Start("xgin", ', 'g.server.Start("", '))
# listening 原先打在监听之前：证书读不出来、端口被占时先说 listening 再报 listen failed
mutate("listening 在端口绑上之后才打", "internal/web/server.go", "./xgin", "TestStart_NoListeningLogWhenListenFails",
       swap('\tln, err := listen(c, srv)\n', '\tslog.Info(module + " listening")\n\tln, err := listen(c, srv)\n'))
mutate("listening 在证书读好之后才打", "internal/web/server.go", "./xgin", "TestStart_NoListeningLogWhenListenFails",
       swap('\t\tcert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)\n\t\tif err != nil {\n\t\t\treturn nil, err\n\t\t}\n\t\tsrv.TLSConfig.Certificates = append(srv.TLSConfig.Certificates, cert)\n',
            '\t\t_ = tls.Certificate{}\n'),
       swap('srv.ServeTLS(ln, "", "")', 'srv.ServeTLS(ln, c.CertFile, c.KeyFile)'))
# 调用点：规矩在 internal/web，XGin 块读配置时真的拦住了映射写法
mutate("XGin 拦住 IPv4 映射写法的代理", "internal/web/proxy.go", "./xgin", "TestLoadConfig_IPv4MappedProxyFailsStartup",
       swap('\t\tif plain, ok := unmapped(p); ok {', '\t\tif plain, ok := unmapped(p); ok && false {'))

section("停止、路由与请求上下文")
# 被劫持走的连接 Shutdown 不等、Close 断不掉：不取消根 ctx 的话，等着请求 ctx 的 WebSocket handler
# 让 Stop 等满整份预算再报 still running
mutate("Stop 之后取消劫持连接的 ctx", "internal/web/server.go", "./xgin", "TestStop_HijackedConnGetsCancelled",
       swap('\tcancelBase()\n', '\t_ = cancelBase\n'))
mutate("请求 ctx 挂在可取消的根上", "internal/web/server.go", "./xgin", "TestStop_HijackedConnGetsCancelled",
       swap('{ return base }', '{ return context.WithoutCancel(base) }'))
# 监听失败还记着「在跑」的话，端口空出来之后每次 Start 都说 already running
mutate("监听失败之后可以再 Start", "internal/web/server.go", "./xgin", "TestStart_RetryAfterListenFailure",
       swap('\t\t\ts.srv, s.cancel = nil, nil\n', ''))
# gin 默认的 301 / 307 不跑中间件：/users/ 不进访问日志、指标和链路
mutate("末尾斜杠不重定向", "xgin/xgin.go", "./xgin", "TestBuild_TrailingSlashGoesThroughMiddlewareAs404",
       swap('\te.RedirectTrailingSlash = false\n', ''))
# 不开的话 xlog.AddKV(c, ...)、Start(c, ...) 丢掉日志作用域和父 Span
mutate("*gin.Context 当 ctx 传转到请求的 ctx", "xgin/xgin.go", "./xgin", "TestBuild_GinContextAsContext",
       swap('\t\te.ContextWithFallback = true\n', ''))
# 调用点：抓 /metrics 每次开一个 Span
mutate("抓指标不开 Span", "xgin/xgin.go", "./xgin", "TestTrace_MetricsScrapeNotTraced",
       swap('skipMetrics(c, middleware.Trace())', 'middleware.Trace()'))
# engine.Handler() 在 gin 的 UseH2C 开着时包一层 x/net 的 h2c.NewHandler，连接被劫持走
mutate("交给 net/http 的是 engine 本身", "xgin/xgin.go", "./xgin", "TestStart_GinUseH2CDoesNotBypassShutdown",
       swap('c.server(), g.engine)', 'c.server(), g.engine.Handler())'))
# 客户端走了不是服务的故障：ERROR 会让告警跟着客户端的网络抖动响
mutate("断连记 WARN", "xgin/middleware/middleware.go", "./xgin", "TestRecover_BrokenPipe",
       swap('slog.WarnContext(ctx, "connection broken"', 'slog.ErrorContext(ctx, "connection broken"'))
