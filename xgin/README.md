# xgin

A web server: what you get is a native `*gin.Engine`, and `xgin.New()` is the Runnable you hand to
`xone.Run`; listening and graceful shutdown are the framework's business.

- Access logging (credentials masked field by field), tracing, metrics (`/metrics` mounted
  automatically) and panic recovery are already installed
- Private ranges only are trusted as proxies by default (gin's own default trusts everyone): what a
  load balancer, an ingress or a pod forwards in `X-Forwarded-For` is accepted while a public peer
  cannot forge it
- Slow connections have a defense: `ReadHeaderTimeout` 10s, `IdleTimeout` 60s, and a 0 fails startup
- HTTPS, mutual authentication and h2c are all turned on from the config
- Shutdown waits for in-flight requests, then drops connections at the deadline and reports how many
  handlers have not returned
- An aborted request, and one whose client left before a single byte was sent, are recorded as 499
  rather than 200

## Quick start

```yaml
# conf/application.yml
XApp:
  Name: demo.user.api          # the service.name in traces
XGin:
  Port: 8080
  LogSkipPaths: [/healthz]     # health checks are not access-logged
```

```go
import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/jupiter-eng/xone"
	"github.com/jupiter-eng/xone/xgin"
)

func main() {
	xone.MustRun(xgin.New().WithRoutes(func(e *gin.Engine) {
		e.GET("/healthz", func(c *gin.Context) { c.Status(http.StatusNoContent) })

		v1 := e.Group("/api/v1")
		v1.GET("/users/:id", func(c *gin.Context) {
			ctx := c.Request.Context() // cancelled when the client leaves or the server stops; pass it downstream
			slog.InfoContext(ctx, "get user", "user_id", c.Param("id")) // carries this request's trace_id automatically
			c.JSON(http.StatusOK, gin.H{"id": c.Param("id")})
		})
	}))
}
```

The response to `curl localhost:8080/api/v1/users/1` carries `X-Trace-Id`, and
`curl localhost:8080/metrics` shows `http_requests_total` with the `route` label
`/api/v1/users/:id`.

## Configuration

```yaml
XGin:
  Host: "0.0.0.0"
  Port: 8080
  Mode: release            # release / debug / test, release by default; process-wide, so several servers have to agree
  UseH2C: false            # HTTP/2 without TLS (prior knowledge only, no Upgrade: h2c)
  TLS:                     # the server certificate; no Enable, setting CertFile and KeyFile makes it https
    CertFile: ""           # set together with KeyFile, or leave both empty
    KeyFile: ""
    ClientCAFile: ""       # the CA verifying client certificates; setting it means mutual TLS and needs a certificate too
    MinVersion: "1.2"      # "1.2" / "1.3", empty means "1.2"; applies, and is validated, only with a certificate
  ReadHeaderTimeout: 10s   # the main defense against slow-connection attacks, must be > 0
  ReadTimeout: 0s          # unlimited by default: a limit cuts off large uploads
  WriteTimeout: 0s         # unlimited by default: a limit cuts off SSE, long polling and large downloads
  IdleTimeout: 60s         # must be > 0
  MaxMultipartMemory: 8388608  # bytes, 8MB by default; the spill-to-disk threshold, not a request body limit
  TrustedProxies: [private]  # whose X-Forwarded-For and forwarded headers are believed; private = loopback + private ranges, [] = nobody
  Log: true                # the access log
  LogSkipPaths: []         # paths that are not access-logged: trailing / matches by prefix, anything else exactly
  LogQuery: false          # the query string in the access log (masked field by field), off by default
  LogRequestHeaders: false  # request headers in the access log (credentials masked), off by default
  LogRequestBody: false    # the request body in the access log (masked field by field), off by default
  LogResponseHeaders: false  # response headers in the access log (credentials masked), off by default
  LogResponseBody: false   # the response body in the access log, off by default
  Trace: true              # a server span per request, with X-Trace-Id echoed back
  Metric: true             # request metrics, with MetricPath mounted automatically
  MetricPath: /metrics     # must start with /; with Metric on it is added to LogSkipPaths automatically
  ZHTranslations: false    # translate validator's errors into Chinese, see xgin/trans for how to use it
```

**A second server in the same process** (an internal admin port, say) reads no config file: hand it
a complete config with `WithConfig`, starting from `CurrentConfig()`:

```go
c := xgin.CurrentConfig()
c.Host, c.Port = "127.0.0.1", 9090
admin := xgin.New().WithConfig(c).WithRoutes(adminRoutes)
```

- `private` expands to `127.0.0.0/8`, `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`,
  `100.64.0.0/10`, `::1/128` and `fc00::/7`. The list replaces the default wholesale: to add a
  range, write `private` alongside it (`[private, 203.0.113.0/24]`), and a malformed range fails
  startup. When the internal network also holds untrusted clients (an office network or VPN users
  can reach the service directly), do not use `private`: list the exact ranges, or write `[]`.
- `TLS.ClientCAFile` governs the whole port: `/metrics` requires a client certificate too. Inside a
  handler, `c.Request.TLS.PeerCertificates` says who it is.

## API

| Function | What it does |
|---|---|
| `New() *XGin` | Creates a server (a Runnable); it reads nothing, and the config is taken at assembly time |
| `(*XGin).WithRoutes(f ...func(*gin.Engine))` | Registers routes, with the callback receiving the native engine; may be called several times, in call order |
| `(*XGin).WithMiddleware(m ...gin.HandlerFunc)` | Appends custom middleware, after all the built-in middleware |
| `(*XGin).WithRecoverFunc(f gin.RecoveryFunc)` | Customizes the response after a panic; the default is a 500 |
| `(*XGin).WithConfig(c Config)` | Starts the server from this complete config instead of the XGin block of the config file |
| `(*XGin).Engine() *gin.Engine` | Triggers assembly and returns the native engine; `With...` has no effect afterwards |
| `(*XGin).Start(ctx)` / `(*XGin).Stop(ctx)` | Listen / stop gracefully; normally left to `xone.Run` |
| `CurrentConfig() Config` | A copy of the XGin block of the config file, callable any time before `Start` |
| `DefaultConfig() Config` | Every default value; where to start when writing a `WithConfig` from scratch |
| `middleware.AddSensitiveFields(...string)` | Appends sensitive words for the access log (applying to bodies and request headers alike) |
| `middleware.AddSensitiveHeaders(...string)` | Appends sensitive request header names matched exactly |
| `trans.ToZH(err) error` / `trans.Msg(err) string` | Translates validator's errors into Chinese while `ZHTranslations` is on |

## Notes

- **The request body has no limit**: the framework does not set one for the business. To limit it,
  do so in middleware with
  `c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, n)`. `MaxMultipartMemory` is the
  spill-to-disk threshold rather than a limit, and costs about three times its value on the heap.
  See [Behavior (measured)](#behavior-measured).
- **Pass `c.Request.Context()` to slow work inside a handler**: `Stop` has no timeout of its own and
  waits for in-flight requests for at most two thirds of the stop budget (2/3 of
  `xone.WithStopTimeout`, 10s by default), then drops connections and cancels the request ctx. A
  handler ignoring the ctx cannot be stopped, and `Stop` reports `N handler(s) still running`. To
  change it, change `WithStopTimeout`.
- **The access log records only a summary of the request by default** (method, route, status,
  duration, client, sizes); the query string, the request headers, the bodies and the response
  headers each have to be turned on. Before turning them on, add the business's own sensitive
  fields with `middleware.AddSensitiveFields(...)`. See [Access log](#access-log).
- **Settings in a `WithRoutes` callback win over the config** (`e.SetTrustedProxies`, for example),
  but which peers are trusted for forwarded headers comes only from `TrustedProxies` in the config;
  to move both, change the config. The XGin block is read at assembly time (`Engine()` or `Start`).
- An error recorded in a handler with `c.Error(err)` reaches the access log's `errors` field and the
  span; for validator errors in Chinese, turn on `ZHTranslations` and call `trans.ToZH(err)`.
- **Routing differs from gin's defaults**: trailing slashes are not redirected (with only `/users`
  registered, `/users/` is a 404 `unmatched`, while gin answers 301 / 307); a matching path with the
  wrong method answers 405 (gin answers 404). To get gin's redirect, write
  `e.RedirectTrailingSlash = true` inside `WithRoutes` — but a redirected request passes through no
  middleware at all and reaches neither the access log, the metrics nor the trace. See
  [Behavior (measured)](#behavior-measured).
- **A `*gin.Context` can be passed as a ctx directly**: `xlog.AddKV(c, ...)`, `db.WithContext(c)`
  and `otel.Tracer(...).Start(c, ...)` all carry the log scope, the parent span and cancellation
  exactly as passing `c.Request.Context()` would (the engine has `ContextWithFallback` on).

## Behind a load balancer or Cloudflare

Whether a peer is trusted depends on **the directly connected hop** (the TCP peer), not on
`X-Forwarded-For`. A common chain in Kubernetes:

```
user ──▶ Cloudflare ──▶ cloud load balancer ──▶ ingress ──▶ service A ──▶ service B
                          ↑ only Cloudflare's ranges are allowed in (security group)
```

- **Nothing needs configuring inside the services**: A's peer is the ingress and B's peer is A, both
  inside private ranges and trusted by default; headers such as `CF-*` listed in
  `XTrace.ForwardHeaders` are forwarded all the way down.
- **Cloudflare's ranges belong at the cluster entrance only** (the load balancer's security group or
  the ingress allowlist), not in a service's config. Without locking the entrance down, anyone
  bypassing Cloudflare can forge `CF-Connecting-IP`, and past the ingress it reads as "sent by a
  trusted peer".
- `client_ip` is then the address of a Cloudflare edge node (outside the private ranges, where gin
  stops while walking back through `X-Forwarded-For`). For the real user, read the forwarded
  `CF-Connecting-IP`, or have the ingress use it as the real IP (ingress-nginx has a setting for it).
- Only when the origin takes Cloudflare's traffic on a public IP with no load balancer in between
  should Cloudflare's ranges (authoritatively <https://www.cloudflare.com/ips/>) be written into
  `TrustedProxies` alongside `private`.
- Do not set `e.TrustedPlatform = gin.PlatformCloudflare` inside `WithRoutes`: gin then believes the
  `CF-Connecting-IP` in the request unconditionally, regardless of who sent it.

## Observability

### Access log

One line when each request ends, message `request completed`, level INFO (while `XGin.Log` is on;
paths in `LogSkipPaths` and the metrics endpoint are not recorded):

| Field | Content |
|---|---|
| `method` | The request method, as is |
| `route` | The route template, `/users/:id` for instance; `unmatched` when no route matched (consistent with the metrics and the span), with the real path in `path` |
| `path` | The request path, **without the query string** |
| `status` | The status code; an aborted request, or one whose client left before a single byte was sent, is recorded as `499`, see [below](#499-aborted-requests) |
| `elapsed_ms` | The duration in milliseconds, down to the microsecond (`0.051`, for instance) |
| `client_ip` | The client address: the direct peer, or the `X-Forwarded-For` forwarded by a proxy in `XGin.TrustedProxies` |
| `host` | The request's `Host` (`:authority` on HTTP/2), as is |
| `proto` | `HTTP/1.1`, `HTTP/2.0` |
| `user_agent` | `User-Agent`, an empty string when absent |
| `bytes_in` | The `Content-Length` of the request; a chunked upload has no such header and records `-1` |
| `bytes_out` | The response body bytes written, headers excluded; `0` when no body was written; with gin-contrib/gzip configured, this is the compressed (on-wire) count |
| `query` | With `LogQuery: true` and a query string present, masked field by field, by the same rules as a form body |
| `request_headers` | With `LogRequestHeaders: true`, credentials already masked |
| `request_body` | With `LogRequestBody: true`, the first 256KB at most, masked field by field; multipart and `application/octet-stream` record only `omitted` |
| `response_headers` | With `LogResponseHeaders: true`, masked as request headers are (`Set-Cookie` and friends) |
| `response_body` | With `LogResponseBody: true` and a textual response, the first 4KB at most, masked field by field; when the response carries `Content-Encoding` (compressed by a middleware) it records only `[gzip-encoded content omitted]` |
| `errors` | The errors recorded in a handler with `c.Error(...)`, absent when there are none; several are separated by `; ` (`Error #01: a; Error #02: b`); a sensitive word masks the whole thing as `***REDACTED***`, and otherwise only the password inside `postgres://app:pw@db` or `app:pw@tcp(db:3306)` becomes `***REDACTED***` (the span's `gin.errors` works the same way) |
| `trace_id` / `span_id` | When tracing is on |

**Masking** matches sensitive words rather than exact field names: both sides are lowercased with
`_ - .` and spaces removed before comparing, and a field whose name **contains** any sensitive word
is masked.

| | Rule |
|---|---|
| Default sensitive words | `password` `passwd` `secret` `token` `authorization` `apikey` `accesskey` `privatekey` `credential` `cookie` `session` `signature` |
| Bodies (JSON / forms) | A key containing a sensitive word masks its value, at any nesting depth; JSON string values then go through the rules of the `errors` field as well (a sensitive word masks the whole thing, a DSN masks only the password); other form values have only the password in a URL or DSN masked (`https://u:pw@host`); every other value is written back as is |
| Other bodies (plain text, XML, no Content-Type, ...) | Anything that is itself a valid JSON object, array or string is masked as JSON (`ShouldBindJSON` ignores the Content-Type); for the rest no field can be located, so a sensitive word masks the whole thing and without one only the password in a URL or DSN is masked |
| Request / response headers | A name on the list (`Authorization` `Proxy-Authorization` `Cookie` `Set-Cookie` `X-Api-Key` `X-Auth-Token`, plus whatever `AddSensitiveHeaders` added) **or** a name containing a sensitive word is masked; other values have only the password inside `postgres://app:pw@db` or `app:pw@tcp(db:3306)` masked, and are not masked wholesale by sensitive word: `Vary: Cookie` and `Access-Control-Allow-Headers: Authorization` are recorded as is |
| Headers whose value is a URL | `Referer`, `X-Original-URL`, `X-Original-URI`, `X-Rewrite-URL`, `X-Forwarded-URI`, and the response headers `Location`, `Content-Location`, `Refresh`: everything after `?` and `#` is dropped, then the userinfo after `://` (`https://tok@host` is recorded as `https://host`) |

**A structural limit**: JSON is matched by key name only. In a name-value array such as
`[{"name":"password","value":"x"}]` the password sits in `value`, whose key holds no sensitive word,
so `x` reaches the log as is (the value `password` under `name` is masked). For a body of that
shape, either add the key to the word list with `AddSensitiveFields("value")` or do not record the
body.

`middleware.AddSensitiveFields(...)` appends sensitive words (applying to bodies and request headers
alike) and `middleware.AddSensitiveHeaders(...)` appends exact header names. Case is compared by
Unicode folding, consistent with how `encoding/json` matches field names. The word list
deliberately leaves out `auth`, `key` and `pwd`: they would match `author`, `Idempotency-Key` and
random strings.

A panic is recorded by the Recover middleware as `panic while handling request` (ERROR, with
`error`, `stack`, `path` and `method`; `error` is masked by the rules of the `errors` field, and
`stack` holds only function names and line numbers and is recorded as is) and answered with a 500.
A write failing because the client disconnected early is recorded as `connection broken` (WARN: it
is not the service's failure and should not fire alerts), with no stack. When the response has
already started (a status code went out) and a panic follows, the status code is not changed: the
access log, the metrics and the trace record what was already sent (usually a 200), `errors` is
empty and the span is not marked as an error, leaving that panic log line as the only sign.

### Logs

| Message | Level | Fields |
|---|---|---|
| `xgin listening` | INFO | `addr`, `tls`, `mtls`, `h2c`; printed only after the certificate is read and the port is bound |
| `xgin http server error` | WARN | `error`: the line net/http reported (`http.Server.ErrorLog`), such as `http: TLS handshake error from 203.0.113.9:1234: EOF` |
| `xgin received the shutdown signal before starting, the server will not start` | WARN | The stop signal arrived before listening |
| `xgin invalid config, assembling the engine with defaults; Start will return the error` | WARN | `error`; with an invalid config `Engine()` assembles from the defaults and `Start` returns that error |

The global logging conventions (`trace_id` injection, `xlog.AddKV`, the framework's start and stop
lines) are in [`docs/observability.md`](../docs/observability.md#logs).

### Metrics

| Metric | Type | Labels | Source |
|---|---|---|---|
| `http_requests_total` | counter | `method`, `route`, `status` | xgin, `XGin.Metric` |
| `http_request_duration_seconds` | histogram | `method`, `route`, `status` | xgin, with buckets from `XMetric.HTTPDurationBuckets` |

- **`route`** is the route template; when no route matched it is `unmatched` rather than the real
  path.

The metric name prefix, the constant labels and a few general rules are in
[`docs/observability.md` "Metrics"](../docs/observability.md#metrics).

### Tracing

| Source | Span name | Key attributes |
|---|---|---|
| xgin (inbound) | `GET /users/:id`: the method plus the route template; unmatched is `GET unmatched` | `http.request.method` (folded), `http.request.method_original` (when the original differs from the folded one), `http.route`, `url.path`, `http.response.status_code`, `gin.errors` |

A 5xx and an abort are marked as errors; a 4xx is not. While `Metric` is on, a scrape of
`MetricPath` opens no span and echoes no `X-Trace-Id`: the scraper polls it every few seconds, the
same reason the access log skips it.

The whole picture of tracing, propagation and the trust boundary is in
[`docs/observability.md` "Tracing"](../docs/observability.md#tracing).

### The X-Trace-Id response header

While xgin's `Trace` is on, every response carries `X-Trace-Id: <32-character trace id>`, leading
from one call straight to its trace. With `XTrace.Enable: false` the span is a noop and there is no
trace id to echo back; with `XGin.Trace: false` none is echoed either.

### 499: aborted requests

Two kinds of request have their status code recorded as **499** in the access log, the metrics and
the trace (borrowed from nginx's client closed request). The code is never sent to a client:

- A handler that aborted with `panic(http.ErrAbortHandler)` (which is also what
  `httputil.ReverseProxy` does when the upstream goes away mid-forward): the connection is simply
  broken, the response has usually written a 200 header already, and reading `c.Writer.Status()` as
  is would record a truncated response as a success in all three places. The span is marked as an
  error and the access log's `errors` carries `net/http: abort Handler`.
- A client that is already gone (a disconnect, a reset HTTP/2 stream, or a forced disconnect at the
  shutdown deadline — net/http cancels the request's ctx) with not one byte of response written:
  otherwise it would be recorded as a 200 with `bytes_out` 0 while the client received nothing. The
  span is **not** marked as an error: this is not the server's fault.

A response that has already started is recorded with the status code that was sent: the client
received at least part of it. "The client is gone" is judged from the request's ctx as the
middleware saw it on the way in — a ctx installed by an inner layer that reaches its deadline or is
cancelled does not count (the client is still waiting).

## Behavior (measured)

The measurement environment and the cross-module summary are in
[`docs/behavior.md`](../docs/behavior.md).

Gin v1.12.0, Go 1.25 net/http.

**`TrustedProxies`**: gin defaults to `0.0.0.0/0`, so anyone sending `X-Forwarded-For: 1.2.3.4`
decides `client_ip`. Here only private ranges are trusted by default: for a service exposed directly
to the internet the peer is a public address, and `X-Forwarded-For` still cannot change `client_ip`.
How to configure it behind a proxy is under
[Behind a load balancer or Cloudflare](#behind-a-load-balancer-or-cloudflare). An IPv4-mapped IPv6
spelling fails startup: measured, gin v1.12.0 parses `::ffff:10.0.0.1` wrongly and ends up trusting
addresses like `::1` and `::5` while not trusting `10.0.0.1` itself; `::ffff:10.0.0.0/104` gin parses
correctly, but the trust check for forwarded headers then disagrees. So only IPv4 spellings such as
`10.0.0.1` and `10.0.0.0/8` are accepted.

**`MaxMultipartMemory`** is not a request body limit, it is the threshold above which data spills to
disk: what exceeds it is written to a temporary file rather than rejected. The real cost is about
three times this number: a 60MB upload at 32MB (gin's default) makes the heap grow by 96MB in that
parsing step, by 24MB at 8MB and by 3MB at 1MB.

**A `ReadHeaderTimeout` / `IdleTimeout` of 0** falls back to `ReadTimeout` in net/http (itself 0 by
default), which means no limit at all: measured, a connection sending half a request header is never
closed. Both must therefore be > 0.

**`UseH2C`** uses the standard library's `Protocols.SetUnencryptedHTTP2` rather than x/net's
`h2c.NewHandler`: the latter hijacks the connection, `Shutdown` returns nil in about 60µs and
in-flight requests keep running. Only h2c with prior knowledge is accepted (gRPC,
`curl --http2-prior-knowledge`), and an `Upgrade: h2c` handshake gets an ordinary HTTP/1.1 response.
What is handed to net/http is the engine itself, not `engine.Handler()`: setting gin's own
`e.UseH2C = true` inside `WithRoutes` has no effect (it wraps `h2c.NewHandler`, with the problem
above), and h2c is governed by `XGin.UseH2C` alone.

**`RedirectTrailingSlash`** is on by default in gin: with only `/users` registered, `GET /users/`
answers 301 outright (`Location: /users`) and POST answers 307, without running the middleware chain
at all — no access log, no metrics, no trace. Here it is off, so `/users/` goes to `NoRoute` like
any other unmatched path and is recorded as a 404 `unmatched`. `RedirectFixedPath` (redirecting
after fixing case and `..`) is already off in gin and was left alone. To redirect, set
`e.RedirectTrailingSlash = true` back inside `WithRoutes`.

**`ContextWithFallback`** is off by default in gin: a `*gin.Context` passed as a `context.Context`
then has `Value` looking only at `c.Keys`, `Done()` returning nil and `Err()` always nil, so
`xlog.AddKV(c, ...)` never reaches the access log, a span opened with `Start(c, ...)` has no parent,
and a client that left cannot cancel anything downstream. Here it is on and all of those go to
`c.Request.Context()`.

**`MetricPath`**: gin does not reject a path without a leading `/`, it quietly rewrites it:
`metrics` is registered as `/metrics` while the access log fails to skip it, and an empty one is
mounted on the root path `/`, where gin panics inside the business's own routing code when a home
page is registered later. So both fail while the config is read.

**`Mode`** is set before `gin.New`: in debug mode `gin.New` prints a warning and then one line per
route registered afterwards. `gin.SetMode` is process-wide.

**TLS and mutual authentication** (e2e, `TLS.MinVersion: "1.3"`): a certificate signed by the CA in
`TLS.ClientCAFile` gives `200`, `HTTP/2.0`, `TLS 1.3`; without a certificate the client reports
`remote error: tls: certificate required`; with a certificate signed by another CA, Go's client does
not present it at all and the result is the same; a client capped at TLS 1.2 reports
`protocol version not supported`; plaintext HTTP sent to that port is answered by net/http with
`400 Client sent an HTTP request to an HTTPS server.`. None of these ever reached a handler.
`TLS.MinVersion` defaults to 1.2 and Go 1.25's own server default is 1.2 as well, and it is still
written explicitly: defaults change with Go versions.

**Graceful shutdown**:

- `http.Server.Shutdown` only returns an error on timeout while in-flight connections keep running.
  So `Shutdown` is given only part of the time before the deadline (leaving 20% of what remains, at
  most 1s), and whatever is still running at that point gets `Close()`, which drops every connection.
- Dropping a connection does not mean the handler returned: `Close()` only closes connections and
  cancels the request ctx. The part left over is used to wait for handlers to really return, and
  whatever has not returned by the deadline is named in the error
  (`N handler(s) still running when the shutdown deadline passed`).
- Calling `Stop` on its own with a ctx that has no deadline waits until every in-flight request is
  done (measured, a 1.5s request made `Shutdown` return after 1.57s).
- A hijacked connection (a WebSocket) is beyond `Shutdown` / `Close()`, while its handler still
  counts as "not returned". So after `Shutdown` / `Close()` return, the root of every request ctx is
  cancelled (`http.Server.BaseContext`): a WebSocket read loop or a long poll watching
  `c.Request.Context()` can then exit. Measured, a handler that hijacked the connection and waits on
  the request ctx makes `Stop` return nil in about 10ms. A handler that ignores the ctx and only
  blocks on `conn.Read` still cannot be stopped — it has to close the connection itself when the ctx
  is cancelled.
- `Start` works again after a failed listen (a taken port, an unreadable certificate): the failed
  attempt does not count as running.

## Troubleshooting

| Error text | Cause | What to do |
|---|---|---|
| `TLS.CertFile and TLS.KeyFile must both be set or both be empty` (XGin) | Only half of the server certificate is configured | Set both, or leave both empty |
| `TLS.ClientCAFile requires TLS.CertFile and TLS.KeyFile, mutual TLS runs on top of TLS` (XGin) | Mutual TLS is configured without a server certificate | Add `TLS.CertFile` / `TLS.KeyFile` |
| `unknown TLS.MinVersion="1.1", supported: 1.2 / 1.3` (XGin) | A version that is not accepted, or written as `TLS1.3` | Write `"1.2"` or `"1.3"` |
| `read TLS.ClientCAFile: ...` / `TLS.ClientCAFile ... contains no PEM certificate` (XGin) | The CA file cannot be read, or holds no PEM certificate | Check the path and the contents; the server does not listen |
| `field CertFile not found in type xgin.Config (did you mean TLS.CertFile? move it under TLS:)` (same for `KeyFile`, `ClientCAFile`, `MinVersion`) | Written at the top level of `XGin`, while these belong to the `TLS:` block | Indent those lines into the `TLS:` block, see [Configuration](#configuration) |
| `TrustedProxies entry "::ffff:10.0.0.1" is an IPv4-mapped IPv6 address; write it as 10.0.0.1` (XGin) | An IPv4-mapped IPv6 address or range in `TrustedProxies` | Write it as the message says: `::ffff:10.0.0.1` → `10.0.0.1`, `::ffff:10.0.0.0/104` → `10.0.0.0/8` |

The `N handler(s) still running when the shutdown deadline passed` on shutdown is covered in
[`docs/troubleshooting.md` "Runnable and shutdown"](../docs/troubleshooting.md#runnable-and-shutdown); client-side
TLS errors are in [xtls "Troubleshooting"](../xtls/README.md#排错).
