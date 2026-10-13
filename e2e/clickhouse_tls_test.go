package e2e

// ClickHouse 的 TLS 端到端测试：TestTLS_ClickHouse。
//
//	scripts/e2e.sh -run 'ClickHouse'     # 或 -run TLS
//
// 服务端是真的 ClickHouse 24.8，但不是 xone-ch：harness 现造一套证书，另起一个开着
// tcp_port_secure / https_port 的容器（见 harness/tls_clickhouse.go），测试结束时删掉。
// 客户端一侧走使用者的路：配置文件里写 TLS 块，跑一遍启动钩子，xgorm.C 取原生 *gorm.DB；
// 连不上的那些直接调 xgorm.New，看它报的错。
//
// 查的三件事同 tls_test.go：CA 对了连得上，而且**服务端**说这条连接是加密的
// （system.query_log 的 is_secure、interface）；CA 不对、名字对不上、明文都被拒，错误说得清；
// 错误里没有密码。native（clickhouse://）和 HTTP（https://）两种协议各走一遍。

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
	"github.com/jupiter-eng/xone/xgorm"
	_ "github.com/jupiter-eng/xone/xgorm/clickhouse" // 注册 Driver: clickhouse
	"github.com/jupiter-eng/xone/xonetest"
	"github.com/jupiter-eng/xone/xtls"
)

func chTLSConfig(dsn string, tc xtls.Config) xgorm.ClientConfig {
	c := xgorm.DefaultClientConfig()
	c.Driver, c.DSN, c.TLS = "clickhouse", dsn, tc
	c.Trace, c.Metric = false, false
	return c
}

// chSecure 在实例 name 上跑一条带记号的查询，再从服务端的 system.query_log 里读出这条查询的
// is_secure、interface（1 = TCP 即 native，2 = HTTP）和登录的账号
func chSecure(t *testing.T, name string) (secure uint8, iface uint8, user string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db := xgorm.CWithCtx(ctx, name)
	mark := "tls-mark-" + harness.NewID()
	var got string
	if err := db.Raw("SELECT '" + mark + "'").Row().Scan(&got); err != nil || got != mark {
		t.Fatalf("%s：查询失败：%v got=%q", name, err, got)
	}
	if err := db.Exec("SYSTEM FLUSH LOGS").Error; err != nil {
		t.Fatalf("%s：SYSTEM FLUSH LOGS：%v", name, err)
	}
	row := db.Raw("SELECT is_secure, interface, user FROM system.query_log "+
		"WHERE type = 'QueryFinish' AND query = ? LIMIT 1", "SELECT '"+mark+"'").Row()
	if err := row.Scan(&secure, &iface, &user); err != nil {
		t.Fatalf("%s：读 system.query_log：%v", name, err)
	}
	return
}

