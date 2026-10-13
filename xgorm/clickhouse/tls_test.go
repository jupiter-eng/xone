package clickhouse

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	chgo "github.com/ClickHouse/clickhouse-go/v2"
	"gorm.io/driver/clickhouse"

	"github.com/jupiter-eng/xone/xgorm"
	"github.com/jupiter-eng/xone/xtls"
)

// tlsSecret 测试 DSN 里的密码，断言「错误里没有密码」时拿它比对
const tlsSecret = "tls-unit-secret"

// testPKI 测试时现造的 CA 和它签的服务端证书（只写 localhost，不写 IP：
// 按 IP 连、或者按别的主机名比对都必须失败）、客户端证书
type testPKI struct {
	caFile, otherCAFile, certFile, keyFile string
	server                                 tls.Certificate
	pool                                   *x509.CertPool
}

func newTestPKI(t *testing.T) testPKI {
	t.Helper()
	dir := t.TempDir()
	newCA := func(name string) (*x509.Certificate, *ecdsa.PrivateKey) {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tpl := &x509.Certificate{
			SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
		}
		der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		ca, _ := x509.ParseCertificate(der)
		return ca, key
	}
	write := func(name, typ string, der []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ca, caKey := newCA("ch-test-ca")
	other, _ := newCA("ch-other-ca")
	issue := func(serial int64, usage x509.ExtKeyUsage, dns []string) ([]byte, *ecdsa.PrivateKey) {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "localhost"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			DNSNames: dns, ExtKeyUsage: []x509.ExtKeyUsage{usage}, KeyUsage: x509.KeyUsageDigitalSignature,
		}
		der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		return der, key
	}
	srvDER, srvKey := issue(2, x509.ExtKeyUsageServerAuth, []string{"localhost"})
	cliDER, cliKey := issue(3, x509.ExtKeyUsageClientAuth, nil)
	cliKeyDER, _ := x509.MarshalPKCS8PrivateKey(cliKey)
	p := testPKI{
		caFile:      write("ca.pem", "CERTIFICATE", ca.Raw),
		otherCAFile: write("other-ca.pem", "CERTIFICATE", other.Raw),
		certFile:    write("client.pem", "CERTIFICATE", cliDER),
		keyFile:     write("client-key.pem", "PRIVATE KEY", cliKeyDER),
		server:      tls.Certificate{Certificate: [][]byte{srvDER}, PrivateKey: srvKey},
		pool:        x509.NewCertPool(),
	}
	p.pool.AddCert(ca)
	return p
}

// handshakes 桩服务端看到的每一条连接
type handshakes struct {
	mu        sync.Mutex
	tls       int      // 握手成功的 TLS 连接
	plaintext int      // 第一个字节就不是 TLS 握手的连接
	sni       []string // 每次握手客户端报的 ServerName
	clientCrt bool     // 见过客户端证书
	users     []string // HTTPS 请求头里的 X-ClickHouse-User
	rawQuery  []string // HTTPS 请求的查询串
}

func (h *handshakes) snapshot() (tlsN, plain int, sni []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tls, h.plaintext, append([]string(nil), h.sni...)
}

func (h *handshakes) record(c *tls.Conn) {
	st := c.ConnectionState()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tls++
	h.sni = append(h.sni, st.ServerName)
	h.clientCrt = h.clientCrt || len(st.PeerCertificates) > 0
}

