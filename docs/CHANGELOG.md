# Changelog

This file records the **caller-visible changes** of every version: new features, behavior changes,
incompatible changes, fixes and performance. Refactors and tooling changes inside the repository
are left out, unless they change what a caller has to do.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the version numbers
follow [Semantic Versioning](https://semver.org/). Every module shares one version number (see
`scripts/release.sh`).

## [Unreleased]

## [v1.4.3] - 2026-10-28

### Fixed

- **xone:** cancel the running context before server and component cleanup when startup fails or
  the runnable returns on its own, so background work can stop before its dependencies close.
- **xkafka:** include waiting for the poll loop in the shutdown budget. A rebalance waiting for an
  in-flight handler no longer prevents the stop deadline from cancelling that handler.
- **xcron:** jobs added during the first startup executions follow startup cancellation; failed
  startup also waits for those dynamically added jobs to exit before returning, for `5s` at most --
  the stop hook that would otherwise bound that wait never runs, so a job whose function ignores
  its `ctx` is named in one `WARN` rather than holding startup for ever.
- **xconfig:** a `--profile` or `XONE_PROFILE` override no longer expands placeholders in the
  replaced `XApp.Profiles` value, so an unused environment variable cannot prevent startup.
- **xbeego:** informational responses such as `103 Early Hints` no longer consume the final
  response status. `101 Switching Protocols` still counts as a final response.

## [v1.4.2] - 2026-10-28

### Fixed

- **xkafka: a partition that has lost its goroutine keeps saying so.** When this module's own code
  panics inside a partition's goroutine, that goroutine is given up rather than taking the process
  down, and one `ERROR` says so. What followed had no sign of its own: `poll` goes on fetching that
  partition and dropping every batch, nothing is marked so the committed offset stands still, and
  nothing is counted -- the process keeps its heartbeat, the other partitions carry on, and the
  state mends itself only at the next rebalance, which may be days away. The single line at the
  moment it happened was the only trace, and anybody looking later saw a healthy consumer. The
  dropped batches are now recorded as one `WARN` per partition per minute (`kafka records dropped`),
  with the topic, the partition, the group, how many records and the offset they start at. Nothing
  about what is kept or committed changes: no message was ever lost here, and none is now.

- **xkafka: `DialTimeout: 0` is franz-go's own 10s, which is what the documentation always said it
  was, rather than no timeout at all.** The option was handed to franz-go whatever its value, and
  franz-go fills its 10s in only where the option was never given: a 0 passed through was taken as
  it stood and reached a `net.Dialer{Timeout: 0}`, a dial bounded by nothing but the caller's `ctx`
  and the operating system's own TCP timeout. Startup was bounded all the same -- each attempt of
  the connectivity probe carries a budget of its own -- but a dial during a metadata refresh, a
  broker change or a rebalance was not, and a broker that accepted the connection and fell silent
  held that dial far past the second it was configured for. The option is now left out when
  `DialTimeout` is 0, which is what makes the default apply; any other value behaves as before.

- **xgin / xecho / xbeego: a request body in `x-www-form-urlencoded` that the access log had to cut
  short no longer lets a DSN password through.** A body reaching the 256KB cap is cut in the middle
  of whatever key/value pair happened to be there, and the masking was never told so on this branch:
  `dsn=postgres%3A%2F%2Fapp%3Apw%40db...` cut before the `%40` decodes to `postgres://app:pw`, which
  holds no sensitive word and no `@` any more, so nothing located the password and it was written
  into the log as it stood -- the very failure the JSON branch is guarded against. Only the last
  pair of a form can be incomplete, and it is now dropped rather than recorded, with
  `[truncated, the rest was not recorded]` after what was complete before it; a cut body holding a
  single pair records `***REDACTED***`. It only ever reached a request body with
  `LogRequestBody: true`: a response body never takes the form branch, and a query string is never
  cut.

