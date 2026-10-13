# xgorm 的变异：module xgorm 里的承诺。写法见 scripts/mutations/__init__.py
from . import cut, mutate, section, swap

section("配置")
# 调用点：三个集成读配置时要把自己的默认值交进去
mutate("xgorm 多实例铺的是自己的默认值", "xgorm/config.go", "./xgorm", "TestConfig_SingleInstanceForm",
       swap('xconfig.UnmarshalClients(ConfigKey, DefaultClientConfig)', 'xconfig.UnmarshalClients(ConfigKey, func() ClientConfig { return ClientConfig{} })'))

section("中间件")
# pgx 的错误原文里就是整串 DSN，它自己的打码只遮得住两种规整写法。
# 打在调用点上：绕开 parsePostgres 直接调 pgx，密码就跟着它的错误进了日志
mutate("PG 的 DSN 解不开时不把密码带进错误", "xgorm/dsn.go", "./xgorm", "TestResolvePostgres_",
       swap('\tpc, err := parsePostgres(dsn)\n', '\tpc, err := pgconn.ParseConfig(dsn)\n'))
# gorm 的 postgres 驱动建连用的是 pgx.ParseConfig，比 pgconn 多校验三项。
# 预检退回 pgconn 的话这三项写错会放行到建连，password = hunter2 跟着 pgx 的错误出去
mutate("PG 预检用的是建连时同一个解析器", "xgorm/dsn.go", "./xgorm", "TestNew_PG_PgxValidationErrorsKeepPasswordOut",
       swap('\tcc, err := pgx.ParseConfig(dsn)\n\tif err == nil {\n\t\treturn &cc.Config, nil\n',
     '\t_ = pgx.ParseConfig\n\tcc, err := pgconn.ParseConfig(dsn)\n\tif err == nil {\n\t\treturn cc, nil\n'))
# gorm 用正则读原串、不解码：整个 query 重新编码，Asia/Shanghai 就成了 Asia%2FShanghai
mutate("URL 形式补参数不改写使用者的 query", "xgorm/dsn.go", "./xgorm", "TestResolveDSN_PG_URLAddsParamsWithoutRewritingUserQuery",
       swap('\tu.RawQuery = strings.Join(pairs, "&")\n',
     '\tu.RawQuery = strings.Join(pairs, "&")\n\tif all, err := url.ParseQuery(u.RawQuery); err == nil {\n\t\tu.RawQuery = all.Encode()\n\t}\n'))
mutate("补进 URL 的值不编码 /", "xgorm/dsn.go", "./xgorm", "TestResolveDSN_PG_URLAddsParamsWithoutRewritingUserQuery",
       swap('queryEscape(k)+"="+queryEscape(injects[k])', 'queryEscape(k)+"="+url.QueryEscape(injects[k])'))
mutate("PG 证书文件读不到时说清是哪个文件", "xgorm/dsn.go", "./xgorm", "TestResolvePostgres_",
       swap('\t\treturn nil, fmt.Errorf("cannot read a file named in the DSN: %w", pathErr)', '\t\treturn nil, errMalformedDSN'))
# 默认值垫在前面、不去判断 DSN 里写没写过，就是为了不再有这种检测。
# 变异把一个朴素的「写过就跳过」塞回来：密码里的 connect_timeout= 就能骗过它
mutate("密码里的参数名骗不过注入", "xgorm/dsn.go", "./xgorm", "TestResolveDSN_PGParamNameInPasswordNotMistaken",
       swap('\t\tif skipTimeZone && gormTimeZone.MatchString(k+"=") {\n',
     '\t\tif strings.Contains(dsn, k+"=") || skipTimeZone && gormTimeZone.MatchString(k+"=") {\n'))

section("登记板")
# 客户端类集成要显式声明 StageClient：漏写就掉进默认的业务档，和用它的业务钩子同档
mutate("xgorm 在业务档之前就绪", "xgorm/xgorm.go", "./xgorm", "TestRegister_MatchesFramework",
       swap('xhook.BeforeStart(initXGorm, xhook.At(xhook.StageClient))', 'xhook.BeforeStart(initXGorm)'))

