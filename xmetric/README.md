# xmetric

Prometheus metrics: the native `*prometheus.Registry` comes from `xmetric.Registry()`, and day-to-day recording has a set of shortcuts that save the boilerplate.

- The metrics endpoint is mounted at `/metrics` automatically by xgin / xecho / xbeego; without them, mount `xmetric.Handler()` yourself
- The shortcuts build metrics by name and reuse them by labels, with nothing to declare first
- Duration metrics take a `time.Duration` and get the `_seconds` suffix automatically
- `log_errors_total` (a count of Error-level logs) and `go_*` / `process_*` come with it
- A malformed name, label or bucket set fails while the configuration is read rather than being quietly escaped on export

## Quick start

```yaml
# conf/application.yml (optional)
XMetric:
  Namespace: shop          # every metric name gets the prefix shop_
  ConstLabels:
    env: "${ENV:dev}"      # attached to every metric
```

```go
import "github.com/jupiter-eng/xone/xmetric"

func Place(ctx context.Context, o *Order) (err error) {
	start := time.Now()
	defer func() {
		status := "ok"
		if err != nil {
			status = "error"
		}
		xmetric.CounterInc("orders_total", xmetric.T("status", status))
		xmetric.ObserveDuration("order_place", time.Since(start), xmetric.T("status", status)) // exported as order_place_seconds
	}()
	return save(ctx, o)
}

// Shorter when the labels do not depend on the outcome: defer xmetric.Timer("order_query")()
```

## Configuration

```yaml
XMetric:
  Namespace: myapp         # the metric name prefix, none by default
  ConstLabels:             # attached to every metric (including go_* / process_*), none by default
    env: "${ENV:dev}"       # service_name / service_version come from XApp by themselves; an entry of that name wins
  HTTPDurationBuckets: [0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10]  # inbound and outbound HTTP duration (seconds); this is the default
  HistogramBuckets: [0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10]           # the histograms the shortcuts build, that is prometheus.DefBuckets
  GoMetrics: true          # the Go runtime metrics, on by default
  ProcessMetrics: true     # the process metrics, on by default
  LogErrorMetric: true     # logs at Error level and above counted into log_errors_total, on by default; only logs going through xlog are counted
```

- Either bucket set left out means the defaults, and writing one replaces it whole; buckets must be strictly increasing.
- The path of the metrics endpoint is `XGin.MetricPath` / `XEcho.MetricPath`.

## API

| Function | Description |
|---|---|
| `CounterInc(name, tags...)` / `CounterAdd(name, v, tags...)` | Increments a counter by 1 / by v (v must be >= 0; a negative value is written to the log as an `ERROR` and recorded nowhere) |
| `GaugeSet(name, v, tags...)` / `GaugeInc` / `GaugeDec` | Gauges |
| `HistogramObserve(name, v, tags...)` | A histogram observation in seconds, with the buckets from `XMetric.HistogramBuckets` |
| `ObserveDuration(name, d, tags...)` | Records one duration; the `time.Duration` is converted to seconds and the name gets `_seconds` |
| `Timer(name, tags...) func()` | `defer xmetric.Timer("x")()`; the labels are fixed at the start and calling it again has no effect |
| `TrackInFlight(name, tags...) func()` | The number in progress, `defer xmetric.TrackInFlight("x")()`, with no missed decrement |
| `T(name, value) Tag` | Builds one label |
| `Registry() *prometheus.Registry` | The native Registry, for full control with your own `NewCounterVec` and registration |
| `MustRegister(cs...)` | Registers a custom collector; registering the same one twice panics |
| `Register(c)` / `RegisterAs[T](c) (T, error)` | Registers, reusing an existing instance with the same name and labels; **always use the return value** |
| `ConstLabels()` / `Namespace()` / `HTTPDurationBuckets()` | Reads the configuration in effect, to fill into `prometheus.Opts` so your own metrics carry the same labels as the framework's |
| `Handler() http.Handler` | The `/metrics` handler, to mount yourself in a service not using xgin / xecho / xbeego |
| `New(cfg) (*Metrics, io.Closer, error)` | The pure constructor: touches no globals, reads no config file, usable outside the framework |

## Notes

