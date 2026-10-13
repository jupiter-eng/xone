package xgorm

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// 配置里的 TLS 块怎么交给两个内置驱动。
//
// 两边都不经 DSN 传：DSN 只说得出驱动认得的那几个参数，说不出「校验时比对这个名字」
// （pgx 没有这个参数，verify-full 只拿 host 比对），也就接不住 xtls.Config 的 ServerName；
// 而 MySQL 要经 DSN 传就得先把 *tls.Config 用 RegisterTLSConfig 登记成一个进程级的名字，
// 每个实例一个、关的时候还得注销。这里直接把 *tls.Config 交给驱动的连接配置：
// DSN 一个字节不动，也不碰任何全局。

// openPostgresTLS PostgreSQL 的 OpenTLS。
//
// DSN 照旧交给 gorm 的 postgres 驱动去解（时区那套处理就还是它的），
// 只在每次建连之前把 pgx 连接配置里的 TLS 换成我们的（stdlib.OptionBeforeConnect，
// 拿到的是一份浅拷贝，改它不影响别的连接）。
func openPostgresTLS(dsn string, cfg *tls.Config) (gorm.Dialector, error) {
	return postgres.New(postgres.Config{
		DSN: dsn,
		OptionOpenDB: []stdlib.OptionOpenDB{stdlib.OptionBeforeConnect(func(_ context.Context, cc *pgx.ConnConfig) error {
			usePostgresTLS(&cc.Config, cfg)
			return nil
		})},
	}), nil
}

// usePostgresTLS 让每个主机都只走 cfg 给出的 TLS。
//
// pgx 把 sslmode 翻译成「主机 × TLS 配置」的一串候选（pgconn v5.10.0 configTLS）：
// 默认的 prefer 是每个主机先试一次不校验证书的 TLS、再试一次明文，allow 反过来。
// 这里按主机去重，每个主机只留一条、TLS 换成 cfg——明文的那条就没了，
// 服务端不肯 TLS 时 pgx 报 server refused TLS connection，而不是悄悄改走明文。
// ServerName 没配时按主机名补，和 verify-full 一样。
func usePostgresTLS(c *pgconn.Config, cfg *tls.Config) {
	type target struct {
		host string
		port uint16
	}
	seen := map[target]bool{}
	var hosts []*pgconn.FallbackConfig
	for _, h := range postgresHosts(c) {
		if t := (target{h.Host, h.Port}); !seen[t] {
			seen[t] = true
			tc := cfg.Clone()
			if tc.ServerName == "" {
				tc.ServerName = h.Host
			}
			hosts = append(hosts, &pgconn.FallbackConfig{Host: h.Host, Port: h.Port, TLSConfig: tc})
		}
	}
	c.Host, c.Port, c.TLSConfig = hosts[0].Host, hosts[0].Port, hosts[0].TLSConfig
	c.Fallbacks = hosts[1:]
}

// postgresHosts 连接配置里的全部主机：主地址在前，之后是 pgx 生成的候选（Fallbacks）
func postgresHosts(c *pgconn.Config) []*pgconn.FallbackConfig {
	return append([]*pgconn.FallbackConfig{{Host: c.Host, Port: c.Port}}, c.Fallbacks...)
}

// checkPostgresTLSParams 开了 TLS 块时，DSN 里不许再写 TLS 参数
func checkPostgresTLSParams(dsn string) error {
	key, err := postgresTLSParam(dsn)
	if err != nil {
		return err
	}
	if key != "" {
		return fmt.Errorf("the DSN sets %s while the TLS block is enabled; configure TLS in one place only "+
			"(remove the ssl* parameters from the DSN, or drop the TLS block)", key)
	}
	return nil
}

// checkPostgresTCP 开了 TLS 块时，每个主机都得走 TCP：pgx 在 Unix socket 上不做 TLS
// （pgconn v5.10.0 ParseConfig，照 libpq）。pc 是这串 DSN 解出来的连接配置
func checkPostgresTCP(pc *pgconn.Config) error {
	for _, h := range postgresHosts(pc) {
		if network, _ := pgconn.NetworkAddress(h.Host, h.Port); network == "unix" {
			return fmt.Errorf("the TLS block is enabled but host %s is a Unix socket, TLS only runs over TCP", h.Host)
		}
	}
	return nil
}

// disallowedKey pgconn 报「这个 key 不在允许列表里」时的那句话，key 用 %q 写在末尾
// （pgconn v5.10.0 ParseConfigWithOptions）
var disallowedKey = regexp.MustCompile(`connection string key (".*") is not in ConnStringAllowedKeys$`)