section("客户端")
# 打在调用点上：没配时不走 Build 的话，注册表一直停在「还没启动」，
# C() 就会把「没配」说成「调早了」
mutate("xgorm 没配也让注册表知道启动过了", "xgorm/xgorm.go", "./xgorm", "TestInitXGorm_CSaysNotConfiguredRatherThanTooEarly",
       swap('\t\treturn xclient.Build(ctx, reg, nil, build)\n', '\t\treturn nil\n'))
# 连接信息交给 pgx 解。变异换回一个按空白切的解法：密码里的 host=… 就被读成地址
mutate("DSN 里的密码不进日志", "xgorm/dsn.go", "./xgorm", "TestResolveDSN_PGPasswordFragmentNotTakenAsConnInfo",
       swap('\t\tAddr:         net.JoinHostPort(pc.Host, strconv.Itoa(int(pc.Port))),\n',
     '\t\tAddr:         net.JoinHostPort(naiveKV(dsn)["host"], strconv.Itoa(int(pc.Port))),\n'),
       swap('// seconds 向上取整为整秒',
     'func naiveKV(dsn string) map[string]string {\n\tall := map[string]string{}\n\tfor _, tok := range strings.Fields(dsn) {\n\t\tif k, v, ok := strings.Cut(tok, "="); ok {\n\t\t\tall[k] = v\n\t\t}\n\t}\n\treturn all\n}\n\n// seconds 向上取整为整秒'))
# pgx 同一个 key 取最后一次：默认值追加在后面的话，使用者显式写的值
# （包括 connect_timeout = 10 这种写法）就被盖掉了
mutate("DSN 里已写的 key 不被默认值盖掉", "xgorm/dsn.go", "./xgorm", "TestResolveDSN_PG",
       swap('\treturn strings.Join(pairs, " ") + " " + dsn\n', '\treturn dsn + " " + strings.Join(pairs, " ")\n'))
mutate("垫在前面的参数不粘到 DSN 的第一项上", "xgorm/dsn.go", "./xgorm", "TestResolveDSN_PG",
       swap('\treturn strings.Join(pairs, " ") + " " + dsn\n', '\treturn strings.Join(pairs, " ") + dsn\n'))
# gorm 另用正则取 DSN 里第一处时区，垫在前面的 Params 时区会抢在使用者写的前面
mutate("DSN 里写了时区就不垫时区", "xgorm/dsn.go", "./xgorm", "TestResolveDSN_PG_DSNTimezoneSkipsDefaultTimezone",
       swap('\tskipTimeZone := gormTimeZone.MatchString(dsn)\n', '\tskipTimeZone := false\n'))
mutate("首次建连受 ctx 管", "xgorm/xgorm.go", "./xgorm", "TestNew",
       swap('\t\tDisableAutomaticPing:                     true,\n', ''))
# 原样交给 gorm.Config 的几项，每一项在调用点上各改坏一次
mutate("DisableForeignKeyConstraintWhenMigrating 交给 GORM", "xgorm/xgorm.go", "./xgorm", "TestNew_GormOptionsReachGorm",
       swap('DisableForeignKeyConstraintWhenMigrating: cfg.DisableForeignKeyConstraintWhenMigrating,', 'DisableForeignKeyConstraintWhenMigrating: false,'))
mutate("SkipDefaultTransaction 交给 GORM", "xgorm/xgorm.go", "./xgorm", "TestNew_GormOptionsReachGorm",
       swap('SkipDefaultTransaction:                   cfg.SkipDefaultTransaction,', 'SkipDefaultTransaction:                   false,'))
mutate("PrepareStmt 交给 GORM", "xgorm/xgorm.go", "./xgorm", "TestNew_GormOptionsReachGorm",
       swap('PrepareStmt:                              cfg.PrepareStmt,', 'PrepareStmt:                              false,'))
mutate("CreateBatchSize 交给 GORM", "xgorm/xgorm.go", "./xgorm", "TestNew_GormOptionsReachGorm",
       swap('CreateBatchSize:                          cfg.CreateBatchSize,', 'CreateBatchSize:                          0,'))
