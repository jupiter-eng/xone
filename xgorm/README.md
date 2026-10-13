# xgorm

数据库客户端：拿到的是原生 `*gorm.DB`，框架按配置建好、退出时关掉。

- 内置 MySQL / PostgreSQL，ClickHouse 驱动在 [`xgorm/clickhouse`](clickhouse/README.md)
- 单实例、多实例都行，`xgorm.C("name")` 按名字取
- 每条 SQL 一个 Span，属性按 OTel 数据库语义约定 v1.43.0
- 连接池指标 `db_pool_*`，按实例开关
- SQL 日志默认关；日志和 Span 里只有带占位符的 SQL，不记参数值
- 启动时探一次，连不上直接启动失败

## 快速上手

```yaml
# conf/application.yml
XGorm:
  DSN: "${DB_DSN}"   # postgres://app:secret@db:5432/app，Driver 默认 postgres
```

```go
import (
	"gorm.io/gorm"

	"github.com/jupiter-eng/xone/xgorm"
)

// ctx 一定要带，截止时间、链路才传得下去。Web 请求里传 c.Request.Context()
var a Account
err := xgorm.CWithCtx(ctx).First(&a, id).Error // 没查到是 gorm.ErrRecordNotFound

// 事务：回调返回错误就回滚，返回 nil 就提交
err = xgorm.CWithCtx(ctx).Transaction(func(tx *gorm.DB) error {
	return tx.Create(&order).Error
})
```

`xgorm.CWithCtx(ctx, "report")` 就是 `xgorm.C("report").WithContext(ctx)` 的简写。

## 配置

