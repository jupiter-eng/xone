# Mutations for xgorm: the promises of the xgorm module. The format is in scripts/mutations/__init__.py.
from . import cut, mutate, section, swap

section("configuration")
# The call site: all three integrations must hand their own defaults in when reading the config.
mutate("xgorm spreads its own defaults over several instances", "xgorm/config.go", "./xgorm", "TestConfig_SingleInstanceForm",
       swap('xconfig.UnmarshalClients(ConfigKey, DefaultClientConfig)', 'xconfig.UnmarshalClients(ConfigKey, func() ClientConfig { return ClientConfig{} })'))

section("middleware")
# pgx's error text is the whole DSN, and its own masking covers only two tidy spellings.
# Aimed at the call site: bypassing parsePostgres and calling pgx directly puts the password in the log with its error.
mutate("a PG DSN that cannot be parsed does not carry the password into the error", "xgorm/dsn.go", "./xgorm", "TestResolvePostgres_",
       swap('\tpc, err := parsePostgres(dsn)\n', '\tpc, err := pgconn.ParseConfig(dsn)\n'))
# gorm's postgres driver connects through pgx.ParseConfig, which validates three more items than pgconn.
# Falling back to pgconn for the pre-check would let those three through to the connection, and
# password = hunter2 would go out with pgx's error.
mutate("the PG pre-check uses the same parser as connecting", "xgorm/dsn.go", "./xgorm", "TestNew_PG_PgxValidationErrorsKeepPasswordOut",
       swap('\tcc, err := pgx.ParseConfig(dsn)\n\tif err == nil {\n\t\treturn &cc.Config, nil\n',
     '\t_ = pgx.ParseConfig\n\tcc, err := pgconn.ParseConfig(dsn)\n\tif err == nil {\n\t\treturn cc, nil\n'))
# gorm reads the raw string with a regexp and does not decode it: re-encoding the whole query turns
# Asia/Shanghai into Asia%2FShanghai.
mutate("adding parameters to a URL does not rewrite the caller's query", "xgorm/dsn.go", "./xgorm", "TestResolveDSN_PG_URLAddsParamsWithoutRewritingUserQuery",
       swap('\tu.RawQuery = strings.Join(pairs, "&")\n',
     '\tu.RawQuery = strings.Join(pairs, "&")\n\tif all, err := url.ParseQuery(u.RawQuery); err == nil {\n\t\tu.RawQuery = all.Encode()\n\t}\n'))
mutate("a value added to a URL does not have its / encoded", "xgorm/dsn.go", "./xgorm", "TestResolveDSN_PG_URLAddsParamsWithoutRewritingUserQuery",
       swap('queryEscape(k)+"="+queryEscape(injects[k])', 'queryEscape(k)+"="+url.QueryEscape(injects[k])'))
mutate("an unreadable PG certificate file is named in the error", "xgorm/dsn.go", "./xgorm", "TestResolvePostgres_",
       swap('\t\treturn nil, fmt.Errorf("cannot read a file named in the DSN: %w", pathErr)', '\t\treturn nil, errMalformedDSN'))
# Placing the defaults first instead of checking whether the DSN already sets something is exactly what
# removes this kind of detection. The mutation puts a naive "skip if already set" back: a connect_timeout=
# inside the password is enough to fool it.
mutate("a parameter name inside the password does not fool the injection", "xgorm/dsn.go", "./xgorm", "TestResolveDSN_PGParamNameInPasswordNotMistaken",
       swap('\t\tif skipTimeZone && gormTimeZone.MatchString(k+"=") {\n',
     '\t\tif strings.Contains(dsn, k+"=") || skipTimeZone && gormTimeZone.MatchString(k+"=") {\n'))

section("the registration board")
# A client-style integration must declare StageClient explicitly: leaving it out drops it into the default
# business stage, alongside the business hooks that use it.
mutate("xgorm is ready before the business stage", "xgorm/xgorm.go", "./xgorm", "TestRegister_MatchesFramework",
       swap('xhook.BeforeStart(initXGorm, xhook.At(xhook.StageClient))', 'xhook.BeforeStart(initXGorm)'))

