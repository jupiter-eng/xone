# Observability: logs, metrics and tracing

The global conventions for the logs, metrics and spans the framework produces are here; each
module's own log fields, metrics and spans are under "Observability" in its README, which is what
dashboards, alerts and log searches are built from. Every field name and message is English, and
the name is the one written in the code. The switches live under "Configuration" in each module's
README (the index is in [`config.md`](config.md#config-blocks--module-documentation)).

| What you want | Where it is |
|---|---|
| Business logs carrying `trace_id` and per-request fields | [Logs](#logs): use `slog.InfoContext(ctx, …)` and `xlog.AddKV(ctx, k, v)` |
| The access log fields and the masking rules | [xgin "Access log"](../xgin/README.md#access-log) (xecho's fields and rules are identical, see [xecho "Access log"](../xecho/README.md#access-log)) |
| Building a dashboard: which metrics the framework ships | [Metrics](#metrics) |
| How tracing propagates and whose headers are accepted | [Propagation and the trust boundary](#propagation-and-the-trust-boundary) |
| Exporting spans | [xtrace "Quick start"](../xtrace/README.md#quick-start): `xtrace.AddSpanProcessor(...)` |

- [Logs](#logs)
  - [The framework's own logs](#the-frameworks-own-logs)
- [Metrics](#metrics)
- [Tracing](#tracing)
  - [Span names and attributes](#span-names-and-attributes)
  - [Propagation and the trust boundary](#propagation-and-the-trust-boundary)

## Logs

xlog replaces `slog.Default()` with a handler configured from `XLog`, which both the business and
the framework write to. It comes along with xgin / xecho / xcron; a program without a web framework
(a consumer, a one-off job, anything using only xgorm / xredis / xkafka) that wants it imports
`github.com/jupiter-eng/xone/xlog` anonymously, since otherwise `slog.Default()` stays as it was
and none of the below exists. To write to your own logging backend (zap, a company logging SDK),
call `xlog.UseHandler(h)` before `xone.Run`; everything below still applies, see
[xlog "Using your own logging backend"](../xlog/README.md#using-your-own-logging-backend).

- **`trace_id` / `span_id`**: with tracing on (see [Tracing](#tracing)), every line written through
  a ctx-aware method (`slog.InfoContext(ctx, …)`) carries them automatically (xtrace wires it up;
  xlog itself does not depend on OpenTelemetry).
- **Per-request fields**: `xlog.AddKV(ctx, "user_id", id)` adds a field at any depth of the call
  stack, and every later log line in the same request (the access log included) carries it. The
  scope is opened at the start of each request by xgin's / xecho's `LogScope` middleware; a
  non-web entry point of your own opens it with `xlog.CtxWithScope(ctx)`.
- **Fields for one stretch of calls**: `xlog.CtxWithKV(ctx, map[string]any{"order_id": id})`
  derives a new ctx, and only lines written through it carry those fields; fields already on the
  parent ctx still come along. Derive one per item in a batch and one per goroutine started, and
  they never bleed into each other.

### The framework's own logs

One line each for starting, stopping and connecting, none of them carrying credentials. The ones
most often searched for:

| Message | Level | Fields |
|---|---|---|
| `loading config` | INFO | `file` |
| `no config file found, using defaults for everything` | WARN | `searched` |
| `starting` / `stopping` | INFO | `hook` (the hook function's name, `xgorm.initXGorm` for instance) |
| `shutdown signal received, closing gracefully; send it again to terminate now` | INFO | `signal` |
| `xgorm ready` / `xredis ready` / `xcache ready` / `xkafka ready` | INFO | `instances` |
| `xgorm go-sql-driver log` / `xredis go-redis log` / `xhttp resty log` | WARN (resty keeps its own level) | `detail`: the line the third-party library would have written to stderr |

Each module's own logs are under "Observability" in its README:
[xgin](../xgin/README.md#observability) (the access log, `xgin listening`,
`xgin http server error`) · [xecho](../xecho/README.md#observability) (the access log,
`xecho listening`, `echo internal log`, `xecho http server error`) ·
[xgorm](../xgorm/README.md#logs) (`xgorm connected`, SQL logs) ·
[xredis](../xredis/README.md#logs) (`xredis connected`, `redis command`, `redis pipeline`) ·
[xhttp](../xhttp/README.md#logs) (`xhttp ready`, `http request`) ·
[xcache](../xcache/README.md#logs) · [xkafka](../xkafka/README.md#logs)
(`kafka produce failed`, `kafka message failed` / `kafka message dead-lettered` /
`kafka message skipped, …` / `kafka handler panicked`, with the handler's logs carrying `topic` /
`partition` / `offset` / `group`) · [xcron](../xcron/README.md#logs) (`cron job finished` /
`cron job failed` / `cron job panicked`, `cron job skipped, previous run still running`) ·
[xflow](../xflow/README.md#logs) (`xflow flow done` / `xflow flow failed`; the per-step
`xflow step … done` is INFO and `… failed` is WARN) · [xtrace](../xtrace/README.md#logs).

## Metrics

Every metric the framework ships carries `XMetric.ConstLabels`; apart from client_golang's own
`go_*` / `process_*`, the names are also prefixed with `XMetric.Namespace` when one is set:

| Module | Metrics |
|---|---|
| [xgin](../xgin/README.md#metrics) · [xecho](../xecho/README.md#metrics) | `http_requests_total`, `http_request_duration_seconds` (identical names and labels on both sides, so one dashboard serves both) |
| [xhttp](../xhttp/README.md#metrics) | `http_client_request_duration_seconds` |
| [xgorm](../xgorm/README.md#metrics) | `db_pool_*` |
| [xredis](../xredis/README.md#metrics) | `redis_pool_*` |
| [xkafka](../xkafka/README.md#metrics) | `kafka_consume_duration_seconds` |
| [xcache](../xcache/README.md#metrics) | `cache_*` |
| [xmetric](../xmetric/README.md#metrics) | `log_errors_total`, `go_*` / `process_*`, and the business's own instrumentation |

A few things worth knowing:

- **`method`** folds into a fixed set: `GET` `HEAD` `POST` `PUT` `PATCH` `DELETE` `CONNECT`
  `OPTIONS` `TRACE`, and everything else (lowercase `get` included) becomes `OTHER` — a method is
  a free-form token, and copying it verbatim lets anyone blow up the time series.
- Connection pool and cache metrics are read when they are scraped; an instance with
  `Metric: false` does not appear in `/metrics` at all.
- A failed metric registration in an integration (a metric of that name already registered as a
  different type, say) **does not fail startup**: one error line is logged and that set of metrics
  is not exported.

## Tracing

xtrace installs the global TracerProvider and Propagator. Every integration that produces spans
(xgin, xecho, xgorm, xredis, xhttp, xcron, xkafka) depends on it, so using any one of them gives
tracing with nothing extra to import; to go without tracing set `XTrace.Enable: false`, and to turn
it off for one component only set that component's own `Trace: false`. For a program using none of
them (a consumer process with only xcache, say) that still wants tracing, import
`github.com/jupiter-eng/xone/xtrace` anonymously. Without xtrace the globals are OpenTelemetry's
noop implementations: the span APIs are still called everywhere and record nothing.

### Span names and attributes

Each module's span names and key attributes are under "Observability · Tracing" in its README:

| Module | Spans |
|---|---|
| [xgin](../xgin/README.md#tracing) | Inbound, one per request; see also [the X-Trace-Id response header](../xgin/README.md#the-x-trace-id-response-header) and [499](../xgin/README.md#499-aborted-requests) |
| [xecho](../xecho/README.md#tracing) | Inbound, with span names and attributes identical to xgin's and the error attribute called `echo.errors` |
| [xhttp](../xhttp/README.md#tracing) | Outbound |
| [xgorm](../xgorm/README.md#tracing) | One per SQL statement, with attributes following OTel's database semantic conventions |
| [xredis](../xredis/README.md#tracing) | One per command |
| [xcron](../xcron/README.md#tracing) | One root span `cron <name>` per execution |
| [xkafka](../xkafka/README.md#tracing) | `<topic> publish` when producing and `<topic> process` when consuming, with consumption joining the trace from the message headers |
| [xflow](../xflow/README.md#tracing) | Opens no spans |

The status is marked as an error only for a server-side 5xx (xgin, xecho), a failing SQL statement
(xgorm) or a failed scheduled job (xcron); a 4xx is not. For a server-side error the status
description holds the error code alone and `RecordError` is not called (it would write the original
text, parameter values included, into `exception.message`).

### Propagation and the trust boundary

Three things are accepted inbound and sent outbound, under different trust rules:

| | Format | Accepted inbound from | Outbound |
|---|---|---|---|
| Trace identifiers | W3C `traceparent`, `b3` | anyone (they are just an id) | `traceparent` and `b3` are injected |
| `baggage` | W3C `baggage` | **trusted peers only** | the baggage on this process's ctx is sent |
| Forwarded headers | whatever `XTrace.ForwardHeaders` / `ForwardHeaderRules` list | **trusted peers only** | `ForwardHeaders` goes to every downstream, `ForwardHeaderRules` only to matching hosts |

- **Trace identifiers are accepted from anyone, and so is the sampling decision**: the sampler is
  `ParentBased` (see [xtrace "Behavior (measured)"](../xtrace/README.md#behavior-measured)), so a request arriving
  with `sampled=01` is always sampled and exported, beyond the reach of `SampleRatio`. A public
  client sending `traceparent: 00-…-01` on every request fills this hop and every hop below it with
  spans — the cost lands on the tracing backend's storage and export bandwidth. To limit it, strip
  or rewrite inbound `traceparent` at the entrance (the gateway or the load balancer).
- **A trusted peer is one whose directly connected hop is in `XGin.TrustedProxies`** (or
  `XEcho.TrustedProxies` with xecho) — the TCP hop, not the client IP derived from
  `X-Forwarded-For`. `TrustedProxies` trusts private ranges only by default (load balancers,
  Kubernetes ingresses and pods, sidecars), and accepts nothing from a public peer connecting
  directly. "Who counts as one of us" is stated here and nowhere else.
- An untrusted peer arriving with any of these headers produces one warning each, once per process.
- A load balancer usually forwards the client's headers untouched: before writing it into
  `TrustedProxies`, strip those headers there, or the effect is trusting every client again.
- `XGin.Trace: false`, `XEcho.Trace: false` and `XHttp.Trace: false` only turn off the span for that
  hop; all three of the above are still accepted and still sent. With `XTrace.Enable: false` the
  trace identifiers and baggage are no longer propagated, while forwarded headers still are.
- Code calling `Extract` itself, outside xgin / xecho (reading from message queue headers, for
  instance), has to provide a carrier implementing `TrustedPeer() bool` returning `true` to be
  treated as trusted; otherwise baggage and forwarded headers are not accepted at all. xkafka
  treats Kafka message headers as trusted when consuming (whoever can write to a topic holds the
  cluster's credentials); the reasoning and the exceptions are in
  [xkafka "链路"](../xkafka/README.md#tracing).
- To read a forwarded value in business code:
  `xtrace.ForwardHeaderFromContext(ctx, "X-Request-Id")`.