mutate("CreateBatchSize 为负启动失败", "xgorm/config.go", "./xgorm", "TestValidate",
       swap('if c.CreateBatchSize < 0 {', 'if false {'))
# 驱动的初始化查询挪到 Ready 里，就是为了受 ctx 管、跟着重试。调用点不接的话，
# 那次查询就没人做了
mutate("方言的 Ready 在建连探测里执行", "xgorm/xgorm.go", "./xgorm", "TestNew",
       swap('xclient.Probe(ctx, policy, probe(pool, readyOf(dialect, db)))', 'xclient.Probe(ctx, policy, probe(pool, nil))'))
# connect_timeout 管的是整个建连（TCP、TLS、认证），预算比它短就会
# 在 pgx 自己放弃之前把一次合法的慢握手判成超时
mutate("PG 的探测预算盖住 connect_timeout", "xgorm/dsn.go", "./xgorm", "TestProbeTimeout",
       swap('\treturn cmp.Or(connect, ceilSeconds(dial)) + dial\n', '\treturn dial + 0*cmp.Or(connect, ceilSeconds(dial))\n'))
# statement_timeout 等的 0 是不限制：不满 1ms 的向下取整成 0，超时就静默消失了
mutate("PG 的毫秒超时向上取整", "xgorm/dsn.go", "./xgorm", "TestSecondsMillis",
       swap('int64((d+time.Millisecond-1)/time.Millisecond)', 'd.Milliseconds()'))
# 配置里的超时只是默认值。DSN 里写了更长的，驱动就等那么久，预算还按配置算的话
# 会在驱动放弃之前把一次慢但合法的建连判超时。打在读出预算的调用点上（方言填的 ProbeTimeout）和各方言算预算的那一行
mutate("探测预算按 DSN 里写的超时放宽", "xgorm/xgorm.go", "./xgorm", "TestProbeTimeout",
       swap('\treturn cmp.Or(info.ProbeTimeout, 2*cfg.DialTimeout, fallbackPingTimeout)\n', '\treturn cmp.Or(2*cfg.DialTimeout, fallbackPingTimeout)\n'))
mutate("PG 的探测预算用 DSN 里的 connect_timeout", "xgorm/dsn.go", "./xgorm", "TestProbeTimeout_DSN",
       swap('ProbeTimeout: postgresProbeTimeout(pc.ConnectTimeout, c.DialTimeout),', 'ProbeTimeout: postgresProbeTimeout(0, c.DialTimeout),'))
mutate("MySQL 的探测预算用 DSN 里的超时", "xgorm/mysql.go", "./xgorm", "TestProbeTimeout_DSN",
       swap('ProbeTimeout: mysqlProbeTimeout(cfg.Timeout, cfg.ReadTimeout),', 'ProbeTimeout: mysqlProbeTimeout(c.DialTimeout, c.MySQL.ReadTimeout),'))
# ReadTimeout: 0 是「驱动不限时」：预算照旧只加上 0 的话，只剩建连超时，登录包还没回来就判超时
mutate("MySQL 配成 0 的那段超时按另一段的量级算预算", "xgorm/mysql.go", "./xgorm", "TestProbeTimeout$",
       swap('\treturn cmp.Or(dial, read) + cmp.Or(read, dial)\n', '\treturn cmp.Or(dial) + read\n'))
# GORM 只在 Logger 实现了 ParamsFilter 时才不把参数代进 SQL：否则日志里就是真实的参数值，凭证跟着出去
mutate("SQL 日志里没有参数值", "xgorm/logger.go", "./xgorm", "TestLogger",
       swap('func (l *gormLogger) ParamsFilter(', 'func (l *gormLogger) paramsFilter('))
# Scan 借用的 Recorder 不问实例 Logger 的 ParamsFilter，只认进程级的 RecorderParamsFilter：
# 打在 init 里接它的那一行上
mutate("Scan 的 SQL 日志里没有参数值", "xgorm/xgorm.go", "./xgorm", "TestLogger_Scan",
       swap('\tlogger.RecorderParamsFilter = withoutParams\n', ''))
