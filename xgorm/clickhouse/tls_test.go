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

// tlsSecret is the password in the test DSNs, used for comparison when asserting that an error carries no password.
const tlsSecret = "tls-unit-secret"

// testPKI is the CA built for the test together with the server certificate it signs (which names only localhost and no IP,
// so connecting by IP or verifying against another host name must fail) and a client certificate.
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

// handshakes records every connection the stub server saw.
type handshakes struct {
	mu        sync.Mutex
	tls       int      // TLS connections that completed the handshake
	plaintext int      // connections whose first byte was not a TLS handshake
	sni       []string // the ServerName the client announced on each handshake
	clientCrt bool     // a client certificate was seen
	users     []string // the X-ClickHouse-User header of the HTTPS requests
	rawQuery  []string // the query string of the HTTPS requests
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

// fakeNativeTLS is a native-protocol stub that accepts TLS only: after the handshake it reads the client Hello and replies with a
// 516 AUTHENTICATION_FAILED Exception packet (which is how a server rejects credentials during the native handshake).
// Replying 516 makes xgorm return after a single attempt, so the test does not have to wait out all three retries.
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
				if first[0] != 0x16 { // the handshake type of the TLS record layer
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

// peekedConn reads the bytes buffered by bufio before continuing with the underlying connection.
type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// exceptionPacket is the native protocol's ServerException packet (field order from clickhouse-go v2.48.0 lib/proto/exception.go).
func exceptionPacket(code int32, name, msg string) []byte {
	b := []byte{2} // proto.ServerException
	b = binary.LittleEndian.AppendUint32(b, uint32(code))
	for _, s := range []string{name, msg, ""} {
		b = binary.AppendUvarint(b, uint64(len(s)))
		b = append(b, s...)
	}
	return append(b, 0) // nested = false
}

// fakeHTTPS is an HTTPS stub that answers every request with 403 + 516, the shape of the HTTP response a server sends when it rejects credentials.
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
		ErrorLog:  log.New(io.Discard, "", 0), // handshake failures are exactly what is being tested, so they do not go to stderr
	}
	go func() { _ = s.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = s.Close() })
	return ln.Addr().String(), h
}

// localhost rewrites 127.0.0.1:port as localhost:port: the server certificate names only localhost.
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
		// https:// without secure=true in the DSN: with a TLS block it is unnecessary (and writing it is a conflict).
		{"https", "https", fakeHTTPS},
	} {
		for _, c := range []struct {
			name    string
			tls     xtls.Config
			ok      bool
			wantSNI string
		}{
			{"CAFile_recognizes_the_server_certificate_ServerName_filled_from_the_host", xtls.Config{Enable: true, CAFile: p.caFile}, true, "localhost"},
			{"ServerName_set_to_the_name_on_the_certificate", xtls.Config{Enable: true, CAFile: p.caFile, ServerName: "localhost"}, true, "localhost"},
			{"without_CAFile_the_system_roots_are_used_and_a_self_signed_certificate_fails", xtls.Config{Enable: true}, false, ""},
			{"wrong_CA", xtls.Config{Enable: true, CAFile: p.otherCAFile}, false, ""},
			{"ServerName_does_not_match_the_certificate", xtls.Config{Enable: true, CAFile: p.caFile, ServerName: "other.internal"}, false, ""},
		} {
			t.Run(proto.name+"_"+c.name, func(t *testing.T) {
				addr, h := proto.fake(t, srv)
				dsn := proto.scheme + "://xone:" + tlsSecret + "@" + localhost(addr) + "/db"
				_, _, err := xgorm.New(context.Background(), tlsCfg(dsn, c.tls))
				if err == nil {
					t.Fatal("the stub server always replies 516, so connecting must not succeed")
				}
				if strings.Contains(err.Error(), tlsSecret) {
					t.Errorf("the error carries the password: %v", err)
				}
				t.Logf("error: %v", err)
				n, plain, sni := h.snapshot()
				if plain != 0 {
					t.Errorf("with a TLS block there must be no plaintext connection, got=%d", plain)
				}
				if c.ok {
					// The handshake passed and the server's 516 came back over TLS: recognized as an auth failure, so only one attempt.
					if !strings.Contains(err.Error(), "authentication to "+localhost(addr)+" failed") {
						t.Errorf("the TLS handshake passed and the server replied 516, which must be reported as an auth failure, got=%v", err)
					}
					if n != 1 || len(sni) != 1 || sni[0] != c.wantSNI {
						t.Errorf("there must be exactly one TLS handshake with ServerName=%q, got=%d handshakes sni=%q", c.wantSNI, n, sni)
					}
					return
				}
				// Wrong certificate: the handshake fails on the server side, the client reports a certificate error, and it is not retried (xclient.tlsRejected).
				if !strings.Contains(err.Error(), "certificate") {
					t.Errorf("a wrong certificate must be reported as a certificate error, got=%v", err)
				}
				var verr *tls.CertificateVerificationError
				if !errors.As(err, &verr) {
					t.Errorf("the error chain must contain a *tls.CertificateVerificationError, got=%v", err)
				}
			})
		}
	}
}

