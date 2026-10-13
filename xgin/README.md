# xgin

Web 服务：拿到的是原生 `*gin.Engine`，`xgin.New()` 就是交给 `xone.Run` 的 Runnable，监听、优雅退出都是框架的事。

- 访问日志（凭证逐字段脱敏）、链路、指标（自动挂 `/metrics`）、panic 恢复已经装好
- 默认只信私有网段的代理（gin 自己默认全信）：负载均衡、Ingress、Pod 转发来的 `X-Forwarded-For` 照收，公网直连的伪造不了
- 慢连接有防线：`ReadHeaderTimeout` 10s、`IdleTimeout` 60s，写 0 启动失败
- HTTPS、双向认证、h2c 都在配置里开
- 停止时等在途请求做完，到点断连并报出还没返回的 handler 数
- 中止的请求、客户端走了还什么都没发的请求记 499，不记成 200

## 快速上手

```yaml
# conf/application.yml
XApp:
  Name: demo.user.api          # 链路里的 service.name
XGin:
  Port: 8080
  LogSkipPaths: [/healthz]     # 健康检查不记访问日志
```

```go
import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/jupiter-eng/xone"
	"github.com/jupiter-eng/xone/xgin"
)

func main() {
	xone.MustRun(xgin.New().WithRoutes(func(e *gin.Engine) {
		e.GET("/healthz", func(c *gin.Context) { c.Status(http.StatusNoContent) })

		v1 := e.Group("/api/v1")
		v1.GET("/users/:id", func(c *gin.Context) {
			ctx := c.Request.Context() // 客户端断开、服务停止时跟着取消，往下游传它
			slog.InfoContext(ctx, "get user", "user_id", c.Param("id")) // 自动带上这次请求的 trace_id
			c.JSON(http.StatusOK, gin.H{"id": c.Param("id")})
		})
	}))
}
```

`curl localhost:8080/api/v1/users/1` 的响应头带 `X-Trace-Id`；`curl localhost:8080/metrics` 能看到
`http_requests_total`，`route` 标签是 `/api/v1/users/:id`。

## 配置

```yaml
XGin:
  Host: "0.0.0.0"
  Port: 8080
  Mode: release            # release / debug / test，默认 release；进程级，多个服务要一致
  UseH2C: false            # 非 TLS 下启用 HTTP/2（只认先验知识，不支持 Upgrade: h2c）
  TLS:                     # 服务端证书；没有 Enable，CertFile 和 KeyFile 都配了就是 https
    CertFile: ""           # 与 KeyFile 同时配或同时留空
    KeyFile: ""
    ClientCAFile: ""       # 校验客户端证书的 CA；配了就是双向认证，需同时配证书
    MinVersion: "1.2"      # "1.2" / "1.3"，留空同 "1.2"；只在配了证书时生效、也只在那时校验
  ReadHeaderTimeout: 10s   # 慢连接攻击的主要防线，必须 > 0
  ReadTimeout: 0s          # 默认不限：限制它会打断大文件上传
  WriteTimeout: 0s         # 默认不限：限制它会打断 SSE、长轮询、大文件下载
  IdleTimeout: 60s         # 必须 > 0
  MaxMultipartMemory: 8388608  # 字节，默认 8MB；是落盘阈值，不是请求体上限
  TrustedProxies: [private]  # 信任哪些直连对端的 X-Forwarded-For 和透传 Header；private = 回环 + 私有网段，[] = 谁都不信
  Log: true                # 访问日志
  LogSkipPaths: []         # 不记访问日志的路径：以 / 结尾的按前缀，其余精确匹配
  LogQuery: false          # 查询串进访问日志（逐字段脱敏），默认关
  LogRequestHeaders: false  # 请求头进访问日志（凭证类脱敏），默认关
  LogRequestBody: false    # 请求体进访问日志（逐字段脱敏），默认关
  LogResponseHeaders: false  # 响应头进访问日志（凭证类脱敏），默认关
  LogResponseBody: false   # 响应体进访问日志，默认关
  Trace: true              # 每个请求一个服务端 Span、回带 X-Trace-Id
  Metric: true             # 请求指标，并自动挂上 MetricPath
  MetricPath: /metrics     # 必须以 / 开头；Metric 开着时自动加进 LogSkipPaths
  ZHTranslations: false    # validator 的报错翻成中文，用法见 xgin/trans
```

