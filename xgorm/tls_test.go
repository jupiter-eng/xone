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

// ---- 测试用的证书 ----

// testPKI 现造的一个 CA，和它签的服务端证书（127.0.0.1 / db.internal）、客户端证书
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

// seen 假服务端看到的事，按连接记
type seen struct {
	mu        sync.Mutex
	tlsConns  int  // 握手成功的 TLS 连接
	plaintext bool // 收到过明文的登录包
	clientCrt bool // 客户端出示过证书
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

// fakePostgres 一个只会登录和回 Ping 的 PostgreSQL。srv 为 nil 时拒绝 TLS（回 'N'）
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
		want string // 失败时错误里该有的话
	}{
		{"CAFile 认得服务端证书", xtls.Config{Enable: true, CAFile: p.caFile}, true, ""},
		{"ServerName 按证书上的域名填", xtls.Config{Enable: true, CAFile: p.caFile, ServerName: "db.internal"}, true, ""},
		{"不填 CAFile 用系统根证书，自签的过不了", xtls.Config{Enable: true}, false, "certificate"},
		{"ServerName 对不上证书", xtls.Config{Enable: true, CAFile: p.caFile, ServerName: "other.internal"}, false, "certificate"},
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
					t.Errorf("错误里该说 %q，got=%v", c.want, err)
				}
				if strings.Contains(err.Error(), secret) {
					t.Errorf("错误里不该有密码：%v", err)
				}
				return
			}
			defer closer.Close()
			if err := db.Exec(";").Error; err != nil {
				t.Errorf("连上之后该能用：%v", err)
			}
			if n, plain, _ := s.get(); n == 0 || plain {
				t.Errorf("服务端应只见过 TLS 连接，tls=%d plaintext=%v", n, plain)
			}
			found := false
			for _, l := range logs() {
				if l["msg"] == "xgorm connected" {
					found = found || l["tls"] == true
				}
			}
			if !found {
				t.Errorf("建连日志该写上 tls=true，got=%v", logs())
			}
		})
	}
}

func TestNew_PG_TLSBlockFailsInsteadOfPlaintextFallback(t *testing.T) {
	// pgx 默认的 sslmode=prefer 在服务端回 'N' 时改走明文；开了 TLS 块就不许
	p := newPKI(t)
	addr, s := fakePostgres(t, nil)
	_, _, err := New(context.Background(), pgTLSCfg(addr, xtls.Config{Enable: true, CAFile: p.caFile}))
	if err == nil || !strings.Contains(err.Error(), "refused TLS") {
		t.Fatalf("服务端不肯 TLS 时该报 server refused TLS connection，got=%v", err)
	}
	if _, plain, _ := s.get(); plain {
		t.Error("不该退回明文发登录包")
	}
}

func TestNew_PG_WithoutTLSBlockFollowsPgxPreferDefault(t *testing.T) {
	// 量的是 pgx 自己的默认（sslmode=prefer）：不开 TLS 块，DSN 里也没写 sslmode 时，
	// 服务端回 'N' 就改走明文——这是开 TLS 块的理由
	addr, s := fakePostgres(t, nil)
	_, closer, err := New(context.Background(), pgTLSCfg(addr, xtls.Config{}))
	if err != nil {
		t.Fatalf("pgx 的默认会退回明文，应连得上：%v", err)
	}
	closer.Close()
	if _, plain, _ := s.get(); !plain {
		t.Error("应当是明文连上的")
	}
}

func TestNew_PG_MutualAuthSendsClientCert(t *testing.T) {
	p := newPKI(t)
	addr, s := fakePostgres(t, &tls.Config{
		Certificates: []tls.Certificate{p.server},
		ClientAuth:   tls.RequireAndVerifyClientCert, ClientCAs: p.pool,
	})
	if _, _, err := New(context.Background(), pgTLSCfg(addr, xtls.Config{Enable: true, CAFile: p.caFile})); err == nil {
		t.Fatal("服务端要客户端证书时，不带证书该连不上")
	}
	_, closer, err := New(context.Background(), pgTLSCfg(addr, xtls.Config{Enable: true, CAFile: p.caFile, CertFile: p.certFile, KeyFile: p.keyFile}))
	if err != nil {
		t.Fatalf("带上客户端证书该连得上：%v", err)
	}
	closer.Close()
	if _, _, crt := s.get(); !crt {
		t.Error("服务端应看到客户端证书")
	}
}