- **xcron: a job whose schedule has no time point left stops being scheduled instead of spinning.**
  `Next` returning the zero time means "there is no next one", and the loop measured the wait to it
  as a large negative duration: the timer fired at once, the next round worked out the same zero
  time, and it went round as fast as the CPU allowed -- starting an execution every round, or
  writing one `cron job skipped, previous run still running` per round. A spec that never fires is
  refused when the job is added, so this was reachable only at the far end of one that does fire:
  cronexpr searches the years 1970 to 2099, which `0 0 29 2 *` runs past in 2096. One `WARN`
  (`cron job has no next time point and will not run again`, with `job`) is now written and that
  job's loop returns; the other jobs and the scheduler are untouched.

## [v1.4.1] - 2026-10-27

### Fixed

- **xbeego: an error handler registered under a name is rendered again, instead of being reported
  as a crash.** `web.ErrorController` registers every method it finds under a name rather than a
  status code -- `ErrorDb` becomes `"Db"` -- and `c.Abort("Db")` then panics with that name.
  Nothing exported reaches such a handler: `web.Exception` takes a numeric code and `errorInfo`'s
  fields are unexported. So the response lost its body, and the request was written as `panic while
  handling request` at `ERROR` with a stack -- a line saying a request had failed while everything
  else said it had succeeded, since a name is not a code and `c.Abort("Db")` leaves the status at
  **200**. An abort onto a registered error handler is now handed back to the recovery that stood
  in beego's slot before xbeego took it over, which is the one piece of code that can render it,
  and the response is the one plain beego answers with. `recoverpanic = false` in `conf/app.conf`
  switches that recovery off; it then declines, and such a response still goes out without its body
  -- as one `WARN` naming the code and the path, not an `ERROR` with a stack. A numeric code is
  rendered through `web.Exception` in that case and is unaffected. Neither is answered through
  `WithRecoverFunc` any more: that is the answer to a panic, and this is control flow.
- **xbeego: `RecoverPanic` and `BeegoLogs` are settled by the first assembly, which removes a data
  race.** Both land on beego globals that are read while requests are being served: beego reads
  `BConfig.RecoverFunc` on every request, and its `logs` package walks its adapter list without
  holding a lock. Writing them from a second server assembling while the first was already
  answering was a data race, which the race detector reports. They are now written once, by the
  first assembly, and an instance assembling later with a different value gets a `WARN` saying its
  own values were ignored instead of having them taken under the first server's feet. **Behavior
  change**: it used to be whichever instance assembled with the switch on. Two servers in one
  process built the documented way -- the second from `CurrentConfig()` -- agree on both and are
  unaffected. This settles the configuration, not beego's router: registering routes or filters on
  an instance assembled while another is already answering remains a data race of its own, so
  assemble every instance before starting any.
- **xbeego: the access log's `response_body` no longer comes up empty for a response written
  through `io.Copy`.** The recorder's `ReadFrom` handed the bytes straight to the writer underneath,
  which counts them but never sees them, so with `LogResponseBody` on the field was empty beside a
  correct `bytes_out`. While the body is being captured the bytes now travel through `Write`; the
  sendfile path is still taken for every request whose body is not recorded. It is reachable from
  middleware mounted with `WithMiddleware`, not from a beego controller, whose `context.Response`
  implements neither `io.ReaderFrom` nor `Unwrap`.
- **xgin: `http.ResponseController` keeps reaching the connection while `LogResponseBody` is on.**
  The writer capturing the response body did not hand the writer below back, and
  `http.ResponseController` walks down through exactly that: `SetReadDeadline`,
  `SetWriteDeadline` and `EnableFullDuplex` all answered `http.ErrNotSupported` as soon as the
  response body was recorded, so SSE and long polling lost the deadlines they set per chunk by way
  of a logging switch. `Flush` and `Hijack` were never affected, being gin's own writer underneath.
