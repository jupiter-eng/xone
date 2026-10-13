package e2e

// TLS 端到端测试：TestTLS_* 系列。
//
//	scripts/e2e.sh -run TLS
//
// 服务端都是真的，但不是本机常驻的那几个：harness 现造一套证书，另起只收 TLS 的
// PostgreSQL 集群、mysqld、redis-server（见 harness/tls.go），测试结束时停掉删掉。
// 客户端一侧走的是使用者的路：配置文件里写 TLS 块，跑一遍启动钩子（xonetest），
// 拿 xgorm.C / xredis.C / xhttp.C 取原生 client。连不上的那些直接调 New，看它报的错。
//
// 每一种都查三件事：CA 对了连得上，而且**服务端**说这条连接是加密的
// （pg_stat_ssl、MySQL 的 Ssl_cipher、Redis 只开了 TLS 端口）；CA 不对、名字对不上、
// 明文都被拒，错误说得清是什么；错误里没有密码。
//
// 文件划分：
//
//	tls_test.go       本文件：PostgreSQL、MySQL、Redis
//	tls_http_test.go  xhttp 连 TLS 桩（自签 CA + 双向认证）、xgin 的 TLS.ClientCAFile / MinVersion
//	clickhouse_tls_test.go  ClickHouse（Docker 里另起的容器，见 harness/tls_clickhouse.go）

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
	"github.com/jupiter-eng/xone/xgorm"
	"github.com/jupiter-eng/xone/xonetest"
	"github.com/jupiter-eng/xone/xredis"
	"github.com/jupiter-eng/xone/xtls"
)

// tlsBlock 配置文件里的一个 TLS 块（流式写法，缩进无所谓）
func tlsBlock(t xtls.Config) string {
	parts := []string{fmt.Sprintf("Enable: %v", t.Enable)}
	for _, kv := range [][2]string{{"CAFile", t.CAFile}, {"CertFile", t.CertFile}, {"KeyFile", t.KeyFile}, {"ServerName", t.ServerName}} {
		if kv[1] != "" {
			parts = append(parts, fmt.Sprintf("%s: %q", kv[0], kv[1]))
		}
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// noSecret 错误里不该有密码
func noSecret(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), harness.TLSPassword) {
		t.Errorf("错误里带着密码：%v", err)
	}
}

// rejected 断言 err 不为 nil、说到了 want 里的每一句，且没有密码；返回耗时供记录
func rejected(t *testing.T, what string, err error, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：应被拒，却连上了", what)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("%s：错误里该有 %q，got=%v", what, w, err)
		}
	}
	noSecret(t, err)
}

// ---- PostgreSQL ----

func pgTLSConfig(dsn string, tc xtls.Config) xgorm.ClientConfig {
	c := xgorm.DefaultClientConfig()
	c.Driver, c.DSN, c.TLS = xgorm.DriverPostgres, dsn, tc
	c.Trace, c.Metric = false, false
	return c
}

// pgSSL 服务端 pg_stat_ssl 里这条连接的那一行
func pgSSL(t *testing.T, name string) (ssl bool, version, cipher, clientDN string) {
	t.Helper()
	row := xgorm.CWithCtx(context.Background(), name).
		Raw("SELECT ssl, coalesce(version, ''), coalesce(cipher, ''), coalesce(client_dn, '') FROM pg_stat_ssl WHERE pid = pg_backend_pid()").Row()
	if err := row.Scan(&ssl, &version, &cipher, &clientDN); err != nil {
		t.Fatalf("%s：查 pg_stat_ssl：%v", name, err)
	}
	return
}

