# xgorm

The database client: what you get is a native `*gorm.DB`, built by the framework from the configuration and closed on shutdown.

- MySQL / PostgreSQL built in; the ClickHouse driver lives in [`xgorm/clickhouse`](clickhouse/README.md)
- One instance or several; `xgorm.C("name")` fetches one by name
- One Span per SQL statement, with attributes following OTel's database semantic conventions v1.43.0
- Connection-pool metrics `db_pool_*`, switchable per instance
- SQL logging is off by default; logs and Spans carry only the SQL with its placeholders, never parameter values
- One probe at startup: if the database cannot be reached, startup fails

## Quick start

```yaml
# conf/application.yml
XGorm:
  DSN: "${DB_DSN}"   # postgres://app:secret@db:5432/app, Driver defaults to postgres
```

```go
import (
	"gorm.io/gorm"

	"github.com/jupiter-eng/xone/xgorm"
)

// The ctx is required: deadlines and traces only travel with it. In a web request pass c.Request.Context()
var a Account
err := xgorm.CWithCtx(ctx).First(&a, id).Error // no row found is gorm.ErrRecordNotFound

// A transaction: the callback returning an error rolls back, returning nil commits
err = xgorm.CWithCtx(ctx).Transaction(func(tx *gorm.DB) error {
	return tx.Create(&order).Error
})
```

`xgorm.CWithCtx(ctx, "report")` is shorthand for `xgorm.C("report").WithContext(ctx)`.

## Configuration

