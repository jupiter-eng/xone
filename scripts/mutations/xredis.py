# xredis 的变异：module xredis 里的承诺。写法见 scripts/mutations/__init__.py
from . import cut, mutate, section, swap

section("配置")
mutate("xredis 多实例铺的是自己的默认值", "xredis/config.go", "./xredis", "TestConfig_SingleInstanceForm",
       swap('xconfig.UnmarshalClients(ConfigKey, DefaultClientConfig)', 'xconfig.UnmarshalClients(ConfigKey, func() ClientConfig { return ClientConfig{} })'))

section("启动与退出")
mutate("建实例 panic 不漏掉已建好的", "internal/xclient/xclient.go", "./xredis", "TestInitAll",
       swap('safeNew(ctx, r.module, name, cfgs[name], new)', 'new(ctx, name, cfgs[name])'))
mutate("一个实例建不起来就把已建好的全关掉", "internal/xclient/xclient.go", "./xredis", "TestInitAll",
       swap('\t\t\treturn abort(err)\n', '\t\t\treturn err\n'))

section("客户端")
mutate("xredis 没配也让注册表知道启动过了", "xredis/xredis.go", "./xredis", "TestInitXRedis_CSaysUnconfiguredNotTooEarly",
       swap('\t\treturn xclient.Build(ctx, reg, nil, build)\n', '\t\treturn nil\n'))
mutate("Redis 命令遵守请求 deadline", "xredis/xredis.go", "./xredis", "TestNew",
       swap('\t\tContextTimeoutEnabled: true,\n',''))
# go-redis 默认每次建连内部重拨 5 次、间隔 100ms：主机宕机时一条命令实测 11.7s，
# 远超文档那条式子推出来的 7s
mutate("Redis 一次建连只拨一次号", "xredis/xredis.go", "./xredis", "TestNew_RedisDownDialsOncePerConnect",
       swap('\t\tDialerRetries: 1,\n', ''))
# 打在调用点上：绕开 probe 直接 Ping，启动期间的退出信号要等到 ReadTimeout 才生效
mutate("Redis 建连探测收到退出信号当场放弃", "xredis/xredis.go", "./xredis", "TestNew_ExitSignalAtStartupSkipsReadTimeout",
       swap('xclient.Probe(ctx, probePolicy(client.Options()), probe(client))',
            'xclient.Probe(ctx, probePolicy(client.Options()), func(ctx context.Context) error { return client.Ping(ctx).Err() })'))
mutate("Redis 建连探测取消时不等这次读", "xredis/xredis.go", "./xredis", "TestNew_ExitSignalAtStartupSkipsReadTimeout",
       swap('\t\t\tif errors.Is(ctx.Err(), context.Canceled) {\n', '\t\t\tif false && errors.Is(ctx.Err(), context.Canceled) {\n'))
mutate("Redis 认证失败不重试", "xredis/xredis.go", "./xredis", "TestNew_AuthFailureReportedClearlyWithoutRetry",
       swap('\t\tAuthFailed: redis.IsAuthError,\n', ''))
mutate("Redis 认证失败不说成连不上", "xredis/xredis.go", "./xredis", "TestNew_AuthFailureReportedClearlyWithoutRetry",
       swap('\t\tif redis.IsAuthError(err) {\n\t\t\treturn nil, nil, xerror.Newf', '\t\tif false {\n\t\t\treturn nil, nil, xerror.Newf'))
# 不接的话 go-redis 往 stderr 写纯文本，不是 JSON、不带 trace_id
mutate("go-redis 自己的日志进 slog", "xredis/xredis.go", "./xredis", "TestGoRedisOwnLogsGoToSlogWithCallerCtx",
       swap('\tredis.SetLogger(slogLogger{})\n', ''))
mutate("go-redis 的日志带着调用方的 ctx", "xredis/xredis.go", "./xredis", "TestGoRedisOwnLogsGoToSlogWithCallerCtx",
       swap('slog.WarnContext(ctx, "xredis go-redis log"', 'slog.WarnContext(context.Background(), "xredis go-redis log"'))
# 配置里的 0 在 go-redis 里是它的默认值（DialTimeout / ReadTimeout 各 5s）：预算按配置里的 0 算，只剩 1s 兜底，
# go-redis 还愿意等的慢建连被建连验证先判了超时。打在交出 Options 的调用点上
mutate("Redis 建连预算按 go-redis 补完默认值之后的超时算", "xredis/xredis.go", "./xredis", "TestNew_ZeroTimeoutsProbeWithGoRedisDefaults",
       swap('xclient.Probe(ctx, probePolicy(client.Options()), probe(client))',
            'xclient.Probe(ctx, probePolicy(&redis.Options{DialTimeout: cfg.DialTimeout, ReadTimeout: cfg.ReadTimeout}), probe(client))'))