section("clients")
# Aimed at the call site: without going through Build when nothing is configured, the registry stays at
# "not started yet" and C() reports "not configured" as "called too early".
mutate("with nothing configured the registry still learns that startup ran", "xgorm/xgorm.go", "./xgorm", "TestInitXGorm_CSaysNotConfiguredRatherThanTooEarly",
       swap('\t\treturn xclient.Build(ctx, reg, nil, build)\n', '\t\treturn nil\n'))
# The connection info is parsed by pgx. The mutation puts back a solution that splits on whitespace: a
# host=... inside the password is then read as the address.
mutate("the password in the DSN does not reach the log", "xgorm/dsn.go", "./xgorm", "TestResolveDSN_PGPasswordFragmentNotTakenAsConnInfo",
       swap('\t\tAddr:         net.JoinHostPort(pc.Host, strconv.Itoa(int(pc.Port))),\n',
     '\t\tAddr:         net.JoinHostPort(naiveKV(dsn)["host"], strconv.Itoa(int(pc.Port))),\n'),
       swap('// seconds rounds up to whole seconds',
     'func naiveKV(dsn string) map[string]string {\n\tall := map[string]string{}\n\tfor _, tok := range strings.Fields(dsn) {\n\t\tif k, v, ok := strings.Cut(tok, "="); ok {\n\t\t\tall[k] = v\n\t\t}\n\t}\n\treturn all\n}\n\n// seconds rounds up to whole seconds'))
# pgx takes the last occurrence of a key: appending the defaults would override what the caller wrote
# explicitly (including the spelling connect_timeout = 10).
mutate("a key already set in the DSN is not overridden by a default", "xgorm/dsn.go", "./xgorm", "TestResolveDSN_PG",
       swap('\treturn strings.Join(pairs, " ") + " " + dsn\n', '\treturn dsn + " " + strings.Join(pairs, " ")\n'))
mutate("the parameters placed first do not run into the DSN's first item", "xgorm/dsn.go", "./xgorm", "TestResolveDSN_PG",
       swap('\treturn strings.Join(pairs, " ") + " " + dsn\n', '\treturn strings.Join(pairs, " ") + dsn\n'))
# gorm separately picks the first time zone in the DSN with a regexp, so a time zone from Params placed
# first would be taken ahead of the one the caller wrote.
mutate("no time zone is prepended when the DSN already sets one", "xgorm/dsn.go", "./xgorm", "TestResolveDSN_PG_DSNTimezoneSkipsDefaultTimezone",
       swap('\tskipTimeZone := gormTimeZone.MatchString(dsn)\n', '\tskipTimeZone := false\n'))
mutate("the first connection honors the ctx", "xgorm/xgorm.go", "./xgorm", "TestNew",
       swap('\t\tDisableAutomaticPing:                     true,\n', ''))
# The items passed to gorm.Config as they are: each one broken once, at the call site.
mutate("DisableForeignKeyConstraintWhenMigrating reaches GORM", "xgorm/xgorm.go", "./xgorm", "TestNew_GormOptionsReachGorm",
       swap('DisableForeignKeyConstraintWhenMigrating: cfg.DisableForeignKeyConstraintWhenMigrating,', 'DisableForeignKeyConstraintWhenMigrating: false,'))
mutate("SkipDefaultTransaction reaches GORM", "xgorm/xgorm.go", "./xgorm", "TestNew_GormOptionsReachGorm",
       swap('SkipDefaultTransaction:                   cfg.SkipDefaultTransaction,', 'SkipDefaultTransaction:                   false,'))
mutate("PrepareStmt reaches GORM", "xgorm/xgorm.go", "./xgorm", "TestNew_GormOptionsReachGorm",
       swap('PrepareStmt:                              cfg.PrepareStmt,', 'PrepareStmt:                              false,'))
mutate("CreateBatchSize reaches GORM", "xgorm/xgorm.go", "./xgorm", "TestNew_GormOptionsReachGorm",
       swap('CreateBatchSize:                          cfg.CreateBatchSize,', 'CreateBatchSize:                          0,'))
