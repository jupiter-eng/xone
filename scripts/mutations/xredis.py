# The xredis mutations: the promises of the xredis module. The format is in
# scripts/mutations/__init__.py
from . import cut, mutate, section, swap

section("Configuration")
mutate("xredis spreads its own defaults over several instances", "xredis/config.go", "./xredis", "TestConfig_SingleInstanceForm",
       swap('xconfig.UnmarshalClients(ConfigKey, DefaultClientConfig)', 'xconfig.UnmarshalClients(ConfigKey, func() ClientConfig { return ClientConfig{} })'))

section("Startup and shutdown")
mutate("a panic while building an instance does not leak what was built", "internal/xclient/xclient.go", "./xredis", "TestInitAll",
       swap('safeNew(ctx, r.module, name, cfgs[name], new)', 'new(ctx, name, cfgs[name])'))
mutate("one instance failing to build closes everything already built", "internal/xclient/xclient.go", "./xredis", "TestInitAll",
       swap('\t\t\treturn abort(err)\n', '\t\t\treturn err\n'))

section("Clients")
mutate("an unconfigured xredis still tells the registry it started", "xredis/xredis.go", "./xredis", "TestInitXRedis_CSaysUnconfiguredNotTooEarly",
       swap('\t\treturn xclient.Build(ctx, reg, nil, build)\n', '\t\treturn nil\n'))
mutate("Redis commands honour the request deadline", "xredis/xredis.go", "./xredis", "TestNew",
       swap('\t\tContextTimeoutEnabled: true,\n',''))
# go-redis redials 5 times internally per connect by default, 100ms apart: with the host down one
# command was measured at 11.7s, far beyond the 7s the formula in the documentation gives
mutate("one Redis connect dials exactly once", "xredis/xredis.go", "./xredis", "TestNew_RedisDownDialsOncePerConnect",
       swap('\t\tDialerRetries: 1,\n', ''))
# Aimed at the call site: Pinging directly, around probe, makes a shutdown signal during startup
# wait for ReadTimeout before it takes effect
mutate("the Redis connection probe gives up on the spot on a shutdown signal", "xredis/xredis.go", "./xredis", "TestNew_ExitSignalAtStartupSkipsReadTimeout",
       swap('xclient.Probe(ctx, probePolicy(client.Options()), probe(client))',
            'xclient.Probe(ctx, probePolicy(client.Options()), func(ctx context.Context) error { return client.Ping(ctx).Err() })'))
mutate("a cancelled Redis connection probe does not wait out the read", "xredis/xredis.go", "./xredis", "TestNew_ExitSignalAtStartupSkipsReadTimeout",
       swap('\t\t\tif errors.Is(ctx.Err(), context.Canceled) {\n', '\t\t\tif false && errors.Is(ctx.Err(), context.Canceled) {\n'))
mutate("a Redis authentication failure is not retried", "xredis/xredis.go", "./xredis", "TestNew_AuthFailureReportedClearlyWithoutRetry",
       swap('\t\tAuthFailed: redis.IsAuthError,\n', ''))
mutate("a Redis authentication failure is not reported as unreachable", "xredis/xredis.go", "./xredis", "TestNew_AuthFailureReportedClearlyWithoutRetry",
       swap('\t\tif redis.IsAuthError(err) {\n\t\t\treturn nil, nil, xerror.Newf', '\t\tif false {\n\t\t\treturn nil, nil, xerror.Newf'))
# Unconnected, go-redis writes plain text to stderr: not JSON and with no trace_id
mutate("go-redis's own logs go into slog", "xredis/xredis.go", "./xredis", "TestGoRedisOwnLogsGoToSlogWithCallerCtx",
       swap('\tredis.SetLogger(slogLogger{})\n', ''))
mutate("go-redis's logs carry the caller's ctx", "xredis/xredis.go", "./xredis", "TestGoRedisOwnLogsGoToSlogWithCallerCtx",
       swap('slog.WarnContext(ctx, "xredis go-redis log"', 'slog.WarnContext(context.Background(), "xredis go-redis log"'))
# A 0 in the configuration is go-redis's default (DialTimeout / ReadTimeout 5s each): a budget
# computed from that 0 leaves a 1s fallback, and a slow connect go-redis is still willing to wait
# for is called a timeout by the connection check first. Aimed at the call site handing Options over
mutate("the Redis connect budget goes by the timeouts after go-redis filled its defaults in", "xredis/xredis.go", "./xredis", "TestNew_ZeroTimeoutsProbeWithGoRedisDefaults",
       swap('xclient.Probe(ctx, probePolicy(client.Options()), probe(client))',
            'xclient.Probe(ctx, probePolicy(&redis.Options{DialTimeout: cfg.DialTimeout, ReadTimeout: cfg.ReadTimeout}), probe(client))'))
