package xgorm

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/jupiter-eng/xone/xtls"
)

// ---- certificates used by the tests ----

// testPKI is a freshly built CA together with the server certificate it signs (127.0.0.1 / db.internal) and a client certificate.
type testPKI struct {
	caFile, certFile, keyFile string
	server                    tls.Certificate
	pool                      *x509.CertPool
}

func newPKI(t *testing.T) testPKI {
	t.Helper()
	dir := t.TempDir()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "xgorm-test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	issue := func(serial int64, usage x509.ExtKeyUsage) ([]byte, *ecdsa.PrivateKey) {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "db.internal"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			DNSNames: []string{"db.internal"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
			ExtKeyUsage: []x509.ExtKeyUsage{usage}, KeyUsage: x509.KeyUsageDigitalSignature,
		}
		der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		return der, key
	}
	write := func(name, typ string, der []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	srvDER, srvKey := issue(2, x509.ExtKeyUsageServerAuth)
	cliDER, cliKey := issue(3, x509.ExtKeyUsageClientAuth)
	cliKeyDER, _ := x509.MarshalECPrivateKey(cliKey)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return testPKI{
		caFile:   write("ca.pem", "CERTIFICATE", caDER),
		certFile: write("client.pem", "CERTIFICATE", cliDER),
		keyFile:  write("client-key.pem", "EC PRIVATE KEY", cliKeyDER),
		server:   tls.Certificate{Certificate: [][]byte{srvDER}, PrivateKey: srvKey},
		pool:     pool,
	}
}

// seen records what the fake server observed, per connection.
type seen struct {
	mu        sync.Mutex
	tlsConns  int  // TLS connections that completed the handshake
	plaintext bool // a plaintext login packet was received
	clientCrt bool // the client presented a certificate
}

func (s *seen) get() (int, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tlsConns, s.plaintext, s.clientCrt
}

func (s *seen) handshook(c *tls.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tlsConns++
	s.clientCrt = s.clientCrt || len(c.ConnectionState().PeerCertificates) > 0
}

func (s *seen) sawPlaintext() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.plaintext = true
}

func serve(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				handle(c)
			}()
		}
	}()
	return ln.Addr().String()
}

// fakePostgres is a PostgreSQL that only handles login and replies to Ping. With srv nil it refuses TLS (replies 'N').
func fakePostgres(t *testing.T, srv *tls.Config) (string, *seen) {
	s := &seen{}
	addr := serve(t, func(c net.Conn) {
		var conn net.Conn = c
		be := pgproto3.NewBackend(conn, conn)
		msg, err := be.ReceiveStartupMessage()
		if err != nil {
			return
		}
		if _, ok := msg.(*pgproto3.SSLRequest); ok {
			if srv == nil {
				if _, err := c.Write([]byte("N")); err != nil {
					return
				}
				if m, err := be.ReceiveStartupMessage(); err == nil {
					if _, ok := m.(*pgproto3.StartupMessage); ok {
						s.sawPlaintext()
					}
				}
				return
			}
			if _, err := c.Write([]byte("S")); err != nil {
				return
			}
			tc := tls.Server(c, srv)
			if err := tc.Handshake(); err != nil {
				return
			}
			s.handshook(tc)
			conn = tc
			be = pgproto3.NewBackend(conn, conn)
			if msg, err = be.ReceiveStartupMessage(); err != nil {
				return
			}
		} else {
			s.sawPlaintext()
		}
		if _, ok := msg.(*pgproto3.StartupMessage); !ok {
			return
		}
		be.Send(&pgproto3.AuthenticationOk{})
		be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		if be.Flush() != nil {
			return
		}
		for {
			m, err := be.Receive()
			if err != nil {
				return
			}
			switch m.(type) {
			case *pgproto3.Query:
				be.Send(&pgproto3.EmptyQueryResponse{})
				be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
				if be.Flush() != nil {
					return
				}
			case *pgproto3.Terminate:
				return
			}
		}
	})
	return addr, s
}

