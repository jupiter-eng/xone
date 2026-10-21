# xtls

The client TLS block: TLS for the outbound connections of XGorm / XRedis / XHttp / XKafka is written as the same `TLS:` block (a core module, `xtls.Config`), and the fields, the defaults and the validation rules exist only once.

- Certificates are always verified, there is no switch to skip verification, and the minimum is TLS 1.2
- While the TLS block is on there is no fallback to plaintext
- A misconfigured combination fails while the config is read; the certificate files are read when the instance is built

## Quick start

Connecting to PostgreSQL with a certificate signed by an internal CA, where the server requires mutual authentication:

```yaml
# conf/application.yml
XGorm:
  DSN: "${DB_DSN}"                    # the DSN no longer sets sslmode
  TLS:
    Enable: true
    CAFile: /etc/ssl/internal-ca.pem  # only this CA is accepted; the system roots no longer take part
    CertFile: /etc/ssl/client.pem     # set together with KeyFile when the server wants a client certificate
    KeyFile: /etc/ssl/client-key.pem
    ServerName: db.internal           # set it when connecting by IP to a certificate that names a domain
```

It is written exactly the same way inside the XRedis, XHttp and XKafka blocks.

## Configuration

XRedis is used as the example; the `TLS:` fields inside the XGorm (PostgreSQL / MySQL / ClickHouse) and XHttp blocks are identical:

```yaml
XRedis:
  TLS:
    Enable: true                      # defaults to false; the fields below only take effect while it is on
    CAFile: /etc/ssl/internal-ca.pem  # the CA verifying the server certificate (PEM, several are allowed). Empty = the system roots
    CertFile: /etc/ssl/client.pem     # the client certificate, set together with KeyFile when the server wants mutual authentication
    KeyFile: /etc/ssl/client-key.pem
    ServerName: redis.internal        # the name the certificate is verified against. Empty = the host part of the connection address
```