func TestTLS_PostgreSQL(t *testing.T) {
	harness.Require(t)
	certs := harness.NewTLSCerts(t)
	pg := harness.StartTLSPostgres(t, certs)
	ca := xtls.Config{Enable: true, CAFile: certs.CAFile}
	mtls := xtls.Config{Enable: true, CAFile: certs.CAFile, CertFile: certs.ClientCert, KeyFile: certs.ClientKey}

	t.Run("配置文件里的TLS块生效且服务端说连接是加密的", func(t *testing.T) {
		named := ca
		named.ServerName = harness.TLSServerName
		xonetest.UseConfigYAML(t, fmt.Sprintf(`XGorm:
  Clients:
    ca:   {DSN: %q, TLS: %s, Trace: false}
    name: {DSN: %q, TLS: %s, Trace: false}
    mtls: {DSN: %q, TLS: %s, Trace: false}
`, pg.DSN("postgres"), tlsBlock(ca), pg.DSN("postgres"), tlsBlock(named), pg.DSN(harness.TLSMTLSUser), tlsBlock(mtls)))
		xonetest.StartHooks(t)

		for _, name := range []string{"ca", "name", "mtls"} {
			ssl, version, cipher, dn := pgSSL(t, name)
			if !ssl || !strings.HasPrefix(version, "TLSv1.") {
				t.Errorf("%s：pg_stat_ssl 说这条连接没加密：ssl=%v version=%q", name, ssl, version)
			}
			if name == "mtls" && !strings.Contains(dn, harness.TLSMTLSUser) {
				t.Errorf("mtls：服务端应看到客户端证书，client_dn=%q", dn)
			}
			t.Logf("数字：实例 %s：%s %s client_dn=%q", name, version, cipher, dn)
		}
	})

	cases := []struct {
		name string
		cfg  xgorm.ClientConfig
		want []string
	}{
		{"CA不对", pgTLSConfig(pg.DSN("postgres"), xtls.Config{Enable: true, CAFile: certs.OtherCAFile}),
			[]string{"cannot reach", "certificate signed by unknown authority"}},
		{"不填CA用系统根证书", pgTLSConfig(pg.DSN("postgres"), xtls.Config{Enable: true}),
			[]string{"certificate signed by unknown authority"}},
		{"ServerName对不上", pgTLSConfig(pg.DSN("postgres"), xtls.Config{Enable: true, CAFile: certs.CAFile, ServerName: "wrong.e2e.internal"}),
			[]string{"certificate is valid for", "wrong.e2e.internal"}},
		// 服务端要客户端证书（clientcert=verify-full）：它回 28000，按认证失败处理
		{"要客户端证书却没带", pgTLSConfig(pg.DSN(harness.TLSMTLSUser), ca),
			[]string{"authentication to", "SQLSTATE 28000"}},
		// 服务端在握手里列出它认的 CA，Go 的客户端手里的证书不是它们签的就不出示（crypto/tls
		// 按 CertificateRequest 挑证书）：服务端看到的是「没带」，同样回 28000
		{"客户端证书不是服务端认的CA签的", pgTLSConfig(pg.DSN(harness.TLSMTLSUser),
			xtls.Config{Enable: true, CAFile: certs.CAFile, CertFile: certs.StrangerCert, KeyFile: certs.StrangerKey}),
			[]string{"authentication to", "connection requires a valid client certificate"}},
		// 明文：pg_hba 里只有 hostssl
		{"明文", pgTLSConfig(pg.DSN("postgres")+" sslmode=disable", xtls.Config{}),
			[]string{"no encryption"}},
		// 两处都说了 TLS：配置错误，一次都不连
		{"TLS块和DSN里的sslmode同时写", pgTLSConfig(pg.DSN("postgres")+" sslmode=require", ca),
			[]string{"sets sslmode", "configure TLS in one place"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start := time.Now()
			_, _, err := xgorm.New(context.Background(), c.cfg)
			rejected(t, c.name, err, c.want...)
			t.Logf("数字：%s 用时 %v，错误：%v", c.name, time.Since(start).Round(time.Millisecond), err)
		})
	}

	t.Run("没开TLS块时照pgx的默认协商TLS但不校验证书", func(t *testing.T) {
		// 量 pgx 的默认（sslmode=prefer）：DSN 里没写 sslmode 时照样走 TLS，但拿一个
		// 不认识的 CA 签的证书也连得上——这是要开 TLS 块（或者写 verify-full）的理由
		xonetest.UseConfigYAML(t, fmt.Sprintf("XGorm:\n  DSN: %q\n  Trace: false\n", pg.DSN("postgres")))
		xonetest.StartHooks(t)
		ssl, version, _, _ := pgSSL(t, xgorm.DefaultName)
		if !ssl {
			t.Errorf("pgx 默认的 prefer 应先试 TLS，pg_stat_ssl 说没加密")
		}
		t.Logf("数字：没开 TLS 块、DSN 不写 sslmode：ssl=%v %s（服务端证书没被校验）", ssl, version)
	})
}

