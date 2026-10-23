package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/jupiter-eng/xone/e2e/harness"
	"github.com/jupiter-eng/xone/xgin"
	"github.com/jupiter-eng/xone/xhttp"
	"github.com/jupiter-eng/xone/xonetest"
	"github.com/jupiter-eng/xone/xtls"
)

// tlsStub is an https stub asking for a client certificate: the body it answers is "the protocol, the TLS version, the client certificate's CN"
func tlsStub(t *testing.T, certs *harness.TLSCerts) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cn := ""
		if len(r.TLS.PeerCertificates) > 0 {
			cn = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		fmt.Fprintf(w, "%s %s %s", r.Proto, tls.VersionName(r.TLS.Version), cn)
	}))
	srv.EnableHTTP2 = true
	srv.TLS = certs.ServerTLS(t, true)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestTLS_XHttp(t *testing.T) {
	harness.Require(t)
	certs := harness.NewTLSCerts(t)
	stub := tlsStub(t, certs)
	ctx := context.Background()

	t.Run("the TLS block in the configuration file takes and the server sees TLS and the client certificate", func(t *testing.T) {
		xonetest.UseConfigYAML(t, "XHttp:\n  TLS: "+tlsBlock(xtls.Config{
			Enable: true, CAFile: certs.CAFile, CertFile: certs.ClientCert, KeyFile: certs.ClientKey,
		})+"\n")
		xonetest.StartHooks(t)
		resp, err := xhttp.R(ctx).Get(stub.URL)
		if err != nil || resp.StatusCode() != 200 {
			t.Fatalf("with the configured CA and client certificate it should succeed: %v %v", resp, err)
		}
		if got := resp.String(); got != "HTTP/2.0 TLS 1.3 "+harness.TLSMTLSUser {
			t.Errorf("the server should see HTTP/2, TLS and the client certificate CN=%s, got=%q", harness.TLSMTLSUser, resp.String())
		}
		t.Logf("numbers: the server saw %s", resp.String())
	})

	for _, c := range []struct {
		name string
		tls  xtls.Config
		want string
	}{
		{"a wrong CA", xtls.Config{Enable: true, CAFile: certs.OtherCAFile, CertFile: certs.ClientCert, KeyFile: certs.ClientKey},
			"certificate signed by unknown authority"},
		{"a ServerName mismatch", xtls.Config{Enable: true, CAFile: certs.CAFile, ServerName: "wrong.e2e.internal", CertFile: certs.ClientCert, KeyFile: certs.ClientKey},
			"certificate is valid for"},
		// The server refuses both of the next two, and what the client reports is not fixed: under TLS 1.3 the client
		// takes the handshake for done once it has sent Finished and starts writing the request, so the server's alert
		// arrives at the next read. Measured, net/http reports sometimes remote error: tls: certificate required and
		// sometimes write: broken pipe; an HTTP/2 connection reports http2: client conn could not be established alone.
		// So only the refusal is asserted
		{"a client certificate is wanted and none is presented", xtls.Config{Enable: true, CAFile: certs.CAFile}, ""},
		// The server lists the CAs it accepts in the handshake, and Go's client does not present a certificate that none of them signed
		{"a client certificate signed by a CA the server does not accept", xtls.Config{Enable: true, CAFile: certs.CAFile, CertFile: certs.StrangerCert, KeyFile: certs.StrangerKey}, ""},
		// No CAFile at all, so the roots are the system's: on macOS the platform verifier answers and
		// words it its own way, see untrustedCA
		{"no TLS block", xtls.Config{}, untrustedCA},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := xhttp.DefaultConfig()
			cfg.TLS, cfg.Metric = c.tls, false
			client, closer, err := xhttp.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer closer.Close()
			_, err = client.R().SetContext(ctx).Get(stub.URL)
			rejected(t, c.name, err, c.want)
			t.Logf("numbers: %s: %v", c.name, err)
		})
	}
}

