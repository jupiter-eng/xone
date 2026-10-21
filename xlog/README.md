# xlog

Logging: once it is set up, `slog.Default()` is a native `*slog.Logger` configured from `XLog`, and business code uses the standard library's `log/slog` directly.

- A core module with no dependencies, arriving only with xgin / xecho / xcron: using any of them sets it up, and without a configuration it is info-level JSON on standard output
- The root package `xone` does not carry it. A program using only the data integrations (xgorm, xredis, xhttp, xcache) or xtrace / xmetric does not get xlog installed and `slog.Default()` stays as it was; to use it, `import _ "github.com/jupiter-eng/xone/xlog"`. Writing an `XLog` block without that import fails startup and suggests exactly that line
- With tracing, every log line carries `trace_id` / `span_id` automatically
- `xlog.AddKV` adds fields for a whole request (the access log carries them too), `xlog.CtxWithKV` only for one stretch of calls
- File output rotates by time and cleans up by retention, deleting only files it named itself
- Timestamps can be rendered in a given time zone

## Quick start

```yaml
# conf/application.yml (optional)
XLog:
  Level: info
  File: {Enable: true, Path: /var/log/app}
```

```go
import (
	"context"
	"log/slog"

	"github.com/jupiter-eng/xone/xlog"
)

func Create(ctx context.Context, userID string, orders []Order) {
	xlog.AddKV(ctx, "user_id", userID) // every later log line of the same request (the access log included) carries it

	for _, o := range orders {
		ctx := xlog.CtxWithKV(ctx, map[string]any{"order_id": o.ID}) // only this round's logs carry it
		slog.InfoContext(ctx, "order created", "amount", o.Amount)
	}
}
```

Always use the ctx-carrying `slog.InfoContext`, which is what brings the `trace_id` and the fields above.
Log messages and field names are best written in English: they become the search terms and the JSON keys in the logging platform.

## Configuration

```yaml
XLog:
  Level: info              # debug / info / warn / error, info by default
  Format: json             # json / text, json by default
  AddSource: false         # record the source location; it costs something and is off by default
  Timezone: ""             # the IANA time zone the timestamps are rendered in, such as Europe/Berlin; empty = the process's local time zone
  Console: true            # write to standard output, on by default
  File:
    Enable: false          # off by default
    Path: ""               # the directory, empty by default = the process's working directory; created when it does not exist
    Name: app.log          # app.log by default; the actual file carries a time suffix, with a symbolic link of the same name pointing at the current file
    RotateTime: 24h        # the rotation period, aligned to the local time zone, one day by default, at least 1m
    MaxAge: 168h           # the retention of historical files, 7 days by default; 0 = no cleanup, a negative value fails startup
    Perm: "0644"           # a string parsed as octal, "0644" by default; 644 and 0o644 are accepted too
  Fields: {}               # static fields carried on every log line, values may use ${VAR}; see "The fields every log line carries" below
```

- The file name suffix follows `RotateTime`'s granularity: `app.log.20260918` at ≥ 24h, `app.log.2026091815` at ≥ 1h, and
  `app.log.202609181504` below that. The finest is the minute, so anything shorter than 1m fails startup (0s would really
  give one file per minute, and 30s would put two periods in the same file name).
- Cleanup runs once at startup and once per rotation after that, deleting only `app.log.<time suffix>`; an `app.log.bak`
  or `app.log.1.gz` is never touched.

## The fields every log line carries

The framework adds "who logged this" to every line by default, matching `service.name`, `host.name` and `process.pid`
on xtrace's Spans:

| Field | Source | Description |
|---|---|---|
| `service` | `XApp.Name` | Not written when unconfigured |
| `version` | `XApp.Version` | Not written when unconfigured |
| `hostname` | `os.Hostname()` | The machine name on a VM, the Pod name in K8s. It is not called `host`: in xgin's access log, `host` is the request's Host header |
| `pid` | `os.Getpid()` | Tells several instances on one machine, and the runs before and after a restart, apart |

Anything only the deployment environment knows (the Pod, the node, the namespace, the data center, the Pod IP) is
injected from the environment through `XLog.Fields`. The framework does not guess: a machine often has several network
interfaces, and an automatically chosen IP may be the wrong one while looking like the right one.

```yaml
XLog:
  Fields:
    pod_ip: ${POD_IP:}          # ${VAR:} is an empty string when unset, and a field with an empty string is not written
    node: ${NODE_NAME:}
    namespace: ${POD_NAMESPACE:}
    hostname: ""                # a name of the same name wins, and an empty string drops that default field
```

```yaml
# K8s: put these values into the environment with the Downward API
env:
  - {name: POD_IP, valueFrom: {fieldRef: {fieldPath: status.podIP}}}
  - {name: NODE_NAME, valueFrom: {fieldRef: {fieldPath: spec.nodeName}}}
  - {name: POD_NAMESPACE, valueFrom: {fieldRef: {fieldPath: metadata.namespace}}}
```

- When the logs go to stdout and are collected by Fluent Bit / Filebeat / Vector, the collector has most likely attached
  the Pod, the namespace and the node already and none of this is needed.
