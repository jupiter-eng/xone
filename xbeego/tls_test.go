package xbeego

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
	"testing"
	"time"

	"github.com/jupiter-eng/xone/internal/testkit"
)

// pki is a certificate set made on the spot: one CA, the server certificate it signed (127.0.0.1)
// and a client certificate signed by the same CA.
//
// The TLS rules themselves live in internal/web and are covered by xgin's tests; what is checked
// here is that XBeego's TLS block reaches that server at all -- a certificate configured and
// quietly not used is the one TLS mistake nobody notices.
type pki struct {
	caFile, certFile, keyFile string
	pool                      *x509.CertPool
	client                    tls.Certificate
}

func newPKI(t *testing.T) pki {
	t.Helper()
	dir := t.TempDir()

	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
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
			IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
			ExtKeyUsage: []x509.ExtKeyUsage{usage}, KeyUsage: x509.KeyUsageDigitalSignature,
		}
		der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		return der, key
	}

	write := func(name string, blocks ...*pem.Block) string {
		path := filepath.Join(dir, name)
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		for _, b := range blocks {
			if err := pem.Encode(f, b); err != nil {
				t.Fatal(err)
			}
		}
		return path
	}
	keyBlock := func(k *ecdsa.PrivateKey) *pem.Block {
		der, err := x509.MarshalECPrivateKey(k)
		if err != nil {
			t.Fatal(err)
		}
		return &pem.Block{Type: "EC PRIVATE KEY", Bytes: der}
	}

	srvDER, srvKey := issue(2, x509.ExtKeyUsageServerAuth)
	cliDER, cliKey := issue(3, x509.ExtKeyUsageClientAuth)

	pool := x509.NewCertPool()
	pool.AddCert(ca)
	cliCert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cliDER}),
		pem.EncodeToMemory(keyBlock(cliKey)))
	if err != nil {
		t.Fatal(err)
	}

	return pki{
		caFile:   write("ca.pem", &pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		certFile: write("srv.pem", &pem.Block{Type: "CERTIFICATE", Bytes: srvDER}),
		keyFile:  write("srv.key", keyBlock(srvKey)),
		pool:     pool,
		client:   cliCert,
	}
}

// httpsClient trusts the test CA and, with certs given, presents them.
func httpsClient(p pki, certs ...tls.Certificate) *http.Client {
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: p.pool, Certificates: certs, MinVersion: tls.VersionTLS12},
	}}
}

func TestStart_TLS(t *testing.T) {
	testkit.QuietSlog(t)
	p := newPKI(t)
	port := testkit.FreePort(t)
	x := New().WithConfig(configWith(quiet, on(port), func(c *Config) {
		c.TLS.CertFile, c.TLS.KeyFile = p.certFile, p.keyFile
	}))

	go func() { _ = x.Start(t.Context()) }()
	t.Cleanup(func() { _ = x.Stop(context.Background()) })

	base := fmt.Sprintf("https://127.0.0.1:%d", port)
	client := httpsClient(p)
	var resp *http.Response
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err = client.Get(base + "/ping"); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("the server should be serving https: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "pong" {
		t.Errorf("the route should be served over TLS, got=%q", body)
	}

	// A plaintext request to a TLS port reaches no handler. net/http answers it rather than
	// dropping the connection (measured on Go 1.27: HTTP/1.0 400 with
	// "Client sent an HTTP request to an HTTPS server."), so what has to be checked is that it is
	// that refusal and not the route.
	plain, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/ping", port))
	if err != nil {
		return // some versions drop the connection instead, which is a refusal just the same
	}
	defer plain.Body.Close()
	plainBody, _ := io.ReadAll(plain.Body)
	if plain.StatusCode != http.StatusBadRequest {
		t.Errorf("a plaintext request to a TLS port should be refused, got=%d %q", plain.StatusCode, plainBody)
	}
	if string(plainBody) == "pong" {
		t.Error("a plaintext request must not reach the route")
	}
}

func TestStart_MutualTLS(t *testing.T) {
	// ClientCAFile governs the whole port, framework routes included: a client without a
	// certificate must not get through, or "mutual TLS is on" is a line in a config file and
	// nothing more.
	testkit.QuietSlog(t)
	p := newPKI(t)
	port := testkit.FreePort(t)
	x := New().WithConfig(configWith(quiet, on(port), func(c *Config) {
		c.TLS.CertFile, c.TLS.KeyFile, c.TLS.ClientCAFile = p.certFile, p.keyFile, p.caFile
	}))

	go func() { _ = x.Start(t.Context()) }()
	t.Cleanup(func() { _ = x.Stop(context.Background()) })

	base := fmt.Sprintf("https://127.0.0.1:%d", port)
	withCert := httpsClient(p, p.client)
	var resp *http.Response
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err = withCert.Get(base + "/ping"); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("a client presenting its certificate should get through: %v", err)
	}
	resp.Body.Close()

	if r, err := httpsClient(p).Get(base + "/ping"); err == nil {
		r.Body.Close()
		t.Error("a client with no certificate should be refused")
	}
}

func TestStart_UnreadableClientCAIsAConfigError(t *testing.T) {
	// It fails before listening: coming up in plaintext while the config file says mutual TLS is
	// the most dangerous way to get this wrong.
	testkit.QuietSlog(t)
	p := newPKI(t)
	x := New().WithConfig(configWith(quiet, on(testkit.FreePort(t)), func(c *Config) {
		c.TLS.CertFile, c.TLS.KeyFile = p.certFile, p.keyFile
		c.TLS.ClientCAFile = filepath.Join(t.TempDir(), "missing.pem")
	}))

	done := make(chan error, 1)
	go func() { done <- x.Start(t.Context()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an unreadable client CA should be an error")
		}
	case <-time.After(2 * time.Second):
		_ = x.Stop(context.Background())
		t.Fatal("it should have returned an error instead of listening")
	}
}