mutate("Redis 命令参数不进 Span", "xredis/xredis.go", "./xredis", "TestTrace",
       swap('redisotel.InstrumentTracing(client, redisotel.WithDBStatement(false), ', 'redisotel.InstrumentTracing(client, '))
# redisotel v9.22.0 把服务端的错误原文写进 exception.message 和状态描述（实测真 Redis 上 ERR unknown command … 'secretarg1'）。
# 打在交给 redisotel 的那个 provider 上，和包装里改写的两处
mutate("Redis Span 上的错误用收过的文本", "xredis/xredis.go", "./xredis", "TestTrace_ServerErrorTextNotInSpan",
       swap('redisotel.WithTracerProvider(tp))', 'redisotel.WithTracerProvider(tp.TracerProvider))'))
mutate("Redis Span 不把服务端原文记成事件", "xredis/trace.go", "./xredis", "TestTrace_ServerErrorTextNotInSpan",
       swap('\tif clientSafe(err) {\n\t\ts.Span.RecordError(err, opts...)', '\tif true {\n\t\ts.Span.RecordError(err, opts...)'))
mutate("Redis Span 的状态描述用收过的文本", "xredis/trace.go", "./xredis", "TestTrace_ServerErrorTextNotInSpan",
       swap('\t\tdesc = s.text\n', '\t\t_ = s.text\n'))
# redisotel v9.22.0 拿 cmd.FullName() 当 Span 名，实测 Do(ctx, "SET k1 <值>") 的 Span 名是 "set k1 <值>"。
# 打在两个调用点上：交出命令的钩子、包装里起名的那一处
mutate("Redis Span 名不带命令参数：命令交给包装", "xredis/xredis.go", "./xredis", "TestTrace_SpanNameCarriesNoArgs",
       swap('\t\tclient.AddHook(spanCmdsHook{})\n', ''))
mutate("Redis Span 名不带命令参数：包装按命令起名", "xredis/trace.go", "./xredis", "TestTrace_SpanNameCarriesNoArgs",
       swap('t.Tracer.Start(ctx, spanName(ctx, name), opts...)', 't.Tracer.Start(ctx, name, opts...)'))
mutate("Redis pipeline 的 Span 名逐条收", "xredis/trace.go", "./xredis", "TestTrace_SpanNameCarriesNoArgs",
       swap('if n := spanCmdName(c); ', 'if n := c.FullName(); '))
mutate("Redis Span 上网络错误照原文记", "xredis/trace.go", "./xredis", "TestTrace_ClientSafeErrorKeptInSpan",
       swap('\tif clientSafe(err) {\n\t\ts.Span.RecordError(err, opts...)', '\tif false {\n\t\ts.Span.RecordError(err, opts...)'))
mutate("Metric 关掉的 Redis 实例不导出", "xredis/xredis.go", "./xredis", "TestInstall", swap('\t\tif inst.metric {\n', '\t\tif true {\n'))
mutate("xmetric 重装后 Redis 连接池指标照样导出", "xredis/xredis.go", "./xredis", "TestInstall",
       swap('func installPoolMetrics() {\n\tif _, err := xmetric.RegisterAs(',
     'var poolInstalled bool\n\nfunc installPoolMetrics() {\n\tif poolInstalled {\n\t\treturn\n\t}\n\tpoolInstalled = true\n\tif _, err := xmetric.RegisterAs('))
mutate("XRedis 块在读配置时就校验", "xredis/config.go", "./xredis", "TestConfig_InvalidValuesFailAtConfigRead",
       swap('clients, err := xconfig.UnmarshalClients(ConfigKey, DefaultClientConfig)\n\treturn Config{Clients: clients}, err',
            'type raw ClientConfig\n\tm, err := xconfig.UnmarshalClients(ConfigKey, func() raw { return raw(DefaultClientConfig()) })\n\tclients := map[string]ClientConfig{}\n\tfor k, v := range m {\n\t\tclients[k] = ClientConfig(v)\n\t}\n\treturn Config{Clients: clients}, err'))
mutate("直接调 xredis.New 也校验", "xredis/xredis.go", "./xredis", "TestNew_NoConnectOnInvalidConfig|TestNew_ValidatesBeforeConnecting|TestNew_TLSUnreadableFileIsConfigError",
       swap('\tif err := cfg.Validate(); err != nil {', '\tif err := cfg.Validate(); false && err != nil {'))