func pgTLSCfg(addr string, t xtls.Config) ClientConfig {
	host, port, _ := net.SplitHostPort(addr)
	c := pgCfg("host=" + host + " port=" + port + " user=u password=" + secret + " dbname=d")
	c.TLS = t
	c.Trace, c.Metric = false, false
	return c
}

func TestNew_PG_TLSBlockApplies(t *testing.T) {
	p := newPKI(t)
	cases := []struct {
		name string
		tls  xtls.Config
		ok   bool
		want string // what the error must say on failure
	}{
		{"CAFile recognizes the server certificate", xtls.Config{Enable: true, CAFile: p.caFile}, true, ""},
		{"ServerName set to the domain on the certificate", xtls.Config{Enable: true, CAFile: p.caFile, ServerName: "db.internal"}, true, ""},
		{"without CAFile the system roots are used and a self-signed certificate fails", xtls.Config{Enable: true}, false, "certificate"},
		{"ServerName does not match the certificate", xtls.Config{Enable: true, CAFile: p.caFile, ServerName: "other.internal"}, false, "certificate"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			addr, s := fakePostgres(t, &tls.Config{Certificates: []tls.Certificate{p.server}})
			logs := capture(t)
			db, closer, err := New(context.Background(), pgTLSCfg(addr, c.tls))
			if c.ok != (err == nil) {
				t.Fatalf("want ok=%v, got err=%v", c.ok, err)
			}
			if err != nil {
				if !strings.Contains(err.Error(), c.want) {
					t.Errorf("the error must say %q, got=%v", c.want, err)
				}
				if strings.Contains(err.Error(), secret) {
					t.Errorf("the error must not contain the password: %v", err)
				}
				return
			}
			defer closer.Close()
			if err := db.Exec(";").Error; err != nil {
				t.Errorf("the connection must be usable once established: %v", err)
			}
			if n, plain, _ := s.get(); n == 0 || plain {
				t.Errorf("the server must have seen TLS connections only, tls=%d plaintext=%v", n, plain)
			}
			found := false
			for _, l := range logs() {
				if l["msg"] == "xgorm connected" {
					found = found || l["tls"] == true
				}
			}
			if !found {
				t.Errorf("the connect log must record tls=true, got=%v", logs())
			}
		})
	}
}

func TestNew_PG_TLSBlockFailsInsteadOfPlaintextFallback(t *testing.T) {
	// pgx's default sslmode=prefer switches to plaintext when the server replies 'N'; with a TLS block configured that is not allowed.
	p := newPKI(t)
	addr, s := fakePostgres(t, nil)
	_, _, err := New(context.Background(), pgTLSCfg(addr, xtls.Config{Enable: true, CAFile: p.caFile}))
	if err == nil || !strings.Contains(err.Error(), "refused TLS") {
		t.Fatalf("when the server refuses TLS the error must be server refused TLS connection, got=%v", err)
	}
	if _, plain, _ := s.get(); plain {
		t.Error("must not fall back to sending the login packet in plaintext")
	}
}

func TestNew_PG_WithoutTLSBlockFollowsPgxPreferDefault(t *testing.T) {
	// This measures pgx's own default (sslmode=prefer): with no TLS block and no sslmode in the DSN,
	// a server replying 'N' makes it switch to plaintext -- which is the reason the TLS block exists.
	addr, s := fakePostgres(t, nil)
	_, closer, err := New(context.Background(), pgTLSCfg(addr, xtls.Config{}))
	if err != nil {
		t.Fatalf("pgx's default falls back to plaintext, so the connection must succeed: %v", err)
	}
	closer.Close()
	if _, plain, _ := s.get(); !plain {
		t.Error("the connection must have been established in plaintext")
	}
}

func TestNew_PG_MutualAuthSendsClientCert(t *testing.T) {
	p := newPKI(t)
	addr, s := fakePostgres(t, &tls.Config{
		Certificates: []tls.Certificate{p.server},
		ClientAuth:   tls.RequireAndVerifyClientCert, ClientCAs: p.pool,
	})
	if _, _, err := New(context.Background(), pgTLSCfg(addr, xtls.Config{Enable: true, CAFile: p.caFile})); err == nil {
		t.Fatal("when the server requires a client certificate, connecting without one must fail")
	}
	_, closer, err := New(context.Background(), pgTLSCfg(addr, xtls.Config{Enable: true, CAFile: p.caFile, CertFile: p.certFile, KeyFile: p.keyFile}))
	if err != nil {
		t.Fatalf("connecting with a client certificate must succeed: %v", err)
	}
	closer.Close()
	if _, _, crt := s.get(); !crt {
		t.Error("the server must have seen the client certificate")
	}
}

