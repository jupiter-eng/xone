# xecho

Web 服务：拿到的是原生 `*echo.Echo`（[Echo](https://echo.labstack.com/) v4），`xecho.New()` 就是交给 `xone.Run` 的 Runnable，
监听、优雅退出都是框架的事。和 [xgin](../xgin/README.md) 是同一套东西换了个框架：访问日志的字段、指标名、Span 名、
client_ip 信谁、退出时等不等 handler，两边一字不差（写在两者共用的 `internal/web` 里）。

- 访问日志（凭证逐字段脱敏）、链路、指标（自动挂 `/metrics`）、panic 恢复已经装好
- handler 返回的错误在中间件里就渲染成响应：404、405、500 在访问日志、指标、链路里记的是客户端真正收到的状态码和字节数
- 内置中间件挂在 `e.Pre` 上：你在 `e.Pre` 里拒掉、重定向的请求照样进访问日志、指标、链路，Pre 里的 panic 也兜得住
- `c.RealIP()` 按 `X-Forwarded-For` / `X-Real-IP` 取客户端真实 IP，但只认可信代理发来的：默认只信私有网段（echo 自己默认全信），负载均衡、Ingress、Pod 转发来的照收，公网直连的伪造不了
- 慢连接有防线：`ReadHeaderTimeout` 10s、`IdleTimeout` 60s，写 0 启动失败（echo 自己的 `e.Server` 全是 0）
- HTTPS、双向认证、h2c 都在配置里开
- 停止时等在途请求做完，到点断连并报出还没返回的 handler 数
- echo 自己的日志（gommon，默认写 stdout）接到 slog

## 快速上手

```yaml
# conf/application.yml
XApp:
  Name: demo.user.api          # 链路里的 service.name
XEcho:
  Port: 8080
  LogSkipPaths: [/healthz]     # 健康检查不记访问日志
```

```go
import (
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/jupiter-eng/xone"
	"github.com/jupiter-eng/xone/xecho"
)

func main() {
	xone.MustRun(xecho.New().WithRoutes(func(e *echo.Echo) {
		e.GET("/healthz", func(c echo.Context) error { return c.NoContent(http.StatusNoContent) })

		v1 := e.Group("/api/v1")
		v1.GET("/users/:id", func(c echo.Context) error {
			ctx := c.Request().Context() // 客户端断开、服务停止时跟着取消，往下游传它
			slog.InfoContext(ctx, "get user", "user_id", c.Param("id")) // 自动带上这次请求的 trace_id
			return c.JSON(http.StatusOK, map[string]string{"id": c.Param("id")})
		})
	}))
}
```

`curl localhost:8080/api/v1/users/1` 的响应头带 `X-Trace-Id`；`curl localhost:8080/metrics` 能看到
`http_requests_total`，`route` 标签是 `/api/v1/users/:id`。

## 配置

```yaml
XEcho:
  Host: "0.0.0.0"
  Port: 8080
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
  TrustedProxies: [private]  # 信任哪些直连对端的 X-Forwarded-For / X-Real-IP 和透传 Header；private = 回环 + 私有网段，[] = 谁都不信
  Debug: false             # echo 的调试开关（实例级）：错误响应带错误原文、c.JSON / c.XML 一律缩进，线上别开
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
```

和 XGin 块相比少了 `Mode`、`MaxMultipartMemory`、`ZHTranslations` 三项（写了启动失败）：echo 没有进程级的运行模式，
`Debug` 是实例级的，同一进程里两个服务可以一个开一个关；multipart 的落盘阈值是写死的（见[「行为与实测」](#行为与实测)）；
validator 不是 echo 内置的，用 [`WithValidator`](#api) 装一个，翻译报错也由那个校验库自己管。

**同一进程里的第二个服务**（比如内部管理端口）不读配置文件，用 `WithConfig` 给一份完整配置，从 `CurrentConfig()` 改起：

```go
c := xecho.CurrentConfig()
c.Host, c.Port = "127.0.0.1", 9090
admin := xecho.New().WithConfig(c).WithRoutes(adminRoutes)
```

- `private` 展开成 `127.0.0.0/8`、`10.0.0.0/8`、`172.16.0.0/12`、`192.168.0.0/16`、`100.64.0.0/10`、`::1/128`、`fc00::/7`。
  列表整体替换默认值：要再加网段就连 `private` 一起写（`[private, 203.0.113.0/24]`），写错的网段启动失败；
  内网里也有不可信的客户端（办公网、VPN 用户能直连服务）时别用 `private`，写确切的那几段，或者 `[]`。
- `TLS.ClientCAFile` 管整个端口：`/metrics` 同样要客户端证书。handler 里用 `c.Request().TLS.PeerCertificates` 看是谁。
- xgin 和 xecho 可以在一个进程里同时用：两块配置各是各的（`XGin` / `XEcho`），端口别撞上。

## API

| 函数 | 说明 |
|---|---|
| `New() *XEcho` | 创建一个服务（Runnable），什么都不读，配置在装配那一刻才取 |
| `(*XEcho).WithRoutes(f ...func(*echo.Echo))` | 注册路由，回调拿到原生 echo；可以调多次，按顺序生效 |
| `(*XEcho).WithMiddleware(m ...echo.MiddlewareFunc)` | 追加自定义中间件（`e.Use`，router 之后），排在所有内置中间件之后，对每个请求都生效（含 `/metrics` 和 404） |
| `(*XEcho).WithRecoverFunc(f func(echo.Context, any) error)` | 自定义 panic 之后的响应；返回的错误交给 `e.HTTPErrorHandler`，默认回 500 |
| `(*XEcho).WithValidator(v echo.Validator)` | 装一个校验器，之后 handler 里可以 `c.Validate(&req)`；不装的话 `c.Validate` 返回 `echo.ErrValidatorNotRegistered` |
| `(*XEcho).WithConfig(c Config)` | 用这份完整配置起服务，不读配置文件里的 XEcho 块 |
| `(*XEcho).Engine() *echo.Echo` | 触发装配并返回原生 echo；之后再 `With...` 不生效 |
| `(*XEcho).Start(ctx)` / `(*XEcho).Stop(ctx)` | 监听 / 优雅停止，通常交给 `xone.Run`，不用自己调 |
| `CurrentConfig() Config` | 配置文件里 XEcho 那一块的拷贝，`Start` 之前任何时候调都行 |
| `DefaultConfig() Config` | 全部默认值；`WithConfig` 从头写时从它开始 |
| `middleware.AddSensitiveFields(...string)` | 追加访问日志的脱敏敏感词（body 和请求头一起生效；和 xgin 共用一张词表） |
| `middleware.AddSensitiveHeaders(...string)` | 追加精确匹配的敏感请求头名 |

## 注意事项

- **不要调 `e.Start` / `e.StartTLS`**：服务由 `Start` 起（`xone.Run` 替你调），超时、TLS、h2c、优雅退出都在那一边；
  `e.Server` 的四个超时全是 0，用不上。
- **handler 返回错误就行**：`return echo.NewHTTPError(http.StatusNotFound, "no such user")` 或者普通的 `error`，
  响应由 `e.HTTPErrorHandler` 写（默认是 echo 的 JSON `{"message":...}`，普通 error 一律 500、不带原文），
  要换格式就在 `WithRoutes` 里设 `e.HTTPErrorHandler`。错误原文进访问日志的 `errors` 和 Span 的 `echo.errors`，出现敏感词（password、token……）就整段遮掉，URL / MySQL DSN 里的密码换成 `***REDACTED***`。
  自定义的错误处理只会被调一次；响应已经写出去了（`c.Response().Committed`）再返回错误时，echo 默认的错误处理什么都不做。
- **请求体没有上限**：框架不替业务定，要限就用 echo 自带的中间件，比如 `e.Use(echomw.BodyLimit("10M"))`
  （`echomw` 即 `github.com/labstack/echo/v4/middleware`）。multipart 的落盘阈值写死 32MB，见[「行为与实测」](#行为与实测)。
- **handler 里的慢操作传 `c.Request().Context()`**：`Stop` 没有单独的超时，等在途请求最多到停止预算的 2/3
  （`xone.WithStopTimeout` 的 2/3，默认 10s），到点断开连接、取消请求的 ctx；不看 ctx 的 handler 停不下来，
  `Stop` 会报 `N handler(s) still running`。
- **`WithRoutes` 回调里的设置盖过配置**（如 `e.IPExtractor`、`e.HTTPErrorHandler`），但透传 Header 的可信判断只看配置里的
  `TrustedProxies`，两边要一起改就改配置。XEcho 块在装配（`Engine()` 或 `Start`）那一刻才读。
- **路由**：末尾斜杠严格匹配（`/a/` 是 404，和 xgin 一样）；只注册了 GET 时 HEAD 是 405（xgin 同样）；
  路径参数在路由末尾时吃得下后面的 `/`（`/users/:id` 匹配 `/users/1/z`，`id` 是 `1/z`；xgin 是 404）。这些是 echo 的行为，框架没改；
  要放宽末尾斜杠用 `e.Pre(echomw.RemoveTrailingSlash())`。
- **中间件的顺序**，自外向内：`LogScope → Trace → Log → Metric → Recover →` 你的 `e.Pre` `→ router →` `WithMiddleware` / 你的 `e.Use` `→ handler`。
  内置的挂在 `e.Pre` 上、排在你的 `e.Pre` 外面：Pre 里拒掉（`return echo.ErrUnauthorized`）、重定向（`RemoveTrailingSlashWithConfig` 的 301）的请求
  照样进访问日志、指标、链路、带 `X-Trace-Id`，Pre 里的 panic 兜住回 500。这时 router 还没跑，`route` 记 `unmatched`。
  要用 `c.Path()`、`c.Param()`、请求 ctx（`trace_id`、`xlog.AddKV`）的中间件挂 `e.Use` 或 `WithMiddleware`：你的 `e.Pre` 看到的是请求原来的 ctx，见[「行为与实测」](#行为与实测)。
  `e.Pre(echomw.MethodOverride())`、`RemoveTrailingSlash`、`Rewrite` 照常生效，记下的是改过之后的方法。
- **超时用 `echomw.ContextTimeout`，别用 `echomw.Timeout`**：后者（echo 已标弃用）在另一个协程里跑 handler、自己往原始的 writer 写 503，
  实测客户端收到 503，访问日志、指标、链路记的却是 200、`bytes_out` 0，`-race` 下还和访问日志中间件读写同一个响应。
  `ContextTimeout` 只给请求套截止时间，handler 看 `c.Request().Context()` 返回，由它换成 503，记的就是 503。
- **校验器要自己装**：echo 不像 gin 那样内置 validator。不装的话 `c.Validate(&req)` 返回的是 `echo.ErrValidatorNotRegistered`
  （普通 error，不是 `*echo.HTTPError`：直接 `return` 它，客户端收到的是 500 `{"message":"Internal Server Error"}`），
  而不是校验结果——忘了装的服务看起来一切正常。选哪个校验库是业务的事，框架只负责把它装到 `e.Validator` 上：

  ```go
  // github.com/go-playground/validator/v10
  type structValidator struct{ v *validator.Validate }

  func (s structValidator) Validate(i any) error { return s.v.Struct(i) }

  xecho.New().WithValidator(structValidator{v: validator.New()}).WithRoutes(register)
  ```

  `Validate` 返回的错误 `c.Validate` 原样交给 handler，由 handler 自己处置（`return echo.NewHTTPError(http.StatusBadRequest, err.Error())`
  才是 400，什么都不做是 500）。`WithRoutes` 回调里设 `e.Validator` 盖得过它。
- **`Debug` 线上别开**：`XEcho.Debug: true` 落到 `e.Debug` 上，错误响应带上错误原文（`{"message":...,"error":"..."}`），
  `c.JSON` / `c.JSONP` / `c.XML` 还一律缩进输出（等于每个请求都带了 `?pretty`，`bytes_out` 跟着变大）。
  默认关，实测的字节数见[「行为与实测」](#行为与实测)。

## 在负载均衡 / Cloudflare 后面

和 xgin 完全一样，见 [xgin「在负载均衡 / Cloudflare 后面」](../xgin/README.md#在负载均衡--cloudflare-后面)。
client_ip 的算法也一样：直连对端可信时先看 `X-Forwarded-For` 再看 `X-Real-IP`，从右往左找第一个不在 `TrustedProxies` 里的地址。
echo 自带的 `ExtractIPFromXFFHeader` 默认信任回环、链路本地和私有网段，规则与这里不同，别在回调里换成它，除非你就是要它的规则。

## 可观测

### 访问日志

每个请求结束时一条，消息 `request completed`，级别 INFO（`XEcho.Log` 开着时；`LogSkipPaths` 里的路径和指标端点不记）。
字段的名字、顺序、取值和脱敏规则与 xgin 相同（同一份实现），完整说明见 [xgin「访问日志」](../xgin/README.md#访问日志)：

| 字段 | 内容 |
|---|---|
| `method` | 请求方法，原样 |
| `route` | 路由模板，如 `/users/:id`；没匹配上路由（404）、方法不对（405）、echo 自动应答的 OPTIONS、落到 `RouteNotFound` 兜底上的（含分组悄悄注册的，见[「行为与实测」](#行为与实测)）、在 `e.Pre` 里就结束了的都是 `unmatched`，真实路径看 `path` |
| `path` | 请求路径，**不带查询串** |
| `status` | 客户端收到的状态码（handler 返回的错误渲染之后的那个）；中止的、客户端走了还什么都没发的记 `499`，见[下文](#499中止的请求) |
| `elapsed_ms` | 耗时，毫秒，保留到微秒 |
| `client_ip` | `c.RealIP()`：直连对端，或 `XEcho.TrustedProxies` 里的代理转发来的地址 |
| `host` / `proto` / `user_agent` | 请求的 `Host`、`HTTP/1.1` / `HTTP/2.0`、`User-Agent` |
| `bytes_in` | 请求头里的 `Content-Length`；分块上传记 `-1` |
| `bytes_out` | 写出的响应体字节数（`c.Response().Size`），不含响应头；错误响应也算。挂了 `echomw.Gzip` 时是**压缩前**的字节数（xgin 配 gin-contrib/gzip 是压缩后的），见[「行为与实测」](#行为与实测) |
| `query` / `request_headers` / `request_body` / `response_headers` / `response_body` | 各自的开关打开时，脱敏规则同 xgin；响应带着 `Content-Encoding`（`echomw.Gzip` 压缩过）时 `response_body` 只记一句 `[gzip-encoded content omitted]` |
| `errors` | handler 返回的错误（`err.Error()`，如 `code=404, message=Not Found`）、中止或断连的原因；没有就不写；出现敏感词就整段记成 `***REDACTED***`，否则只把 `postgres://app:pw@db`、`app:pw@tcp(db:3306)` 里的密码换成 `***REDACTED***`（Span 的 `echo.errors` 同理） |
| `trace_id` / `span_id` | 有链路时 |

`middleware.AddSensitiveFields(...)` / `AddSensitiveHeaders(...)` 和 xgin 的同名函数写的是同一张表，用哪个都行。

panic 由 Recover 中间件记一条 `panic while handling request`（ERROR，带 `error`、`stack`、`path`、`method`；`error` 按 `errors` 字段的规矩脱敏，`stack` 只有函数和行号、原样记）并回 500，
`e.Pre` 里的 panic 也一样。响应已经开始写了（写出了状态码）再 panic 的，不再改状态码：访问日志、指标、链路记的是已经发出去的那个（通常 200），
`errors` 为空、Span 不标错，那条 panic 日志是唯一的迹象（xgin 相同）。

客户端提前断开之后的写失败，echo 是**返回**错误而不是 panic：访问日志照常一条，`status` 是已经发出去的那个（通常 200），
`errors` 是 `write tcp …: write: broken pipe`（或 `connection reset by peer`），不打栈；错误响应已经写不出去，echo 的错误处理见 `Committed` 什么都不做。
只有 handler 自己把这样的错误 panic 出来时才记 `connection broken`（WARN，不打栈，规则同 xgin）。

### 日志

| 消息 | 级别 | 字段 |
|---|---|---|
| `xecho listening` | INFO | `addr`、`tls`、`mtls`、`h2c`；证书读好、端口绑上之后才打 |
| `echo internal log` | echo 的级别（默认只有 ERROR） | `message`：echo 自己写的那一行，如错误响应写失败时的 `write: broken pipe` |
| `xecho http server error` | WARN | `error`：net/http 自己报的那一行，如 `http: TLS handshake error from 203.0.113.9:1234: EOF`；`e.StdLogger` 写的也在这里 |
| `xecho received the shutdown signal before starting, the server will not start` | WARN | 退出信号早于监听到达 |
| `xecho invalid config, assembling the engine with defaults; Start will return the error` | WARN | `error`；配置不合法时 `Engine()` 按默认值装配，`Start` 返回那个错误 |

日志的全局约定见 [`docs/observability.md`](../docs/observability.md#日志)。

### 指标

和 xgin 同名、同标签、同桶，看板和告警两边通用：

| 指标 | 类型 | 标签 | 来源 |
|---|---|---|---|
| `http_requests_total` | counter | `method`、`route`、`status` | xecho，`XEcho.Metric` |
| `http_request_duration_seconds` | histogram | `method`、`route`、`status` | xecho，桶是 `XMetric.HTTPDurationBuckets` |

- **`route`** 是路由模板；没匹配上的是 `unmatched`，规则同访问日志。`method` 收敛到标准方法，别的记 `OTHER`。
- 同一个进程里 xgin 和 xecho 都用时，两边的请求记在同一组指标上，分不出是哪个服务的；要分开就给它们不同的路由前缀。

### 链路

| 来源 | Span 名 | 关键属性 |
|---|---|---|
| xecho（入站） | `GET /users/:id`：方法 + 路由模板；没匹配上是 `GET unmatched` | `http.request.method`（收敛过的）、`http.request.method_original`（原始值和收敛值不同时）、`http.route`、`url.path`、`http.response.status_code`、`echo.errors`（handler 返回的错误） |

5xx 和中止标成错误，4xx 不算。`XEcho.Trace` 开着时每个响应（包括 echo 渲染的错误响应）带 `X-Trace-Id`。
`Metric` 开着时抓 `MetricPath` 的请求不开 Span、不回带 `X-Trace-Id`，同 xgin。
链路的全貌、传播与信任边界见 [`docs/observability.md`「链路」](../docs/observability.md#链路)。

### 499：中止的请求

两种请求在访问日志、指标、链路里记 499，规则同 [xgin「499：中止的请求」](../xgin/README.md#499中止的请求)：

- handler `panic(http.ErrAbortHandler)` 中止的（Span 标错）。**返回**一个包着它的错误（`fmt.Errorf("proxy: %w", http.ErrAbortHandler)`）不算：
  连接没断，客户端收到的是 `HTTPErrorHandler` 渲染的 500，记 500；
- 客户端已经走了、响应还一个字节都没发的（Span 不标错）：handler 照惯例 `return ctx.Err()`，否则会被渲染成一个谁也收不到的 500。
  echo 往断开的连接上写错误响应照样把 `Committed` 置上、`Status` 记成 500（实测），所以在交给 `HTTPErrorHandler` 之前就判。

## 行为与实测

实测环境和跨模块的总表见 [`docs/behavior.md`](../docs/behavior.md)。echo v4.16.0、gommon v0.5.0、Go 1.25 net/http。

**client_ip（`e.IPExtractor`）**：echo 的默认是 `nil`，`c.RealIP()` 退回老的写法——谁发来的 `X-Forwarded-For`、`X-Real-IP` 都信。
实测公网对端 `203.0.113.9` 发 `X-Forwarded-For: 1.2.3.4`，`c.RealIP()` 是 `1.2.3.4`；只发 `X-Real-IP: 5.6.7.8` 就是 `5.6.7.8`。
这里按 `TrustedProxies` 装上 `IPExtractor`（默认只信私有网段），算法照抄 gin v1.12.0 的 `ClientIP`，xgin 的测试逐条比对两边的结果。
IPv4 映射成 IPv6 的写法（`::ffff:10.0.0.1`、`::ffff:10.0.0.0/104`）启动失败，理由同 [xgin](../xgin/README.md#行为与实测)：写成 `10.0.0.1`、`10.0.0.0/8`。

**handler 返回的错误**在整条中间件链返回之后才由 `e.HTTPErrorHandler` 写成响应（`Echo.ServeHTTP` 的最后一步）。
包在外面的中间件拿到错误的那一刻，404、405、500 实测都是 `Status 200`、`Committed false`、`Size 0`——照读的话每个出错的请求
在访问日志、指标、链路里都是一次 `bytes_out` 为 0 的 200。所以内置的三个中间件在自己这一层就调 `c.Error(err)` 渲染、
再往外返回 `nil`：404 是 `{"message":"Not Found"}` 24 字节，405 是 `{"message":"Method Not Allowed"}` 33 字节、带
`Allow: OPTIONS, GET`，普通 error 是 500 `{"message":"Internal Server Error"}` 36 字节（错误原文不进响应），
`echo.NewHTTPError(418)` 是 `{"message":"I'm a teapot"}` 27 字节，都是 `application/json`，访问日志记的就是这些数。
`HTTPErrorHandler` 只跑这一次；照样返回错误的话 echo 会再调一遍，不查 `Committed` 的自定义错误处理会把错误响应写两遍。
handler 先写了 200 再返回错误时，echo 默认的错误处理见 `Committed` 就什么都不做，客户端收到的是那个 200，日志也记 200。

**`e.Debug`**（`XEcho.Debug`，默认 false）在 echo v4.16.0 的源码里一共被读到三处，两处影响使用者看得到的东西：

- 默认的错误处理（`echo.go` 的 `DefaultHTTPErrorHandler`，只对 `Message` 是字符串的那种）把错误原文放进响应。
  一个普通 `error`，关着是 `{"message":"Internal Server Error"}` 36 字节，开着是 60 字节的

  ```json
  {
    "error": "boom",
    "message": "Internal Server Error"
  }
  ```

- `c.JSON` / `c.JSONP` / `c.XML`（`context.go` 里的 `c.echo.Debug || pretty`）一律按两个空格缩进，
  等于每个请求都带了 `?pretty`：`{"a":"b"}` 从 10 字节变成 15 字节，访问日志的 `bytes_out` 记的就是变大之后的数。

第三处在 `e.Start` / `e.StartTLS` 里：把 gommon 的级别调到 DEBUG。这里不用 `e.Start`，所以这一件由 `applyConfig` 自己做，
否则 `Debug: true` 也只看得到 ERROR，echo 唯一一条低于 ERROR 的自有日志
（`response.go` 的 `response already committed`，WARN）照样写不出来。

**校验器**：`e.Validator` 默认是 `nil`，此时 `c.Validate(&req)` 一眼都不看 `req`，直接返回 `echo.ErrValidatorNotRegistered`
（`errors.New("validator not registered")`，普通 error）：handler 把它返回出去是 500 `{"message":"Internal Server Error"}` 36 字节，
原文只进访问日志的 `errors`。`WithValidator` 装的那个在配置之后、`WithRoutes` 之前落到 `e.Validator` 上，回调里再设盖得过它。

**路由模板**：没匹配上任何路由时 `c.Path()` 是空串；路径对、方法不对时 `c.Path()` 是那条路由的模板、handler 是
`MethodNotAllowedHandler`（405），方法是 OPTIONS 时 echo 自己回 204 和 `Allow`。后两种和 404 一样记 `unmatched`，
和 xgin（gin 在这两种情况下都没有模板）对得上。

**分组的兜底路由**：`g.Use(...)` 和 `e.Group("/api/v1", mw)` 会悄悄给分组注册两条 `RouteNotFound`（`e.Routes()` 里 `Method` 是
`echo.RouteNotFound`，即 `echo_route_not_found`）：`/api/v1` 和 `/api/v1/*`，好让分组中间件对分组下的 404 也生效；不带中间件的
`e.Group` 不注册。于是 `GET /api/v1/nope` 的 `c.Path()` 是 `/api/v1/*`；方法不对也落到兜底上——兜底比 405 优先，
`POST /api/v1/users`（只注册了 GET）回的是 **404** 不是 405，这是 echo 的行为，框架不改。落到兜底上的（包括自己 `e.RouteNotFound` 注册的）
都记 `unmatched`；兜底和真正的路由是同一个模板时（分组里 `g.GET("/*", proxy)`），命中那条路由的方法照记模板 `/api/v1/*`。
兜底的表在第一个请求到来时从 `e.Routes()`（连同 `e.Routers()` 里按 Host 分的）取一次。
`e.Static("/static", dir)` 注册的是一条真正的 GET 路由 `/static*`，文件不存在时由它自己回 404，记的是 `/static*`：基数照样有界。

**`e.Pre`**：`Echo.ServeHTTP` 有 Pre 中间件时把 router 包在 Pre 链的最里面，所以挂在 Pre 上的内置中间件在 `next(c)` 返回时
`c.Path()`、`c.Handler()` 已经有了，405 的 `Allow` 也已经留在 c 上。router 找路由用的是请求刚进来时的那个 `*http.Request`
（闭包里捕获的），不是 `c.Request()`：Pre 里换成 `r.WithContext` 的副本的话，排在后面的 `e.Pre(echomw.MethodOverride())`
改的是副本上的 `Method`，实测 `POST` + `X-HTTP-Method-Override: PUT` 走进了 POST 的 handler。原地改请求也不行：net/http 约定
handler 不改交进来的 `Request`，handler 把请求交给活得比它久的协程（异步记日志）时还是数据竞争。所以内置中间件在 Pre 里
一概不动请求，日志作用域和 Span 只挂在 ctx 上、存进 `echo.Context`：访问日志、指标、Span、panic 日志用的都是它，在 Pre 里就结束了的
请求照样带 `trace_id`；路由之后、`WithMiddleware` 之前再照常 `c.SetRequest(r.WithContext(ctx))` 换到请求上（一个请求复制一次）。
代价是**你的 `e.Pre` 看到的是请求原来的 ctx**：那里打的日志不带 `trace_id`、`xlog.AddKV` 写不进访问日志；在 `e.Pre` 里往请求 ctx
放的值到了 handler 里也看不到（两棵 ctx 没法合并，这里保的是 Span 和日志作用域）。要用 ctx 的中间件挂 `e.Use` / `WithMiddleware`。

**超时中间件**：`echomw.Timeout`（已弃用）配 50ms、handler 睡 200ms：客户端收到 503 和一页 HTML，包在外面的中间件读到的是
`Status 200`、`Size 0`，`-race` 报数据竞争。`echomw.ContextTimeout` 返回 `echo.ErrServiceUnavailable`，由 `HTTPErrorHandler` 渲染成
`{"message":"Service Unavailable"}`，记的是 503；它返回时 `defer cancel()` 了自己套的 ctx，出了那一层请求的 ctx 就是 `Canceled`——
判「客户端走了」用的是内置中间件一进来时的 ctx，不受它影响。

**panic** echo 默认不兜：实测客户端读到 `EOF`（连接被 net/http 断掉），栈由 net/http 写进 stderr
（`http: panic serving 127.0.0.1:…: boom`），不经过 slog；`e.Use(echomw.Recover())` 也兜不住 `e.Pre` 里的 panic（实测同样是 EOF）。
这里的 Recover 挂在 Pre 上，记一条 ERROR 日志并回 500。

**echo 自己的日志**（gommon）默认写 `os.Stdout`，是它自己的 JSON：错误响应写失败（客户端已经断开）时实测留下
`{"time":…,"level":"ERROR","prefix":"echo","file":"echo.go","line":"492","message":"write: broken pipe"}`。
级别默认 ERROR，`Warn("response already committed")` 这类写不出来，`Print` 系列不看级别、总会写。
这里把它接到 slog（消息 `echo internal log`），级别沿用 echo 的。
`e.StdLogger` 在 `echo.New` 里就绑定了 `e.Logger` 当时的输出（`os.Stdout`），换 `e.Logger` 的输出改不到它；它是 echo 给
`http.Server.ErrorLog` 准备的（`e.Start` 里这么用），这里接到和服务的 `ErrorLog` 同一处（`xecho http server error`，WARN）。

**客户端断开之后**：handler 等到请求的 ctx 取消、再 `c.Error(ctx.Err())`，调用前 `Committed false`、`Status 200`，调用后
`Committed true`、`Status 500`、`Size 36`——写失败了照样这么记。往断开的连接上 `c.Response().Write` 写 1MB 的块，
实测写出 3963 字节之后返回 `write tcp …: write: broken pipe`，`Committed true`、`Status 200`。

**压缩**：`echomw.Gzip` 把 `c.Response().Writer` 换成它的 gzip writer，`c.Response().Size` 数的是交给它之前的字节：
一个 12000 字节的 `c.String`，线上是 77 字节，`bytes_out` 记 12000。xgin 配 gin-contrib/gzip v1.2.8 记的是线上的 77（它换的是
`c.Writer`，访问日志读的是它下面那个）。要让 xecho 也记线上的字节数，得在 Gzip 下面每个请求再包一层计数的 writer，框架没这么做。
截响应体的 writer 在 Gzip 外面，截到的是压缩过的字节，`response_body` 只记一句 `[gzip-encoded content omitted]`。

**截响应体的 writer**（`LogResponseBody: true` 时换进 `c.Response().Writer` 的那个）实现了 `http.Flusher`、`http.Hijacker` 和 `Unwrap`：
直接做类型断言的老代码、`c.Response().Flush()`、`http.ResponseController` 的读写超时都落到真正的连接上；
下面的 writer 不支持 Hijack 时返回 `http.ErrNotSupported`，和 net/http 一样。

**服务器**：`e.Server` 的 `ReadHeaderTimeout`、`ReadTimeout`、`WriteTimeout`、`IdleTimeout` 全是 0，banner 只在 `e.Start` 时打。
这里不用 `e.Start`，服务由和 xgin 共用的 `web.Server` 按配置起，超时、TLS、h2c、优雅退出的实测见
[xgin「行为与实测」](../xgin/README.md#行为与实测)：劫持了连接（WebSocket）的 handler 在停止时同样看得到请求 ctx 取消，
监听失败之后同样可以再 `Start`。

**`echo.Context` 不是 `context.Context`**（没有 `Deadline` / `Done` / `Err`），传不进要 ctx 的函数：一律传 `c.Request().Context()`。

**`MetricPath`** echo 不拒绝不以 `/` 开头的写法：`metrics` 注册成 `/metrics`，访问日志却跳不过它；留空注册在根路径 `/` 上，
业务再注册首页时后注册的那个悄悄盖掉前一个（不报错，也不 panic）。所以读配置时就失败。

**请求体**：没有上限，实测 64MB 的 body 照收。`c.FormValue`、`c.FormFile`、`c.Bind` 解析 multipart 时用写死的 32MB 落盘阈值
（`ParseMultipartForm(32 << 20)`，没有配置项）：一次 60MB 的上传，解析这一步实测分配 128MB（`TotalAlloc`）；
在中间件里先 `c.Request().ParseMultipartForm(8 << 20)` 的话是 32MB——请求只会被解析一次，先调的那个说了算。
框架不替你做这一步：提前解析会打断用 `c.Request().MultipartReader()` 流式读上传的 handler。

**响应字节数** `c.Response().Size` 数的是经 `c.Response()` 写出去的正文：`c.String(200, "hello")` 是 5；`c.JSON` 带一个换行
（`{"a":"b"}` 记 10）；分三次写、每次 `Flush` 的 `chunk` 是 15；`c.NoContent(204)` 是 0，`Status` 是 204。
绕过 `c.Response()`、直接写 `c.Response().Writer` 的不算在内。`?pretty` 查询参数会让 `c.JSON` 缩进输出（`{"a":"b"}` 变成 15 字节）。

## 排错

| 错误原文 | 原因 | 怎么改 |
|---|---|---|
| `TLS.CertFile and TLS.KeyFile must both be set or both be empty`（XEcho） | 服务端证书只配了一半 | 两个都填，或者都留空 |
| `TLS.ClientCAFile requires TLS.CertFile and TLS.KeyFile, mutual TLS runs on top of TLS`（XEcho） | 配了双向认证却没配服务端证书 | 补上 `TLS.CertFile` / `TLS.KeyFile` |
| `unknown TLS.MinVersion="1.1", supported: 1.2 / 1.3`（XEcho） | 写了不收的版本，或者写成了 `TLS1.3` | 写 `"1.2"` 或 `"1.3"` |
| `read TLS.ClientCAFile: ...` / `TLS.ClientCAFile ... contains no PEM certificate`（XEcho） | CA 文件读不出来，或者里面没有 PEM 证书 | 检查路径和文件内容；服务不监听 |
| `field CertFile not found in type xecho.Config (did you mean TLS.CertFile? move it under TLS:)`（`KeyFile`、`ClientCAFile`、`MinVersion` 同理） | 写在了 `XEcho` 顶层，这几项属于 `TLS:` 块 | 这几行缩进进 `TLS:` 块，见[「配置」](#配置) |
| `TrustedProxies entry "::ffff:10.0.0.1" is an IPv4-mapped IPv6 address; write it as 10.0.0.1`（XEcho） | `TrustedProxies` 里写了 IPv4 映射成 IPv6 的地址或网段 | 照报错给的写：`::ffff:10.0.0.1` → `10.0.0.1`，`::ffff:10.0.0.0/104` → `10.0.0.0/8` |
| 响应是 500 `{"message":"Internal Server Error"}`，访问日志的 `errors` 是 `validator not registered` | handler 调了 `c.Validate`，但没装校验器 | `xecho.New().WithValidator(...)`，见[「注意事项」](#注意事项) |
| `field Mode not found in type xecho.Config`（`MaxMultipartMemory`、`ZHTranslations` 同理） | 照抄了 XGin 块 | 删掉这几项，理由见[「配置」](#配置) |

停止时的 `N handler(s) still running when the shutdown deadline passed` 见 [`docs/troubleshooting.md`「Runnable 与退出」](../docs/troubleshooting.md#runnable-与退出)。