// ---- MySQL ----

func mysqlTLSConfig(dsn string, tc xtls.Config) xgorm.ClientConfig {
	c := xgorm.DefaultClientConfig()
	c.Driver, c.DSN, c.TLS = xgorm.DriverMySQL, dsn, tc
	c.Trace, c.Metric = false, false
	return c
}

// mysqlStatus 这条会话的一项状态变量
func mysqlStatus(t *testing.T, name, key string) string {
	t.Helper()
	var k, v string
	// SHOW 不收占位符（服务端报 1064），key 是测试自己写的常量
	if err := xgorm.CWithCtx(context.Background(), name).Raw("SHOW SESSION STATUS LIKE '"+key+"'").Row().Scan(&k, &v); err != nil {
		t.Fatalf("%s：SHOW STATUS LIKE %s：%v", name, key, err)
	}
	return v
}

func TestTLS_MySQL(t *testing.T) {
	harness.Require(t)
	certs := harness.NewTLSCerts(t)
	my := harness.StartTLSMySQL(t, certs)
	ca := xtls.Config{Enable: true, CAFile: certs.CAFile}
	mtls := xtls.Config{Enable: true, CAFile: certs.CAFile, CertFile: certs.ClientCert, KeyFile: certs.ClientKey}

	t.Run("配置文件里的TLS块生效且服务端说连接是加密的", func(t *testing.T) {
		named := ca
		named.ServerName = harness.TLSServerName
		xonetest.UseConfigYAML(t, fmt.Sprintf(`XGorm:
  Clients:
    ca:   {Driver: mysql, DSN: %q, TLS: %s, Trace: false}
    name: {Driver: mysql, DSN: %q, TLS: %s, Trace: false}
    mtls: {Driver: mysql, DSN: %q, TLS: %s, Trace: false}
`, my.DSN("xone"), tlsBlock(ca), my.DSN("xone"), tlsBlock(named), my.DSN(harness.TLSMTLSUser), tlsBlock(mtls)))
		xonetest.StartHooks(t)

		for _, name := range []string{"ca", "name", "mtls"} {
			cipher, version := mysqlStatus(t, name, "Ssl_cipher"), mysqlStatus(t, name, "Ssl_version")
			if cipher == "" || !strings.HasPrefix(version, "TLSv1.") {
				t.Errorf("%s：服务端说这条连接没加密：Ssl_cipher=%q Ssl_version=%q", name, cipher, version)
			}
			t.Logf("数字：实例 %s：%s %s", name, version, cipher)
		}
		// 连接池里的每一条都走 TLS，不只是第一条
		db, _ := xgorm.C("ca").DB()
		db.SetMaxIdleConns(0)
		for i := range 3 {
			if v := mysqlStatus(t, "ca", "Ssl_cipher"); v == "" {
				t.Errorf("第 %d 条新连接没走 TLS", i+2)
			}
		}
	})

	cases := []struct {
		name string
		cfg  xgorm.ClientConfig
		want []string
	}{
		{"CA不对", mysqlTLSConfig(my.DSN("xone"), xtls.Config{Enable: true, CAFile: certs.OtherCAFile}),
			[]string{"cannot reach", "certificate signed by unknown authority"}},
		{"ServerName对不上", mysqlTLSConfig(my.DSN("xone"), xtls.Config{Enable: true, CAFile: certs.CAFile, ServerName: "wrong.e2e.internal"}),
			[]string{"certificate is valid for", "wrong.e2e.internal"}},
		// REQUIRE X509 的账号不带证书：服务端回 1045
		{"要客户端证书却没带", mysqlTLSConfig(my.DSN(harness.TLSMTLSUser), ca),
			[]string{"authentication to", "1045"}},
		// require_secure_transport=ON：服务端回 3159
		{"明文", mysqlTLSConfig(my.DSN("xone")+"?tls=false", xtls.Config{}),
			[]string{"3159"}},
		{"TLS块和DSN里的tls同时写", mysqlTLSConfig(my.DSN("xone")+"?tls=true", ca),
			[]string{"sets tls", "configure TLS in one place"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start := time.Now()
			_, _, err := xgorm.New(context.Background(), c.cfg)
			rejected(t, c.name, err, c.want...)
			t.Logf("数字：%s 用时 %v，错误：%v", c.name, time.Since(start).Round(time.Millisecond), err)
		})
	}

	t.Run("客户端证书不是服务端认的CA签的", func(t *testing.T) {
		// mysqld 的 CertificateRequest 里不列 CA，Go 的客户端于是照样出示这张证书，服务端回
		// unknown_ca 告警。go-sql-driver v1.10.1 把它报成 invalid connection（照常重试），
		// 告警本身只进驱动的日志——xgorm 把它接成一条 WARN。
		// 告警和驱动那一读谁先到不固定：实测偶尔报成 driver: bad connection（同样照常重试），两种都认
		var logs strings.Builder
		old := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&syncWriter{w: &logs}, nil)))
		defer slog.SetDefault(old)

		start := time.Now()
		_, _, err := xgorm.New(context.Background(), mysqlTLSConfig(my.DSN(harness.TLSMTLSUser),
			xtls.Config{Enable: true, CAFile: certs.CAFile, CertFile: certs.StrangerCert, KeyFile: certs.StrangerKey}))
		rejected(t, "别的 CA 签的客户端证书", err)
		if msg := err.Error(); !strings.Contains(msg, "invalid connection") && !strings.Contains(msg, "bad connection") {
			t.Errorf("别的 CA 签的客户端证书：错误里该有 invalid connection 或 bad connection，got=%v", err)
		}
		if !strings.Contains(logs.String(), "remote error: tls: unknown certificate authority") {
			t.Errorf("驱动日志里该有服务端的告警，got=%s", logs.String())
		}
		if strings.Contains(logs.String(), harness.TLSPassword) {
			t.Error("日志里带着密码")
		}
		t.Logf("数字：别的 CA 签的客户端证书：用时 %v，%v", time.Since(start).Round(time.Millisecond), err)
	})

	t.Run("没开TLS块也不写tls时驱动走明文", func(t *testing.T) {
		// 量 go-sql-driver v1.10.1 的默认：DSN 里不写 tls 就是明文（不像 pgx 先试 TLS），
		// 于是撞上 require_secure_transport
		_, _, err := xgorm.New(context.Background(), mysqlTLSConfig(my.DSN("xone"), xtls.Config{}))
		rejected(t, "没开 TLS 块", err, "3159")
		t.Logf("数字：没开 TLS 块、DSN 不写 tls：%v", err)
	})
}

