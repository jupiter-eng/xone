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

// pki is a CA built for the test together with one server certificate it signs (127.0.0.1 / svc.internal) and one client certificate.
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

// handshake starts a TLS listener, performs one handshake with client, and returns the error seen on the client side and the negotiated version.
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
		"CAFile set without Enable":             {CAFile: "ca.pem"},
		"ServerName set without Enable":         {ServerName: "db.internal"},
		"client certificate set without Enable": {CertFile: "c.pem", KeyFile: "k.pem"},
		"only CertFile":                         {Enable: true, CertFile: "c.pem"},
		"only KeyFile":                          {Enable: true, KeyFile: "k.pem"},
	} {
		if err := c.Validate(); err == nil {
			t.Errorf("%s: must be an error", name)
		}
	}
	for name, c := range map[string]Config{
		"the zero value":                {},
		"Enable alone":                  {Enable: true},
		"a matching client certificate": {Enable: true, CertFile: "c.pem", KeyFile: "k.pem"},
	} {
		if err := c.Validate(); err != nil {
			t.Errorf("%s: must not be an error, got=%v", name, err)
		}
	}
}

func TestBuild_NilWhenDisabled(t *testing.T) {
	cfg, err := Config{}.Build()
	if cfg != nil || err != nil {
		t.Fatalf("with TLS off it must return nil, nil, got=%v, %v", cfg, err)
	}
}

func TestBuild_ErrorsOnFieldsSetWhileDisabled(t *testing.T) {
	if _, err := (Config{CAFile: "ca.pem"}).Build(); err == nil {
		t.Fatal("Build must validate first too: calling it directly must not hand back a nil that quietly goes plaintext")
	}
}

func TestBuild_MinTLS12AndVerifies(t *testing.T) {
	p := newPKI(t)
	cfg, err := Config{Enable: true, CAFile: p.caFile, ServerName: "svc.internal"}.Build()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion must be TLS 1.2, got=%x", cfg.MinVersion)
	}
	if cfg.InsecureSkipVerify {
		t.Error("certificate verification must not be skipped")
	}
	if cfg.ServerName != "svc.internal" {
		t.Errorf("ServerName must be carried over as it is, got=%q", cfg.ServerName)
	}

	// The server will only speak TLS 1.1: the handshake must fail.
	old := &tls.Config{Certificates: []tls.Certificate{p.server}, MaxVersion: tls.VersionTLS11}
	if _, err := handshake(t, old, cfg); err == nil {
		t.Error("the handshake must fail when the server goes no higher than TLS 1.1")
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
		{"CAFile matches", Config{Enable: true, CAFile: p.caFile, ServerName: "127.0.0.1"}, true},
		{"verified against the domain on the certificate", Config{Enable: true, CAFile: p.caFile, ServerName: "svc.internal"}, true},
		{"without CAFile the system roots are used and a self-signed certificate fails", Config{Enable: true, ServerName: "svc.internal"}, false},
		{"ServerName does not match", Config{Enable: true, CAFile: p.caFile, ServerName: "other.internal"}, false},
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
		t.Fatalf("exactly one client certificate must be carried, got=%d", len(cfg.Certificates))
	}
	if _, err := handshake(t, srv, cfg); err != nil {
		t.Fatalf("the handshake must succeed with a client certificate: %v", err)
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
		"CAFile does not exist":                  {Config{Enable: true, CAFile: missing}, "TLS.CAFile"},
		"CAFile holds no certificate":            {Config{Enable: true, CAFile: junk}, "contains no PEM certificate"},
		"the client certificate cannot be read":  {Config{Enable: true, CertFile: missing, KeyFile: missing}, "TLS.CertFile"},
		"the key does not match the certificate": {Config{Enable: true, CertFile: p.certFile, KeyFile: p.caFile}, "TLS.KeyFile"},
	} {
		_, err := c.cfg.Build()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: the error must contain %q, got=%v", name, c.want, err)
		}
	}
}