func TestNew_TLSBlock_HTTPSCredentialsInHeaderNotURL(t *testing.T) {
	// With TLS the driver puts the user name and password into the X-ClickHouse-User / X-ClickHouse-Key headers,
	// and without TLS into the URL as user:password (v2.48.0 conn_http.go applyOptionsToRequest).
	p := newTestPKI(t)
	addr, h := fakeHTTPS(t, &tls.Config{Certificates: []tls.Certificate{p.server}})
	dsn := "https://xone:" + tlsSecret + "@" + localhost(addr) + "/db"
	_, _, _ = xgorm.New(context.Background(), tlsCfg(dsn, xtls.Config{Enable: true, CAFile: p.caFile}))
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.users) == 0 || h.users[0] != "xone" {
		t.Fatalf("the HTTPS request must carry X-ClickHouse-User, got=%q", h.users)
	}
	for _, q := range h.rawQuery {
		if strings.Contains(q, "secure") {
			t.Errorf("the secure=true added for parsing must not become a request parameter (the server would read it as a setting), got=%q", q)
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
		t.Fatalf("the handshake must fail when the server requires a client certificate and none is sent, got=%v", err)
	}
	_, _, err := xgorm.New(context.Background(), tlsCfg(dsn,
		xtls.Config{Enable: true, CAFile: p.caFile, CertFile: p.certFile, KeyFile: p.keyFile}))
	if err == nil || !strings.Contains(err.Error(), "authentication to") {
		t.Fatalf("with a client certificate the handshake must pass and 516 must follow, got=%v", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.clientCrt {
		t.Error("the server must have seen the client certificate")
	}
}

func TestNew_TLSBlock_MultiHostVerifiesCertOfConnectedHost(t *testing.T) {
	// The first host is unreachable, so the driver moves on to the second under in_order: with no ServerName configured,
	// verification must use the second host's name, not the first's. The first is written as 127.0.0.1, which the certificate does not name, so pinning would fail the handshake.
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
		t.Fatalf("the second host's certificate matches, so the handshake must pass and 516 must follow, got=%v", err)
	}
	if n, _, sni := h.snapshot(); n != 1 || sni[0] != "localhost" {
		t.Errorf("the handshake must use the second host's name localhost, got=%d handshakes sni=%q", n, sni)
	}
}

func TestNew_TLSBlockFailsOnPlaintextPort(t *testing.T) {
	// The server speaks plaintext only: the TLS handshake fails and there is no fallback to plaintext.
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
			// A plaintext server answers a ClientHello with its own protocol bytes; here that is an Exception packet.
			_, _ = c.Write(exceptionPacket(102, "DB::Exception", "Unexpected packet"))
			c.Close()
		}
	}()
	p := newTestPKI(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond) // do not wait out all three retries
	defer cancel()
	_, _, err = xgorm.New(ctx, tlsCfg("clickhouse://xone:"+tlsSecret+"@"+localhost(ln.Addr().String())+"/db",
		xtls.Config{Enable: true, CAFile: p.caFile}))
	if err == nil || !strings.Contains(err.Error(), "tls") {
		t.Fatalf("the TLS handshake must fail on a plaintext port, got=%v", err)
	}
	t.Logf("error: %v", err)
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
			t.Errorf("%s: writing TLS parameters in the DSN while a TLS block is on must be a conflict, got=%v", dsn, err)
			continue
		}
		if strings.Contains(err.Error(), tlsSecret) {
			t.Errorf("the error carries the password: %v", err)
		}
	}
	// Without a TLS block the TLS parameters in the DSN keep working as before.
	if _, _, err := resolve(tlsCfg("https://u:p@h:8443/db?secure=true&skip_verify=true", xtls.Config{})); err != nil {
		t.Errorf("without a TLS block secure / skip_verify may still be written: %v", err)
	}
}

