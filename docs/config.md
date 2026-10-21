# Configuration reference

One YAML file, read by the framework and dispatched to the components by top-level key. This
document covers **the rules every config block shares**: file locations, profiles, imports,
merging and placeholders. The fields and defaults of each block live in the README of the module
it belongs to, see [Config blocks → module documentation](#config-blocks--module-documentation);
reading your own block from code is covered in [xconfig](../xconfig/README.md).

The things most often needed:

| What you want | How to write it | Details |
|---|---|---|
| Name the config file | Without one, `conf/application.yml` is found; to change it use `--config=<path>` or `XONE_CONFIG` | [File locations and priority](#file-locations-and-priority) |
| One file per environment | Put the differences in `application-prod.yml` and select it with `--profile=prod` or `XONE_PROFILE=prod` | [Profiles](#profiles--one-file-per-environment), [a complete example](#multi-environment-configuration-a-complete-example) |
| Keep credentials out of version control | `Password: "${DB_PASSWORD}"`, which fails startup when unset; optional ones are `${VAR:default}` | [Placeholders](#placeholders) |
| Split into several files | `XApp: {Import: [shared.yml, optional:local.yml]}` | [Import](#import--pulling-in-other-config-files) |
| See the effective configuration | `XONE_DEBUG=1 ./app` | [XONE_DEBUG](#seeing-the-effective-configuration-xone_debug) |
| Read your own block | `xconfig.Unmarshal("MyApp", &c)` | [xconfig](../xconfig/README.md) |

**Whichever integration is imported takes effect**, and a block nobody wrote keeps its defaults
throughout — `XLog`, `XTrace`, `XMetric`, `XHttp`, `XGin` and `XEcho` all work unconfigured. The
exceptions are **`XGorm`, `XRedis` and `XCache`**: only you know what they should connect to, so
without their block no instance is built at all and `C()` panics saying it is unconfigured.

- [File locations and priority](#file-locations-and-priority)
- [Profiles — one file per environment](#profiles--one-file-per-environment)
- [Import — pulling in other config files](#import--pulling-in-other-config-files)
- [Merge rules](#merge-rules)
- [Multi-environment configuration: a complete example](#multi-environment-configuration-a-complete-example)
- [Seeing the effective configuration: XONE_DEBUG](#seeing-the-effective-configuration-xone_debug)
- [Placeholders](#placeholders)
- [General rules](#general-rules)
- [Editor completion](#editor-completion)
- [Config blocks → module documentation](#config-blocks--module-documentation)

## File locations and priority

Highest first, and the first one given wins:

1. `xone.WithConfigPath("…")` in code
2. The `--config=<path>` flag (`--config <path>` works too)
3. The `XONE_CONFIG` environment variable
4. The conventional paths, in order: `conf/application.yml`, `conf/application.yaml`,
   `config/application.yml`, `config/application.yaml`, `application.yml`, `application.yaml`
   (relative to the process's working directory)

The first three name a file explicitly, and a missing file fails startup
(`config file does not exist: <path>`); when none of the conventional paths exists, one warning is
logged and everything keeps its defaults. A `--config` or `--profile` written last without a value
(`./app --config`) also fails startup (`--config needs a value`) rather than being treated as
absent and falling through to the next option.

The configuration is loaded **the first time anybody reads it**, which may be before `xone.Run`
(when `main` itself calls `xconfig.Unmarshal`, say). `WithConfigPath` has not taken effect by then,
so the file found by 2–4 is used; naming a different file in `Run` afterwards reports an error
outright (`config was already loaded by an earlier read …`). A program that reads the configuration
early therefore names its file with `--config` or `XONE_CONFIG`.

The framework's own startup logs (`loading config`, `starting` and so on) go to `slog.Default()` by
default and follow xlog automatically once it is installed; to use a different logger, pass
`xone.WithLogger(l)`.

## Profiles — one file per environment

The spelling is Spring's: `application.yml` holds what is common and
`application-{profile}.yml` holds what is specific to that environment. Which profiles are active
by default is written in `XApp.Profiles` (Spring's `spring.profiles.active`).

```bash
./app --profile=prod           # or XONE_PROFILE=prod
./app --profile=prod,eu        # several, comma-separated, with later ones winning over earlier ones
```

It can also be written in the base file:

```yaml
XApp:
  Profiles: ${APP_ENV:dev}   # one name, a comma-separated string or a list; placeholders expand before the files to read are decided
```

- Priority: `--profile` > `XONE_PROFILE` > `XApp.Profiles` in the file. The first two **replace**
  what the file says rather than adding to it.
- `XApp.Profiles` may only be written in the base file; writing it in an imported or
  profile-specific file fails startup.
- A profile file's name is derived from the base file, directory and extension included:
  `--config=/etc/app/svc.yaml` with `--profile=prod` looks for `/etc/app/svc-prod.yaml`.
- **One difference from Spring**: a named profile file that does not exist **fails startup**, while
  Spring skips it silently. A missing profile variant of an imported fragment is not an error (see
  the next section).

## Import — pulling in other config files

The counterpart of Spring's `spring.config.import`. **A relative path resolves against the
directory of the file that wrote the `Import`**, not the process's working directory: `shared.yml`
written in `conf/application.yml` means `conf/shared.yml`.

```yaml
XApp:
  Import:                  # a string for one, a list for several, with later ones winning
    - shared.yml           # that is, conf/shared.yml
    - db.yml
    - optional:local.yml   # the optional: prefix skips a file that does not exist
```

- What is imported wins over the file that imported it (an import behaves as if inserted directly
  below it).
- An imported file has profile variants too: `db.yml` with `--profile=prod` also looks for
  `db-prod.yml`; a missing variant of a fragment **is not an error**.
- A file is read once (a diamond counts once, and a cycle is broken on the second encounter with no
  error); nesting goes 16 levels deep at most.
- An imported file may write `XApp.Import` of its own.
- An `Import` path may contain `${VAR}`, which expands before the file is read.

## Merge rules

The priority of several files, lowest first:

```
application.yml  <  what it imports (including a fragment's own -prod variant)  <  application-prod.yml  <  what prod imports
```

| Type | Rule |
|---|---|
| map | **merged recursively**, with the higher-priority value winning for a key both hold |
| list | **replaced wholesale**, never merged element by element — to add, write the complete list |
| scalar | the higher priority overwrites |

**What is not written changes nothing when layered on top**:

| Written in the higher-priority file | Result |
|---|---|
| The whole file is empty, or holds only comments | It contributes nothing |
| `XRedis:`, `XRedis: ~`, `Addr:` (null) | The value from the lower-priority file is kept, as with `XRedis: {}` |
| `Addr: ""`, `Headers: []` | Overwritten with an empty string or an empty list — that is how to clear something |

- **A duplicate key is an error in every file**, reported with the file and both line numbers.
- **One file holds one YAML document**: a second one separated by `---` fails startup
  (`multiple YAML documents in one file are not supported`), and the way to split by environment is
  `application-{profile}.yml`. An empty document does not count: a leading `---`, or a trailing one
  followed by nothing, comments only or `~`, loads as usual.
- **Anchors and aliases** (`&name` / `*name` / `<<: *name`) may be used freely inside one file,
  across top-level blocks included; they cannot cross files. A file expanding to more than a hundred
  thousand nodes fails startup outright.
- **`<<` is flattened into ordinary keys inside each file**, by YAML's own rules: an explicit key
  wins over a merged one (whether written before or after the `<<`), earlier entries of
  `<<: [*a, *b]` win over later ones, and only one level is merged (an explicit `Nested` replaces
  the whole thing). Flattening happens before the cross-file merge in the table above, so a value
  brought in by `<<: *prod` in a profile file still wins over an explicit key of the same name in
  the base file.

## Multi-environment configuration: a complete example

The files below and the result of every way of starting them were actually run (the final values
`xconfig.Unmarshal` reads).

### The directory

```
conf/
├── application.yml        common configuration, read in every environment; decides the default profile and imports the common fragments
├── application-dev.yml    dev-specific (the default profile)
├── application-prod.yml   prod-specific, additionally importing secrets.yml
├── application-eu.yml     Europe-specific, layered on top of prod: --profile=prod,eu
├── secrets.yml            imported by prod only, with every credential a ${VAR}
└── common/
    ├── log.yml            logging configuration, imported by application.yml
    └── log-prod.yml       log.yml's prod variant: layered on automatically under prod, and its absence elsewhere is no error
```

### The files

```yaml
# conf/application.yml
XApp:
  Name: order-api
  Profiles: ${APP_ENV:dev}     # dev unless APP_ENV is set; ignored entirely when --profile / XONE_PROFILE is given
  Import:
    - common/log.yml           # relative to this file's directory: conf/common/log.yml
XGin:
  Port: 8080
Order:                         # the business's own block, read with xconfig.Unmarshal("Order", &c)
  PayTimeout: 15m
  Channels: [alipay, wechat]
```

```yaml
# conf/common/log.yml
XLog:
  Level: info
  Format: json
```

```yaml
# conf/common/log-prod.yml
XLog:
  Level: warn                  # only the field that changes; Format keeps log.yml's json
```

```yaml
# conf/application-dev.yml
XApp:
  Import: [optional:local.yml] # per-developer overrides, kept out of version control; skipped when absent
XLog:
  Level: debug
  Format: text
```

```yaml
# conf/application-prod.yml
XApp:
  Import: [secrets.yml]
XGin:
  Port: 80
Order:
  Channels: [alipay]           # a list is replaced wholesale: prod keeps alipay alone
```

```yaml
# conf/secrets.yml
Order:
  CallbackToken: "${ORDER_TOKEN}"   # unset fails startup rather than coming up with an empty string
```

```yaml
# conf/application-eu.yml
Order:
  PayTimeout: 30m
```

### How to start it, and what is read

| How it is started | Which files are read (lowest priority first) | The final values |
|---|---|---|
| `./app` | `application.yml` → `common/log.yml` → `application-dev.yml` (→ `local.yml`, when present) | `XGin.Port` 8080; `XLog` text / debug; `Channels` [alipay, wechat] |
| `./app --profile=prod`<br>`APP_ENV=prod ./app`<br>`XONE_PROFILE=prod ./app` | `application.yml` → `common/log.yml` → `common/log-prod.yml` → `application-prod.yml` → `secrets.yml` | `XGin.Port` 80; `XLog` json / warn; `Channels` [alipay]; `CallbackToken` taken from `ORDER_TOKEN` |
| `./app --profile=prod,eu` | the row above, with `application-eu.yml` layered on | the same, with `PayTimeout` 30m |
| `XONE_PROFILE=eu ./app` | `application.yml` → `common/log.yml` → `application-eu.yml` | `XLog` json / info: **dev did not take effect**, see point 1 below; `PayTimeout` 30m |
| `./app --profile=prod` without `ORDER_TOKEN` | — | startup fails: `environment variables not set: ORDER_TOKEN` |
| `./app --profile=staging` | — | startup fails: `read config conf/application-staging.yml: … no such file or directory` |

A common arrangement in Kubernetes: the image carries the whole `conf/` (or a ConfigMap is mounted
at `conf/`), the Deployment sets `APP_ENV=prod`, and credentials are injected from a Secret as
environment variables (`ORDER_TOKEN`).

### Easy things to get wrong

1. **`--profile` / `XONE_PROFILE` replaces `XApp.Profiles`.** In the `XONE_PROFILE=eu` row above,
   the dev default from the file no longer applies; to get both, write them out:
   `--profile=dev,eu`.
2. **A list is replaced wholesale.** prod's `Channels: [alipay]` does not merge into
   `[alipay, wechat]`, it replaces it.
3. **An imported file wins over the file that imported it, and a file is read once.**
   `optional:local.yml` written in `application-dev.yml`'s `XApp.Import` wins over dev (a local
   override has the highest priority); importing it from `application.yml` **as well** means only
   the first encounter counts — it then sits at `application.yml`'s position and no longer wins over
   the same field written in `application-dev.yml`.
4. **A fragment's profile variant may be missing, the main file's may not.** `common/log-dev.yml`
   not existing is fine; `application-staging.yml` not existing fails startup — which is almost
   always a misspelled profile name.
5. When it is unclear which files were read and what the final values are, start once with
   `XONE_DEBUG=1`, see [the next section](#seeing-the-effective-configuration-xone_debug). The
   ordinary startup log names the main file only
   (`loading config file=conf/application.yml`).

## Seeing the effective configuration: XONE_DEBUG

```bash
XONE_DEBUG=1 ./app --profile=prod      # 1 / true / yes / on all turn it on
```

A few extra sections go to stderr at startup, written for a person to read:

```
[xone debug] config file: conf/application.yml (default search path)
[xone debug] profiles: prod (from --profile)
[xone debug] files read, lowest to highest priority:
  1. conf/application.yml
  2. conf/common/log.yml
  3. conf/common/log-prod.yml
  4. conf/application-prod.yml
  5. conf/secrets.yml
[xone debug] effective config (secrets redacted):
  XApp:
    Name: order-api
  XGin:
    Port: 80
  Order:
    PayTimeout: 15m
    Channels: [alipay]
    CallbackToken: '***'
  XLog:
    Level: warn
    Format: json
[xone debug] xone v1.0.0, go1.25.0
[xone debug] start hooks, in order (stop hooks run in reverse):
  1. Log       xapp.loadConfig
  2. Log       xlog.initXLog
  3. Telemetry xtrace.initXTrace
  ...
```

- **How the config file was found**: the parenthesis holds `xone.WithConfigPath`, `--config`,
  `XONE_CONFIG` or `default search path`.
- **Where the profiles came from**: `--profile`, `XONE_PROFILE`, or
  `XApp.Profiles in the config file`.
- **The effective configuration is the merged one**, that is, the values the components really
  read; comments from the config files are not carried over (after merging there is no telling
  which file they came from). **A value coming from `${VAR}` is shown as written in the
  configuration** (`Webhook: ${ORDER_WEBHOOK}`) rather than expanded — credentials usually arrive
  that way, and they do not necessarily sit under a key called password.
- **Credentials are masked** and shown as `***`:
  - a key whose name contains password, passwd, secret, token, credential, apikey, accesskey or
    privatekey (lowercased with `_ - .` and spaces removed before comparing) has its value masked
    whole, whether it is a scalar, a list or a map;
  - a password embedded in a value: `postgres://app:***@db`,
    `report:***@tcp(db:3306)/report`. When the password itself contains `@` or `/`, masking runs to
    the last `@` (so a later `@` in the same segment masks the host along with it — better to mask
    too much);
  - query strings and `key=value` spellings whose name contains one of the words above
    (`token=***`, `api_key=***`, `password=***`), plus `pwd=***`; the default inside
    `${VAR:default}` is masked the same way;
  - **empty values are not masked** (`""`, `[]`, `{}`, absent), so a credential nobody configured
    is visible at a glance.
- **Only turn it on while debugging locally**: it writes multi-line plain text rather than JSON,
  and behind a log collector it is a few lines that fail to parse.

**The startup banner** (which carries xone's version) is printed only when stderr is a terminal:
visible under a local `go run`, and not one character inside a container, redirected to a file or
behind a log collector. The version comes from the binary's build info; with a `replace` pointing
at a local directory it reads `(devel)`.

### The environment variables the framework reads

| Variable | What it does | The equivalent flag |
|---|---|---|
| `XONE_CONFIG` | The config file path | `--config=<path>` |
| `XONE_PROFILE` | The active profiles, comma-separated | `--profile=prod,eu` |
| `XONE_DEBUG` | Prints how loading went and the effective configuration, see above | — |

## Placeholders

| Spelling | Meaning |
|---|---|
| `${VAR}` | Required; unset fails startup (`environment variables not set: VAR`). Every credential should be written this way |
| `${VAR:default}` | Optional; `default` is used when it is unset |
| `Port: ${PORT:8080}` | The type is decided from what it expands to, so it fits an int field |
| `Password: "${PW}"` | Quoting forces it to be treated as a string, which is what numeric-looking passwords and version numbers rely on |
| `${PORT:}` or a variable set to an empty string | Counts as **this field was not written**, and the field keeps the struct's default (not the value from a lower-priority file). For a genuine empty string, quote it: `"${PW:}"` |
| A variable whose value happens to be `null` / `~` | Not treated as absent but as a string: a string field receives that literal, and any other type reports a type error |

- Placeholders expand **after every file has been merged** (`XApp.Import` and `XApp.Profiles`
  excepted): a required `${SECRET}` in the base file that a profile file replaced wholesale is no
  longer required to be set.
- Expansion happens on the parsed nodes rather than as text substitution: a colon or a newline
  inside a value cannot change the YAML structure.
- A type error never carries the expanded value: it reads
  ``cannot unmarshal !!str `${DB_PASSWORD}` (expanded value redacted) into int``.

## General rules

| Rule | Details |
|---|---|
| Defaults | Pre-filled in the struct, and a field the file does not write keeps it. There are no `*bool` pointers, so `Enable: false` is false |
| When it is read | Any time before `Start`: it is loaded the first time it is read, so even a read in `main` before `xone.Run` sees the final values |
| A misspelled field | **Fails startup**, with the file and line in the message: `application.yml:3: field Bogus not found in type xgin.Config` |
| A top-level block nobody reads | A top-level key still unread when every start hook has run **fails startup** (`config keys [...] are not read by anyone`) |
| A wrong value | Each module's `Validate` runs while the configuration is read, with the file and line in the message, before a single instance has connected |
| Durations | Written as `30s` / `1500ms` / `1h30m`. A bare number fails startup — whoever writes `30` means 30 seconds, and Go would give them 30 nanoseconds |
| A timeout of 0 | Each field's comment says what 0 means there. `XTrace.ShutdownTimeout`, `XFlow.RollbackTimeout` and `XGin`'s / `XEcho`'s `ReadHeaderTimeout` / `IdleTimeout` fail startup at 0 |
| List fields | Writing one in a file replaces the default wholesale |
| Map fields | What a file writes is **merged** into the default, which is why no map field in the framework has a default |
| Connection retries | XGorm and XRedis probe once at startup, at most 3 attempts; an authentication failure or a rejected certificate is not retried. See [behavior.md "The startup connection probe"](behavior.md#the-startup-connection-probe) |

The outbound TLS of XGorm (PostgreSQL / MySQL / ClickHouse), XRedis and XHttp is written as one and
the same block (`TLS:`), see [xtls](../xtls/README.md#configuration).

## Editor completion

`config_schema.json` in the repository root is the machine-readable version of the
"Configuration" section of each module's README; wired up, it gives YAML field completion,
highlighted typos and hover descriptions:

```jsonc
// .vscode/settings.json — JetBrains configures this under Settings → JSON Schema Mappings
{ "yaml.schemas": { "./config_schema.json": ["conf/application*.yml"] } }
```

It is generated from the Config structs by `go run ./internal/schemagen`, with the field
descriptions taken from the comments on those structs. An unknown field inside a block is
highlighted; both the single- and multi-instance forms are accepted and mixing them is highlighted;
numeric and boolean fields accept placeholders too. An unknown key at the **top level** is not
highlighted — the business's own blocks live there as well, and that case is caught at startup by
"a top-level block nobody reads".

## Config blocks → module documentation

Every field, default and caveat of a block lives under "Configuration" in the README of the module
it belongs to; the same README also holds that module's measurements, its logs / metrics / spans
and its troubleshooting.

| Block | What it is | Module documentation |
|---|---|---|
| `XApp` | Application identity, profiles, Import | [xapp/README.md](../xapp/README.md#configuration) |
| `XLog` | Logging | [xlog/README.md](../xlog/README.md#configuration) |
| `XTrace` | Tracing | [xtrace/README.md](../xtrace/README.md#configuration) |
| `XMetric` | Metrics | [xmetric/README.md](../xmetric/README.md#configuration) |
| `XGorm` | Databases | [xgorm/README.md](../xgorm/README.md#configuration) |
| `XGorm` (`Driver: clickhouse`) | The ClickHouse driver | [xgorm/clickhouse/README.md](../xgorm/clickhouse/README.md#configuration) |
| `XRedis` | Redis | [xredis/README.md](../xredis/README.md#配置) |
| `XKafka` | Kafka | [xkafka/README.md](../xkafka/README.md#configuration) |
| `XCache` | A local cache | [xcache/README.md](../xcache/README.md#configuration) |
| `XHttp` | Outbound HTTP | [xhttp/README.md](../xhttp/README.md#configuration) |
| `XGin` | A web server | [xgin/README.md](../xgin/README.md#configuration) |
| `XGinSwagger` | API documentation | [xginswagger/README.md](../xginswagger/README.md#configuration) |
| `XEcho` | A web server (Echo) | [xecho/README.md](../xecho/README.md#configuration) |
| `XFlow` | Flow orchestration | [xflow/README.md](../xflow/README.md#configuration) |
| `TLS` (inside the XGorm / XRedis / XHttp / XKafka blocks) | Client-side TLS | [xtls/README.md](../xtls/README.md#configuration) |