mutate("a negative CreateBatchSize fails startup", "xgorm/config.go", "./xgorm", "TestValidate",
       swap('if c.CreateBatchSize < 0 {', 'if false {'))
# The driver's initialization query moved into Ready precisely so it honors the ctx and is retried. Without
# the call site wiring it up, nobody runs that query at all.
mutate("the dialect's Ready runs inside the connection probe", "xgorm/xgorm.go", "./xgorm", "TestNew",
       swap('xclient.Probe(ctx, policy, probe(pool, readyOf(dialect, db)))', 'xclient.Probe(ctx, policy, probe(pool, nil))'))
# connect_timeout covers the whole connection (TCP, TLS, auth), so a shorter budget would time out a legal
# slow handshake before pgx itself gives up.
mutate("the PG probe budget covers connect_timeout", "xgorm/dsn.go", "./xgorm", "TestProbeTimeout",
       swap('\treturn cmp.Or(connect, ceilSeconds(dial)) + dial\n', '\treturn dial + 0*cmp.Or(connect, ceilSeconds(dial))\n'))
# A 0 for statement_timeout and friends means no limit: rounding anything under 1ms down to 0 would make the
# timeout silently disappear.
mutate("PG millisecond timeouts round up", "xgorm/dsn.go", "./xgorm", "TestSecondsMillis",
       swap('int64((d+time.Millisecond-1)/time.Millisecond)', 'd.Milliseconds()'))
# The timeouts in the config are only defaults. If the DSN sets something longer the driver waits that long,
# and a budget still computed from the config would time out a slow but legal connection before the driver
# gives up. Aimed at the call site that reads the budget (the ProbeTimeout each dialect fills in) and at the
# line in each dialect that computes it.
mutate("the probe budget widens to the timeout written in the DSN", "xgorm/xgorm.go", "./xgorm", "TestProbeTimeout",
       swap('\treturn cmp.Or(info.ProbeTimeout, 2*cfg.DialTimeout, fallbackPingTimeout)\n', '\treturn cmp.Or(2*cfg.DialTimeout, fallbackPingTimeout)\n'))
mutate("the PG probe budget uses the connect_timeout from the DSN", "xgorm/dsn.go", "./xgorm", "TestProbeTimeout_DSN",
       swap('ProbeTimeout: postgresProbeTimeout(pc.ConnectTimeout, c.DialTimeout),', 'ProbeTimeout: postgresProbeTimeout(0, c.DialTimeout),'))
mutate("the MySQL probe budget uses the timeouts from the DSN", "xgorm/mysql.go", "./xgorm", "TestProbeTimeout_DSN",
       swap('ProbeTimeout: mysqlProbeTimeout(cfg.Timeout, cfg.ReadTimeout),', 'ProbeTimeout: mysqlProbeTimeout(c.DialTimeout, c.MySQL.ReadTimeout),'))
# ReadTimeout: 0 means "no limit in the driver": still adding 0 to the budget would leave only the dial
# timeout, and it would time out before the login packet comes back.
mutate("a MySQL timeout set to 0 gets a budget of the same order as the other one", "xgorm/mysql.go", "./xgorm", "TestProbeTimeout$",
       swap('\treturn cmp.Or(dial, read) + cmp.Or(read, dial)\n', '\treturn cmp.Or(dial) + read\n'))
# GORM leaves parameters out of the SQL only when the Logger implements ParamsFilter: otherwise the log holds
# the real parameter values and credentials go out with them.
mutate("SQL logs hold no parameter values", "xgorm/logger.go", "./xgorm", "TestLogger",
       swap('func (l *gormLogger) ParamsFilter(', 'func (l *gormLogger) paramsFilter('))
# The Recorder that Scan borrows does not consult the instance Logger's ParamsFilter, only the process-wide
# RecorderParamsFilter: aimed at the line in init that wires it up.
mutate("the SQL log of a Scan holds no parameter values", "xgorm/xgorm.go", "./xgorm", "TestLogger_Scan",
       swap('\tlogger.RecorderParamsFilter = withoutParams\n', ''))