// ---- Redis ----

func redisTLSConfig(addr string, tc xtls.Config) xredis.ClientConfig {
	c := xredis.DefaultClientConfig()
	c.Addr, c.Password, c.TLS = addr, harness.TLSPassword, tc
	c.Trace, c.Metric, c.MinIdleConns = false, false, 0
	return c
}

func TestTLS_Redis(t *testing.T) {
	harness.Require(t)
	certs := harness.NewTLSCerts(t)
	rd := harness.StartTLSRedis(t, certs)
	ca := xtls.Config{Enable: true, CAFile: certs.CAFile}
	mtls := xtls.Config{Enable: true, CAFile: certs.CAFile, CertFile: certs.ClientCert, KeyFile: certs.ClientKey}

	t.Run("配置文件里的TLS块生效且服务端只开着TLS端口", func(t *testing.T) {
		named := ca
		named.ServerName = harness.TLSServerName
		xonetest.UseConfigYAML(t, fmt.Sprintf(`XRedis:
  Clients:
    ca:   {Addr: %q, Password: %q, TLS: %s, Trace: false}
    name: {Addr: %q, Password: %q, TLS: %s, Trace: false}
    mtls: {Addr: %q, Password: %q, TLS: %s, Trace: false}
`, rd.Addr, harness.TLSPassword, tlsBlock(ca), rd.Addr, harness.TLSPassword, tlsBlock(named),
			rd.MTLSAddr, harness.TLSPassword, tlsBlock(mtls)))
		xonetest.StartHooks(t)

		ctx := context.Background()
		for _, name := range []string{"ca", "name", "mtls"} {
			c := xredis.C(name)
			if err := c.Set(ctx, "tls-e2e", name, time.Minute).Err(); err != nil {
				t.Fatalf("%s：SET：%v", name, err)
			}
			// 服务端这一侧的证据：明文端口是 0，这条连接连的是 tls-port
			cfg, err := c.ConfigGet(ctx, "*port").Result()
			if err != nil {
				t.Fatalf("%s：CONFIG GET：%v", name, err)
			}
			if cfg["port"] != "0" || !strings.HasSuffix(c.Options().Addr, ":"+cfg["tls-port"]) {
				t.Errorf("%s：服务端应只开着 TLS 端口、而这条连接连的就是它：port=%s tls-port=%s addr=%s",
					name, cfg["port"], cfg["tls-port"], c.Options().Addr)
			}
		}
		info, _ := xredis.C("mtls").Info(ctx, "server").Result()
		for _, l := range strings.Split(info, "\n") {
			if strings.HasPrefix(l, "redis_version:") {
				t.Logf("数字：%s，port=0，只开 tls-port", strings.TrimSpace(l))
			}
		}
	})

	cases := []struct {
		name string
		cfg  xredis.ClientConfig
		want []string
	}{
		{"CA不对", redisTLSConfig(rd.Addr, xtls.Config{Enable: true, CAFile: certs.OtherCAFile}),
			[]string{"certificate signed by unknown authority"}},
		{"ServerName对不上", redisTLSConfig(rd.Addr, xtls.Config{Enable: true, CAFile: certs.CAFile, ServerName: "wrong.e2e.internal"}),
			[]string{"certificate is valid for", "wrong.e2e.internal"}},
		// 下面两条服务端都拒了，但客户端报什么说不准（同 tls_http_test.go）：TLS 1.3 下客户端发完 Finished
		// 就当握手成功、开始写命令，服务端的告警要等下一次读才到。实测报的有时是
		// remote error: tls: certificate required / unknown certificate authority，
		// 有时是 write: connection reset by peer。所以只断言被拒、且归为连不上而不是认证失败
		{"要客户端证书却没带", redisTLSConfig(rd.MTLSAddr, ca), []string{"cannot reach"}},
		{"客户端证书不是服务端认的CA签的", redisTLSConfig(rd.MTLSAddr,
			xtls.Config{Enable: true, CAFile: certs.CAFile, CertFile: certs.StrangerCert, KeyFile: certs.StrangerKey}),
			[]string{"cannot reach"}},
		{"明文", redisTLSConfig(rd.Addr, xtls.Config{}), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start := time.Now()
			_, _, err := xredis.New(context.Background(), c.cfg)
			rejected(t, c.name, err, c.want...)
			t.Logf("数字：%s 用时 %v，错误：%v", c.name, time.Since(start).Round(time.Millisecond), err)
		})
	}
}

// syncWriter 让几个协程一起往同一个 Builder 里写日志
type syncWriter struct {
	mu sync.Mutex
	w  *strings.Builder
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