**同一进程里的第二个服务**（比如内部管理端口）不读配置文件，用 `WithConfig` 给一份完整配置，从 `CurrentConfig()` 改起：

```go
c := xgin.CurrentConfig()
c.Host, c.Port = "127.0.0.1", 9090
admin := xgin.New().WithConfig(c).WithRoutes(adminRoutes)
```

- `private` 展开成 `127.0.0.0/8`、`10.0.0.0/8`、`172.16.0.0/12`、`192.168.0.0/16`、`100.64.0.0/10`、`::1/128`、`fc00::/7`。
  列表整体替换默认值：要再加网段就连 `private` 一起写（`[private, 203.0.113.0/24]`），写错的网段启动失败；
  内网里也有不可信的客户端（办公网、VPN 用户能直连服务）时别用 `private`，写确切的那几段，或者 `[]`。
- `TLS.ClientCAFile` 管整个端口：`/metrics` 同样要客户端证书。handler 里用 `c.Request.TLS.PeerCertificates` 看是谁。

## API

| 函数 | 说明 |
|---|---|
| `New() *XGin` | 创建一个服务（Runnable），什么都不读，配置在装配那一刻才取 |
| `(*XGin).WithRoutes(f ...func(*gin.Engine))` | 注册路由，回调拿到原生 engine；可以调多次，按顺序生效 |
| `(*XGin).WithMiddleware(m ...gin.HandlerFunc)` | 追加自定义中间件，排在所有内置中间件之后 |
| `(*XGin).WithRecoverFunc(f gin.RecoveryFunc)` | 自定义 panic 之后的响应，默认回 500 |
| `(*XGin).WithConfig(c Config)` | 用这份完整配置起服务，不读配置文件里的 XGin 块 |
| `(*XGin).Engine() *gin.Engine` | 触发装配并返回原生 engine；之后再 `With...` 不生效 |
| `(*XGin).Start(ctx)` / `(*XGin).Stop(ctx)` | 监听 / 优雅停止，通常交给 `xone.Run`，不用自己调 |
| `CurrentConfig() Config` | 配置文件里 XGin 那一块的拷贝，`Start` 之前任何时候调都行 |
| `DefaultConfig() Config` | 全部默认值；`WithConfig` 从头写时从它开始 |
| `middleware.AddSensitiveFields(...string)` | 追加访问日志的脱敏敏感词（body 和请求头一起生效） |
| `middleware.AddSensitiveHeaders(...string)` | 追加精确匹配的敏感请求头名 |
| `trans.ToZH(err) error` / `trans.Msg(err) string` | `ZHTranslations` 开着时把 validator 的报错翻成中文 |

## 注意事项

