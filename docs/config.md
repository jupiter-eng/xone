# 配置参考

一个 YAML 文件，框架统一读，按顶层 key 分发给各组件。这份文档讲**所有配置块共用的规则**：文件位置、Profile、
Import、合并、占位符。每个配置块的字段和默认值在它所属模块的 README 里，见[配置块 → 模块文档](#配置块--模块文档)；
在代码里读自己的配置块见 [xconfig](../xconfig/README.md)。

最常用的几件事：

| 想要 | 怎么写 | 详见 |
|---|---|---|
| 指定配置文件 | 不指定就找 `conf/application.yml`；要换用 `--config=<path>` 或 `XONE_CONFIG` | [文件位置与优先级](#文件位置与优先级) |
| 按环境分文件 | 差异写进 `application-prod.yml`，`--profile=prod` 或 `XONE_PROFILE=prod` 选 | [Profiles](#profiles--按环境分文件)、[完整例子](#多环境配置一个完整的例子) |
| 凭证不进版本库 | `Password: "${DB_PASSWORD}"`，没设就启动失败；可选的写 `${VAR:默认值}` | [占位符](#占位符) |
| 拆成几个文件 | `XApp: {Import: [shared.yml, optional:local.yml]}` | [Import](#import--引入别的配置文件) |
| 看最终生效的配置 | `XONE_DEBUG=1 ./app` | [XONE_DEBUG](#看最终生效的配置xone_debug) |
| 读自己的配置块 | `xconfig.Unmarshal("MyApp", &c)` | [xconfig](../xconfig/README.md) |

**import 了哪个集成，它就生效**，没写的块全用默认值——`XLog`、`XTrace`、`XMetric`、`XHttp`、`XGin`、`XEcho`
不配也照常工作。例外是 **`XGorm`、`XRedis`、`XCache`**：它们要连的东西只有你知道，
没配这一块就一个实例都不建，`C()` 会 panic 并说明「没配」。

- [文件位置与优先级](#文件位置与优先级)
- [Profiles —— 按环境分文件](#profiles--按环境分文件)
- [Import —— 引入别的配置文件](#import--引入别的配置文件)
- [合并规则](#合并规则)
- [多环境配置：一个完整的例子](#多环境配置一个完整的例子)
- [看最终生效的配置：XONE_DEBUG](#看最终生效的配置xone_debug)
- [占位符](#占位符)
- [通用规则](#通用规则)
- [编辑器补全](#编辑器补全)
- [配置块 → 模块文档](#配置块--模块文档)

## 文件位置与优先级

从高到低，第一个给出的就是它：

1. 代码里的 `xone.WithConfigPath("…")`
2. 启动参数 `--config=<path>`（也认 `--config <path>`）
3. 环境变量 `XONE_CONFIG`
4. 约定路径，按顺序：`conf/application.yml`、`conf/application.yaml`、`config/application.yml`、
   `config/application.yaml`、`application.yml`、`application.yaml`（相对进程的工作目录）

前三种是点名要的，文件不存在就启动失败（`config file does not exist: <path>`）；约定路径一个都没有时
只打一条告警、全用默认值。`--config`、`--profile` 写在最后却没带值（`./app --config`）同样启动失败
（`--config needs a value`），不会当成没写、接着按后面的顺序找。

配置在**第一次有人读**的时候才加载——可能早于 `xone.Run`（比如你在 `main` 里就调了 `xconfig.Unmarshal`）。
那时 `WithConfigPath` 还没生效，用的是 2–4 找到的文件；之后 `Run` 再点名另一个文件会直接报错
（`config was already loaded by an earlier read …`）。所以提前读配置的程序用 `--config` 或 `XONE_CONFIG` 指定文件。

框架自己的启动日志（`loading config`、`starting` 等）默认写 `slog.Default()`，xlog 装好之后自动跟着走它；
想换一个 logger 用 `xone.WithLogger(l)`。

## Profiles —— 按环境分文件

写法和 Spring 一样：`application.yml` 放公共的，`application-{profile}.yml` 放这个环境特有的。
默认激活哪个 profile 写在 `XApp.Profiles`（对应 Spring 的 `spring.profiles.active`）。

```bash
./app --profile=prod           # 或 XONE_PROFILE=prod
./app --profile=prod,eu        # 多个用逗号分隔，靠后的压过靠前的
```

也可以写在 base 文件里：

```yaml
XApp:
  Profiles: ${APP_ENV:dev}   # 一个名字、逗号分隔的字符串或列表都收；占位符在决定读哪些文件之前就展开
```

- 优先级：`--profile` > `XONE_PROFILE` > 文件里的 `XApp.Profiles`。前两个是**替换**文件里的，不是追加。
- `XApp.Profiles` 只能写在 base 文件里，写在被引入的文件、profile 文件里启动失败。
- profile 文件名由 base 文件推出来，目录和扩展名都跟着它：`--config=/etc/app/svc.yaml` 配 `--profile=prod`
  找的是 `/etc/app/svc-prod.yaml`。
- **与 Spring 的一处不同**：点名的 profile 文件不存在时**直接启动失败**，Spring 是静默跳过。
  Import 进来的片段的 profile 变体不存在则不算错（见下一节）。

## Import —— 引入别的配置文件

对应 Spring 的 `spring.config.import`。**相对路径按写着这个 `Import` 的文件所在的目录解析**，不是进程的工作目录：
`conf/application.yml` 里写 `shared.yml` 就是 `conf/shared.yml`。

```yaml
XApp:
  Import:                  # 一个就写字符串，多个写列表，靠后的压过靠前的
    - shared.yml           # 即 conf/shared.yml
    - db.yml
    - optional:local.yml   # optional: 前缀，文件不存在就跳过
```

- 引进来的压过引它的那个文件（import 相当于插在它正下方）。
- 引进来的文件同样有 profile 变体：`db.yml` 配 `--profile=prod` 会再找 `db-prod.yml`；片段的变体**不存在不算错**。
- 同一个文件只读一次（菱形引用只算一次，成环在第二次遇到时断开、不报错）；嵌套最多 16 层。
- 被引进来的文件里可以再写 `XApp.Import`。
- `Import` 的路径里可以写 `${VAR}`，它在读文件之前就展开。

## 合并规则

多个文件的优先级，从低到高：

```
application.yml  <  它 Import 的（含片段自己的 -prod 变体）  <  application-prod.yml  <  prod 那份 Import 的
```

| 类型 | 规则 |
|---|---|
| map | **递归合并**，两边都有的 key 用优先级高的 |
| 列表 | **整体替换**，不逐元素合并——想追加就把完整的列表写全 |
| 标量 | 优先级高的覆盖 |

**没写的东西叠上来什么都不改**：

| 优先级高的文件里写的 | 结果 |
|---|---|
| 整个文件是空的，或者只有注释 | 不贡献任何东西 |
| `XRedis:`、`XRedis: ~`、`Addr:`（null） | 保留低优先级文件里的值，和 `XRedis: {}` 一样 |
| `Addr: ""`、`Headers: []` | 覆盖成空串、空列表——要清空就这样写 |

- **重复的 key 在每个文件里都是错误**，报出文件和两处的行号。
- **一个文件只能有一份 YAML 文档**：用 `---` 隔开的第二份启动失败（`multiple YAML documents in one file are not supported`），
  分环境的写法是 `application-{profile}.yml`。空的文档不算一份：开头一个 `---`、结尾多写的 `---`（后面什么都没有、
  只有注释或 `~`）照常加载。
- **锚点和别名**（`&name` / `*name` / `<<: *name`）在同一个文件内随便用，可以跨顶层块；不能跨文件。
  展开后一个文件超过十万个节点直接启动失败。
- **`<<` 在每个文件内部就摊平成普通的 key**，规矩和 YAML 一样：写明的 key 压过并进来的（不论写在 `<<` 前后），
  `<<: [*a, *b]` 里靠前的压过靠后的，只并一层（写明了 `Nested` 就整个用写明的）。摊平之后才按上表跨文件合并，
  所以 profile 文件里 `<<: *prod` 带进来的值照样压过 base 里写明的同名 key。

## 多环境配置：一个完整的例子

下面这套文件和每一种启动方式的结果都是实际跑出来的（`xconfig.Unmarshal` 读到的最终值）。

### 目录

```
conf/
├── application.yml        公共配置，每个环境都读；决定默认 profile、引入公共片段
├── application-dev.yml    dev 特有（默认 profile）
├── application-prod.yml   prod 特有，另外引入 secrets.yml
├── application-eu.yml     欧洲区特有，和 prod 叠着用：--profile=prod,eu
├── secrets.yml            只有 prod 引入，凭证全是 ${VAR}
└── common/
    ├── log.yml            日志配置，被 application.yml 引入
    └── log-prod.yml       log.yml 的 prod 变体：prod 时自动叠上，别的环境没有这个文件也不报错
```

### 文件内容

```yaml
# conf/application.yml
XApp:
  Name: order-api
  Profiles: ${APP_ENV:dev}     # 没设 APP_ENV 就是 dev；--profile / XONE_PROFILE 给了就不看这一项
  Import:
    - common/log.yml           # 相对这个文件所在的目录：conf/common/log.yml
XGin:
  Port: 8080
Order:                         # 业务自己的块，用 xconfig.Unmarshal("Order", &c) 读
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
  Level: warn                  # 只写要改的那一项，Format 沿用 log.yml 的 json
```

```yaml
# conf/application-dev.yml
XApp:
  Import: [optional:local.yml] # 各人本机的覆盖，不进版本库；文件不在就跳过
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
  Channels: [alipay]           # 列表整体替换：prod 只剩 alipay
```

```yaml
# conf/secrets.yml
Order:
  CallbackToken: "${ORDER_TOKEN}"   # 没设就启动失败，不会带着空串起来
```

```yaml
# conf/application-eu.yml
Order:
  PayTimeout: 30m
```

### 怎么启动、读到什么

| 启动方式 | 读了哪些文件（优先级从低到高） | 最终的值 |
|---|---|---|
| `./app` | `application.yml` → `common/log.yml` → `application-dev.yml`（→ `local.yml`，有的话） | `XGin.Port` 8080；`XLog` text / debug；`Channels` [alipay, wechat] |
| `./app --profile=prod`<br>`APP_ENV=prod ./app`<br>`XONE_PROFILE=prod ./app` | `application.yml` → `common/log.yml` → `common/log-prod.yml` → `application-prod.yml` → `secrets.yml` | `XGin.Port` 80；`XLog` json / warn；`Channels` [alipay]；`CallbackToken` 取自 `ORDER_TOKEN` |
| `./app --profile=prod,eu` | 上一行，再叠 `application-eu.yml` | 同上，`PayTimeout` 30m |
| `XONE_PROFILE=eu ./app` | `application.yml` → `common/log.yml` → `application-eu.yml` | `XLog` json / info：**dev 没有生效**，见下面第 1 条；`PayTimeout` 30m |
| `./app --profile=prod`，没设 `ORDER_TOKEN` | —— | 启动失败：`environment variables not set: ORDER_TOKEN` |
| `./app --profile=staging` | —— | 启动失败：`read config conf/application-staging.yml: … no such file or directory` |

K8s 里常见的做法：镜像里带着整个 `conf/`（或者用 ConfigMap 挂到 `conf/`），Deployment 里设 `APP_ENV=prod`，
凭证从 Secret 注入成环境变量（`ORDER_TOKEN`）。

### 容易踩的几点

1. **`--profile` / `XONE_PROFILE` 替换 `XApp.Profiles`。** 上面 `XONE_PROFILE=eu` 那一行，
   文件里默认的 dev 就不生效了；两个都要就写全：`--profile=dev,eu`。
2. **列表整体替换。** prod 的 `Channels: [alipay]` 不是往 `[alipay, wechat]` 里合并，而是换掉它。
3. **引入的文件压过引它的文件，同一个文件只读一次。** `optional:local.yml` 写在 `application-dev.yml` 的 `XApp.Import` 里，
   它压过 dev（本机覆盖优先级最高）；要是**同时**在 `application.yml` 里也引了它，只有先遇到的那一处算——
   它就落在 `application.yml` 的位置上，压不过 `application-dev.yml` 里写的同一项。
4. **片段的 profile 变体可以没有，主文件的不行。** `common/log-dev.yml` 不存在没关系；`application-staging.yml`
   不存在是启动失败——几乎总是 profile 名写错了。
5. 拿不准到底读了哪些文件、最终是什么值，`XONE_DEBUG=1` 启动一次，见[下一节](#看最终生效的配置xone_debug)。
   平时的启动日志只打主文件（`loading config file=conf/application.yml`）。

## 看最终生效的配置：XONE_DEBUG

```bash
XONE_DEBUG=1 ./app --profile=prod      # 1 / true / yes / on 都算打开
```

启动时往 stderr 多打几段，给人看的：

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

- **配置文件是怎么找到的**：括号里是 `xone.WithConfigPath`、`--config`、`XONE_CONFIG` 或 `default search path`。
- **profile 是从哪来的**：`--profile`、`XONE_PROFILE`，或者 `XApp.Profiles in the config file`。
- **最终配置是合并之后的**，也就是各组件真正读到的值；配置文件里的注释不带出来（合并之后看不出是哪个文件的）。
  **来自 `${VAR}` 的值显示配置里写的原文**（`Webhook: ${ORDER_WEBHOOK}`），不显示展开出来的值——凭证多半就是这么传进来的，
  而它不一定放在叫 password 的 key 下面。
- **凭证已遮掉**，显示成 `***`：
  - key 名里含 password、passwd、secret、token、credential、apikey、accesskey、privatekey 的（比较前转小写、去掉 `_ - .` 和空格），
    值不论是标量、列表还是 map 都整个遮掉；
  - 值里夹着的密码：`postgres://app:***@db`、`report:***@tcp(db:3306)/report`。密码里带 `@`、`/` 时遮到最后一个 `@`
    （同一段里后面再有 `@` 时连主机一起遮，宁可多遮）；
  - 查询串和 `key=value` 写法里名字含上面那些词的（`token=***`、`api_key=***`、`password=***`），外加 `pwd=***`；
    `${VAR:默认值}` 的默认值里的同样遮；
  - **空的不遮**（`""`、`[]`、`{}`、没写），一眼看得出哪个凭证没配。
- **只在本地排查时开**：写的是多行的纯文本，不是 JSON，接在日志采集器后面就是几行解析失败的日志。

**启动 banner**（带 xone 的版本）只在 stderr 是终端时打：本地 `go run` 能看到，容器里、重定向到文件、接在日志采集器后面时一个字都不写。
版本号取自二进制的构建信息；用 `replace` 指向本地目录时显示 `(devel)`。

### 框架认的环境变量

| 变量 | 作用 | 同义的启动参数 |
|---|---|---|
| `XONE_CONFIG` | 配置文件路径 | `--config=<path>` |
| `XONE_PROFILE` | 激活的 profile，逗号分隔 | `--profile=prod,eu` |
| `XONE_DEBUG` | 打出加载经过和最终配置，见上 | —— |

## 占位符

| 写法 | 含义 |
|---|---|
| `${VAR}` | 必填，未设置则启动失败（`environment variables not set: VAR`）。凭证都该写成这个形式 |
| `${VAR:default}` | 可选，未设置时用 `default` |
| `Port: ${PORT:8080}` | 按展开后的内容判定类型，进得了 int 字段 |
| `Password: "${PW}"` | 加了引号固定按字符串处理，数字形态的密码、版本号靠这一条 |
| `${PORT:}` 或变量是空串 | 等于**这一项没写**，字段保持结构体里的默认值（不是低优先级文件里的值）。真要空串就加引号：`"${PW:}"` |
| 变量的值恰好是 `null` / `~` | 不当成没写，按字符串处理：字符串字段拿到这个字面量，其他类型的字段报类型错误 |

- 占位符在**全部文件合并完之后**才展开（`XApp.Import`、`XApp.Profiles` 例外）：base 里一个必填的 `${SECRET}`
  被 profile 文件整块覆盖掉了，就不再要求它设置。
- 展开发生在解析后的节点上，不是对原始文本替换：值里有冒号、换行也改变不了 YAML 结构。
- 类型不对时报错里不带展开出来的值：报的是 ``cannot unmarshal !!str `${DB_PASSWORD}` (expanded value redacted) into int``。

## 通用规则

| 规则 | 说明 |
|---|---|
| 默认值 | 预填在结构体里，文件没写的字段保持不变。没有 `*bool` 指针，`Enable: false` 就是 false |
| 什么时候读 | 在 `Start` 之前任何时候：第一次读的时候才加载，在 `main` 里、`xone.Run` 之前读到的也是最终值 |
| 字段拼错 | **启动失败**，报错带文件和行号：`application.yml:3: field Bogus not found in type xgin.Config` |
| 没人读的顶层块 | 全部启动钩子跑完时还没人读过的顶层 key **启动失败**（`config keys [...] are not read by anyone`） |
| 值配错 | 各模块的 `Validate` 在读配置时就跑，报错带文件和行号，一个实例都还没连 |
| 时间 | 写 `30s` / `1500ms` / `1h30m`。写裸数字启动失败——写 `30` 的人想要 30 秒，Go 会给他 30 纳秒 |
| 超时写 0 | 各字段的注释写明 0 的含义。`XTrace.ShutdownTimeout`、`XFlow.RollbackTimeout`、`XGin` / `XEcho` 的 `ReadHeaderTimeout` / `IdleTimeout` 写 0 启动失败 |
| 列表字段 | 文件里写了就整体替换默认值 |
| map 字段 | 文件里写的**合并**进默认值，所以框架的 map 字段一律没有默认值 |
| 建连重试 | XGorm、XRedis 启动时探一次，最多试 3 次；认证失败、证书被拒不重试。见 [behavior.md「启动期建连探测」](behavior.md#启动期建连探测) |

XGorm（PostgreSQL / MySQL / ClickHouse）、XRedis、XHttp 连出去时的 TLS 都写成同一个块（`TLS:`），
见 [xtls](../xtls/README.md#配置)。

## 编辑器补全

仓库根目录的 `config_schema.json` 是各模块 README「配置」一节的机器可读版本，挂上之后 YAML 里就有字段补全、拼错标红和悬停说明：

```jsonc
// .vscode/settings.json —— JetBrains 在 Settings → JSON Schema Mappings 里配
{ "yaml.schemas": { "./config_schema.json": ["conf/application*.yml"] } }
```

它由 `go run ./internal/schemagen` 从 Config 结构体生成，字段说明取自结构体上的注释。块里不认识的字段标红；
单实例 / 多实例两种写法都认、混着写标红；数字和布尔字段也收占位符。**顶层**不认识的 key 不标红——
业务自己的配置块也写在顶层，那由启动时的「没人读的顶层块」拦住。

## 配置块 → 模块文档

每个配置块的全部字段、默认值和要点在它所属模块 README 的「配置」一节；同一个 README 里还有这个模块的实测、
日志 / 指标 / Span 和排错。

| 配置块 | 是什么 | 模块文档 |
|---|---|---|
| `XApp` | 应用身份、profile、Import | [xapp/README.md](../xapp/README.md#配置) |
| `XLog` | 日志 | [xlog/README.md](../xlog/README.md#配置) |
| `XTrace` | 链路 | [xtrace/README.md](../xtrace/README.md#配置) |
| `XMetric` | 指标 | [xmetric/README.md](../xmetric/README.md#配置) |
| `XGorm` | 数据库 | [xgorm/README.md](../xgorm/README.md#配置) |
| `XGorm`（`Driver: clickhouse`） | ClickHouse 驱动 | [xgorm/clickhouse/README.md](../xgorm/clickhouse/README.md#配置) |
| `XRedis` | Redis | [xredis/README.md](../xredis/README.md#配置) |
| `XKafka` | Kafka | [xkafka/README.md](../xkafka/README.md#配置) |
| `XCache` | 本地缓存 | [xcache/README.md](../xcache/README.md#配置) |
| `XHttp` | 出站 HTTP | [xhttp/README.md](../xhttp/README.md#配置) |
| `XGin` | Web 服务 | [xgin/README.md](../xgin/README.md#配置) |
| `XGinSwagger` | 接口文档 | [xginswagger/README.md](../xginswagger/README.md#配置) |
| `XEcho` | Web 服务（Echo） | [xecho/README.md](../xecho/README.md#配置) |
| `XFlow` | 流程编排 | [xflow/README.md](../xflow/README.md#配置) |
| `TLS`（XGorm / XRedis / XHttp / XKafka 块里的） | 客户端 TLS | [xtls/README.md](../xtls/README.md#配置) |