# When the PG dialect's Explain has no parameters to substitute it leaves $1 as $1$, and the logged statement
# no longer matches what was sent. Aimed at the call site that records the SQL and at the place that detects
# the dialect's placeholders.
mutate("the PG SQL log is exactly what was sent", "xgorm/logger.go", "./xgorm", "TestLogger",
       swap('\t\tsql, rows := l.statement(fc)\n\t\tslog.InfoContext', '\t\tsql, rows := fc()\n\t\tslog.InfoContext'))
mutate("a dialect that rewrites $N placeholders is recognized", "xgorm/logger.go", "./xgorm", "TestLogger",
       swap('numbered:       d.Explain("$1") == "$1$",', 'numbered:       false,'))
mutate("an xgorm auth failure is not retried", "xgorm/xgorm.go", "./xgorm", "TestNew_|TestNew_MySQLNoRetryOnAuthFailureAndSaysSo",
       swap('\t\tAuthFailed: d.authFailed,\n', ''))
# Aimed at the call site: a zero-value dialect means "does not recognize an auth failure" and affects only
# this one path (the retry policy still asks the dialect).
mutate("an auth failure is reported as an auth failure", "xgorm/xgorm.go", "./xgorm", "TestNew_",
       swap('connectError(dialect, "cannot reach %s: %w"', 'connectError(Dialect{}, "cannot reach %s: %w"'))
# A registered dialect that connects in Initialize surfaces the auth error inside gorm.Open and never reaches
# the ping: recognizing it only on the ping path would report a wrong password as open ... failed. Aimed at
# the gorm.Open call site (which is how the built-in MySQL used to behave, before the version query moved
# into Ready and put it on the ping path).
mutate("a MySQL auth failure is reported as one on the gorm.Open path too", "xgorm/xgorm.go", "./xgorm", "TestNew_MySQLNoRetryOnAuthFailureAndSaysSo",
       swap('connectError(dialect, "open %s failed: %w"', 'connectError(Dialect{}, "open %s failed: %w"'))
mutate("MySQL 1045 / 1044 are recognized", "xgorm/mysql.go", "./xgorm", "TestNew_MySQLNoRetryOnAuthFailureAndSaysSo|TestDialect_BuiltInsRecognizeAuthFailureAndErrorCode",
       swap('return errors.As(err, &myErr) && (myErr.Number == mysqlAccessDenied || myErr.Number == mysqlDBAccessDenied)',
            'return errors.As(err, &myErr) && false'))
# The MySQL Dialector queries the version in Initialize with context.Background(): that is the first
# connection, gorm.Open returns straight away on failure, none of the three retries gets a turn, and the stop
# signal cannot reach it either.
mutate("the first MySQL connection honors the ctx and is retried", "xgorm/mysql.go", "./xgorm", "TestNew_MySQLRetriesThreeTimesWhenPeerSilent|TestMySQLDialect_OpenSkipsNetwork_VersionProbeInReady",
       swap('SkipInitializeWithVersion: true})', 'SkipInitializeWithVersion: false})'))
mutate("MySQL still queries the version", "xgorm/dialect.go", "./xgorm", "TestMySQLDialect_OpenSkipsNetwork_VersionProbeInReady|TestProbeMySQLVersion",
       swap(', Ready: probeMySQLVersion,\n', ',\n'))
mutate("the MySQL version query honors the ctx", "xgorm/mysql.go", "./xgorm", "TestProbeMySQLVersion",
       swap('db.ConnPool.QueryRowContext(ctx, "SELECT VERSION()")', 'db.ConnPool.QueryRowContext(context.Background(), "SELECT VERSION()")'))
# Without the version reaching the switches, a migration on an older version or MariaDB generates DDL it does
# not understand.
mutate("the MySQL version reaches the switches", "xgorm/mysql.go", "./xgorm", "TestProbeMySQLVersion|TestApplyMySQLVersion",
       swap('\treturn applyMySQLVersion(db, d.Config, v)\n', '\t_, _ = d, v\n\treturn nil\n'))