`mysql` and `postgres` are built in; for ClickHouse see [the other driver](clickhouse/README.md#configuration).

```yaml
XGorm:
  Driver: postgres         # mysql / postgres, defaults to postgres
  DSN: "${DB_DSN}"         # required
  DialTimeout: 500ms       # connection timeout, injected into the DSN (MySQL's timeout, PG's connect_timeout); 0 = not injected
  MaxOpenConns: 50         # must be > 0
  MaxIdleConns: 50         # 0 = keep no idle connection at all
  MaxLifetime: 5m          # 0 = no limit
  MaxIdleTime: 5m          # 0 = no limit
  Log: false               # route SQL into slog, off by default; only placeholders are recorded, never parameter values
  SlowThreshold: 3s        # anything slower is logged at warn, requires Log; 0 = not logged
  IgnoreNotFound: false    # true: "no row found" produces no SQL failed log (while Log is on); the returned error is unchanged
  DisableForeignKeyConstraintWhenMigrating: false  # true: AutoMigrate creates no foreign-key constraints; queries and Preload are unaffected
  SkipDefaultTransaction: false  # true: a single Create / Update / Delete is not wrapped in a transaction
  PrepareStmt: false       # true: cache prepared statements; measure first when going through a pooler such as PgBouncer
  CreateBatchSize: 0       # maximum rows per INSERT when creating from a slice, 0 = the whole slice in one
  Trace: true              # one Span per SQL statement
  Metric: true             # connection-pool metrics db_pool_*, per instance
  MySQL:                   # only with Driver: mysql
    ReadTimeout: 3s        # upper bound on waiting for one reply, 0 = not injected (no limit in the driver)
    WriteTimeout: 5s
  Postgres:                # only with Driver: postgres; sent with the connection and applied as session parameters
    StatementTimeout: 0s   # timed by the server, unlimited by default; cannot help when the far end never answers
    LockTimeout: 0s
    IdleInTxTimeout: 0s
    Params: {}             # any PG runtime parameter or pgx connection parameter; on a name clash this wins
  TLS:                     # rules in xtls; an empty ServerName = the host name from the DSN
    Enable: false
    CAFile: ""
    CertFile: ""
    KeyFile: ""
    ServerName: ""
```

**Several instances** go under `Clients`, each with the same fields as above; anything left out uses the defaults above:

```yaml
XGorm:
  Clients:
    default: {DSN: "${DB_DSN}"}                                     # PostgreSQL
    report:  {DSN: "${REPORT_DSN}", Driver: mysql, MaxOpenConns: 5} # report:secret@tcp(report-db:3306)/report
```

`xgorm.C()` fetches `default` and `xgorm.C("report")` the other one. The two forms cannot be mixed.

- **What the DSN sets wins**; the timeouts in the configuration are only defaults. Durations, `MaxIdleConns` and `CreateBatchSize` must not be negative.
- The budget for one connection probe is computed from the values that end up in effect in the DSN (reasons in ["Behavior (measured)"](#behavior-measured)):

  | Driver | Budget | With the default configuration |
  |---|---|---|
  | MySQL | `timeout + readTimeout`; when one is 0, twice the other; when both are 0, 1s | 3.5s |
  | PostgreSQL | `connect_timeout + DialTimeout` | 1.5s |
| ClickHouse | `2 x dial_timeout`; with neither set, the driver's own 30s | 1s |
  | Other drivers | Worked out by the dialect itself; when it cannot, `2 x DialTimeout` | — |

## API

| Function | Description |
|---|---|
| `C(name ...string) *gorm.DB` | Fetch an instance; without an argument, `default`. If there is none it panics, and the message says plainly whether it was called too early, never configured, or the name is misspelled |
| `CWithCtx(ctx, name ...string) *gorm.DB` | Fetch an instance bound to a ctx, that is `C(name...).WithContext(ctx)`; GORM has no other way to pass a ctx |
| `Has(name ...string) bool` | Whether an instance is configured, for checking an optional dependency first |
| `Names() []string` | Which instances are configured |
| `New(ctx, cfg) (*gorm.DB, io.Closer, error)` | The pure constructor: touches no globals, reads no config file, usable outside the framework; the `io.Closer` closes the underlying pool |
| `RegisterDialect(d Dialect)` | Register another driver; registering the same name twice panics. A TLS block is only accepted when the `Dialect` provides `OpenTLS` |
| `Drivers() []Driver` | The registered driver names, in alphabetical order |

## Notes

- **Always pass the ctx, and always give background jobs a deadline.** PostgreSQL has no read timeout: when the far end
  never answers, only the ctx deadline can stop the query (`StatementTimeout` is timed by the server and cannot help here).
  See ["PostgreSQL"](#postgresql).
- **When a MySQL DSN does not set `parseTime`, `parseTime=true` is added**, so `DATETIME` scans into `time.Time` with the
  time zone following the driver's `loc` (UTC by default). See ["MySQL"](#mysql).
- **SQL logging is off by default** (`Log: false` really prints nothing; it is not GORM's colored stdout default). Logs and
  Spans carry only the SQL with its placeholders; a server error is recorded as its error code alone, and a client error is
  recorded verbatim only for the few kinds known to carry no parameter values. The error returned to the caller is unchanged,
  and `errors.As` still reaches `*mysql.MySQLError` / `*pgconn.PgError`. See ["General"](#general).
- **TLS goes in the `TLS:` block**, certificates are always verified and there is no fallback to plaintext. While it is on, the
  DSN must not set TLS parameters of its own (PG's `sslmode`, `sslrootcert` and the other `ssl*`; MySQL's `tls=` and
  `allowFallbackToPlaintext=`), and a Unix socket cannot be used. While it is off, pgx's default `sslmode=prefer` connects
  **without verifying the certificate**. See [xtls](../xtls/README.md).
- **Behind PgBouncer (transaction pooling)** `default_query_exec_mode` has to be changed, otherwise concurrent traffic
  produces batches of `prepared statement ... already exists`. See ["PostgreSQL"](#postgresql).
- **The startup probe**: one per instance, at most 3 attempts; a wrong password is not retried and is reported as
  `authentication to <addr> failed`. See
  [behavior.md "The startup connection probe"](../docs/behavior.md#the-startup-connection-probe).

## Observability

### Logs

| Message | Level | Fields |
|---|---|---|
| `xgorm connected` | INFO | `name`, `driver`, `addr`, `db`, `tls`, `max_open_conns`, `max_idle_conns` |
| `xgorm ready` | INFO | `instances` |
| `SQL` / `slow SQL` / `SQL failed` | INFO / WARN / ERROR | `sql` (with placeholders), `elapsed_ms`, `rows_affected` (omitted when the row count is unknown); on failure `error` and `error_code`; for a slow query `threshold_ms` (requires `XGorm.Log: true`) |
| `xgorm go-sql-driver log` | WARN | `detail`: go-sql-driver's own log text, without a trace_id (see ["MySQL"](#mysql)) |

The global logging conventions (`trace_id` injection, `xlog.AddKV`, the framework's start and stop logs) are in
[`docs/observability.md`](../docs/observability.md#logs).

### Metrics

| Metric | Type | Labels | Source |
|---|---|---|---|
| `db_pool_open` / `db_pool_in_use` / `db_pool_idle` / `db_pool_max_open` | gauge | `name` (the instance name) | xgorm, `XGorm.Metric`, per instance |
| `db_pool_wait_total` / `db_pool_wait_duration_seconds_total` / `db_pool_closed_max_idle_total` / `db_pool_closed_max_lifetime_total` | counter | `name` | xgorm |

The metric name prefix, the constant labels and a few general rules are in
[`docs/observability.md` "Metrics"](../docs/observability.md#metrics).

### Tracing

| Source | Span name | Key attributes |
|---|---|---|
| xgorm | `gorm.create` / `gorm.query` / `gorm.update` / `gorm.delete` / `gorm.row` / `gorm.raw` | below |

The attributes follow OTel's database semantic conventions v1.43.0 (the Tracer carries that version's schema URL):

| Attribute | Value |
|---|---|
| `db.system.name` | `postgresql` / `mysql` / `clickhouse` (other drivers use the driver name) |
| `db.namespace` | The database name |
| `server.address` / `server.port` | The host and port resolved from the DSN (with several hosts, the first one) |
| `db.query.text` | The SQL with its placeholders, without parameter values |
| `db.operation.name` | The first keyword of the statement as sent, in its original case: `SELECT`, `INSERT`, ... (a soft delete sends `UPDATE`) |
| `db.rows_affected` | The number of rows affected (always 0 on ClickHouse) |
| `db.response.status_code` / `error.type` | The error code when the server reports one (the MySQL error number or PG's SQLSTATE); for other errors `error.type` is `_OTHER` |

`gorm.ErrRecordNotFound` does not mark the Span as an error. The whole tracing picture, propagation and the trust boundary are
in [`docs/observability.md` "Tracing"](../docs/observability.md#tracing).

## Behavior (measured)

The measurement environment and the cross-module summary table are in [`docs/behavior.md`](../docs/behavior.md).

### General

GORM v1.31.2.

**`gorm.Open` pings once on its own**, using its own context, which neither the stop signal nor the retries can reach. It is
turned off here (`DisableAutomaticPing`) in favor of the framework's connection probe.

**Not supplying a Logger does not mean nothing is logged**: GORM fills in its own default implementation, which writes to
`os.Stdout` with ANSI colors. With `Log: false` that is replaced by a Logger that writes nothing.

**`AutoMigrate` creates foreign keys for associations by default.** Measured on PostgreSQL 16, `Owner` has many `Order`
creates `fk_owners_orders` across the two tables, and inserting an `order` whose `owner_id` does not exist is rejected. With
`DisableForeignKeyConstraintWhenMigrating: true` no foreign key is created, that row is inserted, and `Preload("Orders")`
still returns it. This only affects the moment of table creation; foreign keys already created are not dropped.

**Three performance-related switches** (local loopback, orders of magnitude for reference, not a benchmark):

- `SkipDefaultTransaction`: 500 single-row Creates, about 500µs → 230µs each on PostgreSQL 16 and about 1.0ms → 0.9ms on
  MySQL 8.0 (saving the `BEGIN` / `COMMIT` round trips). It is off by default, because the saved round trips are paid for in
  correctness: when an `AfterCreate` hook returns an error the default transaction rolls the insert back (0 rows), while with
  it skipped that row stays. Skip it only on hot paths, with
  `xgorm.C().Session(&gorm.Session{SkipDefaultTransaction: true})`.
- `PrepareStmt`: 2000 runs of the same parameterized query, about 180µs → 94µs on MySQL (without it the driver does
  prepare / execute / close, three round trips per statement) and about 100µs → 85µs on PostgreSQL (pgx caches statements
  anyway). The cache size and expiry use GORM's defaults (unlimited, 1h).
- `CreateBatchSize: 3` inserting 10 rows: both databases receive 4 INSERTs; the default of 0 sends 1.

**GORM's other defaults are kept as they are** (measured):

- `NowFunc`: `time.Now().Local()`. On PG, GORM creates `timestamptz` columns for `time.Time`, which store an instant and are
  unaffected; on MySQL see `parseTime`.
- `TranslateError: false`: turning it on turns MySQL's 1062 into `gorm.ErrDuplicatedKey`, but the original
  `*mysql.MySQLError` is replaced wholesale and code that branches on the error number silently stops working.

**Parameter values in SQL logs.** When GORM's Logger does not implement `ParamsFilter`, the real parameters are interpolated
into the SQL. The xgorm Logger does implement it, and what is recorded is the SQL with its placeholders (`?` on MySQL, `$1` on
PostgreSQL; GORM's PG dialect writes `$1$` when it has no parameters to substitute, which is undone here).

**`Scan` takes another path.** During `Raw(...).Scan(...)` and `Table(...).Select(...).Scan(...)`, GORM replaces the
instance's Logger with `logger.Recorder` (`finisher_api.go:539`), which does not consult `ParamsFilter` and only honors the
process-wide `logger.RecorderParamsFilter`, whose default returns the parameters as they are. Measured, `WHERE name = ?` was
recorded as `WHERE name = 'sql-3fa9c1d2b7e4'` on all three databases. So xgorm's `init` replaces that variable with the same
filter that hands out no parameters. It is process-wide: instances the caller opened with `gorm.Open` are covered too, and
changing it means assigning it again in `main`.

**A server's error text carries parameter values**, which would defeat recording only placeholders in the SQL:

| Server | Error | Text |
|---|---|---|
| MySQL 8.0.46 | 1062 | `Error 1062 (23000): Duplicate entry 'a@b.com' for key 'm_err.email'` |
| MySQL 8.0.46 | 1366 | `Incorrect integer value: 'notanint' for column 'n' at row 1` |
| MySQL 8.0.46 | 1292 | `Incorrect datetime value: 'secret-date' for column 'd' at row 1` |
| PG 16 (pgx v5.10.0) | 22P02 | `ERROR: invalid input syntax for type integer: "notanint" (SQLSTATE 22P02)` |
| PG 16 | 23505 | `Error()` carries no value, but `PgError.Detail` is `Key (email)=(a@b.com) already exists.` |
| PG 16 | 23514 | `Detail` is `Failing row contains (3, chk@y, 500, null, null).` |

So the log's `error` field and the Span's status description only say
`mysql error 1062 (message omitted, it may contain parameter values)`, the error code is recorded separately (the fields are
in ["Tracing"](#tracing)), and `RecordError` is not called.

**Client-side errors carry parameter values too**, so they pass an allowlist, the same approach as in xredis:

| Source | Text |
|---|---|
| A parameter pgx v5.10.0 cannot encode (`%#v`) | `failed to encode args[0]: unable to encode xgorm.rvSecret{Token:"hunter2"} into text format for text (OID 25): cannot find encode plan` |
| A failed `database/sql` scan (`%q`) | `converting driver.Value type string ("abc-secret") to a int: invalid syntax` |

What is recorded verbatim is only: network errors (`net.Error`, with the address and the timeout, which is exactly what an
investigation needs), ctx cancellation and deadlines, `io.EOF`, and the sentinel errors with fixed text from `database/sql`,
go-sql-driver and GORM itself (`sql.ErrConnDone`, `driver.ErrBadConn`, `mysql.ErrInvalidConn`, `gorm.ErrRecordNotFound`, ...).
What is recorded is **the one** error recognized on the chain, not the text of the whole chain: GORM's `AddError` uses
`"%v; %w"` to prepend the previous error's text, so merely checking whether the chain contains a safe error would let the
values carried by the previous one out anyway. Everything else is written as
`client error (message omitted, it may contain parameter values)` and `RecordError` is not called on the Span (measured on
PG 16 with `Exec("SELECT ?::text", rvSecret{...})`: without this, the `error` field of `SQL failed`, the Span's status
description and `exception.message` all held the text above, `hunter2` included).

In both cases the error returned to the caller is unchanged.

**Durations do not accept negative values**: every layer below would silently turn a negative into "no limit" --
go-sql-driver v1.10.1's `FormatDSN` only writes `timeout` / `readTimeout` / `writeTimeout` when they are > 0; `database/sql`
treats a negative lifetime as 0; and PG's `connect_timeout` and the GUCs are likewise skipped during injection.

### MySQL

go-sql-driver v1.10.1, `gorm.io/driver/mysql` v1.6.0, MySQL 8.0.46.

**`parseTime` defaults to `true` here.** The driver defaults to `false`: `DATETIME` / `TIMESTAMP` come back as `[]byte`, and a
model with `CreatedAt` fails on the first `First` with
`unsupported Scan, storing driver.Value type []uint8 into type *time.Time`. What the DSN says wins (even
`parseTime=false`), and the check follows the driver's own parsing rules (after the last `/` and the first `?`), so a
`?parseTime=false` inside the password does not count.

Once it is on, the time zone follows the driver's `loc` (`UTC` by default): values are converted to `loc` before writing and
interpreted in `loc` when read, so the same instant survives the round trip. Writing `2024-01-02 03:04:05 +01:00`, for
example, stores `2024-01-02 02:04:05` and reads back as `02:04:05 UTC`, with `Equal` true.

- To have another system read the stored wall-clock time as local time: set `loc=Local` (or `loc=Europe%2FBerlin`) in the DSN.
- Scanning a `DATETIME` into a `string` yields RFC 3339 (`2024-01-02T02:04:05Z`); for the raw text, set `parseTime=false` in
  the DSN.

**The version query in `Initialize`.** When the Dialector initializes it runs one `SELECT VERSION()` with a hard-coded
`context.Background()`, and that is also the first connection, made inside `gorm.Open` before the connection retries. Left as
it is, measured against a far end that accepts the connection and never answers: 1 attempt only, failing after 3.0s, and a
SIGTERM during startup has to wait for that handshake read to hit `MySQL.ReadTimeout`, so the process exits 2.95s after the
signal. It is turned off here (`SkipInitializeWithVersion`) and run inside the connection probe instead, after each successful
Ping. Measured in the same scenario: all 3 attempts, failing after 9.6-11.1s (with a budget of 13.5s), and exiting 7-20ms
after SIGTERM. The version is still set on the Dialector, so the behavior the driver derives from it is unchanged (renaming
indexes and columns, `FOR SHARE` and `DROP CONSTRAINT` on MariaDB / MySQL 5.x, and `RETURNING` on MariaDB 10.5+).

**The budget for one probe** is `DialTimeout + MySQL.ReadTimeout` (computed from the `timeout` / `readTimeout` that end up in
effect in the DSN). The driver uses `timeout` for dialing and `readTimeout` for every read after it (handshake,
authentication, `SELECT VERSION()`). When one of them is 0 (no limit in the driver), twice the other is used: with
`ReadTimeout: 0` and `DialTimeout: 500ms` the budget is 1s, not the 500ms of dialing alone; when both are 0, 1s is the
fallback.

**With a TLS block there is no fallback to plaintext.** go-sql-driver v1.10.1's `allowFallbackToPlaintext=true` (which
`tls=preferred` also turns on) drops TLS and sends the login packet in plaintext when the server's greeting carries no
`CLIENT_SSL` (`packets.go` `readHandshakePacket`). That bit arrives in plaintext, so clearing it is all a man in the middle
has to do. Measured against a fake server whose greeting omits `CLIENT_SSL`: with a TLS block plus
`allowFallbackToPlaintext=true` in the DSN, the login packet went out in plaintext. So while a TLS block is on, setting that
parameter in the DSN (to any value at all) fails startup, the connection config handed to the driver pins it to false as
well, and a server unwilling to do TLS is reported as `TLS requested but server does not support TLS`.

**Cancellation and deadlines.** The client has a read timeout as a backstop: when the far end never answers and the caller
gave no deadline, the query fails at `ReadTimeout` (measured 3.0s, `invalid connection`). The caller's ctx is finer grained:

- with a deadline it returns at that moment -- both for a pooled connection stuck on a read and for a new connection stuck on
  dialing because the host is down (give it 200ms and it is 200ms);
- when the ctx is cancelled, go-sql-driver closes that connection on the spot and the query returns `context canceled`: the
  handler returned 0.3ms after the caller gave up. The cancelled connection is not returned to the pool.

Dialing a new connection is governed by `DialTimeout` (the DSN's `timeout`): with the host down it fails after 500ms, and a
`timeout=1500ms` in the DSN wins.

**`interpolateParams` stays at its default `false`**: a parameterized query is prepare, execute and close -- three writes and
two round trips (measured, one `First` makes 3 writes).

**go-sql-driver's log** defaults to `log.New(os.Stderr, "[mysql] ", ...)` writing plain text
(`[mysql] 2026/09/24 10:00:00 packets.go:58 read tcp ...: i/o timeout`). xgorm's `init` routes it into slog: one
`xgorm go-sql-driver log` record at WARN, with the original text in `detail`. The driver passes no ctx, so these records carry
no trace_id. Measured with a far end that never answers and 8 queries hitting the read timeout: 8 such records, and not a
single `[mysql]` line on stderr. To use your own instead, call `mysql.SetLogger` in `main` before the framework starts: the
driver copies whichever logger is current into the connection config while parsing the DSN, and xgorm parses the DSN in a
start hook.

### PostgreSQL

pgx v5.10.0, `gorm.io/driver/postgres` v1.6.3, PG 16.

**The budget for one probe** is the injected `connect_timeout` (`DialTimeout` rounded up to whole seconds) plus
`DialTimeout`, which is 1.5s by default. pgx uses `connect_timeout` for the whole connection to each host (TLS handshake,
startup and authentication are all inside it; the pgconn source comment says it "restricts the whole connection process", and
measured against a server that never answers the startup message, `connect_timeout=1` waited the full 1.0s). A shorter budget
would time out a slow but legal handshake before pgx gives up.

**There is no read timeout.** When a pooled connection meets a far end that never answers and the caller's ctx has no
deadline, the query waits forever. `StatementTimeout` cannot help here, because it is timed by the server: measured with a far
end that swallows every byte and `StatementTimeout: 1s`, a `SELECT 1` had still not returned after 30s. Only the caller's
deadline governs this (measured, 200ms returns in 200ms):

```go
ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
defer cancel()
xgorm.CWithCtx(ctx).First(&u, id)
```

In a web request `c.Request.Context()` does the job; background and scheduled jobs **must supply a deadline of their own**.

**Behind PgBouncer: `default_query_exec_mode`.** pgx defaults to `cache_statement`: the first execution of each statement
creates a named prepared statement. PgBouncer's transaction pooling sends the next statement to a different server
connection, where that named statement no longer exists. Measured with PgBouncer 1.22.0, `pool_mode = transaction`,
`default_pool_size = 2`, and 20 goroutines each running 50 parameterized queries:

| `default_query_exec_mode` | `max_prepared_statements = 0` | `= 100` | Per statement against PG directly | Writes per statement |
|---|---|---|---|---|
| `cache_statement` (pgx's default) | **701 failures**: `prepared statement "stmtcache_..." already exists (SQLSTATE 42P05)` | 0 | 59-82µs | 1 |
| `cache_describe` | 0 | 0 | 61-79µs | 1 |
| `describe_exec` | 6 failures: `unnamed prepared statement does not exist (SQLSTATE 26000)` | 16 failures | 68-105µs | 2 |
| `exec` | 0 | 0 | 97-104µs | 1 |
| `simple_protocol` | 0 | 0 | 89-102µs | 1 |

The default is not changed: connecting to PG directly is the common case. Behind PgBouncer there are two options: upgrade
PgBouncer to 1.21+ and set `max_prepared_statements` to a non-zero value (the default is 200 from 1.24 on), or set
`default_query_exec_mode=exec` in the DSN (writing it into `Postgres.Params` works too).

**A few DSN details** (pgx v5.10.0):

- In key=value form the defaults are placed before the DSN, and pgx takes the last occurrence of a key, so what the DSN sets
  naturally wins.
- The time zone is the exception: gorm's postgres driver separately picks the first `timezone=` / `TimeZone=` / `time_zone=`
  in the DSN with a regexp, and does not decode it -- a `TimeZone=Europe/Berlin` encoded as `Europe%2FBerlin` would leave
  every connection without its time zone. So in URL form the added parameters are appended after the original query, what the
  caller wrote is kept as it is, and a `/` inside an added value is not encoded either.
- The pre-check uses `pgx.ParseConfig` (which validates three more items than `pgconn.ParseConfig`, among them
  `default_query_exec_mode`). pgx's error text is the whole DSN and masks only the tidy spelling `password=x`
  (`password = hunter2` is carried out as it is), so it is not passed back.
- In a multi-host URL an IPv6 address must not come first: `postgres://u:p@[::1]:1,h2:1/db` is rejected by both `url.Parse`
  and pgx. Move it later, or use the key=value form `host=::1,h2 port=1,1`.

## Troubleshooting

### `unknown Driver="clickhouse", registered: [mysql postgres]`

Either `Driver` is misspelled, or the driver's module was not imported
(`_ "github.com/jupiter-eng/xone/xgorm/clickhouse"`). How to use the driver is in
[xgorm/clickhouse](clickhouse/README.md).

### `the DSN sets allowFallbackToPlaintext while the TLS block is enabled`

The `TLS:` block is on and the MySQL DSN also sets `allowFallbackToPlaintext=`. A TLS block never falls back to plaintext, so
remove that parameter from the DSN. If "plaintext when the server does not support TLS" is really what is wanted, leave the
TLS block off, set `tls=preferred` in the DSN, and be clear that it does not stop a man in the middle.

TLS errors are in [xtls "排错"](../xtls/README.md#排错); connection failures are in
[`docs/troubleshooting.md` "Connecting"](../docs/troubleshooting.md#connecting).