- When `CAFile` is set, **only** the CAs in that file are accepted and the system roots no longer take part. A self-signed certificate goes here.
- For finer control (cipher suites, custom verification), go around the configuration and build a native client.
- The server-side `TLS:` block (XGin, XEcho) is a different set of fields: `CertFile`, `KeyFile`, `ClientCAFile` and `MinVersion`, with no `Enable` (configuring a certificate turns it on). See [XGin](../xgin/README.md#configuration).

## API

Callers only write it in the config file; `xtls.Config` appears in code only when calling a module's `New` directly.

| Function | Description |
|---|---|
| `(Config) Validate() error` | Checks the combinations that do not make sense (each module's `Validate` calls it) |
| `(Config) Build() (*tls.Config, error)` | Validates first, then reads the certificate files and assembles a `*tls.Config`; with TLS off it returns nil |

## Notes

- **While the TLS block is on, the DSN must not set TLS parameters** (PG's `ssl*`, MySQL's `tls=` and
  `allowFallbackToPlaintext=`, ClickHouse's `secure` / `skip_verify` / `tls_server_name`); setting both is a
  configuration error. TCP is also required (there is no TLS over a Unix socket).
- **Without a TLS block, pgx connects without verifying the certificate** (`sslmode=prefer`), and go-sql-driver is
  plaintext unless `tls` is set. See ["Behavior (measured)"](#behavior-measured).
- Setting the other fields without `Enable` (most likely a forgotten Enable), or setting only one of
  `CertFile` / `KeyFile`, fails while the config is read.
- The certificate files are read when the instance is built; an unreadable one is reported as an error with op
  `config`, and nothing is connected to at all.
- A rejected certificate is not retried: another attempt means the same certificate and the same verdict. What each
  driver reports and how long it takes is in ["Behavior (measured)"](#behavior-measured).

## Behavior (measured)

The measurement environment and the cross-module summary table are in [`docs/behavior.md`](../docs/behavior.md).

Measured (e2e: a CA built during the test, with PG 16, MySQL 8.0.46 and Redis 7.0.15 each running one instance that
accepts TLS only):

| | Correct CA: what the server saw | Wrong CA / none (system roots) | `ServerName` does not match | The server wants a client certificate and none is sent | Connecting in plaintext |
|---|---|---|---|---|---|
| XGorm · PostgreSQL | `pg_stat_ssl`: `ssl=t`, `TLSv1.3`, and with mutual authentication `client_dn=/CN=…` | `x509: certificate signed by unknown authority`, 3ms, not retried | `x509: certificate is valid for localhost, db.e2e.internal, not wrong.e2e.internal` | `FATAL: connection requires a valid client certificate (SQLSTATE 28000)`, reported as an auth failure | `no pg_hba.conf entry … no encryption` |
| XGorm · MySQL | `Ssl_version=TLSv1.3`, for every new connection in the pool | The same, 2ms | The same | An account with `REQUIRE X509` replies 1045, reported as an auth failure | `require_secure_transport=ON` replies 3159, retried as usual, 1.9s |
| XRedis | The server only opens `tls-port` | The same, 1-4ms | The same | Usually `remote error: tls: certificate required`, about 0.45s; also measured was `write: connection reset by peer` on every attempt, 2.4s | `EOF`, retried as usual, 2.0-3.5s |
| XHttp | The stub server saw `HTTP/2.0`, `TLS 1.3` and the client certificate's CN | The same | The same | What is reported varies, see below | — |

Two things that only measuring made clear:

- **Under TLS 1.3 a client learns that the server rejected its certificate only on the next read.** The client treats
  the handshake as done once it has sent Finished and starts writing, while the server's alert arrives a step later:
  go-redis often hits `broken pipe` / `EOF` on the first attempt and only reads the alert after one backoff, which is
  why it takes 0.45s instead of a few milliseconds. When both attempts hit it, what is reported is
  `connection reset by peer`, which gives no hint that a certificate is involved; net/http sometimes reports
  `remote error: tls: certificate required` and sometimes `write: broken pipe`, and an HTTP/2 connection only reports
  `http2: client conn could not be established`.
- **With a client certificate signed by a different CA, a Go client may not present it at all.** When the server lists
  the CAs it accepts during the handshake, crypto/tls only presents certificates signed by those CAs -- so the PG and
  net/http servers both saw "none sent". mysqld and redis-server do not list them, the certificate is sent anyway and
  the server replies `unknown_ca`: go-redis reports `remote error: tls: unknown certificate authority`, while
  go-sql-driver v1.10.1 reports `invalid connection` (retried as usual) and the alert appears only in a single
  `xgorm go-sql-driver log` record (whose `detail` is
  `packets.go:58 remote error: tls: unknown certificate authority`).

**Without a TLS block the defaults of the two database drivers are far apart**: pgx (a DSN without `sslmode`, that is
`prefer`) connecting to a PG with ssl enabled still goes through TLS (`pg_stat_ssl` shows `ssl=t`), but **the server
certificate is not verified at all** -- a certificate signed by an unrelated CA connects just as well, and a server
unwilling to do TLS makes it switch quietly to plaintext. go-sql-driver is plaintext unless `tls` is set. With a TLS
block, PostgreSQL deduplicates by host and each host goes through this block's TLS only, with **no fallback to
plaintext** (a server unwilling to do TLS is reported as `server refused TLS connection`).

## Troubleshooting

| Error text | Cause | What to change |
|---|---|---|
| `TLS fields are set but TLS.Enable is false` | `CAFile` or another field is set without `Enable` | Add `Enable: true`, or remove those fields |
| `TLS.CertFile and TLS.KeyFile must be set together` | Only half of the client certificate is configured | Set both |
| `the DSN sets sslmode while the TLS block is enabled; configure TLS in one place only` | A PG DSN or `Postgres.Params` holds an `ssl*` parameter while the TLS block is on | Pick one of the two |
| `the DSN sets tls while the TLS block is enabled` | A MySQL DSN sets `tls=` (even `tls=false`) | Pick one of the two |
| `the DSN sets allowFallbackToPlaintext while the TLS block is enabled` | A MySQL DSN sets `allowFallbackToPlaintext=` (even `=false`) | Remove it from the DSN: a TLS block never falls back to plaintext |
| `the TLS block is enabled but … Unix socket` / `… connects over unix` | A PG host or a MySQL DSN uses a Unix socket | Use a TCP address, or leave the TLS block off |
| `the DSN sets secure while the TLS block is enabled` | A ClickHouse DSN holds `secure` / `skip_verify` / `tls_server_name` | Pick one of the two |
| `the DSN uses http:// while the TLS block is enabled, and http:// never runs TLS` | The ClickHouse DSN is `http://` | Change it to `https://` |
| `Driver="…" does not support the TLS block, configure TLS in its DSN instead` | That driver provides no `OpenTLS` | Configure TLS in its DSN |
| `read TLS.CAFile: …` / `TLS.CAFile … contains no PEM certificate` / `load TLS.CertFile / TLS.KeyFile: …` | A certificate file cannot be read, is not PEM, or the certificate and the key do not match | Check the path and the file contents |
| `x509: certificate signed by unknown authority` | The server certificate is not signed by a CA in `CAFile` (the system roots when `CAFile` is empty) | Put the signing CA into `CAFile` |
| `x509: certificate is valid for …, not …` | The name on the certificate does not match the connection address | Set `ServerName` when connecting by IP |
| `remote error: tls: certificate required` / `unknown certificate authority` | The server wants a client certificate, and none was sent or it was not signed by a CA it accepts | Set `CertFile` / `KeyFile`, signed by a CA the server accepts |