// fakeNativeTLS 一个只收 TLS 的 native 协议桩：握手之后读掉客户端的 Hello，回一个
// 516 AUTHENTICATION_FAILED 的 Exception 包（native 握手阶段服务端就是这样拒绝凭证的）。
// 回 516 是为了让 xgorm 只试一次就返回，用例不必等满三轮重试
func fakeNativeTLS(t *testing.T, srv *tls.Config) (string, *handshakes) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := &handshakes{}
	var wg sync.WaitGroup
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				br := bufio.NewReader(c)
				first, err := br.Peek(1)
				if err != nil {
					return
				}
				if first[0] != 0x16 { // TLS 记录层的 handshake 类型
					h.mu.Lock()
					h.plaintext++
					h.mu.Unlock()
					return
				}
				tc := tls.Server(&peekedConn{Conn: c, r: br}, srv)
				if tc.Handshake() != nil {
					return
				}
				h.record(tc)
				buf := make([]byte, 4096)
				if _, err := tc.Read(buf); err != nil {
					return
				}
				_, _ = tc.Write(exceptionPacket(516, "DB::Exception", "xone: Authentication failed"))
			}()
		}
	}()
	t.Cleanup(func() { ln.Close(); wg.Wait() })
	return ln.Addr().String(), h
}

// peekedConn 先读完 bufio 里预读的字节再读底下的连接
type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// exceptionPacket native 协议的 ServerException 包（clickhouse-go v2.48.0 lib/proto/exception.go 的解码顺序）
func exceptionPacket(code int32, name, msg string) []byte {
	b := []byte{2} // proto.ServerException
	b = binary.LittleEndian.AppendUint32(b, uint32(code))
	for _, s := range []string{name, msg, ""} {
		b = binary.AppendUvarint(b, uint64(len(s)))
		b = append(b, s...)
	}
	return append(b, 0) // nested = false
}

// fakeHTTPS 一个 HTTPS 桩：每个请求都回 403 + 516，形状同服务端拒绝凭证时的 HTTP 响应
func fakeHTTPS(t *testing.T, srv *tls.Config) (string, *handshakes) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := &handshakes{}
	cfg := srv.Clone()
	cfg.VerifyConnection = func(st tls.ConnectionState) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.tls++
		h.sni = append(h.sni, st.ServerName)
		h.clientCrt = h.clientCrt || len(st.PeerCertificates) > 0
		return nil
	}
	s := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.mu.Lock()
			h.users = append(h.users, r.Header.Get("X-ClickHouse-User"))
			h.rawQuery = append(h.rawQuery, r.URL.RawQuery)
			h.mu.Unlock()
			w.Header().Set("X-ClickHouse-Exception-Code", "516")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("Code: 516. DB::Exception: xone: Authentication failed. (AUTHENTICATION_FAILED)\n"))
		}),
		TLSConfig: cfg,
		ErrorLog:  log.New(io.Discard, "", 0), // 握手失败正是要测的，不往 stderr 打
	}
	go func() { _ = s.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = s.Close() })
	return ln.Addr().String(), h
}

// localhost 把 127.0.0.1:port 写成 localhost:port：服务端证书上只有 localhost
func localhost(addr string) string {
	_, port, _ := net.SplitHostPort(addr)
	return net.JoinHostPort("localhost", port)
}

func tlsCfg(dsn string, tc xtls.Config) xgorm.ClientConfig {
	c := cfg(dsn, 500*time.Millisecond)
	c.TLS = tc
	c.Trace, c.Metric = false, false
	return c
}

