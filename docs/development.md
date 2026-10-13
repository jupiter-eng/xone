# 参与开发

给改这个仓库的人看的：目录怎么分、每个脚本干什么、e2e 怎么跑，以及这个仓库的规矩和它们背后的理由。
`.claude/CLAUDE.md` 是给代理看的精简版，**规矩的全文和表格以这里为准**。设计上的来龙去脉在 [`architecture.md`](architecture.md)。

- [仓库结构](#仓库结构)
- [go.work 与 GOWORK=off](#gowork-与-goworkoff)
- [脚本](#脚本)
- [e2e：真实 Web 服务测试](#e2e真实-web-服务测试)
- [规矩](#规矩)
  - [语言](#语言)
  - [优雅优先于快](#优雅优先于快)
  - [错误一律用 xerror](#错误一律用-xerror)
  - [第三方库的默认值一律要量过](#第三方库的默认值一律要量过)
  - [变异测试](#变异测试)
  - [配置与文档](#配置与文档)
  - [更新日志](#更新日志)

## 仓库结构

```
xone/
├── xone.go              根包：Run / MustRun / Option
├── xhook/               两个生命周期钩子，零依赖。集成要认识的两个包之一
├── xconfig/             配置解码：Unmarshal、Has、UnmarshalClients、DecodeStrict。另一个
├── xonetest/            给使用者的测试辅助：换一份配置、跑一遍钩子
├── xtls/                客户端 TLS 块，xgorm / xredis / xhttp / xkafka 共用，零依赖
├── xerror/  xutil/      零第三方依赖
├── xapp/                应用身份（名字、版本），只认领配置不初始化
├── xlog/                日志，基于 log/slog，零第三方依赖
├── xflow/               流程编排 + 自动回滚，零第三方依赖
├── internal/
│   ├── config/          配置加载：定位、profile / import、合并、占位符、严格解码
│   ├── hook/            钩子登记、配对与按档位执行
│   ├── xclient/         xgorm / xredis / xcache / xkafka 共用的具名实例管理和启动期探测
│   ├── web/             xgin / xecho 共用的那一半：监听、TLS、优雅关闭、信任的代理、脱敏，零第三方依赖
│   ├── logext/          xlog 的扩展点（trace_id 提取、错误日志计数、ctx 里的日志字段），xtrace / xmetric / xkafka 往这里注入
│   ├── testkit/         仓库自己的单元测试共用的小工具，只依赖标准库
│   ├── coreonly/        只用核心、不 import 任何集成的程序也能跑通（测试）
│   └── schemagen/       生成 config_schema.json、核对各模块 README 的「配置」一节（独立的工具 module）
├── xtrace/              链路，基于 OpenTelemetry（独立 module）
├── xmetric/             指标，基于 Prometheus（独立 module）
├── xgorm/               数据库，基于 GORM（独立 module）
│   └── clickhouse/      ClickHouse 驱动（独立 module）
├── xredis/              Redis，基于 go-redis（独立 module）
├── xcache/              本地缓存，基于 ristretto（独立 module）
├── xhttp/               出站 HTTP，基于 resty（独立 module）
├── xkafka/              Kafka 生产与消费，基于 franz-go（独立 module）
├── xcron/               进程内的定时任务，cronexpr 只用来解析（独立 module）
├── xgin/                Web 服务，基于 Gin（独立 module）
│   └── middleware/      访问日志、链路、指标、panic 恢复，外加 LogScope / Propagate
├── xginswagger/         Swagger UI（独立 module）
├── xecho/               Web 服务，基于 Echo v4（独立 module）；和 xgin 共用 internal/web
│   └── middleware/      同 xgin/middleware，外加「错误在这一层渲染」
├── docs/                跨模块的使用者文档（README 里有导航）+ 本文件 + CHANGELOG.md；
│                        每个模块自己的文档在它目录下的 README.md（见「配置与文档」）
├── example/             一个 module：可直接跑的示例，同时是进程内的跨模块集成测试
│   ├── consumer/        消息队列消费者：非 Web 服务的形状
│   └── component/       自己写一个集成：两个钩子 + 一个 C() 的完整样例
├── e2e/                 真实 Web 服务测试（独立 module，不发布）
│   ├── service/         被测服务：用齐各集成，配置全部来自 YAML
│   ├── baseline/        裸 gin 的对照服务，压测时比出框架的开销
│   ├── covapp/          一次性任务形状的最小程序，测配置加载时机（提前读、WithConfigPath）
│   ├── harness/         起进程、读日志 / Span / 指标 / /proc、TCP 故障代理、下游桩、压测器
│   └── compose.yml      e2e 要的 PG / MySQL / Redis / ClickHouse / Kafka
├── config_schema.json   配置的 JSON Schema，由结构体生成，给 IDE 用
├── .github/workflows/   ci.yml：check.sh + test.sh，外加用 Go 1.23 单独编译核心；e2e.yml：e2e + 全量变异；
│                        release.yml：发布按钮（打 tag、跑 e2e、只推 tag；验证装得上是单独的 verify job）
└── scripts/
    ├── check.sh         把设计约束编译成检查
    ├── test.sh          跑全仓库测试（go test ./... 不跨模块边界）
    ├── mutate.py        变异测试
    ├── mutations/       变异表，一个 module 一个文件（core.py 是根模块）
    ├── e2e.sh           拉起 PG / MySQL / Redis / ClickHouse / Kafka，跑 e2e/
    └── release.sh       发版：--bump 钉版本号，--tag 打 tag，推送之前 --smoke、推送之后 --verify 验证装得上、跑得起来
```

脚本放在哪个目录下调都行，它们会先切到仓库根。

## go.work 与 GOWORK=off

仓库里提交了 `go.work`，把全部模块（核心、各集成、`example`、`e2e`、`internal/schemagen`）放进一个工作区，
模块之间另外靠各自 `go.mod` 里的 `replace` 互指（main 上一直留着；发版时 `release.sh --bump` 把 require 钉成新版本、经 PR 合进 main，replace 不动，见下面 [release.sh](#releasesh)）。所以 IDE 打开根目录就认得全部模块，
在根目录 `go run ./example --config=example/application.yml` 也能跑。

但 `scripts/check.sh` 和 `scripts/test.sh` 一律用 `GOWORK=off` 逐模块跑——工作区会遮住某个模块自己 `go.mod` 的问题
（漏了 require、版本不对），那必须由 CI 抓出来。同理，`go list -m all` 在工作区里会把所有模块的依赖并在一起，
量模块图（[architecture.md](architecture.md#二每个集成是独立的-go-module) 那张表）时必须 `GOWORK=off`，并且在仓库外的 consumer module 里量。

## 脚本

| 命令 | 干什么 | 在 CI 里 |
|---|---|---|
| `scripts/check.sh` | 架构约束 + 依赖边界 + 文档 + gofmt / vet | 每次（ci.yml） |
| `scripts/test.sh [go test 参数]` | 逐模块 `GOWORK=off go test -race ./...`，几个模块同时跑（默认 CPU 核数个，`XONE_TEST_JOBS` 可改），输出按模块顺序打出；一个模块红了也跑完其余的，最后一起报。本地改代码时不加 `-count=1`，没改过的模块直接用缓存 | 每次（`-count=1`） |
| `scripts/mutate.py [-j N] [--only X] [-k X] [--dry-run]` | 变异测试，并行跑，不动工作区 | 全量每晚、手动触发（e2e.yml）；`--dry-run` 在 check.sh 里 |
| `scripts/e2e.sh [--load] [-run X]` | 真实 Web 服务测试，要 PG / MySQL / Redis（ClickHouse、Kafka 可选）；不给 `-parallel` 时用 16（测试大半时间在等；go test 默认是 GOMAXPROCS，4 核机器上 103s，16 是 60s） | 改了 `go.mod` / `go.sum` 的 PR、每晚、手动触发（e2e.yml，不含压测） |
| `scripts/release.sh vX.Y.Z (--bump \| --tag [--e2e-passed] \| --smoke \| --verify)` | 发布：钉版本号 / 打 tag / 推送前冒烟 / 推送后验证 | 否（打 tag 由 release 按钮做） |

### check.sh

- 核心模块图不超过 3 个第三方模块，核心不依赖任何集成模块；`xhook` / `xerror` / `xutil` / `xtls` / `internal/web` 零第三方依赖；
- 只有 xgin / xecho 带进 xlog：根模块、xtrace、xmetric、数据类集成的任何包都不许 import 它；
- `init()` 只出现在集成包里；
- 公开 API 数量上限：根包 15、`xhook` 6、`xconfig` 6、`xonetest` 3、`xtls` 3；
- 集成包必须导出 `New`，且不许 import 根包；`example/` 不许 import `internal/`（使用者 import 不到）；
- **每个配置字段都写进了它那个模块 README 的 `## 配置` 一节**（配置块 → README 的对应表在 `internal/schemagen/docs.go`），每一节的 YAML 示例都过得了 schema；
- `config_schema.json` 与结构体一致；
- 错误和日志等运行期字符串是英文；错误走 `xerror`、用 `%w` 包底层错误；
- `*.go` / `*.md` / `*.yml` 里不再出现已经删掉的公开名字（删一个公开名字时把它加进脚本里 `gone` 那张表）；
- `scripts/mutate.py --dry-run` 的每条变异模式都还对得上代码；
- `scripts/lib.sh` 的 `hooks()` 认得出各种格式的钩子启停日志（`release.sh --smoke` / `--verify` 靠它比对逆序）；
- 测试函数名是英文；
- gofmt、每个模块的 `go vet`。

它**不查** Markdown 里的相对链接和锚点，挪标题时自己搜一遍引用（见[配置与文档](#配置与文档)）。

### config_schema.json

由 `go run ./internal/schemagen` 从各模块的 Config 结构体生成，字段说明直接取结构体上的注释——注释、文档、schema 是同一个来源。
改了 Config 字段（包括字段上的注释）之后重新生成，再把字段写进那个模块 README 的 `## 配置` 一节。忘了任何一步 `check.sh` 都会红。

### mutate.py

```bash
scripts/mutate.py                  # 全量：改坏、编译、跑测试，默认开 CPU 数那么多路
scripts/mutate.py -j 2             # 只开两路
scripts/mutate.py --only xgorm     # 只跑一个表 / module / 路径前缀（xgorm 连带 xgorm/clickhouse）
scripts/mutate.py -k 超时          # 只跑名字里带这段的
scripts/mutate.py --dry-run        # 只查每条变异的模式还对不对得上代码，不到一秒（过滤照样生效）
```

改坏的副本放在临时目录，经 `go test -overlay` 换进编译，工作区一个字节都不动：**不要求干净的工作区**，
跑的时候照样可以改代码，Ctrl-C 了也没有要写回的文件。结果按表里的顺序打印，两轮的输出可以直接 diff。
规矩见下面的[变异测试](#变异测试)。

### release.sh

多模块仓库每个 module 有自己的 tag，全都打在 `main` 上的同一个提交上。发布分两步，中间是一个普通的 PR：

```bash
scripts/release.sh v1.0.0 --bump     # 1. 各模块 go.mod 里仓库内的 require 钉成 v1.0.0，CHANGELOG 的「未发布」改成这一版，
                                     #    README 安装命令的版本号换成 v1.0.0
                                     #    只改文件：提交、开 PR、合进 main（分支保护照常生效）
scripts/release.sh v1.0.0 --tag      # 2. 在 main 的最新提交上给每个模块打 tag（不推送）
scripts/release.sh v1.0.0 --smoke    # 推送之前：同一个外部工程、同一套检查，依赖 replace 到本地代码
scripts/release.sh v1.0.0 --verify   # 推送之后：在一个全新的外部工程里 go get，验证装得上、跑得起来
```

第 1 步可以交给 Claude：在这个仓库里说「发 v1.1.0」，它按 `.claude/skills/release/SKILL.md` 钉版本号、
整理 CHANGELOG、开好 PR，停在那里。

**第 2 步平时用发布按钮**：GitHub 上 Actions → `release` → Run workflow，填版本号。它跑 `--tag`、e2e（同 `e2e.yml`）、`--smoke`，
只推这一组 tag（`--atomic`），不推任何分支；推送之前任何一步红了都不推送。推送之后的 `--verify` 是单独的 `verify` job：
红了（常见是 proxy 还没拉到新 tag）点「Re-run failed jobs」只重跑它。仓库的
Settings → Actions → General → Workflow permissions 要是 Read and write；有针对 tag 的 ruleset 的话要给它放行。
见 `.github/workflows/release.yml`。

**为什么 `main` 上的 go.mod 能直接发**：使用者的构建会忽略依赖里的 `replace`，只按 `require` 的版本去拉。
所以开发用的 `replace` 一直留着，发布只把 `require` 钉成要发的版本——没有单独的发布提交，也没有事后还原的提交。
两次发布之间 `main` 的 go.mod 写着上一个版本号，靠 `replace` 编的是工作区里的代码，不影响开发。

- `--bump` 连不发布的 `example`、`e2e`、`internal/schemagen` 一起钉：它们 require 的子模块又 require 核心的新版本，
  自己还写着旧版本的话 `go vet` 就报 go.mod 要更新。tag 只打要发布的那几个。
- `--tag` 先确认这个提交能发：工作区干净、仓库内的 require 全是这个版本、CHANGELOG 有 `## [vX.Y.Z]` 这一节、
  tag 还没用过。然后跑 check、单测和 e2e（`--e2e-passed` 跳过这一步的 e2e：这个提交在别处跑绿过，或者像发布按钮那样紧接着、推送之前再跑）。
- 只收 v0 / v1。推送之后 tag 就被 module proxy 永久缓存，删不掉，只能再发一版盖过去。

### 基准测试

改热点代码前后各跑一次，改动要有数字支撑（见[优雅优先于快](#优雅优先于快)）：

```bash
go test -run=NONE -bench=. -benchtime=100000x ./xlog/ ./xflow/ ./xgin/middleware/ ./xecho/middleware/
```

## e2e：真实 Web 服务测试

`e2e/` 把 `e2e/service` 编成二进制、当成一个真的进程起起来，连真的 PostgreSQL、MySQL、Redis、ClickHouse 和 Kafka，
发请求、发信号，再读它的日志、Span、`/metrics` 和 `/proc`。

- 服务的 XGorm 是多实例：`default` 连 PostgreSQL，`mysql` 连 MySQL；`ch` 连 ClickHouse，写在 profile
  `e2e/service/application-ch.yml` 里，只有 `TestClickHouse_*` 起的进程激活它（`harness.Options.ClickHouse`）；
- XKafka 同理写在 `e2e/service/application-kafka.yml` 里，只有 `TestKafka_*` 起的进程激活它（`harness.Options.Kafka`），
  每个用例自己建 topic、用自己的消费组；
- 故障经 harness 里的 TCP 代理注入（断开、拒绝新连接、加延迟、模拟主机宕机），不去停真的服务；
- 用例之间各用各的端口、表名和 key 前缀；
- PG / MySQL / Redis 没在跑时脚本先按本机的装法拉起来（`pg_ctlcluster` / `service mysql` / `redis-server`），
  连接参数用 `XONE_E2E_PG_ADDR`、`XONE_E2E_MYSQL_ADDR`、`XONE_E2E_REDIS_ADDR` 等覆盖；服务归别处管（CI 的服务容器、
  docker compose、另一台机器）时设 `XONE_E2E_EXTERNAL=1`（地址不在本机、或本机没有对应的启动命令时也一样），脚本只等它就绪（最多 60 秒，只看 TCP）；
- ClickHouse 跑在 Docker 容器 `xone-ch` 里（`XONE_E2E_CH_*` 覆盖），起不来时脚本设 `XONE_E2E_CH=0`，`TestClickHouse_*` 各自跳过；
- Kafka 跑在 Docker 容器 `xone-kafka` 里（`mirror.gcr.io/apache/kafka:3.9.1`，KRaft 单节点，`XONE_E2E_KAFKA_*` 覆盖），
  没有这个容器时脚本按 `e2e/compose.yml` 的参数建一个；起不来时设 `XONE_E2E_KAFKA=0`，`TestKafka_*` 各自跳过；
- 后面的参数原样交给 `go test`。

本机没装这些服务的话，用 `e2e/compose.yml` 一次起齐，账号密码和脚本的默认值一致：

```bash
docker compose -f e2e/compose.yml up -d --wait   # 起来并等到健康；默认端口被占了用 XONE_E2E_PG_PORT 等换一个
scripts/e2e.sh
docker compose -f e2e/compose.yml down           # 数据在 tmpfs 里，down 了就没了
```

```bash
scripts/e2e.sh                        # 全部（压测除外）
scripts/e2e.sh -run Smoke             # 只跑冒烟
scripts/e2e.sh --load -run Load       # 压测：裸 gin 对照三种中间件配置、并发 1～256，外加几分钟的浸泡
scripts/e2e.sh --load -run MySQL      # MySQL 那一组，连同对照压测
scripts/e2e.sh --load -run ClickHouse # ClickHouse 那一组，连同对照压测
```

压测默认不跑（一轮十来分钟，要独占机器）。结果只在测试日志里打成表格；压测器、被测服务和数据库在同一台机器上抢 CPU，
这些数字只能拿来互相比较。

**CI**：`.github/workflows/e2e.yml` 在改了任何 `go.mod` / `go.sum` 的 PR、每晚、手动触发时跑（不含压测），四个服务是服务容器；
PG / MySQL / Redis 的 TLS 用例要以 root 在本机另起实例，runner 上跳过。任务摘要里写着 `KNOWN BUG` 跳过了几条。
`test.sh` 遍历到 `e2e` 模块时，每个测试都因为 `XONE_E2E` 不为 1 而跳过，没有数据库的机器上照样全绿。

**harness 的规矩：**

- 三个二进制（`service`、`baseline`、`covapp`）一次 `go build -race` 编完（压测除外）。每个被测进程退出时 harness 查它的输出
  （`harness/output.go` 的 `checkOutput`），查出问题就让那个用例失败：stderr 里有 `WARNING: DATA RACE`；或者 stdout / stderr
  里有一行不是 JSON——使用者的日志平台按行解析 JSON，哪个三方库绕开 slog 写了一行纯文本，在他们那边就是一条解析失败的垃圾。
  放过的只有框架自己预期会写的（xlog 装好之前的默认格式行、`MustRun` 的最后一行错误、运行时的 panic 输出、被信号杀掉时没写完的一截）；
  输出本来就不是 JSON 的用例在 `Options.NonJSON` 里写上理由。
- **开关一律走 `Options.Overlay`。** Overlay 是叠在 `service/application.yml` 上的一份 profile，
  用例里写的就是使用者会写的 YAML；常用的片段（`stopTimeout`、`sqlLog`、`bodyLogs`……）在 `functional_test.go`。
  `E2E_*` 环境变量只用来注入每个进程各不相同的值（端口、连接串、表名、文件路径），不当开关用。
  `application.yml` 不替框架写默认值：没写的项就是框架的默认值，测默认行为的用例拿到的是真的默认值。被测服务要多一个接口给 `TestCoverage_*` 用时挂在 `/probe` 下。
- **时长断言**：下界照实卡，上界给慢机器留余量——带 `-race` 的服务慢几倍，用例又都 `t.Parallel`。上界写成「量出来的数 × 3」
  或「文档推出来的数 + 一个具名的余量常量」（`faultSlack`、`faultUnaffected`），常量的注释里写清量出来是多少、反例是多少。
- **用例照文档写的行为断言**，揭示了框架的 bug 时不改断言去迁就它，而是标成 `KNOWN BUG` 跳过，证据写在用例的注释里。

## 规矩

### 语言

| 位置 | 语言 |
|---|---|
| 对话、代码注释、README 与 docs | 简体中文 |
| **error / panic 的消息** | **英文** |
| **日志的 message 与字段名** | **英文** |
| commit message | 英文 |
| 测试函数名、基准函数名 | 英文（描述场景，便于定位，如 `TestNew_FailsFastOnTypo`） |

这是一个给别人用的库。**它产出的错误和日志会落进使用者的系统里**——进他们的告警、他们的日志检索、他们的 issue。
中文字段名还会变成 JSON 的 key，让日志平台的索引和看板直接对不上。注释是写给读这份代码的人的，那是另一回事。

```go
// 连接池满了就等着，不新建连接 —— 注释用中文
return xerror.Newf("xgorm", "connect", "cannot reach %s: %w", addr, err) // 错误用英文
slog.Info("xgorm ready", "instances", reg.Names())                       // 日志用英文
```

文档和注释里的示例用中性的值（时区写 `Europe/Berlin`，profile 写 `prod,eu`），不写尚未发布的「之前的版本 / 行为变化」。

### 优雅优先于快

**不做过度优化。** 代码要保持优雅、整洁、好读——这比省下几十纳秒重要得多。只有同时满足以下两条才动手优化：

1. **量过**。有基准测试给出改前改后的数字，不接受「看着像是慢」。
2. **改完的代码不比改前复杂**。最好是更简单——把一次多余的分配去掉、把白干的活挪到条件后面，这类改动往往同时让代码更清楚。

为了性能引入缓存层、对象池、手写序列化、`unsafe`，一律先说明为什么没有别的办法。

### 错误一律用 xerror

模块对外返回的每一个错误都是 `*xerror.Error`，带上模块名和操作名：

```go
return xerror.Newf("xgorm", "connect", "cannot reach %s: %w", info.Addr, err)
return xerror.New("xgorm", "config", err)
```

调用方因此永远可以问「这是谁报的」：`xerror.Is(err, "xconfig")`（整棵树里有没有）、`xerror.Module(err)`（最外层是谁）。四条规矩：

1. **底层错误一律用 `%w`，不用 `%v`。** `%v` 把错误变成一段文本，`errors.Is` / `errors.As` 到此为止。
2. **一个模块边界一个 xerror，不是一层一个。** 内部的中间错误（比如 `Config.Validate()` 返回的那些）保持普通 error，
   由边界那一层包一次。每层都包的话文本会套成 `xone xgin config failed, err=[xone xgin validate failed, err=[...]]`。
   xerror 自己也兜着这一条：`New` 遇到同模块的 Error 原样返回；`Newf` 的参数里有 Error 时渲染会折叠——同模块的只留
   「op: 原因」，别的模块的去掉重复的 `xone ` 前缀。兜底不是许可，边界上照样只包一次。
3. **消息里不再重复模块名。** 外框已经有了，再写一遍就是 `xone xgorm connect failed, err=[xgorm: ...]`。
4. **op 从这组词里选**，不要每处现编：

   | op | 用在 |
   |---|---|
   | `config` | 配置不合法、解码失败 |
   | `new` | 构造实例 |
   | `connect` | 建连、探测 |
   | `close` | 关闭、释放 |
   | `register` | 注册指标、注册方言 |
   | `start` / `stop` | 服务启停 |
   | `execute` | 跑一次业务流程（xflow） |

### 第三方库的默认值一律要量过

这个仓库被外部 review 挑出来的问题里，**大半是同一个毛病**：接了一个库，用了它的默认行为，没量过它到底是什么行为，
然后按自己以为的那个写进文档。已经踩过的（每一条都是真的量出来才发现的）：

| 库 | 以为的 | 实际的 |
|---|---|---|
| gin | `TrustedProxies` 默认安全 | 默认 `0.0.0.0/0`，谁发 `X-Forwarded-For` 谁就是 `client_ip` |
| gin | `MaxMultipartMemory` 是请求体上限 | 是落盘阈值，堆开销约为它的三倍；32MB 默认 = 每请求 96MB |
| gorm | 不给 Logger 就是不打日志 | 补上它自己的默认：带 ANSI 颜色写 `os.Stdout` |
| gorm | `gorm.Open` 只装配 | 会自己 ping 一次，用的是它自己的 context |
| gorm | 日志里的 SQL 带的是占位符 | Logger 不实现 `ParamsFilter` 就代进真实参数值 |
| gorm | 实现了 `ParamsFilter` 就不会记参数 | `Scan` 期间换成 `logger.Recorder`，它只认进程级的 `RecorderParamsFilter`，默认原样交出参数 |
| gorm MySQL 驱动 | 建连受调用方的 ctx 管 | `Initialize` 用 `context.Background()` 查 `SELECT VERSION()`：不重试，启动期间的 SIGTERM 要等满 `ReadTimeout` |
| gorm ClickHouse 驱动 | 建连受调用方的 ctx 管 | `Initialize` 用 `context.Background()` 查版本：ctx 取消了也等满 dial_timeout，也不重试 |
| go-sql-driver | 日志走调用方配的 logger | 默认往 stderr 写 `[mysql] …` 纯文本，不是 JSON |
| MySQL / PG | 服务端的错误文本不含数据 | 1062 / 1366 / 1292 的原文、PG 的 `Detail` 带着参数值（`Duplicate entry 'a@b.com'`） |
| pgx | `sslmode=prefer` 至少加密且可信 | 走 TLS 但**不校验服务端证书**，服务端不肯 TLS 就悄悄退回明文 |
| go-redis | 命令听调用方的 deadline | 默认不听，只认 `ReadTimeout`；实测 200ms 的预算等满 5s |
| go-redis | 一次建连拨一次号 | `DialerRetries` 默认 5 次、间隔 100ms：主机宕机时一条命令 11.7s |
| go-redis | 新连接只发 `HELLO` | 还发 `CLIENT SETINFO` 和 `CLIENT MAINT_NOTIFICATIONS`；Redis 7.0 上每条连接留一个报错的 Span |
| go-redis | 日志走调用方配的 logger | 默认往 stderr 写 `redis: …` 纯文本 |
| redisotel | Span 里只有命令名 | 默认 `db.statement` 带整条命令，`SET k v` 的值原样导出 |
| clickhouse-go | `read_timeout` 是「多久没收到字节」 | 管一整段读、中途不续期：健康的长查询照样失败；读超时的查询被 `database/sql` 重发共 3 次 |
| clickhouse-go v2.30 | 读超时的连接会被丢掉 | 还回池里，下一条借到它的查询返回了上一条的结果（v2.47 修掉） |
| ristretto | `MaxCost` 就是容量 | 每条另加 56 字节内部开销，配 2000 实际存 35 条 |
| ristretto | `Close()` 能和在途读写并发 | 先关内部 channel、后置标记，实测并发读写里 750 个协程 panic |
| resty | `Timeout` 管一次请求 | 管一次尝试；配 300ms + 3 次重试实测跑 1.24s |
| resty | 不给 logger 就不打日志 | 往 stderr 写 WARN / ERROR，URL 带着查询串 |
| resty | `resty.New()` 和 `NewWithClient` 一样 | 前者自带 cookie jar，不相干的调用之间串 cookie |
| otelhttp | `CloseIdleConnections` 能传下去 | 它没实现，整条调用变成空操作 |
| otelhttp | 出站 Span 里没有凭证 | `url.full` 带着查询串，只去掉了 user:password |
| net/http | `Shutdown` 超时会断开连接 | 只返回错误，在途连接照跑 |
| net/http | 跨 host 重定向不带凭证 | 只去掉 `Authorization`、`Cookie` 这几个，自定义的 `X-Api-Key` 照样带给新 host |
| x/net h2c | `h2c.NewHandler` 的连接归 `Shutdown` 管 | 连接被劫持走，`Shutdown` 约 60µs 就返回 nil，在途请求照跑 |
| client_golang | 不合规的 Namespace 会报错 | 不报错，导出时转义：`my-app` 变成 `my_app_` |
| OTel SDK | `AlwaysSample` 尊重上游的采样决定 | 无视上游的 `sampled=00`，还把 `-01` 往下游传 |

所以接一个新库、或者升级一个库的时候：

1. **写进文档的每一句行为描述，先用一段代码量出来**，别照抄它的 README。
2. **我们没显式设的字段就是我们接受了它的默认值**——列一遍这些字段，逐个问「它的默认值是什么，我知道吗」。
3. 量出来的数字（连同依赖版本）写进注释和那个模块 README 的「行为与实测」一节（跨模块的写进 [`behavior.md`](behavior.md)）。后来的人不必再量一次，升级依赖之后数字对不上也能立刻看出来。

### 变异测试

`scripts/mutate.py` 是上面那件事的兜底：把每条承诺对应的代码改坏，看有没有测试会失败。
**活下来的变异 = 一条没有牙齿的承诺。** 改完安全或生命周期相关的代码跑一次（`--only <module>` 只跑相关的）。

- 变异表在 `scripts/mutations/` 下，一个 Go module 一个文件：`core.py` 是根模块（含 `./xhook` 这类根模块里的目录），
  其余按 module 目录名，如 `xgin.py`、`xgorm.py`、`clickhouse.py`、`schemagen.py`。一行放在哪个文件，看它在哪个目录下跑测试
  （第三个参数），放错了 `mutate.py` 直接报错。
- 每条变异是一行 `mutate(名字, 文件, 目录, 测试过滤, 改法...)`，前面的注释写「这条承诺防的是哪次真出过的事」。
  **改法只写 `swap()` / `cut()`**，它们自带「模式恰好匹配 N 处」的断言。裸写 `s.replace()` 有两种烂法：模式不再匹配就静默空转，
  匹配到多处就一次改坏两个地方——后者测试照样会红，但红的已经不是你要验的那条承诺了。
- **重构挪动了代码之后，对应的变异要跟着挪。** 变异模式失效（改不动任何东西）和「改坏了没人发现」一样严重：那条承诺这一轮
  根本没被检查。脚本会把这种情况单独报出来并且整轮失败——它自己就这样烂过两次，`applyConfig` 挪走 `SetTrustedProxies`、
  xerror 改了 `safeNew` 的签名，对应的变异都从此没再跑过。`--dry-run` 不到一秒，`check.sh` 每次都跑。
- **变异要打在调用点上，不只是被调用的函数里。** 「指标的 method 标签收敛」原先只有一个直接调 `normalizeMethod` 的单元测试：
  函数本身是对的，但没人验证中间件真的在用它。把调用点绕开（`normalizeMethod(m)` → `m`）测试照过，而那正是这个 bug 的形状。
- 只有「我们自己有代码在守」的承诺才放进来。由依赖库保证的性质没有哪一行可以改坏，那种靠测试守着就行。

### 配置与文档

- 默认值预填在结构体里，未知字段是错误，`${VAR}` 未设置是错误，校验写在 `Validate()` 里（读配置时就跑）。
- 新增或改动 Config 字段：重新生成 schema，写进那个模块 README 的 `## 配置` 一节（`check.sh` 双向检查）。
- **文档按模块放。** 每个模块目录下的 `README.md` 管这个模块的全部文档，固定四节（没有内容的那节省掉），节里再分用 `###`：

  | 一节 | 放什么 |
  |---|---|
  | `## 配置` | 参考：YAML 块 + 几条要点。`check.sh` 按这一节核对字段 |
  | `## 行为与实测` | 长的解释、库的默认、量出来的数字和依赖版本 |
  | `## 可观测` | 这个模块产出的日志消息和字段、指标、Span 名和属性 |
  | `## 排错` | 这个模块自己的报错文案 |

  `docs/` 只放跨模块的：`config.md` 是配置文件的加载、合并、占位符和「配置块 → 模块文档」的索引，`behavior.md` 是总表和
  启动期建连探测，`observability.md` 是日志 / 指标 / 链路的全局约定和传播规则，`troubleshooting.md` 是配置加载、`C()`、
  Runnable 与退出、建连这些不属于某一个模块的报错。一句话同时说到几个模块时写进 `docs/`，模块 README 里放链接。
- 代码注释和测试里引用文档写成 `xgin/README.md「访问日志」`（路径 + 标题），挪了标题要跟着改。
- 根目录 README 的第一个 ```go 代码块由 `example/readme_test.go` 编译一遍，改公开 API 时跟着改。

### 更新日志

**打第一个版本 tag 之前不记录。** 之后，使用者看得见的变化（新功能、行为变化、不兼容变更、修复、性能）**在同一个提交里**
写进 `docs/CHANGELOG.md` 的「未发布」一节，按「不兼容变更 / 新增 / 修复 / 性能」归类，一条一句话，写使用者要知道什么、
要改什么，不写实现细节。仓库内部的重构和工具改动不写。不兼容变更必须写清旧写法怎么迁移。
发版时把「未发布」改成 `[vX.Y.Z] - 日期`，再在上面开一个新的空「未发布」。