mutate("writes on MariaDB 10.5+ carry RETURNING", "xgorm/mysql.go", "./xgorm", "TestApplyMySQLVersion",
       swap('\t\treturn enableReturning(db)\n', '\t\treturn nil\n'))
# Aimed at the call site: without this round's ctx, a probe stuck on the handshake read makes the stop signal
# wait out the full ReadTimeout.
mutate("the MySQL connection probe gives up at once on a stop signal", "xgorm/xgorm.go", "./xgorm", "TestNew_MySQLCancelReturnsImmediatelyWithoutReadTimeout",
       swap('err := pool.PingContext(ctx)', 'err := pool.PingContext(context.Background())'))
# Recognizing an auth failure lives in the dialect: without the built-in dialects wiring it up, nothing else
# in the core recognizes it.
mutate("the built-in PG dialect recognizes an auth failure", "xgorm/dialect.go", "./xgorm", "TestDialect_BuiltInsRecognizeAuthFailureAndErrorCode",
       swap('\t\tAuthFailed: postgresAuthFailed, ErrorCode: postgresErrorCode,\n', '\t\tErrorCode: postgresErrorCode,\n'))
mutate("the built-in MySQL dialect recognizes an auth failure", "xgorm/dialect.go", "./xgorm", "TestDialect_BuiltInsRecognizeAuthFailureAndErrorCode",
       swap('\t\tAuthFailed: mysqlAuthFailed, ErrorCode: mysqlErrorCode,\n', '\t\tErrorCode: mysqlErrorCode,\n'))
mutate("the PG 28 class is recognized", "xgorm/dsn.go", "./xgorm", "TestDialect_BuiltInsRecognizeAuthFailureAndErrorCode|TestNew_",
       swap('return strings.HasPrefix(postgresErrorCode(err), "28")', 'return postgresErrorCode(err) == "28P01"'))
# The server's error text carries parameter values (the Duplicate entry 'a@b.com' of MySQL's 1062): both
# exits, the log and the Span, are aimed at their call sites, plus the place where the dialect wires up the
# error code.
mutate("SQL logs do not record the server's error text", "xgorm/logger.go", "./xgorm", "TestLogger_ServerErrorLogsCodeNotMessage",
       swap('attrs(sql, rows, elapsed, l.errorAttrs(err)...)', 'attrs(sql, rows, elapsed, "error", err)'))
mutate("Spans do not record the server's error text", "xgorm/trace.go", "./xgorm", "TestSpan_ServerErrorRecordsCodeNotMessage",
       swap('\t\t\trecordError(span, d, db.Error)\n', '\t\t\tspan.RecordError(db.Error)\n\t\t\tspan.SetStatus(codes.Error, db.Error.Error())\n'))
mutate("RecordError is not called on a Span for a server error", "xgorm/trace.go", "./xgorm", "TestSpan_ServerErrorRecordsCodeNotMessage",
       swap('\t\tspan.SetAttributes(semconv.DBResponseStatusCode(code), semconv.ErrorTypeKey.String(code))\n',
            '\t\tspan.SetAttributes(semconv.DBResponseStatusCode(code), semconv.ErrorTypeKey.String(code))\n\t\tspan.RecordError(err)\n'))
# Client errors carry parameter values too: a pgx encoding failure writes the whole parameter with %#v, and a
# failed database/sql scan writes the value read back with %q. The allowlist is aimed at redactedError, the
# Span exit's RecordError gets a mutation of its own, and so does "record only the one recognized, not the
# whole chain" (GORM's AddError prepends the previous error's text).
mutate("a client error outside the allowlist is not recorded verbatim", "xgorm/dialect.go", "./xgorm", "TestRedactedError_ClientErrors|TestLogger_ClientErrorKeepsParamValuesOut|TestSpan_ClientErrorKeepsParamValuesOut",
       swap('\treturn clientErrorOmitted, ""\n', '\treturn err.Error(), ""\n'))
mutate("a Span's exception.message records only the recognized safe error", "xgorm/trace.go", "./xgorm", "TestSpan_ClientErrorKeepsParamValuesOut",
       swap('\t\t\tspan.RecordError(safe)\n', '\t\t\tspan.RecordError(err)\n'))