# go-redis 把 ReadTimeout -1 当成「不限时」：一个减号就静默关掉超时保护
mutate("Redis 负的时长要被拦住", "xredis/config.go", "./xredis", "TestValidate|TestConfig_InvalidValuesFailAtConfigRead",
       swap('\t\tif d.val < 0 {\n\t\t\treturn fmt.Errorf("%s must not be negative', '\t\tif false {\n\t\t\treturn fmt.Errorf("%s must not be negative'))
mutate("Redis 负的连接数要被拦住", "xredis/config.go", "./xredis", "TestValidate",
       swap('\t\tif n.val < 0 {', '\t\tif false {'))
mutate("Redis MaxRetries 只收 -1 这一个负数", "xredis/config.go", "./xredis", "TestValidate",
       swap('if c.MaxRetries < -1 {', 'if false {'))
mutate("Redis 退避只收 -1ns 这一个负数", "xredis/config.go", "./xredis", "TestValidate",
       swap('if d.val < 0 && d.val != -1 {', 'if false {'))
mutate("Redis 校验 TLS 块", "xredis/config.go", "./xredis", "TestValidate",
       swap('\treturn c.TLS.Validate()\n}', '\treturn nil\n}'))
mutate("Redis 的 TLS 配置传给 go-redis", "xredis/xredis.go", "./xredis", "TestNew_TLS",
       swap('tlsCfg, err := cfg.TLS.Build()', '_, err := cfg.TLS.Build()'), swap('TLSConfig: tlsCfg,', 'TLSConfig: nil,'))
# Redis 7.2 之前每条新连接一个报错的 CLIENT SETINFO Span
mutate("Redis 建连不发 CLIENT SETINFO", "xredis/xredis.go", "./xredis", "TestNew_NoClientSetinfoNorMaintNotifications",
       swap('DisableIdentity: true,', 'DisableIdentity: false,'))
mutate("Redis 不开维护通知", "xredis/xredis.go", "./xredis", "TestNew_NoClientSetinfoNorMaintNotifications",
       swap('Mode: maintnotifications.ModeDisabled}', 'Mode: maintnotifications.ModeAuto}'))
# 钩子挂在建连验证之前：启动时每次 Ping 尝试都是一个没有父 Span 的 ping
mutate("Redis 链路钩子在建连验证成功之后才挂", "xredis/xredis.go", "./xredis", "TestTrace_StartupConnectCheckOpensNoSpan",
       swap('\tif err := xclient.Probe(ctx, probePolicy(client.Options()), probe(client)); err != nil {',
            '\tif cfg.Trace {\n\t\t_ = redisotel.InstrumentTracing(client, redisotel.WithDBStatement(false))\n\t}\n\tif err := xclient.Probe(ctx, probePolicy(client.Options()), probe(client)); err != nil {'))
mutate("xredis connected 日志带着实例名", "xredis/xredis.go", "./xredis", "TestInstall_LogsNameAndAddrButNotPassword",
       swap('"xredis connected", "name", name,', '"xredis connected", "name", "",'))

section("链路")
# 用了 xredis 就有链路，使用者不用记得另外 import xtrace。摘掉这一行，
# 全局的 TracerProvider 就一直是 noop：Span 什么都不记、日志没有 trace_id，而且没有任何报错
mutate("用了 xredis 不另外 import xtrace 也有链路", "xredis/xredis.go", "./xredis", "TestTracingWorksWithoutImportingXtrace",
       swap('\t_ "github.com/jupiter-eng/xone/xtrace"\n', ''))

section("命令日志")
# Log 默认关着：挂上钩子就是每条命令一行，量大的服务日志平台先被打满
mutate("Redis 命令日志只在 Log 开着时挂", "xredis/xredis.go", "./xredis", "TestLog_OffMeansNoCommandLines",
       swap('\tif cfg.Log {\n\t\tclient.AddHook(logHook{', '\tif true {\n\t\tclient.AddHook(logHook{'))
mutate("Redis Log 开着就挂命令日志", "xredis/xredis.go", "./xredis", "TestLog_CommandLineHasNameCmdFirstKeyElapsed",
       swap('\t\tclient.AddHook(logHook{name: name, slow: cfg.SlowThreshold})\n', ''))
mutate("Redis 慢命令阈值交给钩子", "xredis/xredis.go", "./xredis", "TestLog_SlowThreshold",
       swap('slow: cfg.SlowThreshold}', 'slow: 0}'))