# PG 方言的 Explain 没参数可代时把 $1 留成 $1$：日志里的语句和发出去的对不上。
# 打在记 SQL 的调用点上，和判断方言占位符的那一处
mutate("PG 的 SQL 日志就是发出去的那条", "xgorm/logger.go", "./xgorm", "TestLogger",
       swap('\t\tsql, rows := l.statement(fc)\n\t\tslog.InfoContext', '\t\tsql, rows := fc()\n\t\tslog.InfoContext'))
mutate("认得出方言会改写 $N 占位符", "xgorm/logger.go", "./xgorm", "TestLogger",
       swap('numbered:       d.Explain("$1") == "$1$",', 'numbered:       false,'))
mutate("xgorm 认证失败不重试", "xgorm/xgorm.go", "./xgorm", "TestNew_|TestNew_MySQLNoRetryOnAuthFailureAndSaysSo",
       swap('\t\tAuthFailed: d.authFailed,\n', ''))
# 打在调用点上：换成零值方言就是「认不出认证失败」，只影响这一条路（重试策略仍按方言认）
mutate("认证失败报的是认证失败", "xgorm/xgorm.go", "./xgorm", "TestNew_",
       swap('connectError(dialect, "cannot reach %s: %w"', 'connectError(Dialect{}, "cannot reach %s: %w"'))
# 注册进来的方言要是在 Initialize 里建连，认证错误就在 gorm.Open 里出来，走不到 ping：
# 只在 ping 那条路上认的话，密码错报的是 open … failed。打在 gorm.Open 的调用点上
# （内置的 MySQL 原先就是这样，查版本挪进 Ready 之后走的是 ping 那条路）
mutate("MySQL 认证失败在 gorm.Open 那条路上也报认证失败", "xgorm/xgorm.go", "./xgorm", "TestNew_MySQLNoRetryOnAuthFailureAndSaysSo",
       swap('connectError(dialect, "open %s failed: %w"', 'connectError(Dialect{}, "open %s failed: %w"'))
mutate("认得出 MySQL 的 1045 / 1044", "xgorm/mysql.go", "./xgorm", "TestNew_MySQLNoRetryOnAuthFailureAndSaysSo|TestDialect_BuiltInsRecognizeAuthFailureAndErrorCode",
       swap('return errors.As(err, &myErr) && (myErr.Number == mysqlAccessDenied || myErr.Number == mysqlDBAccessDenied)',
            'return errors.As(err, &myErr) && false'))
# MySQL 的 Dialector 在 Initialize 里用 context.Background() 查版本：那是第一次建连，
# 失败了 gorm.Open 直接返回，三次重试一次都没轮上，退出信号也管不到
mutate("MySQL 首次建连受 ctx 管也会重试", "xgorm/mysql.go", "./xgorm", "TestNew_MySQLRetriesThreeTimesWhenPeerSilent|TestMySQLDialect_OpenSkipsNetwork_VersionProbeInReady",
       swap('SkipInitializeWithVersion: true})', 'SkipInitializeWithVersion: false})'))
mutate("MySQL 仍然查版本", "xgorm/dialect.go", "./xgorm", "TestMySQLDialect_OpenSkipsNetwork_VersionProbeInReady|TestProbeMySQLVersion",
       swap(', Ready: probeMySQLVersion,\n', ',\n'))
mutate("MySQL 查版本受 ctx 管", "xgorm/mysql.go", "./xgorm", "TestProbeMySQLVersion",
       swap('db.ConnPool.QueryRowContext(ctx, "SELECT VERSION()")', 'db.ConnPool.QueryRowContext(context.Background(), "SELECT VERSION()")'))
# 版本号不设进开关的话，老版本 / MariaDB 上迁移生成它不认的 DDL
mutate("MySQL 的版本号设进开关", "xgorm/mysql.go", "./xgorm", "TestProbeMySQLVersion|TestApplyMySQLVersion",
       swap('\treturn applyMySQLVersion(db, d.Config, v)\n', '\t_, _ = d, v\n\treturn nil\n'))