mutate("RecordError is not called for an unrecognized client error", "xgorm/trace.go", "./xgorm", "TestSpan_ClientErrorKeepsParamValuesOut",
       swap('\t\tspan.SetAttributes(semconv.ErrorTypeOther)\n', '\t\tspan.RecordError(err)\n\t\tspan.SetAttributes(semconv.ErrorTypeOther)\n'))
mutate("an allowlisted error records only the one recognized", "xgorm/dialect.go", "./xgorm", "TestRedactedError_ClientErrors",
       swap('\t\tif errors.Is(err, safe) {\n\t\t\treturn safe\n', '\t\tif errors.Is(err, safe) {\n\t\t\treturn err\n'))
mutate("the MySQL dialect recognizes the server's error code", "xgorm/dialect.go", "./xgorm", "TestLogger_ServerErrorLogsCodeNotMessage|TestSpan_ServerErrorRecordsCodeNotMessage",
       swap('AuthFailed: mysqlAuthFailed, ErrorCode: mysqlErrorCode,', 'AuthFailed: mysqlAuthFailed,'))
mutate("the PG dialect recognizes the server's error code", "xgorm/dialect.go", "./xgorm", "TestLogger_ServerErrorLogsCodeNotMessage|TestSpan_ServerErrorRecordsCodeNotMessage",
       swap('AuthFailed: postgresAuthFailed, ErrorCode: postgresErrorCode,', 'AuthFailed: postgresAuthFailed,'))
# The driver defaults to parseTime=false: a DATETIME cannot be scanned into a time.Time.
mutate("MySQL adds parseTime=true when the DSN does not set it", "xgorm/mysql.go", "./xgorm", "TestResolveDSN_MySQLDefaultsParseTimeToTrue",
       swap('\tif !mysqlParamSet(c.DSN, "parseTime") {\n', '\tif false && !mysqlParamSet(c.DSN, "parseTime") {\n'))
mutate("a parseTime written in the DSN wins", "xgorm/mysql.go", "./xgorm", "TestResolveDSN_MySQLDefaultsParseTimeToTrue",
       swap('\tif !mysqlParamSet(c.DSN, "parseTime") {\n', '\tif true {\n'))
mutate("a parseTime inside the password does not fool it", "xgorm/mysql.go", "./xgorm", "TestResolveDSN_MySQLDefaultsParseTimeToTrue",
       swap("\ti := strings.LastIndexByte(dsn, '/')\n", "\ti := strings.IndexByte(dsn, '/')\n"))
# The config is validated as it is read: every layer below silently turns a negative duration into "no limit".
mutate("a negative duration is rejected", "xgorm/config.go", "./xgorm", "TestValidate",
       swap('\t\tif f.val < 0 {\n', '\t\tif false && f.val < 0 {\n'))
mutate("New validates the config too", "xgorm/xgorm.go", "./xgorm", "TestNew_NoConnectOnBadConfig",
       swap('\tif err := cfg.Validate(); err != nil {\n', '\tif err := error(nil); err != nil {\n'))
mutate("Validate names the instance when there are several", "xgorm/config.go", "./xgorm", "TestConfig_ValidateNamesInstance",
       swap('errs = append(errs, fmt.Errorf("Clients.%s: %w", name, err))', 'errs = append(errs, err)'))
# The connect log must say which instance it is: aimed at the call site where build passes the name to open.
mutate("the connect log carries the instance name", "xgorm/xgorm.go", "./xgorm", "TestInstall_ConnectLogNamesInstance",
       swap('open(ctx, name, c)', 'open(ctx, "", c)'))
# The names from OTel's database semantic conventions: putting the old names back makes dashboards and
# collection rules stop matching.
mutate("the Span uses the semantic convention's db.query.text", "xgorm/trace.go", "./xgorm", "TestSpan_IncludesConnInfoAndSQL",
       swap('span.SetAttributes(semconv.DBQueryText(sql),', 'span.SetAttributes(attribute.String("db.statement", sql),'))