func TestUsePostgresTLS_OnePerHostAllTLS(t *testing.T) {
	base := &tls.Config{MinVersion: tls.VersionTLS12}
	for _, dsn := range []string{
		"host=a.internal,b.internal port=5432,5433 user=u",                 // default prefer: TLS plus plaintext for every host
		"host=a.internal,b.internal port=5432,5433 user=u sslmode=allow",   // allow: plaintext first
		"host=a.internal,b.internal port=5432,5433 user=u sslmode=disable", // plaintext only
	} {
		pc, err := pgconn.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		usePostgresTLS(pc, base)
		hosts := append([]*pgconn.FallbackConfig{{Host: pc.Host, Port: pc.Port, TLSConfig: pc.TLSConfig}}, pc.Fallbacks...)
		if len(hosts) != 2 {
			t.Fatalf("%s: two hosts must remain, got=%d", dsn, len(hosts))
		}
		for i, want := range []string{"a.internal", "b.internal"} {
			h := hosts[i]
			if h.Host != want || h.TLSConfig == nil || h.TLSConfig.InsecureSkipVerify || h.TLSConfig.ServerName != want {
				t.Errorf("%s: entry %d must use TLS verified against %s for %s, got host=%s tls=%+v", dsn, i, want, want, h.Host, h.TLSConfig)
			}
		}
	}

	pc, _ := pgconn.ParseConfig("host=10.0.0.1 user=u")
	usePostgresTLS(pc, &tls.Config{ServerName: "db.internal"})
	if pc.TLSConfig.ServerName != "db.internal" || len(pc.Fallbacks) != 0 {
		t.Errorf("a configured ServerName must be what is verified against, got=%q fallbacks=%d", pc.TLSConfig.ServerName, len(pc.Fallbacks))
	}
}

func TestResolveDSN_PG_TLSBlockConflictsWithDSNSSLParams(t *testing.T) {
	on := xtls.Config{Enable: true}
	for name, c := range map[string]struct {
		dsn    string
		params map[string]string
		key    string
	}{
		"sslmode in KV form":                {dsn: "host=h user=u password=" + secret + " sslmode=disable", key: "sslmode"},
		"sslmode in URL form":               {dsn: "postgres://u:" + secret + "@h/d?sslmode=require", key: "sslmode"},
		"sslrootcert":                       {dsn: "host=h user=u sslrootcert=/etc/ca.pem", key: "sslrootcert"},
		"sslnegotiation":                    {dsn: "host=h user=u sslnegotiation=direct", key: "sslnegotiation"},
		"set in Params counts too":          {dsn: "host=h user=u", params: map[string]string{"sslmode": "disable"}, key: "sslmode"},
		"recognized after other parameters": {dsn: "host=h user=u application_name=x TimeZone=UTC sslmode=verify-full", key: "sslmode"},
	} {
		cfg := pgCfg(c.dsn)
		cfg.Postgres.Params = c.params
		cfg.TLS = on
		_, _, err := resolveDSN(cfg)
		if err == nil || !strings.Contains(err.Error(), "sets "+c.key+" ") {
			t.Errorf("%s: the %s conflict must be reported, got=%v", name, c.key, err)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: the error must not contain the password: %v", name, err)
		}
	}

	for name, dsn := range map[string]string{
		"no ssl parameter":                       "host=h user=u application_name=x",
		"the password contains the text sslmode": "host=h user=u password='a sslmode=disable b'",
		"a URL password contains sslmode":        "postgres://u:sslmode%3Ddisable@h/d?application_name=x",
	} {
		cfg := pgCfg(dsn)
		cfg.TLS = on
		if _, _, err := resolveDSN(cfg); err != nil {
			t.Errorf("%s: no conflict must be reported, got=%v", name, err)
		}
	}

	// Without a TLS block the sslmode in the DSN keeps working as before.
	if _, _, err := resolveDSN(pgCfg("host=h user=u sslmode=verify-full")); err != nil {
		t.Errorf("without a TLS block sslmode may still be written: %v", err)
	}
}