mutate("Redis command arguments do not reach the Span", "xredis/xredis.go", "./xredis", "TestTrace",
       swap('redisotel.InstrumentTracing(client, redisotel.WithDBStatement(false), ', 'redisotel.InstrumentTracing(client, '))
# redisotel v9.22.0 writes the server's error text into exception.message and the status
# description (measured against a real Redis: ERR unknown command … 'secretarg1').
# Aimed at the provider handed to redisotel, and at the two places the wrapper rewrites
mutate("an error on a Redis Span uses the text that was taken in", "xredis/xredis.go", "./xredis", "TestTrace_ServerErrorTextNotInSpan",
       swap('redisotel.WithTracerProvider(tp))', 'redisotel.WithTracerProvider(tp.TracerProvider))'))
mutate("a Redis Span does not record the server's text as an event", "xredis/trace.go", "./xredis", "TestTrace_ServerErrorTextNotInSpan",
       swap('\tif clientSafe(err) {\n\t\ts.Span.RecordError(err, opts...)', '\tif true {\n\t\ts.Span.RecordError(err, opts...)'))
mutate("a Redis Span's status description uses the text that was taken in", "xredis/trace.go", "./xredis", "TestTrace_ServerErrorTextNotInSpan",
       swap('\t\tdesc = s.text\n', '\t\t_ = s.text\n'))
# redisotel v9.22.0 uses cmd.FullName() as the Span name: Do(ctx, "SET k1 <value>") was measured
# producing the Span name "set k1 <value>".
# Aimed at both call sites: the hook handing the command over, and the naming inside the wrapper
mutate("a Redis Span name carries no arguments: the command is handed to the wrapper", "xredis/xredis.go", "./xredis", "TestTrace_SpanNameCarriesNoArgs",
       swap('\t\tclient.AddHook(spanCmdsHook{})\n', ''))
mutate("a Redis Span name carries no arguments: the wrapper names it after the command", "xredis/trace.go", "./xredis", "TestTrace_SpanNameCarriesNoArgs",
       swap('t.Tracer.Start(ctx, spanName(ctx, name), opts...)', 't.Tracer.Start(ctx, name, opts...)'))
mutate("a Redis pipeline's Span name takes the commands one by one", "xredis/trace.go", "./xredis", "TestTrace_SpanNameCarriesNoArgs",
       swap('if n := spanCmdName(c); ', 'if n := c.FullName(); '))
mutate("a network error on a Redis Span is recorded verbatim", "xredis/trace.go", "./xredis", "TestTrace_ClientSafeErrorKeptInSpan",
       swap('\tif clientSafe(err) {\n\t\ts.Span.RecordError(err, opts...)', '\tif false {\n\t\ts.Span.RecordError(err, opts...)'))
mutate("a Redis instance with Metric off is not exported", "xredis/xredis.go", "./xredis", "TestInstall", swap('\t\tif inst.metric {\n', '\t\tif true {\n'))
mutate("the Redis pool metrics are still exported after xmetric is reinstalled", "xredis/xredis.go", "./xredis", "TestInstall",
       swap('func installPoolMetrics() {\n\tif _, err := xmetric.RegisterAs(',
     'var poolInstalled bool\n\nfunc installPoolMetrics() {\n\tif poolInstalled {\n\t\treturn\n\t}\n\tpoolInstalled = true\n\tif _, err := xmetric.RegisterAs('))
mutate("the XRedis block is validated while the configuration is read", "xredis/config.go", "./xredis", "TestConfig_InvalidValuesFailAtConfigRead",
       swap('clients, err := xconfig.UnmarshalClients(ConfigKey, DefaultClientConfig)\n\treturn Config{Clients: clients}, err',
            'type raw ClientConfig\n\tm, err := xconfig.UnmarshalClients(ConfigKey, func() raw { return raw(DefaultClientConfig()) })\n\tclients := map[string]ClientConfig{}\n\tfor k, v := range m {\n\t\tclients[k] = ClientConfig(v)\n\t}\n\treturn Config{Clients: clients}, err'))
mutate("an xredis.New called directly validates too", "xredis/xredis.go", "./xredis", "TestNew_NoConnectOnInvalidConfig|TestNew_ValidatesBeforeConnecting|TestNew_TLSUnreadableFileIsConfigError",
       swap('\tif err := cfg.Validate(); err != nil {', '\tif err := cfg.Validate(); false && err != nil {'))