mutate("the Span's db.system.name is postgresql", "xgorm/trace.go", "./xgorm", "TestConnAttrs",
       swap('\t\treturn "postgresql"\n', '\t\treturn driver\n'))
mutate("the Span splits the address into host and port", "xgorm/trace.go", "./xgorm", "TestSpan_IncludesConnInfoAndSQL|TestConnAttrs",
       swap('\tout = append(out, semconv.ServerAddress(host))\n', '\tout = append(out, semconv.ServerAddress(info.Addr+host[:0]))\n'))
mutate("db.operation.name takes the statement's first keyword", "xgorm/trace.go", "./xgorm", "TestSpan_IncludesConnInfoAndSQL",
       swap('\tif op := operationName(sql); op != "" {\n', '\tif op := ""; op != "" {\n'))
mutate("the connection-pool metrics are named db_pool_*", "xgorm/metric.go", "./xgorm", "TestPoolCollector|TestInstall",
       swap('{"db_pool_open", ', '{"db_connections_open", '))
# Without this, go-sql-driver writes plain [mysql] ... text to stderr, which is not JSON.
mutate("go-sql-driver's own log goes into slog", "xgorm/xgorm.go", "./xgorm", "TestNew_MySQLDriverLogsGoToSlog",
       swap('\t_ = mysqldriver.SetLogger(mysqlDriverLogger{})', '\t_ = mysqldriver.SetLogger(nil) // it only errors on a nil argument, so nothing is set'))
# The collector is a single process-wide object that walks every instance when scraped; without consulting
# each instance's own switch, Metric: false is an empty phrase.
mutate("a database instance with Metric off is not exported", "xgorm/xgorm.go", "./xgorm", "TestInstall", swap('\t\tif !inst.metric {\n', '\t\tif false {\n'))
# A process-wide "install only once" would block the install after xmetric is reinstalled: the new Registry
# would hold no pool collector, and none of these metrics would be exported in the second lifecycle.
mutate("the database pool metrics are still exported after xmetric is reinstalled", "xgorm/xgorm.go", "./xgorm", "TestInstall",
       swap('func installPoolMetrics() {\n\tif _, err := xmetric.RegisterAs(',
     'var poolInstalled bool\n\nfunc installPoolMetrics() {\n\tif poolInstalled {\n\t\treturn\n\t}\n\tpoolInstalled = true\n\tif _, err := xmetric.RegisterAs('))
mutate("XGorm validates the TLS block", "xgorm/config.go", "./xgorm", "TestValidate_TLSBlock|TestConfig_TLSBlockFromConfigFile",
       swap('\tif err := c.TLS.Validate(); err != nil {\n\t\treturn err\n\t}\n', ''))
mutate("configuring a TLS block for a dialect that does not accept one fails", "xgorm/config.go", "./xgorm", "TestValidate_TLSBlock",
       swap('if c.TLS.Enable && d.OpenTLS == nil {', 'if c.TLS.Enable && d.OpenTLS == nil && false {'))
mutate("a configured TLS block goes through OpenTLS", "xgorm/dialect.go", "./xgorm", "TestNew_PG_TLS|TestNew_MySQL_TLS",
       swap('\tif cfg == nil {\n\t\treturn d.Open(dsn), nil\n\t}', '\tif true {\n\t\treturn d.Open(dsn), nil\n\t}'))
# pgx's default prefer does not verify the certificate and switches to plaintext when the server refuses TLS.
mutate("PG swaps in the TLS block's config when connecting", "xgorm/tls.go", "./xgorm", "TestNew_PG_TLS",
       swap('\t\t\tusePostgresTLS(&cc.Config, cfg)\n', ''))
mutate("with a PG TLS block there is no fallback to plaintext", "xgorm/tls.go", "./xgorm", "TestNew_PG_TLSBlockFailsInsteadOfPlaintextFallback|TestUsePostgresTLS",
       swap('\tc.Fallbacks = hosts[1:]\n', ''))
mutate("without a configured ServerName PG verifies against the host name", "xgorm/tls.go", "./xgorm", "TestNew_PG_TLSBlockApplies|TestUsePostgresTLS",
       swap('\t\t\tif tc.ServerName == "" {\n\t\t\t\ttc.ServerName = h.Host\n\t\t\t}\n', ''))