func TestUsePostgresTLS_OnePerHostAllTLS(t *testing.T) {
	base := &tls.Config{MinVersion: tls.VersionTLS12}
	for _, dsn := range []string{
		"host=a.internal,b.internal port=5432,5433 user=u",                 // 默认 prefer：每个主机 TLS + 明文两条
		"host=a.internal,b.internal port=5432,5433 user=u sslmode=allow",   // allow：先明文
		"host=a.internal,b.internal port=5432,5433 user=u sslmode=disable", // 只有明文
	} {
		pc, err := pgconn.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		usePostgresTLS(pc, base)
		hosts := append([]*pgconn.FallbackConfig{{Host: pc.Host, Port: pc.Port, TLSConfig: pc.TLSConfig}}, pc.Fallbacks...)
		if len(hosts) != 2 {
			t.Fatalf("%s：应剩两个主机，got=%d", dsn, len(hosts))
		}
		for i, want := range []string{"a.internal", "b.internal"} {
			h := hosts[i]
			if h.Host != want || h.TLSConfig == nil || h.TLSConfig.InsecureSkipVerify || h.TLSConfig.ServerName != want {
				t.Errorf("%s：第 %d 个应是走 TLS、按 %s 校验的 %s，got host=%s tls=%+v", dsn, i, want, want, h.Host, h.TLSConfig)
			}
		}
	}

	pc, _ := pgconn.ParseConfig("host=10.0.0.1 user=u")
	usePostgresTLS(pc, &tls.Config{ServerName: "db.internal"})
	if pc.TLSConfig.ServerName != "db.internal" || len(pc.Fallbacks) != 0 {
		t.Errorf("配了 ServerName 就按它校验，got=%q fallbacks=%d", pc.TLSConfig.ServerName, len(pc.Fallbacks))
	}
}

func TestResolveDSN_PG_TLSBlockConflictsWithDSNSSLParams(t *testing.T) {
	on := xtls.Config{Enable: true}
	for name, c := range map[string]struct {
		dsn    string
		params map[string]string
		key    string
	}{
		"KV 形式的 sslmode":  {dsn: "host=h user=u password=" + secret + " sslmode=disable", key: "sslmode"},
		"URL 形式的 sslmode": {dsn: "postgres://u:" + secret + "@h/d?sslmode=require", key: "sslmode"},
		"sslrootcert":     {dsn: "host=h user=u sslrootcert=/etc/ca.pem", key: "sslrootcert"},
		"sslnegotiation":  {dsn: "host=h user=u sslnegotiation=direct", key: "sslnegotiation"},
		"写在 Params 里也算":   {dsn: "host=h user=u", params: map[string]string{"sslmode": "disable"}, key: "sslmode"},
		"在别的参数后面也认得出来":    {dsn: "host=h user=u application_name=x TimeZone=UTC sslmode=verify-full", key: "sslmode"},
	} {
		cfg := pgCfg(c.dsn)
		cfg.Postgres.Params = c.params
		cfg.TLS = on
		_, _, err := resolveDSN(cfg)
		if err == nil || !strings.Contains(err.Error(), "sets "+c.key+" ") {
			t.Errorf("%s：该报出 %s 冲突，got=%v", name, c.key, err)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s：错误里不该有密码：%v", name, err)
		}
	}

	for name, dsn := range map[string]string{
		"没写 ssl 参数":        "host=h user=u application_name=x",
		"密码里带着 sslmode 字样": "host=h user=u password='a sslmode=disable b'",
		"URL 密码里带 sslmode": "postgres://u:sslmode%3Ddisable@h/d?application_name=x",
	} {
		cfg := pgCfg(dsn)
		cfg.TLS = on
		if _, _, err := resolveDSN(cfg); err != nil {
			t.Errorf("%s：不该报冲突，got=%v", name, err)
		}
	}

	// 没开 TLS 块时 DSN 里的 sslmode 照旧生效
	if _, _, err := resolveDSN(pgCfg("host=h user=u sslmode=verify-full")); err != nil {
		t.Errorf("没开 TLS 块时 sslmode 照旧可以写：%v", err)
	}
}

