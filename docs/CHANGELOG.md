# Changelog

This file records the **caller-visible changes** of every version: new features, behavior changes,
incompatible changes, fixes and performance. Refactors and tooling changes inside the repository
are left out, unless they change what a caller has to do.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the version numbers
follow [Semantic Versioning](https://semver.org/). Every module shares one version number (see
`scripts/release.sh`).

## [Unreleased]

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
