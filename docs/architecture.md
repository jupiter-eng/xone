# Architecture and design

This document covers **why it looks the way it does**. How to use it is in [`guide.md`](guide.md),
the configuration fields are in [`config.md`](config.md), the measured defaults are in
[`behavior.md`](behavior.md), and contributing is in [`development.md`](development.md).

![The module layering of xone](images/architecture.svg)

*The module layering: on top of the core module, every integration with third-party dependencies is
a Go module of its own; integrations join the framework through `xhook` / `xconfig` and never
import the root package.*

![Startup and shutdown](images/lifecycle.svg)

*Startup and shutdown: take over signals → load the configuration → run the start hooks stage by
stage → `Runnable.Start`; on shutdown the stop hooks run in reverse, and the whole shutdown shares
one stop budget.*

- [Design principles](#design-principles)
  - [1. `init()` only registers, it never initializes](#1-init-only-registers-it-never-initializes)
  - [2. Every integration is its own Go module](#2-every-integration-is-its-own-go-module)
  - [3. Every integration exports a pure constructor New](#3-every-integration-exports-a-pure-constructor-new)
  - [4. Joining the framework takes two verbs](#4-joining-the-framework-takes-two-verbs)
- [How hooks pair up](#how-hooks-pair-up)
- [Stop signals: taken over in the process's first millisecond](#stop-signals-taken-over-in-the-processs-first-millisecond)
- [The stop budget is one budget, not one per component](#the-stop-budget-is-one-budget-not-one-per-component)
- [Shared implementations](#shared-implementations)
- [How the modules extend each other](#how-the-modules-extend-each-other)
- [A few decisions about configuration loading](#a-few-decisions-about-configuration-loading)

## Design principles

**Keep the caller-facing interface minimal and push complexity into the framework.** This one wins
over every principle below: whatever the framework can absorb internally does not get pushed onto
the caller — no new terminology, no change to the signatures they write, no further "you have to
remember ..." rule. When weighing a change, ask first what the caller would have to learn, and
when the answer is not "nothing", think again. Things already done this way: the configuration can
be read any time before `Start` (nobody needs to know when it loads), a Runnable only has to write
`Start`, shutdown takes one total duration, and a failing `C()` says whether it was called too
early or never configured.

### 1. `init()` only registers, it never initializes

Go runs `init()` in "topological order plus the lexical order of package paths", which the caller
cannot control. The problem was never `import _` itself, it is that **many frameworks merge
"registering" and "initializing" into one act**, which turns that uncontrollable order into the
initialization order.

xone separates them: `init()` only records "who I am and how to initialize me" into a list, and the
real initialization is run by `Run()` stage by stage. **Go's init order does not affect the order
between stages**; within one stage it is the registration order, which is Go's package
initialization order, so things within one stage cannot depend on each other.

A fixed set of stages rather than arbitrary numbers, because numbers need global coordination: who
should write 30 and who should write 50 is something nobody can answer once there are enough
declarations. A business hook writes no stage and lands in `StageBusiness`: every client is ready
and the server is not up yet.

### 2. Every integration is its own Go module

Go's MVS imposes the version requirements of **the whole module graph** on the caller — even for a
package they never import. Measured: an application importing only a dependency-free error package
and pinning `gin v1.9.1` itself was pushed up to `v1.12.0`, with the versions of gorm, redis and
otel fixed along with it.

So every integration with third-party dependencies is its own module. The size of the module graph
for an empty application importing only that integration:

| Imports | Module graph (excluding the app itself) | Of which third-party |
|---|---|---|
| The core `github.com/jupiter-eng/xone` (xapp, xlog, xconfig, xflow, xhook, xerror, xutil, xtls, xonetest) | 3 | 2 |
| `xtrace` | 25 | 23 |
| `xmetric` | 38 | 36 |
| `xcache` (metrics through xmetric) | 42 | 39 |
| `xhttp` (tracing through xtrace) | 58 | 54 |
| `xcron` (tracing through xtrace) | 27 | 24 |
| `xredis` (tracing through xtrace) | 60 | 56 |
| `xkafka` (tracing through xtrace) | 56 | 52 |
| `xgorm` (tracing through xtrace) | 68 | 64 |
| `xgorm/clickhouse` | 131 | 126 |
| `xgin` (tracing through xtrace) | 89 | 85 |
| `xginswagger` | 84 | 82 |
| `xecho` (tracing through xtrace) | 63 | 59 |

How it was measured: create an empty consumer module outside the repository whose `main.go` holds
a single `import _ "<package>"`, point its `go.mod` at this repository's modules with `replace`,
run `GOWORK=off go mod tidy`, and count the lines of `GOWORK=off go list -m all` beyond the first
(the application itself); "third-party" then drops the ones starting with
`github.com/jupiter-eng/xone`. The numbers move with dependency upgrades, so measure again after
changing a `go.mod`.

- **The integrations that produce spans (xgin, xecho, xgorm, xredis, xhttp, xcron, xkafka) depend
  on xtrace**: using them gives tracing with nobody having to remember importing xtrace — and
  forgetting it would raise no error, it would only make every span a noop and leave `trace_id` out
  of the logs. The cost measured small: they already depend on OpenTelemetry's API (xgin and xgorm
  on the SDK as well), and what xtrace adds is itself, the b3 propagator and the stdout exporter,
  taking the module graph from 85 / 64 / 58 to the 89 / 68 / 60 in the table above (xgin / xgorm /
  xredis). xcache produces no spans and does not depend on it.
- **An integration you do not use keeps its dependencies out of your module graph**, and its Go
  version requirement with them: the core stays on Go 1.23 while the integrations need Go 1.25
  because their upstreams do. Each integration can also take a major upgrade on its own.
- The dependency-free ones (`xlog`, `xflow`, `xapp`) stay in the core: splitting into modules keeps
  dependencies away from the caller, and with no dependencies to keep away there is no need for
  another module. `scripts/check.sh` pins the core's third-party modules at 3 or fewer.
- **Swagger UI** compiles a whole frontend bundle into the binary: importing gin alone is 55
  third-party modules while xginswagger is 82, which is why it is not part of xgin.
- **Database drivers**: mysql and postgres are built in; the ClickHouse driver adds more than 60
  modules (mostly the Docker and testcontainers that clickhouse-go uses for its integration tests,
  which `go.mod` cannot tell apart as "test only"), so it lives in `xgorm/clickhouse` and `xgorm`
  keeps only a registration point (`xgorm.RegisterDialect`).
- **Test dependencies count too**: `go mod tidy` records them as direct dependencies and they reach
  the caller's module graph all the same. Measured, adding a test-only `otelhttp` to `xtrace` took
  its third-party modules from 23 to 27.

### 3. Every integration exports a pure constructor New

```go
func New(cfg Config) (*T, io.Closer, error)                       // touches no globals, reads no files, depends on no framework
func New(ctx context.Context, cfg Config) (*T, io.Closer, error)  // one that connects or probes takes a ctx as well
```

Zero assembly is the default path, and `New` guarantees it is **not the only path**: a test calls
it directly for a clean instance, and a second configuration has a way out. The ones that block
(`xgorm`, `xredis`, `xkafka`, `xtrace`) take a `ctx` and the ones that do not (`xlog`, `xmetric`,
`xcache`, `xhttp`) do not — the signature says honestly whether this constructor can hold you up.
A multi-instance module takes the configuration of one instance (`ClientConfig`). `check.sh`
checks that every integration registering a stop hook has a `New`.

### 4. Joining the framework takes two verbs

`xhook.BeforeStart(fn)` / `xhook.BeforeStop(fn)`, with ordinary Go inside the hook:
`xconfig.Unmarshal(key, &c)` to read the configuration, then build the instance, store it and write
`C()` yourself. No handles, no generics, no terminology to understand before using it. Writing an
integration means knowing two packages, `xhook` and `xconfig`, and never importing the root
package: an integration does not know the framework itself, which is what makes shipping it as a
separate module work. `check.sh` caps the number of exported APIs in those two packages (6 each):
every export added is one more rule every third-party integration has to understand.

## How hooks pair up

The rules are in [guide "Stop hooks: pairing and inheritance"](guide.md#stop-hooks-pairing-and-inheritance);
what is recorded here is why they are what they are:

- **A stop hook never has to handle "it was never built".** When startup fails halfway, the
  resources after it were never built at all, and calling their stop hooks would only fail on a
  pile of nil values.
- **The nearest one rather than the first in the package**: when one package registers several
  pairs and the second cannot be built, its stop hook should not be reached just because the first
  pair succeeded.
- **Pairing is per package, with the package taken from the `init` on the call stack rather than
  from the hook function's name**: a hook registered on your behalf by a helper package belongs,
  as a function, to that helper, so going by the function would make every package registering
  through it one and the same. The package is taken as the full import path, since the last
  segment collides (a caller wrapping their own package called xlog is common).
- **A stop hook without a stage inherits the start hook it pairs with**: the stop order is the
  mirror image of the start order, and declaring a stage once per resource achieves "up first, down
  last" at both ends. An explicit `At` wins.
- **An unpaired stop hook always runs**, even when the Runnable is wrong or the configuration
  cannot be read and not one start hook ran: it never depended on startup in the first place.

## Stop signals: taken over in the process's first millisecond

Registering a signal handler at all **cancels the system's original "die on receipt"**. It is a
switch, not "one more handler", so where it is installed and when it is handed back decides
directly whether the process can be killed. xone takes over SIGINT and SIGTERM, doing three things
at once:

1. **Signals are taken over before the configuration is read.** Installed after initialization, a
   SIGTERM during those seconds of connecting to the database, connecting to Redis and retrying
   would take the system's default disposition — the process dies on the spot and not one of the
   resources already built gets to deregister (that entry in the registry, that distributed lock,
   can only wait for the other side to expire).
2. **Hooks receive a `ctx`, and the framework checks again before each hook.** After a signal the
   remaining start hooks do not run, whatever was built is closed cleanly in reverse order, the
   server is not started, and the process exits with `0` — exiting as asked is not a failure, and
   reporting it as one would leave a "startup failed" behind every time a rolling update lands in
   that window.
3. **The default disposition is handed back after the first signal.** The framework can still be
   stuck in a third-party call that ignores the `ctx`, and then **a second signal terminates the
   process immediately**. Without that escape hatch, being stuck means only `kill -9` gets rid of
   it and Kubernetes waits out the whole termination grace period.

Point 3 covers the shutdown phase as well: when `Stop` hangs, a second Ctrl+C works.

## The stop budget is one budget, not one per component

`xone.WithStopTimeout` (15s by default) is the total budget of **the whole shutdown**: from the
start of shutdown to `Run` returning takes that long at worst, and the deployment's termination
grace period only has to be longer. It has to really govern every step, or it is just a phrase:

- The framework divides it internally: the server (`Stop` plus waiting for `Start`) takes at most
  the first 2/3 and the rest goes to the stop hooks, where each hook behind leaves a share for the
  ones after it (`min(1s, remaining ÷ number of hooks)`) — a server that will not exit and a
  connection pool that will not close cannot eat anybody else's share, and xlog, which comes last,
  always has time to close its file.
- **xgin / xecho have no stop timeout of their own**: `Stop` waits for in-flight requests for the
  server's share, force-closes at the deadline and then waits for the handlers to really return. To
  make it wait more or less, change the total budget — there is no second number to keep aligned
  with it.
- `Stop` and a component's `Close()` may well ignore the `ctx`, so the framework waits for them on
  another goroutine and stops waiting at the deadline. One connection pool that will not close
  should not put every component after it, and the process itself, in a queue behind it.
- It has to be positive: 0 does not mean "no limit", it means "do not wait at all", and `Run`
  reports an error before doing anything.

## Shared implementations

`xgorm` / `xredis` / `xcache` / `xkafka` share one shape: the configuration may hold one instance
or several by name, `C()` takes one by name at runtime, and startup builds them one by one and
closes everything already built when one cannot be built. Those semantics ought to be identical
everywhere — "what to say when a name is not found" and "what to do when the two forms are mixed"
are decisions made once, and spread across three modules they are three implementations that drift
apart. So they are gathered into one:

| What is shared | Where |
|---|---|
| Named instances, building them, rolling back on failure, closing in reverse order, the four messages for a failed lookup | `internal/xclient.Registry` (not exported) |
| The startup connection probe: how many attempts, the per-attempt budget, no retry after an authentication failure or a rejected certificate | `internal/xclient.Probe` (built on `xutil.Retry` / `xutil.Permanent`) |
| Decoding the single- and multi-instance forms | `xconfig.UnmarshalClients` |
| The client TLS block: its fields, validation, and building a `*tls.Config` | `xtls.Config` (shared by xgorm, xredis, xhttp and xkafka; dependency-free) |
| Registering a metric and asserting it back to its concrete type | `xmetric.RegisterAs` |
| The half of a web server that has nothing to do with the framework: listening, h2c, server TLS, graceful shutdown; trusted proxies; masking and the sensitive word list; the access log fields | `internal/web` (not exported; used by both xgin and xecho) |

`C()` panics with a message saying which of the four cases it is, because being called too early,
being called too late, the whole block being unconfigured and a misspelled name are four different
places to look. Being called too early is the easiest to collapse into "the whole block is
unconfigured", which sends somebody taking an instance in `main` off to re-read a configuration
file that was right all along; so when the whole block is unconfigured the start hook runs `Build`
anyway, which is how the registry knows the start hooks ran.

- `xclient` is internal: it is code shared by three modules, not something a caller has to learn.
  An integration of your own that wants several instances needs nothing beyond a map with a lock.
- Server-side TLS (the `TLS:` block of xgin and xecho, with `ClientCAFile` / `MinVersion` and no
  `Enable`) has a different shape, lives in `internal/web`, and is not added to `xtls`.
- `internal/web` is likewise not exported: what the caller sees is still `xgin.Config` and the
  functions of `xgin/middleware`; the word list is one per process, and moving to another web
  framework gives the same answer for which client_ip is believed, what is masked in the log and
  whether shutdown waits for handlers.

## How the modules extend each other

A lower layer does not know an upper one. When it needs an upper layer's ability, the lower layer
holds an extension point and the upper layer injects into it:

| Extension point | Injected by | What it does |
|---|---|---|
| `internal/logext.SetTraceExtractor` | xtrace | Logs carry `trace_id` automatically while xlog does not depend on OpenTelemetry |
| `internal/logext.AddObserver` | xmetric | Error log lines are counted while xlog does not depend on Prometheus |
| `internal/logext.WithKV` | xkafka | Each message's ctx carries the `topic` / `partition` / `offset` / `group` log fields while xkafka does not import xlog |
| `xgorm.RegisterDialect` | driver modules such as xgorm/clickhouse | A driver is added while xgorm does not carry its dependencies |
| A carrier's `TrustedPeer() bool` | xgin, xecho | xtrace is told the peer is trusted while xtrace does not know them |

xlog's extension points live in the core's `internal/logext` (with `xlog.SetTraceExtractor` /
`xlog.AddObserver` / `xlog.CtxWithKV` forwarding to them) rather than in xlog itself: the injecting
side — xtrace, xmetric, xkafka — therefore does not import xlog. xlog replaces `slog.Default()`,
and a program using only integrations such as xgorm, xredis and xkafka (which all carry xtrace and
xmetric) should not have its logging taken over by that; xlog comes along with xgin / xecho /
xcron alone.

The "wrap something around `slog.Default()`" approach is not used: `slog.SetDefault` also routes
the standard library's `log` package into the new handler, and once the chain loops back to slog's
own handler it forms a cycle and deadlocks on the `log` package's non-reentrant lock. Having the
lower layer hold the extension point avoids that entirely.

## A few decisions about configuration loading

The rules themselves are in [`config.md`](config.md); what is recorded here is why they are what
they are:

- **The configuration is loaded the first time somebody reads it** rather than loaded once inside
  `Run`. A read in `main` before `Run` therefore sees the final values too, and the caller need not
  know when it loads. The cost is that `WithConfigPath` cannot reach that earlier read, which is
  why naming a different file after an early read is an error rather than a silent swap.
- **Defaults are pre-filled in the struct, with no `*bool` pointers.** A field the file does not
  write keeps its value, and `Enable: false` is false.
- **A misspelled field and an unread block both fail startup.** Both would otherwise have somebody
  configuring for ages before noticing nothing took effect, and the config file is the caller's
  only interface. "Unread" is decided once every start hook has run: a block read only inside
  `Start` is equally unread by then, so the error says where to move it.
- **Validation happens while reading** (`Validate() error`), with the file and line in the message,
  before a single instance has connected.
- **The single- and multi-instance forms cannot be mixed.** With both written, "which one is
  default" has no answer that does not surprise somebody.
- **Placeholders expand on the parsed nodes** rather than as text substitution on the raw bytes —
  otherwise an environment variable whose value holds a colon or a newline would change the YAML
  structure, which is an injection path. After expansion a scalar's type is decided afresh from the
  new content, and quoting forces it to stay a string.
- **An `Import`'s relative path resolves against the directory of the file that wrote it**: moving
  the whole config directory elsewhere should not break the relative references inside it.
- **Profiles and imports are spelled as in Spring**, and so is merging: maps merge recursively,
  lists are replaced wholesale, scalars overwrite. The list rule is the one most often
  misunderstood — merged element by element, `[C]` on top of `[A,B]` would become `[C,B]`, so you
  believe you replaced the whole table while only the first entry changed.
- **One deliberate difference from Spring**: a named profile file that does not exist fails
  startup. A misspelled profile name is almost always a typo, and skipping it silently leaves a
  configuration nobody ever looked at quietly coming up with defaults. A missing profile variant of
  a fragment is not an error: after splitting into ten fragments, demanding a variant of each for
  every environment is unusable, and a misspelled profile name is already caught by the main file's
  variant.
- **Defaults inside collections**: the multi-instance `Clients` has its defaults laid under each
  instance by `UnmarshalClients`; other map and slice elements decode from their zero value, and
  giving them defaults means writing `UnmarshalYAML` for the element type and using
  `xconfig.DecodeStrict` inside it — `node.Decode` drops the strict checking.
