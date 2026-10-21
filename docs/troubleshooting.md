# Troubleshooting

Look it up by the error text. The framework's errors all have the shape
`xone <module> <op> failed, err=[…]` (see [guide.md "Error handling"](guide.md#error-handling)),
and what follows is the keyword from inside the brackets, which is what to search the logs for.

These are the cross-module errors; anything belonging to one module is under "Troubleshooting" in
its README: [xgorm](../xgorm/README.md#troubleshooting) (`unknown Driver=…`) ·
[xtls](../xtls/README.md#troubleshooting) (client-side TLS) ·
[xgin](../xgin/README.md#troubleshooting) (server certificates) ·
[xecho](../xecho/README.md#troubleshooting) (server certificates, an XGin block copied over).

- [Configuration](#configuration)
- [Taking an instance: C()](#taking-an-instance-c)
- [Runnable and shutdown](#runnable-and-shutdown)
- [Connecting](#connecting)
- [TLS](#tls)

## Configuration

### A configured value had no effect, or it is unclear what was finally read

Start once with `XONE_DEBUG=1`: it prints which config file was used, which profiles were
activated, which files were read in priority order, and the merged configuration in full
(credentials masked). The three most common causes: the profile was not activated (`--profile` /
`XONE_PROFILE` replaces `XApp.Profiles` from the file), a list was replaced wholesale by a
higher-priority file, or the same file was `Import`ed twice and only the first occurrence counted.
See [config.md "Seeing the effective configuration: XONE_DEBUG"](config.md#seeing-the-effective-configuration-xone_debug).

### `XApp.Profiles may only be set in the base config file, found it in conf/shared.yml`

Default profiles may only be written in the main config file: letting an imported or
environment-specific file activate profiles too would make "which files to read" and "in what
order they are read" depend on each other. Move it back to the main file, or use `--profile` /
`XONE_PROFILE` instead.

### `config keys [MyApp] are not read by anyone`

By the time every start hook has run, the config file still holds a top-level key nobody read.

- **A typo**: `XRedsi`, `Xgin`. Check the block names in [config.md](config.md).
- **The package was not imported**: an `XGorm` block written without
  `import _ "github.com/jupiter-eng/xone/xgorm"` (or a package using it).
- **The business's own block is read too late**: a block first read with `xconfig.Unmarshal` inside
  `Start` does not count. Move it into `main` before `xone.Run`, or into a `BeforeStart` hook, see
  [xconfig](../xconfig/README.md#notes).
- A package that uses a block only when it is configured checks with `xconfig.Has(key)`: asking
  counts as claiming it.

### `field Bogus not found in type xgin.Config`

Preceded by a file and line such as `application.yml:3:`. The field is misspelled, or there is no
such field — any extra field in the configuration fails startup.

When it is followed by `(did you mean TLS.CertFile? move it under TLS:)`, a field of a sub-block
was written one level too high; move it in as the message says.

When it is followed by
``(field Endpoint has no yaml tag, so only "endpoint" is accepted: add `yaml:"Endpoint"`)``, a
field of your own struct has no `yaml` tag: yaml.v3 only accepts the all-lowercase key for a field
without one. Add the tag as the message says.

### `environment variables not set: DB_PASSWORD`

The configuration holds `${DB_PASSWORD}` (required) and the process's environment does not. Set it,
or write `${DB_PASSWORD:…}` with a default. `environment variables not set in XApp.Import of …` /
`in XApp.Profiles of …` is the same thing in the path of an `XApp.Import` or inside
`XApp.Profiles`.

A variable set to an empty string counts as "this field was not written" and the field keeps its
default; to get an empty string, write `"${VAR:}"`.

### `config file does not exist: /etc/app/application.yml`

The file named by `WithConfigPath`, `--config` or `XONE_CONFIG` is not there. Check the path; a
relative one is relative to the process's working directory.

### `read config conf/application-prod.yml: … no such file or directory`

The `prod` profile was activated while the base file has no `-prod` sibling. The profile name is
most likely misspelled; the framework does not skip it silently.

An imported fragment that cannot be found produces the same error: a fragment's path is relative to
**the directory of the file that wrote the `Import`**, not to the working directory. To allow it to
be missing, prefix it with `optional:`.

### `config was already loaded by an earlier read (from conf/application.yml), so … cannot take effect`

Something in `main` read the configuration before `xone.Run` (`xconfig.Unmarshal`,
`xgin.CurrentConfig()`, ...), which loaded a copy according to `--config` / `XONE_CONFIG` or the
conventional paths; naming a different file with `WithConfigPath` afterwards no longer agrees with
it. Use `--config` or `XONE_CONFIG` instead.

### `duplicate key "Port" at line 5 (first seen at line 3)`

The same key is written twice at the same level of one file. Remove one.

### `cannot mix the single- and multi-instance forms`

An `XGorm` / `XRedis` / `XCache` block holds both `Clients` and instance fields (`DSN`, `Addr`, ...)
at the top level of the block. Move everything under `Clients.<name>`, or drop `Clients` and use the
single-instance form. `Clients is empty` means `Clients` was written with no instances under it.

### `invalid config XGorm: Clients.report: application.yml:12: MySQL.ReadTimeout must not be negative`

Each module's `Validate` runs while the configuration is read. The error names the instance, the
file, the line and the field, so fix exactly that spot. The common ones: a negative duration,
`MaxOpenConns: 0`, `XGin.ReadHeaderTimeout: 0`, `XTrace.ShutdownTimeout: 0`, or a `-` inside
`XMetric.Namespace`.

### `unknown Driver="clickhouse", registered: [mysql postgres]`

See [xgorm "Troubleshooting"](../xgorm/README.md#troubleshooting).

## Taking an instance: C()

`xgorm.C()` / `xredis.C()` / `xcache.C()` / `xkafka.C()` panic when they cannot produce an instance,
with four different messages pointing at four different places to look:

| What the panic says | Cause | What to do |
|---|---|---|
| `instance "default" was requested before xone.Run started xgorm` | Called too early: in `main`, in an `init()`, while initializing a package-level variable, or in a hook earlier than `StageClient` | Move it into `Start`, into request handling, or into a hook at the default stage (`StageBusiness`) or later; to hold it in a package-level variable, store the function `xgorm.C` rather than its result |
| `instance "default" was requested after xgorm was closed` | Called too late: shutdown already closed it while a goroutine is still using it | Have `Start` wait for in-flight work before returning, see [guide.md "Non-web services"](guide.md#non-web-services-consumer--job) |
| `no instance named "default", and none is configured at all — check the XGorm block` | The whole block is missing from the configuration | Write the `XGorm` block; for an optional dependency, check `xgorm.Has()` first |
| `no instance named "orders", configured ones are [default report]` | A misspelled name, or a multi-instance setup with no `default` while `C()` was called without arguments | Use one of the names listed, or add a `default` under `Clients` |

`xhttp.C()` / `xhttp.R(ctx)` never panic: before `xone.Run` has built them and after they are
closed, what comes back is a fallback client with a 30s timeout (and none of the `XHttp` config).

## Runnable and shutdown

### `main.App has a Stop method of type func(main.App) error, but Run only calls Stop(context.Context) error`

The signature of `Stop` is wrong (usually a missing `ctx`). The framework only recognizes
`Stop(context.Context) error`; written differently it would never be called and the service would
not stop on a signal, so `Run` reports it outright. Fix the signature, or rename the method if it
was never meant for shutdown.

### `main.App has Stop on its pointer receiver, so Run cannot call it on a value: pass a pointer (&main.App{...})`

`Stop` is defined on the pointer while a value was passed to `Run`. Pass `&App{...}`.

### `stop budget must be > 0 (0 is not unlimited, it is no wait at all), got=0s`

`xone.WithStopTimeout` was given 0 or a negative value. 0 does not mean "no limit", it means "do not
wait at all". Give it a positive value, and make the deployment's termination grace period longer
than it.

### `N handler(s) still running when the shutdown deadline passed`

xgin / xecho reached the end of the server's share of the stop budget (2/3 of `WithStopTimeout`,
10s by default) with handlers still running. The connections have been closed and the request ctx
cancelled (a WebSocket handler holding a hijacked connection included), so what is left are
**handlers that ignore the ctx**: slow work and WebSocket read loops have to watch
`c.Request.Context()` (`c.Request().Context()` in xecho). A handler stuck on a Redis read ends up
here too — go-redis does not honor cancellation, so give the ctx a deadline
(`context.WithTimeout`). When more time is genuinely needed, raise `WithStopTimeout`.

### `server did not exit within its share of the stop budget, closing the rest`

`Start` did not return within the server's share of the budget (`Stop` ignores the ctx, or `Start`
does not return after the ctx is cancelled); when `Stop` itself does not return, the message is
`server Stop did not return within its share of the stop budget, closing the rest`. The framework
stops waiting and goes on closing the components — and if the server is still using the database at
that point, it runs into a closed pool. Make `Start` return once the ctx is cancelled.

### The process hangs instead of exiting

After the first SIGINT / SIGTERM the framework begins a graceful shutdown and restores the system's
default disposition: **a second signal terminates the process immediately** (Ctrl+C twice). When the
log shows `shutdown signal received, closing gracefully; send it again to terminate now` and nothing
after it, a stop hook or `Stop` is most likely stuck in a call that ignores the ctx. The framework
never waits forever: `Run` always returns once the overall `WithStopTimeout` budget is up. In
Kubernetes, keep `terminationGracePeriodSeconds` longer than `WithStopTimeout`, see
[guide.md "Deployment"](guide.md#deployment).

### `shutdown signal received during startup, not starting the server`

A stop signal arrived during startup, while connecting or retrying. This is not a failure:
whatever was already built is closed in reverse order and the process exits with 0.

## Connecting

### `authentication to db:5432 failed` and `cannot reach db:5432`

The startup connection probe tries at most 3 times
([behavior.md](behavior.md#the-startup-connection-probe)).

- `authentication to <addr> failed`: the server explicitly rejected the credentials (a wrong
  password, a missing user, no permission on that database in MySQL, or PG requiring a client
  certificate that was not presented), and it is tried **once**. Check the account, the password
  and the permissions; the server's own text (without credentials) follows.
- `cannot reach <addr>`: unreachable, timed out, or the database does not exist (outside MySQL),
  reported after all 3 attempts. Check the address, the network, the firewall, `DialTimeout`, and
  whether the server accepts TLS only (connecting in plaintext to a TLS port usually gives `EOF` or
  `unexpected packet`).

## TLS

Errors from the client-side TLS block (XGorm / XRedis / XHttp) are in
[xtls "Troubleshooting"](../xtls/README.md#troubleshooting), XGin's server certificate errors are in
[xgin "Troubleshooting"](../xgin/README.md#troubleshooting), and XEcho's are in
[xecho "Troubleshooting"](../xecho/README.md#troubleshooting).
