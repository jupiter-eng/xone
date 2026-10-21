# xredis

The Redis client: what you get is the native `*redis.Client` (go-redis v9), built by the framework from the configuration and closed on exit.

- A single instance or several, taken by name with `xredis.C("name")`
- Commands honour the ctx deadline (go-redis ignores it by default and goes by `ReadTimeout` only)
- One Span per command, carrying the command name only, never the arguments
- An optional command log (`Log: true`): the command name and the first key, never a value
- The connection pool metrics `redis_pool_*`, switchable per instance
- One probe at startup: a Redis that cannot be reached fails startup outright

## Quick start

```yaml
# conf/application.yml
XRedis:
  Addr: "redis:6379"
  Password: "${REDIS_PASSWORD}"
```

```go
import (
	"errors"

	"github.com/redis/go-redis/v9"

	"github.com/jupiter-eng/xone/xredis"
)

err := xredis.C().Set(ctx, "k", "v", time.Minute).Err()

v, err := xredis.C().Get(ctx, "k").Result()
if errors.Is(err, redis.Nil) {
	// the key does not exist
}
```

go-redis methods take a ctx to begin with, so there is no `CWithCtx`.

## Configuration

```yaml
XRedis:
  Addr: "127.0.0.1:6379"
  Username: ""                  # the Redis 6+ ACL user name
  Password: "${REDIS_PASSWORD}"
  DB: 0
  DialTimeout: 500ms            # dialling + the TLS handshake; 0 = go-redis's default of 5s
  ReadTimeout: 500ms            # the longest one read waits when the caller gave no deadline; 0 = go-redis's default of 5s
  WriteTimeout: 500ms           # 0 = follows ReadTimeout
  PoolSize: 0                   # 0 = go-redis's default of 10 × GOMAXPROCS
  MinIdleConns: 5               # the idle connections kept open; with several instances, this many each
  MaxIdleConns: 0               # 0 = no limit
  MaxActiveConns: 0             # 0 = no limit
  PoolTimeout: 1s               # the longest to wait for a connection while the pool is full; 0 = ReadTimeout + 1s
  ConnMaxIdleTime: 5m           # 0 = go-redis's default of 30m
  ConnMaxLifetime: 5m           # 0 = connections are not replaced by age
  MaxRetries: 0                 # 0 = go-redis's default of 3; -1 turns retrying off
  MinRetryBackoff: 0s           # 0 = go-redis's default of 10ms; -1ns turns it off
  MaxRetryBackoff: 0s           # 0 = go-redis's default of 1s; -1ns turns it off
  Trace: true                   # one Span per command
  Metric: true                  # the connection pool metrics
  Log: false                    # one log line per command (a whole pipeline is one line): the command name + the first key, never a value
  SlowThreshold: 100ms          # anything above it is logged at WARN; needs Log on, 0 = never
  TLS:                          # the rules are in xtls; an empty ServerName = the host part of Addr
    Enable: false
    CAFile: ""
    CertFile: ""
    KeyFile: ""
    ServerName: ""
```

**Several instances** go under `Clients`, each with the fields above, and whatever is left out takes the default above:

```yaml
XRedis:
  Clients:
    default: {Addr: "redis:6379"}
    session: {Addr: "redis-session:6379", DB: 1}
```

`xredis.C()` takes `default` and `xredis.C("session")` the other one. The two spellings cannot be mixed.

## API

| Function | Description |
|---|---|
| `C(name ...string) *redis.Client` | Takes an instance, `default` without an argument. One that cannot be found panics outright, with a message saying whether it was called too early, never configured, or the name is misspelled |
| `Has(name ...string) bool` | Whether an instance is configured, to check an optional dependency first |
| `Names() []string` | Which instances are configured |
| `New(ctx, cfg) (*redis.Client, io.Closer, error)` | The pure constructor: touches no globals, reads no config file, usable outside the framework |

## Notes