func TestTLS_ClickHouse(t *testing.T) {
	harness.Require(t)
	certs := harness.NewTLSCerts(t)
	ch := harness.StartTLSClickHouse(t, certs)
	ca := xtls.Config{Enable: true, CAFile: certs.CAFile}
	named := ca
	named.ServerName = harness.TLSServerName
	mtls := xtls.Config{Enable: true, CAFile: certs.CAFile, CertFile: certs.ClientCert, KeyFile: certs.ClientKey}

	t.Run("配置文件里的TLS块生效且服务端说连接是加密的", func(t *testing.T) {
		// https:// 不写 secure=true：开了 TLS 块就不必写（写了是冲突）。按 127.0.0.1 连、
		// ServerName 填证书上的域名的那个实例，验的是 ServerName 真的交给了驱动
		xonetest.UseConfigYAML(t, fmt.Sprintf(`XGorm:
  Clients:
    native:      {Driver: clickhouse, DSN: %q, TLS: %s, Trace: false}
    https:       {Driver: clickhouse, DSN: %q, TLS: %s, Trace: false}
    name:        {Driver: clickhouse, DSN: %q, TLS: %s, Trace: false}
    mtls-native: {Driver: clickhouse, DSN: %q, TLS: %s, Trace: false}
    mtls-https:  {Driver: clickhouse, DSN: %q, TLS: %s, Trace: false}
`, ch.DSN("clickhouse", ch.NativeTLS), tlsBlock(ca),
			ch.DSN("https", ch.HTTPS), tlsBlock(ca),
			ch.DSN("clickhouse", ch.NativeTLS), tlsBlock(named),
			ch.MTLSDSN("clickhouse", ch.NativeTLS), tlsBlock(mtls),
			ch.MTLSDSN("https", ch.HTTPS), tlsBlock(mtls)))
		xonetest.StartHooks(t)

		for _, c := range []struct {
			name  string
			iface uint8
			user  string
		}{
			{"native", 1, harness.TLSCHUser},
			{"https", 2, harness.TLSCHUser},
			{"name", 1, harness.TLSCHUser},
			{"mtls-native", 1, harness.TLSMTLSUser},
			{"mtls-https", 2, harness.TLSMTLSUser},
		} {
			secure, iface, user := chSecure(t, c.name)
			if secure != 1 || iface != c.iface || user != c.user {
				t.Errorf("%s：system.query_log 该是 is_secure=1 interface=%d user=%s，got is_secure=%d interface=%d user=%s",
					c.name, c.iface, c.user, secure, iface, user)
			}
			t.Logf("数字：实例 %s：is_secure=%d interface=%d user=%s", c.name, secure, iface, user)
		}
	})

	cases := []struct {
		name string
		cfg  xgorm.ClientConfig
		want []string
	}{
		{"native_CA不对", chTLSConfig(ch.DSN("clickhouse", ch.NativeTLS), xtls.Config{Enable: true, CAFile: certs.OtherCAFile}),
			[]string{"cannot reach", "certificate signed by unknown authority"}},
		{"https_CA不对", chTLSConfig(ch.DSN("https", ch.HTTPS), xtls.Config{Enable: true, CAFile: certs.OtherCAFile}),
			[]string{"cannot reach", "certificate signed by unknown authority"}},
		{"native_不填CA用系统根证书", chTLSConfig(ch.DSN("clickhouse", ch.NativeTLS), xtls.Config{Enable: true}),
			[]string{"certificate signed by unknown authority"}},
		{"native_ServerName对不上", chTLSConfig(ch.DSN("clickhouse", ch.NativeTLS),
			xtls.Config{Enable: true, CAFile: certs.CAFile, ServerName: "wrong.e2e.internal"}),
			[]string{"certificate is valid for", "wrong.e2e.internal"}},
		{"https_ServerName对不上", chTLSConfig(ch.DSN("https", ch.HTTPS),
			xtls.Config{Enable: true, CAFile: certs.CAFile, ServerName: "wrong.e2e.internal"}),
			[]string{"certificate is valid for", "wrong.e2e.internal"}},
		// 证书认证的账号不带客户端证书：服务端当它是密码登录，空密码对不上，516
		{"native_要客户端证书却没带", chTLSConfig(ch.MTLSDSN("clickhouse", ch.NativeTLS), ca),
			[]string{"authentication to", "516"}},
		{"https_要客户端证书却没带", chTLSConfig(ch.MTLSDSN("https", ch.HTTPS), ca),
			[]string{"authentication to", "516"}},
		// 别的 CA 签的客户端证书：relaxed 下带了就校验，握手被拒
		{"native_客户端证书不是服务端认的CA签的", chTLSConfig(ch.MTLSDSN("clickhouse", ch.NativeTLS),
			xtls.Config{Enable: true, CAFile: certs.CAFile, CertFile: certs.StrangerCert, KeyFile: certs.StrangerKey}),
			[]string{"cannot reach", "unknown certificate authority"}},
		{"https_客户端证书不是服务端认的CA签的", chTLSConfig(ch.MTLSDSN("https", ch.HTTPS),
			xtls.Config{Enable: true, CAFile: certs.CAFile, CertFile: certs.StrangerCert, KeyFile: certs.StrangerKey}),
			[]string{"cannot reach", "unknown certificate authority"}},
		// TLS 块开着、连到明文端口：握手失败，不退回明文
		{"native_TLS块开着连明文端口", chTLSConfig(ch.DSN("clickhouse", ch.Native), ca),
			[]string{"cannot reach", "first record does not look like a TLS handshake"}},
		{"https_TLS块开着连明文端口", chTLSConfig(ch.DSN("https", ch.HTTP), ca),
			[]string{"cannot reach", "server gave HTTP response to HTTPS client"}},
		// 不开 TLS 块、明文连到 TLS 端口：服务端回 TLS 告警（0x15），驱动认不出
		{"native_明文连TLS端口", chTLSConfig(ch.DSN("clickhouse", ch.NativeTLS), xtls.Config{}),
			[]string{"cannot reach", "unexpected packet [21]"}},
		{"http_明文连https端口", chTLSConfig(ch.DSN("http", ch.HTTPS), xtls.Config{}),
			[]string{"cannot reach", "malformed HTTP response"}},
		// 两处都说了 TLS、或者 http:// 配 TLS 块：配置错误，一次都不连
		{"TLS块和DSN里的secure同时写", chTLSConfig(ch.DSN("clickhouse", ch.NativeTLS)+"?secure=true", ca),
			[]string{"sets secure", "configure TLS in one place"}},
		{"TLS块和DSN里的skip_verify同时写", chTLSConfig(ch.DSN("https", ch.HTTPS)+"?skip_verify=true", ca),
			[]string{"sets skip_verify", "configure TLS in one place"}},
		{"http配TLS块", chTLSConfig(ch.DSN("http", ch.HTTPS), ca), []string{"http:// never runs TLS"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start := time.Now()
			_, _, err := xgorm.New(context.Background(), c.cfg)
			rejected(t, c.name, err, c.want...)
			t.Logf("数字：%s 用时 %v，错误：%v", c.name, time.Since(start).Round(time.Millisecond), err)
		})
	}

	t.Run("没开TLS块时DSN里的secure照旧生效", func(t *testing.T) {
		// 驱动自己的写法：secure=true 且不写 skip_verify 时按系统根证书校验，自签的过不了；
		// 要么开 TLS 块给 CAFile，要么 skip_verify=true（不校验证书）
		_, _, err := xgorm.New(context.Background(), chTLSConfig(ch.DSN("clickhouse", ch.NativeTLS)+"?secure=true", xtls.Config{}))
		rejected(t, "secure=true", err, "certificate signed by unknown authority")
		db, closer, err := xgorm.New(context.Background(), chTLSConfig(ch.DSN("clickhouse", ch.NativeTLS)+"?secure=true&skip_verify=true", xtls.Config{}))
		if err != nil {
			t.Fatalf("secure=true&skip_verify=true 该连得上（证书不校验）：%v", err)
		}
		defer closer.Close()
		var v string
		if err := db.Raw("SELECT version()").Row().Scan(&v); err != nil {
			t.Fatal(err)
		}
		t.Logf("数字：不开 TLS 块、DSN 写 secure=true&skip_verify=true：连得上，服务端证书没被校验（%s）", v)
	})

	if lines := ch.Log(harness.TLSPassword, 3); len(lines) > 0 {
		t.Errorf("服务端日志里出现了密码：%q", lines)
	}
}