func TestNew_TLSBlockApplies_NativeAndHTTPSOnlyUseTLS(t *testing.T) {
	p := newTestPKI(t)
	srv := &tls.Config{Certificates: []tls.Certificate{p.server}}
	for _, proto := range []struct {
		name   string
		scheme string
		fake   func(*testing.T, *tls.Config) (string, *handshakes)
	}{
		{"native", "clickhouse", fakeNativeTLS},
		// https:// 在 DSN 里不写 secure=true：开了 TLS 块就不必写（写了是冲突）
		{"https", "https", fakeHTTPS},
	} {
		for _, c := range []struct {
			name    string
			tls     xtls.Config
			ok      bool
			wantSNI string
		}{
			{"CAFile 认得服务端证书_ServerName按主机名补", xtls.Config{Enable: true, CAFile: p.caFile}, true, "localhost"},
			{"ServerName 按证书上的名字填", xtls.Config{Enable: true, CAFile: p.caFile, ServerName: "localhost"}, true, "localhost"},
			{"不填 CAFile 用系统根证书_自签的过不了", xtls.Config{Enable: true}, false, ""},
			{"CA 不对", xtls.Config{Enable: true, CAFile: p.otherCAFile}, false, ""},
			{"ServerName 对不上证书", xtls.Config{Enable: true, CAFile: p.caFile, ServerName: "other.internal"}, false, ""},
		} {
			t.Run(proto.name+"_"+c.name, func(t *testing.T) {
				addr, h := proto.fake(t, srv)
				dsn := proto.scheme + "://xone:" + tlsSecret + "@" + localhost(addr) + "/db"
				_, _, err := xgorm.New(context.Background(), tlsCfg(dsn, c.tls))
				if err == nil {
					t.Fatal("桩服务端一律回 516，不该建连成功")
				}
				if strings.Contains(err.Error(), tlsSecret) {
					t.Errorf("错误里带着密码：%v", err)
				}
				t.Logf("错误：%v", err)
				n, plain, sni := h.snapshot()
				if plain != 0 {
					t.Errorf("开了 TLS 块不该有明文连接，got=%d", plain)
				}
				if c.ok {
					// 握手过了、服务端的 516 经 TLS 传回来：认成认证失败，只试一次
					if !strings.Contains(err.Error(), "authentication to "+localhost(addr)+" failed") {
						t.Errorf("TLS 握手过了、服务端回 516，该报认证失败，got=%v", err)
					}
					if n != 1 || len(sni) != 1 || sni[0] != c.wantSNI {
						t.Errorf("该只有一次 TLS 握手、ServerName=%q，got=%d 次 sni=%q", c.wantSNI, n, sni)
					}
					return
				}
				// 证书不对：服务端那一侧握手失败，客户端报证书错误，而且不重试（xclient.tlsRejected）
				if !strings.Contains(err.Error(), "certificate") {
					t.Errorf("证书不对该报证书错误，got=%v", err)
				}
				var verr *tls.CertificateVerificationError
				if !errors.As(err, &verr) {
					t.Errorf("错误链上该有 *tls.CertificateVerificationError，got=%v", err)
				}
			})
		}
	}
}

func TestNew_TLSBlock_HTTPSCredentialsInHeaderNotURL(t *testing.T) {
	// 驱动在有 TLS 时把用户名、密码放进 X-ClickHouse-User / X-ClickHouse-Key 请求头，
	// 没有 TLS 时放进 URL 的 user:password（v2.48.0 conn_http.go applyOptionsToRequest）
	p := newTestPKI(t)
	addr, h := fakeHTTPS(t, &tls.Config{Certificates: []tls.Certificate{p.server}})
	dsn := "https://xone:" + tlsSecret + "@" + localhost(addr) + "/db"
	_, _, _ = xgorm.New(context.Background(), tlsCfg(dsn, xtls.Config{Enable: true, CAFile: p.caFile}))
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.users) == 0 || h.users[0] != "xone" {
		t.Fatalf("HTTPS 请求该带 X-ClickHouse-User，got=%q", h.users)
	}
	for _, q := range h.rawQuery {
		if strings.Contains(q, "secure") {
			t.Errorf("为解析补的 secure=true 不该变成请求参数（服务端会把它当成一个设置），got=%q", q)
		}
	}
}

