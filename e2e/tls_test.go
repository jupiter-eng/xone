package e2e

// The TLS end-to-end tests: the TestTLS_* family.
//
//	scripts/e2e.sh -run TLS
//
// The servers are all real, but not the ones running permanently on this host: the harness makes a
// set of certificates and starts a PostgreSQL cluster, a mysqld and redis-servers that take TLS
// alone (see harness/tls.go), which are stopped and cleaned up when the test ends.
// The client side goes the caller's way: a TLS block in the configuration file, one run of the
// start hooks (xonetest), and xgorm.C / xredis.C / xhttp.C for the native client. The cases that
// cannot connect call New directly and read the error it reports.
//
// Each of them checks three things: with the right CA it connects and **the server** says the
// connection is encrypted (pg_stat_ssl, MySQL's Ssl_cipher, Redis having its TLS port alone open);
// a wrong CA, a name that does not match and plaintext are all refused, with an error that says
// what it is; and no password in the error.
//
// How the files divide up:
//
//	tls_test.go       this file: PostgreSQL, MySQL, Redis
//	tls_http_test.go  xhttp against a TLS stub (a self-signed CA plus mutual authentication), and xgin's TLS.ClientCAFile / MinVersion
//	clickhouse_tls_test.go  ClickHouse (a second container in Docker, see harness/tls_clickhouse.go)

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

// tlsBlock is one TLS block in a configuration file (written as a flow, where the indentation does not matter)
func tlsBlock(t xtls.Config) string {
	parts := []string{fmt.Sprintf("Enable: %v", t.Enable)}
	for _, kv := range [][2]string{{"CAFile", t.CAFile}, {"CertFile", t.CertFile}, {"KeyFile", t.KeyFile}, {"ServerName", t.ServerName}} {
		if kv[1] != "" {
			parts = append(parts, fmt.Sprintf("%s: %q", kv[0], kv[1]))
		}
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// noSecret: the error may not hold the password
func noSecret(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), harness.TLSPassword) {
		t.Errorf("the error carries the password: %v", err)
	}
}

// rejected asserts that err is non-nil, says every sentence in want and holds no password; it
// returns how long it took, to record
// untrustedCA is what a client reports when the server's certificate is signed by no CA it trusts,
// written as the two wordings that mean it (rejected takes a want holding wantEither that way).
//
// There is no single string for it. Go's own verifier says "certificate signed by unknown
// authority" (crypto/x509/verify.go). Where the roots are the system's and the platform brings a
// verifier of its own, crypto/x509 hands the check over instead -- "Use platform verifiers, where
// available, if Roots is from SystemCertPool", verify.go -- and macOS then words it
// `x509: "<name>" certificate is not trusted`; crypto/x509/platform_test.go pins that wording,
// with Windows wording it as Go does.
//
// Which of the two a case meets depends on how its client built the tls.Config as much as on the
// platform, so guessing by GOOS is wrong: measured on macOS, xhttp with no TLS block gets the
// platform's wording while the ClickHouse driver with no CAFile gets Go's. The cases that give no
// CA assert the pair. The ones that give a wrong CAFile have roots of their own, always reach Go's
// verifier, and go on saying so literally.
const untrustedCA = "certificate signed by unknown authority" + wantEither + "certificate is not trusted"

// wantEither separates the alternatives inside one want, see rejected
const wantEither = " -or- "

func rejected(t *testing.T, what string, err error, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: it should be refused, and it connected", what)
	}
	for _, w := range want {
		if !holdsAny(err.Error(), strings.Split(w, wantEither)) {
			t.Errorf("%s: the error should hold %q, got=%v", what, w, err)
		}
	}
	noSecret(t, err)
}

// holdsAny reports whether s holds any one of the alternatives
func holdsAny(s string, alternatives []string) bool {
	for _, a := range alternatives {
		if strings.Contains(s, a) {
			return true
		}
	}
	return false
}

// ---- PostgreSQL ----

func pgTLSConfig(dsn string, tc xtls.Config) xgorm.ClientConfig {
	c := xgorm.DefaultClientConfig()
	c.Driver, c.DSN, c.TLS = xgorm.DriverPostgres, dsn, tc
	c.Trace, c.Metric = false, false
	return c
}

