# xcron

In-process scheduled jobs: `xcron.Add` registers them and the framework starts and stops them. There is no config block; everything is written in code.

- Each execution gets a root Span (`cron <name>`), with the `job` log field and a `trace_id` in its ctx
- When the previous run is unfinished this one is skipped and a WARN is recorded, so nothing piles up (`AllowOverlap` makes them concurrent)
- A panic is caught and recorded at ERROR with the stack, leaving the other jobs and the process untouched
- On shutdown the ctx of the executions in flight is cancelled and the database is only closed once they have returned; any that have not returned by the deadline are named
- The spec is interpreted in UTC by default; `WithLocation` changes the time zone
- `RunOnStartAndWait`: run once as it comes up, and the service does not start if that run fails

## Quick start

**A web service with a few background jobs** -- the jobs run in the background and the service is unaffected:

```go
import (
	"github.com/jupiter-eng/xone"
	"github.com/jupiter-eng/xone/xcron"
	"github.com/jupiter-eng/xone/xgin"
)

func main() {
	if err := xcron.Add("*/5 * * * *", syncOrders); err != nil { // every 5 minutes (UTC)
		log.Fatal(err)
	}
	xcron.Add("@every 30s", refreshRates, xcron.RunOnStart()) // run once as it comes up, without waiting
	xone.MustRun(xgin.New().WithRoutes(routes))
}

func syncOrders(ctx context.Context) error {
	// the ctx is cancelled on shutdown; pass it downstream
	return xgorm.CWithCtx(ctx).Exec("...").Error
}
```

**A process running only scheduled jobs**:

```go
func main() {
	xcron.Add("0 3 * * *", cleanup, xcron.WithTimeout(30*time.Minute))
	xone.MustRun(xone.UntilSignal())
}
```

**Running once before startup and failing startup on error** (a migration, a warm-up) does not need xcron: write it as
`xhook.BeforeStart(migrate)`, which runs after the clients are built and before the servers start listening, and fails
startup when it returns an error. For a process that runs once and exits, use `xone.Func`.

## API

| Function | Description |
|---|---|
| `Add(spec, fn, opts...) error` | Registers a job. It validates on the spot, and the error is an `*xerror.Error` (module `xcron`): a malformed spec, a nil `fn`, and an empty or duplicate name are `config`, while an Add after shutdown has begun is `register` |

`spec`: the standard 5 fields `minute hour day month weekday`, or `@yearly` / `@annually` / `@monthly` / `@weekly` /
`@daily` / `@hourly` / `@every <duration>`. `<duration>` is `time.ParseDuration`'s form (`90s`, `1h30m`) and must be
greater than 0.

| Option | Default | Description |
|---|---|---|
| `WithName(name)` | the function name, below | Used for the log's `job`, the Span name and the errors; it must be unique within one process |
| `WithTimeout(d)` | no limit | The time limit on each execution, cancelling the ctx when reached; it also covers the first run of `RunOnStart*` |
| `AllowOverlap()` | not allowed | Start the next run even if the previous one is unfinished, so the two run concurrently |
| `WithLocation(loc)` | UTC | The time zone the spec is interpreted in |
| `RunOnStart()` | does not run | Run once in the background as the scheduler comes up, without waiting; a failure is only logged |
| `RunOnStartAndWait()` | does not run | Run once as the scheduler comes up and have startup wait for it; its failure fails startup |

**The default name** is the function name with the import path prefix and a method value's `-fm` suffix removed (measured on Go 1.25):

| What is passed | Default name |
|---|---|
| The ordinary function `cleanup` | `main.cleanup` |
| The method value `svc.Sync` (value receiver) | `main.Service.Sync` |
| The method value `svc.Sync` (pointer receiver) | `main.(*Service).Sync` |
| A closure | `main.main.func1` |

A closure's name is numbered by the compiler in order of appearance, so moving the code changes it and dashboards and
alerts break with it -- write `WithName` when passing a closure. Registering the same function twice (two specs, say)
collides, and the second one needs `WithName`.

## When they run and when they stop

The scheduler comes up on `StageServer`: the clients (xgorm, xredis, ...) and the caller's business hooks are all ready,
so a job can call `xgorm.C()` directly. A job `Add`ed before `xone.Run` starts together with the scheduler, and one
`Add`ed later (inside some hook, say) starts on the spot.

- **`RunOnStartAndWait`** jobs run their first execution in parallel during startup, and startup continues only once
  they have all succeeded; if any of them fails, startup fails and the error names which ones
  (`xone xcron start failed, err=[first run failed: job "load-config": ...]`). Their ctx is cancelled when a stop signal
  arrives during startup. For an `Add` after the scheduler is up, `Add` waits for the first run and returns its error,
  and a job whose first run failed is not registered.
