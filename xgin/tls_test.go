package xgin

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

	"github.com/gin-gonic/gin"

	"github.com/jupiter-eng/xone/internal/testkit"
)

// serverPKI is a certificate set made on the spot: one CA, the server certificate it signed
// (127.0.0.1) and a client certificate, plus a client certificate signed by an unrelated CA.
type serverPKI struct {
	caFile, certFile, keyFile string // the CA; the server certificate and its private key
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
	ca, caKey, caDER := newCA("xgin-test-ca")
	other, otherKey, _ := newCA("xgin-stranger-ca")
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

// servingTLS starts an https server and waits for it to listen. hits counts handler calls.
func servingTLS(t *testing.T, p serverPKI, mutate ...func(*Config)) (string, *atomic.Int32) {
	t.Helper()
	port := testkit.FreePort(t)
	c := configWith(append([]func(*Config){quiet, on(port), func(c *Config) {
		c.TLS.CertFile, c.TLS.KeyFile = p.certFile, p.keyFile
	}}, mutate...)...)
	var hits atomic.Int32
	g := New().WithConfig(c).WithRoutes(func(e *gin.Engine) {
		e.GET("/who", func(c *gin.Context) {
			hits.Add(1)
			cn := ""
			if s := c.Request.TLS; s != nil && len(s.PeerCertificates) > 0 {
				cn = s.PeerCertificates[0].Subject.CommonName
			}
			c.String(200, cn)
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
			t.Fatalf("the server never came up: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return "https://" + addr, &hits
}

// get requests /who once with the given client TLS settings.
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

// withCert configures a certificate, which is what turns TLS on (Validate reads no files, so the
// paths can be anything).
func withCert(c *Config) { c.TLS.CertFile, c.TLS.KeyFile = "c.pem", "k.pem" }

func TestValidate_ServerTLSNewFields(t *testing.T) {
	bad := map[string]func(*Config){
		"ClientCAFile without a certificate": func(c *Config) { c.TLS.ClientCAFile = "ca.pem" },
		"MinVersion written as 1.1":          func(c *Config) { withCert(c); c.TLS.MinVersion = "1.1" },
		"MinVersion written as TLS1.3":       func(c *Config) { withCert(c); c.TLS.MinVersion = "TLS1.3" },
	}
	for name, m := range bad {
		if err := configWith(m).Validate(); err == nil {
			t.Errorf("%s: should be rejected", name)
		}
	}
	good := map[string]func(*Config){
		"defaults":                        func(*Config) {},
		"MinVersion 1.3":                  func(c *Config) { c.TLS.MinVersion = "1.3" },
		"a certificate plus ClientCAFile": func(c *Config) { c.TLS.CertFile, c.TLS.KeyFile, c.TLS.ClientCAFile = "c", "k", "ca" },
		"MinVersion is unused without a certificate": func(c *Config) { c.TLS.MinVersion = "1.3" },
		// Regression: MinVersion used to be validated with TLS off as well, so a value nobody used
		// kept the server from starting.
		"a wrong MinVersion is ignored without a certificate": func(c *Config) { c.TLS.MinVersion = "1.1" },
		"an empty MinVersion takes the default":               func(c *Config) { withCert(c); c.TLS.MinVersion = "" },
	}
	for name, m := range good {
		if err := configWith(m).Validate(); err != nil {
			t.Errorf("%s: should be accepted, got=%v", name, err)
		}
	}
	if DefaultConfig().TLS.MinVersion != "1.2" {
		t.Errorf("MinVersion should default to 1.2, got=%q", DefaultConfig().TLS.MinVersion)
	}
}

func TestStart_ClientCAFileEnablesMutualTLS(t *testing.T) {
	p := newServerPKI(t)
	base, hits := servingTLS(t, p, func(c *Config) { c.TLS.ClientCAFile = p.caFile })

	cn, err := get(p, base, &tls.Config{Certificates: []tls.Certificate{p.client}})
	if err != nil || cn != "svc.internal" {
		t.Fatalf("a client certificate signed by this CA should succeed and be visible to the handler: cn=%q err=%v", cn, err)
	}

	for name, cfg := range map[string]*tls.Config{
		"no client certificate":                     {},
		"a client certificate signed by another CA": {Certificates: []tls.Certificate{p.stranger}},
	} {
		before := hits.Load()
		if _, err := get(p, base, cfg); err == nil {
			t.Errorf("%s: should be rejected", name)
		}
		if hits.Load() != before {
			t.Errorf("%s: a rejected request must not reach the handler", name)
		}
	}
}

func TestStart_NoClientCertRequiredWithoutClientCAFile(t *testing.T) {
	p := newServerPKI(t)
	base, _ := servingTLS(t, p)
	if cn, err := get(p, base, &tls.Config{}); err != nil || cn != "" {
		t.Fatalf("one-way TLS without a client certificate should succeed: cn=%q err=%v", cn, err)
	}
}

func TestStart_MinVersion(t *testing.T) {
	p := newServerPKI(t)
	tls12 := func() *tls.Config { return &tls.Config{MaxVersion: tls.VersionTLS12} }
	tls11 := func() *tls.Config { return &tls.Config{MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11} }

	base, _ := servingTLS(t, p)
	if _, err := get(p, base, tls12()); err != nil {
		t.Errorf("default 1.2: a TLS 1.2 client should connect, got=%v", err)
	}
	if _, err := get(p, base, tls11()); err == nil {
		t.Error("default 1.2: a TLS 1.1 client should be rejected")
	}

	base, _ = servingTLS(t, p, func(c *Config) { c.TLS.MinVersion = "1.3" })
	if _, err := get(p, base, tls12()); err == nil {
		t.Error("MinVersion 1.3: a client capped at TLS 1.2 should be rejected")
	}
	if _, err := get(p, base, &tls.Config{}); err != nil {
		t.Errorf("MinVersion 1.3: a TLS 1.3 client should connect, got=%v", err)
	}
}

func TestStart_ClientCAFileUnreadableDoesNotListen(t *testing.T) {
	p := newServerPKI(t)
	junk := filepath.Join(t.TempDir(), "junk.pem")
	if err := os.WriteFile(junk, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, ca := range map[string]string{
		"the file does not exist":       filepath.Join(t.TempDir(), "nope.pem"),
		"the file holds no certificate": junk,
	} {
		c := configWith(quiet, on(testkit.FreePort(t)), func(c *Config) {
			c.TLS.CertFile, c.TLS.KeyFile, c.TLS.ClientCAFile = p.certFile, p.keyFile, ca
		})
		err := startErr(t, New().WithConfig(c))
		if err == nil || !strings.Contains(err.Error(), "TLS.ClientCAFile") {
			t.Errorf("%s: should report a ClientCAFile error before listening, got=%v", name, err)
		}
	}
}

func TestConfig_ServerTLSLoadedFromFile(t *testing.T) {
	c := load(t, "XGin:\n  TLS:\n    CertFile: c.pem\n    KeyFile: k.pem\n    ClientCAFile: ca.pem\n    MinVersion: \"1.3\"\n")
	if c.TLS.ClientCAFile != "ca.pem" || c.TLS.MinVersion != "1.3" {
		t.Errorf("not read correctly: ClientCAFile=%q MinVersion=%q", c.TLS.ClientCAFile, c.TLS.MinVersion)
	}
	if err := loadErr(t, "XGin:\n  TLS:\n    ClientCAFile: ca.pem\n"); err == nil {
		t.Error("ClientCAFile without a certificate should fail while reading the config")
	}
}

func TestConfig_FlatTLSKeysFailAndPointToTLSBlock(t *testing.T) {
	// The certificate fields live in the TLS: block. Written flat, reading the config fails and says
	// where they belong: ignoring them silently would start the server in plaintext while the config
	// file looks like a certificate was configured.
	for _, f := range []string{"CertFile", "KeyFile", "ClientCAFile", "MinVersion"} {
		err := loadErr(t, "XGin:\n  "+f+": x\n")
		want := "did you mean TLS." + f + "? move it under TLS:"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s written flat: should fail and point at %q, got=%v", f, want, err)
		}
	}
}
