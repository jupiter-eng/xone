# Contributing

Written for whoever changes this repository: how the directories are split, what each script does,
how to run the e2e tests, and the repository's rules together with the reasoning behind them.
`.claude/CLAUDE.md` is the condensed version for agents, and **the full text and tables of the
rules are here**. The background of the design decisions is in [`architecture.md`](architecture.md).

- [Repository structure](#repository-structure)
- [go.work and GOWORK=off](#gowork-and-goworkoff)
- [Scripts](#scripts)
- [e2e: tests against a real web server](#e2e-tests-against-a-real-web-server)
- [Rules](#rules)
  - [Language](#language)
  - [Elegance before speed](#elegance-before-speed)
  - [Every error goes through xerror](#every-error-goes-through-xerror)
  - [Every third-party default has to be measured](#every-third-party-default-has-to-be-measured)
  - [Mutation testing](#mutation-testing)
  - [Configuration and documentation](#configuration-and-documentation)
  - [The changelog](#the-changelog)

## Repository structure

```
xone/
├── xone.go              the root package: Run / MustRun / Option
├── xhook/               the two lifecycle hooks, dependency-free. One of the two packages an integration has to know
├── xconfig/             config decoding: Unmarshal, Has, UnmarshalClients, DecodeStrict. The other one
├── xonetest/            test helpers for callers: swap in a configuration, run the hooks
├── xtls/                the client TLS block, shared by xgorm / xredis / xhttp / xkafka, dependency-free
├── xerror/  xutil/      no third-party dependencies
├── xapp/                application identity (name, version); it claims configuration and initializes nothing
├── xlog/                logging, built on log/slog, no third-party dependencies
├── xflow/               flow orchestration with automatic rollback, no third-party dependencies
├── internal/
│   ├── config/          config loading: locating, profiles / imports, merging, placeholders, strict decoding
│   ├── hook/            hook registration, pairing and running by stage
│   ├── xclient/         the named-instance management and startup probe shared by xgorm / xredis / xcache / xkafka
│   ├── web/             the half xgin / xecho / xbeego share: listening, TLS, graceful shutdown, trusted proxies, masking; dependency-free
│   ├── logext/          xlog's extension points (trace_id extraction, error log counting, log fields on a ctx), injected into by xtrace / xmetric / xkafka
│   ├── testkit/         small helpers shared by this repository's own unit tests, standard library only
│   ├── coreonly/        proof that a program using only the core, importing no integration, still works (tests)
│   └── schemagen/       generates config_schema.json and checks the "Configuration" section of each module's README (a tool module of its own)
├── xtrace/              tracing, built on OpenTelemetry (its own module)
├── xmetric/             metrics, built on Prometheus (its own module)
├── xgorm/               databases, built on GORM (its own module)
│   └── clickhouse/      the ClickHouse driver (its own module)
├── xredis/              Redis, built on go-redis (its own module)
├── xcache/              a local cache, built on ristretto (its own module)
├── xhttp/               outbound HTTP, built on resty (its own module)
├── xkafka/              Kafka producing and consuming, built on franz-go (its own module)
├── xcron/               in-process scheduled jobs, with cronexpr used for parsing only (its own module)
├── xgin/                a web server, built on Gin (its own module)
│   └── middleware/      the access log, tracing, metrics and panic recovery, plus LogScope / Propagate
├── xginswagger/         Swagger UI (its own module)
├── xecho/               a web server, built on Echo v4 (its own module); shares internal/web with xgin
│   └── middleware/      the same as xgin/middleware, plus "the error is rendered in this layer"
├── xbeego/              a beego v2 application served without web.Run (its own module); shares internal/web with xgin
│   ├── middleware/      the same again, as plain net/http middleware mounted outside beego's router
│   └── internal/reqstate/  the route template, the status code and the bytes written, which beego hands no http.Handler
├── docs/                cross-module documentation for callers (the README has the navigation) + this file + CHANGELOG.md;
│                        each module's own documentation is the README.md in its directory (see "Configuration and documentation")
├── example/             one module: runnable examples that double as in-process cross-module integration tests
│   ├── consumer/        a message queue consumer: the shape of a non-web service
│   └── component/       writing an integration of your own: a complete sample of two hooks plus a C()
├── e2e/                 tests against a real web server (its own module, not published)
│   ├── service/         the service under test: uses every integration, with all configuration coming from YAML
│   ├── service-beego/   a beego application served by xbeego: the three beego globals it takes over, in a real process
│   ├── baseline/        a bare gin service for comparison, to measure the framework's overhead under load
│   ├── covapp/          a minimal program shaped like a one-off job, to test when the configuration loads (early reads, WithConfigPath)
│   ├── harness/         starting processes, reading logs / spans / metrics / /proc, a TCP fault proxy, downstream stubs, a load generator
│   └── compose.yml      the PG / MySQL / Redis / ClickHouse / Kafka the e2e tests need
├── config_schema.json   the configuration's JSON Schema, generated from the structs, for IDEs
├── .github/workflows/   ci.yml: check.sh + test.sh, plus compiling the core separately with Go 1.23; e2e.yml: e2e plus the full mutation run;
│                        release.yml: the release button (tag, run e2e, push the tags only; verifying installability is a separate verify job)
└── scripts/
    ├── check.sh         design constraints compiled into checks
    ├── test.sh          runs the whole repository's tests (go test ./... does not cross module boundaries)
    ├── mutate.py        mutation testing
    ├── mutations/       the mutation tables, one file per module (core.py is the root module)
    ├── e2e.sh           brings up PG / MySQL / Redis / ClickHouse / Kafka and runs e2e/
    └── release.sh       releasing: --bump pins the version, --tag tags, --smoke before pushing and --verify after it check that it installs and runs
```

The scripts can be invoked from any directory; they change to the repository root first.

## go.work and GOWORK=off

A `go.work` is committed, putting every module (the core, the integrations, `example`, `e2e`,
`internal/schemagen`) into one workspace, with the modules additionally pointing at each other
through `replace` in their own `go.mod` (kept on main permanently; at release time
`release.sh --bump` pins the requires to the new version and merges through a PR, leaving the
replaces alone, see [release.sh](#releasesh) below). So an IDE opened at the root recognizes every
module, and `go run ./example --config=example/application.yml` works from the root.

But `scripts/check.sh` and `scripts/test.sh` always run per module with `GOWORK=off` — a workspace
hides a module's own `go.mod` problems (a missing require, a wrong version), and CI has to catch
those. For the same reason, `go list -m all` inside a workspace merges every module's dependencies,
so measuring the module graph (the table in
[architecture.md](architecture.md#2-every-integration-is-its-own-go-module)) has to use `GOWORK=off`
and a consumer module outside the repository.

## Scripts

| Command | What it does | In CI |
|---|---|---|
| `scripts/check.sh` | Architecture constraints + dependency boundaries + documentation + gofmt / vet | Every time (ci.yml) |
| `scripts/test.sh [go test flags]` | `GOWORK=off go test -race ./...` per module, several at a time (the CPU count by default, changed with `XONE_TEST_JOBS`), printed in module order; one failing module does not stop the others and everything is reported together at the end. Locally, leave out `-count=1` and unchanged modules come from the cache | Every time (with `-count=1`) |
| `scripts/mutate.py [-j N] [--only X] [-k X] [--dry-run]` | Mutation testing, run in parallel, leaving the worktree untouched | The full run nightly and on demand (e2e.yml); `--dry-run` inside check.sh |
| `scripts/e2e.sh [--load] [-run X]` | Tests against a real web server, needing PG / MySQL / Redis (ClickHouse and Kafka optional); without `-parallel` it uses 16 (the tests spend most of their time waiting; go test defaults to GOMAXPROCS, which is 103s on a 4-core machine against 60s at 16) | PRs that changed `go.mod` / `go.sum`, nightly, on demand (e2e.yml, without the load tests) |
| `scripts/release.sh vX.Y.Z (--bump \| --tag [--e2e-passed] \| --smoke \| --verify)` | Releasing: pin the version / tag / smoke before pushing / verify after pushing | No (tagging is done by the release button) |

### check.sh

- The core's module graph holds at most 3 third-party modules, and the core depends on no
  integration module; `xhook` / `xerror` / `xutil` / `xtls` / `internal/web` have no third-party
  dependencies;
- only xgin / xecho / xbeego / xcron bring in xlog: no package of the root module, xtrace, xmetric or the data
  integrations may import it;
- `init()` appears only in integration packages;
- caps on the number of exported APIs: 15 for the root package, 6 for `xhook`, 6 for `xconfig`,
  3 for `xonetest`, 3 for `xtls`;
- an integration package has to export `New` and must not import the root package; `example/` must
  not import `internal/` (a caller cannot import it);
- **every config field is written into the `## Configuration` section of its module's README** (the
  block → README mapping is in `internal/schemagen/docs.go`), and every section's YAML example
  passes the schema;
- `config_schema.json` agrees with the structs;
- runtime strings such as errors and logs are English; errors go through `xerror` and wrap the
  underlying error with `%w`;
- no exported name that has been removed still appears in `*.go` / `*.md` / `*.yml` (removing an
  exported name means adding it to the `gone` table in the script);
- every mutation pattern of `scripts/mutate.py --dry-run` still matches the code;
- `hooks()` in `scripts/lib.sh` recognizes the hook start and stop lines in every format
  (`release.sh --smoke` / `--verify` rely on it to compare the reverse order);
- test function names are English;
- gofmt, and `go vet` per module.

It **does not check** relative links and anchors in Markdown, so when moving a heading, grep for the
references yourself (see [Configuration and documentation](#configuration-and-documentation)).

### config_schema.json

Generated from each module's Config structs by `go run ./internal/schemagen`, with the field
descriptions taken straight from the comments on those structs — the comments, the documentation
and the schema have one source. After changing a Config field (its comment included), regenerate it
and write the field into the `## Configuration` section of that module's README. Forgetting either
step turns `check.sh` red.

### mutate.py

```bash
scripts/mutate.py                  # the full run: break it, compile, run the tests, with as many workers as CPUs by default
scripts/mutate.py -j 2             # two workers only
scripts/mutate.py --only xgorm     # one table / module / path prefix only (xgorm covers xgorm/clickhouse)
scripts/mutate.py -k timeout       # only the ones whose name contains this
scripts/mutate.py --dry-run        # only checks that every mutation pattern still matches the code, in under a second (the filters still apply)
```

The broken copies live in a temporary directory and are swapped into the compilation through
`go test -overlay`, leaving the worktree untouched to the byte: **a clean worktree is not
required**, code can be edited while it runs, and a Ctrl-C leaves nothing to write back. The
results print in table order, so the output of two runs can be diffed directly. The rules are under
[Mutation testing](#mutation-testing) below.

### release.sh

In a multi-module repository every module has its own tag, all on the same commit of `main`.
Releasing takes two steps with an ordinary PR in between:

```bash
scripts/release.sh v1.0.0 --bump     # 1. pin every module's in-repository requires to v1.0.0, turn the CHANGELOG's Unreleased section into this version,
                                     #    and change the version in the README install command to v1.0.0
                                     #    files only: commit, open a PR, merge into main (branch protection applies as usual)
scripts/release.sh v1.0.0 --tag      # 2. tag every module on main's latest commit (without pushing)
scripts/release.sh v1.0.0 --smoke    # before pushing: the same external project and the same checks, with the dependencies replaced to the local code
scripts/release.sh v1.0.0 --verify   # after pushing: go get in a brand-new external project, checking that it installs and runs
```

Step 1 can be handed to Claude: say "release v1.1.0" in this repository and it pins the version,
tidies the CHANGELOG and opens the PR according to `.claude/skills/release/SKILL.md`, stopping
there.

**Step 2 normally uses the release button**: on GitHub, Actions → `release` → Run workflow, filling
in the version. It runs `--tag`, e2e (the same as `e2e.yml`) and `--smoke`, and pushes this one set
of tags (`--atomic`) and no branch; anything red before the push means nothing is pushed. The
`--verify` after the push is a separate `verify` job: when it is red (usually because the proxy has
not picked up the new tags yet), "Re-run failed jobs" reruns only it. The repository's
Settings → Actions → General → Workflow permissions has to be Read and write, and a ruleset
targeting tags has to allow it. See `.github/workflows/release.yml`.

**Why the go.mod on `main` can be released as is**: a caller's build ignores a dependency's
`replace` directives and fetches the versions in `require`. So the development `replace` stays
permanently and releasing only pins `require` to the version being released — there is no separate
release commit and no commit restoring anything afterwards. Between releases, main's go.mod names
the previous version while the `replace` compiles the code in the workspace, which does not affect
development.

- `--bump` also pins `example`, `e2e` and `internal/schemagen`, which are not published: they
  require the submodules, which in turn require the core's new version, and leaving them on the old
  one makes `go vet` report that go.mod needs updating. Only the modules being published get tags.
- `--tag` first confirms the commit can be released: a clean worktree, every in-repository require
  on this version, a `## [vX.Y.Z]` section in the CHANGELOG, and the tags still unused. It then runs
  the checks, the unit tests and e2e (`--e2e-passed` skips the e2e here: the commit passed
  elsewhere, or e2e runs right afterwards before the push, as the release button does).
- Only v0 and v1 are accepted. Once pushed, a tag is cached permanently by the module proxy and
  cannot be deleted; the only way out is releasing another version on top.

### Benchmarks

Run them before and after touching hot code, and back a change with numbers (see
[Elegance before speed](#elegance-before-speed)):

```bash
go test -run=NONE -bench=. -benchtime=100000x ./xlog/ ./xflow/ ./xgin/middleware/ ./xecho/middleware/ ./xbeego/middleware/
```

## e2e: tests against a real web server

`e2e/` compiles `e2e/service` into a binary, starts it as a real process against real PostgreSQL,
MySQL, Redis, ClickHouse and Kafka, sends requests and signals, and then reads its logs, spans,
`/metrics` and `/proc`.

- The service's XGorm is multi-instance: `default` connects to PostgreSQL and `mysql` to MySQL;
  `ch` connects to ClickHouse and is written in the profile `e2e/service/application-ch.yml`,
  activated only by the processes `TestClickHouse_*` starts (`harness.Options.ClickHouse`);
- XKafka likewise lives in `e2e/service/application-kafka.yml`, activated only by the processes
  `TestKafka_*` starts (`harness.Options.Kafka`), with each test creating its own topic and using
  its own consumer group;
- faults are injected through the TCP proxy in the harness (disconnect, refuse new connections, add
  latency, simulate a host going down) rather than by stopping the real services;
- the tests use their own ports, table names and key prefixes;
- when PG / MySQL / Redis are not running, the script brings them up the way the machine installs
  them (`pg_ctlcluster` / `service mysql` / `redis-server`), with the connection parameters
  overridden by `XONE_E2E_PG_ADDR`, `XONE_E2E_MYSQL_ADDR`, `XONE_E2E_REDIS_ADDR` and friends; when
  the services are managed elsewhere (CI service containers, docker compose, another machine) set
  `XONE_E2E_EXTERNAL=1` (likewise when the address is not local or the machine has no command to
  start them) and the script only waits for them to be ready (60 seconds at most, by TCP alone);
- ClickHouse runs in the Docker container `xone-ch` (overridden by `XONE_E2E_CH_*`), and when it
  cannot be started the script sets `XONE_E2E_CH=0` and every `TestClickHouse_*` skips itself;
- Kafka runs in the Docker container `xone-kafka` (`mirror.gcr.io/apache/kafka:3.9.1`, single-node
  KRaft, overridden by `XONE_E2E_KAFKA_*`), and without that container the script creates one from
  the parameters in `e2e/compose.yml`; when it cannot be started it sets `XONE_E2E_KAFKA=0` and
  every `TestKafka_*` skips itself;
- any further arguments are passed to `go test` as they are.

Without those services installed locally, `e2e/compose.yml` brings them all up with the accounts and
passwords the script defaults to:

```bash
docker compose -f e2e/compose.yml up -d --wait   # up and healthy; with a default port taken, change it with XONE_E2E_PG_PORT and friends
scripts/e2e.sh
docker compose -f e2e/compose.yml down           # the data lives in tmpfs and is gone after down
```

```bash
scripts/e2e.sh                        # everything except the load tests
scripts/e2e.sh -run Smoke             # the smoke tests only
scripts/e2e.sh --load -run Load       # load tests: bare gin against three middleware configurations, concurrency 1 to 256, plus a few minutes of soaking
scripts/e2e.sh --load -run MySQL      # the MySQL group, with its comparison load test
scripts/e2e.sh --load -run ClickHouse # the ClickHouse group, with its comparison load test
```

The load tests do not run by default (a round takes ten-odd minutes and wants the machine to
itself). The results are printed as tables in the test log only; the load generator, the service
under test and the databases compete for CPU on one machine, so those numbers are only comparable
with each other.

**CI**: `.github/workflows/e2e.yml` runs on PRs that changed any `go.mod` / `go.sum`, nightly and
on demand (without the load tests), with the four services as service containers; the TLS tests for
PG / MySQL / Redis need a second instance started locally as root and are skipped on the runner.
The job summary says how many cases a `KNOWN BUG` skipped. When `test.sh` reaches the `e2e` module,
every test skips itself because `XONE_E2E` is not 1, so a machine without databases still comes out
green.

**The harness's rules:**

- The three binaries (`service`, `baseline`, `covapp`) are built in one `go build -race` (the load
  tests aside). When a process under test exits, the harness inspects its output (`checkOutput` in
  `harness/output.go`) and fails that test on a problem: `WARNING: DATA RACE` on stderr, or a line
  on stdout / stderr that is not JSON — a caller's logging platform parses JSON line by line, and a
  third-party library bypassing slog with one line of plain text is a parse failure on their side.
  The only exceptions are what the framework itself is expected to write (the default-format lines
  before xlog is installed, `MustRun`'s final error line, the runtime's panic output, and the
  truncated tail when a signal kills it); a test whose output is deliberately not JSON states the
  reason in `Options.NonJSON`.
- **Every switch goes through `Options.Overlay`.** An overlay is a profile layered on
  `service/application.yml`, so what a test writes is what a caller would write in YAML; the common
  fragments (`stopTimeout`, `sqlLog`, `bodyLogs`, ...) are in `functional_test.go`. The `E2E_*`
  environment variables are only for injecting values that differ per process (ports, connection
  strings, table names, file paths) and never as switches. `application.yml` does not write the
  framework's defaults for it: a field left out is the framework's default, so a test of the
  default behavior sees the real default. When the service under test needs another endpoint for
  `TestCoverage_*`, it is mounted under `/probe`.
- **Duration assertions**: the lower bound is asserted as measured while the upper bound leaves
  room for a slow machine — a service with `-race` is several times slower and every test is
  `t.Parallel`. The upper bound is written as "the measured number × 3" or "the number the
  documentation implies + a named slack constant" (`faultSlack`, `faultUnaffected`), with the
  constant's comment stating what was measured and what the counterexample is.
- **Tests assert the behavior the documentation describes**, and when one reveals a bug in the
  framework the assertion is not bent to fit it: it is marked `KNOWN BUG` and skipped, with the
  evidence in the test's comment.

## Rules

### Language

**The repository is English only.** Code, comments, READMEs and docs, test and benchmark names,
failure messages, scripts, YAML, commit messages and PR descriptions.

| Where | Language |
|---|---|
| Code comments, README and docs | English |
| **Error / panic messages** | **English** |
| **Log messages and field names** | **English** |
| Commit messages | English |
| Test and benchmark names | English (describing the scenario so it is easy to locate, such as `TestNew_FailsFastOnTypo`) |

This is a library other people use, and **the errors and logs it produces land in their systems** —
in their alerts, their log searches, their issues. A non-ASCII field name also becomes a JSON key
and breaks the logging platform's indexes and dashboards outright. Comments are written for whoever
reads this code, which is a separate matter — and they are English too, so that the code reads as
one piece.

```go
// A full pool waits rather than opening another connection
return xerror.Newf("xgorm", "connect", "cannot reach %s: %w", addr, err)
slog.Info("xgorm ready", "instances", reg.Names())
```

**The whole repository is English.** `scripts/check_no_zh.py` fails on any tracked file holding
Chinese — Han characters, CJK punctuation or fullwidth forms — so nothing slips back in. Where a
test has to assert on such text, a library's translated validation message for instance, it goes in
as `\u` escapes: the assertion keeps the exact string and the file itself stays clean
(`e2e/gin_options_test.go`). Generated files (`config_schema.json`) follow their source and are
regenerated rather than hand-edited.

Examples in documentation and comments use neutral values (`Europe/Berlin` for a timezone,
`prod,eu` for profiles), and never describe a "previous version / behavior change" that has not
been released.

### Elegance before speed

**No premature optimization.** The code stays elegant, tidy and readable, which matters far more
than saving a few dozen nanoseconds. Optimize only when both of these hold:

1. **It was measured.** A benchmark gives the numbers before and after; "it looks slow" is not
   accepted.
2. **The result is no more complex than before.** Simpler is better — removing one redundant
   allocation, or moving wasted work behind a condition, tends to make the code clearer at the same
   time.

Introducing a cache layer, an object pool, hand-written serialization or `unsafe` for performance
always comes with an explanation of why nothing else works.

### Every error goes through xerror

Every error a module returns to the outside is an `*xerror.Error` carrying a module name and an
operation name:

```go
return xerror.Newf("xgorm", "connect", "cannot reach %s: %w", info.Addr, err)
return xerror.New("xgorm", "config", err)
```

The caller can therefore always ask "who reported this": `xerror.Is(err, "xconfig")` (anywhere in
the tree) and `xerror.Module(err)` (the outermost layer). Four rules:

1. **Always wrap the underlying error with `%w`, never `%v`.** `%v` turns an error into a piece of
   text and `errors.Is` / `errors.As` stop there.
2. **One xerror per module boundary, not one per layer.** Intermediate errors inside (the ones
   `Config.Validate()` returns, say) stay plain errors and are wrapped once by the boundary layer.
   Wrapping at every layer nests the text into
   `xone xgin config failed, err=[xone xgin validate failed, err=[...]]`. xerror guards against this
   itself: `New` returns an Error of the same module unchanged, and `Newf` folds an Error in its
   arguments when rendering — one of the same module keeps "op: cause" and one of another module
   drops the duplicated `xone ` prefix. A guard is not a licence: the boundary still wraps once.
3. **The message does not repeat the module name.** The frame already has it, and repeating it
   gives `xone xgorm connect failed, err=[xgorm: ...]`.
4. **`op` comes from this set of words** rather than being invented at each site:

   | op | Used for |
   |---|---|
   | `config` | An invalid configuration, a failed decode |
   | `new` | Constructing an instance |
   | `connect` | Connecting, probing |
   | `close` | Closing, releasing |
   | `register` | Registering a metric, registering a dialect |
   | `start` / `stop` | Starting and stopping a server |
   | `execute` | Running one business flow (xflow) |

### Every third-party default has to be measured

Of the problems external reviews found in this repository, **most were the same mistake**: a library
was integrated, its default behavior was used, that behavior was never measured, and the
documentation then described what somebody assumed it to be. The ones already stepped in (every one
of them found only by measuring):

| Library | What was assumed | What it actually is |
|---|---|---|
| gin | `TrustedProxies` defaults to something safe | It defaults to `0.0.0.0/0`, so whoever sends `X-Forwarded-For` becomes `client_ip` |
| gin | `MaxMultipartMemory` is a request body limit | It is the spill-to-disk threshold, costing about three times its value on the heap; the 32MB default is 96MB per request |
| gorm | No Logger means no logging | It fills in its own default: ANSI colors written to `os.Stdout` |
| gorm | `gorm.Open` only assembles | It pings once on its own, using its own context |
| gorm | The SQL in the log carries placeholders | Without a Logger implementing `ParamsFilter` it interpolates the real parameter values |
| gorm | Implementing `ParamsFilter` keeps parameters out | During `Scan` it switches to `logger.Recorder`, which only honors the process-wide `RecorderParamsFilter` and hands over the parameters by default |
| gorm's MySQL driver | Connecting honors the caller's ctx | `Initialize` queries `SELECT VERSION()` with `context.Background()`: no retry, and a SIGTERM during startup waits out the full `ReadTimeout` |
| gorm's ClickHouse driver | Connecting honors the caller's ctx | `Initialize` queries the version with `context.Background()`: it waits out dial_timeout even with the ctx cancelled, and does not retry |
| go-sql-driver | Logging goes through the caller's logger | It writes `[mysql] …` as plain text to stderr by default, not JSON |
| MySQL / PG | Server error text carries no data | The text of 1062 / 1366 / 1292, and PG's `Detail`, carry parameter values (`Duplicate entry 'a@b.com'`) |
| pgx | `sslmode=prefer` is at least encrypted and trusted | It uses TLS but **does not verify the server certificate**, and silently falls back to plaintext when the server refuses TLS |
| go-redis | Commands honor the caller's deadline | They do not by default and only honor `ReadTimeout`; measured, a 200ms budget waited the full 5s |
| go-redis | One connection means one dial | `DialerRetries` defaults to 5 with a 100ms interval: one command takes 11.7s when the host is down |
| go-redis | A new connection only sends `HELLO` | It also sends `CLIENT SETINFO` and `CLIENT MAINT_NOTIFICATIONS`; on Redis 7.0 that leaves one failing span per connection |
| go-redis | Logging goes through the caller's logger | It writes `redis: …` as plain text to stderr by default |
| redisotel | The span holds the command name only | `db.statement` carries the whole command by default, exporting the value of `SET k v` verbatim |
| clickhouse-go | `read_timeout` means "no bytes received for this long" | It covers a whole read without renewing midway: a healthy long query fails anyway, and a read-timed-out query is resent by `database/sql` for 3 attempts in total |
| clickhouse-go v2.30 | A read-timed-out connection is discarded | It goes back to the pool, and the next query to borrow it receives the previous query's result (fixed in v2.47) |
| ristretto | `MaxCost` is the capacity | Each entry adds 56 bytes of internal overhead, so 2000 really holds 35 entries |
| ristretto | `Close()` can run alongside in-flight reads and writes | It closes the internal channels first and sets the flag afterwards; measured, 750 goroutines panicked under concurrent reads and writes |
| resty | `Timeout` covers one request | It covers one attempt; 300ms with 3 retries measured 1.24s |
| resty | No logger means no logging | It writes WARN / ERROR to stderr, with the URL's query string |
| resty | `resty.New()` and `NewWithClient` are the same | The former comes with a cookie jar, leaking cookies between unrelated calls |
| otelhttp | `CloseIdleConnections` passes through | It does not implement it, so the whole call becomes a no-op |
| otelhttp | An outbound span holds no credentials | `url.full` carries the query string, with only user:password removed |
| net/http | `Shutdown` drops connections on timeout | It only returns an error while in-flight connections keep running |
| net/http | A cross-host redirect carries no credentials | It only drops `Authorization`, `Cookie` and a few others; a custom `X-Api-Key` still reaches the new host |
| x/net h2c | `h2c.NewHandler`'s connections belong to `Shutdown` | The connection is hijacked, `Shutdown` returns nil in about 60µs, and in-flight requests keep running |
| client_golang | A non-conforming Namespace is an error | It is not; it is escaped on export, turning `my-app` into `my_app_` |
| The OTel SDK | `AlwaysSample` respects the upstream sampling decision | It ignores an upstream `sampled=00` and propagates `-01` downstream |

So when integrating a new library, or upgrading one:

1. **Measure every behavior sentence that goes into the documentation with code first**, rather
   than copying its README.
2. **A field we do not set explicitly is a default we accepted** — list those fields and ask of
   each one, "what is its default, and do I know it?".
3. Write the measured numbers (together with the dependency version) into the comments and the
   "Behavior (measured)" section of that module's README (cross-module ones go into
   [`behavior.md`](behavior.md)). Nobody has to measure again, and after a dependency upgrade a
   number that no longer matches is visible immediately.

### Mutation testing

`scripts/mutate.py` is the backstop for the point above: break the code behind each promise and see
whether a test fails. **A surviving mutant is a promise with no teeth.** Run it after changing
safety- or lifecycle-related code (`--only <module>` runs the relevant ones).

- The mutation tables live under `scripts/mutations/`, one file per Go module: `core.py` is the root
  module (directories inside it such as `./xhook` included), and the rest are named after the module
  directory, `xgin.py`, `xgorm.py`, `clickhouse.py`, `schemagen.py`. Which file a line goes into
  follows the directory its tests run in (the third argument), and `mutate.py` reports it outright
  when it is in the wrong one.
- Each mutation is one `mutate(name, file, dir, test filter, edits...)`, with a comment above it
  saying which incident this promise guards against. **Edits use `swap()` / `cut()` only**, which
  carry their own "the pattern matches exactly N places" assertion. A bare `s.replace()` rots in two
  ways: a pattern that no longer matches silently does nothing, and one matching several places
  breaks two things at once — the tests still go red, but no longer for the promise being checked.
- **When a refactor moves code, its mutations move with it.** A dead mutation pattern (one that
  changes nothing) is as bad as a change nobody noticed: that promise was not checked at all this
  round. The script reports that case separately and fails the whole run — it rotted that way twice
  already, when `applyConfig` lost `SetTrustedProxies` and when xerror changed `safeNew`'s
  signature, after which neither mutation ran again. `--dry-run` takes under a second and
  `check.sh` runs it every time.
- **Mutate the call site, not only the function being called.** "The metrics method label is
  folded" used to have one unit test calling `normalizeMethod` directly: the function was right,
  and nobody verified the middleware actually used it. Bypassing the call site
  (`normalizeMethod(m)` → `m`) still passed the tests — and that is exactly the shape of the bug.
- Only promises **we have code guarding** belong here. A property guaranteed by a dependency has no
  line to break, and tests alone are enough for those.

### Configuration and documentation

- Defaults are pre-filled in the struct, an unknown field is an error, an unset `${VAR}` is an
  error, and validation lives in `Validate()` (which runs while the configuration is read).
- After adding or changing a Config field: regenerate the schema and write it into the
  `## Configuration` section of that module's README (`check.sh` verifies both directions).
- **Documentation is organized per module.** The `README.md` in each module's directory holds all of
  that module's documentation, in four fixed sections (a section with nothing to say is left out),
  with `###` inside them:

  | Section | What goes in |
  |---|---|
  | `## Configuration` | The reference: a YAML block plus a few points. `check.sh` checks the fields against this section |
  | `## Behavior (measured)` | The long explanations, the library's defaults, the measured numbers and the dependency versions |
  | `## Observability` | The log messages and fields, the metrics, and the span names and attributes this module produces |
  | `## Troubleshooting` | This module's own error texts |

  `docs/` holds only what spans modules: `config.md` is the config file's loading, merging,
  placeholders and the "config block → module documentation" index, `behavior.md` is the summary
  table and the startup connection probe, `observability.md` is the global conventions of logs /
  metrics / tracing and the propagation rules, and `troubleshooting.md` is the errors that belong to
  no single module — configuration loading, `C()`, Runnable and shutdown, connecting. A sentence
  covering several modules at once goes into `docs/`, with the module READMEs linking to it.
- Code comments and tests refer to documentation as `xgin/README.md "Access log"` (a path plus a
  heading), and moving a heading means changing those too.
- The first ```go block in the root README is compiled by `example/readme_test.go`; update it when
  the public API changes.

### The changelog

**Nothing is recorded before the first version tag.** After that, a caller-visible change (a new
feature, a behavior change, an incompatible change, a fix, performance) goes **in the same commit**
into the `Unreleased` section of `docs/CHANGELOG.md`, classified under
"Incompatible changes / Added / Fixed / Performance", one sentence each, saying what the caller has
to know and what they have to change rather than implementation details. Refactors and tooling
changes inside the repository are left out. An incompatible change always states how to migrate
from the old spelling. At release time, `Unreleased` becomes `[vX.Y.Z] - date` and a new empty
`Unreleased` is opened above it.