mutate("MariaDB 10.5+ 的增删改带 RETURNING", "xgorm/mysql.go", "./xgorm", "TestApplyMySQLVersion",
       swap('\t\treturn enableReturning(db)\n', '\t\treturn nil\n'))
# 打在调用点上：探测不用这一轮的 ctx，卡在握手读上时退出信号要等满 ReadTimeout
mutate("MySQL 建连探测收到退出信号当场放弃", "xgorm/xgorm.go", "./xgorm", "TestNew_MySQLCancelReturnsImmediatelyWithoutReadTimeout",
       swap('err := pool.PingContext(ctx)', 'err := pool.PingContext(context.Background())'))
# 认证失败的识别住在方言里：内置方言不接上的话，核心里再没有别处认得出来
mutate("内置 PG 方言认得出认证失败", "xgorm/dialect.go", "./xgorm", "TestDialect_BuiltInsRecognizeAuthFailureAndErrorCode",
       swap('\t\tAuthFailed: postgresAuthFailed, ErrorCode: postgresErrorCode,\n', '\t\tErrorCode: postgresErrorCode,\n'))
mutate("内置 MySQL 方言认得出认证失败", "xgorm/dialect.go", "./xgorm", "TestDialect_BuiltInsRecognizeAuthFailureAndErrorCode",
       swap('\t\tAuthFailed: mysqlAuthFailed, ErrorCode: mysqlErrorCode,\n', '\t\tErrorCode: mysqlErrorCode,\n'))
mutate("认得出 PG 的 28 类", "xgorm/dsn.go", "./xgorm", "TestDialect_BuiltInsRecognizeAuthFailureAndErrorCode|TestNew_",
       swap('return strings.HasPrefix(postgresErrorCode(err), "28")', 'return postgresErrorCode(err) == "28P01"'))
# 服务端错误原文里带着参数值（MySQL 1062 的 Duplicate entry 'a@b.com'）：
# 日志和 Span 两个出口都要打在调用点上，外加方言接上错误码的那一处
mutate("SQL 日志不记服务端错误原文", "xgorm/logger.go", "./xgorm", "TestLogger_ServerErrorLogsCodeNotMessage",
       swap('attrs(sql, rows, elapsed, l.errorAttrs(err)...)', 'attrs(sql, rows, elapsed, "error", err)'))
mutate("Span 不记服务端错误原文", "xgorm/trace.go", "./xgorm", "TestSpan_ServerErrorRecordsCodeNotMessage",
       swap('\t\t\trecordError(span, d, db.Error)\n', '\t\t\tspan.RecordError(db.Error)\n\t\t\tspan.SetStatus(codes.Error, db.Error.Error())\n'))
mutate("Span 服务端报错时不调 RecordError", "xgorm/trace.go", "./xgorm", "TestSpan_ServerErrorRecordsCodeNotMessage",
       swap('\t\tspan.SetAttributes(semconv.DBResponseStatusCode(code), semconv.ErrorTypeKey.String(code))\n',
            '\t\tspan.SetAttributes(semconv.DBResponseStatusCode(code), semconv.ErrorTypeKey.String(code))\n\t\tspan.RecordError(err)\n'))
# 客户端的错也带参数值：pgx 编码失败用 %#v 写出整个参数、database/sql 扫描失败用 %q 写出读回来的值。
# 白名单放行打在 redactedError 上，Span 那个出口的 RecordError 另打一条，
# 外加「只记认出来的那一个，不记整条链」（GORM 的 AddError 把前一个错误的原文拼在前面）
mutate("客户端的错不在白名单里就不记原文", "xgorm/dialect.go", "./xgorm", "TestRedactedError_ClientErrors|TestLogger_ClientErrorKeepsParamValuesOut|TestSpan_ClientErrorKeepsParamValuesOut",
       swap('\treturn clientErrorOmitted, ""\n', '\treturn err.Error(), ""\n'))
mutate("Span 的 exception.message 只记认出来的安全错误", "xgorm/trace.go", "./xgorm", "TestSpan_ClientErrorKeepsParamValuesOut",
       swap('\t\t\tspan.RecordError(safe)\n', '\t\t\tspan.RecordError(err)\n'))