func TestResolveDSN_PG_TLSBlockRejectsUnixSocket(t *testing.T) {
	cfg := pgCfg("host=/var/run/postgresql user=u")
	cfg.TLS = xtls.Config{Enable: true}
	if _, _, err := resolveDSN(cfg); err == nil || !strings.Contains(err.Error(), "Unix socket") {
		t.Fatalf("the error must say there is no TLS on a Unix socket, got=%v", err)
	}
}

// ---- MySQL ----

// fakeMySQL is a MySQL that only performs the handshake: it sends the greeting, receives the login packet after the TLS upgrade, and replies 1045 so the client stops retrying.
// With srv nil the greeting does not carry CLIENT_SSL.
func fakeMySQL(t *testing.T, srv *tls.Config) (string, *seen) {
	s := &seen{}
	addr := serve(t, func(c net.Conn) {
		caps := uint32(0x1 | 0x8 | 0x200 | 0x2000 | 0x8000 | 0x80000) // LONG_PASSWORD CONNECT_WITH_DB PROTOCOL_41 TRANSACTIONS SECURE_CONNECTION PLUGIN_AUTH
		if srv != nil {
			caps |= 0x800 // CLIENT_SSL
		}
		g := []byte{10}
		g = append(g, "8.0.46-fake\x00"...)
		g = binary.LittleEndian.AppendUint32(g, 1)
		g = append(g, "abcdefgh\x00"...)
		g = binary.LittleEndian.AppendUint16(g, uint16(caps))
		g = append(g, 0xff)
		g = binary.LittleEndian.AppendUint16(g, 2)
		g = binary.LittleEndian.AppendUint16(g, uint16(caps>>16))
		g = append(g, 21)
		g = append(g, make([]byte, 10)...)
		g = append(g, "ijklmnopqrst\x00"...)
		g = append(g, "mysql_native_password\x00"...)
		if writePacket(c, 0, g) != nil {
			return
		}
		var conn net.Conn = c
		pkt, err := readPacket(conn)
		if err != nil {
			return
		}
		if srv != nil && len(pkt) == 32 && binary.LittleEndian.Uint32(pkt)&0x800 != 0 {
			tc := tls.Server(c, srv)
			if tc.Handshake() != nil {
				return
			}
			s.handshook(tc)
			conn = tc
			if _, err := readPacket(conn); err != nil {
				return
			}
		} else {
			s.sawPlaintext()
		}
		e := []byte{0xff}
		e = binary.LittleEndian.AppendUint16(e, 1045)
		e = append(e, "#28000Access denied for user 'u'"...)
		_ = writePacket(conn, 3, e)
	})
	return addr, s
}

func writePacket(w io.Writer, seq byte, payload []byte) error {
	n := len(payload)
	_, err := w.Write(append([]byte{byte(n), byte(n >> 8), byte(n >> 16), seq}, payload...))
	return err
}

func readPacket(r io.Reader) ([]byte, error) {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	b := make([]byte, int(h[0])|int(h[1])<<8|int(h[2])<<16)
	_, err := io.ReadFull(r, b)
	return b, err
}

func mysqlTLSCfg(addr string, t xtls.Config) ClientConfig {
	c := mysqlCfg("u:" + secret + "@tcp(" + addr + ")/d")
	c.TLS = t
	c.Trace, c.Metric = false, false
	return c
}