// pgSSL is this connection's row in the server's pg_stat_ssl
func pgSSL(t *testing.T, name string) (ssl bool, version, cipher, clientDN string) {
	t.Helper()
	row := xgorm.CWithCtx(context.Background(), name).
		Raw("SELECT ssl, coalesce(version, ''), coalesce(cipher, ''), coalesce(client_dn, '') FROM pg_stat_ssl WHERE pid = pg_backend_pid()").Row()
	if err := row.Scan(&ssl, &version, &cipher, &clientDN); err != nil {
		t.Fatalf("%s: querying pg_stat_ssl: %v", name, err)
	}
	return
}

func TestTLS_PostgreSQL(t *testing.T) {
	harness.Require(t)
	certs := harness.NewTLSCerts(t)
	pg := harness.StartTLSPostgres(t, certs)
	ca := xtls.Config{Enable: true, CAFile: certs.CAFile}
	mtls := xtls.Config{Enable: true, CAFile: certs.CAFile, CertFile: certs.ClientCert, KeyFile: certs.ClientKey}

	t.Run("the TLS block in the configuration file takes and the server says the connection is encrypted", func(t *testing.T) {
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
				t.Errorf("%s: pg_stat_ssl says this connection is not encrypted: ssl=%v version=%q", name, ssl, version)
			}
			if name == "mtls" && !strings.Contains(dn, harness.TLSMTLSUser) {
				t.Errorf("mtls: the server should see the client certificate, client_dn=%q", dn)
			}
			t.Logf("numbers: instance %s: %s %s client_dn=%q", name, version, cipher, dn)
		}
	})

	cases := []struct {
		name string
		cfg  xgorm.ClientConfig
		want []string
	}{
		{"a wrong CA", pgTLSConfig(pg.DSN("postgres"), xtls.Config{Enable: true, CAFile: certs.OtherCAFile}),
			[]string{"cannot reach", "certificate signed by unknown authority"}},
		{"no CA given, the system roots", pgTLSConfig(pg.DSN("postgres"), xtls.Config{Enable: true}),
			[]string{untrustedCA}},
		{"a ServerName mismatch", pgTLSConfig(pg.DSN("postgres"), xtls.Config{Enable: true, CAFile: certs.CAFile, ServerName: "wrong.e2e.internal"}),
			[]string{"certificate is valid for", "wrong.e2e.internal"}},
		// The server wants a client certificate (clientcert=verify-full): it answers 28000, which is handled as an authentication failure
		{"a client certificate is wanted and none is presented", pgTLSConfig(pg.DSN(harness.TLSMTLSUser), ca),
			[]string{"authentication to", "SQLSTATE 28000"}},
		// The server lists the CAs it accepts in the handshake, and Go's client does not present a certificate that none of them signed
		// (crypto/tls picks the certificate by the CertificateRequest): what the server sees is "none presented", and it answers 28000 as well
		{"a client certificate signed by a CA the server does not accept", pgTLSConfig(pg.DSN(harness.TLSMTLSUser),
			xtls.Config{Enable: true, CAFile: certs.CAFile, CertFile: certs.StrangerCert, KeyFile: certs.StrangerKey}),
			[]string{"authentication to", "connection requires a valid client certificate"}},
		// Plaintext: pg_hba holds hostssl alone
		{"plaintext", pgTLSConfig(pg.DSN("postgres")+" sslmode=disable", xtls.Config{}),
			[]string{"no encryption"}},
		// TLS stated in both places: a configuration error, and it never connects
		{"a TLS block and an sslmode in the DSN", pgTLSConfig(pg.DSN("postgres")+" sslmode=require", ca),
			[]string{"sets sslmode", "configure TLS in one place"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start := time.Now()
			_, _, err := xgorm.New(context.Background(), c.cfg)
			rejected(t, c.name, err, c.want...)
			t.Logf("numbers: %s took %v, the error: %v", c.name, time.Since(start).Round(time.Millisecond), err)
		})
	}

	t.Run("with no TLS block it negotiates TLS as pgx does by default, without verifying the certificate", func(t *testing.T) {
		// Measuring pgx's default (sslmode=prefer): with no sslmode in the DSN it still goes through TLS, but connects
		// with a certificate signed by a CA it does not know -- which is the reason to turn the TLS block on (or write verify-full)
		xonetest.UseConfigYAML(t, fmt.Sprintf("XGorm:\n  DSN: %q\n  Trace: false\n", pg.DSN("postgres")))
		xonetest.StartHooks(t)
		ssl, version, _, _ := pgSSL(t, xgorm.DefaultName)
		if !ssl {
			t.Errorf("pgx's default prefer should try TLS first, and pg_stat_ssl says it is not encrypted")
		}
		t.Logf("numbers: no TLS block and no sslmode in the DSN: ssl=%v %s (the server certificate was not verified)", ssl, version)
	})
}

