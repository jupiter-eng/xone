# xbeego

A web server for a [beego](https://github.com/beego/beego) v2 application: the routes stay where
they are -- on beego's global `web.BeeApp`, registered with `web.Router` / `web.InsertFilter` as
they always were -- and `xbeego.New()` is the Runnable you hand to `xone.Run`. Listening and
graceful shutdown become the framework's business.

The access log fields, the metric names, the span names, which `client_ip` is believed and whether
shutdown waits for handlers are identical to [xgin](../xgin/README.md)'s and
[xecho](../xecho/README.md)'s: they live in `internal/web`, which all three share.

- Access logging (credentials masked field by field), tracing, metrics (`/metrics` served
  automatically) and panic recovery are already installed
- The `route` label and the span name use beego's **route template** (`/users/:id`), not the real
  path, so the series count follows the number of routes rather than the number of ids
- The built-in middleware is plain `net/http` and sits **outside** beego's router: a request that
  never reaches a controller still gets an access log, a metric and a span
- `client_ip` comes from `X-Forwarded-For` / `X-Real-IP` but only from a trusted proxy, private
  ranges only by default (beego's own `ctx.Input.IP()` believes whoever sent the header)
- Panics are caught **inside** beego, where the stack still points at the controller: one ERROR
  line through `slog` with `trace_id`, the panic value masked, and the stack
- Beego's own `logs` package is routed into `slog`, so its lines are JSON like everything else
- Slow connections have a defense: `ReadHeaderTimeout` 10s, `IdleTimeout` 60s, and a 0 fails
  startup (beego sets only `ReadTimeout` / `WriteTimeout`, from one `ServerTimeOut`, 0 by default)
- HTTPS, mutual authentication and h2c are all turned on from the config
- Shutdown waits for in-flight requests, then drops connections at the deadline and reports how
  many handlers have not returned

> **For API services.** `web.Run` is not called, so beego's sessions, HTML templates, static-file
> MIME types, gzip, the admin server and its default error pages are not set up. See
> [Notes](#notes).

## Quick start

```yaml
# conf/application.yml
XApp:
  Name: demo.user.api          # the service.name in traces
XBeego:
  Port: 8080
  LogSkipPaths: [/healthz]     # health checks are not access-logged
```

```go
package main

import (
	"log/slog"

	"github.com/beego/beego/v2/server/web"

	"github.com/jupiter-eng/xone"
	"github.com/jupiter-eng/xone/xbeego"

	_ "myapp/routers" // registers the routes on web.BeeApp from its own init, unchanged
)

func main() {
	xone.MustRun(xbeego.New())
}

// A controller is unchanged, except that the ctx to pass downstream is the request's.
type UserController struct{ web.Controller }

func (c *UserController) Get() {
	ctx := c.Ctx.Request.Context() // cancelled when the client leaves or the server stops
	slog.InfoContext(ctx, "get user", "user_id", c.Ctx.Input.Param(":id")) // carries this request's trace_id
	c.Data["json"] = map[string]string{"id": c.Ctx.Input.Param(":id")}
	_ = c.ServeJSON()
}
```

The response to `curl localhost:8080/api/v1/users/1` carries `X-Trace-Id`, and
`curl localhost:8080/metrics` shows `http_requests_total` with the `route` label
`/api/v1/users/:id`.

**What to delete when moving an existing service over**: the `web.Run(...)` call and whatever
surrounded it (a `signal.Notify` block, a hand-written `http.Server`), and the application's own
access-log filters -- leaving those in place writes every request twice, with two different sets of
field names.

## Configuration

```yaml
XBeego:
  Host: "0.0.0.0"
  Port: 8080               # the only listen setting that counts; beego's own httpport is unused
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
  Log: true                # the access log; delete the application's own filters when turning this on
  LogSkipPaths: []         # paths that are not access-logged: trailing / matches by prefix, anything else exactly
  LogQuery: false          # the query string in the access log (masked field by field), off by default
  LogRequestHeaders: false  # request headers in the access log (credentials masked), off by default
  LogRequestBody: false    # the request body in the access log (masked field by field), off by default
  LogResponseHeaders: false  # response headers in the access log (credentials masked), off by default
  LogResponseBody: false   # the response body in the access log, off by default
  Trace: true              # a server span per request, with X-Trace-Id echoed back
  Metric: true             # request metrics, with MetricPath served automatically
  MetricPath: /metrics     # must start with /; with Metric on it is added to LogSkipPaths automatically
  RecoverPanic: true       # take beego's recovery slot over (process-wide, settled by the first assembly)
  BeegoLogs: true          # route beego's own logs package into slog (process-wide, settled by the first assembly)
```

**A second server in the same process** (an internal admin port, say) reads no config file: hand
it a complete config with `WithConfig`, starting from `CurrentConfig()`:

```go
c := xbeego.CurrentConfig()
c.Host, c.Port = "127.0.0.1", 9090
admin := xbeego.New().WithConfig(c)
```

Both serve the same routes: beego has one global router, see [Notes](#notes). One thing does not
carry over to the second instance: `web.InsertFilterChain` registrations made in its `WithRoutes`
callback. Beego folds filter chains into its router in `Handlers.Init()`, which is not idempotent
and therefore runs once per process, during the first assembly -- so register filter chains from an
`init` or before the first server is assembled.

- `private` expands to `127.0.0.0/8`, `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`,
  `100.64.0.0/10`, `::1/128` and `fc00::/7`. The list replaces the default wholesale: to add a
  range, write `private` alongside it (`[private, 203.0.113.0/24]`), and a malformed range fails
  startup. When the internal network also holds untrusted clients (an office network or VPN users
  can reach the service directly), do not use `private`: list the exact ranges, or write `[]`.
- `TLS.ClientCAFile` governs the whole port: `/metrics` requires a client certificate too. Inside
  a controller, `c.Ctx.Request.TLS.PeerCertificates` says who it is.
- `RecoverPanic` and `BeegoLogs` are process-wide because the things they take over are
  (`web.BConfig.RecoverFunc` and beego's one logger). **The first assembly settles both**, and an
  instance assembling later with a different value gets a `WARN` saying its own values were
  ignored. They are settled once rather than per instance because beego reads both while requests
  are being served -- `BConfig.RecoverFunc` on every request, and its logger's adapter list without
  holding a lock -- so a second server writing them while the first is already answering is a data
  race. Starting the second server from `CurrentConfig()`, as above, keeps them identical anyway.
- **Routes and filters are still registered before the first server starts.** Settling those two
  switches takes the config out of the way, not beego's router: `WithRoutes` on an instance
  assembled while another is already answering writes the same route tree beego is reading, which
  is a data race of its own. Assemble every instance first, then start them -- which is what
  `xone.Run` does with the hooks it is given.
- `RecoverPanic` replaces the slot beego calls, not its switch, and a panic is caught either way,
  since what escapes beego is caught outside it. `recoverpanic` in `conf/app.conf` decides one
  thing while this is on: an abort onto a registered error handler is handed back to the recovery
  that stood in the slot before, which is the only code that can render a handler registered under
  a **name** rather than a status code. With `recoverpanic` off that recovery declines and such a
  response carries no body; a numeric code is rendered through `web.Exception` and is unaffected.
- **`conf/app.conf` is still beego's.** xone reads `conf/application.yml`, beego reads
  `conf/app.conf`, and the two do not overlap except on the listen address, where `XBeego.Port`
  wins because `web.Run` is never called.

## API

| Function | What it does |
|---|---|
| `New() *XBeego` | Creates a server (a Runnable); it reads nothing and touches no global, and the config is taken at assembly time |
| `(*XBeego).WithRoutes(f ...func(*web.HttpServer))` | Runs a registration callback during assembly, after the config has been read; optional, since routes registered on `web.BeeApp` from an `init` are served anyway |
| `(*XBeego).WithMiddleware(m ...func(http.Handler) http.Handler)` | Appends custom middleware behind every built-in one and outside beego; this is beego's own `web.MiddleWare` shape, so middleware written for `web.RunWithMiddleWares` moves over unchanged. It applies to every request, `/metrics` included |
| `(*XBeego).WithRecoverFunc(f func(http.ResponseWriter, *http.Request, any))` | Customizes the response after a panic; the default is a bare 500. Per instance, although beego's slot for it is not |
| `(*XBeego).WithConfig(c Config)` | Starts from this config instead of the `XBeego` block, for a second server in one process |
| `(*XBeego).App() *web.HttpServer` | Beego's global `web.BeeApp`, for anything this package does not cover; triggers assembly |
| `(*XBeego).Handler() http.Handler` | The whole chain, built-in middleware outermost and beego's router innermost -- what `Start` serves and what `httptest` can drive; triggers assembly |
| `CurrentConfig() Config` | A copy of the `XBeego` block, readable any time before `Start` |
| `ClientIP(r *http.Request) string` | The `client_ip` the framework computed for this request, which is what the access log, the metrics and the span go by. `middleware.ClientIP` is the same function, for middleware used on its own |
| `middleware.AddSensitiveFields(...)` / `AddSensitiveHeaders(...)` | Adds to the masking word list; one list per process, shared with xgin and xecho |

## Notes

**Beego's API is global, so this module binds to that global.** `web.Router` and
`web.InsertFilter` are package-level functions registering on the single `web.BeeApp`, and an
application's routers package registers into it from its own `init`. A server built beside that
would serve none of those routes, so `XBeego` serves `web.BeeApp.Handlers` itself. `New` stays
pure -- it touches nothing -- and the binding happens during assembly. Two `XBeego` instances in
one process therefore serve the same routes on two ports; they do not have separate routers.

**`web.Run` is not called.** The server is `internal/web.Server`, the one xgin and xecho use, which
is what makes the timeouts, h2c, TLS and the graceful shutdown identical on all three. Beego's
`Run` also performs its own one-time setup (`initBeforeHTTPRun`, unexported), and skipping it
leaves these unavailable:

| Not set up | Consequence |
|---|---|
| Sessions (`web.BConfig.WebConfig.Session.SessionOn`) | `c.GetSession` / `c.SetSession` have no manager |
| HTML templates and the views directory | `c.TplName` renders nothing |
| Static-file MIME types | Only relevant when serving static files from beego |
| Gzip (`web.BConfig.EnableGzip`) | Responses are not compressed by beego |
| The admin server | `web.BConfig.Listen.EnableAdmin` does nothing |
| Beego's default error pages | An unmatched route answers the status code with the code itself as the body, see [Behavior (measured)](#behavior-measured) |
| **`web.AddAPPStartHook` callbacks** | `initBeforeHTTPRun` is the only thing that runs them, so a callback registered this way **never runs** -- with no error and no log. An application initialising its ORM or its cache that way has to call it itself, from `main` or from a `WithRoutes` callback |

For a JSON API none of those is in use. A service that needs one of them should keep `web.Run` and
take logging alone -- a blank `import _ "github.com/jupiter-eng/xone/xlog"` gives it JSON logs with
`service_name` / `service_version` / `host_name` and nothing else changes.

**Middleware runs outside beego's router**, so `WithMiddleware` sees no beego context and no route
template. Anything that needs them is a filter: `web.InsertFilter(pattern, web.BeforeRouter, f)` as
before.

**The request body limits stay beego's**, and they are generous (`server/web/router.go`,
v2.3.8): `web.BConfig.MaxMemory`, 64MB by default, is both the ceiling on a non-upload body and the
threshold `ParseMultipartForm` spills to disk above, while `web.BConfig.MaxUploadSize`, 1GB by
default, caps an upload. Both come from `conf/app.conf` (`maxmemory`, `maxuploadsize`) -- `XBeego`
has no field for either, since reading the body is beego's job here and not the framework's.

That threshold is worth a look before accepting uploads: the parsing step costs roughly three times
it in heap (measured for the same `ParseMultipartForm` call under
[`XGin.MaxMultipartMemory`](../xgin/README.md#configuration)), so at 64MB a handful of concurrent
uploads is gigabytes. The three integrations do not agree on this number -- xgin defaults to 8MB
and makes it a config field, xecho is stuck with echo's fixed 32MB, beego's is 64MB -- which is
worth knowing when a service moves between them.

**The route template travels on the request.** The template only exists inside beego, so xbeego
registers one filter at `web.BeforeExec` that copies it onto the request for the layers outside to
read. `BeforeExec` rather than `FinishRouter`: a panic in a controller unwinds straight to beego's
recovery and the later filter positions never run, so with `FinishRouter` every panicking request
was recorded as `route="unmatched"` -- losing exactly the requests worth finding.

**Importing beego has two side effects at `init`.** Its `web` package reads `conf/app.conf` from
the working directory and panics when it cannot be parsed, which happens before xone reads anything
-- so such an error is not reported with the file and line the framework's own configuration errors
get. With no `conf/app.conf` at all, beego prints one line and carries on, see
[Troubleshooting](#troubleshooting).

Keeping an `app.conf` is worth it for that reason alone: `httpport` in it is ignored (`web.Run` is
never called, so `XBeego.Port` decides), but its presence keeps that one plain-text line out of the
logs. The e2e service does exactly this, see `e2e/service-beego`.

And its `grace` package registers two command-line flags, `-graceful` and `-socketorder`. They do
nothing here (`web.Run`, the only thing that reads them, is never called), but they show up in the
program's `-h` output, and **a program that defines a flag of either name panics at startup** with
`flag redefined`. xone itself is unaffected: it reads `--config` and `--profile` by scanning
`os.Args` and never touches `flag.CommandLine`.

## Observability

### Access log

One `INFO` line per request, message `request completed`. Measured, with `service_name` / `host_name` /
`process_pid` added by xlog and `trace_id` / `span_id` by xtrace:

```json
{"time":"2026-10-22T11:20:27.944388+08:00","level":"INFO","msg":"request completed",
 "service_name":"smoke.beego.api","host_name":"host-1","process_pid":66236,
 "method":"GET","route":"/users/:id","path":"/users/1","status":200,"elapsed_ms":0.071,
 "client_ip":"127.0.0.1","host":"127.0.0.1:18099","proto":"HTTP/1.1","user_agent":"curl/8.7.1",
 "bytes_in":0,"bytes_out":6,
 "trace_id":"1779a5c31fe5b0927c2f10992bec419c","span_id":"ded7b799ad329a69"}
```

| Field | What it is |
|---|---|
| `route` | The route template; `unmatched` when beego matched nothing |
| `path` | The real path, **never** the query string -- `GET /login?token=...` would put the credential into the log |
| `status` | What really went out; `499` when the client left or the handler aborted |
| `elapsed_ms` | Milliseconds, to the microsecond |
| `client_ip` | Computed from `TrustedProxies`, not beego's `ctx.Input.IP()` |
| `bytes_in` | The request's `Content-Length`; `-1` for a chunked upload |
| `bytes_out` | Response body bytes, headers excluded |
| `errors` | The panic text, masked; left out when there is none |

`query`, `request_headers`, `response_headers`, `request_body` and `response_body` are added by
their own switches. Masking is the same word list xgin and xecho use: a field whose name contains
a sensitive word, the listed credential headers, and URLs lose their query string. In the panic
text (and on the span's `beego.errors`) a credential is located by the name in front of it, so
`password=hunter2` is masked while `token expired` is kept whole; the full rules are in
[xgin "Access log"](../xgin/README.md#access-log). The request body is buffered up to 256KB and the
response body recorded up to 4KB; an upload and a compressed response are recorded as omitted.

### Logs

- `xbeego listening` at startup, with `addr`, `tls`, `mtls` and `h2c`
- `panic while handling request` (ERROR) with `error`, `stack`, `path` and `method`
- `connection broken` (WARN) -- a client's flaky network, not the service's fault
- `xbeego an error handler registered under a name was not rendered ...` (WARN) with `code` and
  `path` -- see [Troubleshooting](#troubleshooting)
- `xbeego RecoverPanic and BeegoLogs are process-wide and were settled by the first assembly ...`
  (WARN) with `recover_panic` and `beego_logs`, what stands, and `ignored_recover_panic` and
  `ignored_beego_logs`, what this instance asked for
- `xbeego http server error` (WARN) -- `error`: the line net/http reported through
  `http.Server.ErrorLog`, such as `http: TLS handshake error from 203.0.113.9:1234: EOF`
- `beego internal log` -- beego's own `logs` package, with its text under `message`, its level
  folded onto slog's four, and `source` giving the call site. Turn it off with `BeegoLogs: false`.
  These lines carry no `trace_id`: beego's `logs` API has no ctx to take one from

### Metrics

| Metric | Labels |
|---|---|
| `http_requests_total` (counter) | `method`, `route`, `status` |
| `http_request_duration_seconds` (histogram) | `method`, `route`, `status` |

`method` is folded into a fixed set (anything else becomes `OTHER`) and `route` is the template, so
neither dimension grows without bound. `XMetric.Namespace` prefixes both.

### Tracing

One server span per request, named `<METHOD> <route>`, with `http.request.method`, `http.route`,
`url.path`, `http.response.status_code` and -- for a folded method -- `http.request.method_original`.
A panic is recorded under `beego.errors`, masked. Only 5xx and aborts mark the span as an error.
The response carries `X-Trace-Id` back.

The span name is set on the way **out**: this layer sits outside beego's router, so the template
does not exist when the span starts.

## Behavior (measured)

Measured on beego v2.3.8, Go 1.25.

| What | Beego's default | What xbeego does |
|---|---|---|
| `web.BConfig.RecoverPanic` | `true`, recovering with `logs.Critical` -- plain text, no `trace_id`, no masking | `RecoverFunc` replaced (`RecoverPanic: true`), so the stack is still the controller's |
| `c.Abort("404")` / `c.CustomAbort` / `ctx.Abort` | Stop a handler by panicking: with `web.ErrAbort` once the response is written, or with the code as a string when an error handler is registered for it | Both kept: `ErrAbort` is swallowed, and a registered **numeric** code is rendered through `web.Exception`. Neither is logged as a crash |
| `c.Abort("<name>")` onto a handler registered under a **name** (`web.ErrorController` turns `ErrorDb` into `"Db"`) | Renders the handler; the status is `ctx.Output.Status`, which `c.Abort` leaves at **200** because a name is not a code | The same, by handing the request back to the recovery beego had before: `web.Exception` takes a numeric code and `errorInfo`'s fields are unexported, so nothing exported reaches such a handler. With `recoverpanic` off that recovery declines, and the status then goes out without the body, as one `WARN` naming the code |
| `web.BConfig.EnableErrorsShow` | `true` | Read, and what makes the branch above live |
| `ctx.Output.Status` after an abort | Beego's recovery answers with it, falling back to 500 | The same. A bare 500 would turn every `ctx.Abort(403, …)` into a 500 |
| `web.BConfig.Log.AccessLogs` | `false`, so beego writes no access log of its own | Nothing to switch off; the framework's is the only one |
| `web.BConfig.RunMode` | `PROD`, so beego's per-request dev line is not printed | Untouched; `conf/app.conf` still decides |
| `ctx.Input.IP()` | Walks `X-Forwarded-For` **from the left** and believes whoever sent it | Not used; `client_ip` comes from `TrustedProxies`. `xbeego.ClientIP(r)` gives a controller the same answer |
| Timeouts | One `ServerTimeOut` (0 by default) sets `ReadTimeout` and `WriteTimeout`; nothing sets `ReadHeaderTimeout` or `IdleTimeout` | All four come from the config, two of them must be > 0 |
| Unmatched path | -- | **404**, no `Content-Type`, body `404` (beego's default error pages are not registered) |
| Known method with no matching route (`POST /ping` where only `GET` exists) | -- | **404**, body `404` -- beego matches pattern and method together, so this is not a 405 |
| Unknown method (`BREW /ping`) | -- | **405**, body `405` |

**The module graph** is the largest of the three web integrations. An empty application importing
only this module has 109 modules in its graph, 105 of them third-party, against xecho's 63 / 59
(measured the way [architecture.md](../docs/architecture.md#2-every-integration-is-its-own-go-module)
describes). What is actually **built** is far less: 38 third-party modules and 109 packages, 10 of
them beego's. The gap is beego's own `go.mod`: 38 direct requirements (75 lines counting the
indirect ones) -- MySQL, SQLite, etcd, Casbin, Elasticsearch and the rest -- for packages this
module never imports. They stay in the module graph, and so in `go.sum` and in a vulnerability
scan, while Go's pruning keeps every one of them out of the binary.

## Troubleshooting

| What you see | What it means |
|---|---|
| `init global config instance failed. If you do not use this, just ignore it. open conf/app.conf: no such file or directory` | Beego's `core/config` package `init` writing to **stderr** when there is no `conf/app.conf` (`core/config/ini.go`). Harmless -- xone reads `conf/application.yml`, not this file -- but it is one plain-text line that no logging configuration can intercept, so a collector expecting JSON will flag it. A beego application has an `app.conf`; adding one, even two lines of it, silences the message |
| Every request logged twice, with two different sets of field names | The application's own access-log filters are still registered. Remove them, or set `Log: false` |
| Every request logged twice, the second line in beego's own format | `accesslogs = true` in `conf/app.conf`: beego writes its own line as well. Turn it off there, since `XBeego.Log` does not govern it |
| `xbeego an error handler registered under a name was not rendered ...`, and the response has the right status but no body | Only beego's own recovery reaches a handler registered under a **name** rather than a status code (`web.Exception` takes a numeric code, and `errorInfo`'s fields are unexported), and `recoverpanic = false` in `conf/app.conf` switched that recovery off, so it declined the request. Turn `recoverpanic` back on -- `RecoverPanic` replaces the slot beego calls, not that switch, and a panic is caught either way -- or register the handler under its status code. `web.ErrorController` registers every method it finds under a name, turning `ErrorDb` into `"Db"`. Note the status: `c.Abort("Db")` leaves it at **200**, since a name is not a code |
| `xbeego RecoverPanic and BeegoLogs are process-wide and were settled by the first assembly` | A second server assembled with a different `RecoverPanic` / `BeegoLogs`. Both are beego globals read while requests are served, so the first assembly settles them, see [Notes](#notes). Give both instances the same values -- starting the second from `CurrentConfig()` does that |
| `route="unmatched"` on requests that clearly matched | A filter registered at `web.BeforeExec` **before** xbeego's answered the request on the spot, so the template was never copied. Filters that answer requests belong at `web.BeforeRouter` |
| `c.GetSession` returns nothing / `c.TplName` renders nothing | `web.Run` is not called, so beego's session manager and template engine were never set up, see [Notes](#notes) |
| `XBeego.Port` ignored, the service listens on beego's `httpport` | `web.Run` is still being called somewhere. `xone.MustRun(xbeego.New())` replaces it |
| `cannot reach ...` lines still plain text on stderr | `BeegoLogs: false`, or the application calls `logs.SetLogger(...)` after `xone.Run` has assembled |
| `xbeego: listen on 0.0.0.0:8080 failed: address already in use` | Another process has the port, or `web.Run` is still running beside this server |
| `XBeego.MetricPath="/metrics" is already registered as a route by the application` | The application registers a beego route on the path the framework serves the metrics endpoint on. The endpoint is answered in front of beego's router, so that route would never be reached; it fails startup instead of being shadowed without a word. Move one of the two, or set `Metric: false`. A pattern that merely happens to match (`/:page`) is left alone |
