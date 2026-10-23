# Changelog

This file records the **caller-visible changes** of every version: new features, behavior changes,
incompatible changes, fixes and performance. Refactors and tooling changes inside the repository
are left out, unless they change what a caller has to do.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the version numbers
follow [Semantic Versioning](https://semver.org/). Every module shares one version number (see
`scripts/release.sh`).

## [Unreleased]

### Fixed

- **A Kafka handler returning an error type that cannot be compared no longer takes the process
  down.** Deciding whether a message had been interrupted compared the error `xutil.Retry` handed
  back against the last one the handler returned, and comparing two interface values panics outright
  when their dynamic type is not comparable — `validator.ValidationErrors` (`[]FieldError`) and
  mongo-driver's `WriteException` are two in common use. The panic came from inside the partition's
  goroutine, where nothing caught it, so the first message to use up its retries killed the service.
  The outcome now goes by the stopping context, which is what "interrupted" means, and nothing is
  inferred from an error value. The same comparison also took an `xutil.Permanent(err)` carrying such
  a type for a shutdown, so that message was neither dead-lettered nor committed.
- **One partition giving up no longer silently stops every partition of that consumer.** A
  partition's goroutine used to exit while still listed in the consumer's partition table, and the
  poll loop kept dispatching to it: it filled that queue and then blocked for good, so no further
  fetch and no rebalance happened for any partition, while the member went on heartbeating and
  logged nothing. A goroutine that exits now leaves the table, and a batch with nowhere to go is
  dropped — none of it was marked, so it comes back after the next rebalance.
- **A panic in this module's own consuming code is contained.** Only the handler call was protected
  before; everything the framework does around it (marking, metrics, dead-lettering) could take the
  whole process down. It is now recorded as `kafka partition worker panicked, this partition stops
  until the next rebalance` (ERROR, with the stack) and costs that one partition until the next
  rebalance, as every other lifecycle entry point in the framework already behaved.
- **`XONE_DEBUG` masks a value under an `Authorization` or `Signature` key.** The word list it goes
  by was missing both, so a credential under either was printed in the clear while `Password` and
  `ApiKey` beside it were masked. It stays shorter than the access log's list by `cookie` and
  `session` on purpose: there a word is matched against a request field, here against a
  configuration key, and those two would hide `CookieDomain`, `SessionTimeout` and beego's
  `SessionOn`.
- **A command-line flag followed by another flag is reported instead of swallowing it.**
  `./app --config --profile=dev` read `--profile=dev` as the configuration path and failed with
  `config file does not exist: --profile=dev`, pointing at a file nobody named;
  `./app --profile --config=/etc/a.yml` went looking for
  `application---config=/etc/a.yml.yml`. Both now report `--config needs a value`, which was
  already what a flag written last reported. A value that really begins with a `-` is written as
  `--config=-weird.yml`.
- **`yaml:",inline"` on a field that is neither a struct nor a map says so.** The strict decoding
  reaches the field before yaml does, and what came out was reflect's
  `NumField of non-struct type int`, which names neither the field nor the tag; yaml's own sentence
  was never reached. It now raises the same one yaml does,
  `option ,inline may only be used on a struct or map field`.
- **The global logger is no longer replaced on shutdown when nothing it writes to was closed.** xlog
  is closed last while logs keep coming after it (the stop hooks abandoned at the deadline, the
  requests still in flight), and the replacement exists so those lines do not disappear into a
  closed file. It used to happen whatever was configured: with `XLog.Console: true` — the default,
  and with no file at all — those last lines lost their `trace_id`, their `xlog.AddKV` fields and
  the configured `Level` / `Format` / `Timezone`, and moved from stdout to stderr, which a collector
  reading only stdout does not see. With no output configured at all, "I want no logs" turned into
  logs on stderr the moment the service stopped. File output alone is unchanged: there the
  replacement is what keeps those lines at all.
- **`xmetric.CounterAdd` with a negative value no longer panics inside the caller.** client_golang
  panics on it (`counter cannot decrease in value`); it is now one `ERROR` log line and nothing
  recorded, as every other unrecordable metric in that package already was.
- **The warning `Install` writes when metrics were recorded on a previous registry says which case
  it is.** A second `Install` in the process (another `xone.Run`, or a caller installing by hand)
  was reported as `metrics were recorded before xmetric was initialized`, sending whoever read it
  looking for a startup problem that was not there.
- **`xlog.Observer` documents that the record must not be modified.** Copies of a `slog.Record`
  share state, and the fields the context carries are appended to that same record right after the
  observers have run.

## [v1.3.0] - 2026-10-22

### Changed