func TestNew_TLSBlock_MutualAuthSendsClientCert(t *testing.T) {
	p := newTestPKI(t)
	srv := &tls.Config{Certificates: []tls.Certificate{p.server}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: p.pool}
	addr, h := fakeNativeTLS(t, srv)
	dsn := "clickhouse://xone:" + tlsSecret + "@" + localhost(addr) + "/db"

	if _, _, err := xgorm.New(context.Background(), tlsCfg(dsn, xtls.Config{Enable: true, CAFile: p.caFile})); err == nil ||
		strings.Contains(err.Error(), "authentication to") {
		t.Fatalf("服务端要客户端证书而没带，握手就该失败，got=%v", err)
	}
	_, _, err := xgorm.New(context.Background(), tlsCfg(dsn,
		xtls.Config{Enable: true, CAFile: p.caFile, CertFile: p.certFile, KeyFile: p.keyFile}))
	if err == nil || !strings.Contains(err.Error(), "authentication to") {
		t.Fatalf("带上客户端证书握手该过，随后收到 516，got=%v", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.clientCrt {
		t.Error("服务端该看到客户端证书")
	}
}

func TestNew_TLSBlock_MultiHostVerifiesCertOfConnectedHost(t *testing.T) {
	// 第一台连不上，驱动按 in_order 去连第二台：ServerName 没配时要按第二台的名字比对，
	// 而不是钉死成第一台的。第一台写成 127.0.0.1，证书上没有这个 IP，钉死的话握手必败
	p := newTestPKI(t)
	addr, h := fakeNativeTLS(t, &tls.Config{Certificates: []tls.Certificate{p.server}})
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := closed.Addr().String()
	closed.Close()

	dsn := "clickhouse://xone:" + tlsSecret + "@" + dead + "," + localhost(addr) + "/db"
	_, _, err = xgorm.New(context.Background(), tlsCfg(dsn, xtls.Config{Enable: true, CAFile: p.caFile}))
	if err == nil || !strings.Contains(err.Error(), "authentication to") {
		t.Fatalf("第二台的证书对得上，握手该过、随后收到 516，got=%v", err)
	}
	if n, _, sni := h.snapshot(); n != 1 || sni[0] != "localhost" {
		t.Errorf("该按第二台的名字 localhost 握手，got=%d 次 sni=%q", n, sni)
	}
}

func TestNew_TLSBlockFailsOnPlaintextPort(t *testing.T) {
	// 服务端只说明文：TLS 握手失败，不会退回明文
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
			// 明文服务端对 ClientHello 回的是它自己的协议字节，这里回一个 Exception 包
			_, _ = c.Write(exceptionPacket(102, "DB::Exception", "Unexpected packet"))
			c.Close()
		}
	}()
	p := newTestPKI(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond) // 不等满三轮重试
	defer cancel()
	_, _, err = xgorm.New(ctx, tlsCfg("clickhouse://xone:"+tlsSecret+"@"+localhost(ln.Addr().String())+"/db",
		xtls.Config{Enable: true, CAFile: p.caFile}))
	if err == nil || !strings.Contains(err.Error(), "tls") {
		t.Fatalf("明文端口上 TLS 握手该失败，got=%v", err)
	}
	t.Logf("错误：%v", err)
}

func TestResolve_TLSBlockConflictsWithDSNTLSParams(t *testing.T) {
	on := xtls.Config{Enable: true}
	for _, dsn := range []string{
		"clickhouse://u:" + tlsSecret + "@h:9440/db?secure=true",
		"clickhouse://u:" + tlsSecret + "@h:9440/db?secure=false",
		"clickhouse://u:" + tlsSecret + "@h:9440/db?secure",
		"https://u:" + tlsSecret + "@h:8443/db?secure=true",
		"clickhouse://u:" + tlsSecret + "@h:9440/db?skip_verify=true",
		"clickhouse://u:" + tlsSecret + "@h:9440/db?secure=true&tls_server_name=h",
	} {
		_, _, err := resolve(tlsCfg(dsn, on))
		if err == nil || !strings.Contains(err.Error(), "configure TLS in one place") {
			t.Errorf("%s：TLS 块开着时 DSN 里再写 TLS 参数该报冲突，got=%v", dsn, err)
			continue
		}
		if strings.Contains(err.Error(), tlsSecret) {
			t.Errorf("错误里带着密码：%v", err)
		}
	}
	// 没开 TLS 块时 DSN 里的 TLS 参数照旧生效
	if _, _, err := resolve(tlsCfg("https://u:p@h:8443/db?secure=true&skip_verify=true", xtls.Config{})); err != nil {
		t.Errorf("没开 TLS 块时 secure / skip_verify 照旧可以写：%v", err)
	}
}