mutate("认不出的客户端错误不调 RecordError", "xgorm/trace.go", "./xgorm", "TestSpan_ClientErrorKeepsParamValuesOut",
       swap('\t\tspan.SetAttributes(semconv.ErrorTypeOther)\n', '\t\tspan.RecordError(err)\n\t\tspan.SetAttributes(semconv.ErrorTypeOther)\n'))
mutate("白名单里的错误只记认出来的那一个", "xgorm/dialect.go", "./xgorm", "TestRedactedError_ClientErrors",
       swap('\t\tif errors.Is(err, safe) {\n\t\t\treturn safe\n', '\t\tif errors.Is(err, safe) {\n\t\t\treturn err\n'))
mutate("MySQL 方言认得出服务端错误码", "xgorm/dialect.go", "./xgorm", "TestLogger_ServerErrorLogsCodeNotMessage|TestSpan_ServerErrorRecordsCodeNotMessage",
       swap('AuthFailed: mysqlAuthFailed, ErrorCode: mysqlErrorCode,', 'AuthFailed: mysqlAuthFailed,'))
mutate("PG 方言认得出服务端错误码", "xgorm/dialect.go", "./xgorm", "TestLogger_ServerErrorLogsCodeNotMessage|TestSpan_ServerErrorRecordsCodeNotMessage",
       swap('AuthFailed: postgresAuthFailed, ErrorCode: postgresErrorCode,', 'AuthFailed: postgresAuthFailed,'))
# 驱动默认 parseTime=false：DATETIME 扫不进 time.Time
mutate("MySQL 没写 parseTime 时补成 true", "xgorm/mysql.go", "./xgorm", "TestResolveDSN_MySQLDefaultsParseTimeToTrue",
       swap('\tif !mysqlParamSet(c.DSN, "parseTime") {\n', '\tif false && !mysqlParamSet(c.DSN, "parseTime") {\n'))
mutate("DSN 里写了 parseTime 以 DSN 为准", "xgorm/mysql.go", "./xgorm", "TestResolveDSN_MySQLDefaultsParseTimeToTrue",
       swap('\tif !mysqlParamSet(c.DSN, "parseTime") {\n', '\tif true {\n'))
mutate("密码里的 parseTime 骗不过它", "xgorm/mysql.go", "./xgorm", "TestResolveDSN_MySQLDefaultsParseTimeToTrue",
       swap("\ti := strings.LastIndexByte(dsn, '/')\n", "\ti := strings.IndexByte(dsn, '/')\n"))
# 配置在读的时候就校验：负的时长底下每一处都静默变成「不限」
mutate("负的时长被拒", "xgorm/config.go", "./xgorm", "TestValidate",
       swap('\t\tif f.val < 0 {\n', '\t\tif false && f.val < 0 {\n'))
mutate("New 也校验配置", "xgorm/xgorm.go", "./xgorm", "TestNew_NoConnectOnBadConfig",
       swap('\tif err := cfg.Validate(); err != nil {\n', '\tif err := error(nil); err != nil {\n'))
mutate("多实例的 Validate 点名实例", "xgorm/config.go", "./xgorm", "TestConfig_ValidateNamesInstance",
       swap('errs = append(errs, fmt.Errorf("Clients.%s: %w", name, err))', 'errs = append(errs, err)'))
# 建连日志要写是哪个实例：打在 build 往 open 传名字的调用点上
mutate("建连日志写着实例名", "xgorm/xgorm.go", "./xgorm", "TestInstall_ConnectLogNamesInstance",
       swap('open(ctx, name, c)', 'open(ctx, "", c)'))
# OTel 数据库语义约定的名字：旧名字换回来，看板和采集规则就对不上
mutate("Span 用语义约定的 db.query.text", "xgorm/trace.go", "./xgorm", "TestSpan_IncludesConnInfoAndSQL",
       swap('span.SetAttributes(semconv.DBQueryText(sql),', 'span.SetAttributes(attribute.String("db.statement", sql),'))
