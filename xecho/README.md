# xecho

A web server: what you get is a native `*echo.Echo` ([Echo](https://echo.labstack.com/) v4), and
`xecho.New()` is the Runnable you hand to `xone.Run`; listening and graceful shutdown are the
framework's business. It is the same thing as [xgin](../xgin/README.md) with the framework
swapped: the access log fields, the metric names, the span names, which client_ip is believed and
whether shutdown waits for handlers are identical on both sides (they live in `internal/web`,
which both share).

- Access logging (credentials masked field by field), tracing, metrics (`/metrics` mounted
  automatically) and panic recovery are already installed
- Errors returned by a handler are rendered into a response inside the middleware: 404, 405 and
  500 are recorded in the access log, the metrics and the trace as the status code and byte count
  the client really received
- The built-in middleware sits on `e.Pre`: requests rejected or redirected in your own `e.Pre`
  still reach the access log, the metrics and the trace, and a panic in Pre is caught
- `c.RealIP()` takes the client's real IP from `X-Forwarded-For` / `X-Real-IP`, but only from a
  trusted proxy: private ranges only by default (echo's own default believes everyone), so what a
  load balancer, an ingress or a pod forwards is accepted while a public peer cannot forge it
- Slow connections have a defense: `ReadHeaderTimeout` 10s, `IdleTimeout` 60s, and a 0 fails
  startup (all four on echo's own `e.Server` are 0)
- HTTPS, mutual authentication and h2c are all turned on from the config
- Shutdown waits for in-flight requests, then drops connections at the deadline and reports how
  many handlers have not returned
- Echo's own log (gommon, writing to stdout by default) is routed into slog

## Quick start

```yaml
# conf/application.yml
XApp:
  Name: demo.user.api          # the service.name in traces
XEcho:
  Port: 8080
  LogSkipPaths: [/healthz]     # health checks are not access-logged
```

```go
import (
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/jupiter-eng/xone"
	"github.com/jupiter-eng/xone/xecho"
)

func main() {
	xone.MustRun(xecho.New().WithRoutes(func(e *echo.Echo) {
		e.GET("/healthz", func(c echo.Context) error { return c.NoContent(http.StatusNoContent) })

		v1 := e.Group("/api/v1")
		v1.GET("/users/:id", func(c echo.Context) error {
			ctx := c.Request().Context() // cancelled when the client leaves or the server stops; pass it downstream
			slog.InfoContext(ctx, "get user", "user_id", c.Param("id")) // carries this request's trace_id automatically
			return c.JSON(http.StatusOK, map[string]string{"id": c.Param("id")})
		})
	}))
}
```

The response to `curl localhost:8080/api/v1/users/1` carries `X-Trace-Id`, and
`curl localhost:8080/metrics` shows `http_requests_total` with the `route` label
`/api/v1/users/:id`.

## Configuration

```yaml
XEcho:
  Host: "0.0.0.0"
  Port: 8080
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
  TrustedProxies: [private]  # whose X-Forwarded-For / X-Real-IP and forwarded headers are believed; private = loopback + private ranges, [] = nobody
  Debug: false             # echo's debug switch (per instance): error responses carry the error text and c.JSON / c.XML always indent; keep it off in production
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
```

Compared with the XGin block, three fields are missing and writing them fails startup: `Mode`,
`MaxMultipartMemory` and `ZHTranslations`. Echo has no process-wide run mode (`Debug` is per
instance, so two servers in one process can differ); the multipart spill threshold is fixed (see
[Behavior (measured)](#behavior-measured)); and validation is not built into echo, so install one
with [`WithValidator`](#api), with translated messages being that library's business.

**A second server in the same process** (an internal admin port, say) reads no config file: hand
it a complete config with `WithConfig`, starting from `CurrentConfig()`:

```go
c := xecho.CurrentConfig()
c.Host, c.Port = "127.0.0.1", 9090
admin := xecho.New().WithConfig(c).WithRoutes(adminRoutes)
```

- `private` expands to `127.0.0.0/8`, `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`,
  `100.64.0.0/10`, `::1/128` and `fc00::/7`. The list replaces the default wholesale: to add a
  range, write `private` alongside it (`[private, 203.0.113.0/24]`), and a malformed range fails
  startup. When the internal network also holds untrusted clients (an office network or VPN users
  can reach the service directly), do not use `private`: list the exact ranges, or write `[]`.
- `TLS.ClientCAFile` governs the whole port: `/metrics` requires a client certificate too. Inside
  a handler, `c.Request().TLS.PeerCertificates` says who it is.
- xgin, xecho and xbeego can be used in one process: their config blocks are separate (`XGin` /
  `XEcho` / `XBeego`), just keep the ports apart.

## API

| Function | What it does |
|---|---|
| `New() *XEcho` | Creates a server (a Runnable); it reads nothing, and the config is taken at assembly time |
| `(*XEcho).WithRoutes(f ...func(*echo.Echo))` | Registers routes, with the callback receiving the native echo; may be called several times, in call order |
| `(*XEcho).WithMiddleware(m ...echo.MiddlewareFunc)` | Appends custom middleware (`e.Use`, after the router) behind every built-in one; it applies to every request, `/metrics` and 404s included |
| `(*XEcho).WithRecoverFunc(f func(echo.Context, any) error)` | Customizes the response after a panic; the error it returns goes to `e.HTTPErrorHandler`, and the default is a 500 |
| `(*XEcho).WithValidator(v echo.Validator)` | Installs a validator, after which a handler can call `c.Validate(&req)`; without one, `c.Validate` returns `echo.ErrValidatorNotRegistered` |
| `(*XEcho).WithConfig(c Config)` | Starts the server from this complete config instead of the XEcho block of the config file |
| `(*XEcho).Engine() *echo.Echo` | Triggers assembly and returns the native echo; `With...` has no effect afterwards |
| `(*XEcho).Start(ctx)` / `(*XEcho).Stop(ctx)` | Listen / stop gracefully; normally left to `xone.Run` |
| `CurrentConfig() Config` | A copy of the XEcho block of the config file, callable any time before `Start` |
| `DefaultConfig() Config` | Every default value; where to start when writing a `WithConfig` from scratch |
| `middleware.AddSensitiveFields(...string)` | Appends sensitive words for the access log (applying to bodies and request headers alike; one word list shared with xgin) |
| `middleware.AddSensitiveHeaders(...string)` | Appends sensitive request header names matched exactly |

## Notes

- **Do not call `e.Start` / `e.StartTLS`**: the server is started by `Start` (which `xone.Run`
  calls for you), and the timeouts, TLS, h2c and graceful shutdown all live there; the four
  timeouts on `e.Server` are 0 and unused.
- **Returning an error from a handler is enough**:
  `return echo.NewHTTPError(http.StatusNotFound, "no such user")` or a plain `error`. The response
  is written by `e.HTTPErrorHandler` (echo's JSON `{"message":...}` by default, with a plain error
  always a 500 and no text); to change the format, set `e.HTTPErrorHandler` inside `WithRoutes`.
  The error text goes into the access log's `errors` and the span's `echo.errors`, and a sensitive
  word (password, token, ...) masks the whole thing, while a password inside a URL or MySQL DSN
  becomes `***REDACTED***`. A custom error handler is called exactly once; when the response has
  already gone out (`c.Response().Committed`) and an error is returned, echo's default error
  handler does nothing.
- **The request body has no limit**: the framework does not set one for the business. To limit it,
  use echo's own middleware, for instance `e.Use(echomw.BodyLimit("10M"))` (`echomw` being
  `github.com/labstack/echo/v4/middleware`). The multipart spill threshold is fixed at 32MB, see
  [Behavior (measured)](#behavior-measured).
- **Pass `c.Request().Context()` to slow work inside a handler**: `Stop` has no timeout of its
  own and waits for in-flight requests for at most two thirds of the stop budget (2/3 of
  `xone.WithStopTimeout`, 10s by default), then drops connections and cancels the request ctx.
  A handler ignoring the ctx cannot be stopped, and `Stop` reports `N handler(s) still running`.
- **Settings in a `WithRoutes` callback win over the config** (`e.IPExtractor`,
  `e.HTTPErrorHandler`, `e.Validator`), but which peers are trusted for forwarded headers comes
  only from `TrustedProxies` in the config; to move both, change the config. The XEcho block is
  read at assembly time (`Engine()` or `Start`).
- **Routing**: trailing slashes match strictly (`/a/` is a 404, as in xgin); with only GET
  registered, HEAD is a 405 (xgin too); a path parameter at the end of a route swallows the
  slashes after it (`/users/:id` matches `/users/1/z` with `id` being `1/z`; xgin answers 404).
  These are echo's behaviors and the framework does not change them; to relax trailing slashes,
  use `e.Pre(echomw.RemoveTrailingSlash())`.
- **The middleware order**, outermost first:
  `LogScope -> Trace -> Log -> Metric -> Recover ->` your `e.Pre` `-> router ->` `WithMiddleware` /
  your `e.Use` `-> handler`. The built-ins sit on `e.Pre`, outside your own: a request rejected in
  Pre (`return echo.ErrUnauthorized`) or redirected there (the 301 of
  `RemoveTrailingSlashWithConfig`) still reaches the access log, the metrics and the trace and
  carries `X-Trace-Id`, and a panic in Pre is caught and answered with a 500. The router has not
  run yet, so `route` is recorded as `unmatched`. Middleware that needs `c.Path()`, `c.Param()` or
  the request ctx (`trace_id`, `xlog.AddKV`) goes on `e.Use` or `WithMiddleware`: your `e.Pre`
  sees the request's original ctx, see [Behavior (measured)](#behavior-measured).
  `e.Pre(echomw.MethodOverride())`, `RemoveTrailingSlash` and `Rewrite` all work as usual, and
  what is recorded is the rewritten method.
- **Use `echomw.ContextTimeout` for timeouts, not `echomw.Timeout`**: the latter (deprecated by
  echo) runs the handler in another goroutine and writes a 503 to the original writer itself.
  Measured, the client receives a 503 while the access log, the metrics and the trace record a 200
  with `bytes_out` 0, and under `-race` it reads and writes the same response as the access log
  middleware. `ContextTimeout` only puts a deadline on the request; the handler returns on
  `c.Request().Context()` and it turns that into a 503, which is what gets recorded.
- **Install a validator yourself**: echo has no built-in validation the way gin does. Without one,
  `c.Validate(&req)` returns `echo.ErrValidatorNotRegistered` (a plain error, not an
  `*echo.HTTPError`: returning it directly gives the client a 500
  `{"message":"Internal Server Error"}`) rather than a validation result — a service that forgot
  looks perfectly fine. Which validation library to use is the business's call, and the framework
  only installs it on `e.Validator`:

  ```go
  // github.com/go-playground/validator/v10
  type structValidator struct{ v *validator.Validate }

  func (s structValidator) Validate(i any) error { return s.v.Struct(i) }

  xecho.New().WithValidator(structValidator{v: validator.New()}).WithRoutes(register)
  ```

  `c.Validate` hands whatever `Validate` returns to the handler unchanged, and the handler decides
  (`return echo.NewHTTPError(http.StatusBadRequest, err.Error())` is what makes it a 400; doing
  nothing makes it a 500). An `e.Validator` set in a `WithRoutes` callback wins over it.
- **Keep `Debug` off in production**: `XEcho.Debug: true` lands on `e.Debug`, so error responses
  carry the error text (`{"message":...,"error":"..."}`) and `c.JSON` / `c.JSONP` / `c.XML` always
  indent (the same as every request carrying `?pretty`, with `bytes_out` growing accordingly).
  It is off by default; the measured byte counts are under
  [Behavior (measured)](#behavior-measured).

## Behind a load balancer or Cloudflare

Exactly as in xgin, see
[xgin "Behind a load balancer or Cloudflare"](../xgin/README.md#behind-a-load-balancer-or-cloudflare). The
client_ip algorithm is the same too: with the direct peer trusted, look at `X-Forwarded-For` first
and `X-Real-IP` second, walking right to left for the first address not in `TrustedProxies`.
Echo's own `ExtractIPFromXFFHeader` trusts loopback, link-local and private ranges by its own
fixed rules, which differ from these; do not swap it in from a callback unless those rules are
exactly what you want.

## Observability

### Access log

One line when each request ends, message `request completed`, level INFO (while `XEcho.Log` is on;
paths in `LogSkipPaths` and the metrics endpoint are not recorded). The field names, their order,
their values and the masking rules are identical to xgin's (the same implementation), and the full
description is in [xgin "Access log"](../xgin/README.md#access-log):

| Field | Content |
|---|---|
| `method` | The request method, as is |
| `route` | The route template, `/users/:id` for instance; an unmatched route (404), a wrong method (405), an OPTIONS echo answered itself, anything landing on a `RouteNotFound` fallback (including the ones a group registers quietly, see [Behavior (measured)](#behavior-measured)) and anything that ended inside `e.Pre` are all `unmatched`, with the real path in `path` |
| `path` | The request path, **without the query string** |
| `status` | The status code the client received (after an error returned by the handler was rendered); an aborted request, or one whose client left before a single byte was sent, is recorded as `499`, see [below](#499-aborted-requests) |
| `elapsed_ms` | The duration in milliseconds, down to the microsecond |
| `client_ip` | `c.RealIP()`: the direct peer, or the address forwarded by a proxy in `XEcho.TrustedProxies` |
| `host` / `proto` / `user_agent` | The request's `Host`, `HTTP/1.1` / `HTTP/2.0`, and `User-Agent` |
| `bytes_in` | The `Content-Length` of the request; a chunked upload records `-1` |
| `bytes_out` | The response body bytes written (`c.Response().Size`), headers excluded; error responses count too. With `echomw.Gzip` installed this is the byte count **before** compression (xgin with gin-contrib/gzip records the compressed one), see [Behavior (measured)](#behavior-measured) |
| `query` / `request_headers` / `request_body` / `response_headers` / `response_body` | Each when its own switch is on, masked by the same rules as xgin; when the response carries `Content-Encoding` (compressed by `echomw.Gzip`), `response_body` records only `[gzip-encoded content omitted]` |
| `errors` | The error returned by the handler (`err.Error()`, such as `code=404, message=Not Found`), or the reason for an abort or a broken connection; absent when there is none; a sensitive word masks the whole thing as `***REDACTED***`, and otherwise only the password inside `postgres://app:pw@db` or `app:pw@tcp(db:3306)` becomes `***REDACTED***` (the span's `echo.errors` works the same way) |
| `trace_id` / `span_id` | When tracing is on |

`middleware.AddSensitiveFields(...)` / `AddSensitiveHeaders(...)` write to the same table as
xgin's identically named functions, so either will do.

A panic is recorded by the Recover middleware as `panic while handling request` (ERROR, with
`error`, `stack`, `path` and `method`; `error` is masked by the rules of the `errors` field, and
`stack` holds only function names and line numbers and is recorded as is) and answered with a 500,
and a panic inside `e.Pre` works the same way. When the response has already started (a status
code went out) and a panic follows, the status code is not changed: the access log, the metrics
and the trace record what was already sent (usually a 200), `errors` is empty and the span is not
marked as an error, leaving that panic log line as the only sign (xgin behaves identically).

A write failing after the client disconnected early is **returned** by echo rather than panicked:
there is one access log line as usual, `status` is what was already sent (usually 200), `errors`
is `write tcp …: write: broken pipe` (or `connection reset by peer`), and no stack is printed. The
error response can no longer be written, and echo's error handler sees `Committed` and does
nothing. Only when a handler panics with such an error is `connection broken` recorded (WARN, no
stack, the same rules as xgin).

### Logs

| Message | Level | Fields |
|---|---|---|
| `xecho listening` | INFO | `addr`, `tls`, `mtls`, `h2c`; printed only after the certificate is read and the port is bound |
| `echo internal log` | Echo's level (ERROR only, by default) | `message`: the line echo wrote itself, such as `write: broken pipe` when an error response fails to go out |
| `xecho http server error` | WARN | `error`: the line net/http reported, such as `http: TLS handshake error from 203.0.113.9:1234: EOF`; what `e.StdLogger` writes lands here too |
| `xecho received the shutdown signal before starting, the server will not start` | WARN | The stop signal arrived before listening |
| `xecho invalid config, assembling the engine with defaults; Start will return the error` | WARN | `error`; with an invalid config `Engine()` assembles from the defaults and `Start` returns that error |

The global logging conventions are in
[`docs/observability.md`](../docs/observability.md#logs).

### Metrics

The same names, labels and buckets as xgin, so dashboards and alerts work for both:

| Metric | Type | Labels | Source |
|---|---|---|---|
| `http_requests_total` | counter | `method`, `route`, `status` | xecho, `XEcho.Metric` |
| `http_request_duration_seconds` | histogram | `method`, `route`, `status` | xecho, with buckets from `XMetric.HTTPDurationBuckets` |

- **`route`** is the route template; anything unmatched is `unmatched`, by the same rules as the
  access log. `method` folds into the standard methods, and anything else is recorded as `OTHER`.
- When one process uses more than one of xgin / xecho / xbeego, their requests land on the same metrics and cannot be
  told apart; to separate them, give them different route prefixes.

### Tracing

| Source | Span name | Key attributes |
|---|---|---|
| xecho (inbound) | `GET /users/:id`: the method plus the route template; unmatched is `GET unmatched` | `http.request.method` (folded), `http.request.method_original` (when the original differs from the folded one), `http.route`, `url.path`, `http.response.status_code`, `echo.errors` (the error returned by the handler) |

A 5xx and an abort are marked as errors; a 4xx is not. While `XEcho.Trace` is on, every response
(including an error response echo rendered) carries `X-Trace-Id`. While `Metric` is on, a scrape
of `MetricPath` opens no span and echoes no `X-Trace-Id`, as in xgin. The whole picture of
tracing, propagation and the trust boundary is in
[`docs/observability.md` "Tracing"](../docs/observability.md#tracing).

### 499: aborted requests

Two kinds of request are recorded as 499 in the access log, the metrics and the trace, by the same
rules as [xgin "499: aborted requests"](../xgin/README.md#499-aborted-requests):

- A handler that aborted with `panic(http.ErrAbortHandler)` (the span is marked as an error).
  **Returning** an error wrapping it (`fmt.Errorf("proxy: %w", http.ErrAbortHandler)`) does not
  count: the connection was never broken, the client received the 500 `HTTPErrorHandler` rendered,
  and 500 is recorded;
- A request whose client is already gone with not one byte of response sent (the span is not
  marked as an error): the handler conventionally does `return ctx.Err()`, which would otherwise
  be rendered into a 500 nobody will receive. Echo writing an error response to a broken
  connection still sets `Committed` and records `Status` 500 (measured), so the decision is made
  before handing it to `HTTPErrorHandler`.

## Behavior (measured)

The measurement environment and the cross-module summary are in
[`docs/behavior.md`](../docs/behavior.md). Echo v4.16.0, gommon v0.5.0, Go 1.25 net/http.

**client_ip (`e.IPExtractor`)**: echo's default is `nil`, which makes `c.RealIP()` fall back to
the old behavior — whoever sends `X-Forwarded-For` or `X-Real-IP` is believed. Measured, a public
peer `203.0.113.9` sending `X-Forwarded-For: 1.2.3.4` makes `c.RealIP()` return `1.2.3.4`, and
`X-Real-IP: 5.6.7.8` alone makes it `5.6.7.8`. Here an `IPExtractor` is installed from
`TrustedProxies` (private ranges only by default), with the algorithm copied from gin v1.12.0's
`ClientIP`; xgin's tests compare the two side by side. An IPv4-mapped IPv6 spelling
(`::ffff:10.0.0.1`, `::ffff:10.0.0.0/104`) fails startup for the reason given in
[xgin](../xgin/README.md#behavior-measured): write `10.0.0.1` and `10.0.0.0/8`.

**An error returned by a handler** is only written into a response by `e.HTTPErrorHandler` after
the whole middleware chain has returned (the last step of `Echo.ServeHTTP`). At the moment an
outer middleware receives the error, 404, 405 and 500 all measure as `Status 200`,
`Committed false`, `Size 0` — taken at face value, every failing request would appear in the
access log, the metrics and the trace as a 200 with `bytes_out` 0. So the three built-in
middlewares call `c.Error(err)` in their own layer and return `nil` outwards: a 404 is
`{"message":"Not Found"}` at 24 bytes, a 405 is `{"message":"Method Not Allowed"}` at 33 bytes
with `Allow: OPTIONS, GET`, a plain error is a 500 `{"message":"Internal Server Error"}` at 36
bytes (the error text stays out of the response), and `echo.NewHTTPError(418)` is
`{"message":"I'm a teapot"}` at 27 bytes, all of them `application/json` — those are the numbers
the access log records. `HTTPErrorHandler` runs exactly once; returning the error as well makes
echo call it again, and a custom error handler that does not check `Committed` writes the error
response twice. When a handler writes a 200 and then returns an error, echo's default error
handler sees `Committed` and does nothing, the client receives that 200, and the log records 200.

**`e.Debug`** (`XEcho.Debug`, false by default) is read in exactly three places in echo v4.16.0's
source, two of which affect what the caller sees:

- The default error handler (`DefaultHTTPErrorHandler` in `echo.go`, and only when `Message` is a
  string) puts the error text into the response. For a plain `error`, off it is
  `{"message":"Internal Server Error"}` at 36 bytes and on it is the 60 bytes of

  ```json
  {
    "error": "boom",
    "message": "Internal Server Error"
  }
  ```

- `c.JSON` / `c.JSONP` / `c.XML` (`c.echo.Debug || pretty` in `context.go`) indent by two spaces,
  the same as every request carrying `?pretty`: `{"a":"b"}` goes from 10 bytes to 15, and the
  access log's `bytes_out` records the larger number.

The third place is inside `e.Start` / `e.StartTLS`: raising gommon's level to DEBUG. `e.Start` is
not used here, so `applyConfig` does that part itself; otherwise `Debug: true` would still show
ERROR only, and echo's one log line below ERROR (`response already committed` in `response.go`,
WARN) would stay invisible.

**The validator**: `e.Validator` is `nil` by default, and `c.Validate(&req)` then returns
`echo.ErrValidatorNotRegistered` (`errors.New("validator not registered")`, a plain error) without
looking at `req` at all: a handler returning it produces a 500
`{"message":"Internal Server Error"}` at 36 bytes, with the text reaching only the access log's
`errors`. What `WithValidator` installs lands on `e.Validator` after the config and before
`WithRoutes`, and setting it in a callback wins over it.

**Route templates**: with no route matching the path, `c.Path()` is empty; when the path matches
and the method does not, `c.Path()` is that route's template and the handler is
`MethodNotAllowedHandler` (405), while for OPTIONS echo answers 204 with `Allow` itself. The
latter two are recorded as `unmatched` just like a 404, which matches xgin (gin has no template in
either case).

**A group's fallback routes**: `g.Use(...)` and `e.Group("/api/v1", mw)` quietly register two
`RouteNotFound` routes for the group (their `Method` in `e.Routes()` is `echo.RouteNotFound`, that
is `echo_route_not_found`): `/api/v1` and `/api/v1/*`, so that group middleware applies to 404s
under the group as well; an `e.Group` without middleware registers none. So the `c.Path()` of
`GET /api/v1/nope` is `/api/v1/*`; a wrong method lands on the fallback too — it wins over 405, so
`POST /api/v1/users` (with only GET registered) answers **404** rather than 405, which is echo's
behavior and is left alone. Anything landing on a fallback (including one registered with
`e.RouteNotFound`) is recorded as `unmatched`; when a fallback and a real route share a template
(`g.GET("/*", proxy)` inside a group), the method hitting that route still records the template
`/api/v1/*`. The fallback table is taken once from `e.Routes()` (together with the host-specific
ones in `e.Routers()`) when the first request arrives. `e.Static("/static", dir)` registers a real
GET route `/static*` which answers 404 itself when the file is missing, and records `/static*`:
the cardinality stays bounded.

**`e.Pre`**: when Pre middleware exists, `Echo.ServeHTTP` wraps the router innermost in the Pre
chain, so by the time `next(c)` returns for a built-in middleware mounted on Pre, `c.Path()` and
`c.Handler()` are set and the `Allow` of a 405 is already on c. The router matches on the
`*http.Request` as it arrived (captured in the closure), not on `c.Request()`: swapping in an
`r.WithContext` copy inside Pre makes a later `e.Pre(echomw.MethodOverride())` change the `Method`
on the copy, and measured, `POST` plus `X-HTTP-Method-Override: PUT` walked into the POST handler.
Mutating the request in place is no better: net/http's contract says a handler does not modify the
request it was handed, and it is still a data race once the handler passes the request to a
goroutine outliving it. So the built-in middleware touches no request inside Pre and attaches the
log scope and the span to a ctx stored in `echo.Context`: the access log, the metrics, the span
and the panic log all use it, so a request that ended inside Pre still carries `trace_id`; after
routing and before `WithMiddleware`, the usual `c.SetRequest(r.WithContext(ctx))` swaps it onto
the request (one copy per request). The cost is that **your `e.Pre` sees the request's original
ctx**: logs written there carry no `trace_id`, `xlog.AddKV` does not reach the access log, and a
value put into the request ctx inside `e.Pre` is not visible in the handler (two ctx trees cannot
be merged, and what is preserved here is the span and the log scope). Middleware that needs the
ctx goes on `e.Use` / `WithMiddleware`.

**Timeout middleware**: `echomw.Timeout` (deprecated) with 50ms and a handler sleeping 200ms: the
client receives a 503 and a page of HTML, the outer middleware reads `Status 200` and `Size 0`,
and `-race` reports a data race. `echomw.ContextTimeout` returns `echo.ErrServiceUnavailable`,
which `HTTPErrorHandler` renders into `{"message":"Service Unavailable"}` and records as 503; it
cancels the ctx it wrapped in a defer when it returns, so outside that layer the request ctx is
`Canceled` — deciding whether "the client left" uses the ctx as the built-in middleware saw it on
the way in, which that does not affect.

**Panics** are not caught by echo: measured, the client reads `EOF` (net/http breaks the
connection) and the stack goes to stderr through net/http
(`http: panic serving 127.0.0.1:…: boom`), bypassing slog; `e.Use(echomw.Recover())` does not
catch a panic inside `e.Pre` either (measured, also an EOF). The Recover here sits on Pre, records
one ERROR log line and answers 500.

**Echo's own log** (gommon) writes to `os.Stdout` by default in its own JSON: measured, a failing
error response write (the client is already gone) leaves
`{"time":…,"level":"ERROR","prefix":"echo","file":"echo.go","line":"492","message":"write: broken pipe"}`.
The level is ERROR by default, so a `Warn("response already committed")` never comes out, while
the `Print` family ignores the level and always writes. Here it is routed into slog (message
`echo internal log`), keeping echo's level. `e.StdLogger` is bound in `echo.New` to `e.Logger`'s
output at that moment (`os.Stdout`), and replacing `e.Logger`'s output does not reach it; it is
what echo prepares for `http.Server.ErrorLog` (`e.Start` uses it that way), and here it is routed
where the server's `ErrorLog` goes (`xecho http server error`, WARN).

**After the client disconnects**: the handler waits for the request ctx to be cancelled and then
calls `c.Error(ctx.Err())`; before the call `Committed` is false and `Status` is 200, and after it
`Committed` is true, `Status` is 500 and `Size` is 36 — recorded that way even when the write
failed. Writing 1MB chunks to a broken connection through `c.Response().Write` measured 3963 bytes
written before `write tcp …: write: broken pipe` came back, with `Committed` true and `Status` 200.

**Compression**: `echomw.Gzip` replaces `c.Response().Writer` with its gzip writer, and
`c.Response().Size` counts the bytes handed to it: a 12000-byte `c.String` is 77 bytes on the wire
while `bytes_out` records 12000. xgin with gin-contrib/gzip v1.2.8 records the 77 on the wire (it
replaces `c.Writer`, and the access log reads the one below it). Making xecho record the on-wire
count too would mean wrapping another counting writer under Gzip for every request, which the
framework does not do. The writer capturing the response body sits outside Gzip, so what it
captures is compressed bytes and `response_body` records only
`[gzip-encoded content omitted]`.

**The writer capturing the response body** (the one swapped into `c.Response().Writer` while
`LogResponseBody: true`) implements `http.Flusher`, `http.Hijacker` and `Unwrap`: older code that
type-asserts directly, `c.Response().Flush()` and `http.ResponseController`'s read and write
deadlines all reach the real connection; when the writer below does not support Hijack it returns
`http.ErrNotSupported`, as net/http does.

**The server**: the `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout` and `IdleTimeout` of
`e.Server` are all 0, and the banner is printed only by `e.Start`. `e.Start` is not used here; the
server is built from the config by `web.Server`, shared with xgin, and the measurements for
timeouts, TLS, h2c and graceful shutdown are in
[xgin "Behavior (measured)"](../xgin/README.md#behavior-measured): a handler that hijacked the connection (a
WebSocket) also sees the request ctx cancelled on shutdown, and `Start` works again after a failed
listen.

**`echo.Context` is not a `context.Context`** (it has no `Deadline` / `Done` / `Err`) and cannot
be passed to a function expecting one: always pass `c.Request().Context()`.

**`MetricPath`**: echo does not reject a path without a leading `/`: `metrics` is registered as
`/metrics` while the access log fails to skip it, and an empty one is registered on the root path
`/`, where a home page registered later silently replaces one or the other (with no error and no
panic). So both fail while the config is read.

**The request body**: there is no limit, and a 64MB body measured fine. `c.FormValue`,
`c.FormFile` and `c.Bind` parse multipart with a fixed 32MB spill threshold
(`ParseMultipartForm(32 << 20)`, with no config field): a 60MB upload measured 128MB allocated
(`TotalAlloc`) in that parsing step, which becomes 32MB after a middleware calls
`c.Request().ParseMultipartForm(8 << 20)` first — a request is parsed only once and whoever calls
first decides. The framework does not do that step: parsing early would break a handler streaming
the upload through `c.Request().MultipartReader()`.

**The response byte count**: `c.Response().Size` counts the body written through `c.Response()`:
`c.String(200, "hello")` is 5; `c.JSON` adds one newline (`{"a":"b"}` records 10); three writes
each followed by `Flush` of `chunk` is 15; `c.NoContent(204)` is 0 with `Status` 204. Anything
written straight to `c.Response().Writer`, bypassing `c.Response()`, does not count. A `?pretty`
query parameter makes `c.JSON` indent its output (`{"a":"b"}` becomes 15 bytes).

## Troubleshooting

| Error text | Cause | What to do |
|---|---|---|
| `TLS.CertFile and TLS.KeyFile must both be set or both be empty` (XEcho) | Only half of the server certificate is configured | Set both, or leave both empty |
| `TLS.ClientCAFile requires TLS.CertFile and TLS.KeyFile, mutual TLS runs on top of TLS` (XEcho) | Mutual TLS is configured without a server certificate | Add `TLS.CertFile` / `TLS.KeyFile` |
| `unknown TLS.MinVersion="1.1", supported: 1.2 / 1.3` (XEcho) | A version that is not accepted, or written as `TLS1.3` | Write `"1.2"` or `"1.3"` |
| `read TLS.ClientCAFile: ...` / `TLS.ClientCAFile ... contains no PEM certificate` (XEcho) | The CA file cannot be read, or holds no PEM certificate | Check the path and the contents; the server does not listen |
| `field CertFile not found in type xecho.Config (did you mean TLS.CertFile? move it under TLS:)` (same for `KeyFile`, `ClientCAFile`, `MinVersion`) | Written at the top level of `XEcho`, while these belong to the `TLS:` block | Indent those lines into the `TLS:` block, see [Configuration](#configuration) |
| `TrustedProxies entry "::ffff:10.0.0.1" is an IPv4-mapped IPv6 address; write it as 10.0.0.1` (XEcho) | An IPv4-mapped IPv6 address or range in `TrustedProxies` | Write it as the message says: `::ffff:10.0.0.1` → `10.0.0.1`, `::ffff:10.0.0.0/104` → `10.0.0.0/8` |
| A 500 `{"message":"Internal Server Error"}` with `validator not registered` in the access log's `errors` | A handler called `c.Validate` without a validator installed | `xecho.New().WithValidator(...)`, see [Notes](#notes) |
| `field Mode not found in type xecho.Config` (same for `MaxMultipartMemory`, `ZHTranslations`) | An XGin block was copied over | Remove those fields, the reasoning is under [Configuration](#configuration) |

The `N handler(s) still running when the shutdown deadline passed` on shutdown is covered in
[`docs/troubleshooting.md` "Runnable and shutdown"](../docs/troubleshooting.md#runnable-and-shutdown).
