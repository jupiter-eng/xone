# xgorm/clickhouse

给 [xgorm](../README.md) 加 ClickHouse 驱动：匿名 import 一行、配置里写 `Driver: clickhouse`，拿到的仍是原生 `*gorm.DB`。

- 配置项、多实例、日志、指标、Span 都和 xgorm 一样
- 独立 module：ClickHouse 驱动多带进 60 多个模块，不用它的应用不必背（见 [architecture.md](../../docs/architecture.md#2-every-integration-is-its-own-go-module)）
- DSN 认 `clickhouse://` / `tcp://`（native 协议）和 `http://` / `https://`
- TLS 块对 native 和 `https://` 都生效
- 解析错误一律不回显 DSN（原始错误里带着明文密码）

## 快速上手

```yaml
# conf/application.yml
XGorm:
  Clients:
    default: {DSN: "${DB_DSN}"}                     # PostgreSQL
    events:  {Driver: clickhouse, DSN: "${CH_DSN}"} # clickhouse://user:pass@ch:9000/analytics
```

```go
import (
	"github.com/jupiter-eng/xone/xgorm"
	_ "github.com/jupiter-eng/xone/xgorm/clickhouse" // 注册驱动，代码里不直接调它
)

ctx, cancel := context.WithTimeout(ctx, 10*time.Second) // 截止时间要比 read_timeout 短，见「注意事项」
defer cancel()

var out []PageViews
err := xgorm.CWithCtx(ctx, "events").
	Raw("SELECT path, count() AS views FROM page_views WHERE day = today() GROUP BY path ORDER BY views DESC LIMIT 10").
	Scan(&out).Error
```

## 配置

只列 ClickHouse 相关的；其余字段、多实例写法见 [xgorm「配置」](../README.md#配置)。

```yaml
XGorm:
  Driver: clickhouse
  DSN: "${CH_DSN}"         # clickhouse://user:pass@host:9000/db，也认 tcp:// http:// https://
  DialTimeout: 500ms       # 注入 DSN 的 dial_timeout，DSN 里已写的不覆盖；0 不注入，驱动用它自己的 30s
```

- `dial_timeout` 接在 DSN 的 query 末尾，你写的其余参数一个字节都不动（不重排、不重新转义）。
- 库名写在路径里或写成 `?database=` 都行，两处都写时以 query 里的为准（建连日志的 `db`、Span 的 `db.namespace` 也按它记）。
- TLS 规则见 [xtls](../../xtls/README.md)。开着 TLS 块时：`https://` 不必再写 `secure=true`；DSN 里不能再写
  `secure` / `skip_verify` / `tls_server_name`；DSN 是 `http://` 的启动失败。

## API

| 名字 | 说明 |
|---|---|
| `Driver` | 常量 `"clickhouse"`，即配置里 `Driver` 要写的值。包在 `init` 里自己注册，取实例照样用 `xgorm.C` / `xgorm.CWithCtx` |

自己写驱动：在 `xgorm.RegisterDialect` 注册的 `Dialect` 里提供 `OpenTLS` 才收 TLS 块，否则配了 TLS 块就启动失败。

## 注意事项

- **`read_timeout` 没写时是 300s，而且管的是一整段读**：健康的长查询也会被它打断；读超时的查询会被 `database/sql` 重发，一共发 3 次。
  **给查询的截止时间要比 `read_timeout` 短**。见[「行为与实测」](#行为与实测)。
- **新建连接不听 ctx**：拨号和握手只按 `dial_timeout`（`DialTimeout`，默认 500ms）。
- 驱动对写入报的影响行数永远是 0，Span 的 `db.rows_affected` 在 ClickHouse 上没有意义。
- DSN 必须是上面四种 scheme 之一的 URL。
- 驱动名写错或忘了 import 时启动失败：`unknown Driver="clickhouse", registered: [mysql postgres]`，列出的就是 `xgorm.Drivers()`。

## 行为与实测

实测环境和跨模块的总表见 [`docs/behavior.md`](../../docs/behavior.md)。

clickhouse-go v2.48.0（native 协议）、`gorm.io/driver/clickhouse` v0.7.0、ch-go v0.74.0、ClickHouse 24.8.14（e2e）。

**为什么是独立的 module**：只 import xgorm 的应用模块图（`GOWORK=off go list -m all`，不含应用自己的模块）是 68 个，
加上 ClickHouse 驱动变成 131 个（`go list -deps` 里的非标准库包 165 → 208）。多出来的大头是 Docker 和 testcontainers——
clickhouse-go 用它们跑集成测试，而 `go.mod` 分不出「只测试用」。

**`Initialize` 里的版本查询**同 MySQL：写死的 `context.Background()`、发生在建连重试之前。实测对一个收下连接却不回话的
地址，ctx 早已取消也要等满 `dial_timeout`，然后直接失败。这里同样挪进建连探测，版本号照样设进 Dialector
（驱动靠它判断改列名（< 20.4）和列精度（< 21.11））。

**超时与取消：**

- `read_timeout` 没写时是 **300s**（实测对端不回话、不给截止时间：300.6s 后才报错）。
- **`read_timeout` 管的是一整段读，不是「多久没收到字节」。** 一条查询分两段读：读到第一个数据块、再读余下的全部，
  每段开始时设一次 deadline，中途不续期。所以**健康的查询只要余下的结果读得比它久，照样失败**：实测
  50 行 × 100ms 的流配 `read_timeout=1s`，1.0s 报 `i/o timeout`；`SELECT sleep(2)` 同样 1.0s 失败。
  跑得久的查询要么调大 `read_timeout`，要么给调用方的截止时间。
- **调用方的截止时间代替 `read_timeout`**，比它长也照截止时间来（同一条 5s 的流配 `read_timeout=1s`、截止时间 10s，5.0s 读完）；
  池里的连接给 200ms 就在 201ms 返回。
- **新建连接不听 ctx**：拨号和握手只按 `dial_timeout`。池里的连接用完之后，要新建连接的查询照样等满 `dial_timeout`
  （默认 500ms，实测 500.2–501.5ms）。
- **读超时的查询会被 `database/sql` 重发，一共发 3 次。** 驱动把读超时认成坏连接、报 `driver.ErrBadConn`，
  `database/sql` 换一条连接再发（`maxBadConnRetries` = 2）。实测 `read_timeout=1s`：服务端要睡 1.5s 的查询 3.0s 后才失败，
  `system.query_log` 里它开始了 3 次，返回的错误是 `driver: bad connection`（`i/o timeout` 这个根因丢了）。
  clickhouse-go v2.30.0 时相反：读超时的连接还回池里，下一条借到它的查询**成功返回了上一条的结果**（上游 v2.47.0 修掉）。
- ctx **取消**时当场返回 `context canceled`，同时给服务端发 Cancel 包、关掉这条连接：客户端断开之后 0.5ms handler 就返回了。
  服务端按数据块停下（约 115ms 从 `system.processes` 消失），`sleep(2.5)` 打断不了。
- `Ping` 只认截止时间、不认取消。建连探测每次都带截止时间（`2 × dial_timeout`），启动期间收到退出信号约 400ms 后退出。
- **`DialTimeout: 0` 且 DSN 里没写 `dial_timeout`** 时驱动按 30s 建连（v2.48.0 `setDefaults`），建连探测的单次预算也按它算（60s），
  不是兜底的 1s。实测（ClickHouse 24.8.14，中间一层代理让服务端的第一个字节晚 1.5s）：按 0 算时 3 次各 1s 全部超时、5.3s 后启动失败；
  按 30s 算 1.5s 连上。
- 参数由驱动代进语句再发给服务端：浮点数写成 `cast(1.5, 'Float64')`，在 `system.query_log` 里按语句文本找时要按这个写法找。

**认证失败**认的是服务端回的 `*clickhouse.Exception`（HTTP 协议下包在 `*clickhouse.HTTPError` 里）：
516（AUTHENTICATION_FAILED）、192 / 193 / 194（老版本的 UNKNOWN_USER、WRONG_PASSWORD、REQUIRED_PASSWORD）。
实测密码错、用户不存在都是 `code: 516`，只试 1 次、50ms 内失败；HTTP 协议下是 `[HTTP 403] code: 516`；
库不存在是 81，不算认证失败，照常试满 3 次。
报错里通常带着 `code: 81`，偶尔只有 `driver: bad connection`：库不存在时握手照样成功，服务端随后发来异常、关掉连接；
驱动的 `database/sql` 路径在 ping 之前先从连接上读一个字节检查它还活着没有，异常要是已经到了，就被这一下读走，
连接被当成坏的丢掉。实测 64 个协程并发各连 20 次（共 1280 次），`database/sql` 路径约 9% 是这样，驱动的原生 API 一次都没有。

**TLS**（`verificationMode=relaxed`）：native 与 `https://` 都走 TLS 块，`system.query_log` 都是 `is_secure=1`。

- CA 不对、`ServerName` 对不上、客户端证书不是服务端认的 CA 签的：都在 2–6ms 内失败、不重试。
- TLS 块开着连到明文端口：`first record does not look like a TLS handshake`（native）/
  `server gave HTTP response to HTTPS client`（HTTPS），照常重试。
- 不开 TLS 块时 DSN 的 `skip_verify=true` 连得上，但服务端证书根本不校验。
- GORM 的 clickhouse 驱动拿到 DSN 会另解一份、在带 `UpdateLocalTable` 的 UPDATE 里按那一份直连每台主机（`update.go`），
  那几条直连不带 TLS 块——所以开着 TLS 块时不把 DSN 交给它。

多主机 `clickhouse://u:p@h1:9000,h2:9000/db` 按 `in_order` 依次去连，第一个挂了之后查询照常（e2e）；
日志的 `addr`、Span 的 `server.address` 记第一个。