func TestResolve_TLSBlockRejectsHTTP(t *testing.T) {
	_, _, err := resolve(tlsCfg("http://u:"+tlsSecret+"@h:8123/db", xtls.Config{Enable: true}))
	if err == nil || !strings.Contains(err.Error(), "http:// never runs TLS") {
		t.Fatalf("http:// 配 TLS 块会悄悄走明文，该报配置错误，got=%v", err)
	}
	if strings.Contains(err.Error(), tlsSecret) {
		t.Errorf("错误里带着密码：%v", err)
	}
}

func TestResolve_TLSBlockHTTPSNeedsNoSecureAndDSNUnchanged(t *testing.T) {
	const in = "https://u:p@h:8443/db?max_execution_time=5"
	dsn, info, err := resolve(tlsCfg(in, xtls.Config{Enable: true}))
	if err != nil {
		t.Fatalf("开了 TLS 块时 https:// 不写 secure 该能解：%v", err)
	}
	if strings.Contains(dsn, "secure") {
		t.Errorf("为解析补的 secure 不该写回 DSN，got=%q", dsn)
	}
	if info.Addr != "h:8443" || info.DB != "db" {
		t.Errorf("连接信息不对，got=%+v", info)
	}
	// 没开 TLS 块时驱动的规矩不变：https:// 必须写 secure=true
	if _, _, err := resolve(tlsCfg(in, xtls.Config{})); err == nil {
		t.Error("没开 TLS 块时 https:// 不写 secure 该照驱动的规矩报错")
	}
}

func TestParseDSN_HttpsNeedsSecureToKeepScheme(t *testing.T) {
	// 驱动建 HTTP 连接时按解析时记下的 scheme 拼地址；按 http:// 解的话手里有 TLS 也发明文
	opts, err := parseDSN("https://u:p@h:8443/db", true)
	if err != nil || opts.Protocol != chgo.HTTP {
		t.Fatalf("https:// 该解成 HTTP 协议，got=%v err=%v", opts, err)
	}
	if opts, err := parseDSN("clickhouse://u:p@h:9440/db", true); err != nil || opts.Protocol != chgo.Native || opts.TLS != nil {
		t.Errorf("native 的 DSN 原样解，TLS 留给 openTLS 设，got=%+v err=%v", opts, err)
	}
}

func TestOpenTLS_DoesNotPassDSNToGORMDriver(t *testing.T) {
	// gorm 的 clickhouse 驱动拿到 DSN 会自己解一份 Options，UpdateLocalTable 按它直连每台主机、不带 TLS 块
	d, err := openTLS("clickhouse://u:p@h:9440/db", &tls.Config{})
	if err != nil {
		t.Fatal(err)
	}
	cd := d.(*clickhouse.Dialector)
	t.Cleanup(func() { _ = cd.Conn.(interface{ Close() error }).Close() })
	if cd.DSN != "" || cd.Conn == nil || !cd.SkipInitializeWithVersion {
		t.Errorf("该只给连接池、关掉 Initialize 里的查版本，got DSN=%q Conn=%v Skip=%v", cd.DSN, cd.Conn, cd.SkipInitializeWithVersion)
	}
	// 驱动解析 http_proxy 失败时回显整个代理地址（见 TestResolve_RejectsDriverUnparsableDSNWithoutEcho）
	bad := "clickhouse://h:9440/db?http_proxy=" + url.QueryEscape("http://u:"+tlsSecret+"@proxy:3128/%zz")
	if _, err := openTLS(bad, &tls.Config{}); err == nil || strings.Contains(err.Error(), tlsSecret) {
		t.Errorf("解析失败该报错且不回显 DSN，got=%v", err)
	}
}

func TestRegister_OpenTLSIsOpenTLS(t *testing.T) {
	// 打在注册的方言上：没接的话配了 TLS 块会被 xgorm 以「不支持」拒掉
	c := tlsCfg("clickhouse://u:p@h:9440/db", xtls.Config{Enable: true})
	if err := c.Validate(); err != nil {
		t.Errorf("clickhouse 该收 TLS 块：%v", err)
	}
}