# 打在调用点上：build 不把名字交下去，多实例时分不出日志是哪个实例的
mutate("Redis 命令日志带着配置里的实例名", "xredis/xredis.go", "./xredis", "TestLog_InstanceNameFromConfigReachesLog",
       swap('newClient(ctx, name, c)', 'newClient(ctx, "", c)'))
# 日志钩子挂到链路钩子外面，日志的 span_id 就成了调用方的，对不上这条命令的 Span
mutate("Redis 命令日志在 redis Span 里面", "xredis/xredis.go", "./xredis", "TestLog_CarriesRedisSpanOfCallerTrace",
       swap('\tif cfg.Log {\n\t\tclient.AddHook(logHook{name: name, slow: cfg.SlowThreshold})\n\t}\n', ''),
       swap('\tif cfg.Trace {\n\t\t// Turn db.statement off', '\tif cfg.Log {\n\t\tclient.AddHook(logHook{name: name, slow: cfg.SlowThreshold})\n\t}\n\tif cfg.Trace {\n\t\t// Turn db.statement off'))
mutate("Redis 命令日志用命令的 ctx", "xredis/log.go", "./xredis", "TestLog_CarriesRedisSpanOfCallerTrace",
       swap('slog.InfoContext(ctx, "redis command"', 'slog.InfoContext(context.Background(), "redis command"'))
# 读穿缓存天天走 key 不存在：当成失败的话 WARN 刷屏，真故障淹在里面
mutate("redis.Nil 不算失败", "xredis/log.go", "./xredis", "TestLog_NilIsNotAFailure",
       swap('failed := err != nil && !errors.Is(err, redis.Nil)', 'failed := err != nil'))
mutate("pipeline 里的 redis.Nil 不算失败", "xredis/log.go", "./xredis", "TestLog_PipelineNilIsNotFailureButLaterErrorIs",
       swap('\tif err == nil || errors.Is(err, redis.Nil) || txFailed {\n', '\tif err == nil || txFailed {\n'))
mutate("pipeline 认出 nil 之后的真错误", "xredis/log.go", "./xredis", "TestLog_PipelineNilIsNotFailureButLaterErrorIs",
       swap('if e := c.Err(); e != nil && !errors.Is(e, redis.Nil) && !errors.Is(e, redis.TxFailedErr) {', 'if e := c.Err(); false && e != nil {'))
mutate("pipeline 记第一个真错误，不是最后一个", "xredis/log.go", "./xredis", "TestLog_PipelineReportsFirstError",
       swap('\t\t\t\terr = e\n\t\t\t\tbreak\n', '\t\t\t\terr = e\n'))
# WATCH 冲突是 proto.RedisError（和服务端的错误同一个类型），原先记成 redis pipeline failed 的 WARN
mutate("WATCH 冲突不算失败", "xredis/log.go", "./xredis", "TestLog_WatchConflict",
       swap('\ttxFailed := errors.Is(err, redis.TxFailedErr)\n', '\ttxFailed := false\n'))
mutate("WATCH 冲突时每条命令上的 TxFailedErr 也不算失败", "xredis/log.go", "./xredis", "TestLog_WatchConflict",
       swap(' && !errors.Is(e, redis.TxFailedErr) {', ' {'))
mutate("WATCH 冲突带 tx_failed", "xredis/log.go", "./xredis", "TestLog_WatchConflict",
       swap('\t\tattrs = append(attrs, "tx_failed", true)\n', ''))
# cmd.Name() 是第 1 个参数原样小写：Do(ctx, "SET k1 <值>") 把值带进了 cmd。打在两个调用点上
mutate("命令名不像命令名就不记", "xredis/log.go", "./xredis", "TestLog_InvalidCmdNameReplaced",
       swap('attrs = append(attrs, "cmd", cmdName(cmd))', 'attrs = append(attrs, "cmd", cmd.Name())'))
mutate("pipeline 的命令名不像命令名就不记", "xredis/log.go", "./xredis", "TestLog_InvalidCmdNameReplaced",
       swap('names = append(names, cmdName(c))', 'names = append(names, c.Name())'))
mutate("命令名最长 64 字节", "xredis/log.go", "./xredis", "TestLog_InvalidCmdNameReplaced",
       swap('[a-z0-9._|-]{0,63}$', '[a-z0-9._|-]*$'))
mutate("又慢又失败的命令记失败", "xredis/log.go", "./xredis", "TestLog_SlowAndFailedLogsFailed",
       swap('\tcase failed:\n', '\tcase failed && !h.isSlow(elapsed):\n'))