- **Shutdown**: the web server stops first (waiting for the requests in flight), then xcron stops before anything else --
  it starts no new execution, cancels the ctx of those in flight, and waits for them to return within its share of the
  stop budget, after which the database and the cache are closed. Any that have not returned by then are named in the
  stop hook's error and in a WARN. Jobs keep being scheduled while the server is stopping.
- **A job can only be stopped if it watches the ctx**: Go has no way to stop a goroutine from the outside, and
  cancelling the ctx is all that can be done. xcron is the first stop hook, and how long it can wait is "the stop budget
  (`WithStopTimeout`, 15s by default) minus what the servers used, leaving at most 1s for each stop hook queued behind
  it": in a process running only scheduled jobs the servers take no time, and with 6 components such as xgorm, xredis
  and xlog attached it is about 9s; a web service using its full two thirds leaves a few hundred milliseconds. The rules
  are in [`docs/architecture.md` "The stop budget is one budget"](../docs/architecture.md#the-stop-budget-is-one-budget-not-one-per-component).

## Multiple replicas

**Every replica runs them.** xcron is an in-process scheduler and does not know whether other replicas exist; deploy 3
replicas and `0 3 * * *` runs 3 times a day. To have only one replica run it, take a lock, for example with xredis's
`SetNX`:

```go
xcron.Add("0 3 * * *", func(ctx context.Context) error {
	// the lock's lifetime: longer than the job's longest run, shorter than the interval between two runs.
	// The lock is not deleted when the job finishes: replica clocks differ by milliseconds, and deleting it early lets a late replica take it and run again
	got, err := xredis.C().SetNX(ctx, "lock:cleanup", hostname, 30*time.Minute).Result()
	if err != nil || !got {
		return err // another replica took it, so this round does not run
	}
	return cleanup(ctx)
}, xcron.WithName("cleanup"), xcron.WithTimeout(20*time.Minute))
```

`WithTimeout` must be shorter than the lock's lifetime: otherwise the lock expires while the job is still running, and
another replica can take it at the next time point.

## Observability

### Logs

| Message | Level | Fields |
|---|---|---|
| `cron job started` | DEBUG | `job`, `trace_id` |
| `cron job finished` | INFO | `job`, `trace_id`, `elapsed_ms` |
| `cron job failed` | WARN | `job`, `trace_id`, `elapsed_ms`, `error` |
| `cron job panicked` | ERROR | `job`, `trace_id`, `elapsed_ms`, `error`, `stack` |
| `cron job skipped, previous run still running` | WARN | `job` |
| `xcron ready` | INFO | `jobs` |
| `xcron jobs still running when the stop budget ran out` | WARN | `jobs` |

- `job` is in the job's ctx, so every line recorded inside the job with `slog.InfoContext(ctx, ...)` carries it; fields
  added inside the job with `xlog.AddKV(ctx, ...)` reach the end line too.
- The start line is recorded at DEBUG: an `@every 1s` job at INFO would be two lines every second, while the end line
  already carries `elapsed_ms` and the start time follows from it. A job that hangs shows no end line, and the error on
  shutdown names it.
- Using xcron brings xlog (as xgin does): a process running only scheduled jobs still logs JSON with a `trace_id`.

The global logging conventions are in [`docs/observability.md`](../docs/observability.md#logs).

### Tracing

| Source | Span name | Description |
|---|---|---|
| xcron | `cron <name>` | One root Span per execution, `SpanKindInternal`; on a failure or a panic the status is marked Error with the description `cron job failed` / `cron job panicked`, and the error text is not recorded (it often wraps an SQL or Redis error carrying parameter values; the text is in the log). The Spans of a job's database, Redis and outbound HTTP calls hang below it |

Using xcron brings tracing (it carries xtrace), with no separate import needed.

### Metrics

xcron exports no metrics: a process running only scheduled jobs has no `/metrics` endpoint, and pulling in a Prometheus
dependency for it is not worth it. To track duration per job, instrument it inside the job:

```go
start := time.Now()
err := cleanup(ctx)
xmetric.ObserveDuration("cron_job_duration", time.Since(start), xmetric.T("job", "cleanup"), xmetric.T("status", status(err)))
```

## Behavior (measured)

The measurement environment and the cross-module summary table are in [`docs/behavior.md`](../docs/behavior.md).

hashicorp/cronexpr v1.1.3 (used only for parsing and for computing the next time point; the scheduling loop is xcron's
own) and Go 1.25. After a cronexpr upgrade, whichever of the `TestCronexpr_*` group in `schedule_test.go` goes red, the
matching entry here has to be measured again.

**cronexpr accepts more than 5 fields; only 5 are accepted here.**

| spec | cronexpr itself | xcron |
|---|---|---|
| 6 fields `0 30 2 * * *` | The extra field is the **year**, not seconds: it reports `syntax error in hour field: '30'`; `30 2 * * * 2027` only starts running in 2027 | `want 5 fields` |
| 7 fields | An extra leading field of seconds and a trailing one of years | `want 5 fields` |
| 8 fields or more | Everything after the 7th is dropped silently | `want 5 fields` |
| `CRON_TZ=Europe/Berlin 30 2 * * *` | `syntax error in minute field: 'CRON_TZ=Europe/Berlin'` | Points at `WithLocation` |
| `@midnight`, `@reboot`, `@every 1m` | Always `missing field(s)` | `@every` is implemented here; the rest report `unknown descriptor` |
| `0 0 30 2 *` (30 February) | It parses and `Next` returns the zero value: the job is registered and never runs | `spec never fires` |

It also accepts `L`, `W`, `#`, `?` and English abbreviations for months and weekdays (`MON-FRI`, `JAN`), which xcron does
not block.

**Time zones**: cronexpr computes the next time point in the time zone of the moment passed in -- the same
`30 2 * * *` gives 02:30 Berlin for a Berlin time and 02:30 UTC for a UTC time. `WithLocation` simply converts "now" to
that time zone before handing it over; without it the zone is UTC, regardless of the machine's `TZ`.

**Daylight saving** (`WithLocation(America/New_York)`, the clock going forward from 02:00 to 03:00 on 2026-03-08 and back
from 02:00 to 01:00 on 2026-11-01):

| spec | The day the clock goes forward | The day it goes back |
|---|---|---|
| `30 2 * * *` | 02:30 does not exist, so **the whole day is skipped** (the next run after 3-07 is 3-09) rather than moved to 03:30 | Runs once (02:30 EST, 07:30 UTC) |
| `30 1 * * *` | As usual | **Runs twice**: 01:30 EDT and 01:30 EST, an hour apart |
| `0 * * * *` | 01:00 is followed by 03:00 | 01:00 runs twice |

In a time zone with daylight saving, a job falling between 01:00 and 03:00 either skips a day or runs twice on one. To
avoid that, keep the default UTC.

**The next time point is strictly after "now"**: computing `* * * * *` from exactly 10:01:00 gives 10:02:00. When the
wall clock is turned back (an NTP correction), xcron computes forward from the previous time point and never runs the one
that has just run again.

**`@every <d>` does not align**: it counts `d` forward from the moment of each trigger -- an `@every 1m` starting at
10:00:17.5 next runs at 10:01:17.5, aligned neither to a whole minute nor with milliseconds truncated. The first run is
`d` after the scheduler comes up (for an immediate first run, write `RunOnStart`). Counting forward from the actual
trigger each time makes timer delays accumulate: measured, an `@every 10ms` running for 2s was about 0.32ms late per run
(4-core Linux). An `@every 1m` accumulates about 0.5s a day, so anything that has to align to the hour uses the 5-field
form.

**A time point arriving while the previous run is unfinished**: it is skipped by default and
`cron job skipped, previous run still running` is recorded, with the next time point proceeding as usual; so a job
slower than its own interval actually runs at "the next time point after it finishes". With `AllowOverlap` every time
point starts one.

## Troubleshooting

### `xone xcron config failed, err=[job "main.main.func1": bad spec "...": want 5 fields ...]`

The spec does not have 5 fields. The second-level form (6 fields) is not accepted; for second-level scheduling use
`@every 10s`. cronexpr's 6-field form means "with a year", see the table above.

### `duplicate job name "main.cleanup": give one of them xcron.WithName`

The same function was registered twice, or two closures happen to have the same name. Give one of them a `WithName`.

### `xone xcron register failed, err=[cannot add job ...: the scheduler is shutting down]`

`Add` was called after the shutdown sequence had begun, most likely after `Start` returned or inside a stop hook.

### `xone xcron stop failed, err=[jobs [cleanup] still running when the stop budget ran out: context deadline exceeded]`

These jobs had not returned when the stop budget ran out: either they do not watch the ctx, or their cleanup takes longer
than the budget. Have the job pass the ctx to every blocking call it makes, or raise `xone.WithStopTimeout`.