mutate("Span 的 db.system.name 是 postgresql", "xgorm/trace.go", "./xgorm", "TestConnAttrs",
       swap('\t\treturn "postgresql"\n', '\t\treturn driver\n'))
mutate("Span 的地址拆成主机和端口", "xgorm/trace.go", "./xgorm", "TestSpan_IncludesConnInfoAndSQL|TestConnAttrs",
       swap('\tout = append(out, semconv.ServerAddress(host))\n', '\tout = append(out, semconv.ServerAddress(info.Addr+host[:0]))\n'))
mutate("db.operation.name 取语句的第一个关键字", "xgorm/trace.go", "./xgorm", "TestSpan_IncludesConnInfoAndSQL",
       swap('\tif op := operationName(sql); op != "" {\n', '\tif op := ""; op != "" {\n'))
mutate("连接池指标叫 db_pool_*", "xgorm/metric.go", "./xgorm", "TestPoolCollector|TestInstall",
       swap('{"db_pool_open", ', '{"db_connections_open", '))
# 不接的话 go-sql-driver 往 stderr 写 [mysql] … 纯文本，不是 JSON
mutate("go-sql-driver 自己的日志进 slog", "xgorm/xgorm.go", "./xgorm", "TestNew_MySQLDriverLogsGoToSlog",
       swap('\t_ = mysqldriver.SetLogger(mysqlDriverLogger{})', '\t_ = mysqldriver.SetLogger(nil) // 只在参数为 nil 时报错，于是什么都没设'))
# collector 是进程级的一个、抓取时遍历全部实例，不看实例自己的开关，
# Metric: false 就是一句空话
mutate("Metric 关掉的数据库实例不导出", "xgorm/xgorm.go", "./xgorm", "TestInstall", swap('\t\tif !inst.metric {\n', '\t\tif false {\n'))
# 进程级的「只挂一次」会把 xmetric 重装之后的那次挡掉：新 Registry 上
# 没有连接池 collector，第二轮生命周期里这组指标一个都导不出去
mutate("xmetric 重装后数据库连接池指标照样导出", "xgorm/xgorm.go", "./xgorm", "TestInstall",
       swap('func installPoolMetrics() {\n\tif _, err := xmetric.RegisterAs(',
     'var poolInstalled bool\n\nfunc installPoolMetrics() {\n\tif poolInstalled {\n\t\treturn\n\t}\n\tpoolInstalled = true\n\tif _, err := xmetric.RegisterAs('))
mutate("XGorm 校验 TLS 块", "xgorm/config.go", "./xgorm", "TestValidate_TLSBlock|TestConfig_TLSBlockFromConfigFile",
       swap('\tif err := c.TLS.Validate(); err != nil {\n\t\treturn err\n\t}\n', ''))
mutate("方言不收 TLS 块时配了要失败", "xgorm/config.go", "./xgorm", "TestValidate_TLSBlock",
       swap('if c.TLS.Enable && d.OpenTLS == nil {', 'if c.TLS.Enable && d.OpenTLS == nil && false {'))
mutate("开了 TLS 块走 OpenTLS", "xgorm/dialect.go", "./xgorm", "TestNew_PG_TLS|TestNew_MySQL_TLS",
       swap('\tif cfg == nil {\n\t\treturn d.Open(dsn), nil\n\t}', '\tif true {\n\t\treturn d.Open(dsn), nil\n\t}'))
# pgx 默认的 prefer 不校验证书、服务端不肯 TLS 就改走明文
mutate("PG 建连时换上 TLS 块的配置", "xgorm/tls.go", "./xgorm", "TestNew_PG_TLS",
       swap('\t\t\tusePostgresTLS(&cc.Config, cfg)\n', ''))
mutate("PG 开了 TLS 块不退回明文", "xgorm/tls.go", "./xgorm", "TestNew_PG_TLSBlockFailsInsteadOfPlaintextFallback|TestUsePostgresTLS",
       swap('\tc.Fallbacks = hosts[1:]\n', ''))
mutate("PG 没配 ServerName 时按主机名比对", "xgorm/tls.go", "./xgorm", "TestNew_PG_TLSBlockApplies|TestUsePostgresTLS",
       swap('\t\t\tif tc.ServerName == "" {\n\t\t\t\ttc.ServerName = h.Host\n\t\t\t}\n', ''))
