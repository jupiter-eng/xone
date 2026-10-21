# Defaults that differ from the underlying libraries, and what was measured

Every library the framework integrates has its own default behavior, and plenty of those differ
from its README and from what people assume. This document is the **summary table**: each row is
one place where "the library's default ≠ the default here", linking to that module's README under
"Behavior (measured)", where the measured numbers and the dependency version live — when the
numbers no longer match after a dependency upgrade, the behavior changed and has to be measured
again. The cross-module startup connection probe is documented here as well.

The measurement environment: everything without a note is local loopback on Go 1.25; anything
marked e2e was measured under `e2e/`, with real processes against real services (PG 16,
MySQL 8.0.46, Redis 7.0.15, ClickHouse 24.8.14).

- [The summary table](#the-summary-table)
- [The startup connection probe](#the-startup-connection-probe)
- [What was measured per module](#what-was-measured-per-module)

## The summary table

| Setting | The library's default | The default here | Why |
|---|---|---|---|
| [`XGin.TrustedProxies`](../xgin/README.md#behavior-measured) | Trust everyone (`0.0.0.0/0`) | Private ranges only (`private`) | Otherwise whoever sends `X-Forwarded-For` decides `client_ip` in the access log, and the rate limiting and auditing built on it fail with it; the private ranges are the hop the load balancer, the ingress and the pods sit on |
| [`XGin.MaxMultipartMemory`](../xgin/README.md#behavior-measured) | 32MB | 8MB | It is the spill-to-disk threshold rather than a request body limit, and costs about three times its value on the heap |
| [`XGin.ReadHeaderTimeout: 0`](../xgin/README.md#behavior-measured) | Falls back to `ReadTimeout` (0 by default), so no limit at all | Fails startup | Half a request header is enough to hold a connection indefinitely |
| [`XGin.UseH2C`](../xgin/README.md#behavior-measured) | x/net's `h2c.NewHandler` | The standard library's `Protocols.SetUnencryptedHTTP2` | The former hijacks the connection and `Shutdown` cannot reach in-flight requests |
| [gin `RedirectTrailingSlash`](../xgin/README.md#behavior-measured) | On: `/users/` answers 301 (GET) / 307 (other methods) to `/users` | Off, so it goes to `NoRoute` and is recorded as a 404 `unmatched` | A redirected request runs no middleware at all and appears in neither the access log, the metrics nor the trace |
| [gin `ContextWithFallback`](../xgin/README.md#behavior-measured) | Off: a `*gin.Context`'s `Value` only looks at `c.Keys` and `Done()` is nil | On, delegating to `c.Request.Context()` | Otherwise `xlog.AddKV(c, ...)` and `Start(c, ...)` lose the log scope and the parent span, and cancellation does not propagate |
| [Hijacked connections on `http.Server`](../xgin/README.md#behavior-measured) | `Shutdown` does not wait, `Close()` cannot drop them, and the request ctx is never cancelled | `BaseContext` is cancelled after `Shutdown` / `Close()` | Otherwise a WebSocket handler waiting on the ctx makes `Stop` burn the whole budget before reporting |
| [`XEcho.TrustedProxies`](../xecho/README.md#behavior-measured) | `IPExtractor` nil: `X-Forwarded-For` / `X-Real-IP` believed from anyone | Private ranges only (`private`), with the same algorithm as xgin | Measured, a public peer sending `X-Forwarded-For: 1.2.3.4` makes `c.RealIP()` return `1.2.3.4` |
| [`XEcho` error responses](../xecho/README.md#behavior-measured) | Rendered by `HTTPErrorHandler` only after the whole middleware chain returns | The built-in middleware renders in its own layer | Otherwise 404 / 405 / 500 all appear in the access log, the metrics and the trace as a 200 with `bytes_out` 0 |
| [`XEcho` panics](../xecho/README.md#behavior-measured) | Not caught: the connection breaks and net/http writes the stack to stderr | Caught, logged as ERROR and answered with a 500 | The stack never reaches the logging platform and the client sees only an EOF |
| [`XEcho` server timeouts](../xecho/README.md#behavior-measured) | All four timeouts on `e.Server` are 0 | `e.Start` is not used, and the four timeouts match `XGin`'s | Same as `XGin.ReadHeaderTimeout` |
| [echo's own log](../xecho/README.md#behavior-measured) | gommon writes to `os.Stdout` in its own JSON format | Routed into slog, keeping echo's level (raised to DEBUG while `XEcho.Debug` is on, which echo itself only does inside `e.Start`) | Output bypassing slog never reaches the logging platform |
| [`XEcho` group fallback routes](../xecho/README.md#behavior-measured) | A group with middleware quietly registers `RouteNotFound` and `c.Path()` becomes `/api/v1/*` | Anything landing on a fallback is recorded as `unmatched` | Otherwise a 404 shows up on the dashboard as `/api/v1/*` and does not match xgin |
| [`XEcho`'s `e.Pre`](../xecho/README.md#behavior-measured) | Middleware on `e.Use` cannot see requests that ended inside Pre and cannot catch panics there | The built-in middleware sits on `e.Pre`, touches no request there, keeps the ctx on `echo.Context` and swaps it onto the request after routing | Auth, rate limiting and 301s in Pre reach neither the log nor the metrics; replacing the request in Pre breaks `MethodOverride` |
| [`http.Server.ErrorLog`](../xgin/README.md#logs) | nil: writes through the standard library's log (INFO through slog, with a different message per line) | Routed into slog as WARN, with the message `<module> http server error` | Errors such as a failed TLS handshake cannot be searched or alerted on by message |
| [`XGorm.Log: false`](../xgorm/README.md#behavior-measured) | Replaced by GORM's own stdout logger | Really prints nothing | That default implementation writes ANSI colors straight to `os.Stdout` |
| [`XGorm.Log: true`](../xgorm/README.md#behavior-measured) | Parameter values are interpolated into the SQL before logging | Only the SQL with its placeholders is logged | Otherwise `WHERE password = ?` is logged with the real password |
| [`XGorm` connecting](../xgorm/README.md#behavior-measured) | `gorm.Open` pings once on its own | Turned off, in favor of the framework's ctx-aware probe | It uses its own context, beyond the reach of the stop signal and the retries |
| [`XGorm` MySQL / ClickHouse version query](../xgorm/README.md#behavior-measured) | Queried inside `Initialize` with `context.Background()` | Moved into the connection probe | That is the first connection, and it neither retries nor honors the stop signal |
| [`XGorm` MySQL `parseTime`](../xgorm/README.md#behavior-measured) | `false` | `true` is added when the DSN does not say | Otherwise a `DATETIME` cannot be scanned into a `time.Time` |
| [`XGorm` server error text](../xgorm/README.md#behavior-measured) | Logged and put on the span as is | Only the error code is recorded | The text carries parameter values |
| [go-sql-driver's log](../xgorm/README.md#behavior-measured) | The standard library's log to `os.Stderr` | Routed into slog as WARN | Plain text bypassing slog never reaches the logging platform |
| [`XRedis` command timeout](../xredis/README.md#行为与实测) | Honors `ReadTimeout` only | Honors the caller's ctx deadline | Otherwise a request with a 200ms budget waits out the whole `ReadTimeout` |
| [`XRedis` connecting](../xredis/README.md#行为与实测) | Redials 5 times internally per connection | Dials once | Otherwise one command takes 11.7s when the host is down |
| [`XRedis` handshake on a new connection](../xredis/README.md#行为与实测) | `CLIENT SETINFO` and maintenance notifications both on | Both off | A server older than 7.2 leaves one failing span per connection |
| [`XRedis.Trace`](../xredis/README.md#行为与实测) | The whole command with its arguments goes into `db.statement` | The command name only | Otherwise the value of a `SET` reaches the tracing backend verbatim |
| [go-redis's log](../xredis/README.md#行为与实测) | The standard library's log to `os.Stderr` | Routed into slog as WARN | Same as go-sql-driver |
| [`XKafka` consumer commits](../xkafka/README.md#行为与实测) | Automatically commits "whatever the last poll returned", processed or not | Commits only what was processed (`AutoCommitMarks`) | With one goroutine per partition, messages still queued and unprocessed get committed first and are lost the moment the process dies |
| [`XKafka.Consumer.ResetOffset`](../xkafka/README.md#行为与实测) | earliest | latest | Otherwise a newly deployed consumer group reprocesses the topic's entire history |
| [`XKafka` stopping](../xkafka/README.md#行为与实测) | `Close` fails every buffered message | `Flush` first, then `Close` | Otherwise messages handed to `Produce` at the moment of exit are dropped silently |
| [`XKafka` startup probe](../xkafka/README.md#行为与实测) | `Ping` ignores the ctx for an unresponsive broker and waits the full 10s | Moved into a goroutine that watches the ctx | Otherwise a stop signal during startup waits 10s |
| [`XKafka`'s franz-go log](../xkafka/README.md#行为与实测) | Not recorded | Routed into slog at WARN and above | Connection failures and rebalance errors would pass in silence |
| [`XKafka.Trace`](../xkafka/README.md#可观测) | kotel writes the message key onto the span | kotel is not used, and the span holds no key | Keys often hold a user id or a phone number |
| [`XCache.MaxCost`](../xcache/README.md#behavior-measured) | 56 bytes of internal overhead are counted per entry | Only the cost you give is counted | Otherwise `MaxCost: 2000` really holds 35 entries |
| [`XCache` stopping](../xcache/README.md#behavior-measured) | `Close` | `Clear` | `Close` running alongside concurrent reads and writes panics |
| [`XHttp`'s resty log](../xhttp/README.md#行为与实测) | Writes to `os.Stderr` with the URL's query string | Routed into slog with the query string removed | Tokens do not reach disk |
| [`XHttp` cookies](../xhttp/README.md#行为与实测) | `resty.New()` comes with a cookie jar | None | Unrelated calls would share a session cookie somebody else was given |
| [`XHttp` outbound spans](../xhttp/README.md#行为与实测) | `url.full` carries the query string | The query string is removed and the span name is the method alone | Tokens stay out of the tracing backend and the span name cardinality stays bounded |
| [`XHttp` retry conditions](../xhttp/README.md#行为与实测) | Adding a condition voids resty's own judgment | Only transport errors are retried | Otherwise a 200 with broken JSON is retried as well |
| [`XTrace` forwarding and baggage](../xtrace/README.md#行为与实测) | Inbound values accepted from anyone | Accepted only when the direct peer is in `XGin.TrustedProxies` (`XEcho.TrustedProxies`) | Otherwise a public client can forge `X-Tenant-Id` and carry it into the internal network |
| [`XTrace` sampling](../xtrace/README.md#行为与实测) | `AlwaysSample`, ignoring upstream | `ParentBased`, honoring upstream when there is one | Otherwise an upstream `sampled=00` is rewritten to `-01` on the way down |
| [`XMetric.Namespace`](../xmetric/README.md#behavior-measured) | A non-conforming name is escaped on export | Fails while the config is read | Otherwise the dashboard cannot find the name you wrote |

## The startup connection probe

XGorm, XRedis and XKafka each probe once at startup through one shared implementation
(`internal/xclient.Probe`, built on `xutil.Retry`):

- **At most 3 attempts** (the first plus two retries), with the wait between them doubling and
  jittered: the backoff starts at 1s, the actual wait is drawn from `[0, the current backoff]`, and
  the two backoff ceilings are 1s and 2s. One instance therefore waits at most
  `3 × the per-probe budget + 3s`: XGorm against PostgreSQL defaults to `3 × 1.5s + 3s = 7.5s`,
  XRedis to `3 × 1s + 3s = 6s` and XKafka to `3 × 2s + 3s = 9s`.
- **An authentication failure is not retried**: PostgreSQL's SQLSTATE class 28 (a wrong password
  and a missing user are both 28P01); MySQL's 1045 (a wrong password or a missing user) and 1044
  (no permission on that database — an account without global permissions connecting to a database
  that does not exist also gets 1044; measured on MySQL 8.0.46); Redis's `WRONGPASS` / `NOAUTH`;
  ClickHouse's 516 and 192 / 193 / 194; Kafka's `SASL_AUTHENTICATION_FAILED` and friends (not
  measured against a real Kafka, see xkafka). The error reads
  `authentication to <address> failed` rather than `cannot reach`.
- **A rejected certificate is not retried**: an `x509` verification failure, or the peer sending a
  `bad_certificate` / `unknown_ca` / `certificate_required` alert. Trying again presents the same
  certificate and reaches the same conclusion.
- A stop signal during startup gives up on the spot, without waiting for a stuck probe to hit its
  timeout.

## What was measured per module

| Module | Measurements |
|---|---|
| [xtls](../xtls/README.md#behavior-measured) | Client-side TLS: each driver's errors and timings, and the driver defaults without a TLS block |
| [xgorm](../xgorm/README.md#behavior-measured) | GORM in general, MySQL, PostgreSQL |
| [xgorm/clickhouse](../xgorm/clickhouse/README.md#behavior-measured) | ClickHouse: timeouts and cancellation, resends, authentication, TLS |
| [xredis](../xredis/README.md#行为与实测) | go-redis: timeouts, connection retries, the handshake on a new connection, memory |
| [xcache](../xcache/README.md#behavior-measured) | ristretto: internal overhead, stopping, the cost of metrics, TTL |
| [xkafka](../xkafka/README.md#行为与实测) | franz-go: automatic commits, offset reset, `Close` and `Flush`, `Ping`, a missing topic, rebalancing and session timeouts |
| [xhttp](../xhttp/README.md#行为与实测) | The defaults of resty / otelhttp / the standard library's Transport |
| [xcron](../xcron/README.md#behavior-measured) | cronexpr: field counts, timezone prefixes, DST, specs that never fire; the drift of `@every` |
| [xgin](../xgin/README.md#behavior-measured) | gin / net/http: proxies, uploads, timeouts, h2c, TLS, graceful shutdown |
| [xecho](../xecho/README.md#behavior-measured) | echo: client_ip, error rendering, route templates, group fallback routes, `e.Pre`, timeout middleware, panics, gommon logs, compression, the multipart threshold, strict route matching |
| [xmetric](../xmetric/README.md#behavior-measured) | client_golang: buckets, names, constant labels |
| [xtrace](../xtrace/README.md#行为与实测) | The OTel SDK: sampling, service.name, forwarding |
| [xlog](../xlog/README.md#行为与实测) | `Perm`, rotated file names, timezones |
