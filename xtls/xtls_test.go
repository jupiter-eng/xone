package xtls

import (
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
)

// pki 测试时现造的一个 CA，和它签的一张服务端证书（127.0.0.1 / svc.internal）、一张客户端证书
type pki struct {
	caFile, certFile, keyFile string
	server                    tls.Certificate
	pool                      *x509.CertPool
}

func newPKI(t *testing.T) pki {
	t.Helper()
	dir := t.TempDir()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "xtls-test-ca"},
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
	srvDER, srvKey := issue(2, x509.ExtKeyUsageServerAuth)
	cliDER, cliKey := issue(3, x509.ExtKeyUsageClientAuth)
	cliKeyDER, _ := x509.MarshalECPrivateKey(cliKey)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return pki{
		caFile:   write("ca.pem", "CERTIFICATE", caDER),
		certFile: write("client.pem", "CERTIFICATE", cliDER),
		keyFile:  write("client-key.pem", "EC PRIVATE KEY", cliKeyDER),
		server:   tls.Certificate{Certificate: [][]byte{srvDER}, PrivateKey: srvKey},
		pool:     pool,
	}
}

// handshake 起一个 TLS 监听，用 client 握一次手，返回客户端这一侧的错误和协商出的版本
func handshake(t *testing.T, srv, client *tls.Config) (uint16, error) {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srv)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = c.(*tls.Conn).Handshake()
		c.Close()
	}()
	conn, err := tls.Dial("tcp", ln.Addr().String(), client)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	return conn.ConnectionState().Version, nil
}

func TestValidate_NonsensicalCombosFailAtConfigRead(t *testing.T) {
	for name, c := range map[string]Config{
		"没开却写了 CAFile":     {CAFile: "ca.pem"},
		"没开却写了 ServerName": {ServerName: "db.internal"},
		"没开却写了客户端证书":       {CertFile: "c.pem", KeyFile: "k.pem"},
		"只配了 CertFile":     {Enable: true, CertFile: "c.pem"},
		"只配了 KeyFile":      {Enable: true, KeyFile: "k.pem"},
	} {
		if err := c.Validate(); err == nil {
			t.Errorf("%s：该报错", name)
		}
	}
	for name, c := range map[string]Config{
		"零值":       {},
		"只开不配别的":   {Enable: true},
		"成对的客户端证书": {Enable: true, CertFile: "c.pem", KeyFile: "k.pem"},
	} {
		if err := c.Validate(); err != nil {
			t.Errorf("%s：不该报错，got=%v", name, err)
		}
	}
}

func TestBuild_NilWhenDisabled(t *testing.T) {
	cfg, err := Config{}.Build()
	if cfg != nil || err != nil {
		t.Fatalf("没开 TLS 应返回 nil, nil，got=%v, %v", cfg, err)
	}
}

func TestBuild_ErrorsOnFieldsSetWhileDisabled(t *testing.T) {
	if _, err := (Config{CAFile: "ca.pem"}).Build(); err == nil {
		t.Fatal("Build 也要先查一遍：直接调它的人拿不到一个悄悄走明文的 nil")
	}
}

func TestBuild_MinTLS12AndVerifies(t *testing.T) {
	p := newPKI(t)
	cfg, err := Config{Enable: true, CAFile: p.caFile, ServerName: "svc.internal"}.Build()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion 应是 TLS 1.2，got=%x", cfg.MinVersion)
	}
	if cfg.InsecureSkipVerify {
		t.Error("不该跳过证书校验")
	}
	if cfg.ServerName != "svc.internal" {
		t.Errorf("ServerName 应原样带上，got=%q", cfg.ServerName)
	}

	// 服务端只肯说 TLS 1.1：握手必须失败
	old := &tls.Config{Certificates: []tls.Certificate{p.server}, MaxVersion: tls.VersionTLS11}
	if _, err := handshake(t, old, cfg); err == nil {
		t.Error("服务端最高只到 TLS 1.1 时该握手失败")
	}
}

func TestBuild_CAFileTrustsSelfSignedCert(t *testing.T) {
	p := newPKI(t)
	srv := &tls.Config{Certificates: []tls.Certificate{p.server}}
	cases := []struct {
		name string
		cfg  Config
		ok   bool
	}{
		{"CAFile 对", Config{Enable: true, CAFile: p.caFile, ServerName: "127.0.0.1"}, true},
		{"按证书上的域名比对", Config{Enable: true, CAFile: p.caFile, ServerName: "svc.internal"}, true},
		{"不填 CAFile 用系统根证书，自签的过不了", Config{Enable: true, ServerName: "svc.internal"}, false},
		{"ServerName 对不上", Config{Enable: true, CAFile: p.caFile, ServerName: "other.internal"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := c.cfg.Build()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := handshake(t, srv, cfg); c.ok != (err == nil) {
				t.Fatalf("want ok=%v, got err=%v", c.ok, err)
			}
		})
	}
}

func TestBuild_ClientCert(t *testing.T) {
	p := newPKI(t)
	srv := &tls.Config{
		Certificates: []tls.Certificate{p.server},
		ClientAuth:   tls.RequireAndVerifyClientCert, ClientCAs: p.pool,
	}
	cfg, err := Config{Enable: true, CAFile: p.caFile, CertFile: p.certFile, KeyFile: p.keyFile, ServerName: "svc.internal"}.Build()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("应带上一张客户端证书，got=%d", len(cfg.Certificates))
	}
	if _, err := handshake(t, srv, cfg); err != nil {
		t.Fatalf("带着客户端证书该握得上：%v", err)
	}
}

func TestBuild_ErrorsOnUnreadableFile(t *testing.T) {
	p := newPKI(t)
	missing := filepath.Join(t.TempDir(), "nope.pem")
	junk := filepath.Join(t.TempDir(), "junk.pem")
	if err := os.WriteFile(junk, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		cfg  Config
		want string
	}{
		"CAFile 不存在":   {Config{Enable: true, CAFile: missing}, "TLS.CAFile"},
		"CAFile 里没有证书": {Config{Enable: true, CAFile: junk}, "contains no PEM certificate"},
		"客户端证书读不出来":    {Config{Enable: true, CertFile: missing, KeyFile: missing}, "TLS.CertFile"},
		"私钥和证书对不上":     {Config{Enable: true, CertFile: p.certFile, KeyFile: p.caFile}, "TLS.KeyFile"},
	} {
		_, err := c.cfg.Build()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：错误里该有 %q，got=%v", name, c.want, err)
		}
	}
}