mutate("PG TLS 块和 DSN 里的 ssl 参数不能同时写", "xgorm/dsn.go", "./xgorm", "TestResolveDSN_PG_TLSBlockConflictsWithDSNSSLParams",
       swap('\t\tif err := checkPostgresTLSParams(dsn); err != nil {', '\t\tif err := checkPostgresTLSParams(dsn); false && err != nil {'))
mutate("PG 冲突查的是补完 Params 之后的 DSN", "xgorm/dsn.go", "./xgorm", "TestResolveDSN_PG_TLSBlockConflictsWithDSNSSLParams",
       swap('checkPostgresTLSParams(dsn)', 'checkPostgresTLSParams(c.DSN)'))
mutate("PG 只有 ssl 开头的参数算冲突", "xgorm/tls.go", "./xgorm", "TestResolveDSN_PG_TLSBlockConflictsWithDSNSSLParams",
       swap('if strings.HasPrefix(key, "ssl") {', 'if strings.HasPrefix(key, "") {'))
mutate("PG TLS 块不收 Unix socket", "xgorm/dsn.go", "./xgorm", "TestResolveDSN_PG_TLSBlockRejectsUnixSocket",
       swap('\t\tif err := checkPostgresTCP(pc); err != nil {', '\t\tif err := checkPostgresTCP(pc); false && err != nil {'))
mutate("MySQL TLS 块和 DSN 里的 tls 不能同时写", "xgorm/mysql.go", "./xgorm", "TestResolveDSN_MySQL_TLSBlockConflictsWithDSNTLS",
       swap('\t\tif err := checkMySQLTLS(c.DSN, cfg); err != nil {', '\t\tif err := checkMySQLTLS(c.DSN, cfg); false && err != nil {'))
# 问候包里的 CLIENT_SSL 是明文的，中间人清掉它，allowFallbackToPlaintext=true 的驱动就改发明文登录包。
# 校验的调用点和交给驱动的配置各打一条
mutate("MySQL TLS 块不收 allowFallbackToPlaintext", "xgorm/tls.go", "./xgorm", "TestNew_MySQL_TLSBlockRejectsPlaintextFallbackParam",
       swap('\tif mysqlParamSet(dsn, "allowFallbackToPlaintext") {', '\tif false && mysqlParamSet(dsn, "allowFallbackToPlaintext") {'))
mutate("MySQL TLS 块的连接配置钉死不退回明文", "xgorm/tls.go", "./xgorm", "TestOpenMySQLTLS_NeverFallsBackToPlaintext",
       swap('\tdc.AllowFallbackToPlaintext = false\n', ''))
mutate("MySQL 连接配置带上 TLS 块", "xgorm/tls.go", "./xgorm", "TestNew_MySQL_TLS",
       swap('\tdc.TLS = cfg.Clone()\n', ''))
mutate("Log 关掉时 GORM 不自己往标准输出写", "xgorm/xgorm.go", "./xgorm", "TestNew",
       swap('gormCfg.Logger = logger.Discard','gormCfg.Logger = logger.Default'))

section("链路")
# 用了 xgorm 就有链路，使用者不用记得另外 import xtrace。摘掉这一行，
# 全局的 TracerProvider 就一直是 noop：Span 什么都不记、日志没有 trace_id，而且没有任何报错
mutate("用了 xgorm 不另外 import xtrace 也有链路", "xgorm/xgorm.go", "./xgorm", "TestTracingWorksWithoutImportingXtrace",
       swap('\t_ "github.com/jupiter-eng/xone/xtrace"\n', ''))

# 慢查询的阈值和耗时都要是毫秒：写成纳秒的话 threshold_ms 看着是 10000000
mutate("慢查询日志的阈值是毫秒", "xgorm/logger.go", "./xgorm", "TestLogger_SlowQueryLogsWarn",
       swap('"threshold_ms", ms(l.slowThreshold))', '"threshold_ms", float64(l.slowThreshold))'))

