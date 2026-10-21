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

// testPKI is a set of certificates built during the test: a CA, the server certificate it signs (127.0.0.1 / redis.internal)
// and a client certificate. All are written into a temporary directory and the paths go into the configuration.
type testPKI struct {
	caFile, certFile, keyFile string // the CA certificate; the client certificate and its key
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
		{"CAFile recognizes the server certificate", xtls.Config{Enable: true, CAFile: pki.caFile}, true},
		// Without CAFile the system roots are used, and the self-signed CA is not among them: verification really is on
		{"without CAFile a self-signed certificate fails verification", xtls.Config{Enable: true}, false},
		{"a ServerName not matching the certificate fails", xtls.Config{Enable: true, CAFile: pki.caFile, ServerName: "other.internal"}, false},
		{"ServerName set to the domain on the certificate", xtls.Config{Enable: true, CAFile: pki.caFile, ServerName: "redis.internal"}, true},
		// Without TLS it is plaintext, and plaintext against a TLS port does not connect
		{"without TLS it is plaintext", xtls.Config{}, false},
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
					t.Errorf("the connection must be usable once established: %v", err)
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
		t.Fatal("when the server requires a client certificate, connecting without one must fail")
	}

	cfg.TLS.CertFile, cfg.TLS.KeyFile = pki.certFile, pki.keyFile
	_, closer, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connecting with a client certificate must succeed: %v", err)
	}
	closer.Close()
}

func TestNew_TLSUnreadableFileIsConfigError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.pem")
	notPEM := filepath.Join(t.TempDir(), "junk.pem")
	os.WriteFile(notPEM, []byte("not a certificate"), 0o600)

	for name, tc := range map[string]xtls.Config{
		"CAFile does not exist":                 {Enable: true, CAFile: missing},
		"CAFile holds no certificate":           {Enable: true, CAFile: notPEM},
		"the client certificate cannot be read": {Enable: true, CertFile: missing, KeyFile: missing},
	} {
		cfg := DefaultClientConfig()
		cfg.Addr = deadAddr(t)
		cfg.TLS = tc
		_, _, err := New(context.Background(), cfg)
		if err == nil || !strings.Contains(err.Error(), "config") {
			t.Errorf("%s: it must be a configuration error before connecting, got=%v", name, err)
		}
	}
}