- These values are computed and serialized once at startup; the cost is in ["Behavior (measured)"](#behavior-measured).
- They are carried with another backend through `UseHandler` too; `New` is a pure constructor and carries only
  `cfg.Fields`, not the 4 defaults.

## API

| Function | Description |
|---|---|
| `AddKV(ctx, k, v)` / `AddKVs(ctx, map[string]any)` | Writes fields into the current scope in place, carried by every later log line of the same request |
| `CtxWithKV(ctx, map[string]any) context.Context` | Derives a new ctx carrying these fields, carried only by logs written with it; it affects neither the parent ctx nor its siblings |
| `CtxWithScope(ctx) context.Context` | Opens a field scope, for a non-web entry point (consuming a message, running a job); an existing scope is returned as it is |
| `DroppedKVCount() int64` | The number of `AddKV` fields dropped for want of a scope; a non-zero value most likely means a missing `CtxWithScope` |
| `Location() *time.Location` | The time zone in effect: `t.In(xlog.Location()).Format(time.RFC3339)`; `time.Local` when unconfigured |
| `TraceIDs(ctx) (traceID, spanID string)` | The current ctx's trace identifiers, the two values written into the logs; two empty strings when there are none |
| `UseHandler(h slog.Handler)` | Have the logs written by a handler of your own (a slog bridge for zap, a company logging SDK, ...), called before `xone.Run`; see ["Using your own logging backend"](#using-your-own-logging-backend) |
| `New(cfg) (*slog.Logger, io.Closer, error)` | The pure constructor: touches no globals, reads no config file, usable outside the framework |

`SetTraceExtractor` / `AddObserver` (and the types `TraceExtractor` and `Observer`) are extension points for
integrations to inject capabilities and are not for business code; xtrace and xmetric use the same injection point
(`internal/logext`) without importing xlog.

## Using your own logging backend

The entry point is always `slog`; what changes is only who does the final writing. Hand the handler to xlog before
`xone.Run`:

```go
func main() {
	xlog.UseHandler(zapslog.NewHandler(core)) // or any slog.Handler
	xone.MustRun(xgin.New().WithRoutes(routes))
}
```

- xlog still wraps it and installs it as `slog.Default()`: the `trace_id`, the fields from `AddKV` / `CtxWithKV`, the
  error log counting and `Fields` are all still there, and the framework's access logs, SQL logs and start/stop logs go
  into it too.
- The level, the format and the destination are all decided by that handler, and nothing in `XLog` except `Fields` has
  any effect: **writing any of it fails startup** (`XLog has no effect when xlog.UseHandler is set`), so nobody believes
  a `Level: debug` took. Writing only the defaults is not a conflict.
- That handler is yours: xlog does not close it on exit and does not replace `slog.Default()` either.
- Calling it after logging is set up has no effect and only logs a WARN -- the logs before that have gone elsewhere
  already.

## Notes

- **Use `slog.InfoContext(ctx, ...)`**: a `slog.Info` without a ctx cannot reach the `trace_id` and carries none of the
  `AddKV` / `CtxWithKV` fields.
- **`AddKV` needs a scope**: xgin / xecho open one at the start of every request (not with `Log: false`); a non-web entry
  point of your own opens it with `xlog.CtxWithScope(ctx)`, or the fields are dropped (and counted in
  `DroppedKVCount`). See [observability.md "Logs"](../docs/observability.md#logs).
- **The division of labor between `AddKV` and `CtxWithKV` is the reach**: `AddKV` writes in place and the whole request
  carries it; `CtxWithKV` affects only the ctx it returns, so each item in a batch and each goroutine derives its own,
  without bleeding into each other and without flowing back into the access log. It also does not see fields the parent
  ctx adds after it was derived.
- **A configured `Timezone` that cannot be loaded fails startup outright** rather than quietly falling back to local; a
  scratch / distroless image needs `import _ "time/tzdata"`, see ["Behavior (measured)"](#behavior-measured).
- **A file that is not a symbolic link already sitting at `Name`'s position** fails startup rather than overwriting the
  old logs.

## Behavior (measured)

The measurement environment and the cross-module summary table are in [`docs/behavior.md`](../docs/behavior.md).

**`Perm`**: yaml.v3 parses the number `0644` as 420 (correct), while `644` is decimal 644 = 0o1204 (`--w----r-T`), with
no error. So it is a string, always parsed as octal.

**`RotateTime`**'s alignment and file name suffix both follow the process's local time zone, not `Timezone` (which only
governs the timestamps inside the logs).

**`Timezone`**: a scratch / distroless image has no `/usr/share/zoneinfo`, so one `import _ "time/tzdata"` line in your
own `main` package is needed (about 400KB). The framework does not compile it in: anyone not using this setting should
not carry those 400KB.

**What the default fields cost**: measured, the default 4 (with a hostname as long as a Pod name) add 88 bytes per line,
and neither time nor allocations are measurably different (`BenchmarkHandle_WithIdentityFields` against
`BenchmarkHandle_Bare`).
