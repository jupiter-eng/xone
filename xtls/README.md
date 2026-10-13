# xtls

客户端 TLS 块：XGorm / XRedis / XHttp / XKafka 连出去时的 TLS 写成同一个 `TLS:` 块（核心模块，`xtls.Config`），字段、默认值、校验规则只有这一份。

- 证书一律校验，没有跳过校验的开关；最低 TLS 1.2
- 开着 TLS 块就不会退回明文
- 配错的组合在读配置时就失败，证书文件在建实例时读

## 快速上手

用内部 CA 签的证书连 PostgreSQL，服务端要求双向认证：

```yaml
# conf/application.yml
XGorm:
  DSN: "${DB_DSN}"                    # DSN 里不再写 sslmode
  TLS:
    Enable: true
    CAFile: /etc/ssl/internal-ca.pem  # 只认这个 CA，系统根证书不再参与
    CertFile: /etc/ssl/client.pem     # 服务端要客户端证书时和 KeyFile 成对填
    KeyFile: /etc/ssl/client-key.pem
    ServerName: db.internal           # 按 IP 连、证书上是域名时填
```

XRedis、XHttp、XKafka 块里写法完全一样。

## 配置

以 XRedis 为例，XGorm（PostgreSQL / MySQL / ClickHouse）、XHttp 块里的 `TLS:` 字段一模一样：

```yaml
XRedis:
  TLS:
    Enable: true                      # 默认 false；下面几项只在开着时生效
    CAFile: /etc/ssl/internal-ca.pem  # 校验服务端证书的 CA（PEM，可以多张）。空 = 系统根证书
    CertFile: /etc/ssl/client.pem     # 客户端证书，服务端要求双向认证时和 KeyFile 成对填
    KeyFile: /etc/ssl/client-key.pem
    ServerName: redis.internal        # 比对证书的名字。空 = 连接地址的主机部分
```

