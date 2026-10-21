package xhttp

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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/xonetest"
	"github.com/jupiter-eng/xone/xtls"
)

// testPKI is a freshly built CA together with the server certificate it signs (127.0.0.1 / api.internal) and a client certificate.
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
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "xhttp-test-ca"},
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
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "api.internal"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			DNSNames: []string{"api.internal"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
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

// tlsServer starts an https server with HTTP/2 on; the body it answers is the protocol version,
// and clientCrt records whether a client certificate was seen.
func tlsServer(t *testing.T, cfg *tls.Config) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	var clientCrt atomic.Bool
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			clientCrt.Store(true)
		}
		_, _ = w.Write([]byte(r.Proto))
	}))
	srv.EnableHTTP2 = true
	srv.TLS = cfg
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, &clientCrt
}

func TestNew_TLSBlockApplied(t *testing.T) {
	p := newPKI(t)
	srv, _ := tlsServer(t, &tls.Config{Certificates: []tls.Certificate{p.server}})
	cases := []struct {
		name string
		tls  xtls.Config
		ok   bool
	}{
		{"CAFile recognizes the server certificate", xtls.Config{Enable: true, CAFile: p.caFile}, true},
		{"ServerName set to the domain on the certificate", xtls.Config{Enable: true, CAFile: p.caFile, ServerName: "api.internal"}, true},
		{"without a TLS block the system roots are used and a self-signed certificate fails", xtls.Config{}, false},
		{"ServerName does not match the certificate", xtls.Config{Enable: true, CAFile: p.caFile, ServerName: "other.internal"}, false},
	}
	for _, c := range cases {
		// Tracing on and off are two different transport chains, and the TLS has to reach the connection pool in both
		for _, trace := range []bool{true, false} {
			t.Run(c.name+"/Trace="+strconv.FormatBool(trace), func(t *testing.T) {
				cfg := DefaultConfig()
				cfg.TLS, cfg.Trace = c.tls, trace
				client, _ := newQuiet(t, cfg)
				resp, err := client.R().SetContext(context.Background()).Get(srv.URL)
				if c.ok != (err == nil) {
					t.Fatalf("want ok=%v, got err=%v", c.ok, err)
				}
				if err != nil {
					if !strings.Contains(err.Error(), "certificate") {
						t.Errorf("the error must say it is a certificate problem, got=%v", err)
					}
					return
				}
				// HTTP/2 still works on a custom TLSClientConfig (ForceAttemptHTTP2)
				if got := resp.String(); got != "HTTP/2.0" {
					t.Errorf("HTTP/2.0 must still be negotiated after replacing TLSClientConfig, got=%s", got)
				}
			})
		}
	}
}

func TestNew_TLSMutualAuth(t *testing.T) {
	p := newPKI(t)
	srv, clientCrt := tlsServer(t, &tls.Config{
		Certificates: []tls.Certificate{p.server},
		ClientAuth:   tls.RequireAndVerifyClientCert, ClientCAs: p.pool,
	})

	cfg := DefaultConfig()
	cfg.TLS = xtls.Config{Enable: true, CAFile: p.caFile}
	client, _ := newQuiet(t, cfg)
	if _, err := client.R().SetContext(context.Background()).Get(srv.URL); err == nil {
		t.Fatal("when the server requires a client certificate, sending none must fail")
	}

	cfg.TLS.CertFile, cfg.TLS.KeyFile = p.certFile, p.keyFile
	client, _ = newQuiet(t, cfg)
	resp, err := client.R().SetContext(context.Background()).Get(srv.URL)
	if err != nil || resp.StatusCode() != 200 {
		t.Fatalf("with a client certificate it must succeed: %v %v", resp, err)
	}
	if !clientCrt.Load() {
		t.Error("the server must have seen the client certificate")
	}
}

func TestNew_TLSBlockDoesNotAffectPlaintext(t *testing.T) {
	p := newPKI(t)
	srv, _ := echo(t, nil)
	cfg := DefaultConfig()
	cfg.TLS = xtls.Config{Enable: true, CAFile: p.caFile, ServerName: "api.internal"}
	client, _ := newQuiet(t, cfg)
	resp, err := client.R().SetContext(context.Background()).Get(srv.URL)
	if err != nil || resp.StatusCode() != 204 {
		t.Fatalf("an http:// request uses no TLS and succeeds as usual: %v %v", resp, err)
	}
}

func TestNew_TLSMisconfigIsConfigError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.pem")
	for name, tc := range map[string]xtls.Config{
		"CAFile set without Enable": {CAFile: missing},
		"CAFile does not exist":     {Enable: true, CAFile: missing},
		"only CertFile":             {Enable: true, CertFile: missing},
	} {
		cfg := DefaultConfig()
		cfg.TLS = tc
		_, _, err := New(cfg)
		if err == nil || !strings.Contains(err.Error(), "config") || !strings.Contains(err.Error(), "TLS") {
			t.Errorf("%s: it must be a configuration error, got=%v", name, err)
		}
	}
}

func TestInit_TLSBlockReadFromConfigFile(t *testing.T) {
	keepGlobals(t)
	withMetrics(t)
	p := newPKI(t)
	srv, _ := tlsServer(t, &tls.Config{Certificates: []tls.Certificate{p.server}})
	xonetest.UseConfigYAML(t, "XHttp:\n  TLS:\n    Enable: true\n    CAFile: "+strconv.Quote(p.caFile)+"\n")
	if err := initXHttp(context.Background()); err != nil {
		t.Fatal(err)
	}
	C().SetLogger(discardLogger{})
	resp, err := R(context.Background()).Get(srv.URL)
	if err != nil || resp.StatusCode() != 200 {
		t.Fatalf("it must connect with the CA from the config file: %v %v", resp, err)
	}
}

func TestValidate_TLSBlock(t *testing.T) {
	// It has to be caught while the config is read: xconfig.Unmarshal calls Validate, not New.
	for name, tc := range map[string]xtls.Config{
		"CAFile set without Enable": {CAFile: "/etc/ca.pem"},
		"only CertFile":             {Enable: true, CertFile: "c.pem"},
	} {
		cfg := DefaultConfig()
		cfg.TLS = tc
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "TLS.") {
			t.Errorf("%s: Validate must report the TLS error, got=%v", name, err)
		}
	}
}
