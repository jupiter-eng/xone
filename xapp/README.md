# xapp

The `XApp` block covers the application itself: what it is called, which version it is, which environment it runs in by default, and which files its configuration comes from.

- Tracing's `service.name` / `service.version` and the API docs' default title and version all come from here, configured once
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

`XApp` is read on the logging stage (`StageLog`), so the hooks on the tracing, client, business and server stages all read the final value.

## Notes

- **`Profiles` and `Import` are read by the configuration loader itself** and taken out of `XApp` once read, so what xapp sees is only `Name` and `Version`.
- **Metrics do not read this block**: to tell applications apart in metrics, use `XMetric.ConstLabels` (such as `app: shop.order.api`).
- **Environment variables win over this block**: `OTEL_RESOURCE_ATTRIBUTES` / `OTEL_SERVICE_NAME` override `Name` / `Version`. The precedence is in
  [xtrace "行为与实测"](../xtrace/README.md#行为与实测).
