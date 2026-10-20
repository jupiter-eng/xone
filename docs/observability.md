# 可观测性：日志、指标、链路

框架产出的日志、指标和 Span 的全局约定在这里；每个模块自己的日志字段、指标和 Span 在它 README 的「可观测」一节，
照着建看板、写告警、配日志检索。所有字段名和消息都是英文，名字就是代码里写的那个。开关在各模块 README 的「配置」一节里
（索引见 [`config.md`](config.md#配置块--模块文档)）。

| 想要 | 去哪 |
|---|---|
| 业务日志带上 `trace_id`、请求级字段 | [日志](#日志)：用 `slog.InfoContext(ctx, …)`、`xlog.AddKV(ctx, k, v)` |
| 访问日志的字段、脱敏规则 | [xgin「访问日志」](../xgin/README.md#访问日志)（xecho 的字段和规则相同，见 [xecho「访问日志」](../xecho/README.md#access-log)） |
| 建看板：框架自带哪些指标 | [指标](#指标) |
| 链路怎么传、谁的 Header 会被收下 | [传播与信任边界](#传播与信任边界) |
| 把 Span 上报出去 | [xtrace「快速上手」](../xtrace/README.md#快速上手)：`xtrace.AddSpanProcessor(...)` |

- [日志](#日志)
  - [框架自己的日志](#框架自己的日志)
- [指标](#指标)
- [链路](#链路)
  - [Span 的名字和属性](#span-的名字和属性)
  - [传播与信任边界](#传播与信任边界)

## 日志

xlog 把 `slog.Default()` 换成按 `XLog` 配好的 handler，业务和框架都写它。它跟着 xgin / xecho / xcron 来；
没有 Web 框架的程序（消费者、一次性任务、只用 xgorm / xredis / xkafka 的）要用它就匿名 import `github.com/jupiter-eng/xone/xlog`，
否则 `slog.Default()` 保持原样，下面这些都没有。要写到自己的日志后端（zap、公司的日志 SDK），
在 `xone.Run` 之前 `xlog.UseHandler(h)`，下面这些照样生效，见 [xlog「用自己的日志后端」](../xlog/README.md#用自己的日志后端)。

- **`trace_id` / `span_id`**：有链路时（见[链路](#链路)），用带 ctx 的方法（`slog.InfoContext(ctx, …)`）写的每一条都自动带上
  （由 xtrace 接上，xlog 本身不依赖 OpenTelemetry）。
- **请求级字段**：`xlog.AddKV(ctx, "user_id", id)` 在任意调用层级补一个字段，之后同一请求里的每条日志
  （包括访问日志）都带着它。作用域由 xgin / xecho 的 `LogScope` 中间件在每个请求开头开好；自己的非 Web 入口用
  `xlog.CtxWithScope(ctx)` 开。
- **一段调用的字段**：`xlog.CtxWithKV(ctx, map[string]any{"order_id": id})` 派生一个新 ctx，只有用它写的日志带着；
  父 ctx 已有的字段照样带上。批量处理的每一条、起的每个 goroutine 各派生一个，互相不串。

### 框架自己的日志

启停与建连各一条，都不含凭证。常用来检索的几条：

| 消息 | 级别 | 字段 |
|---|---|---|
| `loading config` | INFO | `file` |
| `no config file found, using defaults for everything` | WARN | `searched` |
| `starting` / `stopping` | INFO | `hook`（钩子函数名，如 `xgorm.initXGorm`） |
| `shutdown signal received, closing gracefully; send it again to terminate now` | INFO | `signal` |
| `xgorm ready` / `xredis ready` / `xcache ready` / `xkafka ready` | INFO | `instances` |
| `xgorm go-sql-driver log` / `xredis go-redis log` / `xhttp resty log` | WARN（resty 照搬它的级别） | `detail`：三方库原本写到 stderr 的那一行 |

各模块自己的日志在它 README 的「可观测」一节：[xgin](../xgin/README.md#可观测)（访问日志、`xgin listening`、`xgin http server error`）· [xecho](../xecho/README.md#observability)（访问日志、`xecho listening`、`echo internal log`、`xecho http server error`）· [xgorm](../xgorm/README.md#日志)（`xgorm connected`、SQL 日志）· [xredis](../xredis/README.md#日志)（`xredis connected`、`redis command`、`redis pipeline`）· [xhttp](../xhttp/README.md#日志)（`xhttp ready`、`http request`）· [xcache](../xcache/README.md#日志) · [xkafka](../xkafka/README.md#日志)（`kafka produce failed`、`kafka message failed` / `kafka message dead-lettered` / `kafka message skipped, …` / `kafka handler panicked`，处理函数的日志带 `topic` / `partition` / `offset` / `group`）· [xcron](../xcron/README.md#日志)（`cron job finished` / `cron job failed` / `cron job panicked`、`cron job skipped, previous run still running`）· [xflow](../xflow/README.md#日志)（`xflow flow done` / `xflow flow failed`；逐步的 `xflow step … done` 记 INFO、`… failed` 记 WARN）· [xtrace](../xtrace/README.md#日志)。

## 指标

框架自带的指标都带 `XMetric.ConstLabels`；除了 client_golang 现成的 `go_*` / `process_*`，名字前面还加 `XMetric.Namespace`（有的话）：

| 模块 | 指标 |
|---|---|
| [xgin](../xgin/README.md#指标) · [xecho](../xecho/README.md#metrics) | `http_requests_total`、`http_request_duration_seconds`（两者同名同标签，看板通用） |
| [xhttp](../xhttp/README.md#指标) | `http_client_request_duration_seconds` |
| [xgorm](../xgorm/README.md#指标) | `db_pool_*` |
| [xredis](../xredis/README.md#指标) | `redis_pool_*` |
| [xkafka](../xkafka/README.md#指标) | `kafka_consume_duration_seconds` |
| [xcache](../xcache/README.md#指标) | `cache_*` |
| [xmetric](../xmetric/README.md#指标) | `log_errors_total`、`go_*` / `process_*`，以及业务打点 |

几条要知道的：

- **`method`** 收敛到固定集合：`GET` `HEAD` `POST` `PUT` `PATCH` `DELETE` `CONNECT` `OPTIONS` `TRACE`，其余（包括小写的 `get`）
  一律 `OTHER`——方法是自由 token，照抄的话谁都能把时间序列撑爆。
- 连接池、缓存的指标在被抓取时才读；`Metric: false` 的实例不出现在 `/metrics` 里。
- 各集成的指标注册失败（比如同名指标已被注册成别的类型）**不让启动失败**，只打一条错误日志，那组指标导不出去。

## 链路

xtrace 装好全局的 TracerProvider 和 Propagator。会产生 Span 的集成（xgin、xecho、xgorm、xredis、xhttp、xcron、xkafka）都依赖它，
用了其中任何一个就有链路，不用另外 import；不要链路配 `XTrace.Enable: false`，只关某个组件的配它自己的 `Trace: false`。
一个都没用（比如只用 xcache 的消费者进程）又想要链路时，匿名 import `github.com/jupiter-eng/xone/xtrace`。
没有 xtrace 时全局的是 OpenTelemetry 的 noop 实现：各处照样调 Span 的接口，但什么都不记。

### Span 的名字和属性

各模块的 Span 名和关键属性在它 README 的「可观测 · 链路」：

| 模块 | Span |
|---|---|
| [xgin](../xgin/README.md#链路) | 入站，每个请求一个；另见 [X-Trace-Id 响应头](../xgin/README.md#x-trace-id-响应头)、[499](../xgin/README.md#499中止的请求) |
| [xecho](../xecho/README.md#tracing) | 入站，Span 名和属性同 xgin，错误属性叫 `echo.errors` |
| [xhttp](../xhttp/README.md#链路) | 出站 |
| [xgorm](../xgorm/README.md#链路) | 每条 SQL 一个，属性按 OTel 数据库语义约定 |
| [xredis](../xredis/README.md#链路) | 每条命令一个 |
| [xcron](../xcron/README.md#链路) | 每次执行一个根 Span `cron <name>` |
| [xkafka](../xkafka/README.md#链路) | 生产 `<topic> publish`、消费 `<topic> process`，消费接着消息头里的链路 |
| [xflow](../xflow/README.md#链路) | 不开 Span |

状态只在服务端的 5xx（xgin、xecho）、SQL 出错（xgorm）或定时任务失败（xcron）时标成错误；4xx 不算。服务端报错时状态描述只有错误码，
不调 `RecordError`（它会把带参数值的原文写进 `exception.message`）。

### 传播与信任边界

入站接、出站带的有三样东西，信任规则不同：

| | 格式 | 入站收谁的 | 出站 |
|---|---|---|---|
| 链路标识 | W3C `traceparent`、`b3` | 谁发来的都接（只是一个 id） | 注入 `traceparent` 和 `b3` |
| `baggage` | W3C `baggage` | **只收可信对端的** | 带上本进程 ctx 里的 baggage |
| 透传 Header | `XTrace.ForwardHeaders` / `ForwardHeaderRules` 里列的 | **只收可信对端的** | `ForwardHeaders` 发给所有下游，`ForwardHeaderRules` 只发给匹配域名的 |

- **链路标识谁的都接，采样决定也跟着接**：采样器是 `ParentBased`（见 [xtrace「行为与实测」](../xtrace/README.md#行为与实测)），
  上游带着 `sampled=01` 来的请求一律采样、导出，`SampleRatio` 管不到它们。公网客户端在每个请求上带一个 `traceparent: 00-…-01`，
  就能让这一跳和它往下的每一跳都记满 Span——代价落在链路后端的存储和导出带宽上。要限就在入口（网关、负载均衡）剥掉或重写外来的 `traceparent`。
- **可信对端 = 直连的那一跳在 `XGin.TrustedProxies`（用 xecho 时是 `XEcho.TrustedProxies`）里**（TCP 那一跳，不是从 `X-Forwarded-For` 推出来的 client IP）。
  `TrustedProxies` 默认只信私有网段（负载均衡、K8s 的 Ingress 和 Pod、sidecar），公网直连的对端发来的一个都不收。
  「谁是自己人」只在这一处说。
- 不可信的对端带着这些头来时各打一条告警，整个进程只打一次。
- 负载均衡一般原样转发客户端发来的头：把它写进 `TrustedProxies` 之前，先在它那里剥掉这些头，否则等于又信了所有客户端。
- `XGin.Trace: false`、`XEcho.Trace: false`、`XHttp.Trace: false` 只关这一跳的 Span，上面三样照常接、照常带。`XTrace.Enable: false` 时
  链路标识和 baggage 不再传播，透传 Header 照常。
- 不经过 xgin / xecho、自己调 `Extract` 的（比如从消息队列的消息头里取），carrier 要实现 `TrustedPeer() bool` 并返回 `true`
  才会被当作可信，否则一律不收 baggage 和透传 Header。xkafka 消费时把 Kafka 消息头当成可信的（能往 topic 里写的是持有集群凭证的生产者），
  理由和例外见 [xkafka「链路」](../xkafka/README.md#链路)。
- 业务代码里读透传的值：`xtrace.ForwardHeaderFromContext(ctx, "X-Request-Id")`。