- **To be able to stop, give the ctx a deadline.** Commands honour a deadline but not cancellation: cancelling the ctx
  does not wake a command already blocked on a read, which waits for `ReadTimeout` all the same. See
  ["Behavior (measured)"](#behavior-measured).
- **The longest one command can take** (when the caller gave no deadline):
  `(MaxRetries+1) × (DialTimeout+ReadTimeout) + MaxRetries × MaxRetryBackoff`, 7s by default.
- **A duration of 0 does not mean "no limit"**, it means go-redis's default (see the comments in the configuration
  above); the startup probe's budget goes by the converted value too.
- **Negative values are refused** everywhere except `MaxRetries: -1`, `MinRetryBackoff: -1ns` and
  `MaxRetryBackoff: -1ns`, which mean "off".
- **Memory**: about 66KiB of heap per connection, which is `PoolSize × 66KiB` once the pool has filled up.
- **The startup probe**: one probe per instance, at most 3 attempts; `WRONGPASS` / `NOAUTH` is not retried. See
  [behavior.md "The startup connection probe"](../docs/behavior.md#the-startup-connection-probe).

## Observability

### Logs

| Message | Level | Fields |
|---|---|---|
| `xredis connected` | INFO | `name`, `addr`, `db`, `tls`, `min_idle_conns` |
| `xredis ready` | INFO | `instances` |
| `redis command` / `slow redis command` / `redis command failed` | INFO / WARN / WARN | `name`, `cmd`, `key`, `elapsed_ms`; as the case may be `key_truncated`, `nil`, `threshold_ms`, `error`, `error_code` (requires `XRedis.Log: true`, the fields are described below) |
| `redis pipeline` / `slow redis pipeline` / `redis pipeline failed` | INFO / WARN / WARN | `name`, `count`, `cmds`, `elapsed_ms`; as the case may be `tx_failed`, `threshold_ms`, `error`, `error_code` (requires `XRedis.Log: true`) |
| `xredis go-redis log` | WARN | `detail`: go-redis's own log line verbatim (see ["Behavior (measured)"](#behavior-measured)) |

The fields of the command log:

| Field | Description |
|---|---|
| `name` | The instance name (one built by calling `New` directly has none) |
| `cmd` | The command name, lowercased; one that does not look like a command name is logged as `<invalid>`. A pipeline's `cmds` holds the first 10 command names, by the same rule |
| `key` | The first key, left out for a command whose key cannot be told reliably; truncated beyond 256 bytes, with `key_truncated: true` |
| `count` | The number of commands in the pipeline, `MULTI` and `EXEC` included for `TxPipelined` |
| `elapsed_ms` | Measured from entering the hook: waiting for the pool, and a new connection's dialling and `HELLO`, are part of it |
| `nil` | `true` when the key does not exist (`redis.Nil`), still INFO |
| `tx_failed` | `true` on a `WATCH` conflict (`redis.TxFailedErr`), still INFO and with no `error` |
| `error` / `error_code` | On a failure; a pipeline logs the first error that is not `redis.Nil` |

- **Only the command name and the first key are logged**: values and the remaining arguments never are, and a command
  whose arguments may hold a credential -- `AUTH`, `HELLO`, `MIGRATE`, `CONFIG` -- is logged by name alone.
- **A key is logged whole** (truncated only beyond 256 bytes): with `Log` on, do not build tokens, phone numbers or
  email addresses into a key, as they land in the log along with it.
- **An error text is not always logged**: a server-side error writes `error` as
  `redis server error <code> (message omitted, ...)` only, with the code in `error_code`; on the client side only
  timeouts, a refused connection, a dropped connection, `context canceled`, `redis: client is closed` and a pool
  timeout are logged verbatim, and everything else writes
  `redis client error (message omitted, it may contain reply data)`. The reasoning is in
  ["Behavior (measured)"](#behavior-measured). The error returned to the caller is unchanged.
- One that is both slow and failed is logged as `... failed`, with no separate slow line.

The global logging conventions (`trace_id` injection, `xlog.AddKV`, the framework's start and stop logs) are in
[`docs/observability.md`](../docs/observability.md#logs).

### Metrics

| Metric | Type | Labels | Source |
|---|---|---|---|
| `redis_pool_connections` / `redis_pool_connections_idle` | gauge | `name` | xredis, `XRedis.Metric`, per instance |
| `redis_pool_connections_stale_total` / `redis_pool_hits_total` / `redis_pool_misses_total` / `redis_pool_timeouts_total` | counter | `name` | xredis |

The metric name prefix, the constant labels and a few general rules are in
[`docs/observability.md` "Metrics"](../docs/observability.md#metrics).

### Tracing

| Source | Span name | Key attributes |
|---|---|---|
| xredis | The command name (`get`, `cluster info`), `redis.pipeline get set` for a pipeline and `redis.dial` for connecting; one that does not look like a command name becomes `<invalid>`, by the same rule as the command log's `cmd` | The command name alone, with no arguments in `db.statement`; the status description on an error follows the same rules as the command log's `error` |

The whole picture of tracing, propagation and the trust boundary is in
[`docs/observability.md` "Tracing"](../docs/observability.md#tracing).

## Behavior (measured)

The measurement environment and the cross-module summary table are in [`docs/behavior.md`](../docs/behavior.md).

go-redis v9.22.0, redisotel v9.22.0, Redis 7.0.15.

**Commands honour the ctx deadline, not cancellation.** By default go-redis ignores even the deadline and goes by
`ReadTimeout`: a request with a 200ms budget was measured waiting the full 5s on a slow Redis. Here
`ContextTimeoutEnabled` is turned on and the deadline becomes the socket deadline. But **cancelling** the ctx does not
wake a command already blocked on a read (`ReadTimeout: 1s`, cancelled at 100ms, the command returned at 1.0s), and
xredis cannot make up for it from the inside: the result is written into the `*Cmd` the caller holds, and returning
early would race the goroutine still reading. What that means for the stop sequence: when XGin disconnects at its
deadline it cancels the request's ctx, and a handler stuck on a Redis read waits for `ReadTimeout`, or for xredis's
stop hook to close the connection pool (measured with `ReadTimeout: 10s` and `WithStopTimeout(3s)`: disconnected at
1.6s, the handler returned at 2.0s when the pool was closed, and `Stop` reported `1 handler(s) still running`).

**The longest one command can take** (when the caller gave no deadline):

```
(MaxRetries+1) × (DialTimeout+ReadTimeout) + MaxRetries × MaxRetryBackoff
```

4 × 1s + 3 × 1s = 7s by default, and 1s with `MaxRetries: -1` (`MaxRetries` defaults to 3, `MinRetryBackoff` to 10ms
and `MaxRetryBackoff` to 1s). The formula holds because connecting dials exactly once: by default go-redis hides
another layer of retrying inside every connect (`DialerRetries` 5 times, 100ms apart), making one "connect"
5 × 500ms + 4 × 100ms = 2.9s, and one command was measured taking 11.7s against a host that was down (no answer to
the SYN). xredis fixes `DialerRetries` at 1, which brings the same scenario to 2.1s with the defaults and 0.5s with
`MaxRetries: -1`. Once the pool has accumulated `PoolSize` failed connects, go-redis stops dialling and returns the
previous error right away.

**A duration of 0 is go-redis's default.** go-redis v9.22.0 replaces the 0 inside `NewClient` (`options.go`, `init`),
measured through `client.Options()`: `DialTimeout` 5s, `ReadTimeout` 5s, `WriteTimeout` following the converted
`ReadTimeout`, `PoolTimeout` being `ReadTimeout + 1s` (3s with `ReadTimeout: 2s`) and `ConnMaxIdleTime` 30m;
`ConnMaxLifetime` is left at 0, which means connections are not replaced by age. The startup connection check's budget
per attempt is the converted `DialTimeout + ReadTimeout`: with both configured as 0 that is 10s, not a fallback of 1s --
going by the 0 in the configuration, go-redis would still be willing to wait for a connection that answers `PONG`
after 1.5s while the check had already called it a timeout, failing all 3 attempts.

**The TLS handshake is governed by `DialTimeout`** (go-redis uses `tls.DialWithDialer`, where dialling and the
handshake share one timeout).

**What is sent on every new connection** (counting Spans with redisotel attached):

| | go-redis's default | Here |
|---|---|---|
| `HELLO` | Once per new connection, negotiating RESP3; a server that does not know it falls back to RESP2. Configured as 2 it still sends `HELLO 2` | Unchanged |
| `CLIENT SETINFO` | One more pipeline per new connection, only so that `CLIENT LIST` shows the library name. The subcommand did not exist before Redis 7.2: on 7.0.15 **every connection** gets `unknown subcommand 'setinfo'`, the error is swallowed, but every connection leaves behind a failed `redis.pipeline` Span -- and since `ConnMaxLifetime` replaces the connections every 5 minutes, a batch of them every 5 minutes | Turned off (`DisableIdentity: true`) |
| `CLIENT MAINT_NOTIFICATIONS` | auto: tried once on every new connection, and a server that knows it turns on "maintenance notifications", which widen the read and write timeouts during maintenance (10s by default in the source; no server knowing the command was at hand, so this is not measured). 7.0.15 answers `unknown subcommand`, and the first few connections opened concurrently each leave a failed Span | Turned off: widening to 10s does not square with what `ReadTimeout` promises |

**The memory one connection takes**: a 32KiB read buffer + a 32KiB write buffer, measured at about 66KiB of heap each
(over 200 idle connections). Once the pool has filled up that is `PoolSize × 66KiB`: `PoolSize` defaults to
10 × GOMAXPROCS, so 40 connections on 4 cores is about 2.6MB and 640 on 64 cores about 41MB; day to day there are only
`MinIdleConns` (5 by default). The buffer sizes are not exposed in the configuration. `MaxConcurrentDials` defaults to
`PoolSize` and is not set separately.

**`Trace`**: redisotel writes the whole command including its arguments into `db.statement` by default (a `SET`'s value
was measured appearing verbatim), which is turned off here (`WithDBStatement(false)`). The hook is not free: with no
tracing installed it was measured at about +3µs and +8 allocations per command.

**A Span carries no error text.** redisotel v9.22.0 records an error as `span.RecordError(err)` plus
`span.SetStatus(codes.Error, err.Error())`, which puts the server's text into `exception.message` and the status
description (measured on Redis 7.0.15: `ERR unknown command 'foo', with args beginning with: 'secretarg1'`, the
`ERR secretarg1` of `redis.error_reply(ARGV[1])` inside `EVAL`, and the `secretarg1` of `return {err=ARGV[1]}`). It
offers no option to rewrite the error, so xredis wraps the TracerProvider it hands over: the status description becomes
the same text as the command log's `error` field (`redis server error ERR (message omitted, it may contain argument
values)`), and `RecordError` records only network errors, timeouts and the like, whose text can be logged verbatim,
while the rest become no event at all. The error returned to the caller is unchanged.

**A Span name carries no arguments.** redisotel v9.22.0 uses `cmd.FullName()` as the Span name -- the first argument
lowercased verbatim, unvalidated: `Do(ctx, "SET k1 hunter2")` was measured producing the Span name `set k1 hunter2`,
and inside a pipeline `redis.pipeline set k2 pipesecret get`. xredis names it after the command instead, by the same
rule as the command log's `cmd` field: one that does not look like a command name becomes `<invalid>`
(`redis.pipeline <invalid> get`), while a well-formed command name and `redis.dial` are unchanged.

**What the command log (`Log: true`) holds**. Measured with go-redis v9.22.0 and Redis 7.0.15, with no Span in the
caller's ctx (one root Span per command):

| Call | Log |
|---|---|
| `Set(ctx, "user:42", <value>, time.Minute)` | `redis command`, `cmd: set`, `key: user:42` |
| `Get(ctx, "user:42")`, a hit | `redis command`, `cmd: get`, `key: user:42` |
| `Get(ctx, "user:missing")`, no such key | `redis command` (INFO), `cmd: get`, `key: user:missing`, `nil: true` |
| `HSet(ctx, "h:1", <field>, <value>)` | `cmd: hset`, `key: h:1` |
| `MGet(ctx, "a:1", "a:2")` | `cmd: mget`, `key: a:1` (only the first one) |
| `Eval(ctx, script, []string{"lua:key"}, <ARGV>)` | `cmd: eval`, `key: lua:key`; with an empty `keys` there is no key (the third argument is already ARGV) |
| `XRead(... Streams: {"stream:1", "0"})` | `cmd: xread`, `key: stream:1` |
| `Do(ctx, "auth", <password>)` | `cmd: auth`, nothing beyond the command name |
| `Incr` on a value that is not a number | `redis command failed` (WARN), `error_code: ERR` |
| A pipeline: `Set`, `Get` (no such key), `Incr` | One `redis pipeline` line, `count: 3`, `cmds: [set get incr]`; for `TxPipelined`, `multi` / `exec` count towards both `count` and `cmds` (a single `Set` is `count: 3`, `cmds: [multi set exec]`) |
| `Do(ctx, "SET k1 <value>")` (the whole command crammed into one argument) | `redis command failed`, `cmd: <invalid>`: go-redis takes the command name to be the first argument lowercased verbatim, neither validated nor truncated, so without correcting it the log would read `cmd: "set k1 <value>"`. Only `^[a-z][a-z0-9._\|-]{0,63}$` is accepted, module commands (`json.set`) included; a pipeline's `cmds` follows the same rule |
| Somebody changed the key first inside a `Watch`, and `TxPipelined` returned `redis.TxFailedErr` | One `redis pipeline` line at INFO, `cmds: [multi set exec]`, `tx_failed: true`, and no `error` (what the caller gets is still `TxFailedErr`) |

**Where the key is is decided here, not by go-redis**. go-redis does know (`Cmder`'s unexported `firstKeyPos`), but its
fallback is "anything not on the no-key list has the key as its first argument": `MIGRATE`'s first argument is a host,
and `INFO` and `KEYS` would be treated as having a key too. Here it is the other way round -- only a list of commands
whose first argument is certainly a key is accepted (the common string, hash, list, set, zset, stream, geo and bitmap
commands), plus `EVAL` / `EVALSHA` / `FCALL` (the third argument when `numkeys` is greater than 0; go-redis's `Eval`
passes `numkeys` as an int while `Do` assembles a string, and both are accepted) and `XREAD` / `XREADGROUP` (the one
after `STREAMS`). Anything not on the list has no key logged -- the price of guessing wrong is a value in the log,
while a missing key is only a missing field. A key is taken only from a string or `[]byte` argument, and no other type
is guessed at.

**A key beyond 256 bytes is truncated without cutting a UTF-8 character apart**: the search back for a character start
goes at most 3 bytes. A key is an arbitrary byte string, and one that is a run of `0x80` has no character start, so it
is cut at 256 bytes (a run of `�` in JSON) rather than becoming an empty string.

**A server-side error text carries the arguments along**, so only the error code is logged (measured on Redis 7.0.15):

| Case | The text | `error_code` in the log |
|---|---|---|
| An unknown command | `ERR unknown command 'foo', with args beginning with: 'secretarg1' 'secretarg2'` | `ERR` |
| `redis.error_reply(ARGV[1])` inside `EVAL` | `ERR mysecretvalue` (7.0 filled in the `ERR` itself) | `ERR` |
| `return {err=ARGV[1]}` inside `EVAL` | `plainsecret`: the value sits where the error code belongs | none |
| `WRONGTYPE`, `ERR value is not an integer or out of range` | No arguments in it | `WRONGTYPE` / `ERR` |

The third case shows that "take the first word as the error code" is not safe either: an error code is recognized only
from the set Redis uses itself (`ERR`, `WRONGTYPE`, `NOSCRIPT`, `BUSY`, `NOAUTH`, `WRONGPASS`, `NOPERM`, `OOM`,
`READONLY`, `EXECABORT`, `LOADING`, `MOVED`, `ASK`, `CROSSSLOT`, `CLUSTERDOWN` and so on), and anything else is
reported as a server-side error and nothing more. Not every client-side error is logged verbatim either: go-redis
writes the reply into the error when it fails to parse one (`reader.go`'s `can't parse %q`).

**`redis.Nil` and a WATCH conflict are not failures.** A missing key is the branch a read-through cache takes all day
long. `redis.TxFailedErr` is the normal outcome of optimistic locking, but it is a `proto.RedisError` go-redis builds
on the client side, of the same type as a server-side error, so without recognizing it separately it would be logged as
`redis pipeline failed` with `error: redis server error (message omitted, ...)`. A pipeline's `Exec` returns
`redis.Nil` as a whole as soon as one command got nil, so the first error that is not nil is looked for instead: a
`GET` (no such key) followed by an `INCR` that fails logs `redis pipeline failed` and `error_code: ERR`.

**`elapsed_ms` is more than the network round trip**: with `PoolSize: 1` and the only connection held by a 1s `BLPOP`,
the `GET` right behind it was measured logging `elapsed_ms: 1028` (waiting for the pool); a command that has to open a
connection includes dialling and `HELLO` (with a fake server making `HELLO` take 80ms, that `GET` logged 81.7).

**Logging and tracing**: the logging hook is attached after redisotel's, and what go-redis attaches first ends up on
the outside, so it sits inside the redis Span -- the log's `span_id` is that command's Span, and its `trace_id` is the
caller's (measured: the caller's Span `48ee662c...`, the redis Span `c97ed554...` whose parent it is, and the log
carrying `c97ed554...`). With no Span in the caller's ctx, each command gets a root Span of its own and the log carries
its `trace_id`; with `Trace: false` the log's ctx is the caller's.

**One `hello` line per new connection**: the `HELLO` go-redis sends on a new connection (and the `SELECT` when `DB` is
not 0) goes through the hooks as well, measured as one `redis command` with `cmd: hello` per new connection (plus one
`redis pipeline` with `cmds: [select]` when `DB: 1`). With a password configured, `HELLO 3 AUTH <user> <password>` is
likewise logged as `cmd: hello` alone, and the password was not found anywhere in the measured output. The pool
replaces connections by `ConnMaxLifetime` (5m by default), so that is a batch every 5 minutes. Redis before 6 has no
`HELLO`, and every new connection becomes one `redis command failed` line (go-redis swallows the error and falls back
to RESP2). The connection probe at startup is not logged: the hooks are attached only after it has succeeded.

**go-redis's own log** goes to stderr as plain text through the standard library's log by default
(`redis: 2026/09/24 10:00:00 pool.go:762: ...`). xredis's `init` connects it to slog: one `xredis go-redis log` record
at WARN, with the original text in `detail`. The ones logged with a command's ctx carry a trace_id; the most common
one, `failed to dial`, does not -- go-redis dials with `context.Background()` in a goroutine of its own, and with Redis
refusing connections all 45 records were measured without a trace_id. A custom logger (`redis.SetLogger`) is to be set
in `main`, or in a package that imports xredis; set in the `init` of a package that does not import xredis, it may be
overwritten by xredis.