func TestResolve_TLSBlockRejectsHTTP(t *testing.T) {
	_, _, err := resolve(tlsCfg("http://u:"+tlsSecret+"@h:8123/db", xtls.Config{Enable: true}))
	if err == nil || !strings.Contains(err.Error(), "http:// never runs TLS") {
		t.Fatalf("http:// with a TLS block would quietly go plaintext and must be a configuration error, got=%v", err)
	}
	if strings.Contains(err.Error(), tlsSecret) {
		t.Errorf("the error carries the password: %v", err)
	}
}

func TestResolve_TLSBlockHTTPSNeedsNoSecureAndDSNUnchanged(t *testing.T) {
	const in = "https://u:p@h:8443/db?max_execution_time=5"
	dsn, info, err := resolve(tlsCfg(in, xtls.Config{Enable: true}))
	if err != nil {
		t.Fatalf("with a TLS block, https:// without secure must parse: %v", err)
	}
	if strings.Contains(dsn, "secure") {
		t.Errorf("the secure added for parsing must not be written back into the DSN, got=%q", dsn)
	}
	if info.Addr != "h:8443" || info.DB != "db" {
		t.Errorf("wrong connection info, got=%+v", info)
	}
	// Without a TLS block the driver's rule is unchanged: https:// must carry secure=true.
	if _, _, err := resolve(tlsCfg(in, xtls.Config{})); err == nil {
		t.Error("without a TLS block, https:// without secure must be rejected by the driver's rule")
	}
}

func TestParseDSN_HttpsNeedsSecureToKeepScheme(t *testing.T) {
	// When opening an HTTP connection the driver builds the address from the scheme recorded at parse time; parsed as http:// it would send plaintext even with TLS in hand.
	opts, err := parseDSN("https://u:p@h:8443/db", true)
	if err != nil || opts.Protocol != chgo.HTTP {
		t.Fatalf("https:// must resolve to the HTTP protocol, got=%v err=%v", opts, err)
	}
	if opts, err := parseDSN("clickhouse://u:p@h:9440/db", true); err != nil || opts.Protocol != chgo.Native || opts.TLS != nil {
		t.Errorf("a native DSN is parsed as is and TLS is left for openTLS to set, got=%+v err=%v", opts, err)
	}
}

func TestOpenTLS_DoesNotPassDSNToGORMDriver(t *testing.T) {
	// Given a DSN, gorm's clickhouse driver parses its own Options; UpdateLocalTable then connects to each host directly, without the TLS block.
	d, err := openTLS("clickhouse://u:p@h:9440/db", &tls.Config{})
	if err != nil {
		t.Fatal(err)
	}
	cd := d.(*clickhouse.Dialector)
	t.Cleanup(func() { _ = cd.Conn.(interface{ Close() error }).Close() })
	if cd.DSN != "" || cd.Conn == nil || !cd.SkipInitializeWithVersion {
		t.Errorf("only the pool must be handed over, with the version query in Initialize turned off, got DSN=%q Conn=%v Skip=%v", cd.DSN, cd.Conn, cd.SkipInitializeWithVersion)
	}
	// When the driver fails to parse http_proxy it echoes the whole proxy address (see TestResolve_RejectsDriverUnparsableDSNWithoutEcho).
	bad := "clickhouse://h:9440/db?http_proxy=" + url.QueryEscape("http://u:"+tlsSecret+"@proxy:3128/%zz")
	if _, err := openTLS(bad, &tls.Config{}); err == nil || strings.Contains(err.Error(), tlsSecret) {
		t.Errorf("a parse failure must be an error and must not echo the DSN, got=%v", err)
	}
}

func TestRegister_OpenTLSIsOpenTLS(t *testing.T) {
	// Aimed at the registered dialect: without it, a configured TLS block would be rejected by xgorm as unsupported.
	c := tlsCfg("clickhouse://u:p@h:9440/db", xtls.Config{Enable: true})
	if err := c.Validate(); err != nil {
		t.Errorf("clickhouse must accept a TLS block: %v", err)
	}
}