// ginGet makes one /who request with the given client TLS settings. With h2 false it speaks
// HTTP/1.1 alone: on a failed handshake an HTTP/2 client reports http2: client conn could not be
// established alone, which does not say why
func ginGet(certs *harness.TLSCerts, url string, cfg *tls.Config, h2 bool) (string, error) {
	if cfg.RootCAs == nil {
		cfg.RootCAs = certs.Pool
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg, ForceAttemptHTTP2: h2}, Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Get(url + "/who")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return fmt.Sprintf("%d %s", resp.StatusCode, b), nil
}

func TestTLS_XGin(t *testing.T) {
	harness.Require(t)
	certs := harness.NewTLSCerts(t)
	port := harness.FreePort(t)
	xonetest.UseConfigYAML(t, fmt.Sprintf(`XGin:
  Host: 127.0.0.1
  Port: %d
  TLS:
    CertFile: %q
    KeyFile: %q
    ClientCAFile: %q
    MinVersion: "1.3"
  Log: false
  Metric: false
`, port, certs.ServerCert, certs.ServerKey, certs.CAFile))

	var hits atomic.Int32
	g := xgin.New().WithRoutes(func(e *gin.Engine) {
		e.GET("/who", func(c *gin.Context) {
			hits.Add(1)
			s := c.Request.TLS
			c.String(200, "%s %s %s", c.Request.Proto, tls.VersionName(s.Version), s.PeerCertificates[0].Subject.CommonName)
		})
	})
	done := make(chan error, 1)
	go func() { done <- g.Start(context.Background()) }()
	t.Cleanup(func() {
		_ = g.Stop(context.Background())
		<-done
	})
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for i := 0; ; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			break
		}
		if i == 200 {
			t.Fatal("xgin did not start listening")
		}
		time.Sleep(20 * time.Millisecond)
	}
	base := "https://" + addr

	got, err := ginGet(certs, base, &tls.Config{Certificates: []tls.Certificate{mustPair(t, certs.ClientCert, certs.ClientKey)}}, true)
	if err != nil || got != "200 HTTP/2.0 TLS 1.3 "+harness.TLSMTLSUser {
		t.Fatalf("with the right client certificate it should succeed and the handler should see TLS 1.3 and the certificate: got=%q err=%v", got, err)
	}
	t.Logf("numbers: %s", got)

	for _, c := range []struct {
		name string
		cfg  *tls.Config
		want string
	}{
		{"no client certificate", &tls.Config{}, "certificate required"},
		// The server lists the CAs of TLS.ClientCAFile in the handshake, and Go's client does not present a certificate that none of them signed
		{"a client certificate signed by a CA that is not in ClientCAFile", &tls.Config{Certificates: []tls.Certificate{mustPair(t, certs.StrangerCert, certs.StrangerKey)}},
			"certificate required"},
		{"a TLS 1.2 client meeting MinVersion 1.3", &tls.Config{MaxVersion: tls.VersionTLS12,
			Certificates: []tls.Certificate{mustPair(t, certs.ClientCert, certs.ClientKey)}}, "protocol version"},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := hits.Load()
			got, err := ginGet(certs, base, c.cfg, false)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("it should be refused and say %q, got=%q err=%v", c.want, got, err)
			}
			if hits.Load() != before {
				t.Error("a refused request should not reach the handler")
			}
		})
	}

	t.Run("a plaintext request against the TLS port", func(t *testing.T) {
		resp, err := http.Get("http://" + addr + "/who")
		if err != nil {
			t.Fatalf("net/http answers 400 to a plaintext request rather than breaking the connection: %v", err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 400 || hits.Load() != 1 {
			t.Errorf("a plaintext request should get a 400 and never reach the handler, got=%d %q hits=%d", resp.StatusCode, b, hits.Load())
		}
		t.Logf("numbers: the plaintext request: %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	})
}

func mustPair(t *testing.T, cert, key string) tls.Certificate {
	t.Helper()
	c, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
