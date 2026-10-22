# Changelog

This file records the **caller-visible changes** of every version: new features, behavior changes,
incompatible changes, fixes and performance. Refactors and tooling changes inside the repository
are left out, unless they change what a caller has to do.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the version numbers
follow [Semantic Versioning](https://semver.org/). Every module shares one version number (see
`scripts/release.sh`).

## [Unreleased]

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