func TestNew_MySQL_TLSBlockApplies(t *testing.T) {
	p := newPKI(t)
	cases := []struct {
		name   string
		tls    xtls.Config
		viaTLS bool   // the server must see the login packet arrive over TLS
		want   string // what the error must say
	}{
		// The fake server replies 1045 after the login packet: getting this far means the TLS handshake passed.
		{"CAFile recognizes the server certificate", xtls.Config{Enable: true, CAFile: p.caFile}, true, "authentication to "},
		{"ServerName set to the domain on the certificate", xtls.Config{Enable: true, CAFile: p.caFile, ServerName: "db.internal"}, true, "authentication to "},
		{"without CAFile the system roots are used and a self-signed certificate fails", xtls.Config{Enable: true}, false, "certificate"},
		{"ServerName does not match the certificate", xtls.Config{Enable: true, CAFile: p.caFile, ServerName: "other.internal"}, false, "certificate"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			addr, s := fakeMySQL(t, &tls.Config{Certificates: []tls.Certificate{p.server}})
			_, _, err := New(context.Background(), mysqlTLSCfg(addr, c.tls))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("the error must say %q, got=%v", c.want, err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("the error must not contain the password: %v", err)
			}
			n, plain, _ := s.get()
			if plain {
				t.Error("the login packet must not be sent in plaintext")
			}
			if c.viaTLS != (n > 0) {
				t.Errorf("TLS connections completing the handshake at the server want>0=%v, got=%d", c.viaTLS, n)
			}
		})
	}
}

func TestNew_MySQL_TLSBlockFailsWhenServerLacksTLS(t *testing.T) {
	p := newPKI(t)
	addr, s := fakeMySQL(t, nil)
	_, _, err := New(context.Background(), mysqlTLSCfg(addr, xtls.Config{Enable: true, CAFile: p.caFile}))
	if err == nil || !strings.Contains(err.Error(), "TLS") {
		t.Fatalf("connecting must fail when the server does not support TLS, got=%v", err)
	}
	if _, plain, _ := s.get(); plain {
		t.Error("must not fall back to sending the login packet in plaintext")
	}
}

// A greeting without CLIENT_SSL can mean a server that really does not support TLS, or a man in the middle that cleared the bit.
// With allowFallbackToPlaintext=true in the DSN, go-sql-driver v1.10.1 then drops TLS and sends the
// login packet in plaintext (packets.go readHandshakePacket) -- even with a TLS block configured.
func TestNew_MySQL_TLSBlockRejectsPlaintextFallbackParam(t *testing.T) {
	p := newPKI(t)
	addr, s := fakeMySQL(t, nil)
	cfg := mysqlTLSCfg(addr, xtls.Config{Enable: true, CAFile: p.caFile})
	cfg.DSN += "?allowFallbackToPlaintext=true"
	_, _, err := New(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "sets allowFallbackToPlaintext") {
		t.Fatalf("the conflict between the DSN and the TLS block must be reported, got=%v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the error must not contain the password: %v", err)
	}
	if _, plain, _ := s.get(); plain {
		t.Error("must not fall back to sending the login packet in plaintext")
	}
}

// Last line of defence: even if the validation step missed it, the connection config handed to the driver must not allow a plaintext fallback.
func TestOpenMySQLTLS_NeverFallsBackToPlaintext(t *testing.T) {
	p := newPKI(t)
	addr, s := fakeMySQL(t, nil)
	tc, err := xtls.Config{Enable: true, CAFile: p.caFile}.Build()
	if err != nil {
		t.Fatal(err)
	}
	d, err := openMySQLTLS("u:"+secret+"@tcp("+addr+")/d?allowFallbackToPlaintext=true&timeout=2s", tc)
	if err != nil {
		t.Fatal(err)
	}
	// gorm.Open pings once by itself (gorm v1.31 Open); that is where the connection happens.
	db, err := gorm.Open(d, &gorm.Config{Logger: logger.Discard})
	if err == nil {
		sqlDB, _ := db.DB()
		_ = sqlDB.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "TLS") {
		t.Errorf("connecting must fail when the server refuses TLS, got=%v", err)
	}
	if _, plain, _ := s.get(); plain {
		t.Error("must not fall back to sending the login packet in plaintext")
	}
}

