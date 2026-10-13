package xecho

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/jupiter-eng/xone/internal/testkit"
)

// serverPKI 现造的证书：一个 CA，它签的服务端证书（127.0.0.1）和客户端证书；
// 另一个不相干的 CA 签的客户端证书
type serverPKI struct {
	caFile, certFile, keyFile string // CA；服务端证书与私钥
	pool                      *x509.CertPool
	client, stranger          tls.Certificate
}

func newServerPKI(t *testing.T) serverPKI {
	t.Helper()
	dir := t.TempDir()
	newCA := func(name string) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
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
		return ca, key, der
	}
	issue := func(ca *x509.Certificate, caKey *ecdsa.PrivateKey, serial int64, usage x509.ExtKeyUsage) ([]byte, *ecdsa.PrivateKey) {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "svc.internal"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			DNSNames: []string{"svc.internal"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
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
	ca, caKey, caDER := newCA("xecho-test-ca")
	other, otherKey, _ := newCA("xecho-stranger-ca")
	srvDER, srvKey := issue(ca, caKey, 2, x509.ExtKeyUsageServerAuth)
	srvKeyDER, _ := x509.MarshalECPrivateKey(srvKey)
	cliDER, cliKey := issue(ca, caKey, 3, x509.ExtKeyUsageClientAuth)
	strDER, strKey := issue(other, otherKey, 4, x509.ExtKeyUsageClientAuth)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return serverPKI{
		caFile:   write("ca.pem", "CERTIFICATE", caDER),
		certFile: write("server.pem", "CERTIFICATE", srvDER),
		keyFile:  write("server-key.pem", "EC PRIVATE KEY", srvKeyDER),
		pool:     pool,
		client:   tls.Certificate{Certificate: [][]byte{cliDER}, PrivateKey: cliKey},
		stranger: tls.Certificate{Certificate: [][]byte{strDER}, PrivateKey: strKey},
	}
}

// servingTLS 起 https 服务，等它开始监听。hits 数 handler 被调到几次
func servingTLS(t *testing.T, p serverPKI, mutate ...func(*Config)) (string, *atomic.Int32) {
	t.Helper()
	port := testkit.FreePort(t)
	c := configWith(append([]func(*Config){quiet, on(port), func(c *Config) {
		c.TLS.CertFile, c.TLS.KeyFile = p.certFile, p.keyFile
	}}, mutate...)...)
	var hits atomic.Int32
	g := New().WithConfig(c).WithRoutes(func(e *echo.Echo) {
		e.GET("/who", func(c echo.Context) error {
			hits.Add(1)
			cn := ""
			if s := c.Request().TLS; s != nil && len(s.PeerCertificates) > 0 {
				cn = s.PeerCertificates[0].Subject.CommonName
			}
			return c.String(200, cn)
		})
	})
	go func() { _ = g.Start(context.Background()) }()
	t.Cleanup(func() { _ = g.Stop(context.Background()) })
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for i := 0; ; i++ {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			c.Close()
			break
		}
		if i == 100 {
			t.Fatalf("服务没有起来：%v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return "https://" + addr, &hits
}

// get 用给定的客户端 TLS 设置请求一次 /who
func get(p serverPKI, base string, cfg *tls.Config) (string, error) {
	if cfg.RootCAs == nil {
		cfg.RootCAs = p.pool
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Get(base + "/who")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

// withCert 配上证书（Validate 不读文件，路径随便写），TLS 就算开了
func withCert(c *Config) { c.TLS.CertFile, c.TLS.KeyFile = "c.pem", "k.pem" }

func TestValidate_ServerTLSNewFields(t *testing.T) {
	bad := map[string]func(*Config){
		"只配 ClientCAFile 没配证书": func(c *Config) { c.TLS.ClientCAFile = "ca.pem" },
		"MinVersion 写成 1.1":    func(c *Config) { withCert(c); c.TLS.MinVersion = "1.1" },
		"MinVersion 写成 TLS1.3": func(c *Config) { withCert(c); c.TLS.MinVersion = "TLS1.3" },
	}
	for name, m := range bad {
		if err := configWith(m).Validate(); err == nil {
			t.Errorf("%s：该报错", name)
		}
	}
	good := map[string]func(*Config){
		"默认":                   func(*Config) {},
		"MinVersion 1.3":       func(c *Config) { c.TLS.MinVersion = "1.3" },
		"证书加 ClientCAFile":     func(c *Config) { c.TLS.CertFile, c.TLS.KeyFile, c.TLS.ClientCAFile = "c", "k", "ca" },
		"没配证书时 MinVersion 不管用": func(c *Config) { c.TLS.MinVersion = "1.3" },
		// 回归用例：没开 TLS 时 MinVersion 原先也要校验，一个用不上的值让服务起不来
		"没配证书时 MinVersion 写错也不管": func(c *Config) { c.TLS.MinVersion = "1.1" },
		"MinVersion 留空取默认":       func(c *Config) { withCert(c); c.TLS.MinVersion = "" },
	}
	for name, m := range good {
		if err := configWith(m).Validate(); err != nil {
			t.Errorf("%s：不该报错，got=%v", name, err)
		}
	}
	if DefaultConfig().TLS.MinVersion != "1.2" {
		t.Errorf("MinVersion 默认应是 1.2，got=%q", DefaultConfig().TLS.MinVersion)
	}
}

func TestStart_ClientCAFileEnablesMutualTLS(t *testing.T) {
	p := newServerPKI(t)
	base, hits := servingTLS(t, p, func(c *Config) { c.TLS.ClientCAFile = p.caFile })

	cn, err := get(p, base, &tls.Config{Certificates: []tls.Certificate{p.client}})
	if err != nil || cn != "svc.internal" {
		t.Fatalf("带着这个 CA 签的客户端证书该成功，且 handler 看得到它：cn=%q err=%v", cn, err)
	}

	for name, cfg := range map[string]*tls.Config{
		"不带客户端证书":       {},
		"别的 CA 签的客户端证书": {Certificates: []tls.Certificate{p.stranger}},
	} {
		before := hits.Load()
		if _, err := get(p, base, cfg); err == nil {
			t.Errorf("%s：该被拒绝", name)
		}
		if hits.Load() != before {
			t.Errorf("%s：被拒的请求不该到 handler", name)
		}
	}
}

func TestStart_NoClientCertRequiredWithoutClientCAFile(t *testing.T) {
	p := newServerPKI(t)
	base, _ := servingTLS(t, p)
	if cn, err := get(p, base, &tls.Config{}); err != nil || cn != "" {
		t.Fatalf("单向 TLS 不带客户端证书该成功：cn=%q err=%v", cn, err)
	}
}

func TestStart_MinVersion(t *testing.T) {
	p := newServerPKI(t)
	tls12 := func() *tls.Config { return &tls.Config{MaxVersion: tls.VersionTLS12} }
	tls11 := func() *tls.Config { return &tls.Config{MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11} }

	base, _ := servingTLS(t, p)
	if _, err := get(p, base, tls12()); err != nil {
		t.Errorf("默认 1.2：TLS 1.2 的客户端该连得上，got=%v", err)
	}
	if _, err := get(p, base, tls11()); err == nil {
		t.Error("默认 1.2：TLS 1.1 的客户端该被拒")
	}

	base, _ = servingTLS(t, p, func(c *Config) { c.TLS.MinVersion = "1.3" })
	if _, err := get(p, base, tls12()); err == nil {
		t.Error("MinVersion 1.3：最高只到 TLS 1.2 的客户端该被拒")
	}
	if _, err := get(p, base, &tls.Config{}); err != nil {
		t.Errorf("MinVersion 1.3：TLS 1.3 的客户端该连得上，got=%v", err)
	}
}

func TestStart_ClientCAFileUnreadableDoesNotListen(t *testing.T) {
	p := newServerPKI(t)
	junk := filepath.Join(t.TempDir(), "junk.pem")
	if err := os.WriteFile(junk, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, ca := range map[string]string{
		"文件不存在":   filepath.Join(t.TempDir(), "nope.pem"),
		"文件里没有证书": junk,
	} {
		c := configWith(quiet, on(testkit.FreePort(t)), func(c *Config) {
			c.TLS.CertFile, c.TLS.KeyFile, c.TLS.ClientCAFile = p.certFile, p.keyFile, ca
		})
		err := startErr(t, New().WithConfig(c))
		if err == nil || !strings.Contains(err.Error(), "TLS.ClientCAFile") {
			t.Errorf("%s：该在监听之前报 ClientCAFile 的错，got=%v", name, err)
		}
	}
}

func TestConfig_ServerTLSLoadedFromFile(t *testing.T) {
	c := load(t, "XEcho:\n  TLS:\n    CertFile: c.pem\n    KeyFile: k.pem\n    ClientCAFile: ca.pem\n    MinVersion: \"1.3\"\n")
	if c.TLS.ClientCAFile != "ca.pem" || c.TLS.MinVersion != "1.3" {
		t.Errorf("没读对：ClientCAFile=%q MinVersion=%q", c.TLS.ClientCAFile, c.TLS.MinVersion)
	}
	if err := loadErr(t, "XEcho:\n  TLS:\n    ClientCAFile: ca.pem\n"); err == nil {
		t.Error("没配证书却配了 ClientCAFile，读配置时就该失败")
	}
}

func TestConfig_FlatTLSKeysFailAndPointToTLSBlock(t *testing.T) {
	// 证书这几项在 TLS: 块里。照平铺的写法写，读配置就失败，并且说清楚该挪到哪：
	// 静默忽略的话服务会以明文起来，而配置文件看上去是配了证书的
	for _, f := range []string{"CertFile", "KeyFile", "ClientCAFile", "MinVersion"} {
		err := loadErr(t, "XEcho:\n  "+f+": x\n")
		want := "did you mean TLS." + f + "? move it under TLS:"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("平铺写 %s：该失败并提示 %q，got=%v", f, want, err)
		}
	}
}