- **xgin: a panic ends the handler chain, whatever `WithRecoverFunc` answers with.** A recovered
  panic unwinds the handler that threw it, but gin's `Next()` goes on with the one after it, and
  only the default `RecoverFunc` stopped that (it answers through `AbortWithStatus`). A
  `RecoverFunc` of the caller's own that writes a response and returns let the request carry on:
  middleware that panicked while turning a request away was followed by the handler it stands in
  front of, which appended its own body to the response already sent. The chain is now abandoned in
  both cases.
- **xgin: `ZHTranslations` with gin's validator disabled is reported instead of panicking.**
  `gin.DisableBindValidation()` sets `binding.Validator` to nil, and asking a nil interface for its
  `Engine` panics -- during assembly, where it took the whole server down over a translation that
  could not have been used anyway. It is now the same `xgin` / `register` error the other failures
  there are, which assembly logs as a WARN.
- **xmetric: a label name holding a comma no longer panics inside a business call.** The collector
  cache was keyed on the label names joined by a separator, and nothing checks what a name holds:
  one label called `method,path` and the two labels `method` and `path` produced the same key, so
  whichever call came second reached a collector built for the other number of labels. That is a
  `WithLabelValues` with the wrong count, which panics out of whichever business call made it --
  while a negative `CounterAdd`, an unregistrable metric and a name already taken by another type
  are all one `ERROR` line and a recording that goes nowhere. A refactor splitting one tag into two
  and missing a call site was enough. The key now carries each piece's length and cannot be read
  two ways; the two spellings get their own collectors, and the second one to register is refused
  with that same `ERROR` line.
- **xlog: a log line that could not be written is reported, and one failing output no longer takes
  the others with it.** `slog` discards what a handler returns (`Logger.log` in `log/slog`), so a
  full disk, an fd that went away or a log file somebody removed looked exactly like a service with
  nothing to say -- the failure rotation warns about one file over, for the same reason. It is now
  one stderr line through the same rate-limited path, at most once a minute. The outputs were also
  assembled with `io.MultiWriter`, which returns at the first writer that fails, and the console
  comes first: with `Console` and `File` both on, a console whose reader had gone (the collector
  closing the pipe) silenced a perfectly writable log file. Every output now gets the line.
- **xlog: an empty `File.Name` fails startup.** It was the one file setting that did not fail on its
  own: with a `Path` the writer took the directory itself for the symlink and the error talked about
  symlinks, and with `Path` empty as well it started cleanly and wrote a hidden file named after
  nothing but the time suffix into the working directory.
- **xtrace: a header name in `ForwardHeaders` / `ForwardHeaderRules` that is not an HTTP field name
  fails startup.** `X-Tenant-Id:` with a stray colon, or a name with a space in it, passed the
  configuration and then matched nothing an inbound request carries -- net/http hands over canonical
  names and nothing else -- so it forwarded nothing, in silence, while the configuration read as if
  it did. `Domains` has been held to that standard for the same reason: this configuration decides
  who internal identifiers are sent to.
- **xginswagger: two engines mounting the Swagger UI on one spec no longer race.** The metadata is
  filled on the first visit to the document, guarded by a `sync.Once` -- but that Once belongs to
  the `Register` call while the `*swag.Spec` it fills is the one swag registered globally. A public
  server and an admin one, which is what `xgin`'s `WithConfig` is for, hold one Once each, so the
  first request to each filled the same object: measured, the race detector reports the two fills
  writing at once, and one engine's request reading what the other is writing is the same race one
  step on. The values written are identical (both read the same `XGinSwagger` block), so what was at
  stake was the race itself rather than the contents. The fills of all of them are now kept apart
  from the reads.
