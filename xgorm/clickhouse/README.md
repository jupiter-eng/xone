# xgorm/clickhouse

The ClickHouse driver for [xgorm](../README.md): one blank import line and `Driver: clickhouse` in the configuration, and
what comes back is still a native `*gorm.DB`.

- Configuration fields, several instances, logs, metrics and Spans are all the same as in xgorm
- A module of its own: the ClickHouse driver pulls in more than 60 extra modules, which applications that do not use it
  should not have to carry (see [architecture.md](../../docs/architecture.md#2-every-integration-is-its-own-go-module))
- The DSN accepts `clickhouse://` / `tcp://` (the native protocol) and `http://` / `https://`
- The TLS block applies to both native and `https://`
- A parse error never echoes the DSN (the original error carries the plaintext password)

## Quick start

```yaml
# conf/application.yml
XGorm:
  Clients:
    default: {DSN: "${DB_DSN}"}                     # PostgreSQL
    events:  {Driver: clickhouse, DSN: "${CH_DSN}"} # clickhouse://user:pass@ch:9000/analytics
```

```go
import (
	"github.com/jupiter-eng/xone/xgorm"
	_ "github.com/jupiter-eng/xone/xgorm/clickhouse" // registers the driver; nothing in it is called directly
)

ctx, cancel := context.WithTimeout(ctx, 10*time.Second) // the deadline must be shorter than read_timeout, see "Notes"
defer cancel()

var out []PageViews
err := xgorm.CWithCtx(ctx, "events").
	Raw("SELECT path, count() AS views FROM page_views WHERE day = today() GROUP BY path ORDER BY views DESC LIMIT 10").
	Scan(&out).Error
```

## Configuration

Only the ClickHouse-specific fields are listed here; the rest, and how to configure several instances, are in
[xgorm "Configuration"](../README.md#configuration).

```yaml
XGorm:
  Driver: clickhouse
  DSN: "${CH_DSN}"         # clickhouse://user:pass@host:9000/db; tcp:// http:// https:// are accepted too
  DialTimeout: 500ms       # the dial_timeout injected into the DSN; one already in the DSN is not overridden; 0 injects nothing and the driver uses its own 30s
```

- `dial_timeout` is appended to the end of the DSN's query, and the other parameters are not touched by a single byte (no
  reordering, no re-escaping).
- The database name may be written in the path or as `?database=`; when both are present the one in the query wins (and is
  what the connect log's `db` and the Span's `db.namespace` record).
- The TLS rules are in [xtls](../../xtls/README.md). While a TLS block is on: `https://` does not need `secure=true`; the DSN
  must not set `secure` / `skip_verify` / `tls_server_name`; and a `http://` DSN fails startup.

## API

| Name | Description |
|---|---|
| `Driver` | The constant `"clickhouse"`, that is the value `Driver` takes in the configuration. The package registers itself in `init`, and instances are still fetched with `xgorm.C` / `xgorm.CWithCtx` |

Writing a driver of your own: a TLS block is only accepted when the `Dialect` registered with `xgorm.RegisterDialect`
provides `OpenTLS`; otherwise configuring one fails startup.

## Notes

- **When `read_timeout` is not set it is 300s, and it governs a whole read**: it interrupts healthy long-running queries too,
  and a query that times out on a read is resent by `database/sql` for a total of 3 attempts.
  **A query's deadline must be shorter than `read_timeout`.** See ["Behavior (measured)"](#behavior-measured).
- **A new connection does not honor the ctx**: dialing and the handshake follow `dial_timeout` only (`DialTimeout`, 500ms by
  default).
- The driver always reports 0 rows affected for writes, so the Span's `db.rows_affected` is meaningless on ClickHouse.
- The DSN must be a URL with one of the four schemes above.
- A misspelled driver name or a forgotten import fails startup with
  `unknown Driver="clickhouse", registered: [mysql postgres]`, where the list is exactly `xgorm.Drivers()`.

## Behavior (measured)

The measurement environment and the cross-module summary table are in [`docs/behavior.md`](../../docs/behavior.md).

clickhouse-go v2.48.0 (the native protocol), `gorm.io/driver/clickhouse` v0.7.0, ch-go v0.74.0, ClickHouse 24.8.14 (e2e).

**Why it is a module of its own**: the module graph of an application that imports only xgorm
(`GOWORK=off go list -m all`, excluding the application's own modules) holds 68 modules, and adding the ClickHouse driver
makes it 131 (the non-standard-library packages in `go list -deps` go from 165 to 208). The bulk of the addition is Docker
and testcontainers -- clickhouse-go uses them for its integration tests, and `go.mod` cannot distinguish "test only".

**The version query in `Initialize`** is the same as on MySQL: a hard-coded `context.Background()`, run before the connection
retries. Measured against an address that accepts the connection and never answers, it waits out the full `dial_timeout`
even though the ctx was cancelled long before, and then just fails. It is likewise moved into the connection probe here, and
the version is still set on the Dialector (the driver uses it to decide how to rename columns (< 20.4) and how to handle
column precision (< 21.11)).

**Timeouts and cancellation:**

- When `read_timeout` is not set it is **300s** (measured with a far end that never answers and no deadline: the error came
  after 300.6s).
- **`read_timeout` governs a whole read, not "how long without receiving a byte".** A query is read in two stages: up to the
  first data block, then all the rest, with a deadline set once at the start of each stage and never extended. So **a healthy
  query fails as soon as the remaining results take longer than that**: measured, a stream of 50 rows x 100ms with
  `read_timeout=1s` reported `i/o timeout` after 1.0s, and `SELECT sleep(2)` likewise failed after 1.0s. Long-running
  queries need either a larger `read_timeout` or a deadline from the caller.
- **The caller's deadline replaces `read_timeout`**, even when it is longer (the same 5s stream with `read_timeout=1s` and a
  10s deadline finished reading in 5.0s); a pooled connection given 200ms returned in 201ms.
- **A new connection does not honor the ctx**: dialing and the handshake follow `dial_timeout` only. Once the pool is
  exhausted, a query that needs a new connection still waits out the full `dial_timeout` (500ms by default, measured
  500.2-501.5ms).
- **A query that times out on a read is resent by `database/sql` for a total of 3 attempts.** The driver reads a read timeout
  as a bad connection and returns `driver.ErrBadConn`, so `database/sql` picks another connection and sends it again
  (`maxBadConnRetries` = 2). Measured with `read_timeout=1s`: a query the server sleeps 1.5s on failed only after 3.0s, it
  appears started 3 times in `system.query_log`, and the error returned is `driver: bad connection` (the `i/o timeout` root
  cause is lost). In clickhouse-go v2.30.0 it was the other way round: the timed-out connection went back into the pool and
  the next query to borrow it **returned the previous query's results** (fixed upstream in v2.47.0).
- A **cancelled** ctx returns `context canceled` on the spot while sending the server a Cancel packet and closing the
  connection: the handler returned 0.5ms after the client disconnected. The server stops at a data-block boundary
  (disappearing from `system.processes` after about 115ms), and `sleep(2.5)` cannot be interrupted.
- `Ping` honors deadlines but not cancellation. The connection probe always carries a deadline (`2 x dial_timeout`), and a
  stop signal received during startup exits after about 400ms.
- **With `DialTimeout: 0` and no `dial_timeout` in the DSN**, the driver connects with a 30s budget (v2.48.0 `setDefaults`),
  and the probe's per-attempt budget follows it (60s) rather than the 1s fallback. Measured (ClickHouse 24.8.14, with a proxy
  in between delaying the server's first byte by 1.5s): computed from 0, all 3 attempts timed out after 1s each and startup
  failed after 5.3s; computed from 30s, it connected in 1.5s.
- Parameters are interpolated into the statement by the driver before it is sent to the server: a float becomes
  `cast(1.5, 'Float64')`, which is the spelling to search for when looking up statement text in `system.query_log`.

**An auth failure** is recognized from the `*clickhouse.Exception` the server returns (wrapped in a
`*clickhouse.HTTPError` over the HTTP protocol): 516 (AUTHENTICATION_FAILED) and 192 / 193 / 194 (the older UNKNOWN_USER,
WRONG_PASSWORD and REQUIRED_PASSWORD). Measured, both a wrong password and a missing user are `code: 516`, failing within
50ms after a single attempt; over HTTP it is `[HTTP 403] code: 516`. A missing database is 81, which is not an auth failure
and runs through all 3 attempts as usual.

That error usually carries `code: 81`, and occasionally only `driver: bad connection`: with a missing database the handshake
still succeeds, the server then sends the exception and closes the connection, and the driver's `database/sql` path reads one
byte off the connection before pinging to check whether it is still alive -- if the exception has already arrived it is
consumed by that read and the connection is discarded as bad. Measured with 64 goroutines connecting 20 times each (1280
connections in total), about 9% of the `database/sql` path behaved this way, and the driver's native API never did.

**TLS** (`verificationMode=relaxed`): both native and `https://` go through the TLS block, and `system.query_log` shows
`is_secure=1` for both.

- A wrong CA, a `ServerName` that does not match, or a client certificate not signed by the CA the server accepts: all fail
  within 2-6ms and are not retried.
- A TLS block connecting to a plaintext port: `first record does not look like a TLS handshake` (native) or
  `server gave HTTP response to HTTPS client` (HTTPS), retried as usual.
- Without a TLS block, `skip_verify=true` in the DSN connects, but the server certificate is not verified at all.
- Given a DSN, gorm's clickhouse driver parses its own copy and uses it to connect to each host directly for an UPDATE with
  `UpdateLocalTable` (`update.go`), and those direct connections carry no TLS block -- which is why the DSN is not handed to
  it while a TLS block is on.

A multi-host `clickhouse://u:p@h1:9000,h2:9000/db` is tried `in_order`, and queries carry on after the first host goes down
(e2e); the log's `addr` and the Span's `server.address` record the first one.