# go-redis takes a ReadTimeout of -1 as "unlimited": one minus sign quietly turns the timeout
# protection off
mutate("a negative Redis duration is caught", "xredis/config.go", "./xredis", "TestValidate|TestConfig_InvalidValuesFailAtConfigRead",
       swap('\t\tif d.val < 0 {\n\t\t\treturn fmt.Errorf("%s must not be negative', '\t\tif false {\n\t\t\treturn fmt.Errorf("%s must not be negative'))
mutate("a negative Redis connection count is caught", "xredis/config.go", "./xredis", "TestValidate",
       swap('\t\tif n.val < 0 {', '\t\tif false {'))
mutate("Redis MaxRetries takes -1 as its only negative value", "xredis/config.go", "./xredis", "TestValidate",
       swap('if c.MaxRetries < -1 {', 'if false {'))
mutate("a Redis backoff takes -1ns as its only negative value", "xredis/config.go", "./xredis", "TestValidate",
       swap('if d.val < 0 && d.val != -1 {', 'if false {'))
mutate("Redis validates the TLS block", "xredis/config.go", "./xredis", "TestValidate",
       swap('\treturn c.TLS.Validate()\n}', '\treturn nil\n}'))
mutate("the Redis TLS configuration is passed to go-redis", "xredis/xredis.go", "./xredis", "TestNew_TLS",
       swap('tlsCfg, err := cfg.TLS.Build()', '_, err := cfg.TLS.Build()'), swap('TLSConfig: tlsCfg,', 'TLSConfig: nil,'))
# Before Redis 7.2, every new connection leaves a failed CLIENT SETINFO Span
mutate("a Redis connect does not send CLIENT SETINFO", "xredis/xredis.go", "./xredis", "TestNew_NoClientSetinfoNorMaintNotifications",
       swap('DisableIdentity: true,', 'DisableIdentity: false,'))
mutate("Redis does not turn maintenance notifications on", "xredis/xredis.go", "./xredis", "TestNew_NoClientSetinfoNorMaintNotifications",
       swap('Mode: maintnotifications.ModeDisabled}', 'Mode: maintnotifications.ModeAuto}'))
# With the hook attached before the connection check, every Ping attempt at startup becomes a ping
# Span with no parent
mutate("the Redis tracing hook is attached only after the connection check succeeds", "xredis/xredis.go", "./xredis", "TestTrace_StartupConnectCheckOpensNoSpan",
       swap('\tif err := xclient.Probe(ctx, probePolicy(client.Options()), probe(client)); err != nil {',
            '\tif cfg.Trace {\n\t\t_ = redisotel.InstrumentTracing(client, redisotel.WithDBStatement(false))\n\t}\n\tif err := xclient.Probe(ctx, probePolicy(client.Options()), probe(client)); err != nil {'))
mutate("the xredis connected log carries the instance name", "xredis/xredis.go", "./xredis", "TestInstall_LogsNameAndAddrButNotPassword",
       swap('"xredis connected", "name", name,', '"xredis connected", "name", "",'))

section("Tracing")
# Using xredis brings tracing along and the caller need not remember to import xtrace. Remove this
# line and the global TracerProvider stays a noop: the Spans record nothing, the logs carry no
# trace_id, and nothing is reported at all
mutate("using xredis gives tracing without importing xtrace separately", "xredis/xredis.go", "./xredis", "TestTracingWorksWithoutImportingXtrace",
       swap('\t_ "github.com/jupiter-eng/xone/xtrace"\n', ''))

section("The command log")
# Log is off by default: attaching the hook means one line per command, and a busy service fills
# the logging platform up first
mutate("the Redis command log is attached only while Log is on", "xredis/xredis.go", "./xredis", "TestLog_OffMeansNoCommandLines",
       swap('\tif cfg.Log {\n\t\tclient.AddHook(logHook{', '\tif true {\n\t\tclient.AddHook(logHook{'))
mutate("Log on attaches the Redis command log", "xredis/xredis.go", "./xredis", "TestLog_CommandLineHasNameCmdFirstKeyElapsed",
       swap('\t\tclient.AddHook(logHook{name: name, slow: cfg.SlowThreshold})\n', ''))
mutate("the Redis slow threshold is handed to the hook", "xredis/xredis.go", "./xredis", "TestLog_SlowThreshold",
       swap('slow: cfg.SlowThreshold}', 'slow: 0}'))