mutate("又慢又失败的 pipeline 记失败", "xredis/log.go", "./xredis", "TestLog_SlowAndFailedLogsFailed",
       swap('\tcase err != nil:\n', '\tcase err != nil && !h.isSlow(elapsed):\n'))
mutate("slog 只收 WARN 时照样记失败和慢命令", "xredis/log.go", "./xredis", "TestLog_WarnLevelHandlerStillGetsProblems",
       swap('if !failed && !h.isSlow(elapsed) && !slog.Default().Enabled(ctx, slog.LevelInfo) {', 'if !slog.Default().Enabled(ctx, slog.LevelInfo) {'))
mutate("slog 只收 WARN 时照样记失败和慢 pipeline", "xredis/log.go", "./xredis", "TestLog_WarnLevelHandlerStillGetsProblems",
       swap('if err == nil && !h.isSlow(elapsed) && !slog.Default().Enabled(ctx, slog.LevelInfo) {', 'if !slog.Default().Enabled(ctx, slog.LevelInfo) {'))
mutate("ctx 取消照原文记", "xredis/log.go", "./xredis", "TestLog_ContextCanceledKeepsText",
       swap('errors.Is(err, context.Canceled) || ', ''))
mutate("Redis 命令日志的耗时保留到微秒", "xredis/log.go", "./xredis", "TestMs_KeepsSubMillisecond",
       swap('func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }', 'func ms(d time.Duration) float64 { return float64(d.Milliseconds()) }'))
# 只记第一个 key：往后错一位，SET 的值就成了 key 进了日志
mutate("Redis 命令日志不记值", "xredis/log.go", "./xredis", "TestLog_ValuesAndOtherArgsNeverLogged|TestFirstKey",
       swap('\treturn argString(args, pos)\n}', '\treturn argString(args, pos+1)\n}'))
# go-redis 自己的兜底就是「不在免 key 名单里就当第 1 个参数」：AUTH 的用户名、MIGRATE 的 host 都会被当成 key
mutate("名单外的命令不记 key", "xredis/log.go", "./xredis", "TestLog_ValuesAndOtherArgsNeverLogged|TestFirstKey",
       swap('\t\tif _, ok := keyAtFirstArg[name]; ok {\n', '\t\tif true {\n'))
mutate("EVAL 的 numkeys 为 0 时第 3 个参数是 ARGV", "xredis/log.go", "./xredis", "TestFirstKey",
       swap('\t\tif numKeys(args) > 0 {\n', '\t\tif len(args) > 3 {\n'))
mutate("XREADGROUP 跳过组名和消费者名", "xredis/log.go", "./xredis", "TestFirstKey",
       swap('pos = afterStreams(args, 4)', 'pos = afterStreams(args, 1)'))
mutate("超长的 key 截断", "xredis/log.go", "./xredis", "TestLog_LongKeyTruncated",
       swap('\tif len(key) <= maxLoggedKey {\n', '\tif true {\n'))
# 一串 0x80 没有字符起点：一路往回找会截成空串
mutate("不是 UTF-8 的长 key 不截成空串", "xredis/log.go", "./xredis", "TestKeyAttrs_InvalidUTF8NotTruncatedToEmpty",
       swap('\tif !utf8.RuneStart(key[cut]) {\n\t\tcut = maxLoggedKey\n\t}\n', ''))
# 实测 Redis 7.0.15：ERR unknown command 的原文把参数带出来
mutate("Redis 服务端错误只记错误码", "xredis/log.go", "./xredis", "TestLog_FailedCommandLogsOnlyErrorCode",
       swap('return "redis server error " + code + " (message omitted, it may contain argument values)", code',
            'return err.Error(), code'))
# EVAL 里 return {err=ARGV[1]}：错误原文整个就是值，取第一个词当错误码就把值记下来了
mutate("Redis 错误码只认已知的", "xredis/log.go", "./xredis", "TestLog_FailedCommandLogsOnlyErrorCode",
       swap('if _, ok := knownErrorCodes[code]; ok {', 'if code != "" {'))
# go-redis 解析回复失败时把回复内容写进错误
mutate("不认得的客户端错误不记原文", "xredis/log.go", "./xredis", "TestErrorAttrs",
       swap('return "redis client error (message omitted, it may contain reply data)", ""', 'return err.Error(), ""'))
mutate("Redis SlowThreshold 为负要被拦住", "xredis/config.go", "./xredis", "TestValidate",
       swap('\t\t{"SlowThreshold", c.SlowThreshold},\n', ''))