func TestResolveDSN_PG_TLSBlockRejectsUnixSocket(t *testing.T) {
	cfg := pgCfg("host=/var/run/postgresql user=u")
	cfg.TLS = xtls.Config{Enable: true}
	if _, _, err := resolveDSN(cfg); err == nil || !strings.Contains(err.Error(), "Unix socket") {
		t.Fatalf("该报 Unix socket 上没有 TLS，got=%v", err)
	}
}

// ---- MySQL ----

// fakeMySQL 一个只会握手的 MySQL：发问候包，TLS 升级之后收登录包，回 1045 让客户端别再试。
// srv 为 nil 时问候包里不带 CLIENT_SSL
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
		viaTLS bool   // 服务端该看到 TLS 上来的登录包
		want   string // 错误里该有的话
	}{
		// 假服务端在登录包之后回 1045：走到这一步说明 TLS 握手通过了
		{"CAFile 认得服务端证书", xtls.Config{Enable: true, CAFile: p.caFile}, true, "authentication to "},
		{"ServerName 按证书上的域名填", xtls.Config{Enable: true, CAFile: p.caFile, ServerName: "db.internal"}, true, "authentication to "},
		{"不填 CAFile 用系统根证书，自签的过不了", xtls.Config{Enable: true}, false, "certificate"},
		{"ServerName 对不上证书", xtls.Config{Enable: true, CAFile: p.caFile, ServerName: "other.internal"}, false, "certificate"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			addr, s := fakeMySQL(t, &tls.Config{Certificates: []tls.Certificate{p.server}})
			_, _, err := New(context.Background(), mysqlTLSCfg(addr, c.tls))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误里该说 %q，got=%v", c.want, err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("错误里不该有密码：%v", err)
			}
			n, plain, _ := s.get()
			if plain {
				t.Error("不该发明文的登录包")
			}
			if c.viaTLS != (n > 0) {
				t.Errorf("服务端握手成功的 TLS 连接 want>0=%v，got=%d", c.viaTLS, n)
			}
		})
	}
}

func TestNew_MySQL_TLSBlockFailsWhenServerLacksTLS(t *testing.T) {
	p := newPKI(t)
	addr, s := fakeMySQL(t, nil)
	_, _, err := New(context.Background(), mysqlTLSCfg(addr, xtls.Config{Enable: true, CAFile: p.caFile}))
	if err == nil || !strings.Contains(err.Error(), "TLS") {
		t.Fatalf("服务端不支持 TLS 时该失败，got=%v", err)
	}
	if _, plain, _ := s.get(); plain {
		t.Error("不该退回明文发登录包")
	}
}

// 问候包里不带 CLIENT_SSL 的，可以是真不支持 TLS 的服务端，也可以是中间人把这一位清掉了。
// DSN 里写了 allowFallbackToPlaintext=true 的话，go-sql-driver v1.10.1 此时把 TLS 丢掉、
// 改发明文的登录包（packets.go readHandshakePacket）——开了 TLS 块也一样
func TestNew_MySQL_TLSBlockRejectsPlaintextFallbackParam(t *testing.T) {
	p := newPKI(t)
	addr, s := fakeMySQL(t, nil)
	cfg := mysqlTLSCfg(addr, xtls.Config{Enable: true, CAFile: p.caFile})
	cfg.DSN += "?allowFallbackToPlaintext=true"
	_, _, err := New(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "sets allowFallbackToPlaintext") {
		t.Fatalf("该报 DSN 与 TLS 块冲突，got=%v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("错误里不该有密码：%v", err)
	}
	if _, plain, _ := s.get(); plain {
		t.Error("不该退回明文发登录包")
	}
}

// 兜底：就算校验那一步漏了，交给驱动的连接配置里也不许退回明文
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
	// gorm.Open 自己会 ping 一次（gorm v1.31 Open），建连就在这里
	db, err := gorm.Open(d, &gorm.Config{Logger: logger.Discard})
	if err == nil {
		sqlDB, _ := db.DB()
		_ = sqlDB.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "TLS") {
		t.Errorf("服务端不肯 TLS 时该失败，got=%v", err)
	}
	if _, plain, _ := s.get(); plain {
		t.Error("不该退回明文发登录包")
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
		t.Fatalf("握手通过之后才会报认证失败，got=%v", err)
	}
	if _, _, crt := s.get(); !crt {
		t.Error("服务端应看到客户端证书")
	}
}