# Aimed at the call site: without build passing the name down, the logs of several instances
# cannot be told apart
mutate("the Redis command log carries the instance name from the configuration", "xredis/xredis.go", "./xredis", "TestLog_InstanceNameFromConfigReachesLog",
       swap('newClient(ctx, name, c)', 'newClient(ctx, "", c)'))
# With the logging hook attached outside the tracing hook, the log's span_id becomes the caller's
# and no longer matches this command's Span
mutate("the Redis command log sits inside the redis Span", "xredis/xredis.go", "./xredis", "TestLog_CarriesRedisSpanOfCallerTrace",
       swap('\tif cfg.Log {\n\t\tclient.AddHook(logHook{name: name, slow: cfg.SlowThreshold})\n\t}\n', ''),
       swap('\tif cfg.Trace {\n\t\t// Turn db.statement off', '\tif cfg.Log {\n\t\tclient.AddHook(logHook{name: name, slow: cfg.SlowThreshold})\n\t}\n\tif cfg.Trace {\n\t\t// Turn db.statement off'))
mutate("the Redis command log uses the command's ctx", "xredis/log.go", "./xredis", "TestLog_CarriesRedisSpanOfCallerTrace",
       swap('slog.InfoContext(ctx, "redis command"', 'slog.InfoContext(context.Background(), "redis command"'))
# A read-through cache misses all day long: counted as a failure it floods the WARNs and a real
# failure drowns in them
mutate("redis.Nil is not a failure", "xredis/log.go", "./xredis", "TestLog_NilIsNotAFailure",
       swap('failed := err != nil && !errors.Is(err, redis.Nil)', 'failed := err != nil'))
mutate("a redis.Nil inside a pipeline is not a failure", "xredis/log.go", "./xredis", "TestLog_PipelineNilIsNotFailureButLaterErrorIs",
       swap('\tif err == nil || errors.Is(err, redis.Nil) || txFailed {\n', '\tif err == nil || txFailed {\n'))
mutate("a pipeline recognizes the real error after a nil", "xredis/log.go", "./xredis", "TestLog_PipelineNilIsNotFailureButLaterErrorIs",
       swap('if e := c.Err(); e != nil && !errors.Is(e, redis.Nil) && !errors.Is(e, redis.TxFailedErr) {', 'if e := c.Err(); false && e != nil {'))
mutate("a pipeline logs the first real error, not the last", "xredis/log.go", "./xredis", "TestLog_PipelineReportsFirstError",
       swap('\t\t\t\terr = e\n\t\t\t\tbreak\n', '\t\t\t\terr = e\n'))
# A WATCH conflict is a proto.RedisError (the same type as a server-side error) and used to be
# logged as a redis pipeline failed WARN
mutate("a WATCH conflict is not a failure", "xredis/log.go", "./xredis", "TestLog_WatchConflict",
       swap('\ttxFailed := errors.Is(err, redis.TxFailedErr)\n', '\ttxFailed := false\n'))
mutate("on a WATCH conflict the TxFailedErr on each command is not a failure either", "xredis/log.go", "./xredis", "TestLog_WatchConflict",
       swap(' && !errors.Is(e, redis.TxFailedErr) {', ' {'))
mutate("a WATCH conflict carries tx_failed", "xredis/log.go", "./xredis", "TestLog_WatchConflict",
       swap('\t\tattrs = append(attrs, "tx_failed", true)\n', ''))
# cmd.Name() is the first argument lowercased verbatim: Do(ctx, "SET k1 <value>") brings the value
# into cmd. Aimed at both call sites
mutate("a command name that does not look like one is not logged", "xredis/log.go", "./xredis", "TestLog_InvalidCmdNameReplaced",
       swap('attrs = append(attrs, "cmd", cmdName(cmd))', 'attrs = append(attrs, "cmd", cmd.Name())'))
mutate("a pipeline command name that does not look like one is not logged", "xredis/log.go", "./xredis", "TestLog_InvalidCmdNameReplaced",
       swap('names = append(names, cmdName(c))', 'names = append(names, c.Name())'))
mutate("a command name is at most 64 bytes", "xredis/log.go", "./xredis", "TestLog_InvalidCmdNameReplaced",
       swap('[a-z0-9._|-]{0,63}$', '[a-z0-9._|-]*$'))
mutate("a command that is both slow and failed is logged as failed", "xredis/log.go", "./xredis", "TestLog_SlowAndFailedLogsFailed",
       swap('\tcase failed:\n', '\tcase failed && !h.isSlow(elapsed):\n'))
