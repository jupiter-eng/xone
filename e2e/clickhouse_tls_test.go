package e2e

// The ClickHouse TLS end-to-end tests: TestTLS_ClickHouse.
//
//	scripts/e2e.sh -run 'ClickHouse'     # or -run TLS
//
// The server is a real ClickHouse 24.8, but not xone-ch: the harness makes a set of certificates
// and starts a second container with tcp_port_secure / https_port open (see
// harness/tls_clickhouse.go), which is removed when the test ends.
// The client side goes the caller's way: a TLS block in the configuration file, one run of the
// start hooks, and xgorm.C for the native *gorm.DB; the cases that cannot connect call xgorm.New
// directly and read the error it reports.
//
// The three things it checks are the ones tls_test.go checks: with the right CA it connects and
// **the server** says the connection is encrypted (is_secure and interface in system.query_log);
// a wrong CA, a name that does not match and plaintext are all refused, with an error that says
// so; and no password in the error. It goes once over native (clickhouse://) and once over HTTP
// (https://).

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
	"github.com/jupiter-eng/xone/xgorm"
	_ "github.com/jupiter-eng/xone/xgorm/clickhouse" // registers Driver: clickhouse
	"github.com/jupiter-eng/xone/xonetest"
	"github.com/jupiter-eng/xone/xtls"
)

func chTLSConfig(dsn string, tc xtls.Config) xgorm.ClientConfig {
	c := xgorm.DefaultClientConfig()
	c.Driver, c.DSN, c.TLS = "clickhouse", dsn, tc
	c.Trace, c.Metric = false, false
	return c
}

