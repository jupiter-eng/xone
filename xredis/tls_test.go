package xredis

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/xtls"
)

// testPKI 测试时现造的一套证书：一个 CA，它签的服务端证书（127.0.0.1 / redis.internal）
// 和客户端证书。都写进临时目录，路径交给配置
type testPKI struct {
	caFile, certFile, keyFile string // CA 证书；客户端证书与私钥
	server                    tls.Certificate
	pool                      *x509.CertPool
}

func newPKI(t *testing.T) testPKI {
	t.Helper()
	dir := t.TempDir()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "xredis-test-ca"},
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
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "redis.internal"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			DNSNames: []string{"redis.internal"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
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

func TestNew_TLS(t *testing.T) {
	pki := newPKI(t)
	f := newFakeRedisTLS(t, &tls.Config{Certificates: []tls.Certificate{pki.server}})

	cases := []struct {
		name string
		tls  xtls.Config
		ok   bool
	}{
		{"CAFile 认得服务端证书", xtls.Config{Enable: true, CAFile: pki.caFile}, true},
		// 不填 CAFile 用系统根证书，自签的 CA 不在里面：证书校验真的开着
		{"不填 CAFile 时自签证书过不了校验", xtls.Config{Enable: true}, false},
		{"ServerName 对不上证书就失败", xtls.Config{Enable: true, CAFile: pki.caFile, ServerName: "other.internal"}, false},
		{"ServerName 按证书上的域名填", xtls.Config{Enable: true, CAFile: pki.caFile, ServerName: "redis.internal"}, true},
		// 没开 TLS 就是明文，明文打到 TLS 端口上连不上
		{"没开 TLS 就是明文", xtls.Config{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := liveCfg(f)
			cfg.TLS = c.tls
			client, closer, err := New(context.Background(), cfg)
			if c.ok != (err == nil) {
				t.Fatalf("want ok=%v, got err=%v", c.ok, err)
			}
			if err == nil {
				defer closer.Close()
				if err := client.Ping(context.Background()).Err(); err != nil {
					t.Errorf("连上之后该能用：%v", err)
				}
			}
		})
	}
}

func TestNew_TLSMutualAuth(t *testing.T) {
	pki := newPKI(t)
	f := newFakeRedisTLS(t, &tls.Config{
		Certificates: []tls.Certificate{pki.server},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pki.pool,
	})

	cfg := liveCfg(f)
	cfg.TLS = xtls.Config{Enable: true, CAFile: pki.caFile}
	if _, _, err := New(context.Background(), cfg); err == nil {
		t.Fatal("服务端要客户端证书时，不带证书该连不上")
	}

	cfg.TLS.CertFile, cfg.TLS.KeyFile = pki.certFile, pki.keyFile
	_, closer, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("带上客户端证书该连得上：%v", err)
	}
	closer.Close()
}

func TestNew_TLSUnreadableFileIsConfigError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.pem")
	notPEM := filepath.Join(t.TempDir(), "junk.pem")
	os.WriteFile(notPEM, []byte("not a certificate"), 0o600)

	for name, tc := range map[string]xtls.Config{
		"CAFile 不存在":   {Enable: true, CAFile: missing},
		"CAFile 里没有证书": {Enable: true, CAFile: notPEM},
		"客户端证书读不出来":    {Enable: true, CertFile: missing, KeyFile: missing},
	} {
		cfg := DefaultClientConfig()
		cfg.Addr = deadAddr(t)
		cfg.TLS = tc
		_, _, err := New(context.Background(), cfg)
		if err == nil || !strings.Contains(err.Error(), "config") {
			t.Errorf("%s：该在建连之前报配置错误，got=%v", name, err)
		}
	}
}