- **A malformed name fails while the configuration is read**: `Namespace` and the keys of `ConstLabels` accept only
  letters, digits and underscores and must not start with a digit, and a `ConstLabels` key must not start with `__`
  either (client_golang reports no error and quietly exports `my-app` as `my_app_...`). See
  ["Behavior (measured)"](#behavior-measured).
- **The application identity is attached by itself**: `XApp.Name` and `XApp.Version` become the constant labels
  `service_name` and `service_version`, named as xlog's log fields are and as `service.name` / `service.version` on
  the spans (the rule is in
  [observability.md "One application, three signals"](../docs/observability.md#one-application-three-signals)).
  Configuring neither attaches neither label -- a constant `service_name=""` would say nothing on every query -- and
  a `ConstLabels` entry of the same name overrides what `XApp` says. Note that `name` and `version` are reserved
  (see below), which is why the labels are not called that.
- **Some `ConstLabels` keys are reserved**: `le`, `quantile`, `version` and the framework metrics' own variable labels
  (`level`, `caller`, `method`, `status`, `route`, `host`, `name`) cannot be used and fail while the config is read.
- **An empty bucket list `[]` does not mean "use the defaults"** and fails outright; for the defaults, leave the field
  out.
- **`Register` returns an error for a same name with a different type or different labels**: the one passed in is not in
  the registry and the values recorded through it cannot be exported.
- **A label name the shortcuts are given is not checked**, unlike `ConstLabels`: client_golang takes it and exports a
  non-conforming one escaped (`T("method,path", ...)` comes out as `method_path`). What it does not do is take the
  recording down with it — one spelling never lands on another's collector, and a second spelling of the same metric
  that disagrees about the labels is one `ERROR` line and values that go nowhere, as everywhere else here.
- The framework's own metrics are in [observability.md "Metrics"](../docs/observability.md#metrics).

## Observability

### Metrics

| Metric | Type | Labels | Source |
|---|---|---|---|
| `log_errors_total` | counter | `level`, `caller` | xmetric, `XMetric.LogErrorMetric`, requires xlog |
| `go_*` / `process_*` | — | — | `XMetric.GoMetrics` / `ProcessMetrics` |

The metric name prefix, the constant labels and a few general rules are in
[`docs/observability.md` "Metrics"](../docs/observability.md#metrics).

### Process metrics in a container

`ProcessMetrics` registers client_golang's `NewProcessCollector` with no options, so every number
below is read out of `/proc/<pid>` for the process that registered it -- its own PID, taken once
when the collector is built. Inside a container that is **this process**: not the container, and
not the Pod around it.

| Metric | Read from | Scope |
|---|---|---|
| `process_cpu_seconds_total` | `/proc/<pid>/stat`, utime + stime | this process |
| `process_resident_memory_bytes` | `/proc/<pid>/stat`, rss | this process |
| `process_virtual_memory_bytes` | `/proc/<pid>/stat`, vsize | this process |
| `process_start_time_seconds` | `/proc/<pid>/stat` | this process |
| `process_open_fds` | `/proc/<pid>/fd` | this process |
| `process_max_fds` | `/proc/<pid>/limits`, `RLIMIT_NOFILE` | this process |
| `process_virtual_memory_max_bytes` | `/proc/<pid>/limits`, `RLIMIT_AS` | this process |
| `process_network_receive_bytes_total` | `/proc/<pid>/net/netstat` | the whole network namespace |
| `process_network_transmit_bytes_total` | `/proc/<pid>/net/netstat` | the whole network namespace |

Three things to know before an alert is built on them:

- **`process_resident_memory_bytes` is not the number an orchestrator kills on.** That one belongs
  to the cgroup (`container_memory_working_set_bytes` as cAdvisor exports it) and counts page cache
  and the container's other processes as well. Held against the container's memory limit, this
  metric under-reports.
- **`process_cpu_seconds_total` cannot show cgroup throttling.** A service throttled to a crawl by
  its CPU limit draws a flat line here; the cgroup's own throttling counter is the one that moves.
- **The two network counters cover the network namespace**, which the containers of one Pod share,
  so a sidecar's traffic is counted in.

What they are for is the view from inside the process, which no cgroup metric gives:
`process_open_fds` against `process_max_fds` is the only warning of a descriptor leak,
`process_start_time_seconds` jumping means the process restarted while the container did not, and
`process_resident_memory_bytes` minus `go_memstats_heap_inuse_bytes` growing steadily points at
something outside the Go heap (cgo, mmap, thread stacks).

The set is not the same everywhere. Measured on darwin, the nine above come out as seven -- there
is no `process_network_*` at all -- and as five with `CGO_ENABLED=0`, which drops
`process_resident_memory_bytes` and `process_virtual_memory_bytes` as well. A dashboard built
against what a development machine exports is missing what the service exports in production.

## Behavior (measured)

The measurement environment and the cross-module summary table are in [`docs/behavior.md`](../docs/behavior.md).

client_golang v1.24.1.

| What is written | Without catching it (measured) |
|---|---|
| Buckets that are not strictly increasing (`[1, 0.5, 2]`, `[1, 1]`), or that contain NaN / Inf | Startup passes and the first `HistogramObserve` panics inside a business request |
| An empty bucket list `[]` | Not "use the defaults": Prometheus quietly replaces it with its own `DefBuckets` (the HTTP set then lacks the 1ms bucket) |
| A non-conforming `Namespace` or `ConstLabels` key | No error, escaped on export: `my-app` is exported as `my_app_...`, `1app` as `_app_...`, and non-ASCII text as a string of underscores |
| A `ConstLabels` key of `le` / `quantile` | `le` passes startup and panics at the first `HistogramObserve`; `quantile` makes building a Summary panic on the spot |
| A `ConstLabels` key colliding with a framework metric's variable label | That metric fails to register, one error log is written, and none of that group is ever exported |
| A `ConstLabels` key of `version` | `go_info` carries the constant label `version`, so client_golang refuses to register the whole Go runtime group (`attempted wrapping with already existing label name "version"`) |

Which is why all of these fail while the configuration is read.
