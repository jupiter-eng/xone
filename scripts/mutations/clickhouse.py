# clickhouse 的变异：module clickhouse 里的承诺。写法见 scripts/mutations/__init__.py
from . import cut, mutate, section, swap

section("客户端")
mutate("ClickHouse 的探测预算用 DSN 里的 dial_timeout", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestResolve_DSNValuesNotOverridden",
       swap('\t\tProbeTimeout: 2 * cmp.Or(opts.DialTimeout, driverDialTimeout),\n', '\t\tProbeTimeout: 2*cmp.Or(c.DialTimeout, driverDialTimeout+0*opts.DialTimeout),\n'))
# DialTimeout 配 0、DSN 里也没写时驱动按 30s 建连：预算按 0 算只剩 xgorm 的 1s 兜底，
# 实测服务端第一个字节晚 1.5s 的建连 3 次全超时、启动失败
mutate("ClickHouse 的探测预算按驱动补完默认值之后的 dial_timeout 算", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestResolve_ZeroTimeoutProbesWithDriverDefault",
       swap('\t\tProbeTimeout: 2 * cmp.Or(opts.DialTimeout, driverDialTimeout),\n', '\t\tProbeTimeout: 2 * cmp.Or(opts.DialTimeout, 0*driverDialTimeout),\n'))
# 驱动在 Initialize 里用 context.Background() 查版本：ctx 取消了也要等满
# dial_timeout，失败了一次重试都没有
# 库名写成 ?database= 时 URL 的路径是空的：只看路径的话日志和 Span 里的 db 是空串或者是被盖掉的那个
mutate("ClickHouse 的 db 是驱动真正用的库名", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestResolve_DBIsTheOneDriverUses",
       swap('\t\tDB:           opts.Auth.Database,\n', '\t\tDB:           strings.TrimPrefix(u.Path, "/"),\n'))
mutate("ClickHouse 首次建连受 ctx 管也会重试", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestNew",
       swap('SkipInitializeWithVersion: true', 'SkipInitializeWithVersion: false'))
# 解开再 q.Encode() 会把使用者的参数按 key 重排、逗号斜杠转义掉：打在注入的调用点上
mutate("ClickHouse 注入 dial_timeout 不改写使用者的 query", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestResolve_UserQueryKeptVerbatim",
       swap('\t\tu.RawQuery = appendQuery(u.RawQuery, dialTimeoutKey, c.DialTimeout.String())\n',
            '\t\tq.Set(dialTimeoutKey, c.DialTimeout.String())\n\t\tu.RawQuery = q.Encode()\n'))
mutate("ClickHouse 仍然查版本", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestRegister",
       swap('\tReady:      probeVersion,\n', ''))
# 原样透传的话，驱动建连时的解析错误会连同整串 DSN、包括明文密码一起进日志
mutate("ClickHouse 不是 URL 的 DSN 被拒绝", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestResolve",
       swap('\t\treturn "", xgorm.ConnInfo{}, errNotURL\n', '\t\treturn c.DSN, xgorm.ConnInfo{Driver: string(Driver)}, nil\n'))
# 多主机 DSN 的 URL Host 是整串 "h1:9000,h2:9000"：当成地址的话 Span 的 server.address 是整串、
# 没有 server.port，建连日志里也不是一个「主机:端口」
mutate("ClickHouse 多主机 DSN 记第一个主机", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestResolve_MultiHostDSNRecordsFirstHost",
       swap('\t\tAddr:         opts.Addr[0],\n', '\t\tAddr:         u.Host,\n'))
mutate("ClickHouse 驱动的解析错误不回显 DSN", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestResolve",
       swap('parseDSN(dsn, c.TLS.Enable)\n\tif err != nil {\n\t\treturn "", xgorm.ConnInfo{}, errMalformedDSN',
     'parseDSN(dsn, c.TLS.Enable)\n\tif err != nil {\n\t\treturn "", xgorm.ConnInfo{}, err'))
mutate("ClickHouse 认证失败不重试", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestNew_NoRetryOnAuthFailure|TestDialect_",
       swap('\tAuthFailed: authFailed,\n', ''))
mutate("ClickHouse 认得出老版本的认证错误码", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestDialect_",
       swap('\tchproto.ErrWrongPassword,        // 193\n', ''))
mutate("ClickHouse 方言认得出服务端错误码", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestDialect_",
       swap('\tErrorCode:  errorCode,\n', ''))
# ClickHouse 的 TLS 块：打在注册的方言、resolve 与 openTLS 的调用点上
mutate("ClickHouse 收 TLS 块", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestRegister_OpenTLS|TestNew_TLSBlock",
       swap('\tOpenTLS:    openTLS,\n', ''))
mutate("ClickHouse 连接配置带上 TLS 块", "xgorm/clickhouse/tls.go", "./xgorm/clickhouse", "TestNew_TLSBlock",
       swap('\topts.TLS = cfg.Clone()\n', ''))
mutate("ClickHouse TLS 块和 DSN 里的 TLS 参数不能同时写", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestResolve_TLSBlock",
       swap('\t\tif err := checkTLS(u, q); err != nil {', '\t\tif err := checkTLS(u, q); false && err != nil {'))
# http:// 按解析时记下的 scheme 拼请求地址，手里有 *tls.Config 也发明文
mutate("ClickHouse http:// 配 TLS 块是配置错误", "xgorm/clickhouse/tls.go", "./xgorm/clickhouse", "TestResolve_TLSBlockRejectsHTTP",
       swap('if u.Scheme == "http" {', 'if false {'))
mutate("ClickHouse 开了 TLS 块时 https 不必写 secure", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestResolve_TLSBlockHTTPSNeedsNoSecureAndDSNUnchanged",
       swap('opts, err := parseDSN(dsn, c.TLS.Enable)', 'opts, err := chgo.ParseDSN(dsn)'))
mutate("ClickHouse openTLS 按 https 解 https", "xgorm/clickhouse/tls.go", "./xgorm/clickhouse", "TestNew_TLSBlockApplies_NativeAndHTTPSOnlyUseTLS",
       swap('opts, err := parseDSN(dsn, true)', 'opts, err := parseDSN(dsn, false)'))
mutate("ClickHouse https 补 secure 才记得住 scheme", "xgorm/clickhouse/tls.go", "./xgorm/clickhouse", "TestParseDSN_HttpsNeedsSecureToKeepScheme|TestNew_TLSBlockApplies_NativeAndHTTPSOnlyUseTLS",
       swap('\t\tq.Set("secure", "true")\n', '\t\tq.Set("secure", "false")\n'))
# GORM 的驱动拿到 DSN 会另解一份，UpdateLocalTable 按它直连每台主机、不带 TLS 块
mutate("ClickHouse TLS 下不把 DSN 交给 GORM 的驱动", "xgorm/clickhouse/tls.go", "./xgorm/clickhouse", "TestOpenTLS",
       swap('clickhouse.Config{Conn: chgo.OpenDB(opts),', 'clickhouse.Config{DSN: dsn, Conn: chgo.OpenDB(opts),'))
mutate("ClickHouse TLS 下同样关掉驱动自带的查版本", "xgorm/clickhouse/tls.go", "./xgorm/clickhouse", "TestOpenTLS",
       swap('SkipInitializeWithVersion: true', 'SkipInitializeWithVersion: false'))
mutate("ClickHouse openTLS 的解析错误不回显 DSN", "xgorm/clickhouse/tls.go", "./xgorm/clickhouse", "TestOpenTLS",
       swap('\t\treturn nil, errMalformedDSN // 不回传驱动的错误', '\t\treturn nil, err // 不回传驱动的错误'))