mutate("a PG TLS block and ssl parameters in the DSN cannot both be set", "xgorm/dsn.go", "./xgorm", "TestResolveDSN_PG_TLSBlockConflictsWithDSNSSLParams",
       swap('\t\tif err := checkPostgresTLSParams(dsn); err != nil {', '\t\tif err := checkPostgresTLSParams(dsn); false && err != nil {'))
mutate("the PG conflict check looks at the DSN after Params have been merged in", "xgorm/dsn.go", "./xgorm", "TestResolveDSN_PG_TLSBlockConflictsWithDSNSSLParams",
       swap('checkPostgresTLSParams(dsn)', 'checkPostgresTLSParams(c.DSN)'))
mutate("only parameters starting with ssl count as a PG conflict", "xgorm/tls.go", "./xgorm", "TestResolveDSN_PG_TLSBlockConflictsWithDSNSSLParams",
       swap('if strings.HasPrefix(key, "ssl") {', 'if strings.HasPrefix(key, "") {'))
mutate("a PG TLS block does not accept a Unix socket", "xgorm/dsn.go", "./xgorm", "TestResolveDSN_PG_TLSBlockRejectsUnixSocket",
       swap('\t\tif err := checkPostgresTCP(pc); err != nil {', '\t\tif err := checkPostgresTCP(pc); false && err != nil {'))
mutate("a MySQL TLS block and a tls option in the DSN cannot both be set", "xgorm/mysql.go", "./xgorm", "TestResolveDSN_MySQL_TLSBlockConflictsWithDSNTLS",
       swap('\t\tif err := checkMySQLTLS(c.DSN, cfg); err != nil {', '\t\tif err := checkMySQLTLS(c.DSN, cfg); false && err != nil {'))
# The CLIENT_SSL bit in the greeting arrives in plaintext; a man in the middle clearing it makes a driver with
# allowFallbackToPlaintext=true send the login packet in plaintext. One mutation for the validation call site
# and one for the config handed to the driver.
mutate("a MySQL TLS block does not accept allowFallbackToPlaintext", "xgorm/tls.go", "./xgorm", "TestNew_MySQL_TLSBlockRejectsPlaintextFallbackParam",
       swap('\tif mysqlParamSet(dsn, "allowFallbackToPlaintext") {', '\tif false && mysqlParamSet(dsn, "allowFallbackToPlaintext") {'))
mutate("the connection config of a MySQL TLS block pins away the plaintext fallback", "xgorm/tls.go", "./xgorm", "TestOpenMySQLTLS_NeverFallsBackToPlaintext",
       swap('\tdc.AllowFallbackToPlaintext = false\n', ''))
mutate("the MySQL connection config carries the TLS block", "xgorm/tls.go", "./xgorm", "TestNew_MySQL_TLS",
       swap('\tdc.TLS = cfg.Clone()\n', ''))
mutate("with Log off GORM does not write to standard output itself", "xgorm/xgorm.go", "./xgorm", "TestNew",
       swap('gormCfg.Logger = logger.Discard','gormCfg.Logger = logger.Default'))

section("tracing")
# Using xgorm brings tracing with it, and the caller does not have to remember to import xtrace separately.
# Removing this line leaves the global TracerProvider a noop forever: Spans record nothing, logs have no
# trace_id, and nothing is reported at all.
mutate("tracing works with xgorm without importing xtrace separately", "xgorm/xgorm.go", "./xgorm", "TestTracingWorksWithoutImportingXtrace",
       swap('\t_ "github.com/jupiter-eng/xone/xtrace"\n', ''))

# The slow-query threshold and the elapsed time must both be milliseconds: in nanoseconds threshold_ms would
# read as 10000000.
mutate("the slow-query log's threshold is in milliseconds", "xgorm/logger.go", "./xgorm", "TestLogger_SlowQueryLogsWarn",
       swap('"threshold_ms", ms(l.slowThreshold))', '"threshold_ms", float64(l.slowThreshold))'))
