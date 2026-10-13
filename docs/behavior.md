# 与底层库不同的默认值与实测

框架接的每个库都有自己的默认行为，其中不少和它的 README、和「大家以为的」不一样。这份文档是**总表**：
每一行是一处「库的默认 ≠ 这里的默认」，链接跳到那个模块 README 的「行为与实测」，那里有实测的数字和依赖版本——
升级依赖之后数字对不上，就说明行为变了，要重新量。跨模块的启动期建连探测也写在这里。

实测环境：没有特别说明的都是本机回环、Go 1.25；带 e2e 的是 `e2e/` 下真起进程、连真服务
（PG 16、MySQL 8.0.46、Redis 7.0.15、ClickHouse 24.8.14）量出来的。

- [总表](#总表)
- [启动期建连探测](#启动期建连探测)
- [各模块的实测](#各模块的实测)

## 总表

| 配置 | 库自己的默认 | 这里的默认 | 为什么 |
|---|---|---|---|
| [`XGin.TrustedProxies`](../xgin/README.md#行为与实测) | 全都信（`0.0.0.0/0`） | 只信私有网段（`private`） | 否则谁发 `X-Forwarded-For` 谁就是访问日志里的 `client_ip`，限流和审计跟着失效；私有网段是负载均衡、Ingress、Pod 所在的那一跳 |
| [`XGin.MaxMultipartMemory`](../xgin/README.md#行为与实测) | 32MB | 8MB | 它是落盘阈值不是请求体上限，堆开销约为它的三倍 |
| [`XGin.ReadHeaderTimeout: 0`](../xgin/README.md#行为与实测) | 退到 `ReadTimeout`（默认 0），即不限时 | 启动失败 | 发半个请求头就能一直占着连接 |
| [`XGin.UseH2C`](../xgin/README.md#行为与实测) | x/net 的 `h2c.NewHandler` | 标准库的 `Protocols.SetUnencryptedHTTP2` | 前者劫持连接，`Shutdown` 管不到在途请求 |
| [gin `RedirectTrailingSlash`](../xgin/README.md#行为与实测) | 开着：`/users/` 回 301（GET）/ 307（其余方法）到 `/users` | 关掉，走 `NoRoute` 记 404 `unmatched` | 重定向的请求不跑任何中间件，访问日志、指标、链路里都没有 |
| [gin `ContextWithFallback`](../xgin/README.md#行为与实测) | 关着：`*gin.Context` 的 `Value` 只查 `c.Keys`、`Done()` 是 nil | 开着，转到 `c.Request.Context()` | 否则 `xlog.AddKV(c, ...)`、`Start(c, ...)` 丢掉日志作用域和父 Span，取消也传不下去 |
| [`http.Server` 劫持的连接](../xgin/README.md#行为与实测) | `Shutdown` 不等、`Close()` 断不掉，请求 ctx 不取消 | `Shutdown` / `Close()` 之后取消 `BaseContext` | 否则等着 ctx 的 WebSocket handler 让 `Stop` 等满预算再报错 |
| [`XEcho.TrustedProxies`](../xecho/README.md#行为与实测) | `IPExtractor` 为 nil：`X-Forwarded-For` / `X-Real-IP` 谁发来的都信 | 只信私有网段（`private`），算法同 xgin | 实测公网对端发 `X-Forwarded-For: 1.2.3.4`，`c.RealIP()` 就是 `1.2.3.4` |
| [`XEcho` 错误响应](../xecho/README.md#行为与实测) | 整条中间件链返回之后才由 `HTTPErrorHandler` 渲染 | 内置中间件在自己这一层渲染 | 否则 404 / 405 / 500 在访问日志、指标、链路里都是 `bytes_out` 为 0 的 200 |
| [`XEcho` panic](../xecho/README.md#行为与实测) | 不兜：连接断掉，栈由 net/http 写 stderr | 兜住，记 ERROR 日志、回 500 | 栈进不了日志平台，客户端只看到 EOF |
| [`XEcho` 服务器超时](../xecho/README.md#行为与实测) | `e.Server` 四个超时全是 0 | 不用 `e.Start`，同 `XGin` 的四个超时 | 同 `XGin.ReadHeaderTimeout` |
| [echo 自己的日志](../xecho/README.md#行为与实测) | gommon 写 `os.Stdout`，自己的 JSON 格式 | 接到 slog，级别不变 | 绕开 slog 的输出进不了日志平台 |
| [`XEcho` 分组的兜底路由](../xecho/README.md#行为与实测) | 带中间件的分组悄悄注册 `RouteNotFound`，`c.Path()` 是 `/api/v1/*` | 落到兜底上的记 `unmatched` | 否则 404 在看板上是 `/api/v1/*`，和 xgin 对不上 |
| [`XEcho` 的 `e.Pre`](../xecho/README.md#行为与实测) | 挂在 `e.Use` 上的中间件看不到 Pre 里结束的请求，也兜不住 Pre 里的 panic | 内置中间件挂在 `e.Pre` 上，Pre 里不动请求、ctx 存在 `echo.Context` 上，路由之后才换到请求上 | Pre 里的鉴权、限流、301 不进日志和指标；Pre 里换请求的话 `MethodOverride` 失效 |
| [`http.Server.ErrorLog`](../xgin/README.md#日志) | nil：写标准库的 log（经 slog 是 INFO、消息每行不同） | 接到 slog，WARN，消息 `<模块> http server error` | TLS 握手失败这类错误没法按消息检索和告警 |
| [`XGorm.Log: false`](../xgorm/README.md#行为与实测) | 换成 GORM 自己的 stdout logger | 真的不打 | 那个默认实现带 ANSI 颜色直写 `os.Stdout` |
| [`XGorm.Log: true`](../xgorm/README.md#行为与实测) | 参数值代进 SQL 再记 | 只记带占位符的 SQL | 否则 `WHERE password = ?` 记下来的是真实的密码 |
| [`XGorm` 建连](../xgorm/README.md#行为与实测) | `gorm.Open` 自己 ping 一次 | 关掉，走框架的 ctx-aware 探测 | 它用自己的 context，退出信号和重试都管不到 |
| [`XGorm` MySQL / ClickHouse 查版本](../xgorm/README.md#行为与实测) | `Initialize` 里用 `context.Background()` 查 | 挪进建连探测 | 那是第一次建连，失败不重试、不听退出信号 |
| [`XGorm` MySQL `parseTime`](../xgorm/README.md#行为与实测) | `false` | DSN 没写时补 `true` | 否则 `DATETIME` 扫不进 `time.Time` |
| [`XGorm` 服务端错误原文](../xgorm/README.md#行为与实测) | 原样进日志和 Span | 只记错误码 | 原文带着参数值 |
| [go-sql-driver 的日志](../xgorm/README.md#行为与实测) | 标准库 log 写 `os.Stderr` | 接到 slog，记成 WARN | 绕开 slog 的纯文本进不了日志平台 |
| [`XRedis` 命令超时](../xredis/README.md#行为与实测) | 只认 `ReadTimeout` | 听调用方 ctx 的截止时间 | 不然 200ms 预算的请求会等满 `ReadTimeout` |
| [`XRedis` 建连](../xredis/README.md#行为与实测) | 每次建连内部重拨 5 次 | 只拨一次 | 否则主机宕机时一条命令 11.7s |
| [`XRedis` 新连接上的握手](../xredis/README.md#行为与实测) | `CLIENT SETINFO`、维护通知都开 | 都关 | 7.2 之前的服务端每条连接留一个报错的 Span |
| [`XRedis.Trace`](../xredis/README.md#行为与实测) | 整条命令连同参数写进 `db.statement` | 只有命令名 | 否则 `SET` 的值原样进链路后端 |
| [go-redis 的日志](../xredis/README.md#行为与实测) | 标准库 log 写 `os.Stderr` | 接到 slog，记成 WARN | 同 go-sql-driver |
| [`XKafka` 消费的提交](../xkafka/README.md#行为与实测) | 自动提交「上一次 poll 取到的」，不管处理完没有 | 只提交处理完的（`AutoCommitMarks`） | 每个分区一个协程时，排在队列里没处理的消息先被提交，进程一崩就丢 |
| [`XKafka.Consumer.ResetOffset`](../xkafka/README.md#行为与实测) | earliest | latest | 否则新上线的消费组把 topic 里留着的全部历史处理一遍 |
| [`XKafka` 停止](../xkafka/README.md#行为与实测) | `Close` 让缓冲里的消息全部失败 | 先 `Flush` 再 `Close` | 否则退出那一刻 `Produce` 出去的消息静默丢掉 |
| [`XKafka` 启动探测](../xkafka/README.md#行为与实测) | `Ping` 对不回话的 broker 不听 ctx，等满 10s | 放进协程、看着 ctx | 否则启动期间的退出信号要等 10s |
| [`XKafka` 的 franz-go 日志](../xkafka/README.md#行为与实测) | 不记 | 接到 slog，WARN 及以上 | 连不上、再均衡出错悄无声息 |
| [`XKafka.Trace`](../xkafka/README.md#可观测) | kotel 把消息的 key 写进 Span | 不用 kotel，Span 里没有 key | key 里常有用户 ID、手机号 |
| [`XCache.MaxCost`](../xcache/README.md#行为与实测) | 每条另计 56 字节内部开销 | 只算你给的 cost | 否则 `MaxCost: 2000` 实际只存得下 35 条 |
| [`XCache` 停止](../xcache/README.md#行为与实测) | `Close` | `Clear` | `Close` 与并发读写一起跑会 panic |
| [`XHttp` 的 resty 日志](../xhttp/README.md#行为与实测) | 写 `os.Stderr`，URL 带查询串 | 接到 slog，去掉查询串 | 令牌不落盘 |
| [`XHttp` 的 cookie](../xhttp/README.md#行为与实测) | `resty.New()` 自带 cookie jar | 没有 | 不相干的调用会共享别人种下的会话 cookie |
| [`XHttp` 出站 Span](../xhttp/README.md#行为与实测) | `url.full` 带查询串 | 去掉查询串，Span 名只用方法 | 令牌不进链路后端，Span 名基数有界 |
| [`XHttp` 重试条件](../xhttp/README.md#行为与实测) | 挂上条件后 resty 自己的判断作废 | 只重试传输层错误 | 否则 200 + 坏 JSON 也会重试 |
| [`XTrace` 透传与 baggage](../xtrace/README.md#行为与实测) | 入站的值照单全收 | 只收直连对端在 `XGin.TrustedProxies`（`XEcho.TrustedProxies`）里的 | 否则公网客户端能伪造 `X-Tenant-Id` 带进内网 |
| [`XTrace` 采样](../xtrace/README.md#行为与实测) | `AlwaysSample` 无视上游 | `ParentBased`，有上游时听上游 | 否则上游 `sampled=00` 被改成 `-01` 往下传 |
| [`XMetric.Namespace`](../xmetric/README.md#行为与实测) | 不合规的名字导出时转义 | 读配置时失败 | 否则看板按你写的名字查不到 |

## 启动期建连探测

XGorm、XRedis、XKafka 启动时各探一次，共用同一份实现（`internal/xclient.Probe`，基于 `xutil.Retry`）：

- **最多试 3 次**（第一次加两次重试），两次之间的等待逐次翻倍并带抖动：退避从 1s 起，
  实际等待在 `[0, 当前退避]` 之间取值，两次退避的上界是 1s、2s。
  所以一个实例最多等 `3 × 单次探测预算 + 3s`：XGorm 连 PostgreSQL 默认 `3 × 1.5s + 3s = 7.5s`，
  XRedis 默认 `3 × 1s + 3s = 6s`，XKafka 默认 `3 × 2s + 3s = 9s`。
- **认证失败不重试**：PostgreSQL 的 SQLSTATE 第 28 类（密码错、用户不存在都是 28P01）；
  MySQL 的 1045（密码错、用户不存在）和 1044（没有这个库的权限——没有全局权限的账号连一个不存在的库
  拿到的也是 1044；实测 MySQL 8.0.46）；Redis 的 `WRONGPASS` / `NOAUTH`；ClickHouse 的 516 和 192 / 193 / 194；Kafka 的 `SASL_AUTHENTICATION_FAILED` 等（未在真 Kafka 上量过，见 xkafka）。
  错误报 `authentication to <地址> failed`，不是 `cannot reach`。
- **证书被拒不重试**：`x509` 校验失败，或者对端发来 `bad_certificate` / `unknown_ca` /
  `certificate_required` 告警。再试还是同一张证书、同一个结论。
- 启动期间收到退出信号时当场放弃，不等卡着的那次探测撞上超时。

## 各模块的实测

| 模块 | 实测 |
|---|---|
| [xtls](../xtls/README.md#行为与实测) | 客户端 TLS：各驱动的报错、耗时，不开 TLS 块时驱动的默认 |
| [xgorm](../xgorm/README.md#行为与实测) | GORM 通用、MySQL、PostgreSQL |
| [xgorm/clickhouse](../xgorm/clickhouse/README.md#行为与实测) | ClickHouse：超时与取消、重发、认证、TLS |
| [xredis](../xredis/README.md#行为与实测) | go-redis：超时、建连重试、新连接上的握手、内存 |
| [xcache](../xcache/README.md#行为与实测) | ristretto：内部开销、停止、指标开销、TTL |
| [xkafka](../xkafka/README.md#行为与实测) | franz-go：自动提交、重置位置、`Close` 与 `Flush`、`Ping`、不存在的 topic、再均衡和会话超时 |
| [xhttp](../xhttp/README.md#行为与实测) | resty / otelhttp / 标准库 Transport 的默认 |
| [xcron](../xcron/README.md#行为与实测) | cronexpr：段数、时区前缀、夏令时、永远不会到的 spec；`@every` 的漂移 |
| [xgin](../xgin/README.md#行为与实测) | gin / net/http：代理、上传、超时、h2c、TLS、优雅退出 |
| [xecho](../xecho/README.md#行为与实测) | echo：client_ip、错误渲染、路由模板、分组的兜底路由、`e.Pre`、超时中间件、panic、gommon 日志、压缩、multipart 阈值、路由的严格匹配 |
| [xmetric](../xmetric/README.md#行为与实测) | client_golang：桶、名字、常量标签 |
| [xtrace](../xtrace/README.md#行为与实测) | OTel SDK：采样、service.name、透传 |
| [xlog](../xlog/README.md#行为与实测) | `Perm`、轮转文件名、时区 |
