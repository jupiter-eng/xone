# xhttp

出站 HTTP 客户端：拿到的是原生 `*resty.Client`（resty v2），任何时候都有一个可用的实例，不配也能用。

- 链路已装好：`traceparent` 自动带给下游，Span 名只用方法（`Run` 起来之后的实例；之前、之后的兜底实例没有）
- 出站耗时指标 `http_client_request_duration_seconds`，一次逻辑请求记一次
- 只重试传输层的错，默认只重试幂等方法
- 可选的请求日志（`Log: true`）：一个逻辑请求一行，方法、host、路径、状态码、耗时、尝试次数，不记查询串
- 日志和 Span 里的 URL 去掉查询串；没有 cookie jar
- 要第二套参数（比如调公网、关指标）就用 `xhttp.New` 另建一个

## 快速上手

```yaml
# conf/application.yml（可选，不写就全用默认值）
XHttp:
  Timeout: 1s              # 一次尝试的超时
  RetryCount: 2            # 只重试传输层的错，且只重试幂等方法
```

```go
// Now 是下游返回的 JSON 对应的结构体
func Current(ctx context.Context, city string) (*Now, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second) // 给整次请求封顶：重试和退避都算在里面
	defer cancel()

	var out Now
	resp, err := xhttp.R(ctx).
		SetQueryParam("city", city).
		SetResult(&out). // 2xx 时把 JSON 解进 out
		Get("https://weather.internal/api/v1/now")
	if err != nil {
		return nil, err
	}
	if resp.IsError() { // 4xx / 5xx 不是 err，要自己判断
		return nil, fmt.Errorf("weather api returned %s", resp.Status())
	}
	return &out, nil
}
```

## 配置

不配也能用：`xhttp.R(ctx)` 任何时候都有一个可用的客户端。`Run` 之前和之后拿到的是兜底实例：只有 30s 超时，
没有下面这些配置，也没有重试、链路和指标。只有单实例。

```yaml
XHttp:
  Timeout: 60s               # 一次尝试的超时，不是整次逻辑请求；0 = 永不超时
  DialTimeout: 30s
  DialKeepAlive: 30s         # 负数 = 不发 keep-alive 探测
  MaxIdleConns: 100
  MaxIdleConnsPerHost: 10    # 标准库默认只有 2
  MaxConnsPerHost: 0         # 每 host 连接数上限，0 = 不限
  IdleConnTimeout: 90s       # 要小于下游的 keep-alive 超时
  RetryCount: 0              # 默认不重试
  RetryWaitTime: 100ms       # 重试的起始等待
  RetryMaxWaitTime: 2s       # 重试等待的上限
  RetryOnlyIdempotent: true  # 只重试幂等方法
  Trace: true                # 出站 Span；关掉后 traceparent、baggage、透传 Header 照样带给下游
  Metric: true               # 出站耗时指标
  Log: false                 # 每个逻辑请求一行日志（重试完记一次）；不记查询串、Header、body
  SlowThreshold: 1s          # 整次请求超过它记 WARN；需 Log 开启，0 = 不记
  TLS:                       # 规则见 xtls；管这个客户端发出的每一个 https 请求，http:// 不受影响
    Enable: false
    CAFile: ""               # 填了就只认它：公网的 https 下游从此校验不过
    CertFile: ""
    KeyFile: ""
    ServerName: ""           # 填了就拿它比对每一个下游的证书
```

TLS 块适合「只调一类内部下游」的客户端；要同时调公网的，另用 `xhttp.New` 建一个。

## API

| 函数 | 说明 |
|---|---|
| `R(ctx) *resty.Request` | 开一个绑定了 ctx 的请求，链路和超时才传得到下游。常用的就是它 |
| `C() *resty.Client` | 取 resty client。不会 panic：初始化前、关闭后拿到的是带 30s 超时的兜底实例 |
| `RawClient() *http.Client` | 底层的原生 `*http.Client`，给 SSE 这类要自己读响应体的流式请求 |
| `New(cfg) (*resty.Client, io.Closer, error)` | 纯构造器：不碰本包的全局实例；`Metric` 开着时直方图注册到 xmetric 的全局 Registry |