func TestNew_MySQL_MutualAuthSendsClientCert(t *testing.T) {
	p := newPKI(t)
	addr, s := fakeMySQL(t, &tls.Config{
		Certificates: []tls.Certificate{p.server},
		ClientAuth:   tls.RequireAndVerifyClientCert, ClientCAs: p.pool,
	})
	_, _, err := New(context.Background(), mysqlTLSCfg(addr, xtls.Config{Enable: true, CAFile: p.caFile, CertFile: p.certFile, KeyFile: p.keyFile}))
	if err == nil || !strings.Contains(err.Error(), "authentication to ") {
		t.Fatalf("the auth failure is only reported once the handshake has passed, got=%v", err)
	}
	if _, _, crt := s.get(); !crt {
		t.Error("the server must have seen the client certificate")
	}
}

func TestResolveDSN_MySQL_TLSBlockConflictsWithDSNTLS(t *testing.T) {
	for _, v := range []string{"true", "false", "skip-verify", "preferred"} {
		cfg := mysqlCfg("u:" + secret + "@tcp(h:3306)/d?tls=" + v)
		cfg.TLS = xtls.Config{Enable: true}
		_, _, err := resolveDSN(cfg)
		if err == nil || !strings.Contains(err.Error(), "sets tls") {
			t.Errorf("tls=%s: a conflict must be reported, got=%v", v, err)
		}
	}
	// A ?tls=false inside the password does not fool it: the password ends before the last /.
	cfg := mysqlCfg("u:x?tls=false@tcp(h:3306)/d")
	cfg.TLS = xtls.Config{Enable: true}
	if _, _, err := resolveDSN(cfg); err != nil {
		t.Errorf("a tls= inside the password does not count, got=%v", err)
	}
	// Without a TLS block the tls option in the DSN keeps working as before.
	if _, _, err := resolveDSN(mysqlCfg("u:p@tcp(h:3306)/d?tls=true")); err != nil {
		t.Errorf("without a TLS block tls= may still be written: %v", err)
	}

	cfg = mysqlCfg("u:p@unix(/run/mysqld/mysqld.sock)/d")
	cfg.TLS = xtls.Config{Enable: true}
	if _, _, err := resolveDSN(cfg); err == nil || !strings.Contains(err.Error(), "only runs over TCP") {
		t.Errorf("there is no TLS on a Unix socket, got=%v", err)
	}
}

// ---- configuration ----

func TestValidate_TLSBlock(t *testing.T) {
	cfg := pgCfg("host=h")
	cfg.TLS = xtls.Config{CAFile: "/etc/ca.pem"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "TLS.Enable") {
		t.Errorf("a CAFile without TLS enabled must be rejected, got=%v", err)
	}

	withDialect(t, Dialect{Name: "notls", Open: openPostgres})
	cfg = DefaultClientConfig()
	cfg.Driver, cfg.DSN = "notls", "x"
	cfg.TLS = xtls.Config{Enable: true}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "does not support the TLS block") {
		t.Errorf("a TLS block must be rejected when the dialect has no OpenTLS, got=%v", err)
	}
}

func TestNew_TLSUnreadableCertIsConfigError(t *testing.T) {
	cfg := pgTLSCfg(deadAddr(t), xtls.Config{Enable: true, CAFile: filepath.Join(t.TempDir(), "nope.pem")})
	_, _, err := New(context.Background(), cfg)
	assertOneFrame(t, err, "xgorm", "config")
	if !strings.Contains(err.Error(), "TLS.CAFile") {
		t.Errorf("the error must name the file, got=%v", err)
	}
}

func TestConfig_TLSBlockFromConfigFile(t *testing.T) {
	p := newPKI(t)
	c := load(t, "XGorm:\n  DSN: host=h\n  TLS:\n    Enable: true\n    CAFile: "+strconv.Quote(p.caFile)+"\n    ServerName: db.internal\n")
	got := c.Clients[DefaultName].TLS
	if !got.Enable || got.CAFile != p.caFile || got.ServerName != "db.internal" {
		t.Errorf("the TLS block was not read correctly: %+v", got)
	}
	if err := loadErr(t, "XGorm:\n  DSN: host=h\n  TLS:\n    CAFile: /etc/ca.pem\n"); err == nil {
		t.Error("a CAFile without TLS enabled must already fail when the config is read")
	}
}