`mysql` / `postgres` 内置，ClickHouse 见[其它驱动](clickhouse/README.md#配置)。

```yaml
XGorm:
  Driver: postgres         # mysql / postgres，默认 postgres
  DSN: "${DB_DSN}"         # 必填
  DialTimeout: 500ms       # 建连超时，注入 DSN（MySQL 的 timeout、PG 的 connect_timeout）；0 = 不注入
  MaxOpenConns: 50         # 必须 > 0
  MaxIdleConns: 50         # 0 = 一条空闲连接都不留
  MaxLifetime: 5m          # 0 = 不限
  MaxIdleTime: 5m          # 0 = 不限
  Log: false               # 把 SQL 接到 slog，默认关；只记占位符，不记参数值
  SlowThreshold: 3s        # 超过就记 warn，需 Log 开启；0 = 不记
  IgnoreNotFound: false    # true：「没查到记录」不记 SQL failed 日志（Log 开着时）；返回的错误不变
  DisableForeignKeyConstraintWhenMigrating: false  # true：AutoMigrate 建表时不建外键约束；查询、Preload 不受影响
  SkipDefaultTransaction: false  # true：单条 Create / Update / Delete 不自动包事务
  PrepareStmt: false       # true：缓存预编译语句；经 PgBouncer 这类连接池时先量过再开
  CreateBatchSize: 0       # Create 切片时每条 INSERT 最多几行，0 = 整个切片一条
  Trace: true              # 每条 SQL 一个 Span
  Metric: true             # 连接池指标 db_pool_*，按实例生效
  MySQL:                   # 仅 Driver: mysql 生效
    ReadTimeout: 3s        # 等一次回包的上限，0 = 不注入（驱动不限时）
    WriteTimeout: 5s
  Postgres:                # 仅 Driver: postgres 生效，随建连发给服务端成为会话级参数
    StatementTimeout: 0s   # 服务端计时，默认不限；管不到网络那头不回话
    LockTimeout: 0s
    IdleInTxTimeout: 0s
    Params: {}             # 任意 PG 运行时参数 / pgx 连接参数，同名时以它为准
  TLS:                     # 规则见 xtls；空 ServerName = DSN 里的主机名
    Enable: false
    CAFile: ""
    CertFile: ""
    KeyFile: ""
    ServerName: ""
```

**多实例**写在 `Clients` 下，每个实例的字段同上，没写的用上面的默认值：

```yaml
XGorm:
  Clients:
    default: {DSN: "${DB_DSN}"}                                     # PostgreSQL
    report:  {DSN: "${REPORT_DSN}", Driver: mysql, MaxOpenConns: 5} # report:secret@tcp(report-db:3306)/report
```

`xgorm.C()` 取 `default`，`xgorm.C("report")` 取另一个。两种写法不能混用。

- **DSN 里写了的参数以 DSN 为准**，配置里的超时只是默认值。时长、`MaxIdleConns`、`CreateBatchSize` 都不能为负。
- 单次建连探测的预算，按 DSN 里最终生效的值算（理由见[「行为与实测」](#行为与实测)）：

  | 驱动 | 预算 | 默认配置下 |
  |---|---|---|
  | MySQL | `timeout + readTimeout`；一个是 0 时按另一个算两份，都是 0 用 1s | 3.5s |
  | PostgreSQL | `connect_timeout + DialTimeout` | 1.5s |
| ClickHouse | `2 × dial_timeout`；都没写时按驱动自己的 30s 算 | 1s |
  | 其余驱动 | 方言自己推算；推算不出是 `2 × DialTimeout` | — |

## API

| 函数 | 说明 |
|---|---|
| `C(name ...string) *gorm.DB` | 取实例，不带参数取 `default`。取不到直接 panic，消息里说清是调早了、没配还是名字写错 |
| `CWithCtx(ctx, name ...string) *gorm.DB` | 取实例并绑定 ctx，即 `C(name...).WithContext(ctx)`；GORM 只能这样传 ctx |
| `Has(name ...string) bool` | 实例配了没有，可选依赖先判断 |
| `Names() []string` | 配了哪些实例 |
| `New(ctx, cfg) (*gorm.DB, io.Closer, error)` | 纯构造器：不碰全局、不读配置文件，离开框架也能用；`io.Closer` 关闭底层连接池 |
| `RegisterDialect(d Dialect)` | 注册别的驱动，同名重复注册直接 panic；`Dialect` 提供了 `OpenTLS` 才收 TLS 块 |
| `Drivers() []Driver` | 已注册的驱动名，按字母序 |

## 注意事项

- **ctx 一定要带，后台任务一定要给截止时间。** PostgreSQL 没有读超时：对端不回话时只有 ctx 的截止时间管得住查询
  （`StatementTimeout` 是服务端计时，救不了这一段）。见[「PostgreSQL」](#postgresql)。
- **MySQL 的 DSN 没写 `parseTime` 时补成 `parseTime=true`**，`DATETIME` 扫得进 `time.Time`，时区跟着驱动的 `loc`（默认 UTC）。
  见[「MySQL」](#mysql)。
- **SQL 日志默认关**（`Log: false` 就是真的不写，不是 GORM 那个带颜色写 stdout 的默认）。日志和 Span 里只有带占位符的 SQL；
  服务端报错只记错误码，客户端的错只有认得出不带参数值的几类才记原文。返回给你的错误原样不变，
  `errors.As` 照样取得到 `*mysql.MySQLError` / `*pgconn.PgError`。见[「通用」](#通用)。
- **TLS 写在 `TLS:` 块里**，证书一律校验、不会退回明文。开着它时 DSN 里不能再写 TLS 参数（PG 的 `sslmode`、`sslrootcert`
  等 `ssl*`，MySQL 的 `tls=`、`allowFallbackToPlaintext=`），也不能走 Unix socket。
  不开时 pgx 默认的 `sslmode=prefer` 连上了也**不校验证书**。见 [xtls](../xtls/README.md)。
- **经 PgBouncer（事务池）** 要改 `default_query_exec_mode`，否则并发下成批报 `prepared statement … already exists`。
  见[「PostgreSQL」](#postgresql)。
- **启动探测**：每个实例探一次，最多试 3 次；密码错不重试，报 `authentication to <addr> failed`。见
  [behavior.md「启动期建连探测」](../docs/behavior.md#启动期建连探测)。

## 可观测

### 日志

| 消息 | 级别 | 字段 |
|---|---|---|
| `xgorm connected` | INFO | `name`、`driver`、`addr`、`db`、`tls`、`max_open_conns`、`max_idle_conns` |
| `xgorm ready` | INFO | `instances` |
| `SQL` / `slow SQL` / `SQL failed` | INFO / WARN / ERROR | `sql`（带占位符）、`elapsed_ms`、`rows_affected`（行数未知时不带）；失败时 `error`、`error_code`；慢查询时 `threshold_ms`（需 `XGorm.Log: true`） |
| `xgorm go-sql-driver log` | WARN | `detail`：go-sql-driver 自己的日志原文，不带 trace_id（见[「MySQL」](#mysql)） |

日志的全局约定（`trace_id` 注入、`xlog.AddKV`、框架的启停日志）见 [`docs/observability.md`](../docs/observability.md#日志)。

### 指标

| 指标 | 类型 | 标签 | 来源 |
|---|---|---|---|
| `db_pool_open` / `db_pool_in_use` / `db_pool_idle` / `db_pool_max_open` | gauge | `name`（实例名） | xgorm，`XGorm.Metric`，按实例 |
| `db_pool_wait_total` / `db_pool_wait_duration_seconds_total` / `db_pool_closed_max_idle_total` / `db_pool_closed_max_lifetime_total` | counter | `name` | xgorm |

指标名的前缀、常量标签和几条通用规则见 [`docs/observability.md`「指标」](../docs/observability.md#指标)。

### 链路

| 来源 | Span 名 | 关键属性 |
|---|---|---|
| xgorm | `gorm.create` / `gorm.query` / `gorm.update` / `gorm.delete` / `gorm.row` / `gorm.raw` | 见下 |

属性按 OTel 数据库语义约定 v1.43.0（Tracer 带着这一版的 schema URL）：

| 属性 | 值 |
|---|---|
| `db.system.name` | `postgresql` / `mysql` / `clickhouse`（其余驱动按驱动名） |
| `db.namespace` | 库名 |
| `server.address` / `server.port` | 从 DSN 解出的主机、端口（多主机时是第一个） |
| `db.query.text` | 带占位符的 SQL，不含参数值 |
| `db.operation.name` | 发出去的语句的第一个关键字，原样大小写：`SELECT`、`INSERT`……（软删除发出去的是 `UPDATE`） |
| `db.rows_affected` | 影响行数（ClickHouse 上永远是 0） |
| `db.response.status_code` / `error.type` | 服务端报错时的错误码（MySQL 错误号 / PG 的 SQLSTATE）；其余错误 `error.type` 是 `_OTHER` |

`gorm.ErrRecordNotFound` 不把 Span 标成错误。链路的全貌、传播与信任边界见 [`docs/observability.md`「链路」](../docs/observability.md#链路)。

## 行为与实测

实测环境和跨模块的总表见 [`docs/behavior.md`](../docs/behavior.md)。

### 通用

GORM v1.31.2。

**`gorm.Open` 自己 ping 一次**，用它自己的 context：退出信号和重试都管不到。这里关掉（`DisableAutomaticPing`），
改走框架的建连探测。

**不给 Logger 不等于不打日志**：GORM 会补上它自己的默认实现，带 ANSI 颜色直写 `os.Stdout`。
`Log: false` 时这里换成什么都不写的 Logger。

**`AutoMigrate` 默认按关联建外键。** 实测 PostgreSQL 16，`Owner` has many `Order` 两张表建出 `fk_owners_orders`，
插一条 `owner_id` 不存在的 `order` 被拒。`DisableForeignKeyConstraintWhenMigrating: true` 时不建外键，这条照样插进去，
`Preload("Orders")` 照常查得出来。它只管建表那一刻，已经建好的外键不会被删。

**三个性能相关的开关**（本机回环，量级参考，不是基准）：

- `SkipDefaultTransaction`：500 次单条 Create，PostgreSQL 16 每次约 500µs → 230µs，MySQL 8.0 约 1.0ms → 0.9ms
  （省掉 `BEGIN` / `COMMIT` 两个来回）。默认不开，因为省下的来回是拿正确性换的：`AfterCreate` 钩子返回错误时，
  默认的事务把插入回滚了（0 行），跳过之后那一行留了下来。只在热点路径上跳过，用
  `xgorm.C().Session(&gorm.Session{SkipDefaultTransaction: true})`。
- `PrepareStmt`：2000 次同一条带参数的查询，MySQL 约 180µs → 94µs（不开时驱动每条 prepare / execute / close 三个来回），
  PostgreSQL 约 100µs → 85µs（pgx 本来就缓存语句）。缓存上限和过期用 GORM 的默认（不限条数、1h）。
- `CreateBatchSize: 3` 插 10 行：两种数据库都发 4 条 INSERT；默认 0 是 1 条。

**GORM 其余的默认值原样保留**（量过）：

- `NowFunc`：`time.Now().Local()`。PG 上 GORM 给 `time.Time` 建的列是 `timestamptz`，存的是时刻，不受影响；MySQL 见 `parseTime`。
- `TranslateError: false`：打开之后 MySQL 的 1062 变成 `gorm.ErrDuplicatedKey`，但原来的 `*mysql.MySQLError`
  被整个换掉，按错误号判断的代码会静默失效。

**SQL 日志里的参数值。** GORM 的 Logger 不实现 `ParamsFilter` 时把真实参数代进 SQL。
xgorm 的 Logger 实现了它，记的是带占位符的 SQL（MySQL 是 `?`，PostgreSQL 是 `$1`；
GORM 的 PG 方言没参数可代时会写成 `$1$`，这里改回去了）。

**`Scan` 走的是另一条路。** `Raw(...).Scan(...)`、`Table(...).Select(...).Scan(...)` 执行期间 GORM 把实例的 Logger
换成 `logger.Recorder`（`finisher_api.go:539`），它不问 `ParamsFilter`，只认进程级的
`logger.RecorderParamsFilter`，而那个变量的默认值原样返回参数。实测 `WHERE name = ?` 记成了
`WHERE name = 'sql-3fa9c1d2b7e4'`，三种数据库一样。所以 xgorm 的 `init` 把这个变量换成同一个不交出参数的过滤器。
它是进程级的：使用者自己 `gorm.Open` 的实例也吃这一条；要换就在 `main` 里重新赋值。

**服务端的错误原文带着参数值**，SQL 只记占位符也就白记了：

| 服务端 | 错误 | 原文 |
|---|---|---|
| MySQL 8.0.46 | 1062 | `Error 1062 (23000): Duplicate entry 'a@b.com' for key 'm_err.email'` |
| MySQL 8.0.46 | 1366 | `Incorrect integer value: 'notanint' for column 'n' at row 1` |
| MySQL 8.0.46 | 1292 | `Incorrect datetime value: 'secret-date' for column 'd' at row 1` |
| PG 16（pgx v5.10.0） | 22P02 | `ERROR: invalid input syntax for type integer: "notanint" (SQLSTATE 22P02)` |
| PG 16 | 23505 | `Error()` 里没有值，但 `PgError.Detail` 是 `Key (email)=(a@b.com) already exists.` |
| PG 16 | 23514 | `Detail` 是 `Failing row contains (3, chk@y, 500, null, null).` |

所以日志的 `error` 字段和 Span 的状态描述只写 `mysql error 1062 (message omitted, it may contain parameter values)`，
错误码另记（字段见[「链路」](#链路)），不调 `RecordError`。

**客户端一侧的错也带参数值**，所以按白名单放行，和 xredis 同一个做法：

| 来源 | 原文 |
|---|---|
| pgx v5.10.0 编码不了的参数（`%#v`） | `failed to encode args[0]: unable to encode xgorm.rvSecret{Token:"hunter2"} into text format for text (OID 25): cannot find encode plan` |
| `database/sql` 扫描失败（`%q`） | `converting driver.Value type string ("abc-secret") to a int: invalid syntax` |

照原文记的只有：网络错误（`net.Error`，地址、超时，排查正要看这个）、ctx 取消 / 超时、`io.EOF`，
以及 `database/sql`、go-sql-driver、GORM 自己那些文案固定的哨兵错误（`sql.ErrConnDone`、`driver.ErrBadConn`、
`mysql.ErrInvalidConn`、`gorm.ErrRecordNotFound`……）。记的是错误链上认出来的**那一个**，不是整条链的原文：
GORM 的 `AddError` 用 `"%v; %w"` 把前一个错误的原文拼在前面，只看链上有没有安全的错误的话，前一个带着的值照样出去。
其余一律写 `client error (message omitted, it may contain parameter values)`，Span 上不调 `RecordError`
（实测 PG 16 上 `Exec("SELECT ?::text", rvSecret{...})`：不这么做时 `SQL failed` 的 `error` 字段、Span 的状态描述和
`exception.message` 里都是上面那句带着 `hunter2` 的原文）。

两种情况下返回给调用方的错误都原样不变。

**时长不收负数**：底下每一处都会把负数静默变成「不限」——go-sql-driver v1.10.1 的 `FormatDSN` 只写 > 0 的
`timeout` / `readTimeout` / `writeTimeout`；`database/sql` 把负的存活时间当成 0；PG 的 `connect_timeout` 和几个 GUC
在注入时同样被跳过。

### MySQL

go-sql-driver v1.10.1、`gorm.io/driver/mysql` v1.6.0、MySQL 8.0.46。

**`parseTime` 默认补成 `true`。** 驱动默认 `false`：`DATETIME` / `TIMESTAMP` 读出来是 `[]byte`，
带 `CreatedAt` 的模型 `First` 一次就报 `unsupported Scan, storing driver.Value type []uint8 into type *time.Time`。
DSN 里写了的（哪怕是 `parseTime=false`）以 DSN 为准；判断照抄驱动的解析规则（最后一个 `/`、第一个 `?` 之后），
密码里的 `?parseTime=false` 不算数。

打开之后时区跟着驱动的 `loc`（默认 `UTC`）：写入前转成 `loc`、读出来按 `loc` 解释，同一个时刻来回不变。
例如写 `2024-01-02 03:04:05 +01:00`，库里存 `2024-01-02 02:04:05`，读回来是 `02:04:05 UTC`，`Equal` 为 true。

- 库里的墙上时间要给别的系统按本地时间读：DSN 里写 `loc=Local`（或 `loc=Europe%2FBerlin`）。
- 把 `DATETIME` 扫进 `string` 拿到的是 RFC 3339（`2024-01-02T02:04:05Z`）；要原样文本就在 DSN 里写 `parseTime=false`。

**`Initialize` 里的版本查询。** Dialector 初始化时查一次 `SELECT VERSION()`，写死了 `context.Background()`，
而且它就是第一次建连，发生在 `gorm.Open` 里、建连重试之前。原样用的话实测（对端收下连接却不回话）：
只试 1 次、3.0s 后失败；启动期间的 SIGTERM 要等握手那一读撞上 `MySQL.ReadTimeout`，信号之后 2.95s 进程才退。
这里关掉（`SkipInitializeWithVersion`），改在建连探测里、每次 Ping 成功之后查。同一场景实测：试满 3 次、
9.6–11.1s 后失败（预算 13.5s）；SIGTERM 之后 7–20ms 退出。版本号照样设进 Dialector，驱动靠它决定的行为
（MariaDB / MySQL 5.x 的改索引名、改列名、`FOR SHARE`、`DROP CONSTRAINT`，MariaDB 10.5+ 的 `RETURNING`）不变。

**单次探测预算**是 `DialTimeout + MySQL.ReadTimeout`（按 DSN 里最终生效的 `timeout` / `readTimeout` 算）。
驱动拿 `timeout` 管拨号、`readTimeout` 管之后的每一读（握手、认证、`SELECT VERSION()`）。其中一个是 0（驱动不限时）的，
按另一个算两份：`ReadTimeout: 0`、`DialTimeout: 500ms` 的预算是 1s，不是只剩拨号的 500ms；两个都是 0 用 1s 兜底。

**开了 TLS 块就不会退回明文。** go-sql-driver v1.10.1 的 `allowFallbackToPlaintext=true`（`tls=preferred` 也会打开它）
在服务端的问候包里没有 `CLIENT_SSL` 时丢掉 TLS、改发明文的登录包（`packets.go` `readHandshakePacket`）。
这一位是明文传过来的，中间人清掉它就够了。实测（一个问候包不带 `CLIENT_SSL` 的假服务端）：TLS 块加上
DSN 里的 `allowFallbackToPlaintext=true`，登录包明文发了出去。所以开着 TLS 块时 DSN 里写了这个参数（写成什么值都算）
直接启动失败，交给驱动的连接配置里也钉死成 false，服务端不肯 TLS 时报 `TLS requested but server does not support TLS`。

**取消与截止时间。** 客户端有读超时兜底：对端不回话、调用方又没给截止时间时，查询在 `ReadTimeout`
失败（实测 3.0s，`invalid connection`）。调用方的 ctx 管得更细：

- 给了截止时间就在那一刻返回——池里卡在读上的连接和主机宕机时卡在拨号上的新连接都一样（给 200ms 就是 200ms）；
- ctx 被取消时 go-sql-driver 当场关掉那条连接，查询返回 `context canceled`：客户端放弃之后 0.3ms handler 就返回了。
  被取消的那条连接不还回池里。

新建连接的拨号受 `DialTimeout`（DSN 的 `timeout`）管：主机宕机时 500ms 失败；DSN 里写了 `timeout=1500ms` 的以 DSN 为准。

**`interpolateParams` 默认 `false`，不改**：带参数的查询是 PREPARE、EXECUTE、CLOSE 三次写、两个往返（实测一次 `First` 3 次写）。

**go-sql-driver 的日志**默认 `log.New(os.Stderr, "[mysql] ", …)` 写纯文本
（`[mysql] 2026/09/24 10:00:00 packets.go:58 read tcp …: i/o timeout`）。xgorm 的 `init` 把它接到 slog：
一条 `xgorm go-sql-driver log`，级别 WARN，原文在 `detail`。驱动不给 ctx，这些日志不带 trace_id。
实测对端不回话、8 条查询读超时，就是 8 条这样的日志，stderr 里一行 `[mysql]` 都没有。
想换成自己的就在 `main` 里、框架启动之前调 `mysql.SetLogger`：驱动在解析 DSN 时把当时的 logger 抄进连接配置，
而 xgorm 在启动钩子里才解析 DSN。

### PostgreSQL

pgx v5.10.0、`gorm.io/driver/postgres` v1.6.3、PG 16。

**单次探测预算**是「注入的 `connect_timeout`（`DialTimeout` 向上取整到整秒）+ `DialTimeout`」，默认 1.5s。
pgx 拿 `connect_timeout` 管每个主机的整个建连（TLS 握手、startup、认证都在里面；pgconn 源码注释原话
"restricts the whole connection process"；实测 startup 不回话的服务端，`connect_timeout=1` 等满 1.0s）。
预算比它短的话，一次慢一点但合法的握手会在 pgx 放弃之前被判超时。

**没有读超时。** 连接池里的连接遇上不回话的对端，调用方的 ctx 又没有截止时间，查询就一直等。
`StatementTimeout` 救不了这一段——它是服务端的计时：实测对端吞掉全部字节、`StatementTimeout: 1s`，
一条 `SELECT 1` 30s 后仍没返回。能管住它的只有调用方的截止时间（实测给 200ms 就在 200ms 返回）：

```go
ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
defer cancel()
xgorm.CWithCtx(ctx).First(&u, id)
```

Web 请求里用 `c.Request.Context()` 也行；后台任务、定时任务**一定要自己给截止时间**。

**经 PgBouncer：`default_query_exec_mode`。** pgx 默认 `cache_statement`：每条语句第一次执行时建一个具名预备语句。
PgBouncer 的事务池会把下一条语句派到另一个服务端连接上，具名语句就对不上了。实测 PgBouncer 1.22.0
`pool_mode = transaction`、`default_pool_size = 2`，20 个协程各跑 50 条带参数的查询：

| `default_query_exec_mode` | `max_prepared_statements = 0` | `= 100` | 直连 PG 每条耗时 | 每条写几次 |
|---|---|---|---|---|
| `cache_statement`（pgx 默认） | **701 条失败**：`prepared statement "stmtcache_…" already exists (SQLSTATE 42P05)` | 0 | 59–82µs | 1 |
| `cache_describe` | 0 | 0 | 61–79µs | 1 |
| `describe_exec` | 6 条失败：`unnamed prepared statement does not exist (SQLSTATE 26000)` | 16 条失败 | 68–105µs | 2 |
| `exec` | 0 | 0 | 97–104µs | 1 |
| `simple_protocol` | 0 | 0 | 89–102µs | 1 |

默认值不改：直连 PG 的是大多数。经 PgBouncer 的，二选一：PgBouncer 升到 1.21+ 并把 `max_prepared_statements`
配成非 0（1.24 起默认 200），或者在 DSN 里写 `default_query_exec_mode=exec`（写进 `Postgres.Params` 也生效）。

**DSN 的几个细节**（pgx v5.10.0）：

- key=value 形式里默认值垫在 DSN 前面，pgx 同一个 key 取最后一次，所以 DSN 里写了的自然作数。
- 时区是例外：gorm 的 postgres 驱动另用正则取 DSN 里第一处 `timezone=` / `TimeZone=` / `time_zone=`，
  而且不解码——`TimeZone=Europe/Berlin` 被编码成 `Europe%2FBerlin` 的话每条连接都设不上时区。
  所以 URL 形式里补的参数接在原 query 后面，使用者写的部分原样保留，补进去的值里的 `/` 也不编码。
- 预检用 `pgx.ParseConfig`（比 `pgconn.ParseConfig` 多校验 `default_query_exec_mode` 等三项）。
  pgx 的错误原文是整串 DSN、只遮得住 `password=x` 这种规整写法（`password = hunter2` 原样带出），所以不回传它。
- 多主机 URL 里 IPv6 地址不能排在第一个：`postgres://u:p@[::1]:1,h2:1/db` 会被 `url.Parse` 和 pgx 同时拒绝。
  挪到后面，或者改用 key=value 形式 `host=::1,h2 port=1,1`。

## 排错

### `unknown Driver="clickhouse", registered: [mysql postgres]`

`Driver` 写错了，或者没 import 驱动的 module（`_ "github.com/jupiter-eng/xone/xgorm/clickhouse"`）。
驱动的用法见 [xgorm/clickhouse](clickhouse/README.md)。

### `the DSN sets allowFallbackToPlaintext while the TLS block is enabled`

开了 `TLS:` 块，MySQL 的 DSN 里又写了 `allowFallbackToPlaintext=`。TLS 块从不退回明文，把这个参数从 DSN 里删掉；
真要「服务端不支持就明文」的，别开 TLS 块、在 DSN 里写 `tls=preferred`，并且清楚它挡不住中间人。

TLS 相关的报错见 [xtls「排错」](../xtls/README.md#排错)；建连失败见 [`docs/troubleshooting.md`「建连」](../docs/troubleshooting.md#建连)。