// chSecure runs one marked query on the instance name and then reads that query's is_secure,
// interface (1 = TCP, that is native, 2 = HTTP) and the account it logged in as out of the
// server's system.query_log
func chSecure(t *testing.T, name string) (secure uint8, iface uint8, user string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db := xgorm.CWithCtx(ctx, name)
	mark := "tls-mark-" + harness.NewID()
	var got string
	if err := db.Raw("SELECT '" + mark + "'").Row().Scan(&got); err != nil || got != mark {
		t.Fatalf("%s: the query failed: %v got=%q", name, err, got)
	}
	if err := db.Exec("SYSTEM FLUSH LOGS").Error; err != nil {
		t.Fatalf("%s: SYSTEM FLUSH LOGS: %v", name, err)
	}
	row := db.Raw("SELECT is_secure, interface, user FROM system.query_log "+
		"WHERE type = 'QueryFinish' AND query = ? LIMIT 1", "SELECT '"+mark+"'").Row()
	if err := row.Scan(&secure, &iface, &user); err != nil {
		t.Fatalf("%s: reading system.query_log: %v", name, err)
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

	t.Run("the TLS block in the configuration file takes and the server says the connection is encrypted", func(t *testing.T) {
		// https:// does not write secure=true: with a TLS block on it is unnecessary (and writing it is a conflict). The
		// instance connecting to 127.0.0.1 with ServerName set to the name on the certificate is what proves ServerName really reaches the driver
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
				t.Errorf("%s: system.query_log should say is_secure=1 interface=%d user=%s, got is_secure=%d interface=%d user=%s",
					c.name, c.iface, c.user, secure, iface, user)
			}
			t.Logf("numbers: instance %s: is_secure=%d interface=%d user=%s", c.name, secure, iface, user)
		}
	})

	cases := []struct {
		name string
		cfg  xgorm.ClientConfig
		want []string
	}{
		{"native_wrong_CA", chTLSConfig(ch.DSN("clickhouse", ch.NativeTLS), xtls.Config{Enable: true, CAFile: certs.OtherCAFile}),
			[]string{"cannot reach", "certificate signed by unknown authority"}},
		{"https_wrong_CA", chTLSConfig(ch.DSN("https", ch.HTTPS), xtls.Config{Enable: true, CAFile: certs.OtherCAFile}),
			[]string{"cannot reach", "certificate signed by unknown authority"}},
		{"native_no_CA_system_roots", chTLSConfig(ch.DSN("clickhouse", ch.NativeTLS), xtls.Config{Enable: true}),
			[]string{untrustedCA}},
		{"native_ServerName_mismatch", chTLSConfig(ch.DSN("clickhouse", ch.NativeTLS),
			xtls.Config{Enable: true, CAFile: certs.CAFile, ServerName: "wrong.e2e.internal"}),
			[]string{"certificate is valid for", "wrong.e2e.internal"}},
		{"https_ServerName_mismatch", chTLSConfig(ch.DSN("https", ch.HTTPS),
			xtls.Config{Enable: true, CAFile: certs.CAFile, ServerName: "wrong.e2e.internal"}),
			[]string{"certificate is valid for", "wrong.e2e.internal"}},
		// An account authenticating by certificate that presents none: the server takes it for a password login, the empty password does not match, 516
		{"native_client_cert_wanted_but_absent", chTLSConfig(ch.MTLSDSN("clickhouse", ch.NativeTLS), ca),
			[]string{"authentication to", "516"}},
		{"https_client_cert_wanted_but_absent", chTLSConfig(ch.MTLSDSN("https", ch.HTTPS), ca),
			[]string{"authentication to", "516"}},
		// A client certificate signed by another CA: under relaxed one that is presented is verified, and the handshake is refused
		{"native_client_cert_from_another_CA", chTLSConfig(ch.MTLSDSN("clickhouse", ch.NativeTLS),
			xtls.Config{Enable: true, CAFile: certs.CAFile, CertFile: certs.StrangerCert, KeyFile: certs.StrangerKey}),
			[]string{"cannot reach", "unknown certificate authority"}},
		{"https_client_cert_from_another_CA", chTLSConfig(ch.MTLSDSN("https", ch.HTTPS),
			xtls.Config{Enable: true, CAFile: certs.CAFile, CertFile: certs.StrangerCert, KeyFile: certs.StrangerKey}),
			[]string{"cannot reach", "unknown certificate authority"}},
		// A TLS block on, connecting to the plaintext port: the handshake fails and it does not fall back to plaintext
		{"native_TLS_block_to_plaintext_port", chTLSConfig(ch.DSN("clickhouse", ch.Native), ca),
			[]string{"cannot reach", "first record does not look like a TLS handshake"}},
		{"https_TLS_block_to_plaintext_port", chTLSConfig(ch.DSN("https", ch.HTTP), ca),
			[]string{"cannot reach", "server gave HTTP response to HTTPS client"}},
		// No TLS block, connecting in plaintext to a TLS port: the server answers a TLS alert (0x15), which the driver does not recognize
		{"native_plaintext_to_TLS_port", chTLSConfig(ch.DSN("clickhouse", ch.NativeTLS), xtls.Config{}),
			[]string{"cannot reach", "unexpected packet [21]"}},
		{"http_plaintext_to_https_port", chTLSConfig(ch.DSN("http", ch.HTTPS), xtls.Config{}),
			[]string{"cannot reach", "malformed HTTP response"}},
		// TLS stated in both places, or a TLS block on http://: a configuration error, and it never connects
		{"TLS_block_and_secure_in_the_DSN", chTLSConfig(ch.DSN("clickhouse", ch.NativeTLS)+"?secure=true", ca),
			[]string{"sets secure", "configure TLS in one place"}},
		{"TLS_block_and_skip_verify_in_the_DSN", chTLSConfig(ch.DSN("https", ch.HTTPS)+"?skip_verify=true", ca),
			[]string{"sets skip_verify", "configure TLS in one place"}},
		{"http_with_a_TLS_block", chTLSConfig(ch.DSN("http", ch.HTTPS), ca), []string{"http:// never runs TLS"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start := time.Now()
			_, _, err := xgorm.New(context.Background(), c.cfg)
			rejected(t, c.name, err, c.want...)
			t.Logf("numbers: %s took %v, the error: %v", c.name, time.Since(start).Round(time.Millisecond), err)
		})
	}

	t.Run("secure in the DSN still takes with no TLS block", func(t *testing.T) {
		// The driver's own way: with secure=true and no skip_verify it verifies against the system roots, which a self-signed certificate does not pass;
		// either turn the TLS block on and give CAFile, or write skip_verify=true (no certificate verification)
		_, _, err := xgorm.New(context.Background(), chTLSConfig(ch.DSN("clickhouse", ch.NativeTLS)+"?secure=true", xtls.Config{}))
		rejected(t, "secure=true", err, untrustedCA)
		db, closer, err := xgorm.New(context.Background(), chTLSConfig(ch.DSN("clickhouse", ch.NativeTLS)+"?secure=true&skip_verify=true", xtls.Config{}))
		if err != nil {
			t.Fatalf("secure=true&skip_verify=true should connect (the certificate is not verified): %v", err)
		}
		defer closer.Close()
		var v string
		if err := db.Raw("SELECT version()").Row().Scan(&v); err != nil {
			t.Fatal(err)
		}
		t.Logf("numbers: no TLS block with secure=true&skip_verify=true in the DSN: it connects and the server certificate was not verified (%s)", v)
	})

	if lines := ch.Log(harness.TLSPassword, 3); len(lines) > 0 {
		t.Errorf("the password turned up in the server log: %q", lines)
	}
}
