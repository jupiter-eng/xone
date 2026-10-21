# Mutations for clickhouse: the promises of the clickhouse module. The format is in scripts/mutations/__init__.py.
from . import cut, mutate, section, swap

section("clients")
mutate("the ClickHouse probe budget uses the dial_timeout from the DSN", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestResolve_DSNValuesNotOverridden",
       swap('\t\tProbeTimeout: 2 * cmp.Or(opts.DialTimeout, driverDialTimeout),\n', '\t\tProbeTimeout: 2*cmp.Or(c.DialTimeout, driverDialTimeout+0*opts.DialTimeout),\n'))
# With DialTimeout at 0 and nothing in the DSN the driver connects with a 30s budget: computing it from 0
# leaves only xgorm's 1s fallback, and measured against a server whose first byte is 1.5s late all 3
# attempts timed out and startup failed.
mutate("the ClickHouse probe budget follows the dial_timeout after the driver filled in its defaults", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestResolve_ZeroTimeoutProbesWithDriverDefault",
       swap('\t\tProbeTimeout: 2 * cmp.Or(opts.DialTimeout, driverDialTimeout),\n', '\t\tProbeTimeout: 2 * cmp.Or(opts.DialTimeout, 0*driverDialTimeout),\n'))
# The driver queries the version in Initialize with context.Background(): a cancelled ctx still waits out
# dial_timeout, and a failure gets no retry at all.
# When the database is given as ?database= the URL path is empty: looking only at the path would make the db
# in logs and Spans an empty string, or the one that was overridden.
mutate("the ClickHouse db is the database name the driver really uses", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestResolve_DBIsTheOneDriverUses",
       swap('\t\tDB:           opts.Auth.Database,\n', '\t\tDB:           strings.TrimPrefix(u.Path, "/"),\n'))
mutate("the first ClickHouse connection honors the ctx and is retried", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestNew",
       swap('SkipInitializeWithVersion: true', 'SkipInitializeWithVersion: false'))
# Parsing and re-running q.Encode() would reorder the caller's parameters by key and escape commas and
# slashes: aimed at the call site that does the injection.
mutate("injecting dial_timeout does not rewrite the caller's query", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestResolve_UserQueryKeptVerbatim",
       swap('\t\tu.RawQuery = appendQuery(u.RawQuery, dialTimeoutKey, c.DialTimeout.String())\n',
            '\t\tq.Set(dialTimeoutKey, c.DialTimeout.String())\n\t\tu.RawQuery = q.Encode()\n'))
mutate("ClickHouse still queries the version", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestRegister",
       swap('\tReady:      probeVersion,\n', ''))
# Passing it through would put the driver's parse error, together with the whole DSN including the plaintext
# password, into the log.
mutate("a ClickHouse DSN that is not a URL is rejected", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestResolve",
       swap('\t\treturn "", xgorm.ConnInfo{}, errNotURL\n', '\t\treturn c.DSN, xgorm.ConnInfo{Driver: string(Driver)}, nil\n'))
# The URL Host of a multi-host DSN is the whole string "h1:9000,h2:9000": taking it as the address would make
# the Span's server.address that whole string with no server.port, and the connect log would not hold a
# "host:port" either.
mutate("a multi-host ClickHouse DSN records the first host", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestResolve_MultiHostDSNRecordsFirstHost",
       swap('\t\tAddr:         opts.Addr[0],\n', '\t\tAddr:         u.Host,\n'))
mutate("the ClickHouse driver's parse error does not echo the DSN", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestResolve",
       swap('parseDSN(dsn, c.TLS.Enable)\n\tif err != nil {\n\t\treturn "", xgorm.ConnInfo{}, errMalformedDSN',
     'parseDSN(dsn, c.TLS.Enable)\n\tif err != nil {\n\t\treturn "", xgorm.ConnInfo{}, err'))
mutate("a ClickHouse auth failure is not retried", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestNew_NoRetryOnAuthFailure|TestDialect_",
       swap('\tAuthFailed: authFailed,\n', ''))
mutate("ClickHouse recognizes the older authentication error codes", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestDialect_",
       swap('\tchproto.ErrWrongPassword,        // 193\n', ''))
mutate("the ClickHouse dialect recognizes the server's error code", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestDialect_",
       swap('\tErrorCode:  errorCode,\n', ''))
# The ClickHouse TLS block: aimed at the registered dialect and at the call sites of resolve and openTLS.
mutate("ClickHouse accepts a TLS block", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestRegister_OpenTLS|TestNew_TLSBlock",
       swap('\tOpenTLS:    openTLS,\n', ''))
mutate("the ClickHouse connection config carries the TLS block", "xgorm/clickhouse/tls.go", "./xgorm/clickhouse", "TestNew_TLSBlock",
       swap('\topts.TLS = cfg.Clone()\n', ''))
mutate("a ClickHouse TLS block and TLS parameters in the DSN cannot both be set", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestResolve_TLSBlock",
       swap('\t\tif err := checkTLS(u, q); err != nil {', '\t\tif err := checkTLS(u, q); false && err != nil {'))
# http:// builds the request address from the scheme recorded at parse time and sends plaintext even with a
# *tls.Config in hand.
mutate("ClickHouse http:// with a TLS block is a configuration error", "xgorm/clickhouse/tls.go", "./xgorm/clickhouse", "TestResolve_TLSBlockRejectsHTTP",
       swap('if u.Scheme == "http" {', 'if false {'))
mutate("with a ClickHouse TLS block https needs no secure", "xgorm/clickhouse/clickhouse.go", "./xgorm/clickhouse", "TestResolve_TLSBlockHTTPSNeedsNoSecureAndDSNUnchanged",
       swap('opts, err := parseDSN(dsn, c.TLS.Enable)', 'opts, err := chgo.ParseDSN(dsn)'))
mutate("ClickHouse openTLS parses https as https", "xgorm/clickhouse/tls.go", "./xgorm/clickhouse", "TestNew_TLSBlockApplies_NativeAndHTTPSOnlyUseTLS",
       swap('opts, err := parseDSN(dsn, true)', 'opts, err := parseDSN(dsn, false)'))
mutate("adding secure to a ClickHouse https DSN is what keeps the scheme", "xgorm/clickhouse/tls.go", "./xgorm/clickhouse", "TestParseDSN_HttpsNeedsSecureToKeepScheme|TestNew_TLSBlockApplies_NativeAndHTTPSOnlyUseTLS",
       swap('\t\tq.Set("secure", "true")\n', '\t\tq.Set("secure", "false")\n'))
# Given a DSN, GORM's driver parses its own copy and UpdateLocalTable uses it to connect to each host
# directly, without the TLS block.
mutate("under TLS the DSN is not handed to GORM's ClickHouse driver", "xgorm/clickhouse/tls.go", "./xgorm/clickhouse", "TestOpenTLS",
       swap('clickhouse.Config{Conn: chgo.OpenDB(opts),', 'clickhouse.Config{DSN: dsn, Conn: chgo.OpenDB(opts),'))
mutate("under TLS the driver's own version query is turned off as well", "xgorm/clickhouse/tls.go", "./xgorm/clickhouse", "TestOpenTLS",
       swap('SkipInitializeWithVersion: true', 'SkipInitializeWithVersion: false'))
mutate("the parse error in ClickHouse openTLS does not echo the DSN", "xgorm/clickhouse/tls.go", "./xgorm/clickhouse", "TestOpenTLS",
       swap("\t\treturn nil, errMalformedDSN // the driver's error", "\t\treturn nil, err // the driver's error"))