- **请求体没有上限**：框架不替业务定，要限就在中间件里 `c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, n)`。
  `MaxMultipartMemory` 是落盘阈值，不是上限，堆开销约为它的三倍。见[「行为与实测」](#行为与实测)。
- **handler 里的慢操作传 `c.Request.Context()`**：`Stop` 没有单独的超时，等在途请求最多到停止预算的 2/3
  （`xone.WithStopTimeout` 的 2/3，默认 10s），到点断开连接、取消请求的 ctx；不看 ctx 的 handler 停不下来，
  `Stop` 会报 `N handler(s) still running`。要调就调 `WithStopTimeout`。
- **访问日志默认只记请求的概要**（方法、路由、状态、耗时、客户端、大小），查询串、请求头、body、响应头都要单独打开；
  开之前用 `middleware.AddSensitiveFields(...)` 补上业务自己的敏感字段。见[「访问日志」](#访问日志)。
- **`WithRoutes` 回调里的设置盖过配置**（如 `e.SetTrustedProxies`），但透传 Header 的可信判断只看配置里的 `TrustedProxies`，
  两边要一起改就改配置。XGin 块在装配（`Engine()` 或 `Start`）那一刻才读。
- handler 里 `c.Error(err)` 登记的错误进访问日志的 `errors` 字段和 Span；validator 报错要中文就开 `ZHTranslations`，再 `trans.ToZH(err)`。
- **路由和 gin 的默认不同**：末尾斜杠不重定向（只注册了 `/users` 时 `/users/` 是 404 `unmatched`，gin 默认 301 / 307）；
  路径对、方法不对回 405（gin 默认 404）。要 gin 的重定向就在 `WithRoutes` 里写 `e.RedirectTrailingSlash = true`，
  但重定向的请求不经过任何中间件，不进访问日志、指标和链路。见[「行为与实测」](#行为与实测)。
- **`*gin.Context` 可以直接当 ctx 传**：`xlog.AddKV(c, ...)`、`db.WithContext(c)`、`otel.Tracer(...).Start(c, ...)` 和传
  `c.Request.Context()` 一样带着日志作用域、父 Span 和取消（engine 开了 `ContextWithFallback`）。

## 在负载均衡 / Cloudflare 后面

判断可不可信只看**直连的那一跳**（TCP 对端），不看 `X-Forwarded-For`。K8s 里常见的链路是：

```
用户 ──▶ Cloudflare ──▶ 云负载均衡 ──▶ Ingress ──▶ 服务 A ──▶ 服务 B
                          ↑ 只放行 Cloudflare 的网段（安全组）
```

- **服务里什么都不用配**：A 的对端是 Ingress、B 的对端是 A，都在私有网段里，默认可信；`CF-*` 这类头写进
  `XTrace.ForwardHeaders` 就一路透传下去。
- **Cloudflare 的网段只配在集群入口**（负载均衡的安全组 / Ingress 白名单），不进服务的配置。入口不锁死的话，
  绕过 Cloudflare 直连的人能伪造 `CF-Connecting-IP`，而它经过 Ingress 之后就成了「可信对端发来的」。
- 这样 `client_ip` 是 Cloudflare 边缘节点的地址（不在私有网段里，gin 从 `X-Forwarded-For` 往回找时停在那一跳）。
  真实用户看透传下去的 `CF-Connecting-IP`，或者让 Ingress 用它作真实 IP（ingress-nginx 有现成的配置）。
- 源站直接以公网 IP 接 Cloudflare 的回源、中间没有负载均衡时，才把 Cloudflare 的网段（以
  <https://www.cloudflare.com/ips/> 为准）连同 `private` 一起写进 `TrustedProxies`。
- 不要在 `WithRoutes` 里设 `e.TrustedPlatform = gin.PlatformCloudflare`：gin 会无条件相信请求里的
  `CF-Connecting-IP`，不看是谁发来的。

## 可观测

### 访问日志

每个请求结束时一条，消息 `request completed`，级别 INFO（`XGin.Log` 开着时；`LogSkipPaths` 里的路径和指标端点不记）：

| 字段 | 内容 |
|---|---|
| `method` | 请求方法，原样 |
| `route` | 路由模板，如 `/users/:id`；没匹配上路由时是 `unmatched`（和指标、Span 一致），真实路径看 `path` |
| `path` | 请求路径，**不带查询串** |
| `status` | 状态码；中止的、客户端走了还什么都没发的记 `499`，见[下文](#499中止的请求) |
| `elapsed_ms` | 耗时，毫秒，保留到微秒（如 `0.051`） |
| `client_ip` | 客户端地址：直连对端，或 `XGin.TrustedProxies` 里的代理转发来的 `X-Forwarded-For` |
| `host` | 请求的 `Host`（HTTP/2 是 `:authority`），原样 |
| `proto` | `HTTP/1.1`、`HTTP/2.0` |
| `user_agent` | `User-Agent`，没有就是空串 |
| `bytes_in` | 请求头里的 `Content-Length`；分块上传没有这个头，记 `-1` |
| `bytes_out` | 写出的响应体字节数，不含响应头；没写响应体是 `0`；配了 gin-contrib/gzip 时是压缩后（线上）的字节数 |
| `query` | `LogQuery: true` 且有查询串时，逐字段脱敏，规则同表单 body |
| `request_headers` | `LogRequestHeaders: true` 时，凭证类已脱敏 |
| `request_body` | `LogRequestBody: true` 时，最多前 256KB，逐字段脱敏；multipart 和 `application/octet-stream` 只记一句 `omitted` |
| `response_headers` | `LogResponseHeaders: true` 时，脱敏规则同请求头（`Set-Cookie` 等遮掉） |
| `response_body` | `LogResponseBody: true` 且是文本类响应时，最多前 4KB，逐字段脱敏；响应带着 `Content-Encoding`（压缩中间件压过的）时只记一句 `[gzip-encoded content omitted]` |
| `errors` | handler 里 `c.Error(...)` 登记的错误，没有就不写；多条用 `; ` 隔开（`Error #01: a; Error #02: b`）；出现敏感词就整段记成 `***REDACTED***`，否则只把 `postgres://app:pw@db`、`app:pw@tcp(db:3306)` 里的密码换成 `***REDACTED***`（Span 的 `gin.errors` 同理） |
| `trace_id` / `span_id` | 有链路时 |

**脱敏**按敏感词匹配，不是按字段名精确匹配：比较前双方都转小写、去掉 `_ - .` 和空格，字段名里**含**任一敏感词就遮。

| | 规则 |
|---|---|
| 默认敏感词 | `password` `passwd` `secret` `token` `authorization` `apikey` `accesskey` `privatekey` `credential` `cookie` `session` `signature` |
| body（JSON / 表单） | 键含敏感词就遮这个值，任意嵌套层级都算；JSON 的字符串值再按 `errors` 字段的规矩过一遍（含敏感词整个遮，DSN 只遮密码）；表单的其余值只遮 URL / DSN 里的密码（`https://u:pw@host`）；其余的值原样写回 |
| 其它 body（纯文本、XML、没带 Content-Type…） | 本身是合法 JSON 对象 / 数组 / 字符串的按 JSON 遮（`ShouldBindJSON` 不看 Content-Type）；别的定位不了字段，出现敏感词就整个遮掉，没有敏感词的只遮 URL / DSN 里的密码 |
| 请求头 / 响应头 | 名字在名单里（`Authorization` `Proxy-Authorization` `Cookie` `Set-Cookie` `X-Api-Key` `X-Auth-Token`，加上 `AddSensitiveHeaders` 追加的）**或者**名字含敏感词就遮；其余的值只遮 `postgres://app:pw@db`、`app:pw@tcp(db:3306)` 里的密码，不按敏感词整段遮：`Vary: Cookie`、`Access-Control-Allow-Headers: Authorization` 原样记 |
| 值是 URL 的头 | `Referer`、`X-Original-URL`、`X-Original-URI`、`X-Rewrite-URL`、`X-Forwarded-URI`，响应头 `Location`、`Content-Location`、`Refresh`：去掉 `?` 和 `#` 之后的部分，再去掉 `://` 后面的 userinfo（`https://tok@host` 记成 `https://host`） |

**结构上的局限**：JSON 只认键名。`[{"name":"password","value":"x"}]` 这种名值对数组，密码在 `value` 里、键名里没有敏感词，
`x` 原样进日志（`name` 的值 `password` 本身会被遮掉）。这种形状的 body 要么用 `AddSensitiveFields("value")` 把键加进词表，要么别记 body。

`middleware.AddSensitiveFields(...)` 追加敏感词（body 和请求头一起生效），`middleware.AddSensitiveHeaders(...)`
追加精确的头名。大小写按 Unicode 折叠比较，与 `encoding/json` 匹配字段名一致。词表故意不收 `auth`、`key`、`pwd`：
会误中 `author`、`Idempotency-Key`、随机串。

panic 由 Recover 中间件记一条 `panic while handling request`（ERROR，带 `error`、`stack`、`path`、`method`；`error` 按 `errors` 字段的规矩脱敏，`stack` 只有函数和行号、原样记）并回 500；
客户端提前断开导致的写失败记 `connection broken`（WARN：不是服务的故障，不该触发告警），不打栈。响应已经开始写了（写出了状态码）再 panic 的，不再改状态码：
访问日志、指标、链路记的是已经发出去的那个（通常 200），`errors` 为空、Span 不标错，那条 panic 日志是唯一的迹象。

### 日志

| 消息 | 级别 | 字段 |
|---|---|---|
| `xgin listening` | INFO | `addr`、`tls`、`mtls`、`h2c`；证书读好、端口绑上之后才打 |
| `xgin http server error` | WARN | `error`：net/http 自己报的那一行（`http.Server.ErrorLog`），如 `http: TLS handshake error from 203.0.113.9:1234: EOF` |
| `xgin received the shutdown signal before starting, the server will not start` | WARN | 退出信号早于监听到达 |
| `xgin invalid config, assembling the engine with defaults; Start will return the error` | WARN | `error`；配置不合法时 `Engine()` 按默认值装配，`Start` 返回那个错误 |

日志的全局约定（`trace_id` 注入、`xlog.AddKV`、框架的启停日志）见 [`docs/observability.md`](../docs/observability.md#日志)。

### 指标

| 指标 | 类型 | 标签 | 来源 |
|---|---|---|---|
| `http_requests_total` | counter | `method`、`route`、`status` | xgin，`XGin.Metric` |
| `http_request_duration_seconds` | histogram | `method`、`route`、`status` | xgin，桶是 `XMetric.HTTPDurationBuckets` |

- **`route`** 是路由模板；没匹配上任何路由时是 `unmatched`，不是真实路径。

指标名的前缀、常量标签和几条通用规则见 [`docs/observability.md`「指标」](../docs/observability.md#指标)。

### 链路

| 来源 | Span 名 | 关键属性 |
|---|---|---|
| xgin（入站） | `GET /users/:id`：方法 + 路由模板；没匹配上是 `GET unmatched` | `http.request.method`（收敛过的）、`http.request.method_original`（原始值和收敛值不同时）、`http.route`、`url.path`、`http.response.status_code`、`gin.errors` |

5xx 和中止标成错误，4xx 不算。`Metric` 开着时抓 `MetricPath` 的请求不开 Span、不回带 `X-Trace-Id`：抓取系统按秒轮询它，理由同访问日志跳过它。

链路的全貌、传播与信任边界见 [`docs/observability.md`「链路」](../docs/observability.md#链路)。

### X-Trace-Id 响应头

xgin 的 `Trace` 开着时，每个响应带 `X-Trace-Id: <32 位 trace id>`，从一次调用直接跳到链路。
`XTrace.Enable: false` 时 Span 是 noop，没有 trace id 可回带；`XGin.Trace: false` 时也不回带。

### 499：中止的请求

两种请求在访问日志、指标、链路里的状态码记成 **499**（借 nginx 的 client closed request）。这个码不会发给客户端：

- handler 以 `panic(http.ErrAbortHandler)` 中止的（`httputil.ReverseProxy` 转发到一半上游断开时也是这样）：连接直接断了，
  响应往往已经写出了 200 的响应头，照读 `c.Writer.Status()` 的话被截断的响应在三处都记成成功。Span 标为错误，访问日志的 `errors` 里带着 `net/http: abort Handler`。
- 客户端已经走了（断开连接、HTTP/2 的流被重置，或者停止时到点强制断连——请求的 ctx 被 net/http 取消），而响应一个字节都还没写：
  否则会记成 `bytes_out` 为 0 的 200，而客户端什么都没收到。Span **不**标错：那不是服务端的错。

响应已经开始写了的照记已经写出去的状态码：客户端至少收到了一部分。「客户端走了」只看中间件一进来时请求的 ctx——
里面几层换上的、业务自己的 ctx 到了截止时间或者被 cancel 掉，不算（客户端还在等）。

## 行为与实测

实测环境和跨模块的总表见 [`docs/behavior.md`](../docs/behavior.md)。

gin v1.12.0、Go 1.25 net/http。

**`TrustedProxies`**：gin 默认 `0.0.0.0/0`，任何人发 `X-Forwarded-For: 1.2.3.4` 就能决定 `client_ip`。
这里默认只信私有网段：直接暴露在公网的服务，对端是公网地址，`X-Forwarded-For` 照样改不了 `client_ip`。
在代理后面怎么配见[「在负载均衡 / Cloudflare 后面」](#在负载均衡--cloudflare-后面)。
IPv4 映射成 IPv6 的写法启动失败：实测 gin v1.12.0 把 `::ffff:10.0.0.1` 解歪了，信的是 `::1`、`::5` 这类地址，`10.0.0.1` 本身反而不信；
`::ffff:10.0.0.0/104` gin 解对了，透传 Header 的可信判断却对不上。所以只收 `10.0.0.1`、`10.0.0.0/8` 这样的 IPv4 写法。

**`MaxMultipartMemory`** 不是请求体上限，是「超过多少才落盘」，超出的部分写进临时文件、不会被拒绝。
实际代价约是这个数的三倍：一次 60MB 的上传，配 32MB（gin 默认）时解析这一步让堆多占 96MB，8MB 是 24MB，1MB 是 3MB。

**`ReadHeaderTimeout` / `IdleTimeout` 写 0** 在 net/http 里退到 `ReadTimeout`（默认 0），结果是不限时：
实测发半个请求头的连接一直不被断开。所以两者都必须 > 0。

**`UseH2C`** 用标准库的 `Protocols.SetUnencryptedHTTP2`，不用 x/net 的 `h2c.NewHandler`：后者把连接劫持走，
`Shutdown` 约 60µs 就返回 nil，在途请求照跑。只认先验知识的 h2c（gRPC、`curl --http2-prior-knowledge`），
`Upgrade: h2c` 握手拿到的是普通 HTTP/1.1 响应。交给 net/http 的是 engine 本身、不是 `engine.Handler()`：
在 `WithRoutes` 里设 gin 自己的 `e.UseH2C = true` 不起作用（它会包一层 `h2c.NewHandler`，问题同上），h2c 只看 `XGin.UseH2C`。

**`RedirectTrailingSlash`** gin 默认开着：只注册了 `/users` 时 `GET /users/` 直接回 301（`Location: /users`），POST 回 307，
整条中间件链都不跑——不进访问日志、指标和链路。这里关掉，`/users/` 和别的对不上的路径一样走 `NoRoute`，记 404 `unmatched`。
`RedirectFixedPath`（大小写、`..` 修正后重定向）gin 默认就是关的，没动。要重定向就在 `WithRoutes` 里设回 `e.RedirectTrailingSlash = true`。

**`ContextWithFallback`** gin 默认关着：`*gin.Context` 当 `context.Context` 传时 `Value` 只查 `c.Keys`、`Done()` 返回 nil、`Err()` 永远是 nil，
`xlog.AddKV(c, ...)` 写不进访问日志、`Start(c, ...)` 开的 Span 没有父 Span，客户端走了也取消不到下游。这里开着，这几样都转到 `c.Request.Context()`。

**`MetricPath`** gin 不拒绝不以 `/` 开头的写法，而是悄悄改写：`metrics` 注册成 `/metrics`，访问日志却跳不过它；
留空挂在根路径 `/` 上，业务再注册首页时 gin 在业务自己的路由代码里 panic。所以读配置时就失败。

**`Mode`** 在 `gin.New` 之前设：debug 模式下 `gin.New` 会打一段警告，之后每条路由再各打一行。`gin.SetMode` 是进程级的。

**TLS 与双向认证**（e2e，`TLS.MinVersion: "1.3"`）：带着 `TLS.ClientCAFile` 的 CA 签的证书是 `200`、`HTTP/2.0`、`TLS 1.3`；
不带证书，客户端报 `remote error: tls: certificate required`；拿别的 CA 签的证书，Go 的客户端根本不出示它，结果同上；
最高只到 TLS 1.2 的客户端报 `protocol version not supported`；明文 HTTP 打到这个端口，net/http 回
`400 Client sent an HTTP request to an HTTPS server.`。这几种一次都没进 handler。
`TLS.MinVersion` 默认 1.2，Go 1.25 服务端自己的默认也是 1.2，照样显式写上：默认值会随 Go 版本变。

**优雅退出**：

- `http.Server.Shutdown` 超时只返回错误，在途连接照跑。所以 `Shutdown` 只用到截止时间前的一截（留出剩余时间的 20%，
  最多 1s），到那时还有请求就 `Close()` 断开所有连接。
- 断开连接不等于 handler 返回了：`Close()` 只关连接、取消请求的 ctx。留出来的那一截用来等 handler 真正返回，
  到截止时间还有没返回的，错误里写明几个（`N handler(s) still running when the shutdown deadline passed`）。
- 单独调 `Stop`、传不带截止时间的 ctx 时一直等到在途请求全部做完（实测 1.5s 的请求，`Shutdown` 等了 1.57s）。
- 被劫持走的连接（WebSocket）不归 `Shutdown` / `Close()` 管，它的 handler 同样算在「还没返回」里。
  所以 `Shutdown` / `Close()` 返回之后取消所有请求 ctx 的根（`http.Server.BaseContext`）：WebSocket 的读循环、长轮询看
  `c.Request.Context()` 就能退出。实测一个劫持了连接、等着请求 ctx 的 handler，`Stop` 约 10ms 返回 nil。
  不看 ctx、只阻塞在 `conn.Read` 上的 handler 照样停不下来——要自己在 ctx 取消时关掉连接。
- 监听失败（端口被占、证书读不出来）之后可以再调 `Start`：失败的那次不算在跑。

## 排错

| 错误原文 | 原因 | 怎么改 |
|---|---|---|
| `TLS.CertFile and TLS.KeyFile must both be set or both be empty`（XGin） | 服务端证书只配了一半 | 两个都填，或者都留空 |
| `TLS.ClientCAFile requires TLS.CertFile and TLS.KeyFile, mutual TLS runs on top of TLS`（XGin） | 配了双向认证却没配服务端证书 | 补上 `TLS.CertFile` / `TLS.KeyFile` |
| `unknown TLS.MinVersion="1.1", supported: 1.2 / 1.3`（XGin） | 写了不收的版本，或者写成了 `TLS1.3` | 写 `"1.2"` 或 `"1.3"` |
| `read TLS.ClientCAFile: ...` / `TLS.ClientCAFile ... contains no PEM certificate`（XGin） | CA 文件读不出来，或者里面没有 PEM 证书 | 检查路径和文件内容；服务不监听 |
| `field CertFile not found in type xgin.Config (did you mean TLS.CertFile? move it under TLS:)`（`KeyFile`、`ClientCAFile`、`MinVersion` 同理） | 写在了 `XGin` 顶层，这几项属于 `TLS:` 块 | 这几行缩进进 `TLS:` 块，见[「配置」](#配置) |
| `TrustedProxies entry "::ffff:10.0.0.1" is an IPv4-mapped IPv6 address; write it as 10.0.0.1`（XGin） | `TrustedProxies` 里写了 IPv4 映射成 IPv6 的地址或网段 | 照报错给的写：`::ffff:10.0.0.1` → `10.0.0.1`，`::ffff:10.0.0.0/104` → `10.0.0.0/8` |

停止时的 `N handler(s) still running when the shutdown deadline passed` 见 [`docs/troubleshooting.md`「Runnable 与退出」](../docs/troubleshooting.md#runnable-与退出)；客户端 TLS 的报错见 [xtls「排错」](../xtls/README.md#排错)。