mutate("a pipeline that is both slow and failed is logged as failed", "xredis/log.go", "./xredis", "TestLog_SlowAndFailedLogsFailed",
       swap('\tcase err != nil:\n', '\tcase err != nil && !h.isSlow(elapsed):\n'))
mutate("failures and slow commands are still logged when slog only takes WARN", "xredis/log.go", "./xredis", "TestLog_WarnLevelHandlerStillGetsProblems",
       swap('if !failed && !h.isSlow(elapsed) && !slog.Default().Enabled(ctx, slog.LevelInfo) {', 'if !slog.Default().Enabled(ctx, slog.LevelInfo) {'))
mutate("failures and slow pipelines are still logged when slog only takes WARN", "xredis/log.go", "./xredis", "TestLog_WarnLevelHandlerStillGetsProblems",
       swap('if err == nil && !h.isSlow(elapsed) && !slog.Default().Enabled(ctx, slog.LevelInfo) {', 'if !slog.Default().Enabled(ctx, slog.LevelInfo) {'))
mutate("a cancelled ctx is logged verbatim", "xredis/log.go", "./xredis", "TestLog_ContextCanceledKeepsText",
       swap('errors.Is(err, context.Canceled) || ', ''))
mutate("the Redis command log keeps the elapsed time to the microsecond", "xredis/log.go", "./xredis", "TestMs_KeepsSubMillisecond",
       swap('func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }', 'func ms(d time.Duration) float64 { return float64(d.Milliseconds()) }'))
# Only the first key is logged: off by one and a SET's value becomes the key and lands in the log
mutate("the Redis command log logs no value", "xredis/log.go", "./xredis", "TestLog_ValuesAndOtherArgsNeverLogged|TestFirstKey",
       swap('\treturn argString(args, pos)\n}', '\treturn argString(args, pos+1)\n}'))
# go-redis's own fallback is "anything not on the no-key list has the key as its first argument":
# AUTH's user name and MIGRATE's host would both be taken for a key
mutate("a command outside the list logs no key", "xredis/log.go", "./xredis", "TestLog_ValuesAndOtherArgsNeverLogged|TestFirstKey",
       swap('\t\tif _, ok := keyAtFirstArg[name]; ok {\n', '\t\tif true {\n'))
mutate("with EVAL's numkeys at 0 the third argument is ARGV", "xredis/log.go", "./xredis", "TestFirstKey",
       swap('\t\tif numKeys(args) > 0 {\n', '\t\tif len(args) > 3 {\n'))
mutate("XREADGROUP skips the group and consumer names", "xredis/log.go", "./xredis", "TestFirstKey",
       swap('pos = afterStreams(args, 4)', 'pos = afterStreams(args, 1)'))
mutate("an overlong key is truncated", "xredis/log.go", "./xredis", "TestLog_LongKeyTruncated",
       swap('\tif len(key) <= maxLoggedKey {\n', '\tif true {\n'))
# A run of 0x80 has no character start: searching back all the way would truncate it to an empty
# string
mutate("a long key that is not UTF-8 is not truncated to an empty string", "xredis/log.go", "./xredis", "TestKeyAttrs_InvalidUTF8NotTruncatedToEmpty",
       swap('\tif !utf8.RuneStart(key[cut]) {\n\t\tcut = maxLoggedKey\n\t}\n', ''))
# Measured on Redis 7.0.15: the text of ERR unknown command carries the arguments out with it
mutate("a Redis server error logs the error code alone", "xredis/log.go", "./xredis", "TestLog_FailedCommandLogsOnlyErrorCode",
       swap('return "redis server error " + code + " (message omitted, it may contain argument values)", code',
            'return err.Error(), code'))
# return {err=ARGV[1]} inside EVAL: the whole error text is the value, and taking the first word
# as the error code logs that value
mutate("only a known Redis error code is recognized", "xredis/log.go", "./xredis", "TestLog_FailedCommandLogsOnlyErrorCode",
       swap('if _, ok := knownErrorCodes[code]; ok {', 'if code != "" {'))
# go-redis writes the reply into the error when it fails to parse one
mutate("an unrecognized client error is not logged verbatim", "xredis/log.go", "./xredis", "TestErrorAttrs",
       swap('return "redis client error (message omitted, it may contain reply data)", ""', 'return err.Error(), ""'))
mutate("a negative Redis SlowThreshold is caught", "xredis/config.go", "./xredis", "TestValidate",
       swap('\t\t{"SlowThreshold", c.SlowThreshold},\n', ''))