## 注意事项

- **`Timeout` 管一次尝试，不是整次请求**：`Timeout: 300ms` 配 `RetryCount: 3` 实测跑满 1.24s。要封顶就像上面那样用 ctx。
  见[「行为与实测」](#行为与实测)。
- **只重试传输层的错**（建连失败、超时、连接被重置），拿到了响应就不重试，5xx 也不重试；`RetryOnlyIdempotent` 默认开着，POST 不重发，
  自己 `AddRetryCondition` 挂的条件也放不回来——按状态码重试的条件除外，见[「行为与实测」](#行为与实测)。
- **body 是 `io.Reader` 的请求不重试**：第一次尝试就把它读完了，重发的是空 body。要重试就传 `[]byte` / `string`。
- **设了 `HTTP_PROXY` / `HTTPS_PROXY` 就全部出站都走代理**；跨 host 重定向时自定义的凭证头（如 `X-Api-Key`）照样带给新 host。
  见[「行为与实测」](#行为与实测)的表。
- **TLS 块管这个客户端的每一个 https 请求**：填了 `CAFile` 公网的 https 下游就校验不过了。
- **指标的 `host` 标签是 URL 的 `host[:port]` 原样**：目标来自用户输入或直连一批 IP 时基数会失控，那种调用另建一个
  `Metric: false` 的客户端。见[「指标」](#指标)。
- **`SetDebug(true)` 会把整个请求原样写进日志**（查询串、`Authorization` 都在），只在本机排查时开。见[「日志」](#日志)。

## 可观测

### 日志

| 消息 | 级别 | 字段 |
|---|---|---|
| `xhttp ready` | INFO | `timeout`、`max_idle_conns_per_host`、`retries` |
| `http request` / `slow http request` / `http request failed` | INFO / WARN / WARN | 见下表（需 `XHttp.Log: true`） |
| `xhttp resty log` | resty 的原级别 | `detail`：resty 自己的日志原文，URL 去掉查询串、片段和 userinfo（`SetDebug(true)` 的请求转储例外，见下） |
| `xhttp skipped retrying a non-idempotent method` | DEBUG | `method`、`to_allow_set` |
| `xhttp cannot tune the connection pool` | WARN | `default_transport_type`：`http.DefaultTransport` 被换成了别的类型，连接池参数没生效 |

请求日志的字段：

| 字段 | 说明 |
|---|---|
| `method` | 请求方法 |
| `host` | `host[:port]` |
| `path` | 转义过的形式（如 `/a%2Fb`），不带查询串 |
| `status` | 状态码，没拿到响应时 `0` |
| `elapsed_ms` | 整次逻辑请求，含重试和退避；一次都没发出去时 `0` |
| `attempts` | resty 的尝试次数 |
| `threshold_ms` | 慢请求才有 |
| `error` | 有错误时；URL 去掉查询串、片段和 userinfo |

- **失败**是 resty 返回了错误，或者下游回了 5xx；4xx 是下游的业务回答，记 INFO。返回错误的不只是没拿到响应（传输层错误、ctx 到期）：
  200 但 `SetResult` 解不开（`status: 200`、`error: invalid character …`）、`NoRedirectPolicy` 下的 3xx 和 `FlexibleRedirectPolicy`
  用完（`status: 302`、`error: Get "/b": auto redirect is disabled`）也记 `http request failed`。又慢又失败的记失败，不另记慢。
- **一个逻辑请求一行**，所有重试结束后才记。`Log: true` 时 resty 自己在重试路径上的那几行 `xhttp resty log`
  （每次失败的尝试一行 WARN、用完一行 ERROR）不再打，内容就是这一行的 `error`；resty 别的提醒（比如明文 HTTP 上用 Basic Auth）、
  配置调用被忽略时的 ERROR 照旧。自己 `client.SetLogger(…)` 或 `R().SetLogger(…)` 的，resty 的日志照 resty 的规矩全交给你的 logger（含重试路径那两种）。
- **没有日志行的**：发出去之前就被 resty 拒掉的请求（比如 GET 带 multipart，报 `multipart content is not allowed in HTTP verb [GET]`），
  resty 只调 `OnInvalid`，不调 `OnSuccess` / `OnError`。
- **`SetDoNotParseResponse(true)` 时 `elapsed_ms` 是到响应头的时间**：body 交给你自己读，resty 拿到响应头就算完。
  实测下游先回头、150ms 后才写完 body：这样记 `0.25`，照常读 body 记 `151`。
- 查询串、片段、userinfo、Header、body 一律不记。日志用调用方的 ctx，`trace_id` / `span_id` 是调用方的（出站 Span 这时已经结束）。
- **警告：`SetDebug(true)` 会把整个请求原样写进日志**。resty 把请求行（带查询串）、全部 Header（`Authorization` 也在）、body 和响应拼成一段文本，
  经 `xhttp resty log`（DEBUG）写出去；请求行里的路径不带 `http://` 也不在引号里，xhttp 认不出来，实测 `GET  /ok?token=…`、
  `Authorization: Bearer …` 原样出现。只在本机排查时开，不要在线上开。

日志的全局约定（`trace_id` 注入、`xlog.AddKV`、框架的启停日志）见 [`docs/observability.md`](../docs/observability.md#日志)。

### 指标

| 指标 | 类型 | 标签 | 来源 |
|---|---|---|---|
| `http_client_request_duration_seconds` | histogram | `method`、`host`、`status` | xhttp，`XHttp.Metric`；一次逻辑请求记一次（含重试和退避） |

- **`method`** 收敛到 RFC 9110 的方法加 `PATCH`，其余记 `OTHER`。**`status`** 没拿到响应时是 `0`。
- **`host`** 是出站请求 URL 的 `host[:port]`，原样照抄，基数等于你调过的目标数。每个新值乘上 `method` × `status`，
  每个组合 15 条时间序列（默认 12 个桶加 `+Inf`、`_sum`、`_count`）。目标来自用户输入或直连一批 IP 的调用另建一个
  `Metric: false` 的客户端。

指标名的前缀、常量标签和几条通用规则见 [`docs/observability.md`「指标」](../docs/observability.md#指标)。

### 链路

| 来源 | Span 名 | 关键属性 |
|---|---|---|
| xhttp（出站） | 只用方法，如 `GET` | otelhttp 的标准属性；`url.full` **去掉了查询串、片段和 userinfo** |

链路的全貌、传播与信任边界见 [`docs/observability.md`「链路」](../docs/observability.md#链路)。

## 行为与实测

实测环境和跨模块的总表见 [`docs/behavior.md`](../docs/behavior.md)。

resty v2.17.2、otelhttp v0.71.0、Go 1.25。

**`Timeout` 管一次尝试，不是一次逻辑请求。** 开了 `RetryCount` 之后最坏是 `(RetryCount+1) × Timeout` 加上几次退避：
`Timeout: 300ms` 配 `RetryCount: 3`，实测跑了 1.24s。要给整次逻辑请求封顶，用调用方的 ctx（`xhttp.R(ctx)`），
每次尝试和中间的退避都听它的。

**重试什么**：和 resty 自己的默认一致，只重试传输层的错（建连失败、超时、连接被重置、响应体没收全），
拿到了响应就不重试——5xx 也不重试。挂上重试条件会让 resty 自己的判断整个作废，所以这条是 xhttp 自己守着的：
不守的话实测 200 + 坏 JSON、`RetryCount: 3` 时同一个 GET 发了 4 次。`RetryOnlyIdempotent` 默认开着：
传输层超时分不出「请求没到服务端」和「处理完了但响应丢了」，重发一个 POST 就可能重复下单。

**使用者自己挂的重试条件。** resty v2.17.2 的条件是「或」：请求级的排在前面、client 级的按挂上的顺序，
一个返回 true 就不看后面的（`retry.go` `Backoff`），xhttp 的「POST 不重试」只是其中一个 false。所以另有一道关挂在
`RetryAfter` 上（决定重试之后、等待之前调，返回错误就不再重试）：没拿到响应的 POST / PATCH 不重试，调用方拿到的仍是
第一次的传输层错误（resty 只在这一次没有错误时才把 `RetryAfter` 的错误交出去）。实测 `RetryCount: 2`、
挂一个 `err != nil` 就重试的条件、连接被掐断：不加这道关时 POST 发了 2 次，加了之后 1 次。

- **挡不住的一种**：拿到了响应之后按状态码重试的条件（比如 `StatusCode() >= 500`）。那时没有原来的错误可交，
  否决就得把那个 503 换成一个编出来的错误，所以不否决：POST 照你的条件重试（实测发 3 次，最后拿到 503、错误为 nil），
  方法要你自己在条件里判断。
- 自己 `SetRetryAfter` 会把这道关换掉。`OnRetry` 钩子在这道关之前调，被否决的那次也会调一次。

**body 是 `io.Reader` 的请求不重试。** resty v2.17.2 每次尝试都拿 `Request.Body` 重建请求（`middleware.go`
`createHTTPRequest`），`[]byte`、`string`、结构体每次重新序列化，`io.Reader` 第一次就读到了头、也不替你倒回去
（`RetryResetReaders` 只管 multipart）。实测 `PUT` 一个 `strings.NewReader(…)`、第一次连接被掐断：第二次发出去 0 字节，
服务端回 200，调用方看到的是成功。所以这种请求不重试（`RetryOnlyIdempotent` 关着也一样），调用方拿到第一次的错误；
按状态码重试的条件要重发它时，调用方拿到 `not retrying: the request body is an io.Reader that the first attempt already consumed`
和那个响应。例外：`SetContentLength(true)` 的，resty 把 `io.Reader` 读进缓冲、之后每次发那份缓冲（实测两次都是完整的 body）；
没有 body 的 PUT / DELETE 照旧重试。

**连接池没配的那些是标准库的默认**（从 `http.DefaultTransport` 克隆）：

| 项 | 默认 | 实测的行为 |
|---|---|---|
| 代理 | `ProxyFromEnvironment` | 设了 `HTTP_PROXY` / `HTTPS_PROXY` 就全部出站都走代理，`http://` 的请求连同查询串原样交给代理；回环地址不走，`NO_PROXY` 可以排除。环境变量第一次用到时读一次就缓存，之后再改不生效 |
| TLS 握手超时 | 10s | 对端收下 TCP 连接不回握手，10.0s 报 `TLS handshake timeout` |
| 等响应头 | 不限 | 由 `Timeout` 管住整次尝试；`Timeout` 也配 0 的话会永远挂着 |
| `MaxConnsPerHost` | 0，不限 | 对一个 300ms 才回的下游并发 200 个请求，它收到 200 条新连接；紧接着再来 200 个，只有 `MaxIdleConnsPerHost` 那 10 条复用得上 |
| 重定向 | 标准库：最多跟 10 次 | 第 11 次报 `stopped after 10 redirects`。跨 host 跳转时只去掉 `Authorization`、`Cookie` 这几个，**自定义的凭证头（如 `X-Api-Key`）照样带给新 host**；302 把 POST 变成不带 body 的 GET，307 保留方法和 body |
| HTTP/2 | `ForceAttemptHTTP2: true` | 换了 `TLSClientConfig`（TLS 块）之后照旧协商出 `HTTP/2.0` |

不想跟随重定向：`xhttp.C().SetRedirectPolicy(resty.NoRedirectPolicy())`。

**`IdleConnTimeout` 要小于下游的 keep-alive 超时**：对端先关掉空闲连接时，恰好在那一刻复用它的请求会失败。
实测服务端空闲超时 200ms、请求间隔在 200ms 上下：300 个 POST 失败 27 个（`connection reset by peer`），
GET 由标准库自动在新连接上重发，200 个一个没失败。

**请求日志（`Log: true`）**。实测 resty v2.17.2：`OnSuccess` / `OnError` 在所有重试结束之后只调一次，
`Request.Attempt` 是用掉的尝试次数；拿到响应就走 `OnSuccess`，**5xx 也是**（resty 只在有 error 时调 `OnError`），
所以 5xx 算不算失败是这里判的。`RetryCount: 2`、`RetryWaitTime` / `RetryMaxWaitTime: 5ms` 时量出来的：

| 下游 | 下游收到 | 回调 | 日志 |
|---|---|---|---|
| 200 | 1 次 | `OnSuccess` 1 次 | `http request`，`status: 200`、`attempts: 1` |
| 500 | 1 次（拿到响应不重试） | `OnSuccess` 1 次 | `http request failed`，`status: 500`、`attempts: 1` |
| 每次都掐断连接 | 3 次；池子里有这个下游的空闲连接时 4 次 | `OnError` 1 次，`Attempt` 3 | `http request failed`，`status: 0`、`attempts: 3`、`elapsed_ms` 约 12（含两次退避）、`error: Get "http://127.0.0.1:…/cut": EOF` |
| 端口没人监听 | — | `OnError` 1 次，`Attempt` 3 | `http request failed`，`attempts: 3`、`error: Get "http://127.0.0.1:1/x/y": dial tcp …: connection refused` |

掐断连接那一行收到 4 次的情形：第一次尝试用的是池里复用的空闲连接，标准库在它被对端关掉时自己在新连接上重发了一次 GET
（同上面 `IdleConnTimeout` 那一条），这一次 resty 看不见，所以 `attempts` 仍是 3。

**错误里的 URL。** 错误原文是 `*url.Error`：`Get "http://someone:***@host/x?token=…": …`——整条 URL 连查询串都在，
标准库只把密码换成 `***`、用户名照留。那个 URL 还不一定是绝对的：重定向策略拒绝时放进去的是 `Location` 原样，
实测 `Location: /b?token=…` 时原文是 `Get "/b?token=…": auto redirect is disabled`；查询串里也可能有没转义的空格
（`?q=hello world&token=…`）。所以日志里的 `error` 按结构去：

- 错误链上（`%w` 包的、`errors.Join` 的都算）每个 `*url.Error` 的 URL 解析后去掉查询串、片段和 userinfo 再渲染；
  解析不了的切在第一个 `?` / `#` 上。
- 链外的自由文本再按文本兜一遍（引号里的到右引号为止）。`Location` 转义不合法时它在错误文本里：
  `failed to parse Location header "/b%zz": …`。

resty 自己的日志、出站 Span 的 `url.full` 用的是同一套。返回给调用方的错误不变。

**跟 resty / otelhttp 默认不一样的地方：**

| | 库自己的默认 | 这里 |
|---|---|---|
| resty 的日志 | 写 `os.Stderr`；开了重试后每次失败打一行 `WARN RESTY Get "http://…?token=…": …, Attempt 1`，用完再打一行 ERROR | 接到 slog（`xhttp resty log`，内容在 `detail`），级别照搬，URL 去掉查询串；`Log: true` 时重试路径上的那几行不打，由 `http request failed` 一行代替 |
| cookie jar | `resty.New()` 自带一个，同一 client 的所有请求共享会话 cookie | 没有（用 `NewWithClient`）。初始化前 / 关闭后的兜底实例也没有 |
| 出站 Span 名 | 常见写法是 `GET /users/42`，基数随 id 增长 | 只用方法 `GET`（OTel 语义约定在没有路由模板时的写法） |
| `url.full` | otelhttp 只去掉 `user:password`，查询串原样写进去 | 查询串、片段和 userinfo 一并去掉 |
| `CloseIdleConnections` | otelhttp 的 Transport 没实现，`http.Client.CloseIdleConnections()` 断在它那一层，整条调用变成空操作 | 关闭时 xhttp 直接关它自己持有的连接池 |