- **A route of the application's own on `MetricPath` now fails startup** in xgin, xecho and xbeego,
  with one sentence naming the field:
  `XGin.MetricPath="/metrics" is already registered as a route by the application` (incompatible — a
  service that had such a route and relied on whatever its framework did with it will not start
  until one of the two moves). The three frameworks each resolved that collision differently, two of
  them silently: gin panicked on the duplicate, out of the caller's own routing code and with
  nothing in the message pointing at the field; echo let the later registration win, so the metrics
  endpoint disappeared while the path kept being left out of the access log and the tracing (both go
  by `MetricPath`); beego never saw the request at all, the endpoint being answered in front of its
  router. To keep a route of your own there, give the endpoint another path or turn it off with
  `Metric: false`.
  - In xgin and xecho the endpoint is now registered **after** the `WithRoutes` callbacks, which is
    what makes the collision visible rather than something to run into. The caller's middleware
    still covers it, which was why it is registered after `WithMiddleware` in the first place.
  - In xbeego the route template is compared, not the request: a pattern that merely happens to
    match `MetricPath` (`/:page`) is left alone, since it was never meant to be this endpoint.
- **An error, a panic or a JSON string value is no longer masked whole because it holds a sensitive
  word.** What is masked is now the value under a sensitive **name**: `password=hunter2` and
  `Authorization: Bearer ...` lose everything from the separator to the end of that line, while the
  sentence in front of it is kept. This affects the access log's `errors` field, the span's
  `gin.errors` / `echo.errors` / `beego.errors`, the `error` field of the panic log line, and the
  string values inside a logged JSON body.
  - What it buys: `token expired`, `session not found`, `invalid signature`, `failed to parse cookie
    header` and `Key: 'Req.Password' Error:Field validation ...` reached the logs as `***REDACTED***`
    before, taking the messages wanted most during an authentication incident with them.
  - What it costs: a credential written into a sentence with no name in front of it (`bad token
    abc123`) now reaches the log. A name is what makes a value locatable, and there is none there.
  - **Request and response bodies are unchanged**: an unstructured body still goes whole on a
    sensitive word, and a JSON or form key containing one still masks its value at any depth. The
    rules are in [xgin "Access log"](../xgin/README.md#access-log).

### Fixed

- **xgin's access log is written with the ctx its logging layer was entered with**, as xecho's and
  xbeego's already were. gin's `c.Request` is one field every layer shares, so a handler putting
  another ctx on it decided what the access log saw: the fields of a scope derived with
  `xlog.CtxWithKV` reached the line, although `CtxWithKV` documents that they stay inside their own
  stretch of calls and never reach the access log at the entry point; and a ctx not derived from the
  request's took `trace_id` and every `xlog.AddKV` field off that line altogether. Fields written
  into the request's own scope are unaffected.

## [v1.2.0] - 2026-10-22

### Changed

- **One name for the application across all three signals** (incompatible). The identity configured in
  `XApp` now reaches the logs, the metrics and the spans under one rule: OTel's semantic conventions
  with the dots written as underscores wherever a dot cannot go. The table is in
  [observability.md "One application, three signals"](observability.md#one-application-three-signals).
  - The default log fields are renamed: `service` → `service_name`, `version` → `service_version`,
    `hostname` → `host_name`, `pid` → `process_pid`. **Log queries, dashboards and alerts built on the
    old names have to be updated**, as does an `XLog.Fields` entry that overrode or dropped one of them
    by name (`hostname: ""` becomes `host_name: ""`).
  - xmetric attaches `service_name` and `service_version` as constant labels on every metric, the
    `go_*` / `process_*` ones included. An `XMetric.ConstLabels` entry of the same name still wins.
    They are not called `name` / `version` because both are reserved (`name` by the instance metrics,
    `version` by `go_info`).
- **An identity nobody configured is no longer guessed on the spans** (incompatible). OTel's
  `resource.WithService` is no longer used, so without `XApp.Name` (and without `OTEL_SERVICE_NAME` or
  `OTEL_RESOURCE_ATTRIBUTES`) no `service.name` attribute is written at all, where the SDK used to
  invent `unknown_service:<executable name>`. A guessed name looks like a real service on a dashboard
  without being one; `service.name` being a required attribute, a backend will fall back to whatever it
  shows for unnamed services, which is the intended visible gap. `service.instance.id` (a UUID per
  process) is kept and is written either way.
- **Kafka's `client.id` is no longer given an invented name either.** With neither `XKafka.ClientID` nor
  `XApp.Name` configured, franz-go sends its own `kgo` and **startup writes one WARN** naming the
  consequence (the brokers' request logs and quotas cannot tell the services apart) and what to set. The
  v1.1.0 fallback to the executable's name is gone, so no signal invents a name any more.

### Removed

- `xutil.ExecutableName()` (added in v1.1.0), which existed only for that Kafka fallback.

## [v1.1.0] - 2026-10-22

### Added

- **xbeego**, a new module: a [beego](https://github.com/beego/beego) v2 application served by the
  framework. The routes stay where they are -- on beego's global `web.BeeApp`, registered with
  `web.Router` / `web.InsertFilter` -- and `xone.MustRun(xbeego.New())` replaces `web.Run`, so a
  beego service gets the access log, tracing, metrics, panic recovery and graceful shutdown that
  xgin and xecho have, with the same field names, metric names and span names (they come from the
  same `internal/web`). The details are in [xbeego/README.md](../xbeego/README.md).
  - The `route` label and the span name use beego's route template (`/users/:id`), so the series
    count follows the number of routes rather than the number of ids.
  - `client_ip` comes from `XBeego.TrustedProxies` rather than beego's `ctx.Input.IP()`, which
    believes whoever sent `X-Forwarded-For`; `xbeego.ClientIP(r)` gives a controller the same
    value.
  - `XBeego.RecoverPanic` (on by default) takes beego's panic recovery over, so a panic is one
    ERROR line through slog with `trace_id`, the panic value masked and a stack still pointing at
    the controller. The two ways beego stops a handler by panicking keep working as they did:
    `web.ErrAbort` is swallowed, and `c.Abort("404")` onto a handler registered with
    `web.ErrorHandler` still renders it rather than becoming a 500.
  - `XBeego.BeegoLogs` (on by default) routes beego's own `logs` package into slog.
  - It is for API services: `web.Run` is not called, so beego's sessions, templates, gzip, admin
    server and default error pages are not set up. A service needing one of those keeps `web.Run`
    and imports `xlog` alone.

- **`xapp.SetDefault(xapp.Config)`**: the application identity given in code rather than in a file,
  which is where a version stamped in at build time (`-ldflags "-X main.version=v1.2.0"`) already is.
  The `XApp` block stays optional, and the file still wins over it field by field: a field it writes
  overrides what the program supplied, a field it leaves out keeps it. Call it before `xone.Run`,
  since the block is read on `StageLog`. See [xapp/README.md](../xapp/README.md).
- **`xutil.ExecutableName()`**: the running program's own file name, which is what a component falls
  back on for a name nobody configured.

### Changed

- **xkafka: `client.id` no longer ends up as franz-go's shared `kgo`.** With `ClientID` unconfigured
  it still uses `XApp.Name`, and when there is no application name either it now uses the executable's
  name -- the one OTel builds its own `unknown_service:<name>` from -- so a service that configures no identity is
  still told apart in the brokers' logs, quotas and monitoring. Producers and consumers send the same
  id. A direct call to `xkafka.New` reads neither and is unchanged.

## [v1.0.4] - 2026-10-21

### Other

- No behavior change in this version. The repository is English throughout: the comments, the
  documentation a caller reads (every module's README and everything under `docs/`), the two
  diagrams, the test names and the failure messages. `scripts/check.sh` now fails on any file that
  holds Chinese, so this stays the case.

## [v1.0.3] - 2026-10-20

### Added

- xecho gained the config field `XEcho.Debug` (`false` by default, as in echo itself): with it on,
  echo's default error handler puts the error text into the response, `c.JSON` / `c.JSONP` /
  `c.XML` always indent, and echo's own log level is raised to DEBUG. Keep it off in production;
  the details are in [xecho/README.md "Behavior (measured)"](../xecho/README.md#behavior-measured).
- xecho gained `(*XEcho).WithValidator(echo.Validator)`: with a validator installed, a handler can
  call `c.Validate(&req)`. Echo has no built-in validator, and without one `c.Validate` returns
  `echo.ErrValidatorNotRegistered` (returning it directly from a handler is a 500). Which
  validation library to use remains the business's call, and xecho takes on no dependency for it.

## [v1.0.2] - 2026-10-13

### Other

- No caller-visible changes in this version: the code is identical to v1.0.1 and only the
  repository's own release tooling was updated.

## [v1.0.1] - 2026-10-13

### Fixed

- v1.0.0 pushed only the root module's tag and none for the integration modules such as xgin and
  xgorm, so installing with the commands in the README failed; v1.0.1 is the first version where
  every module installs. v1.0.0 has been retracted in the root `go.mod` (`retract`).

## [v1.0.0] - 2026-10-13

### Added

- First version. The core: configuration loading and the lifecycle of `xone.Run`, including xapp,
  xconfig, xerror, xflow, xhook, xlog, xtls, xutil and xonetest.
- The integration modules: xgin, xecho, xginswagger, xgorm (with the clickhouse driver), xredis,
  xcache, xhttp, xkafka, xcron, xmetric and xtrace.