func TestResolveDSN_MySQL_TLSBlockConflictsWithDSNTLS(t *testing.T) {
	for _, v := range []string{"true", "false", "skip-verify", "preferred"} {
		cfg := mysqlCfg("u:" + secret + "@tcp(h:3306)/d?tls=" + v)
		cfg.TLS = xtls.Config{Enable: true}
		_, _, err := resolveDSN(cfg)
		if err == nil || !strings.Contains(err.Error(), "sets tls") {
			t.Errorf("tls=%s：该报冲突，got=%v", v, err)
		}
	}
	// 密码里的 ?tls=false 骗不过它：密码在最后一个 / 之前
	cfg := mysqlCfg("u:x?tls=false@tcp(h:3306)/d")
	cfg.TLS = xtls.Config{Enable: true}
	if _, _, err := resolveDSN(cfg); err != nil {
		t.Errorf("密码里带 tls= 不算，got=%v", err)
	}
	// 没开 TLS 块时 DSN 里的 tls 照旧生效
	if _, _, err := resolveDSN(mysqlCfg("u:p@tcp(h:3306)/d?tls=true")); err != nil {
		t.Errorf("没开 TLS 块时 tls= 照旧可以写：%v", err)
	}

	cfg = mysqlCfg("u:p@unix(/run/mysqld/mysqld.sock)/d")
	cfg.TLS = xtls.Config{Enable: true}
	if _, _, err := resolveDSN(cfg); err == nil || !strings.Contains(err.Error(), "only runs over TCP") {
		t.Errorf("Unix socket 上没有 TLS，got=%v", err)
	}
}

// ---- 配置 ----

func TestValidate_TLSBlock(t *testing.T) {
	cfg := pgCfg("host=h")
	cfg.TLS = xtls.Config{CAFile: "/etc/ca.pem"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "TLS.Enable") {
		t.Errorf("没开 TLS 却写了 CAFile 该报错，got=%v", err)
	}

	withDialect(t, Dialect{Name: "notls", Open: openPostgres})
	cfg = DefaultClientConfig()
	cfg.Driver, cfg.DSN = "notls", "x"
	cfg.TLS = xtls.Config{Enable: true}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "does not support the TLS block") {
		t.Errorf("方言没有 OpenTLS 时配 TLS 块该报错，got=%v", err)
	}
}

func TestNew_TLSUnreadableCertIsConfigError(t *testing.T) {
	cfg := pgTLSCfg(deadAddr(t), xtls.Config{Enable: true, CAFile: filepath.Join(t.TempDir(), "nope.pem")})
	_, _, err := New(context.Background(), cfg)
	assertOneFrame(t, err, "xgorm", "config")
	if !strings.Contains(err.Error(), "TLS.CAFile") {
		t.Errorf("该说是哪个文件，got=%v", err)
	}
}

func TestConfig_TLSBlockFromConfigFile(t *testing.T) {
	p := newPKI(t)
	c := load(t, "XGorm:\n  DSN: host=h\n  TLS:\n    Enable: true\n    CAFile: "+strconv.Quote(p.caFile)+"\n    ServerName: db.internal\n")
	got := c.Clients[DefaultName].TLS
	if !got.Enable || got.CAFile != p.caFile || got.ServerName != "db.internal" {
		t.Errorf("TLS 块没读对：%+v", got)
	}
	if err := loadErr(t, "XGorm:\n  DSN: host=h\n  TLS:\n    CAFile: /etc/ca.pem\n"); err == nil {
		t.Error("没开 TLS 却写了 CAFile，读配置时就该失败")
	}
}