- 填了 `CAFile` 就**只认**这个文件里的 CA，系统根证书不再参与。自签证书把 CA 填进这里。
- 要更细的控制（加密套件、自定义校验）就绕开配置、自己造原生 client。
- 服务端（XGin、XEcho）的 `TLS:` 块是另一组字段：`CertFile`、`KeyFile`、`ClientCAFile`、`MinVersion`，没有 `Enable`（证书配了就开），见 [XGin](../xgin/README.md#配置)。

## API

使用者只在配置文件里写它；直接调某个模块的 `New` 时才会在代码里写到 `xtls.Config`。

| 函数 | 说明 |
|---|---|
| `(Config) Validate() error` | 检查几项之间说不通的组合（各模块的 `Validate` 会调它） |
| `(Config) Build() (*tls.Config, error)` | 先校验，再读证书文件装出 `*tls.Config`；没开 TLS 时返回 nil |

## 注意事项

- **开着 TLS 块时 DSN 里不能再写 TLS 参数**（PG 的 `ssl*`、MySQL 的 `tls=` / `allowFallbackToPlaintext=`、ClickHouse 的 `secure` /
  `skip_verify` / `tls_server_name`），两处都写是配置错误；也必须走 TCP（Unix socket 上不做 TLS）。
- **不开 TLS 块时 pgx 默认连上了也不校验证书**（`sslmode=prefer`），go-sql-driver 不写 `tls` 就是明文。见[「行为与实测」](#行为与实测)。
- 没开 `Enable` 却写了别的几项（多半是忘了开）、`CertFile` / `KeyFile` 只配一个，读配置时就失败。
- 证书文件在建实例时读，读不出来报 op 为 `config` 的错，一次都不连。
- 证书被拒不重试：再试还是同一张证书、同一个结论。各驱动报什么错、要多久，见[「行为与实测」](#行为与实测)。

## 行为与实测

实测环境和跨模块的总表见 [`docs/behavior.md`](../docs/behavior.md)。

实测（e2e：测试时现造的 CA，PG 16、MySQL 8.0.46、Redis 7.0.15 各起一个只收 TLS 的实例）：

| | CA 对：服务端看到的 | CA 不对 / 不填（系统根证书） | `ServerName` 对不上 | 服务端要客户端证书而没带 | 明文连过去 |
|---|---|---|---|---|---|
| XGorm · PostgreSQL | `pg_stat_ssl`：`ssl=t`、`TLSv1.3`，双向认证时 `client_dn=/CN=…` | `x509: certificate signed by unknown authority`，3ms，不重试 | `x509: certificate is valid for localhost, db.e2e.internal, not wrong.e2e.internal` | `FATAL: connection requires a valid client certificate (SQLSTATE 28000)`，按认证失败报 | `no pg_hba.conf entry … no encryption` |
| XGorm · MySQL | `Ssl_version=TLSv1.3`，连接池里每条新连接都是 | 同上，2ms | 同上 | `REQUIRE X509` 的账号回 1045，按认证失败报 | `require_secure_transport=ON` 回 3159，照常重试，1.9s |
| XRedis | 服务端只开 `tls-port` | 同上，1–4ms | 同上 | 多数是 `remote error: tls: certificate required`，约 0.45s；也实测到每次尝试都撞上 `write: connection reset by peer`，2.4s | `EOF`，照常重试，2.0–3.5s |
| XHttp | 桩服务端看到 `HTTP/2.0`、`TLS 1.3` 和客户端证书的 CN | 同上 | 同上 | 报什么说不准，见下 | —— |

两件量出来才知道的事：

- **TLS 1.3 下服务端拒客户端证书，客户端要到下一次读才知道。** 客户端发完 Finished 就当握手成功、开始写，
  服务端的告警晚一步到：go-redis 第一次尝试常常先撞上 `broken pipe` / `EOF`，退避一次之后才读到告警，
  所以是 0.45s 而不是几毫秒；两次尝试都撞上的话报的是 `connection reset by peer`，看不出是证书的事；net/http 有时报 `remote error: tls: certificate required`，
  有时报 `write: broken pipe`，HTTP/2 的连接只报 `http2: client conn could not be established`。
- **拿着别的 CA 签的客户端证书，Go 的客户端可能根本不出示它。** 服务端在握手里列出它认的 CA 时，
  crypto/tls 只出示这些 CA 签的证书——PG、net/http 的服务端看到的都是「没带」。
  mysqld 和 redis-server 不列，证书照样发过去，服务端回 `unknown_ca`：go-redis 报
  `remote error: tls: unknown certificate authority`；go-sql-driver v1.10.1 报 `invalid connection`（照常重试），
  告警只出现在一条 `xgorm go-sql-driver log` 里（`detail` 是 `packets.go:58 remote error: tls: unknown certificate authority`）。

**不开 TLS 块时，两个数据库驱动的默认差得很远**：pgx（DSN 不写 `sslmode` 即 `prefer`）连一个开着 ssl 的 PG
照样走 TLS（`pg_stat_ssl` 是 `ssl=t`），但**服务端证书根本不校验**——拿一个不相干的 CA 签的证书也连得上；
服务端不肯 TLS 就悄悄改走明文。go-sql-driver 不写 `tls` 就是明文。
开着 TLS 块时 PostgreSQL 按主机去重、每个主机只走这一块的 TLS，**不会退回明文**（服务端不肯 TLS 时报
`server refused TLS connection`）。

## 排错

| 错误原文 | 原因 | 怎么改 |
|---|---|---|
| `TLS fields are set but TLS.Enable is false` | 写了 `CAFile` 等却没开 `Enable` | 加 `Enable: true`，或者删掉那几项 |
| `TLS.CertFile and TLS.KeyFile must be set together` | 客户端证书只配了一半 | 两个都填 |
| `the DSN sets sslmode while the TLS block is enabled; configure TLS in one place only` | PG 的 DSN / `Postgres.Params` 里有 `ssl*` 参数，同时开了 TLS 块 | 二选一 |
| `the DSN sets tls while the TLS block is enabled` | MySQL 的 DSN 里写了 `tls=`（哪怕 `tls=false`） | 二选一 |
| `the DSN sets allowFallbackToPlaintext while the TLS block is enabled` | MySQL 的 DSN 里写了 `allowFallbackToPlaintext=`（哪怕 `=false`） | 从 DSN 里删掉：TLS 块从不退回明文 |
| `the TLS block is enabled but … Unix socket` / `… connects over unix` | PG 的主机或 MySQL 的 DSN 走 Unix socket | 改成 TCP 地址，或者不开 TLS 块 |
| `the DSN sets secure while the TLS block is enabled` | ClickHouse 的 DSN 里有 `secure` / `skip_verify` / `tls_server_name` | 二选一 |
| `the DSN uses http:// while the TLS block is enabled, and http:// never runs TLS` | ClickHouse 的 DSN 是 `http://` | 改成 `https://` |
| `Driver="…" does not support the TLS block, configure TLS in its DSN instead` | 这个驱动没提供 `OpenTLS` | 在它的 DSN 里配 TLS |
| `read TLS.CAFile: …` / `TLS.CAFile … contains no PEM certificate` / `load TLS.CertFile / TLS.KeyFile: …` | 证书文件读不出来、不是 PEM，或证书和私钥不配对 | 检查路径和文件内容 |
| `x509: certificate signed by unknown authority` | 服务端证书不是 `CAFile` 里的 CA 签的（没填 `CAFile` 时是系统根证书） | 把签发它的 CA 填进 `CAFile` |
| `x509: certificate is valid for …, not …` | 证书上的名字和连接地址对不上 | 按 IP 连时填 `ServerName` |
| `remote error: tls: certificate required` / `unknown certificate authority` | 服务端要客户端证书，没带或不是它认的 CA 签的 | 填 `CertFile` / `KeyFile`，用服务端认的 CA 签 |