- **xutil: a `Retry` timeout of 0 means no limit on an attempt, rather than an attempt that has
  already expired.** Taken as a number, the round budget became the sum of the backoffs -- a couple
  of seconds -- and every attempt was handed a ctx that was already past its deadline before `fn`
  saw it: measured, three attempts and three dead ctxs, each failing the instant it started for
  anything that honours its ctx. It now means what it means for net/http's own timeouts, with the
  round bounded by the parent ctx alone. Every caller in this repository configures a positive
  timeout and fails startup without one, so nothing inside it behaved differently.

## [v1.4.0] - 2026-10-23

### Added

- **`XONE_BANNER=off` leaves the startup banner unprinted.** The banner has always been limited to
  a terminal, which is the judgement that matters in a container or behind a log collector; what it
  had no answer for was somebody who sees it and does not want it. `0`, `false`, `no` and `off`
  turn it off, in any case and with surrounding whitespace ignored, and anything else -- a
  misspelling included -- leaves it on, which is what tells somebody their spelling was wrong.
  It is read the same way as `XONE_CONFIG`, `XONE_PROFILE` and `XONE_DEBUG`, so the knob sits where
  somebody already looks, and no code change or rebuild is needed to quieten one's own shell. It
  cannot turn the banner **on**: whether this is a place for multi-line ASCII art at all stays the
  framework's decision, and setting it inside a container still prints nothing.
- **`XKafka.Consumer.SessionTimeout`**: how long the broker waits for a heartbeat before it takes a
  consumer for gone and hands its partitions to another member of the group. It is the time a
  partition stops being consumed after a process is killed outright -- one that exits through its
  stop hooks leaves the group on its way out and its partitions move at once, while one that was
  SIGKILLed, fenced or lost with its host is only noticed when this runs out. Lower it for a faster
  take-over, at the price of a rebalance whenever a pause (a long GC, a stalled network) outlasts
  it. Left unset it keeps franz-go's own default of 45s, so nothing changes for a service that does
  not write it. The floor is 3s, franz-go's heartbeat interval, and the broker's
  `group.min.session.timeout.ms` (6s by default) and `group.max.session.timeout.ms` apply on top;
  a value outside those is refused when the consumer joins the group rather than at startup.

### Fixed

- **A DSN password no longer reaches the access log in the clear when the body was cut right
  before the `@`.** The fast path that skips the masking for a body holding nothing sensitive goes
  by three tests, all of which assume a complete body — and the cut can take away the very
  character one of them looks for. Measured, `{"dsn":"postgres://app:pw@db:5432/prod"}` cut before
  the `@` holds no sensitive word, no backslash and no `@` any more, so it took that path and the
  password was written out as it arrived; the parsing branch would not have masked it either, since
  the expressions that recognize a DSN need that `@` as well. A body as long as the cap is now
  treated as one that may have been cut and never takes the fast path. A complete body is
  unaffected, which is every ordinary request. **This applies to the JSON branch only** — the same
  cut leaks through a form, plain text or XML body, which is tracked separately.
- **A JSON body the access log had to cut is recorded as far as it was read, instead of as
  `***REDACTED***`.** The log records a response up to 4KB and a request up to 256KB, so anything
  larger arrives at the masking cut in the middle of a field; a body that did not parse was then
  masked whole. The literal pre-check hands anything holding a backslash to the parser, and `\"`
  and `\n` are everywhere in JSON, so in practice every large JSON body was logged as
  `***REDACTED***` — the switch was on, nothing of the body was visible, and the line read as
  though a credential had been found. It is now decoded as far as the decoder gets, member by
  member and into nested values (`{"page":1,"items":[…]}` cut inside the list keeps the envelope
  and the elements that were read), masked by the usual rules, and followed by
  `[truncated, the rest was not recorded]`. Everything from the cut onwards is dropped rather than
  printed, so a key whose letters were written as `\uXXXX` escapes is still masked: the decoder
  turns those back before the word list sees it. A body nothing could be read out of records
  `[unparseable content omitted]`, and `***REDACTED***` goes back to meaning only "a sensitive
  field was found and masked".
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