// ---- MySQL ----

func mysqlTLSConfig(dsn string, tc xtls.Config) xgorm.ClientConfig {
	c := xgorm.DefaultClientConfig()
	c.Driver, c.DSN, c.TLS = xgorm.DriverMySQL, dsn, tc
	c.Trace, c.Metric = false, false
	return c
}

// mysqlStatus is one status variable of this session
func mysqlStatus(t *testing.T, name, key string) string {
	t.Helper()
	var k, v string
	// SHOW takes no placeholder (the server reports 1064), and key is a constant the test writes itself
	if err := xgorm.CWithCtx(context.Background(), name).Raw("SHOW SESSION STATUS LIKE '"+key+"'").Row().Scan(&k, &v); err != nil {
		t.Fatalf("%s: SHOW STATUS LIKE %s: %v", name, key, err)
	}
	return v
}

func TestTLS_MySQL(t *testing.T) {
	harness.Require(t)
	certs := harness.NewTLSCerts(t)
	my := harness.StartTLSMySQL(t, certs)
	ca := xtls.Config{Enable: true, CAFile: certs.CAFile}
	mtls := xtls.Config{Enable: true, CAFile: certs.CAFile, CertFile: certs.ClientCert, KeyFile: certs.ClientKey}

	t.Run("the TLS block in the configuration file takes and the server says the connection is encrypted", func(t *testing.T) {
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
				t.Errorf("%s: the server says this connection is not encrypted: Ssl_cipher=%q Ssl_version=%q", name, cipher, version)
			}
			t.Logf("numbers: instance %s: %s %s", name, version, cipher)
		}
		// Every connection in the pool goes through TLS, not the first one alone
		db, _ := xgorm.C("ca").DB()
		db.SetMaxIdleConns(0)
		for i := range 3 {
			if v := mysqlStatus(t, "ca", "Ssl_cipher"); v == "" {
				t.Errorf("new connection %d did not go through TLS", i+2)
			}
		}
	})

	cases := []struct {
		name string
		cfg  xgorm.ClientConfig
		want []string
	}{
		{"a wrong CA", mysqlTLSConfig(my.DSN("xone"), xtls.Config{Enable: true, CAFile: certs.OtherCAFile}),
			[]string{"cannot reach", "certificate signed by unknown authority"}},
		{"a ServerName mismatch", mysqlTLSConfig(my.DSN("xone"), xtls.Config{Enable: true, CAFile: certs.CAFile, ServerName: "wrong.e2e.internal"}),
			[]string{"certificate is valid for", "wrong.e2e.internal"}},
		// An account with REQUIRE X509 presenting no certificate: the server answers 1045
		{"a client certificate is wanted and none is presented", mysqlTLSConfig(my.DSN(harness.TLSMTLSUser), ca),
			[]string{"authentication to", "1045"}},
		// require_secure_transport=ON: the server answers 3159
		{"plaintext", mysqlTLSConfig(my.DSN("xone")+"?tls=false", xtls.Config{}),
			[]string{"3159"}},
		{"a TLS block and a tls in the DSN", mysqlTLSConfig(my.DSN("xone")+"?tls=true", ca),
			[]string{"sets tls", "configure TLS in one place"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start := time.Now()
			_, _, err := xgorm.New(context.Background(), c.cfg)
			rejected(t, c.name, err, c.want...)
			t.Logf("numbers: %s took %v, the error: %v", c.name, time.Since(start).Round(time.Millisecond), err)
		})
	}

	t.Run("a client certificate signed by a CA the server does not accept", func(t *testing.T) {
		// mysqld's CertificateRequest lists no CA, so Go's client presents this certificate all the same and the server answers
		// an unknown_ca alert. go-sql-driver v1.10.1 reports it as invalid connection (and retries as usual),
		// while the alert itself only reaches the driver's log -- which xgorm wires into one WARN.
		// Which of the alert and the driver's read arrives first is not fixed: measured, it is now and then reported as driver: bad connection (retried as usual too), and both are accepted
		var logs strings.Builder
		old := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&syncWriter{w: &logs}, nil)))
		defer slog.SetDefault(old)

		start := time.Now()
		_, _, err := xgorm.New(context.Background(), mysqlTLSConfig(my.DSN(harness.TLSMTLSUser),
			xtls.Config{Enable: true, CAFile: certs.CAFile, CertFile: certs.StrangerCert, KeyFile: certs.StrangerKey}))
		rejected(t, "a client certificate signed by another CA", err)
		if msg := err.Error(); !strings.Contains(msg, "invalid connection") && !strings.Contains(msg, "bad connection") {
			t.Errorf("a client certificate signed by another CA: the error should hold invalid connection or bad connection, got=%v", err)
		}
		if !strings.Contains(logs.String(), "remote error: tls: unknown certificate authority") {
			t.Errorf("the driver's log should hold the server's alert, got=%s", logs.String())
		}
		if strings.Contains(logs.String(), harness.TLSPassword) {
			t.Error("the log carries the password")
		}
		t.Logf("numbers: a client certificate signed by another CA: it took %v, %v", time.Since(start).Round(time.Millisecond), err)
	})

	t.Run("with no TLS block and no tls in the DSN the driver goes plaintext", func(t *testing.T) {
		// Measuring go-sql-driver v1.10.1's default: with no tls in the DSN it is plaintext (unlike pgx, which tries TLS first),
		// and it meets require_secure_transport
		_, _, err := xgorm.New(context.Background(), mysqlTLSConfig(my.DSN("xone"), xtls.Config{}))
		rejected(t, "no TLS block", err, "3159")
		t.Logf("numbers: no TLS block and no tls in the DSN: %v", err)
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

	t.Run("the TLS block in the configuration file takes and the server has its TLS port alone open", func(t *testing.T) {
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
				t.Fatalf("%s: SET: %v", name, err)
			}
			// The evidence on the server's side: the plaintext port is 0 and this connection goes to tls-port
			cfg, err := c.ConfigGet(ctx, "*port").Result()
			if err != nil {
				t.Fatalf("%s: CONFIG GET: %v", name, err)
			}
			if cfg["port"] != "0" || !strings.HasSuffix(c.Options().Addr, ":"+cfg["tls-port"]) {
				t.Errorf("%s: the server should have its TLS port alone open and this connection should go to it: port=%s tls-port=%s addr=%s",
					name, cfg["port"], cfg["tls-port"], c.Options().Addr)
			}
		}
		info, _ := xredis.C("mtls").Info(ctx, "server").Result()
		for _, l := range strings.Split(info, "\n") {
			if strings.HasPrefix(l, "redis_version:") {
				t.Logf("numbers: %s, port=0, tls-port alone open", strings.TrimSpace(l))
			}
		}
	})

	cases := []struct {
		name string
		cfg  xredis.ClientConfig
		want []string
	}{
		{"a wrong CA", redisTLSConfig(rd.Addr, xtls.Config{Enable: true, CAFile: certs.OtherCAFile}),
			[]string{"certificate signed by unknown authority"}},
		{"a ServerName mismatch", redisTLSConfig(rd.Addr, xtls.Config{Enable: true, CAFile: certs.CAFile, ServerName: "wrong.e2e.internal"}),
			[]string{"certificate is valid for", "wrong.e2e.internal"}},
		// The server refuses both of the next two, and what the client reports is not fixed (as in tls_http_test.go): under TLS 1.3 the client
		// takes the handshake for done once it has sent Finished and starts writing commands, so the server's alert arrives at the next read. Measured, it reports
		// sometimes remote error: tls: certificate required / unknown certificate authority and
		// sometimes write: connection reset by peer. So only the refusal is asserted, and as a failure to connect rather than an authentication failure
		{"a client certificate is wanted and none is presented", redisTLSConfig(rd.MTLSAddr, ca), []string{"cannot reach"}},
		{"a client certificate signed by a CA the server does not accept", redisTLSConfig(rd.MTLSAddr,
			xtls.Config{Enable: true, CAFile: certs.CAFile, CertFile: certs.StrangerCert, KeyFile: certs.StrangerKey}),
			[]string{"cannot reach"}},
		{"plaintext", redisTLSConfig(rd.Addr, xtls.Config{}), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start := time.Now()
			_, _, err := xredis.New(context.Background(), c.cfg)
			rejected(t, c.name, err, c.want...)
			t.Logf("numbers: %s took %v, the error: %v", c.name, time.Since(start).Round(time.Millisecond), err)
		})
	}
}

// syncWriter lets several goroutines write log lines into one Builder together
type syncWriter struct {
	mu sync.Mutex
	w  *strings.Builder
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
