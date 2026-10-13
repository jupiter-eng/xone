package clickhouse

import (
	"crypto/tls"
	"fmt"
	"net/url"
	"strings"

	chgo "github.com/ClickHouse/clickhouse-go/v2"
	"gorm.io/driver/clickhouse"
	"gorm.io/gorm"
)

// 配置里的 TLS 块怎么交给 clickhouse-go。
//
// 驱动的 DSN 只说得出 secure / skip_verify / tls_server_name 三项，说不出 CA、客户端证书，
// 所以不经 DSN 传：用驱动自己的解析器解开 DSN，把 *tls.Config 放进 Options，
// 再用 clickhouse.OpenDB 建连接池交给 GORM。DSN 一个字节不动，也不碰任何全局。

// tlsParams DSN 里说 TLS 的那几个参数（clickhouse-go v2.48.0 Options.fromDSN）。
// 开了 TLS 块时它们一个都不许写：两处都说 TLS，就说不清最后听谁的
var tlsParams = []string{"secure", "skip_verify", "tls_server_name"}

// checkTLS 开了 TLS 块时 DSN 里不许再说 TLS，也不许走 http://。
//
// http:// 不是「HTTP 协议、要不要 TLS 另说」：驱动把 scheme 原样记下来，
// 建 HTTP 连接时就用它拼请求地址（v2.48.0 conn_http.go dialHttp），
// 手里有 *tls.Config 也照样发明文 HTTP。所以它和 TLS 块放在一起只能是配错了。
func checkTLS(u *url.URL, q url.Values) error {
	for _, k := range tlsParams {
		if q.Has(k) {
			return fmt.Errorf("the DSN sets %s while the TLS block is enabled; configure TLS in one place only "+
				"(remove secure / skip_verify / tls_server_name from the DSN, or drop the TLS block)", k)
		}
	}
	if u.Scheme == "http" {
		return fmt.Errorf("the DSN uses http:// while the TLS block is enabled, and http:// never runs TLS; " +
			"use https:// for the HTTP protocol or clickhouse:// for the native protocol")
	}
	return nil
}

// parseDSN 用驱动的解析器解 DSN。withTLS 表示开了 TLS 块。
//
// 驱动要求 https:// 必须同时写 secure=true，否则报 https without TLS；而开了 TLS 块的
// DSN 里不许写 secure（见 checkTLS）。所以这时补上 secure=true 再解——只补在交给解析器的
// 这一份上。解出来的 TLS 设置随后整个换成 TLS 块的（见 openTLS），补的这一项不留任何作用，
// 它只为让驱动记下 https 这个 scheme：驱动建 HTTP 连接时照这个 scheme 拼地址。
func parseDSN(dsn string, withTLS bool) (*chgo.Options, error) {
	if withTLS && strings.HasPrefix(dsn, "https://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return nil, err
		}
		q, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			return nil, err
		}
		q.Set("secure", "true")
		u.RawQuery = q.Encode()
		dsn = u.String()
	}
	return chgo.ParseDSN(dsn)
}

// openTLS ClickHouse 的 OpenTLS：解开 DSN、把 cfg 放进驱动的 Options，用 OpenDB 建连接池。
//
// ServerName 没配时留空，由标准库按每次建连的地址补上：native 协议走 tls.DialWithDialer，
// HTTP 协议走 http.Transport，两者都在 ServerName 为空时取所连主机的主机部分。
// 在这里按第一个主机补反而不对：多主机 DSN 连到第二台时还拿第一台的名字去比对证书。
//
// 不把 DSN 交给 GORM 的驱动（Config 里只有 Conn）：gorm.io/driver/clickhouse v0.7.0
// 拿到 DSN 会自己再解一份 Options 留着，UPDATE 带了 UpdateLocalTable 时按那一份
// 用 clickhouse.Open 直连每一台主机（update.go），不经连接池——那一份里没有 TLS 块，
// 这几条直连就是明文。没有 DSN 时那条路径不走，UPDATE 照常经连接池发出。
//
// 版本探测照旧挪到 Ready，理由见 open。连接池在这里就建出来了（OpenDB 不碰网络），
// 之后 gorm.Open 与 xgorm 建连验证里的每一步失败，由 xgorm 经 db.DB() 取到它关掉。
func openTLS(dsn string, cfg *tls.Config) (gorm.Dialector, error) {
	opts, err := parseDSN(dsn, true)
	if err != nil {
		return nil, errMalformedDSN // 不回传驱动的错误，理由见 resolve
	}
	opts.TLS = cfg.Clone()
	return clickhouse.New(clickhouse.Config{Conn: chgo.OpenDB(opts), SkipInitializeWithVersion: true}), nil
}
