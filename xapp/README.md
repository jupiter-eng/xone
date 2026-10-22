# xapp

The `XApp` block covers the application itself: what it is called, which version it is, which environment it runs in by default, and which files its configuration comes from.

- Tracing's `service.name` / `service.version` and the API docs' default title and version all come from here, configured once
- **The block is optional**: a program that would rather keep its identity in the binary calls `SetDefault` instead, and the file still wins over it
- The root package `xone` brings it along, so there is no need to import xapp for this block
- `Profiles`: which environment's config files are active by default
- `Import`: which further config files to read

## Quick start

```yaml
# conf/application.yml
XApp:
  Name: shop.order.api
  Version: v1.2.0
  Profiles: ${APP_ENV:dev}    # the profile active by default
  Import: [common/log.yml]    # which further files to read
```

```go
slog.Info("build info", "app", xapp.Name(), "version", xapp.Version())
```

Without a file, in code -- where a version stamped in at build time already is:

```go
var version = "dev" // go build -ldflags "-X main.version=v1.2.0"

func main() {
	xapp.SetDefault(xapp.Config{Name: "shop.order.api", Version: version})
	xone.Run(...)
}
```

## Configuration

```yaml
XApp:
  Name: shop.order.api     # preferably team.system.app, empty by default
  Version: v1.2.0          # empty by default
  Profiles: dev            # the profile active by default: one name, "prod,eu" or [prod, eu]; it can only be written in the main config file
  Import:                  # which further files to read: a string for one, a list for several; the optional: prefix allows it to be missing
    - common/log.yml
```

- `Name` and `Version` can be overridden in an environment file, for example a different `Name` in `application-prod.yml`.
- The file wins over `SetDefault` field by field: a field it writes overrides what the program supplied, and a field it
  leaves out keeps it. Whoever deploys has the last word over whoever built.
- `Profiles` is ignored when `--profile` or `XONE_PROFILE` is given (a replacement, not an addition). The rules are in
  [docs/config.md "Profiles"](../docs/config.md#profiles--one-file-per-environment).
- `Import`: a relative path resolves against the directory of the file that writes it, and an imported file wins over the
  one importing it; an imported file may write `Import` of its own, but not `Profiles`. The rules are in
  [docs/config.md "Import"](../docs/config.md#import--pulling-in-other-config-files).

## API

| Function | Description |
|---|---|
| `Name() string` | The application name, or an empty string when it is not configured |
| `Version() string` | The application version, or an empty string when it is not configured |
| `SetDefault(Config)` | The identity the program itself knows, for a build-time version or a name nobody should have to write into a file. Call it before `xone.Run`: the block is read on `StageLog`, and a later call would overwrite what the file said |

`XApp` is read on the logging stage (`StageLog`), so the hooks on the tracing, client, business and server stages all read the final value.

## Notes

- **Nothing is guessed when neither is given**, and nothing is written either: no `service_name` log field, no
  constant label on the metrics and no `service.name` on the spans -- not even OTel's own
  `unknown_service:<executable name>`, which the SDK would otherwise invent. An invented name would be worse than
  none: the dashboards would fill up with identically named unknowns, which hides who is who instead of showing it.
  The full table is in
  [observability.md "One application, three signals"](../docs/observability.md#one-application-three-signals).
- **Two readers cannot write nothing**, and say so in their own documentation: the API docs keep what the annotations
  say ([xginswagger](../xginswagger/README.md)), and Kafka's `client.id` is sent on every request, so franz-go's own
  `kgo` goes out when nobody configured a name -- xkafka invents none either and writes one WARN at startup
  ([xkafka](../xkafka/README.md)).
- **`Profiles` and `Import` are read by the configuration loader itself** and taken out of `XApp` once read, so what xapp sees is only `Name` and `Version`.
- **The metrics read it too**: `Name` and `Version` become the constant labels `service_name` and `service_version`
  on every metric, and an `XMetric.ConstLabels` entry of the same name overrides them. They are not called
  `name` / `version` because both are reserved ([xmetric](../xmetric/README.md#notes)).
- **Environment variables win over this block**: `OTEL_RESOURCE_ATTRIBUTES` / `OTEL_SERVICE_NAME` override `Name` / `Version`. The precedence is in
  [xtrace "Behavior (measured)"](../xtrace/README.md#behavior-measured).