// postgresTLSParam DSN 里写了的第一个 TLS 参数（ssl 开头的 key），没写是空串。
//
// 读 DSN 用的是 pgx 自己的解析器，不在这里照抄一遍它的引号与转义规则（理由见
// prependPostgresKV）：ConnStringAllowedKeys 让 pgx 在遇到不在列表里的 key 时报出它的名字，
// 而且是在读任何文件之前。从空列表开始，每报出一个不是 ssl 开头的 key 就放进列表再解，
// 直到解得通（没写 TLS 参数）或报出一个 ssl 开头的。key 本身不含凭证，可以写进错误；
// 错误里的其余部分（整串 DSN）不回传。
//
// 环境变量（PGSSLMODE 等）和 service 文件里的 TLS 设置不算：它们不在 DSN 里，
// 开了 TLS 块时一律被这一块盖掉（见 usePostgresTLS）。
func postgresTLSParam(dsn string) (string, error) {
	allowed := []string{}
	for {
		_, err := pgconn.ParseConfigWithOptions(dsn, pgconn.ParseConfigOptions{ConnStringAllowedKeys: allowed})
		if err == nil {
			return "", nil
		}
		key, ok := disallowed(err)
		if !ok || slices.Contains(allowed, key) {
			// 这串 DSN parsePostgres 已经解通过了，走到这里只可能是 pgx 换了那句话的写法
			return "", errors.New("cannot tell whether the DSN sets TLS parameters (details omitted to keep credentials out of logs)")
		}
		if strings.HasPrefix(key, "ssl") {
			return key, nil
		}
		allowed = append(allowed, key)
	}
}

// disallowed 从 pgconn 的错误里取出被拒的那个 key
func disallowed(err error) (string, bool) {
	var pce *pgconn.ParseConfigError
	if !errors.As(err, &pce) {
		return "", false
	}
	m := disallowedKey.FindStringSubmatch(err.Error())
	if m == nil {
		return "", false
	}
	key, uerr := strconv.Unquote(m[1])
	return key, uerr == nil
}

// checkMySQLTLS 开了 TLS 块时，DSN 里不许再写 tls= 和 allowFallbackToPlaintext=，也不能走 Unix socket。
//
// allowFallbackToPlaintext=true 的意思是「服务端的问候包里没有 CLIENT_SSL 就改走明文」
// （go-sql-driver v1.10.1 packets.go readHandshakePacket）。这一位是明文发过来的，
// 中间人清掉它，客户端就把登录包连同之后的每条 SQL 明文交出去——开了 TLS 块也一样。
// 写成 false 也拒：这个参数和 tls= 一样只该在一处配
func checkMySQLTLS(dsn string, cfg *mysqldriver.Config) error {
	if mysqlParamSet(dsn, "tls") {
		return fmt.Errorf("the DSN sets tls while the TLS block is enabled; configure TLS in one place only " +
			"(remove tls= from the DSN, or drop the TLS block)")
	}
	if mysqlParamSet(dsn, "allowFallbackToPlaintext") {
		return fmt.Errorf("the DSN sets allowFallbackToPlaintext while the TLS block is enabled; " +
			"the TLS block never falls back to plaintext (remove allowFallbackToPlaintext= from the DSN)")
	}
	if cfg.Net != "tcp" && cfg.Net != "tcp6" && cfg.Net != "tcp4" {
		return fmt.Errorf("the TLS block is enabled but the DSN connects over %s, TLS only runs over TCP", cfg.Net)
	}
	return nil
}

// openMySQLTLS MySQL 的 OpenTLS：解开 DSN、把 cfg 放进驱动的连接配置，用 connector 建连接池。
//
// ServerName 没配时由驱动按 Addr 的主机部分补上（go-sql-driver v1.10.1 Config.normalize，
// NewConnector 会调它）。版本探测照旧挪到 Ready，理由见 openMySQL。
//
// 连接池在这里就建出来了：gorm.Open 的 Initialize 失败时会替我们关掉它，
// 之后的每一步失败由 open 关（和 Open 那条路一样经 db.DB() 取到的就是它）。
func openMySQLTLS(dsn string, cfg *tls.Config) (gorm.Dialector, error) {
	dc, err := mysqldriver.ParseDSN(dsn)
	if err != nil {
		return nil, errMalformedDSN // 不回传驱动的错误，理由见 resolveMySQL
	}
	dc.TLS = cfg.Clone()
	// 兜底：checkMySQLTLS 已经拒了 DSN 里的这个参数，这里再钉死一次，
	// 服务端（或中间人）不给 CLIENT_SSL 时报 ErrNoTLS，不改走明文
	dc.AllowFallbackToPlaintext = false
	connector, err := mysqldriver.NewConnector(dc)
	if err != nil {
		return nil, errMalformedDSN
	}
	return mysql.New(mysql.Config{
		Conn:                      sql.OpenDB(connector),
		DSNConfig:                 dc,
		SkipInitializeWithVersion: true,
	}), nil
}
